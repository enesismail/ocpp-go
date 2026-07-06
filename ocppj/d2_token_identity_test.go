package ocppj

// PR-0 (tasks/d2-evict-old-duplicate-policy.md, "## PR-0 — dispatcher
// token-identity hardening") RED-FIRST test suite.
//
// This file lives in `package ocppj` (not `ocppj_test`) so it can reach
// DefaultServerDispatcher's unexported pump internals (timerC,
// readyForDispatch, mutex, doneC, stoppedC) exactly like
// request_timeout_test.go already does. It cannot reuse the MockFeature /
// MockWebsocketServer helpers from ocppj_test.go (different, non-importable
// package), so it defines its own minimal equivalents below (prefixed d2 to
// avoid any clash with the production package or other internal test files).
//
// RED-FIRST discipline: every test below references the PR-0 surface exactly
// as the spec names it - the not-yet-existing DeleteClientAndWait method, and
// the not-yet-existing {clientID, ctx} / {clientID, requestID} element shapes
// for timerC / readyForDispatch. Against today's dispatcher.go (timerC is
// `chan string`, readyForDispatch is `chan string`, and DeleteClientAndWait
// does not exist), this whole file fails to compile - that IS the intended
// red state pinning the PR-0 contract. See the report for exactly which
// tests are compile-red vs. would-be behavior-red once compilation is fixed.
//
// Naming choice (flagged for the implementer + codex): the spec's prose
// gives the timerC element as `chan struct{ clientID string; ctx
// context.Context }` and says readyForDispatch/CompleteRequest should "carry
// {clientID, requestID}". This file uses exactly those field names
// (`clientID`, `ctx`, `requestID`) in anonymous struct literals sent directly
// on d.timerC / d.readyForDispatch. Go's assignability rule (identical
// underlying type, at least one side unnamed) means this compiles against
// EITHER an anonymous or a same-shaped NAMED channel element type in the
// production implementation, as long as the implementer keeps these exact
// field names/order/types. If the implementer picks different field names,
// only this file's literals need a matching rename - no other test logic
// changes.
//
// Codex review round (this revision): every numbered finding below refers to
// the codex review of the first draft of this file. Each test/comment below
// states which finding it addresses and how.

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/enesismail/ocpp-go/ocpp"
	"github.com/enesismail/ocpp-go/ws"
)

// ---------------------------------------------------------------------
// Minimal in-package mock feature/request (package ocppj cannot import the
// ocppj_test package's MockFeature/newMockRequest - see file comment above).
// ---------------------------------------------------------------------

const d2MockFeatureName = "D2Mock"

type d2MockRequest struct {
	MockValue string `json:"mockValue"`
}

func (r *d2MockRequest) GetFeatureName() string { return d2MockFeatureName }

type d2MockConfirmation struct {
	MockValue string `json:"mockValue"`
}

func (c *d2MockConfirmation) GetFeatureName() string { return d2MockFeatureName }

type d2MockFeature struct{}

func (f *d2MockFeature) GetFeatureName() string { return d2MockFeatureName }
func (f *d2MockFeature) GetRequestType() reflect.Type {
	return reflect.TypeOf(d2MockRequest{})
}
func (f *d2MockFeature) GetResponseType() reflect.Type {
	return reflect.TypeOf(d2MockConfirmation{})
}

// d2NewBundle creates a fresh Call/RequestBundle pair via a throwaway Server
// endpoint (mirrors the "Create mock request" step used throughout
// dispatcher_test.go), returning the bundle and its generated unique ID.
func d2NewBundle(t *testing.T, endpoint *Server, value string) (RequestBundle, string) {
	t.Helper()
	req := &d2MockRequest{MockValue: value}
	call, err := endpoint.CreateCall(req)
	require.NoError(t, err)
	data, err := call.MarshalJSON()
	require.NoError(t, err)
	return RequestBundle{Call: call, Data: data}, call.UniqueId
}

// ---------------------------------------------------------------------
// Minimal in-package fake ws.Server. Only Write is ever exercised by
// DefaultServerDispatcher; every other ws.Server method is left to the
// embedded nil interface, so an accidental call panics loudly instead of
// silently no-opping (a real bug would then fail the test immediately).
// ---------------------------------------------------------------------

type d2FakeServer struct {
	ws.Server // embedded nil interface; only Write is overridden below

	mu      sync.Mutex
	onWrite func(clientID string, data []byte) error
}

func (f *d2FakeServer) Write(clientID string, data []byte) error {
	f.mu.Lock()
	cb := f.onWrite
	f.mu.Unlock()
	if cb != nil {
		return cb(clientID, data)
	}
	return nil
}

func (f *d2FakeServer) setOnWrite(cb func(clientID string, data []byte) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onWrite = cb
}

// d2NewDispatcher wires a DefaultServerDispatcher exactly like
// ServerDispatcherTestSuite.SetupTest (dispatcher_test.go), except it returns
// the CONCRETE type (not the ServerDispatcher interface) so these white-box
// tests can reach unexported fields/methods - including the PR-0 surface
// (DeleteClientAndWait) that is deliberately NOT added to the public
// interface (see spec item 3).
func d2NewDispatcher(t *testing.T) (d *DefaultServerDispatcher, state ServerState, queueMap ServerQueueMap, network *d2FakeServer, endpoint *Server) {
	t.Helper()
	queueMap = NewFIFOQueueMap(10)
	d = NewDefaultServerDispatcher(queueMap)
	var mutex sync.RWMutex
	state = NewServerState(&mutex)
	d.SetPendingRequestState(state)
	network = &d2FakeServer{}
	d.SetNetworkServer(network)
	endpoint = &Server{}
	endpoint.AddProfile(ocpp.NewProfile("d2mock", &d2MockFeature{}))
	return
}

// d2CancelEvent captures one onRequestCanceled invocation for assertion back
// on the test goroutine (the callback runs on the messagePump goroutine).
type d2CancelEvent struct {
	clientID  string
	requestID string
	err       *ocpp.Error
}

const d2Bound = 2 * time.Second               // generous bound for events that MUST happen
const d2SilenceBound = 300 * time.Millisecond // window to prove an event does NOT happen

