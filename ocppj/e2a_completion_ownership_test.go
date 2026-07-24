package ocppj

// PR-E2a (tasks/e2-server-context-aware-send.md, "## A. E2a - completion
// ownership (pre-existing bug fixes)") RED-FIRST test suite.
//
// This file lives in `package ocppj` (not `ocppj_test`) so it can reach
// DefaultServerDispatcher's unexported pump internals (readyForDispatch,
// timerC, stoppedC, waitForTimeout, clientTimeoutContext) exactly like
// d2_token_identity_test.go and e1a_completion_ownership_test.go (the
// already-merged CLIENT-side equivalent of this exact work, E1a). It defines
// its own minimal in-package fakes (prefixed e2a) to avoid any dependency on
// the ocppj_test package.
//
// RED-FIRST discipline: every test below references the PR-E2a surface
// exactly as the spec (tasks/e2-server-context-aware-send.md, SS A) names it.
// Against today's codebase:
//   - DefaultServerDispatcher.CompleteRequest returns void (not bool)
//   - completeRequestOwned does not exist
//   - waitForTimeout takes (clientID, clientCtx) only - no generation-pinned
//     stoppedC/timerC parameters (B2/B3, folded into E2a per the spec)
//   - server.go's CALL_RESULT/CALL_ERROR sites fire responseHandler/
//     errorHandler unconditionally (no CompleteRequest-return guard)
//   - the SetTimeout(0) dispatch guard lacks a HasPendingRequest check (A3)
//   - the write-error path in dispatchNextRequest calls the public,
//     signaling CompleteRequest from the pump goroutine itself (A1)
//
// This whole file is EXPECTED to fail compilation because of the bool-return
// and waitForTimeout-arity references below - that IS part of the intended
// red state pinning the PR-E2a contract. Two tests (A6.1, A6.8) are ALSO
// runtime-red independent of compilation (they reproduce a real pump
// self-deadlock via fault injection, guarded by a bounded watchdog so a
// broken build fails cleanly instead of wedging the test run).
//
// Spec tests implemented: A6.1, A6.2, A6.3, A6.4, A6.5, A6.6, A6.8 (A6.7 -
// "existing suites still green" - is not a written test per the spec).

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/enesismail/ocpp-go/ocpp"
	"github.com/enesismail/ocpp-go/ws"
)

// ============================================================================
// In-package mock feature/request (package ocppj cannot import ocppj_test's
// MockFeature/newMockRequest - see file comments in d2/e1a for the same note)
// ============================================================================

const e2aMockFeatureName = "E2aMock"

type e2aMockRequest struct {
	MockValue string `json:"mockValue"`
}

func (r *e2aMockRequest) GetFeatureName() string { return e2aMockFeatureName }

type e2aMockConfirmation struct {
	MockValue string `json:"mockValue"`
}

func (c *e2aMockConfirmation) GetFeatureName() string { return e2aMockFeatureName }

type e2aMockFeature struct{}

func (f *e2aMockFeature) GetFeatureName() string        { return e2aMockFeatureName }
func (f *e2aMockFeature) GetRequestType() reflect.Type  { return reflect.TypeOf(e2aMockRequest{}) }
func (f *e2aMockFeature) GetResponseType() reflect.Type { return reflect.TypeOf(e2aMockConfirmation{}) }

// e2aNewBundle creates a fresh Call/RequestBundle pair via a throwaway Server
// endpoint (mirrors d2NewBundle/e1aNewBundle), returning the bundle and its
// generated unique ID.
func e2aNewBundle(t *testing.T, endpoint *Server, value string) (RequestBundle, string) {
	t.Helper()
	req := &e2aMockRequest{MockValue: value}
	call, err := endpoint.CreateCall(req)
	require.NoError(t, err)
	data, err := call.MarshalJSON()
	require.NoError(t, err)
	return RequestBundle{Call: call, Data: data}, call.UniqueId
}

// ============================================================================
// Minimal in-package fake ws.Channel - only ID() is ever exercised by the
// code paths under test.
// ============================================================================

type e2aChannel struct{ id string }

func (c *e2aChannel) ID() string                               { return c.id }
func (c *e2aChannel) RemoteAddr() net.Addr                     { return nil }
func (c *e2aChannel) TLSConnectionState() *tls.ConnectionState { return nil }
func (c *e2aChannel) IsConnected() bool                        { return true }

// ============================================================================
// Minimal in-package fake ws.Server, recording every Write call (clientID +
// data) and allowing a per-test hook, mirroring d2FakeServer/e1aCountingClient.
// ============================================================================

type e2aWrite struct {
	clientID string
	data     []byte
}

type e2aServer struct {
	ws.Server // embedded nil interface; only Write is overridden below

	mu      sync.Mutex
	onWrite func(clientID string, data []byte) error
	writes  []e2aWrite
}

func (s *e2aServer) Write(clientID string, data []byte) error {
	s.mu.Lock()
	s.writes = append(s.writes, e2aWrite{clientID: clientID, data: data})
	cb := s.onWrite
	s.mu.Unlock()
	if cb != nil {
		return cb(clientID, data)
	}
	return nil
}

func (s *e2aServer) setOnWrite(cb func(clientID string, data []byte) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onWrite = cb
}

func (s *e2aServer) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.writes)
}

func (s *e2aServer) writeCountFor(clientID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, w := range s.writes {
		if w.clientID == clientID {
			n++
		}
	}
	return n
}

func (s *e2aServer) writesSnapshot() []e2aWrite {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]e2aWrite, len(s.writes))
	copy(out, s.writes)
	return out
}

// e2aNewDispatcher wires a DefaultServerDispatcher exactly like
// ServerDispatcherTestSuite.SetupTest (dispatcher_test.go) / d2NewDispatcher,
// returning the CONCRETE type so these white-box tests can reach unexported
// fields/methods (the not-yet-existing PR-E2a surface).
func e2aNewDispatcher(t *testing.T) (d *DefaultServerDispatcher, state ServerState, queueMap ServerQueueMap, network *e2aServer, endpoint *Server) {
	t.Helper()
	queueMap = NewFIFOQueueMap(10)
	d = NewDefaultServerDispatcher(queueMap)
	var mutex sync.RWMutex
	state = NewServerState(&mutex)
	d.SetPendingRequestState(state)
	network = &e2aServer{}
	d.SetNetworkServer(network)
	endpoint = &Server{}
	endpoint.AddProfile(ocpp.NewProfile("e2amock", &e2aMockFeature{}))
	return
}

const e2aBound = 2 * time.Second               // generous bound for events that MUST happen
const e2aSilenceBound = 300 * time.Millisecond // window to prove an event does NOT happen

// ============================================================================
// A6.3 - CompleteRequest returns false for a non-front / already-completed
// id, and fires nothing.
// ============================================================================

// TestE2aCompleteRequestReturnsFalseForNonFrontOrAlreadyCompleted pins the
// A4/A5 bool-return contract directly on the dispatcher: PR-E2a changes
// ServerDispatcher.CompleteRequest(clientID, requestID string) from void to
// bool. This reference alone (`ok := d.CompleteRequest(...)`) makes the
// package fail to compile against today's void signature - that IS the
// intended red state for this test.
func TestE2aCompleteRequestReturnsFalseForNonFrontOrAlreadyCompleted(t *testing.T) {
	d, state, queueMap, network, endpoint := e2aNewDispatcher(t)
	clientID := "e2a-a3-client"

	written := make(chan string, 8)
	network.setOnWrite(func(cID string, data []byte) error {
		written <- cID
		return nil
	})

	var canceled int32
	d.SetOnRequestCanceled(func(cID, rID string, req ocpp.Request, err *ocpp.Error) {
		atomic.AddInt32(&canceled, 1)
	})

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()

	d.CreateClient(clientID)
	bundleA, requestA := e2aNewBundle(t, endpoint, "a")
	require.NoError(t, d.SendRequest(clientID, bundleA))
	select {
	case <-written:
	case <-time.After(e2aBound):
		t.Fatal("timed out waiting for A to be dispatched")
	}
	require.True(t, state.HasPendingRequest(clientID))

	bundleB, requestB := e2aNewBundle(t, endpoint, "b")
	require.NoError(t, d.SendRequest(clientID, bundleB))
	q, ok := queueMap.Get(clientID)
	require.True(t, ok)
	require.Equal(t, 2, q.Size())

	// A non-front id (neither A nor B) must return false and must not touch
	// the queue.
	okWrong := d.CompleteRequest(clientID, "wrong-id-not-A-or-B")
	assert.False(t, okWrong, "CompleteRequest must return false for a non-front id")
	assert.Equal(t, 2, q.Size(), "queue must be untouched by a losing CompleteRequest")
	assert.True(t, state.HasPendingRequest(clientID))

	// The genuine front (A) completes normally: true.
	okA := d.CompleteRequest(clientID, requestA)
	assert.True(t, okA, "CompleteRequest must return true for the genuine front id")
	assert.Equal(t, 1, q.Size())

	// Now B is front. A stale repeat completion of the now-already-completed
	// A must return false and must not disturb B.
	okAAgain := d.CompleteRequest(clientID, requestA)
	assert.False(t, okAAgain, "a stale repeat CompleteRequest for an already-completed id must return false")
	assert.Equal(t, 1, q.Size(), "B must be untouched by the stale repeat completion")

	// No cancel must ever fire as a side effect of any losing call.
	assert.Equal(t, int32(0), atomic.LoadInt32(&canceled))

	// B still completes normally.
	okB := d.CompleteRequest(clientID, requestB)
	assert.True(t, okB)
	assert.True(t, q.IsEmpty())
}

// ============================================================================
// A6.4 - SetTimeout(0) with two queued requests => request 1 written exactly
// once. (A3 regression: the dispatch guard lacks a HasPendingRequest check.)
// ============================================================================

// TestE2aSetTimeoutZeroWritesFrontExactlyOnce reproduces the A3 bug: with
// SetTimeout(0), clientCtx is never made active (dispatcher.go: "only
// populated when d.timeout > 0"), so the dispatch guard's `rdy` is
// permanently true. Since dispatchNextRequest PEEKS (never pops) the queue
// front, queuing a SECOND request for the same client re-enters the dispatch
// block and rewrites the still in-flight FRONT (A) to the wire a second
// time - purely because the requestChannel event for B's SendRequest looked
// "ready to transmit". This is a purely behavioral (runtime) regression: no
// new API is referenced here, only the mock's Write call count.
func TestE2aSetTimeoutZeroWritesFrontExactlyOnce(t *testing.T) {
	d, state, _, network, endpoint := e2aNewDispatcher(t)
	d.SetTimeout(0) // A3: clientCtx never becomes active

	clientID := "e2a-a3-timeout0-client"
	writeSeen := make(chan struct{}, 8)
	network.setOnWrite(func(cID string, data []byte) error {
		writeSeen <- struct{}{}
		return nil
	})

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()

	d.CreateClient(clientID)
	bundleA, requestA := e2aNewBundle(t, endpoint, "a")
	require.NoError(t, d.SendRequest(clientID, bundleA))

	select {
	case <-writeSeen:
	case <-time.After(e2aBound):
		t.Fatal("timed out waiting for A to be dispatched")
	}
	require.True(t, state.HasPendingRequest(clientID))
	require.Equal(t, 1, network.writeCountFor(clientID), "exactly one write for A right after dispatch")

	// Queue B behind A. On master, this SendRequest's requestChannel event
	// re-derives `rdy` from `!clientCtx.isActive()` (true, since
	// SetTimeout(0) never activates clientCtx) instead of also checking
	// HasPendingRequest - so the dispatch block runs again and rewrites the
	// still-pending FRONT (A, since B is behind it and dispatchNextRequest
	// only Peeks) to the wire.
	bundleB, _ := e2aNewBundle(t, endpoint, "b")
	require.NoError(t, d.SendRequest(clientID, bundleB))

	// Give the pump a full silence window before asserting the final count,
	// so a delayed spurious write is not missed.
	select {
	case <-writeSeen:
	case <-time.After(e2aSilenceBound):
	}

	writes := network.writesSnapshot()
	var forClient []e2aWrite
	for _, w := range writes {
		if w.clientID == clientID {
			forClient = append(forClient, w)
		}
	}

	assert.Equal(t, 1, len(forClient), "request A must be written exactly once with SetTimeout(0) and B merely queued behind it")
	if len(forClient) >= 2 {
		// Decisive: confirm the SPECIFIC bug - the second write re-sends A's
		// own request id, rather than B ever having been legitimately
		// dispatched (which would be an entirely different, non-A3 bug).
		assert.Contains(t, string(forClient[1].data), requestA,
			"the spurious second write re-sent A's own request id - the SetTimeout(0) dispatch guard lacks the HasPendingRequest check (A3)")
	}
}

// ============================================================================
// A6.5 - A losing CompleteRequest (returns false) => responseHandler /
// errorHandler do NOT fire. (The A4 handler guard at server.go CALL_RESULT/
// CALL_ERROR call sites.)
// ============================================================================