// promptAckBound proves DeleteClientAndWait returned because it observed the
// real FIFO delete-ack, not because some internal fallback timeout elapsed.
// PR-0 contract point for the implementer (codex finding 4): the bounded
// `select` on ack/doneC/timeout described in the spec (item 3 / design
// decision 1) MUST use a fallback timeout that is large (>=1s recommended)
// specifically so tests like TestD2DeleteClientAndWaitQueueRemovedBranch and
// TestD2DeleteClientAndWaitQueueRecreatedBranch can tell "returned via the
// real ack" apart from "returned because the fallback elapsed" - if the
// fallback were e.g. 200ms, asserting a prompt return here would spuriously
// pass even for a completely broken ack path. Consider making the timeout
// injectable for direct testability instead of relying on this margin.
const promptAckBound = 200 * time.Millisecond

// ---------------------------------------------------------------------
// Test 1 - stale timerC token dropped (spec PR-0 item 1 / Tests item 1).
//
// Codex BLOCKER 1: the original single test only proved "no cancel event /
// no queue pop" after injecting a stale token. A wrong implementation that
// cancels the LIVE clientContextMap[id].cancel() (instead of dropping the
// whole stale event) but still correctly gates the queue-pop would have
// passed that assertion, because canceling a context this way never fires a
// cancel EVENT (fireRequestCancel is only invoked from the timeout branch,
// on context.DeadlineExceeded) - it just silently kills the real deadline.
// Split into two tests per the finding's suggestion:
//   - RealTimeoutStillFires: DECISIVE. Proves the live ctx was not
//     canceled, by proving its own real deadline still independently fires
//     later. A wrong impl that cancels the live ctx would make this test
//     hang (context.Canceled, not DeadlineExceeded, never reaches timerC -
//     see dispatcher.go:826-841) and fail on the bounded wait.
//   - RealResponseStillCompletes: the original scenario (long timeout, a
//     real response instead of a real timeout), kept as an additional,
//     independent regression guard.
// ---------------------------------------------------------------------

func TestD2StaleTimerCTokenDroppedRealTimeoutStillFires(t *testing.T) {
	d, state, queueMap, network, endpoint := d2NewDispatcher(t)
	const timeout = 300 * time.Millisecond
	d.SetTimeout(timeout)

	clientID := "client1"
	written := make(chan string, 4)
	network.setOnWrite(func(cID string, data []byte) error {
		written <- cID
		return nil
	})

	canceled := make(chan d2CancelEvent, 4)
	d.SetOnRequestCanceled(func(cID, rID string, req ocpp.Request, err *ocpp.Error) {
		canceled <- d2CancelEvent{clientID: cID, requestID: rID, err: err}
	})

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()

	d.CreateClient(clientID)
	bundle, requestID := d2NewBundle(t, endpoint, "a")
	require.NoError(t, d.SendRequest(clientID, bundle))

	select {
	case cID := <-written:
		require.Equal(t, clientID, cID)
	case <-time.After(d2Bound):
		t.Fatal("timed out waiting for the request to be dispatched")
	}
	dispatchedAt := time.Now()
	require.True(t, state.HasPendingRequest(clientID))

	// An arbitrary, independently-constructed ctx: guaranteed to mismatch
	// whatever the pump actually holds for clientID.
	staleCtx, staleCancel := context.WithCancel(context.Background())
	defer staleCancel()

	// PR-0: inject the stale timerC token directly, bypassing waitForTimeout,
	// well before the REAL deadline (timeout=300ms) so there is ample margin
	// left to observe the real deadline firing afterward.
	d.timerC <- struct {
		clientID string
		ctx      context.Context
	}{clientID: clientID, ctx: staleCtx}

	// Immediate bounded silence: the stale token must not trigger an
	// instantaneous cancel/pop. This window (100ms) stays comfortably below
	// the real deadline (300ms).
	const staleSilence = 100 * time.Millisecond
	select {
	case ev := <-canceled:
		t.Fatalf("stale timerC token incorrectly canceled/popped request %s: %v", ev.requestID, ev.err)
	case <-time.After(staleSilence):
		// expected: dropped
	}

	// Pending state and queue must be untouched by the stale token.
	require.True(t, state.HasPendingRequest(clientID))
	q, ok := queueMap.Get(clientID)
	require.True(t, ok)
	require.Equal(t, 1, q.Size())

	// DECISIVE: the REAL in-flight request's own deadline must still
	// independently fire later, at ~the real timeout - not never (a wrong
	// impl that canceled the live ctx via the stale token would make
	// waitForTimeout observe context.Canceled, not context.DeadlineExceeded,
	// and NEVER signal timerC - see dispatcher.go:825-841 - hanging this
	// select until d2Bound and failing, exactly catching that bug), and not
	// suspiciously early (close to the stale injection instead of the real
	// deadline).
	select {
	case ev := <-canceled:
		require.Equal(t, clientID, ev.clientID)
		require.Equal(t, requestID, ev.requestID)
		elapsed := time.Since(dispatchedAt)
		require.GreaterOrEqual(t, elapsed, timeout-100*time.Millisecond,
			"real timeout fired suspiciously early - close to the stale injection rather than the real deadline")
	case <-time.After(d2Bound):
		t.Fatal("the real in-flight request's own timeout never fired - the stale token may have canceled the live ctx")
	}

	require.False(t, state.HasPendingRequest(clientID))
	require.True(t, q.IsEmpty())
}

func TestD2StaleTimerCTokenDroppedRealResponseStillCompletes(t *testing.T) {
	d, state, queueMap, network, endpoint := d2NewDispatcher(t)
	d.SetTimeout(5 * time.Second) // long enough that no real timeout interferes

	clientID := "client1"
	written := make(chan string, 4)
	network.setOnWrite(func(cID string, data []byte) error {
		written <- cID
		return nil
	})

	canceled := make(chan d2CancelEvent, 4)
	d.SetOnRequestCanceled(func(cID, rID string, req ocpp.Request, err *ocpp.Error) {
		canceled <- d2CancelEvent{clientID: cID, requestID: rID, err: err}
	})

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()

	d.CreateClient(clientID)
	bundle, requestID := d2NewBundle(t, endpoint, "a")
	require.NoError(t, d.SendRequest(clientID, bundle))

	select {
	case cID := <-written:
		require.Equal(t, clientID, cID)
	case <-time.After(d2Bound):
		t.Fatal("timed out waiting for the request to be dispatched")
	}
	require.True(t, state.HasPendingRequest(clientID))

	// An arbitrary, independently-constructed ctx: guaranteed to mismatch
	// whatever the pump actually holds for clientID.
	staleCtx, staleCancel := context.WithCancel(context.Background())
	defer staleCancel()

	// PR-0: inject the stale timerC token directly, bypassing waitForTimeout.
	d.timerC <- struct {
		clientID string
		ctx      context.Context
	}{clientID: clientID, ctx: staleCtx}

	// Bounded negative assertion: the stale token must not trigger a cancel.
	select {
	case ev := <-canceled:
		t.Fatalf("stale timerC token incorrectly canceled/popped request %s: %v", ev.requestID, ev.err)
	case <-time.After(d2SilenceBound):
		// expected: dropped
	}

	// Pending state and queue must be untouched.
	require.True(t, state.HasPendingRequest(clientID))
	q, ok := queueMap.Get(clientID)
	require.True(t, ok)
	require.Equal(t, 1, q.Size())

	// The REAL response for A must still complete normally.
	d.CompleteRequest(clientID, requestID)
	require.False(t, state.HasPendingRequest(clientID))
	require.True(t, q.IsEmpty())
}

// ---------------------------------------------------------------------
// Test 2 - legit pipelining not stalled + timeout/response photo-finish
// (spec PR-0 item 2 / Tests item 2, fable B1 regression guard).
// ---------------------------------------------------------------------

// 2(a): queue [A, B] on one client; complete A directly; B must be dispatched
// and no cancel must ever be reported for A's own request (i.e. its timeout
// context was properly torn down rather than left to fire later).
func TestD2LegitPipeliningDispatchesNext(t *testing.T) {
	d, state, queueMap, network, endpoint := d2NewDispatcher(t)
	d.SetTimeout(5 * time.Second)

	clientID := "client1"
	written := make(chan string, 4)
	network.setOnWrite(func(cID string, data []byte) error {
		written <- cID
		return nil
	})
	canceled := make(chan d2CancelEvent, 4)
	d.SetOnRequestCanceled(func(cID, rID string, req ocpp.Request, err *ocpp.Error) {
		canceled <- d2CancelEvent{clientID: cID, requestID: rID, err: err}
	})

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()

	d.CreateClient(clientID)
	bundleA, requestA := d2NewBundle(t, endpoint, "a")
	bundleB, requestB := d2NewBundle(t, endpoint, "b")

	require.NoError(t, d.SendRequest(clientID, bundleA))
	select {
	case <-written:
	case <-time.After(d2Bound):
		t.Fatal("timed out waiting for A to be dispatched")
	}
	require.True(t, state.HasPendingRequest(clientID))

	require.NoError(t, d.SendRequest(clientID, bundleB))
	q, ok := queueMap.Get(clientID)
	require.True(t, ok)
	require.Equal(t, 2, q.Size()) // A pending (head), B queued behind it

	// Complete A: the real CompleteRequest path, now carrying {clientID,
	// requestID} internally, must recognize its own dispatched requestID
	// (recorded == A) and dispatch B.
	d.CompleteRequest(clientID, requestA)

	select {
	case cID := <-written:
		require.Equal(t, clientID, cID)
	case <-time.After(d2Bound):
		t.Fatal("timed out waiting for B to be dispatched after completing A")
	}

	cs := state.GetClientState(clientID)
	_, hasB := cs.GetPendingRequest(requestB)
	require.True(t, hasB, "B should now be the pending request")

	// A's timeout must have been torn down: no cancel ever reported for it.
	select {
	case ev := <-canceled:
		t.Fatalf("unexpected cancel for %s after A was legitimately completed", ev.requestID)
	case <-time.After(d2SilenceBound):
	}

	// Clean up B too, for hygiene.
	d.CompleteRequest(clientID, requestB)
	require.False(t, state.HasPendingRequest(clientID))
	require.True(t, q.IsEmpty())
}

// 2(b): timeout-vs-response photo finish. CompleteRequest(A) is held in the
// pop-before-pending-delete window while A's real timeout fires, so the pump
// observes the exact inconsistent state that previously let B go on the wire
// without valid pending state.
func TestD2TimeoutResponsePhotoFinishStillDispatchesNext(t *testing.T) {
	runD2TimeoutDuringCompletePopPendingWindowDoesNotDispatchNextEarly(t)
}

type d2BlockingPopQueue struct {
	RequestQueue
	popped  chan struct{}
	release chan struct{}
	once    sync.Once
}

func newD2BlockingPopQueue(capacity int) *d2BlockingPopQueue {
	return &d2BlockingPopQueue{
		RequestQueue: NewFIFOClientQueue(capacity),
		popped:       make(chan struct{}),
		release:      make(chan struct{}),
	}
}

func (q *d2BlockingPopQueue) Pop() interface{} {
	el := q.RequestQueue.Pop()
	q.once.Do(func() {
		close(q.popped)
		<-q.release
	})
	return el
}

func TestD2TimeoutDuringCompletePopPendingWindowDoesNotDispatchNextEarly(t *testing.T) {
	runD2TimeoutDuringCompletePopPendingWindowDoesNotDispatchNextEarly(t)
}