// TestE2aLosingCompleteRequestDoesNotFireResponseHandler drives the scenario
// entirely by hand (no running pump), mirroring e1a's Finding-1 technique on
// the client side: with a live pump, its own readiness wakeup from A's
// completion could race ahead of this test's own state manipulation and
// change what "front" means before the assertions run. Deterministic,
// single-goroutine construction removes that race entirely.
//
// This test compiles fine in isolation (it calls Server.ocppMessageHandler,
// not CompleteRequest's bool return, except for one bool cleanup call at the
// very end) but is RUNTIME-red against today's server.go: the CALL_RESULT
// call site (server.go ~:313) invokes responseHandler UNCONDITIONALLY,
// regardless of whether CompleteRequest actually won ownership.
func TestE2aLosingCompleteRequestDoesNotFireResponseHandler(t *testing.T) {
	d, state, queueMap, network, endpoint := e2aNewDispatcher(t)
	// No d.Start() - hand-driven, see comment above.
	clientID := "e2a-guard-client"
	channel := &e2aChannel{id: clientID}

	s := NewServer(network, d, state, ocpp.NewProfile("e2amock", &e2aMockFeature{}))

	q := queueMap.GetOrCreate(clientID)

	bundleA, requestA := e2aNewBundle(t, endpoint, "guard-a")
	state.AddPendingRequest(clientID, requestA, bundleA.Call.Payload)
	require.NoError(t, q.Push(bundleA))

	var responseCalls int32
	s.SetResponseHandler(func(client ws.Channel, response ocpp.Response, requestId string) {
		atomic.AddInt32(&responseCalls, 1)
	})

	callResultA := []byte(fmt.Sprintf(`[3,"%s",{"mockValue":"resp-a"}]`, requestA))

	// Genuine completion: A is the real front. responseHandler must fire once.
	err := s.ocppMessageHandler(channel, callResultA)
	require.NoError(t, err)
	require.Equal(t, int32(1), atomic.LoadInt32(&responseCalls))
	require.False(t, state.HasPendingRequest(clientID))
	require.True(t, q.IsEmpty())

	// Push B directly onto the (now empty) queue. The pump is not running,
	// so B is queued but deliberately NOT marked pending - matches reality:
	// nothing has dispatched it yet, and CompleteRequest's identity check
	// only ever consults the QUEUE FRONT, never pendingRequestState.
	bundleB, requestB := e2aNewBundle(t, endpoint, "guard-b")
	require.NoError(t, q.Push(bundleB))
	require.Equal(t, 1, q.Size())

	// Re-add A to pendingRequestState (legal now - it was cleared above) so
	// ParseMessage's OWN pending-check passes and the stale message reaches
	// the CompleteRequest guard inside ocppMessageHandler at all.
	state.AddPendingRequest(clientID, requestA, bundleA.Call.Payload)
	require.True(t, state.HasPendingRequest(clientID))

	// Deliver the SAME (now stale) CALL_RESULT for A again. The queue front
	// is now B, so CompleteRequest(clientID, A) must LOSE (front mismatch).
	// The A4 guard requires ocppMessageHandler to NOT invoke responseHandler
	// a second time - today it does, unconditionally.
	err = s.ocppMessageHandler(channel, callResultA)
	require.NoError(t, err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&responseCalls),
		"a stale CALL_RESULT that loses CompleteRequest's ownership race must NOT invoke responseHandler again")

	// B must be untouched by the losing attempt.
	require.Equal(t, 1, q.Size())
	el := q.Peek()
	bundle, ok := el.(RequestBundle)
	require.True(t, ok)
	assert.Equal(t, requestB, bundle.Call.UniqueId, "front must still be B")
	// Deliberately no further CompleteRequest(clientID, requestB) call here:
	// this test never Starts the pump, so nothing ever drains the cap-1
	// readyForDispatch buffer. A winning completion's blocking signal send
	// would therefore wedge the GREEN suite forever (BLOCKER-2 in
	// findings/e2a-tests-fable-review.md) - the guard contract this test
	// exists to pin is already fully covered above (the winning first
	// delivery fired once; the losing stale delivery did not fire again;
	// B is untouched by the loss).
}

// TestE2aLosingCompleteRequestDoesNotFireErrorHandler is the CALL_ERROR
// sibling of TestE2aLosingCompleteRequestDoesNotFireResponseHandler, pinning
// the identical guard at the server.go CALL_ERROR call site (~:323).
func TestE2aLosingCompleteRequestDoesNotFireErrorHandler(t *testing.T) {
	d, state, queueMap, network, endpoint := e2aNewDispatcher(t)
	clientID := "e2a-guard-err-client"
	channel := &e2aChannel{id: clientID}

	s := NewServer(network, d, state, ocpp.NewProfile("e2amock", &e2aMockFeature{}))

	q := queueMap.GetOrCreate(clientID)

	bundleA, requestA := e2aNewBundle(t, endpoint, "guard-err-a")
	state.AddPendingRequest(clientID, requestA, bundleA.Call.Payload)
	require.NoError(t, q.Push(bundleA))

	var errorCalls int32
	s.SetErrorHandler(func(client ws.Channel, err *ocpp.Error, details interface{}) {
		atomic.AddInt32(&errorCalls, 1)
	})

	callErrorA := []byte(fmt.Sprintf(`[4,"%s","GenericError","boom",{}]`, requestA))

	err := s.ocppMessageHandler(channel, callErrorA)
	require.NoError(t, err)
	require.Equal(t, int32(1), atomic.LoadInt32(&errorCalls))
	require.False(t, state.HasPendingRequest(clientID))
	require.True(t, q.IsEmpty())

	bundleB, requestB := e2aNewBundle(t, endpoint, "guard-err-b")
	require.NoError(t, q.Push(bundleB))
	require.Equal(t, 1, q.Size())

	state.AddPendingRequest(clientID, requestA, bundleA.Call.Payload)
	require.True(t, state.HasPendingRequest(clientID))

	err = s.ocppMessageHandler(channel, callErrorA)
	require.NoError(t, err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&errorCalls),
		"a stale CALL_ERROR that loses CompleteRequest's ownership race must NOT invoke errorHandler again")

	require.Equal(t, 1, q.Size())
	el := q.Peek()
	bundle, ok := el.(RequestBundle)
	require.True(t, ok)
	assert.Equal(t, requestB, bundle.Call.UniqueId, "front must still be B")
	// Deliberately no further CompleteRequest(clientID, requestB) call here -
	// see the identical comment at the end of
	// TestE2aLosingCompleteRequestDoesNotFireResponseHandler (BLOCKER-2 in
	// findings/e2a-tests-fable-review.md).
}

// ============================================================================
// A6.2 - timeout and CALL_RESULT racing on the same request => exactly one of
// {responseHandler, onRequestCanceled} fires; the next queued request is
// still dispatched (not silently discarded). (A2 regression; -race, looped.)
// ============================================================================

// e2aBlockingPeekQueue wraps a RequestQueue to deterministically force the A2
// completion race. Once ARMED (via arm(), which the test calls only AFTER the
// first request has been dispatched — so the request's own dispatch Peek is not
// intercepted), the first COMPLETION operation blocks after computing its result
// until released; every other call passes straight through.
//
// It intercepts BOTH Peek() and PopIf() because the two code shapes it must
// straddle differ: today's (buggy) server completion is Peek-then-compare-then-
// Pop (the non-atomic TOCTOU the A2 bug lives in), while the fixed completion
// routes through the atomic RequestQueue.PopIf (completeRequestOwned). Blocking
// only Peek would miss the fixed path entirely and time out (false-red); blocking
// only PopIf would miss the master reproduction.
type e2aBlockingPeekQueue struct {
	RequestQueue
	armed     int32 // set once the initial dispatch is done; only then do completions block
	triggered int32 // CAS-guarded: only the chronologically-first armed completion blocks
	entered   chan struct{}
	release   chan struct{}
}

func newE2aBlockingPeekQueue(inner RequestQueue) *e2aBlockingPeekQueue {
	return &e2aBlockingPeekQueue{
		RequestQueue: inner,
		entered:      make(chan struct{}),
		release:      make(chan struct{}),
	}
}

// arm enables completion-blocking. Called after the first request is dispatched
// so that request's own dispatch-time Peek passes through unblocked.
func (q *e2aBlockingPeekQueue) arm() { atomic.StoreInt32(&q.armed, 1) }

// blockIfFirst blocks the first armed completion op after its result is computed
// (and the underlying queue lock is released), so the racing completer can still
// proceed through the now-unlocked queue.
func (q *e2aBlockingPeekQueue) blockIfFirst() {
	if atomic.LoadInt32(&q.armed) == 1 && atomic.CompareAndSwapInt32(&q.triggered, 0, 1) {
		close(q.entered)
		<-q.release
	}
}

// Peek is the master (pre-fix) completion path: CompleteRequest and the timeout
// arm both Peek-then-Pop. The blocked Peek holds a stale "front is A" snapshot
// open across the race.
func (q *e2aBlockingPeekQueue) Peek() interface{} {
	el := q.RequestQueue.Peek()
	q.blockIfFirst()
	return el
}

// PopIf is the fixed completion path (completeRequestOwned → PopIf). The real
// PopIf pops atomically under the queue lock and releases it; then we block, so
// the racing completer's PopIf sees the updated front and its predicate fails
// (exactly-once by atomicity).
func (q *e2aBlockingPeekQueue) PopIf(pred func(interface{}) bool) (interface{}, bool) {
	el, ok := q.RequestQueue.PopIf(pred)
	q.blockIfFirst()
	return el, ok
}

// TestE2aTimeoutRaceCallResultExactlyOnce forces the interleaving
// deterministically (see e2aBlockingPeekQueue) rather than relying purely on
// wall-clock luck, then loops the whole deterministic construction so the
// suite is exercised repeatedly under `go test -race` per the spec.
func TestE2aTimeoutRaceCallResultExactlyOnce(t *testing.T) {
	const iterations = 10
	for i := 0; i < iterations; i++ {
		i := i
		t.Run(fmt.Sprintf("iteration_%d", i), func(t *testing.T) {
			queueMap := NewFIFOQueueMap(10)
			d := NewDefaultServerDispatcher(queueMap)
			var mutex sync.RWMutex
			state := NewServerState(&mutex)
			d.SetPendingRequestState(state)
			network := &e2aServer{}
			d.SetNetworkServer(network)
			endpoint := &Server{}
			endpoint.AddProfile(ocpp.NewProfile("e2amock", &e2aMockFeature{}))
			s := NewServer(network, d, state, ocpp.NewProfile("e2amock", &e2aMockFeature{}))

			// MAJOR-1 (findings/e2a-tests-fable-review.md): shortTimeout must
			// be large enough that the post-deadline sleep below (150ms
			// margin) cannot itself contain a full timeout cycle for an
			// eagerly-dispatched B - otherwise B can legitimately time out
			// DURING the sleep (its own, unrelated, genuine timeout),
			// inflating the exactly-once counters with B's own lifecycle and
			// false-redding an otherwise-correct fix. 250ms keeps the 150ms
			// margin well under B's own timeout.
			const shortTimeout = 250 * time.Millisecond
			d.SetTimeout(shortTimeout)

			clientID := fmt.Sprintf("e2a-a2-client-%d", i)
			channel := &e2aChannel{id: clientID}

			blockingQueue := newE2aBlockingPeekQueue(NewFIFOClientQueue(10))
			queueMap.Add(clientID, blockingQueue)

			// MINOR-3 (findings/e2a-tests-fable-review.md): guarantee
			// release is closed even if the `entered` wait below fatals, so
			// the CALL_RESULT handler goroutine (if it later reaches the
			// armed CAS) cannot park forever on <-blockingQueue.release.
			// sync.Once lets this cleanup and the normal-path release below
			// safely coexist without a double-close panic.
			var releaseOnce sync.Once
			releaseBlockingQueue := func() { releaseOnce.Do(func() { close(blockingQueue.release) }) }
			t.Cleanup(releaseBlockingQueue)

			written := make(chan string, 8)
			network.setOnWrite(func(cID string, data []byte) error {
				written <- cID
				return nil
			})

			// Bundles are created before the handlers below so their
			// generated ids (requestA in particular) are in scope for the
			// per-request filtering the handlers need (MAJOR-1).
			bundleA, requestA := e2aNewBundle(t, endpoint, "a")
			bundleB, requestB := e2aNewBundle(t, endpoint, "b")

			// MAJOR-1 (findings/e2a-tests-fable-review.md): count fires
			// PER-REQUEST, filtered to requestA - the exactly-once invariant
			// under test is about the single raced request A, not global
			// handler activity. Without this filter, B's own (entirely
			// unrelated, legitimate) dispatch and eventual response/timeout
			// could conflate with A's race and false-red a correct fix.
			var responseCalls, cancelCalls int32
			s.SetResponseHandler(func(client ws.Channel, response ocpp.Response, requestId string) {
				if requestId == requestA {
					atomic.AddInt32(&responseCalls, 1)
				}
			})
			d.SetOnRequestCanceled(func(cID, rID string, req ocpp.Request, err *ocpp.Error) {
				if rID == requestA {
					atomic.AddInt32(&cancelCalls, 1)
				}
			})

			d.Start()
			require.True(t, d.IsRunning())
			defer d.Stop()

			require.NoError(t, d.SendRequest(clientID, bundleA))
			select {
			case <-written:
			case <-time.After(e2aBound):
				t.Fatal("timed out waiting for A to be dispatched")
			}
			require.True(t, state.HasPendingRequest(clientID))
			// Arm ONLY now: A's own dispatch Peek has already happened above, so
			// arming here means the next intercepted op is a completion, not the
			// dispatch. (A is now in-flight with an active timeout ctx, so B's
			// SendRequest below sets rdy=false and does not trigger a dispatch Peek.)
			blockingQueue.arm()

			require.NoError(t, d.SendRequest(clientID, bundleB))
			require.Equal(t, 2, blockingQueue.Size())

			// Deliver the GENUINE CALL_RESULT for A on a simulated read goroutine.
			// Its completion op (Peek on master, PopIf on the fixed impl) is the
			// first armed op, so it blocks — holding a stale "A is front" snapshot
			// (master) or having already atomically popped A (fixed).
			callResultA := []byte(fmt.Sprintf(`[3,"%s",{"mockValue":"resp-a"}]`, requestA))
			handlerDone := make(chan error, 1)
			go func() {
				handlerDone <- s.ocppMessageHandler(channel, callResultA)
			}()

			select {
			case <-blockingQueue.entered:
			case <-time.After(e2aBound):
				t.Fatal("timed out waiting for the CALL_RESULT's completion to reach (and block in) the queue")
			}

			// Let A's real deadline elapse while the first completer is blocked,
			// so the timeout arm runs and RACES the blocked completion on A. On
			// master the timeout wins the pop (Peek passes through, pops A, fires
			// onRequestCanceled) and the stale blocked completer will then mis-pop
			// B; on the fixed impl A was already atomically popped by the blocked
			// PopIf, so the timeout's PopIf sees B, its predicate fails, and it is
			// a no-op. The 150ms margin is deliberately kept UNDER shortTimeout
			// (MAJOR-1) so even an eagerly-dispatched B cannot complete a full
			// timeout cycle of its own before release - without that headroom, a
			// correct fix's eager dispatch of B could let B genuinely time out
			// mid-sleep and inflate the counters with B's own, unrelated,
			// lifecycle. (A legitimate dispatch of B by the pump during this
			// window is CORRECT, not a failure — it is drained by the B-dispatch
			// assertion below.)
			time.Sleep(shortTimeout + 150*time.Millisecond)

			// Release the blocked completer.
			releaseBlockingQueue()

			select {
			case err := <-handlerDone:
				require.NoError(t, err)
			case <-time.After(e2aBound):
				t.Fatal("timed out waiting for the CALL_RESULT handler to finish after releasing the blocked Peek")
			}

			// Exactly-one invariant (A2 regression): exactly one of
			// {responseHandler, onRequestCanceled} must fire for this single raced
			// request — the test does NOT mandate which side wins (with the fixed
			// atomic PopIf, whichever completer pops first wins and the other's
			// predicate fails). On master, BOTH fire: the stale, mis-popped
			// completion fires responseHandler AND the timeout fires
			// onRequestCanceled — total == 2.
			total := atomic.LoadInt32(&responseCalls) + atomic.LoadInt32(&cancelCalls)
			assert.Equal(t, int32(1), total,
				"exactly one of {responseHandler, onRequestCanceled} must fire for the raced request (got responseCalls=%d cancelCalls=%d)",
				atomic.LoadInt32(&responseCalls), atomic.LoadInt32(&cancelCalls))

			// B must still be dispatched (not silently discarded). On master, B
			// was mis-popped by the stale completion — written but its pending
			// state deleted, a permanent leak — so the pending-state assertion
			// below fails even though the write drained here.
			select {
			case cID := <-written:
				assert.Equal(t, clientID, cID)
			case <-time.After(e2aBound):
				t.Fatal("A2 REGRESSION: the next queued request (B) was never dispatched - silently discarded by the non-atomic Peek-then-Pop race")
			}
			require.True(t, state.HasPendingRequest(clientID))
			cs := state.GetClientState(clientID)
			_, hasB := cs.GetPendingRequest(requestB)
			require.True(t, hasB)

			okB := d.CompleteRequest(clientID, requestB)
			assert.True(t, okB)
		})
	}
}