func runD2TimeoutDuringCompletePopPendingWindowDoesNotDispatchNextEarly(t *testing.T) {
	t.Helper()
	queueMap := NewFIFOQueueMap(10)
	d := NewDefaultServerDispatcher(queueMap)
	var mutex sync.RWMutex
	state := NewServerState(&mutex)
	d.SetPendingRequestState(state)
	network := &d2FakeServer{}
	d.SetNetworkServer(network)
	d.SetTimeout(80 * time.Millisecond)
	endpoint := &Server{}
	endpoint.AddProfile(ocpp.NewProfile("d2mock", &d2MockFeature{}))

	clientID := "client1"
	q := newD2BlockingPopQueue(10)
	queueMap.Add(clientID, q)

	written := make(chan string, 4)
	network.setOnWrite(func(cID string, data []byte) error {
		written <- cID
		return nil
	})

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()

	bundleA, requestA := d2NewBundle(t, endpoint, "a")
	bundleB, requestB := d2NewBundle(t, endpoint, "b")

	require.NoError(t, d.SendRequest(clientID, bundleA))
	select {
	case cID := <-written:
		require.Equal(t, clientID, cID)
	case <-time.After(d2Bound):
		t.Fatal("timed out waiting for A to be dispatched")
	}
	require.NoError(t, d.SendRequest(clientID, bundleB))

	completeDone := make(chan struct{})
	go func() {
		d.CompleteRequest(clientID, requestA)
		close(completeDone)
	}()

	select {
	case <-q.popped:
	case <-time.After(d2Bound):
		t.Fatal("CompleteRequest did not reach the injected pop/pending window")
	}

	cs := state.GetClientState(clientID)
	_, hasA := cs.GetPendingRequest(requestA)
	_, hasB := cs.GetPendingRequest(requestB)
	require.True(t, hasA, "A must still be pending while CompleteRequest is blocked after Pop")
	require.False(t, hasB, "B must not be pending before it is legitimately dispatched")

	select {
	case cID := <-written:
		t.Fatalf("timeout dispatched %s while A was still pending in the pop/pending window", cID)
	case <-time.After(d2SilenceBound):
	}

	close(q.release)
	select {
	case <-completeDone:
	case <-time.After(d2Bound):
		t.Fatal("CompleteRequest did not finish after releasing the injected Pop block")
	}

	select {
	case cID := <-written:
		require.Equal(t, clientID, cID)
	case <-time.After(d2Bound):
		t.Fatal("B did not dispatch after A completed")
	}

	cs = state.GetClientState(clientID)
	_, hasB = cs.GetPendingRequest(requestB)
	require.True(t, hasB, "B must have valid pending state after it is dispatched")

	d.CompleteRequest(clientID, requestB)
	require.False(t, state.HasPendingRequest(clientID))
	require.True(t, q.IsEmpty())
}

// ---------------------------------------------------------------------
// Test 2c - deterministic positive test for the three-way rule's
// recorded==empty -> process-as-kick arm (codex findings 2 & 7).
//
// The photo-finish test above only exercises this arm probabilistically,
// and since the pump dispatches inline (redispatch happens in the same
// select iteration that pops a completed/timed-out request, right before
// looping back), the buffered-ready "rescue" window it is meant to prove
// may never actually be hit in a given run. This test instead drives the
// pump precisely (white-box) to FORCE the exact state the kick arm exists
// for: the client's recorded dispatched-requestID is empty (no in-flight
// request) while a request nonetheless sits in its queue, undispatched -
// then a readyForDispatch token arrives and must "kick" it out rather than
// being dropped.
//
// Review finding: an earlier draft of this test reached the "recorded
// empty" precondition by dispatching+completing a SOLO request (nothing
// queued behind it) via the real SendRequest/CompleteRequest path, then
// pushing a "kick" bundle onto the queue and injecting its own
// readyForDispatch token carrying the just-completed solo requestID. That
// was a FALSE-GREEN: readyForDispatch is a cap-1 buffered channel
// (dispatcher.go:536, make(chan string, 1), soon the same-cap-1 struct
// channel), and CompleteRequest itself sends a ready{solo} token into that
// same buffer as part of its real production behavior. Nothing in the old
// test forced the pump to drain that leftover token before the kick bundle
// was pushed and the test's own second ready{solo} token was injected, so
// the pump could legitimately process the LEFTOVER token first: at that
// point recorded still equalled the just-completed solo ID, so the ordinary
// MATCH arm (not empty->kick) fired and dispatched "kick" that way - the
// test's own injected token then arrived to find recorded already
// reset/mismatched and was silently dropped on the mismatch->drop arm. Net
// effect: the assertions passed via the MATCH arm, and the empty->kick arm
// this test exists to pin was never actually exercised - a dispatcher with
// only match-or-drop (no separate empty-recorded-kick path) would still
// pass.
//
// Fixed by making the empty-recorded state come "for free" from a brand-new,
// NEVER-DISPATCHED client instead of a completed one: right after
// CreateClient, nothing has ever been dispatched for clientID, so the
// pump's recorded dispatched-requestID is empty/absent by construction, AND
// there is no leftover token that could possibly be sitting in the cap-1
// buffer for it (nothing was ever put there). This eliminates the race
// instead of just narrowing it. The injected token's requestID is then a
// completely ARBITRARY, non-matching value ("kick-token-does-not-match-
// anything") that was never dispatched for anything - proving the dispatch
// happens purely because recorded is empty, not because of any requestID
// coincidence. This is also the decisive property versus a broken
// match-or-drop-only implementation: such an implementation would find no
// match for the arbitrary token and drop it, never dispatching the queued
// request, and this test would fail.
// ---------------------------------------------------------------------
func TestD2EmptyRecordedKicksQueuedRequest(t *testing.T) {
	d, state, queueMap, network, endpoint := d2NewDispatcher(t)
	d.SetTimeout(5 * time.Second)

	clientID := "client1"
	written := make(chan string, 4)
	network.setOnWrite(func(cID string, data []byte) error {
		written <- cID
		return nil
	})
	canceled := make(chan d2CancelEvent, 4)
	d.SetOnRequestCanceled(func(cID, rID string, req ocpp.Request, err *ocpp.Error) {
		canceled <- d2CancelEvent{clientID: cID, requestID: rID, err: err}
	})

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()

	// Brand-new, NEVER-DISPATCHED client: by construction, recorded is
	// empty/absent for clientID and the cap-1 readyForDispatch buffer holds
	// no leftover token for it - no race to synchronize against.
	d.CreateClient(clientID)
	require.False(t, state.HasPendingRequest(clientID))

	// Push a request DIRECTLY onto the queue, bypassing SendRequest's
	// requestChannel signal, so the pump remains unaware of it: this
	// freezes "recorded empty, queue non-empty, nothing dispatched" as a
	// fixed precondition instead of an instantaneous, racy window.
	q, ok := queueMap.Get(clientID)
	require.True(t, ok)
	bundleFresh, requestFresh := d2NewBundle(t, endpoint, "fresh")
	require.NoError(t, q.Push(bundleFresh))
	require.Equal(t, 1, q.Size())
	require.False(t, state.HasPendingRequest(clientID))

	// Inject a readyForDispatch token carrying an ARBITRARY, non-matching
	// requestID - never dispatched for anything, and deliberately NOT
	// requestFresh's ID and NOT any completed-request's ID. Since recorded
	// is empty, the three-way rule's empty->kick arm must fire and dispatch
	// the queued head regardless of what the token's requestID is.
	d.readyForDispatch <- struct {
		clientID  string
		requestID string
	}{clientID: clientID, requestID: "kick-token-does-not-match-anything"}

	// The kick arm must dispatch the queued head.
	select {
	case cID := <-written:
		require.Equal(t, clientID, cID)
	case <-time.After(d2Bound):
		t.Fatal("empty-recorded readyForDispatch token failed to kick the queued request")
	}
	require.True(t, state.HasPendingRequest(clientID))
	cs := state.GetClientState(clientID)
	_, hasFresh := cs.GetPendingRequest(requestFresh)
	require.True(t, hasFresh)

	// No spurious cancel event as a side effect of the kick.
	select {
	case ev := <-canceled:
		t.Fatalf("unexpected cancel %s as a side effect of the kick", ev.requestID)
	case <-time.After(d2SilenceBound):
	}

	d.CompleteRequest(clientID, requestFresh)
	require.False(t, state.HasPendingRequest(clientID))
	require.True(t, q.IsEmpty())
}