// ============================================================================
// A6.1 - Write error concurrent with a pre-filled readyForDispatch => pump
// survives, keeps dispatching for a SECOND client. (A1 regression; MUST HANG
// on master - carries a watchdog so the hang fails cleanly instead of
// wedging the test run.)
// ============================================================================

// e2aBlockingWriter is a fake ws.Server whose Write for a specific clientID
// blocks (signalling `entered` first) on its FIRST call only, then returns a
// configured error. Every subsequent call (any clientID, including the
// blocked one on a later attempt) passes straight through, recording on
// `written`. Mirrors d2BlockingServer/d2SelectiveBlockingServer.
type e2aBlockingWriter struct {
	ws.Server
	blockClientID string
	blockErr      error
	tripped       int32
	release       chan struct{}
	entered       chan struct{}
	written       chan string
}

func newE2aBlockingWriter(blockClientID string, blockErr error) *e2aBlockingWriter {
	return &e2aBlockingWriter{
		blockClientID: blockClientID,
		blockErr:      blockErr,
		release:       make(chan struct{}),
		entered:       make(chan struct{}),
		written:       make(chan string, 8),
	}
}

func (w *e2aBlockingWriter) Write(clientID string, data []byte) error {
	if clientID == w.blockClientID && atomic.CompareAndSwapInt32(&w.tripped, 0, 1) {
		close(w.entered)
		<-w.release
		return w.blockErr
	}
	w.written <- clientID
	return nil
}

// TestE2aA1WriteErrorWithPrefilledReadyForDispatchPumpSurvivesForSecondClient
// reproduces the A1 self-deadlock: dispatchNextRequest's write-error path
// (dispatcher.go ~:965) calls the public, SIGNALING CompleteRequest from the
// pump goroutine itself. If readyForDispatch (cap 1) is already full - here,
// from a simulated off-pump completion that landed while the pump was
// blocked inside Write - that call's `d.readyForDispatch <- ...` blocks
// forever: the pump is the sole reader of that channel and is also the one
// trying to write to it. The whole pump (shared by every client) then never
// dispatches anything again, for any client.
func TestE2aA1WriteErrorWithPrefilledReadyForDispatchPumpSurvivesForSecondClient(t *testing.T) {
	queueMap := NewFIFOQueueMap(10)
	d := NewDefaultServerDispatcher(queueMap)
	var mutex sync.RWMutex
	state := NewServerState(&mutex)
	d.SetPendingRequestState(state)
	clientA := "e2a-a1-clientA"
	clientB := "e2a-a1-clientB"
	network := newE2aBlockingWriter(clientA, errors.New("simulated write failure"))
	d.SetNetworkServer(network)
	endpoint := &Server{}
	endpoint.AddProfile(ocpp.NewProfile("e2amock", &e2aMockFeature{}))

	d.Start()
	require.True(t, d.IsRunning())
	// NOTE: deliberately no `defer d.Stop()`. On master this scenario wedges
	// the pump goroutine permanently (the very bug under test), and Stop()
	// would itself hang forever waiting on the pump's doneC. The watchdog
	// below is what keeps THIS TEST bounded; the dispatcher goroutine is
	// knowingly abandoned on the red path (isolated per-test resource, no
	// shared state with other tests).

	d.CreateClient(clientA)
	d.CreateClient(clientB)

	bundleA, _ := e2aNewBundle(t, endpoint, "a")
	require.NoError(t, d.SendRequest(clientA, bundleA))

	select {
	case <-network.entered:
	case <-time.After(e2aBound):
		t.Fatal("pump never entered the blocking Write call for client A")
	}

	// Pre-fill the cap-1 readyForDispatch buffer directly (white-box),
	// simulating an off-pump CompleteRequest (a genuine CALL_RESULT for some
	// other, already in-flight request) that already landed while the pump
	// is blocked inside Write. The pump is not in its select loop right now
	// (it is deep inside dispatchNextRequest's synchronous Write call), so
	// this buffered send cannot race the pump's own reads.
	select {
	case d.readyForDispatch <- serverReadyToken{clientID: "someone-else", requestID: "irrelevant"}:
	case <-time.After(e2aBound):
		t.Fatal("could not pre-fill readyForDispatch (should be empty/available at this point)")
	}

	// Release the blocked Write: it returns the simulated failure.
	// dispatchNextRequest's write-error path then attempts its own
	// completion+signal for A on the SAME pump goroutine - on master this
	// self-deadlocks against the buffer just filled.
	close(network.release)

	bundleB, _ := e2aNewBundle(t, endpoint, "b")
	sendErrC := make(chan error, 1)
	go func() {
		sendErrC <- d.SendRequest(clientB, bundleB)
	}()

	select {
	case err := <-sendErrC:
		require.NoError(t, err)
	case <-time.After(e2aBound):
		t.Fatal("SendRequest for client B did not even return")
	}

	// Watchdog: a second, unrelated client's request must still get
	// dispatched within a bounded time. On master, the pump is wedged
	// forever at this point, so this MUST time out - turning "hang" into a
	// clean, reported test failure instead of wedging the whole CI run.
	select {
	case cID := <-network.written:
		assert.Equal(t, clientB, cID, "the pump must keep dispatching for a second, unrelated client after a write error races a pre-filled readiness buffer")
		// Green path only: the pump is healthy and provably drainable here,
		// so clean it up instead of leaking it (MINOR-2 in
		// findings/e2a-tests-fable-review.md). The red path above still
		// abandons the pump as designed - Stop() would itself hang forever
		// waiting on a wedged pump's doneC.
		d.Stop()
	case <-time.After(e2aBound):
		t.Fatal("A1 REGRESSION: the pump is wedged - the write-error completion path deadlocked against an already-full readyForDispatch buffer (self-send-deadlock), so client B's request was never dispatched")
	}
}

// ============================================================================
// A6.8 - Write error for client A with a SECOND request already queued for A
// => request 2 is dispatched with NO further external stimulus for A.
// (MAJOR-2 re-entry regression; distinct from A6.1, which only asserts a
// SECOND CLIENT keeps going.)
// ============================================================================

// e2aArmedBlockingWriter is a fake ws.Server whose Write for a specific
// clientID passes straight through (recording on `written`) until armed via
// arm(). Only the FIRST write for blockClientID AFTER arming blocks
// (signalling `entered` first), then returns a configured error once
// released; every other call - including blockClientID's writes before
// arming and any after the one triggered write - passes straight through.
//
// Unlike e2aBlockingWriter (A6.1's writer, which blocks on the very first
// write), this lets the client's FIRST dispatch (request 1) succeed normally
// - request 1 ends up legitimately in-flight with an active clientCtx before
// anything is armed - so it is the SECOND write (request 2, dispatched only
// once request 1 is completed) that trips the fault injection. That is what
// lets this test prove every requestChannel event for the client was
// already consumed by the pump BEFORE the write error ever happens, closing
// the false-green BLOCKER-1 found in findings/e2a-tests-fable-review.md (the
// original construction let request 2's own SendRequest event sit buffered,
// unconsumed, in requestChannel at the moment of the write error, so an
// A1-only fix - deadlock removed, but no loop-the-dispatch-step - could
// still dispatch request 2 off that stale buffered event and false-pass).
type e2aArmedBlockingWriter struct {
	ws.Server
	blockClientID string
	blockErr      error
	armed         int32
	tripped       int32
	release       chan struct{}
	entered       chan struct{}
	written       chan string
}

func newE2aArmedBlockingWriter(blockClientID string, blockErr error) *e2aArmedBlockingWriter {
	return &e2aArmedBlockingWriter{
		blockClientID: blockClientID,
		blockErr:      blockErr,
		release:       make(chan struct{}),
		entered:       make(chan struct{}),
		written:       make(chan string, 8),
	}
}

// arm enables the fault injection. Called only after request 1's write has
// already gone through cleanly, so request 1's own dispatch is never
// intercepted - only the next (second) write for blockClientID is.
func (w *e2aArmedBlockingWriter) arm() { atomic.StoreInt32(&w.armed, 1) }

func (w *e2aArmedBlockingWriter) Write(clientID string, data []byte) error {
	if clientID == w.blockClientID && atomic.LoadInt32(&w.armed) == 1 &&
		atomic.CompareAndSwapInt32(&w.tripped, 0, 1) {
		close(w.entered)
		<-w.release
		return w.blockErr
	}
	w.written <- clientID
	return nil
}