// ---------------------------------------------------------------------
// Test 3 - stale readyForDispatch dropped, no double-dispatch (spec PR-0
// item 2 / Tests item 3).
// ---------------------------------------------------------------------

// A late/duplicate ready token carrying an OLD (already-completed) requestID,
// delivered while a DIFFERENT request is genuinely in flight for the same
// clientID, must be dropped: no cancel of the new context, no second
// dispatch. A double-dispatch would make the new request's real response
// unmatchable (AddPendingRequest ignores a 2nd pending, state.go:48-57).
//
// Codex MAJOR (finding 3): Write happens inside dispatchNextRequest BEFORE
// the pump returns to messagePump and records NEW's dispatched requestID in
// its sibling pump-local map (mirroring how clientContextMap is only
// assigned AFTER dispatchNextRequest returns, dispatcher.go:769-770). Merely
// observing NEW's write on the `written` channel is therefore not enough to
// guarantee recorded==requestNew yet - there is a window, between Write
// returning and the pump completing that recording, in which injecting the
// stale token could race the recording itself and land on the wrong arm.
// Fixed by forcing a full pump cycle boundary: dispatch an unrelated "sync"
// client's request right after NEW's and wait for ITS write. The pump is
// single-threaded and drains requestChannel strictly in FIFO order,
// completing one iteration (dispatch AND recording) before starting the
// next, so observing the sync client's write proves NEW's entire iteration,
// including the recording, has already completed.
func TestD2StaleReadyForDispatchDroppedNoDoubleDispatch(t *testing.T) {
	d, state, queueMap, network, endpoint := d2NewDispatcher(t)
	d.SetTimeout(5 * time.Second)

	clientID := "client1"
	written := make(chan string, 4)
	network.setOnWrite(func(cID string, data []byte) error {
		written <- cID
		return nil
	})
	canceled := make(chan d2CancelEvent, 4)
	d.SetOnRequestCanceled(func(cID, rID string, req ocpp.Request, err *ocpp.Error) {
		canceled <- d2CancelEvent{clientID: cID, requestID: rID, err: err}
	})

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()

	d.CreateClient(clientID)

	// "OLD": dispatch and complete a first request with nothing queued
	// behind it, so the pump's recorded dispatched-requestID for clientID
	// returns to empty/absent afterward.
	bundleOld, requestOld := d2NewBundle(t, endpoint, "old")
	require.NoError(t, d.SendRequest(clientID, bundleOld))
	select {
	case <-written:
	case <-time.After(d2Bound):
		t.Fatal("timed out waiting for OLD to be dispatched")
	}
	d.CompleteRequest(clientID, requestOld)
	require.False(t, state.HasPendingRequest(clientID))

	// "NEW": a different in-flight request now dispatched for the SAME
	// clientID (simulating a reconnect that reuses the ID).
	bundleNew, requestNew := d2NewBundle(t, endpoint, "new")
	require.NotEqual(t, requestOld, requestNew)
	require.NoError(t, d.SendRequest(clientID, bundleNew))
	select {
	case cID := <-written:
		require.Equal(t, clientID, cID)
	case <-time.After(d2Bound):
		t.Fatal("timed out waiting for NEW to be dispatched")
	}
	require.True(t, state.HasPendingRequest(clientID))

	// Barrier: force a full pump cycle boundary so recorded==requestNew is
	// GUARANTEED before the stale token below is even sent (see finding-3
	// comment above). An unrelated "sync" client's dispatch can only be
	// processed by the single-threaded pump AFTER NEW's entire iteration
	// (dispatch + recording) has completed, since requestChannel is FIFO.
	syncClientID := "syncClient"
	d.CreateClient(syncClientID)
	bundleSync, _ := d2NewBundle(t, endpoint, "sync")
	require.NoError(t, d.SendRequest(syncClientID, bundleSync))
	select {
	case cID := <-written:
		require.Equal(t, syncClientID, cID)
	case <-time.After(d2Bound):
		t.Fatal("timed out waiting for the sync client's dispatch (pump-cycle barrier)")
	}

	// PR-0: NOW inject a stale ready token carrying OLD's requestID.
	// recorded == requestNew (guaranteed set by the barrier above) !=
	// requestOld -> non-empty mismatch -> must be dropped.
	d.readyForDispatch <- struct {
		clientID  string
		requestID string
	}{clientID: clientID, requestID: requestOld}

	// No cancel of NEW's context.
	select {
	case ev := <-canceled:
		t.Fatalf("stale ready for OLD incorrectly canceled request %s", ev.requestID)
	case <-time.After(d2SilenceBound):
	}

	// No second dispatch (no extra Write call).
	select {
	case cID := <-written:
		t.Fatalf("stale ready for OLD incorrectly triggered a second dispatch for %s", cID)
	case <-time.After(d2SilenceBound):
	}

	// NEW's pending state must be exactly NEW, untouched.
	cs := state.GetClientState(clientID)
	_, hasNew := cs.GetPendingRequest(requestNew)
	require.True(t, hasNew)
	q, ok := queueMap.Get(clientID)
	require.True(t, ok)
	require.Equal(t, 1, q.Size())

	// Decisive: NEW's real completion must still work normally.
	d.CompleteRequest(clientID, requestNew)
	require.False(t, state.HasPendingRequest(clientID))
	require.True(t, q.IsEmpty())
}