// TestE2aMAJOR2WriteErrorSameClientSecondRequestDispatchedNoExternalStimulus
// pins the MAJOR-2 nuance: once the A1 fix removes the readyForDispatch
// self-send from the write-error path (switching it to the non-signaling
// completeRequestOwned), re-entry to THIS client's own next queued request
// must come from the pump looping the dispatch step internally (spec §A4's
// "loop-the-dispatch-step" fix) - not from any external kick, and NOT from a
// requestChannel event that merely happened to still be sitting unconsumed
// in the channel. A fix that only prevents the deadlock (A1) without also
// adding the loop would strand request 3 forever; this test - unlike A6.1 -
// is specifically about that same-client re-entry, not just "the pump isn't
// wedged".
//
// Construction (BLOCKER-1 restructure, see e2aArmedBlockingWriter above):
// request 1 is dispatched and completes cleanly BEFORE the write error ever
// happens. Requests 2 and 3 are queued behind it WHILE request 1 is still
// legitimately in-flight (rdy=false, active clientCtx) - proven, white-box,
// by draining requestChannel to length 0 before request 1 is completed. Only
// THEN is request 1 completed (from the test goroutine, simulating a genuine
// CALL_RESULT), which wakes the pump to dispatch request 2 - the write that
// is armed to fail. This rules out the original construction's false-green:
// there, request 2's own dispatch event was still buffered, unconsumed, in
// requestChannel at the moment of the write error, so an A1-only fix could
// dispatch request 2 off that stale event without ever exercising the loop.
func TestE2aMAJOR2WriteErrorSameClientSecondRequestDispatchedNoExternalStimulus(t *testing.T) {
	queueMap := NewFIFOQueueMap(10)
	d := NewDefaultServerDispatcher(queueMap)
	var mutex sync.RWMutex
	state := NewServerState(&mutex)
	d.SetPendingRequestState(state)
	clientID := "e2a-a8-client"
	network := newE2aArmedBlockingWriter(clientID, errors.New("simulated write failure"))
	d.SetNetworkServer(network)
	endpoint := &Server{}
	endpoint.AddProfile(ocpp.NewProfile("e2amock", &e2aMockFeature{}))

	d.Start()
	require.True(t, d.IsRunning())
	// No defer d.Stop() - see A6.1's comment; this scenario wedges the pump
	// on master via the identical self-send-deadlock mechanism (this time
	// tripped by request 2's write instead of request 1's).

	d.CreateClient(clientID)

	bundle1, request1 := e2aNewBundle(t, endpoint, "one")
	require.NoError(t, d.SendRequest(clientID, bundle1))

	// Request 1's write is unarmed, so it passes straight through cleanly.
	// SetTimeout was not called, so the default (positive) timeout gives
	// request 1 an active clientCtx - the client is NOT ready to transmit
	// again until request 1 completes.
	select {
	case cID := <-network.written:
		require.Equal(t, clientID, cID)
	case <-time.After(e2aBound):
		t.Fatal("request 1 was never dispatched")
	}

	// Arm the writer now: request 1's own dispatch write has already
	// happened, unintercepted. Only the NEXT write for this client - request
	// 2's, once it is legitimately dispatched below - will trip the fault.
	network.arm()

	// Queue requests 2 and 3 behind request 1, entirely WHILE request 1 is
	// still in-flight (rdy=false, active clientCtx) - so both requestChannel
	// events are consumed by the pump WITHOUT any dispatch attempt, unlike
	// the original (BLOCKER-1'd) construction where request 2's event was
	// still sitting unconsumed when the write error happened.
	bundle2, _ := e2aNewBundle(t, endpoint, "two")
	require.NoError(t, d.SendRequest(clientID, bundle2))
	bundle3, _ := e2aNewBundle(t, endpoint, "three")
	require.NoError(t, d.SendRequest(clientID, bundle3))

	// White-box: prove the pump has drained BOTH requestChannel events for
	// this client BEFORE request 1 is ever completed - i.e., before the
	// write error this test is about can even occur. This is the decisive
	// difference from the BLOCKER-1'd construction.
	require.Eventually(t, func() bool {
		return len(d.requestChannel) == 0
	}, e2aBound, 5*time.Millisecond, "requestChannel must fully drain both queued events while the client is not ready to transmit")

	// Complete request 1 now - simulating its genuine CALL_RESULT arriving -
	// from the test goroutine, exactly like a real off-pump completion. This
	// wakes the pump via readyForDispatch (currently empty, so this send
	// cannot yet block) to dispatch request 2, whose write is armed to fail.
	d.CompleteRequest(clientID, request1)

	select {
	case <-network.entered:
	case <-time.After(e2aBound):
		t.Fatal("pump never entered the blocking Write call for request 2")
	}

	// Pre-fill readyForDispatch (empty again - the token from completing
	// request 1 was already consumed by the pump to get here) so the
	// write-error path's own signaling completion self-deadlocks - the exact
	// A1 fault injection, this time on request 2's error path.
	select {
	case d.readyForDispatch <- serverReadyToken{clientID: "someone-else", requestID: "irrelevant"}:
	case <-time.After(e2aBound):
		t.Fatal("could not pre-fill readyForDispatch")
	}

	close(network.release)

	// Watchdog: request 3 must be dispatched for the SAME client with NO
	// further external stimulus - requestChannel is provably empty and
	// nothing else ever sends to it. On master, the pump wedges via the
	// self-send-deadlock (write-error path calls the public, signaling
	// CompleteRequest from the pump goroutine itself, against the buffer
	// just pre-filled). On an A1-only fix (deadlock removed, but no
	// loop-the-dispatch-step), request 2 completes cleanly but request 3 is
	// stranded forever - nothing left to trigger its dispatch. Only the full
	// fix loops the dispatch step internally and writes request 3.
	select {
	case cID := <-network.written:
		assert.Equal(t, clientID, cID)
		// Green path only - see MINOR-2 in findings/e2a-tests-fable-review.md.
		d.Stop()
	case <-time.After(e2aBound):
		t.Fatal("MAJOR-2 REGRESSION: request 3 was never dispatched for the same client after request 2's write-error completion - pump is wedged (A1) or request 3 was stranded (missing loop-the-dispatch-step re-entry)")
	}
}

// ============================================================================
// A6.6 - Stop() -> Start() -> an old-generation watcher fires => no
// cross-generation delivery and no parked goroutine, clean under -race.
// (B2+B3, folded into E2a.)
// ============================================================================

// TestE2aB2B3StopStartOldGenerationWatcherNoCrossDelivery pins B2 (shutdown-
// safe send: `select { case timerC <- tok: case <-stoppedC: }`) and B3
// (generation-pinned parameters at spawn) together. Today's waitForTimeout
// reads d.stoppedC/d.timerC DYNAMICALLY from the receiver - both a data race
// against Start()'s reassignment of those fields, and a vector for a stale
// watcher to post into a NEW generation's timerC after a Stop->Start cycle.
//
// NAMING ASSUMPTION (flagged for the implementer, same convention as
// d2_token_identity_test.go's file-level comment about its own literal
// shapes): this test calls waitForTimeout with two EXTRA parameters,
// `stoppedC chan struct{}` and `timerC chan serverTimeoutToken`, appended
// after the existing (clientID, clientCtx) parameters - matching the spec's
// "pass stoppedC/cancelC/timerC as parameters at spawn" (cancelC is E2c-only,
// not part of E2a's scope, so omitted here). Against TODAY's 2-parameter
// waitForTimeout, this file fails to compile - that IS the intended red
// state pinning the B2/B3 contract. If the real implementation picks a
// different parameter order/name, only this call site needs a matching
// update.
//
// MAJOR-2 (findings/e2a-tests-fable-review.md): the single-watcher phase
// above pins B3 (compile-shape + -race) but NOT B2's send shape, because its
// watcher's OUTER select always exits via the already-closed gen1StoppedC
// arm before its ctx ever expires post-Stop - the send statement is simply
// never reached, so an implementation that accepts the B3 parameters but
// keeps today's plain guarded send (`if running { timerC <- tok }`, reading
// the properly-synchronized d.running - still passing -race - instead of
// `select { case timerC <- tok: case <-stoppedC: }`) would pass identically.
// The second phase below closes that gap.
func TestE2aB2B3StopStartOldGenerationWatcherNoCrossDelivery(t *testing.T) {
	queueMap := NewFIFOQueueMap(10)
	d := NewDefaultServerDispatcher(queueMap)
	var mutex sync.RWMutex
	state := NewServerState(&mutex)
	d.SetPendingRequestState(state)
	network := &e2aServer{}
	d.SetNetworkServer(network)

	d.Start()
	require.True(t, d.IsRunning())

	// Capture generation 1's own stoppedC/timerC/cancelC - exactly what a
	// real spawn at this point in time would have captured as parameters,
	// before any Stop/Start cycle. cancelC is PR-E2c's addition (waitForTimeout
	// grew a cancelC parameter alongside a requestID/userCtx pair); this test
	// predates E2c but must still compile and pass against the generalized
	// signature, so a requestID and a never-firing userCtx (Background) are
	// supplied - this test is only exercising the TIMEOUT (timerC) arm's
	// generation-pinning, not the cancel arm.
	gen1StoppedC := d.stoppedC
	gen1TimerC := d.timerC
	gen1CancelC := d.cancelC

	clientID := "e2a-b2b3-client"
	requestID := "e2a-b2b3-request"
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	clientCtx := clientTimeoutContext{ctx: ctx, cancel: cancel}

	watcherDone := make(chan struct{})
	go func() {
		// PR-E2a: pinned (generation-captured) parameters, not the dynamic
		// d.stoppedC / d.timerC fields.
		d.waitForTimeout(clientID, requestID, clientCtx, context.Background(), gen1StoppedC, gen1TimerC, gen1CancelC)
		close(watcherDone)
	}()

	// Stop generation 1, then immediately start generation 2 - reassigning
	// d.stoppedC/d.timerC to brand-new channel instances. The watcher above
	// must remain pinned to the OLD (gen1) channels regardless of this swap.
	d.Stop()
	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()

	// The stale watcher's context fires ~150ms after spawn - well after the
	// Stop/Start cycle above. It must NOT deliver anything on the NEW
	// (gen2) d.timerC: a bounded silence window on the CURRENT field proves
	// no cross-generation delivery.
	select {
	case tok := <-d.timerC:
		t.Fatalf("cross-generation delivery: stale watcher posted %+v onto the NEW generation's timerC", tok)
	case <-time.After(400 * time.Millisecond):
		// expected: nothing arrives on the new generation's timerC.
	}

	// No parked goroutine: the watcher itself must have exited (either by
	// successfully posting to its OWN pinned gen1TimerC - which nothing
	// reads anymore, so it would then take the gen1StoppedC arm instead - or
	// by observing gen1StoppedC closing) well within a bound.
	select {
	case <-watcherDone:
	case <-time.After(e2aBound):
		t.Fatal("stale watcher goroutine never exited - leaked, parked forever on stale generation-pinned channels")
	}

	// --- Phase 2 (MAJOR-2 fold) --------------------------------------------
	//
	// Force enough stale, gen1-pinned watchers that an implementation
	// lacking B2's shutdown-safe send shape MUST park at least one of them
	// forever, while a B2-shaped implementation always lets every single one
	// of them exit.
	//
	// Each watcher below is given an ALREADY-EXPIRED ctx (negative timeout;
	// context.WithDeadline synchronously cancels before even returning when
	// the deadline has already passed), so BOTH of its outer select's arms -
	// ctx.Done() and the already-closed gen1StoppedC - are ready from the
	// instant it is spawned. Go's select makes a uniform pseudo-random
	// choice between simultaneously-ready cases, independently per watcher,
	// so which arm each individual watcher takes is NOT itself deterministic
	// (fable's review picked N=11 - exactly one more than gen1TimerC's cap-10
	// buffer - on the assumption that ALL 11 would take the ctx.Done() arm;
	// that assumption only holds if every one of 11 independent coin flips
	// lands the same way, a ~1-in-2000 event, not a reliable red signal). To
	// turn "some watchers happen to take the deadline arm" into a reliable
	// pigeonhole instead of a coin-flip, this uses a much larger N: with
	// N=staleWatcherCount fair coin flips, the number k that take the
	// ctx.Done() arm is Binomial(N, 0.5); we only need k > 10 (to overflow
	// the cap-10 buffer starting empty) and P(k <= 10) is astronomically
	// small at this N (many sigma below the mean) - far below the residual
	// timing risk this same suite already accepts elsewhere (see MINOR-4 in
	// findings/e2a-tests-fable-review.md).
	//
	// An old-shaped watcher that takes the ctx.Done() arm performs a plain
	// guarded send with no escape: once the buffer's 10 slots are claimed by
	// other watchers (nothing ever drains gen1TimerC in this phase), every
	// further blocking send parks forever. A B2-shaped watcher's send is
	// itself select-guarded by the SAME already-closed gen1StoppedC, so it
	// can never block regardless of buffer state or how many watchers took
	// the ctx.Done() arm - every single one always exits.
	const staleWatcherCount = 100
	staleDone := make(chan struct{}, staleWatcherCount)
	for i := 0; i < staleWatcherCount; i++ {
		expiredCtx, expiredCancel := context.WithTimeout(context.Background(), -1*time.Millisecond)
		defer expiredCancel()
		staleClientCtx := clientTimeoutContext{ctx: expiredCtx, cancel: expiredCancel}
		go func() {
			// Pinned to the SAME stale gen1 channels as the phase-1 watcher.
			d.waitForTimeout(clientID, requestID, staleClientCtx, context.Background(), gen1StoppedC, gen1TimerC, gen1CancelC)
			staleDone <- struct{}{}
		}()
	}

	watchdog := time.NewTimer(e2aBound)
	defer watchdog.Stop()
	doneCount := 0
countLoop:
	for doneCount < staleWatcherCount {
		select {
		case <-staleDone:
			doneCount++
		case <-watchdog.C:
			break countLoop
		}
	}
	assert.Equal(t, staleWatcherCount, doneCount,
		"B2 REGRESSION: a stale-generation watcher parked forever on a plain guarded send into the unread gen1 timerC buffer - the shutdown-safe select{case timerC<-tok: case <-stoppedC:} shape (B2) is required so a stale watcher always has an escape via the already-closed stoppedC, regardless of buffer state (%d/%d watchers exited)", doneCount, staleWatcherCount)
}