// ---------------------------------------------------------------------
// Test 4 - DeleteClientAndWait ack (spec PR-0 item 3 / Tests item 4).
// ---------------------------------------------------------------------

// d2BlockingServer is a fake ws.Server whose Write blocks until released,
// letting a test deterministically wedge the pump goroutine (Write runs
// synchronously inside messagePump's dispatchNextRequest call) to create a
// controlled window in which to race other dispatcher calls, without any
// timing-based flakiness.
//
// Codex MINOR (finding 8): `entered` is closed (via sync.Once, so it is safe
// even though multiple Write calls may happen across a test's lifetime) the
// FIRST time Write is called, BEFORE it blocks on `release` - giving callers
// a deterministic "the pump is now inside (and blocked in) Write" signal, in
// place of a fixed time.Sleep "settle" wait.
type d2BlockingServer struct {
	ws.Server
	release chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (f *d2BlockingServer) Write(clientID string, data []byte) error {
	f.once.Do(func() { close(f.entered) })
	<-f.release
	return nil
}

// newD2BlockingServer constructs a d2BlockingServer with both channels ready.
func newD2BlockingServer() *d2BlockingServer {
	return &d2BlockingServer{release: make(chan struct{}), entered: make(chan struct{})}
}

type d2SelectiveBlockingServer struct {
	ws.Server
	blockClientID string
	release       chan struct{}
	entered       chan struct{}
	written       chan string
	once          sync.Once
}

func (f *d2SelectiveBlockingServer) Write(clientID string, data []byte) error {
	f.written <- clientID
	if clientID == f.blockClientID {
		f.once.Do(func() { close(f.entered) })
		<-f.release
	}
	return nil
}

func newD2SelectiveBlockingServer(blockClientID string) *d2SelectiveBlockingServer {
	return &d2SelectiveBlockingServer{
		blockClientID: blockClientID,
		release:       make(chan struct{}),
		entered:       make(chan struct{}),
		written:       make(chan string, 8),
	}
}

// 4(a): DeleteClientAndWait must actually WAIT for the pump to process the
// delete marker on the plain queue-removed branch, not merely reflect the
// synchronous queueMap.Remove call that happens before the marker is even
// sent.
//
// Codex MAJOR (finding 5): the original version of this test used the
// non-blocking d2FakeServer, so it only proved queueMap.Remove is
// synchronous - it never proved DeleteClientAndWait's ack actually waits for
// the pump's FIFO marker. Strengthened the same way as 4(b): wedge the pump
// on an unrelated client's blocking Write, launch DeleteClientAndWait,
// assert it does NOT return while wedged, then release and assert it
// returns promptly.
func TestD2DeleteClientAndWaitQueueRemovedBranch(t *testing.T) {
	queueMap := NewFIFOQueueMap(10)
	d := NewDefaultServerDispatcher(queueMap)
	var mutex sync.RWMutex
	state := NewServerState(&mutex)
	d.SetPendingRequestState(state)
	blocker := newD2BlockingServer()
	d.SetNetworkServer(blocker)
	d.SetTimeout(5 * time.Second)
	endpoint := &Server{}
	endpoint.AddProfile(ocpp.NewProfile("d2mock", &d2MockFeature{}))

	d.Start()
	require.True(t, d.IsRunning())
	defer func() {
		// Ensure the pump isn't left permanently wedged for TearDown/Stop.
		select {
		case <-blocker.release:
		default:
			close(blocker.release)
		}
		d.Stop()
	}()

	// Wedge the pump: dispatch a request for an unrelated client; its Write
	// call blocks synchronously inside the pump goroutine.
	blockerID := "blocker"
	d.CreateClient(blockerID)
	bundle, _ := d2NewBundle(t, endpoint, "x")
	require.NoError(t, d.SendRequest(blockerID, bundle))
	select {
	case <-blocker.entered:
	case <-time.After(d2Bound):
		t.Fatal("pump never entered the blocking Write call")
	}

	clientID := "client1"
	d.CreateClient(clientID)
	_, ok := queueMap.Get(clientID)
	require.True(t, ok)

	done := make(chan struct{})
	go func() {
		d.DeleteClientAndWait(clientID)
		close(done)
	}()

	// While the pump is still wedged, DeleteClientAndWait must not have
	// returned yet - proving it actually waits on the pump's FIFO marker,
	// not just the synchronous queueMap.Remove that already happened before
	// the marker was even sent.
	select {
	case <-done:
		t.Fatal("DeleteClientAndWait returned before the pump could have processed the delete marker")
	case <-time.After(150 * time.Millisecond):
	}

	_, ok = queueMap.Get(clientID)
	require.False(t, ok, "queueMap.Remove is synchronous and must have already run")

	// Release the pump: DeleteClientAndWait must now return PROMPTLY (well
	// under the production fallback timeout - see promptAckBound's comment),
	// proving it returned via the real ack rather than a long internal
	// timeout.
	close(blocker.release)
	select {
	case <-done:
	case <-time.After(promptAckBound):
		t.Fatal("DeleteClientAndWait did not return promptly after the pump processed the delete marker")
	}

	_, ok = queueMap.Get(clientID)
	require.False(t, ok, "queue should remain removed")
}

// 4(b): DeleteClientAndWait must not hang when a same-ID CreateClient races
// in and recreates the queue before the pump reaches the delete marker (the
// "queue exists" branch must ALSO close the ack, unconditionally). The pump
// is deterministically wedged (via d2BlockingServer) on an unrelated
// client's dispatch so the whole sequence below - Remove, marker send,
// CreateClient recreate - is guaranteed to land before the pump ever
// observes the marker, with no reliance on timing/goroutine scheduling.
//
// Codex MINOR (finding 8) + BLOCKER 4: replaced the fixed time.Sleep(100ms)
// "wait for the pump to enter Write" with the deterministic blocker.entered
// signal, and replaced the final d2Bound (2s) wait after releasing the pump
// with promptAckBound - proving DeleteClientAndWait returned because it saw
// the real queue-exists-branch ack, not because of a multi-second internal
// fallback timeout (see promptAckBound's comment for the contract point
// this pins for the implementer).
func TestD2DeleteClientAndWaitQueueRecreatedBranch(t *testing.T) {
	queueMap := NewFIFOQueueMap(10)
	d := NewDefaultServerDispatcher(queueMap)
	var mutex sync.RWMutex
	state := NewServerState(&mutex)
	d.SetPendingRequestState(state)
	blockerID := "blocker"
	blocker := newD2SelectiveBlockingServer(blockerID)
	d.SetNetworkServer(blocker)
	d.SetTimeout(5 * time.Second)
	endpoint := &Server{}
	endpoint.AddProfile(ocpp.NewProfile("d2mock", &d2MockFeature{}))

	d.Start()
	require.True(t, d.IsRunning())
	defer func() {
		// Ensure the pump isn't left permanently wedged for TearDown/Stop.
		select {
		case <-blocker.release:
		default:
			close(blocker.release)
		}
		d.Stop()
	}()

	clientID := "client1"
	d.CreateClient(clientID)

	// First establish an active in-flight timeout context for client1. The
	// recreated-queue delete branch must cancel and remove this old context,
	// otherwise a fresh reconnect request remains wedged until the old
	// deadline expires.
	bundleOld, _ := d2NewBundle(t, endpoint, "old")
	require.NoError(t, d.SendRequest(clientID, bundleOld))
	select {
	case cID := <-blocker.written:
		require.Equal(t, clientID, cID)
	case <-time.After(d2Bound):
		t.Fatal("timed out waiting for OLD to be dispatched")
	}
	require.True(t, state.HasPendingRequest(clientID))

	// Wedge the pump behind OLD's dispatch: an unrelated client's Write
	// blocks synchronously inside the pump goroutine.
	d.CreateClient(blockerID)
	bundleBlocker, _ := d2NewBundle(t, endpoint, "x")
	require.NoError(t, d.SendRequest(blockerID, bundleBlocker))
	select {
	case <-blocker.entered:
	case <-time.After(d2Bound):
		t.Fatal("pump never entered the blocking Write call")
	}
	select {
	case cID := <-blocker.written:
		require.Equal(t, blockerID, cID)
	case <-time.After(d2Bound):
		t.Fatal("timed out waiting for blocker write to be recorded")
	}

	_, ok := queueMap.Get(clientID)
	require.True(t, ok)

	done := make(chan struct{})
	go func() {
		d.DeleteClientAndWait(clientID)
		close(done)
	}()

	// While the pump is still wedged, DeleteClientAndWait must not have
	// returned yet (its marker is buffered behind the pump, unprocessed).
	select {
	case <-done:
		t.Fatal("DeleteClientAndWait returned before the pump could have processed the delete marker")
	case <-time.After(150 * time.Millisecond):
	}

	// Recreate the queue before the pump ever sees the delete marker.
	d.CreateClient(clientID)
	_, ok = queueMap.Get(clientID)
	require.True(t, ok)

	// Release the pump.
	close(blocker.release)

	select {
	case <-done:
	case <-time.After(promptAckBound):
		t.Fatal("DeleteClientAndWait did not return promptly on the queue-recreated branch")
	}

	_, ok = queueMap.Get(clientID)
	require.True(t, ok, "the recreated queue must survive: the delete marker's ack must close unconditionally, not by deleting the recreated queue")
	require.True(t, state.HasPendingRequest(clientID), "DeleteClientAndWait must not clear pending state; onClientDisconnected owns that clear")
	state.ClearClientPendingRequest(clientID)
	require.False(t, state.HasPendingRequest(clientID))

	bundleFresh, requestFresh := d2NewBundle(t, endpoint, "fresh")
	require.NoError(t, d.SendRequest(clientID, bundleFresh))
	select {
	case cID := <-blocker.written:
		require.Equal(t, clientID, cID)
	case <-time.After(promptAckBound):
		t.Fatal("fresh request did not dispatch promptly after recreated-queue delete ack; old timeout context may still be active")
	}

	cs := state.GetClientState(clientID)
	_, hasFresh := cs.GetPendingRequest(requestFresh)
	require.True(t, hasFresh)
	d.CompleteRequest(clientID, requestFresh)
}

// 4(c): DeleteClientAndWait returns immediately when the dispatcher isn't
// running.
func TestD2DeleteClientAndWaitReturnsImmediatelyWhenNotRunning(t *testing.T) {
	d, _, _, _, _ := d2NewDispatcher(t)
	require.False(t, d.IsRunning())

	done := make(chan struct{})
	go func() {
		d.DeleteClientAndWait("client1")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("DeleteClientAndWait should return immediately when the dispatcher isn't running")
	}
}

func TestD2DeleteClientAndWaitReturnsImmediatelyWhenStopInProgress(t *testing.T) {
	queueMap := NewFIFOQueueMap(10)
	d := NewDefaultServerDispatcher(queueMap)
	var mutex sync.RWMutex
	state := NewServerState(&mutex)
	d.SetPendingRequestState(state)
	blocker := newD2BlockingServer()
	d.SetNetworkServer(blocker)
	d.SetTimeout(5 * time.Second)
	endpoint := &Server{}
	endpoint.AddProfile(ocpp.NewProfile("d2mock", &d2MockFeature{}))

	d.Start()
	require.True(t, d.IsRunning())

	blockerID := "blocker"
	d.CreateClient(blockerID)
	bundle, _ := d2NewBundle(t, endpoint, "x")
	require.NoError(t, d.SendRequest(blockerID, bundle))
	select {
	case <-blocker.entered:
	case <-time.After(d2Bound):
		t.Fatal("pump never entered the blocking Write call")
	}

	clientID := "client1"
	d.CreateClient(clientID)

	d.mutex.Lock()
	d.running = false
	close(d.stoppedC)
	doneC := d.doneC
	d.mutex.Unlock()

	done := make(chan struct{})
	go func() {
		d.DeleteClientAndWait(clientID)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(promptAckBound):
		t.Fatal("DeleteClientAndWait should return immediately while Stop is in progress, even if doneC is still open")
	}

	close(blocker.release)
	select {
	case <-doneC:
	case <-time.After(d2Bound):
		t.Fatal("pump did not exit after releasing the blocking Write")
	}
}

// 4(d): DeleteClientAndWait must not hang when Stop races with a disconnect
// while the pump can never reach the delete marker on its own. If the delete
// marker was already accepted while running, it unblocks when Stop closes
// doneC; if Stop flips running=false first, DeleteClientAndWait returns
// immediately per the PR-0 contract.
//
// Codex MINOR (finding 8): replaced the fixed time.Sleep(100ms) with the
// deterministic blocker.entered signal.
func TestD2DeleteClientAndWaitUnblocksOnStop(t *testing.T) {
	queueMap := NewFIFOQueueMap(10)
	d := NewDefaultServerDispatcher(queueMap)
	var mutex sync.RWMutex
	state := NewServerState(&mutex)
	d.SetPendingRequestState(state)
	blocker := newD2BlockingServer()
	d.SetNetworkServer(blocker)
	d.SetTimeout(5 * time.Second)
	endpoint := &Server{}
	endpoint.AddProfile(ocpp.NewProfile("d2mock", &d2MockFeature{}))

	d.Start()
	require.True(t, d.IsRunning())

	blockerID := "blocker"
	d.CreateClient(blockerID)
	bundle, _ := d2NewBundle(t, endpoint, "x")
	require.NoError(t, d.SendRequest(blockerID, bundle))
	select {
	case <-blocker.entered:
	case <-time.After(d2Bound):
		t.Fatal("pump never entered the blocking Write call")
	}

	clientID := "client1"
	d.CreateClient(clientID)

	waitDone := make(chan struct{})
	go func() {
		d.DeleteClientAndWait(clientID)
		close(waitDone)
	}()

	stopDone := make(chan struct{})
	go func() {
		d.Stop()
		close(stopDone)
	}()

	// Stop itself cannot return yet: the pump is still wedged. Depending on
	// whether DeleteClientAndWait observed running=true before Stop acquired
	// the lock, it may still be waiting on the pump marker/doneC or may have
	// already returned immediately after Stop flipped running=false.
	select {
	case <-stopDone:
		t.Fatal("Stop returned before the pump could exit")
	case <-time.After(150 * time.Millisecond):
	}

	// Release the pump: it must observe stoppedC, exit, and close doneC,
	// which unblocks BOTH the pending DeleteClientAndWait and Stop.
	close(blocker.release)

	select {
	case <-waitDone:
	case <-time.After(d2Bound):
		t.Fatal("DeleteClientAndWait did not unblock on Stop")
	}
	select {
	case <-stopDone:
	case <-time.After(d2Bound):
		t.Fatal("Stop did not return")
	}
}

// 4(e) - codex MAJOR (finding 6): DeleteClientAndWait must CLEAR the
// per-client dispatched-requestID tracking (spec PR-0 item 3: "the pump's
// delete branch also clears the per-client dispatched-requestID tracking"),
// not just the queue/timeout-context.
//
// Black-box design note: TestD2EmptyRecordedKicksQueuedRequest's comment
// already establishes that the three-way rule's "match" and "empty->kick"
// arms take the IDENTICAL action (cancel-if-active + dispatch-if-non-empty);
// only "non-empty mismatch" differs (drop). That means a stale token that
// exactly equals the pre-delete requestID dispatching successfully is only a
// WEAK signal - it would also "pass" under a buggy impl that leaks the
// pre-delete value, since a leftover value equal to the injected token is
// itself a (harmless) match. The DECISIVE half is a token that does NOT
// equal the pre-delete requestID: that can only dispatch if recorded is
// genuinely empty/absent (a leaked non-empty value would instead send it
// down the mismatch->drop branch, silently swallowing the fresh request). A
// wrong impl that fails to clear tracking on delete is caught by that second
// subtest failing to ever dispatch.
func TestD2DeleteClearsRecordedRequestIDTracking(t *testing.T) {
	runOnce := func(t *testing.T, staleToken func(preDeleteRequestID string) string) {
		d, state, queueMap, network, endpoint := d2NewDispatcher(t)
		d.SetTimeout(5 * time.Second)

		clientID := "client1"
		written := make(chan string, 4)
		network.setOnWrite(func(cID string, data []byte) error {
			written <- cID
			return nil
		})

		d.Start()
		require.True(t, d.IsRunning())
		defer d.Stop()

		d.CreateClient(clientID)
		bundleOld, requestOld := d2NewBundle(t, endpoint, "old")
		require.NoError(t, d.SendRequest(clientID, bundleOld))
		select {
		case <-written:
		case <-time.After(d2Bound):
			t.Fatal("timed out waiting for the pre-delete request to be dispatched")
		}
		require.True(t, state.HasPendingRequest(clientID))

		// Delete while a request is genuinely in flight (recorded ==
		// requestOld) and wait for the pump to process the delete.
		d.DeleteClientAndWait(clientID)
		_, stillExists := queueMap.Get(clientID)
		require.False(t, stillExists)

		// Recreate with the SAME clientID (simulating a reconnect).
		d.CreateClient(clientID)
		q, ok := queueMap.Get(clientID)
		require.True(t, ok)
		require.True(t, state.HasPendingRequest(clientID), "DeleteClientAndWait is only the queue/pump barrier; onClientDisconnected owns pending-state clearing")
		state.ClearClientPendingRequest(clientID)
		require.False(t, state.HasPendingRequest(clientID))

		// Push a fresh request directly onto the recreated queue, bypassing
		// the channel signal, so we freeze "recorded should-be-empty, queue
		// non-empty, nothing dispatched yet" as a fixed precondition instead
		// of a race.
		bundleFresh, requestFresh := d2NewBundle(t, endpoint, "fresh")
		require.NoError(t, q.Push(bundleFresh))

		d.readyForDispatch <- struct {
			clientID  string
			requestID string
		}{clientID: clientID, requestID: staleToken(requestOld)}

		select {
		case cID := <-written:
			require.Equal(t, clientID, cID)
		case <-time.After(d2Bound):
			t.Fatal("stale readyForDispatch after delete+recreate failed to dispatch the fresh request - recorded requestID tracking may not have been cleared on delete")
		}
		cs := state.GetClientState(clientID)
		_, hasFresh := cs.GetPendingRequest(requestFresh)
		require.True(t, hasFresh)

		d.CompleteRequest(clientID, requestFresh)
	}

	t.Run("token_equals_pre_delete_id_weak_check", func(t *testing.T) {
		runOnce(t, func(preDeleteRequestID string) string { return preDeleteRequestID })
	})
	t.Run("token_differs_from_pre_delete_id_decisive_check", func(t *testing.T) {
		runOnce(t, func(preDeleteRequestID string) string { return "not-" + preDeleteRequestID })
	})
}
