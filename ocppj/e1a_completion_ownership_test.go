package ocppj

// PR-E1a (tasks/e1-context-aware-send.md, "## PR-E1a — completion-ownership +
// readiness rework") RED-FIRST test suite.
//
// This file lives in `package ocppj` (not `ocppj_test`) so it can reach
// the unexported newRequestCanceledError constructor, DefaultClientDispatcher's
// concrete fields, and FIFOClientQueue's internals — exactly like
// d2_token_identity_test.go. It defines its own minimal fake network helpers
// (prefixed e1a) to avoid any dependency on the ocppj_test package.
//
// RED-FIRST discipline: every test below references the PR-E1a surface exactly
// as the spec names it. Against today's codebase:
//   - CompleteRequest returns void (not bool)
//   - RequestQueue / FIFOClientQueue have no PopIf
//   - ErrRequestCanceled does not exist
//   - newRequestCanceledError does not exist
//   - ocpp.Error has no Cause field and no Unwrap method
//
// This whole file is EXPECTED to fail compilation — that IS the intended red
// state pinning the PR-E1a contract.
//
// Codex review round (this revision): every numbered finding below refers to
// the codex review of the first draft of this file. Each new/rewritten
// test/comment states which finding it addresses and how.

import (
	"context"
	"errors"
	"fmt"
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
// In-package fake network client (package ocppj cannot import ocppj_test)
// ============================================================================

// e1aClient is a minimal ws.Client fake for the client dispatcher tests.
// It records writes so tests can assert "exactly N writes" without a real
// network. It implements ws.Client by embedding the interface and overriding
// only the methods DefaultClientDispatcher calls.
type e1aClient struct {
	ws.Client // embedded nil; accidental calls panic loudly (fail-fast)

	mu      sync.Mutex
	onWrite func(data []byte) error
}

func (c *e1aClient) Write(data []byte) error {
	c.mu.Lock()
	cb := c.onWrite
	c.mu.Unlock()
	if cb != nil {
		return cb(data)
	}
	return nil
}

func (c *e1aClient) setOnWrite(cb func(data []byte) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onWrite = cb
}

// e1aCountingClient is a ws.Client fake that counts Write calls thread-safely.
type e1aCountingClient struct {
	ws.Client

	mu      sync.Mutex
	onWrite func(data []byte) error
	writes  int
}

func (c *e1aCountingClient) Write(data []byte) error {
	c.mu.Lock()
	c.writes++
	cb := c.onWrite
	c.mu.Unlock()
	if cb != nil {
		return cb(data)
	}
	return nil
}

func (c *e1aCountingClient) writeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writes
}

func (c *e1aCountingClient) setOnWrite(cb func(data []byte) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onWrite = cb
}

// ============================================================================
// Helpers for creating dispatcher + endpoint
// ============================================================================

// e1aNewDispatcher wires a DefaultClientDispatcher exactly like
// ClientDispatcherTestSuite.SetupTest but returns the CONCRETE type so these
// white-box tests can call CompleteRequest directly on the concrete dispatcher
// (which changes signature from void to bool).
func e1aNewDispatcher(t *testing.T, capacity int) (d *DefaultClientDispatcher, state ClientState, queue *FIFOClientQueue, network *e1aClient, endpoint *Client) {
	t.Helper()
	queue = NewFIFOClientQueue(capacity)
	d = NewDefaultClientDispatcher(queue)
	state = NewClientState()
	d.SetPendingRequestState(state)
	network = &e1aClient{}
	d.SetNetworkClient(network)
	endpoint = &Client{Id: "e1a-test"}
	endpoint.AddProfile(ocpp.NewProfile("mock", &e1aMockFeature{}))
	return
}

// e1aNewDispatcherWithState is like e1aNewDispatcher but lets the caller
// supply their own ClientState. Used by the callback-steal tests (Finding 1)
// to install a hooked wrapper around a real ClientState.
func e1aNewDispatcherWithState(t *testing.T, capacity int, state ClientState) (d *DefaultClientDispatcher, queue *FIFOClientQueue, network *e1aClient, endpoint *Client) {
	t.Helper()
	queue = NewFIFOClientQueue(capacity)
	d = NewDefaultClientDispatcher(queue)
	d.SetPendingRequestState(state)
	network = &e1aClient{}
	d.SetNetworkClient(network)
	endpoint = &Client{Id: "e1a-test"}
	endpoint.AddProfile(ocpp.NewProfile("mock", &e1aMockFeature{}))
	return
}

// e1aNewCountingDispatcher is like e1aNewDispatcher but returns a counting
// fake network so tests can assert exactly-N writes.
func e1aNewCountingDispatcher(t *testing.T, capacity int) (d *DefaultClientDispatcher, state ClientState, queue *FIFOClientQueue, network *e1aCountingClient, endpoint *Client) {
	t.Helper()
	queue = NewFIFOClientQueue(capacity)
	d = NewDefaultClientDispatcher(queue)
	state = NewClientState()
	d.SetPendingRequestState(state)
	network = &e1aCountingClient{}
	d.SetNetworkClient(network)
	endpoint = &Client{Id: "e1a-test"}
	endpoint.AddProfile(ocpp.NewProfile("mock", &e1aMockFeature{}))
	return
}

// e1aNewBundle creates a fresh Call/RequestBundle pair via the given endpoint.
func e1aNewBundle(t *testing.T, endpoint *Client, value string) (RequestBundle, string) {
	t.Helper()
	req := &e1aMockRequest{MockValue: value}
	call, err := endpoint.CreateCall(req)
	require.NoError(t, err)
	data, err := call.MarshalJSON()
	require.NoError(t, err)
	return RequestBundle{Call: call, Data: data}, call.UniqueId
}

// ============================================================================
// In-package mock feature
// ============================================================================

const e1aMockFeatureName = "E1aMock"

type e1aMockRequest struct {
	MockValue string `json:"mockValue"`
}

func (r *e1aMockRequest) GetFeatureName() string { return e1aMockFeatureName }

type e1aMockConfirmation struct {
	MockValue string `json:"mockValue"`
}

func (c *e1aMockConfirmation) GetFeatureName() string { return e1aMockFeatureName }

type e1aMockFeature struct{}

func (f *e1aMockFeature) GetFeatureName() string        { return e1aMockFeatureName }
func (f *e1aMockFeature) GetRequestType() reflect.Type  { return reflect.TypeOf(e1aMockRequest{}) }
func (f *e1aMockFeature) GetResponseType() reflect.Type { return reflect.TypeOf(e1aMockConfirmation{}) }

const e1aBound = 2 * time.Second               // generous bound for events that MUST happen
const e1aSilenceBound = 300 * time.Millisecond // window to prove an event does NOT happen

// ============================================================================
// Test 1 — Completion-ownership return value
// ============================================================================

// TestE1aCompleteRequestReturnsTrueWhenFrontMatches tests that CompleteRequest
// returns true iff this call popped the matching front request. The spec
// changes CompleteRequest from void to bool; true means "I won the race to
// own this completion."
func TestE1aCompleteRequestReturnsTrueWhenFrontMatches(t *testing.T) {
	d, state, q, network, endpoint := e1aNewDispatcher(t, 10)

	sent := make(chan struct{}, 8)
	network.setOnWrite(func(data []byte) error {
		sent <- struct{}{}
		return nil
	})

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()

	bundleA, requestA := e1aNewBundle(t, endpoint, "a")
	require.NoError(t, d.SendRequest(bundleA))

	select {
	case <-sent:
	case <-time.After(e1aBound):
		t.Fatal("timed out waiting for request A to be dispatched")
	}
	require.True(t, state.HasPendingRequest())

	// PR-E1a: CompleteRequest now returns bool. This call must return true
	// because requestA IS the front of the queue.
	ok := d.CompleteRequest(requestA)
	assert.True(t, ok, "CompleteRequest must return true when it pops the matching front request")

	assert.False(t, state.HasPendingRequest(), "pending state must be cleared after CompleteRequest")
	assert.True(t, q.IsEmpty(), "queue must be empty after popping the only element")
}

func TestE1aCompleteRequestReturnsFalseWhenQueueEmpty(t *testing.T) {
	d, _, _, _, _ := e1aNewDispatcher(t, 10)

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()

	// PR-E1a: CompleteRequest returns false when the queue is empty.
	ok := d.CompleteRequest("any-id")
	assert.False(t, ok, "CompleteRequest must return false when the queue is empty")
}

func TestE1aCompleteRequestReturnsFalseWhenFrontIDDiffers(t *testing.T) {
	d, state, q, network, endpoint := e1aNewDispatcher(t, 10)

	sent := make(chan struct{}, 8)
	network.setOnWrite(func(data []byte) error {
		sent <- struct{}{}
		return nil
	})

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()

	bundleA, requestA := e1aNewBundle(t, endpoint, "a")
	require.NoError(t, d.SendRequest(bundleA))

	select {
	case <-sent:
	case <-time.After(e1aBound):
		t.Fatal("timed out waiting for request A to be dispatched")
	}
	require.True(t, state.HasPendingRequest())

	// PR-E1a: CompleteRequest returns false when the front's UniqueId does not
	// match the passed id — this call must NOT pop the queue.
	ok := d.CompleteRequest("wrong-id-not-A")
	assert.False(t, ok, "CompleteRequest must return false when front ID does not match")

	// A must still be the front, pending, and untouched.
	assert.True(t, state.HasPendingRequest(), "pending state must still contain A")
	assert.False(t, q.IsEmpty(), "queue must not be empty — A was not popped")
	assert.Equal(t, 1, q.Size(), "queue size must still be 1")

	// A must still be completable by its real ID.
	ok2 := d.CompleteRequest(requestA)
	assert.True(t, ok2)
	assert.False(t, state.HasPendingRequest())
	assert.True(t, q.IsEmpty())
}

// ============================================================================
// Test 2 — PopIf atomicity vs DrainAll (Finding 2)
// ============================================================================

// TestE1aPopIfSerializesWithDrainAllOnQueueMutex directly exercises
// queue.PopIf(pred) through the RequestQueue interface, with the predicate
// BLOCKING WHILE THE QUEUE LOCK IS HELD. The real FIFOClientQueue.PopIf
// acquires the mutex, THEN calls pred — so a blocking predicate holds the
// lock for the duration. This is the correct harness (the earlier, now-
// removed harness had pred block BEFORE the mutex was taken, which could
// never actually prove PopIf and DrainAll serialize on the same lock).
//
// While pred is blocked (holding the mutex), a concurrent DrainAll must be
// unable to proceed — proving PopIf and Stop's DrainAll serialize on the same
// queue mutex per the spec (A1), so completion and Stop-drain can never
// double-deliver the same element.
func TestE1aPopIfSerializesWithDrainAllOnQueueMutex(t *testing.T) {
	endpoint := &Client{Id: "e1a-popif"}
	endpoint.AddProfile(ocpp.NewProfile("mock", &e1aMockFeature{}))
	bundle, requestID := e1aNewBundle(t, endpoint, "popif")

	concreteQueue := NewFIFOClientQueue(10)
	var q RequestQueue = concreteQueue
	require.NoError(t, q.Push(bundle))

	predEntered := make(chan struct{})
	predRelease := make(chan struct{})
	var predCalls int32
	var predOnce sync.Once

	pred := func(el interface{}) bool {
		atomic.AddInt32(&predCalls, 1)
		b, ok := el.(RequestBundle)
		matches := ok && b.Call != nil && b.Call.UniqueId == requestID
		predOnce.Do(func() { close(predEntered) })
		<-predRelease
		return matches
	}

	type popIfResult struct {
		el interface{}
		ok bool
	}
	popIfDone := make(chan popIfResult, 1)
	go func() {
		el, ok := q.PopIf(pred)
		popIfDone <- popIfResult{el: el, ok: ok}
	}()

	select {
	case <-predEntered:
	case <-time.After(e1aBound):
		t.Fatal("PopIf never invoked its predicate")
	}

	// DrainAll must be unable to proceed while pred (holding the queue mutex
	// per the real implementation) is blocked: a short silence window proves
	// it does not return.
	drainDone := make(chan []interface{}, 1)
	go func() {
		drainDone <- concreteQueue.DrainAll()
	}()

	select {
	case <-drainDone:
		t.Fatal("DrainAll returned while PopIf's predicate was still blocked holding the queue mutex")
	case <-time.After(e1aSilenceBound):
		// expected: DrainAll is blocked on the same mutex.
	}

	close(predRelease)

	var popRes popIfResult
	select {
	case popRes = <-popIfDone:
	case <-time.After(e1aBound):
		t.Fatal("PopIf did not return after its predicate was released")
	}
	require.True(t, popRes.ok, "PopIf must report that it popped the element")
	poppedBundle, ok := popRes.el.(RequestBundle)
	require.True(t, ok)
	assert.Equal(t, requestID, poppedBundle.Call.UniqueId)

	var drained []interface{}
	select {
	case drained = <-drainDone:
	case <-time.After(e1aBound):
		t.Fatal("DrainAll did not return after the queue mutex was released")
	}
	assert.Empty(t, drained, "DrainAll must find nothing left — PopIf was the sole claimant")
	assert.Equal(t, int32(1), atomic.LoadInt32(&predCalls), "predicate must be invoked exactly once")
	assert.True(t, q.IsEmpty(), "queue must be empty after both PopIf and DrainAll finish")
}

// ============================================================================
// Test 2b — CompleteRequest routes through PopIf, not bare Pop
// ============================================================================

// e1aInstrumentedQueue wraps a *FIFOClientQueue and delegates every
// RequestQueue method to it, counting calls to PopIf and bare Pop via atomic
// counters. Used to prove CompleteRequest exclusively uses PopIf (the
// mandatory atomic path), never a separate Peek+Pop sequence.
type e1aInstrumentedQueue struct {
	*FIFOClientQueue
	popCalls   int32
	popIfCalls int32
}

func (q *e1aInstrumentedQueue) Pop() interface{} {
	atomic.AddInt32(&q.popCalls, 1)
	return q.FIFOClientQueue.Pop()
}

func (q *e1aInstrumentedQueue) PopIf(predicate func(interface{}) bool) (interface{}, bool) {
	atomic.AddInt32(&q.popIfCalls, 1)
	return q.FIFOClientQueue.PopIf(predicate)
}

func (q *e1aInstrumentedQueue) popCount() int32   { return atomic.LoadInt32(&q.popCalls) }
func (q *e1aInstrumentedQueue) popIfCount() int32 { return atomic.LoadInt32(&q.popIfCalls) }

// TestE1aCompleteRequestUsesPopIfNotBarePop proves that CompleteRequest routes
// its pop through PopIf (the atomic path), and does NOT issue a bare Pop call.
// An implementation that uses Peek+Pop (non-atomic) would pass every other
// test in this suite but fail this one — the instrumentation counts Pop calls
// during the CompleteRequest window and asserts exactly 0.
func TestE1aCompleteRequestUsesPopIfNotBarePop(t *testing.T) {
	// Build an instrumented queue that delegates to a real FIFOClientQueue.
	inner := NewFIFOClientQueue(10)
	iq := &e1aInstrumentedQueue{FIFOClientQueue: inner}

	// Wire the dispatcher by hand — same pattern as e1aNewDispatcher but
	// with the instrumented queue.
	d := NewDefaultClientDispatcher(iq)
	state := NewClientState()
	d.SetPendingRequestState(state)
	network := &e1aClient{}
	d.SetNetworkClient(network)
	endpoint := &Client{Id: "e1a-popif-route"}
	endpoint.AddProfile(ocpp.NewProfile("mock", &e1aMockFeature{}))

	sent := make(chan struct{}, 8)
	network.setOnWrite(func(data []byte) error {
		sent <- struct{}{}
		return nil
	})

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()

	bundleA, requestA := e1aNewBundle(t, endpoint, "a")
	require.NoError(t, d.SendRequest(bundleA))
	select {
	case <-sent:
	case <-time.After(e1aBound):
		t.Fatal("timed out waiting for request A to be dispatched")
	}
	require.True(t, state.HasPendingRequest())

	// dispatchNextRequest Peek-ed (not Popped) during dispatch — that's
	// fine. We snapshot counters NOW, right before CompleteRequest, so
	// only the completion's own calls are counted.
	popBefore := iq.popCount()
	popIfBefore := iq.popIfCount()

	ok := d.CompleteRequest(requestA)
	require.True(t, ok, "CompleteRequest(A) must return true — A was front")

	popDelta := iq.popCount() - popBefore
	popIfDelta := iq.popIfCount() - popIfBefore

	assert.Equal(t, int32(0), popDelta,
		"CompleteRequest must NOT call bare Pop — it must use PopIf for atomicity")
	assert.Equal(t, int32(1), popIfDelta,
		"CompleteRequest must call PopIf exactly once to atomically pop the matching front element")

	assert.False(t, state.HasPendingRequest())
	assert.True(t, inner.IsEmpty())
}

// ============================================================================
// Test 3 — Gated handlers: CompleteRequest return gates response/error handler
// (Finding 1 fix: no pump — driven entirely by hand, deterministically)
// ============================================================================

// TestE1aGatedHandlersCallResultNotInvokedAfterAlreadyCompleted tests that when
// CompleteRequest returns false, the responseHandler is NOT invoked. This
// guards the CALL_RESULT path in Client.ocppMessageHandler (client.go).
//
// Finding 1: this test deliberately does NOT start the pump (no d.Start()).
// With a real pump running, the readiness signal emitted by completing A
// could race ahead and redispatch B — setting pending state to B — before
// this test's own re-add of A's pending state below. ParseMessage's own
// pending-check (unrelated to the CompleteRequest gate) would then discard
// the "late" response before it ever reached the gate this test exists to
// pin, letting even a MISSING gate pass. Driving the queue/pending state by
// hand on a single goroutine — with no pump goroutine to race against —
// makes the whole scenario deterministic.
//
// Scenario:
//  1. Add pending A, push bundle A → Queue [A].
//  2. CALL_RESULT for A → CompleteRequest(A)=true → handler fires. Queue [].
//  3. Push bundle B → Queue [B].
//  4. Re-add pending A (so ParseMessage passes the pending check).
//  5. CALL_RESULT for A again → CompleteRequest(A)=false (front is B) → handler NOT fired.
//  6. B is still at the front, untouched.
func TestE1aGatedHandlersCallResultNotInvokedAfterAlreadyCompleted(t *testing.T) {
	d, state, q, network, _ := e1aNewDispatcher(t, 10)

	// Build a real Client wired to our dispatcher (pump never started).
	endpoint := &Client{Id: "e1a-gate"}
	endpoint.AddProfile(ocpp.NewProfile("mock", &e1aMockFeature{}))
	client := NewClient("e1a-gate", network, d, state, ocpp.NewProfile("mock", &e1aMockFeature{}))

	// Request A.
	reqA := &e1aMockRequest{MockValue: "gate"}
	callA, err := endpoint.CreateCall(reqA)
	require.NoError(t, err)
	requestID := callA.UniqueId

	// Add A to pending state and queue.
	state.AddPendingRequest(requestID, reqA)
	require.NoError(t, q.Push(RequestBundle{Call: callA, Data: []byte(`[2,"` + requestID + `","E1aMock",{"mockValue":"gate"}]`)}))

	var responseCalls int32
	client.SetResponseHandler(func(response ocpp.Response, reqID string) {
		atomic.AddInt32(&responseCalls, 1)
	})

	// First CALL_RESULT: CompleteRequest returns true → handler fires.
	callResultJSON := []byte(fmt.Sprintf(`[3,"%s",{"mockValue":"response"}]`, requestID))
	err = client.ocppMessageHandler(callResultJSON)
	require.NoError(t, err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&responseCalls), "first CALL_RESULT must invoke responseHandler")

	// Push B onto the now-empty queue.
	bundleB, requestB := e1aNewBundle(t, endpoint, "b")
	require.NoError(t, q.Push(bundleB))
	assert.Equal(t, 1, q.Size(), "queue must contain B")

	// Re-add A to pending state so ParseMessage passes its check and we
	// reach the CompleteRequest gate. Deterministic: no pump is running to
	// race this re-add, so this precondition is fixed, not probabilistic.
	state.AddPendingRequest(requestID, reqA)
	require.True(t, state.HasPendingRequest())

	// Second CALL_RESULT for A: CompleteRequest(A) returns false because B is
	// the front. Handler must NOT fire.
	err = client.ocppMessageHandler(callResultJSON)
	require.NoError(t, err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&responseCalls),
		"second CALL_RESULT for A must NOT invoke responseHandler (CompleteRequest returned false)")

	// B must still be in the queue, untouched.
	assert.Equal(t, 1, q.Size(), "B must still be in the queue")
	el := q.Peek()
	require.NotNil(t, el)
	bundle, ok := el.(RequestBundle)
	require.True(t, ok)
	assert.Equal(t, requestB, bundle.Call.UniqueId, "front must be B")

	// B itself can complete normally.
	state.AddPendingRequest(requestB, bundleB.Call.Payload)
	ok2 := d.CompleteRequest(requestB)
	assert.True(t, ok2, "B must be completable normally")
}

// TestE1aGatedHandlersCallErrorNotInvokedAfterAlreadyCompleted tests that when
// CompleteRequest returns false, the errorHandler is NOT invoked. This
// guards the CALL_ERROR path in Client.ocppMessageHandler (client.go).
//
// Same structure and same Finding 1 fix (no pump, hand-driven, deterministic)
// as TestE1aGatedHandlersCallResultNotInvokedAfterAlreadyCompleted, but
// exercises the CALL_ERROR branch.
func TestE1aGatedHandlersCallErrorNotInvokedAfterAlreadyCompleted(t *testing.T) {
	d, state, q, network, _ := e1aNewDispatcher(t, 10)

	endpoint := &Client{Id: "e1a-gate-err"}
	endpoint.AddProfile(ocpp.NewProfile("mock", &e1aMockFeature{}))
	client := NewClient("e1a-gate-err", network, d, state, ocpp.NewProfile("mock", &e1aMockFeature{}))

	// Request A.
	reqA := &e1aMockRequest{MockValue: "gate-err"}
	callA, err := endpoint.CreateCall(reqA)
	require.NoError(t, err)
	requestID := callA.UniqueId

	state.AddPendingRequest(requestID, reqA)
	require.NoError(t, q.Push(RequestBundle{Call: callA, Data: []byte(`[2,"` + requestID + `","E1aMock",{"mockValue":"gate-err"}]`)}))

	var errorCalls int32
	client.SetErrorHandler(func(err *ocpp.Error, details interface{}) {
		atomic.AddInt32(&errorCalls, 1)
	})

	// First CALL_ERROR: CompleteRequest returns true → handler fires.
	callErrorJSON := []byte(fmt.Sprintf(`[4,"%s","GenericError","boom",{}]`, requestID))
	err = client.ocppMessageHandler(callErrorJSON)
	require.NoError(t, err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&errorCalls), "first CALL_ERROR must invoke errorHandler")

	// Push B onto the now-empty queue.
	bundleB, requestB := e1aNewBundle(t, endpoint, "b")
	require.NoError(t, q.Push(bundleB))
	assert.Equal(t, 1, q.Size(), "queue must contain B")

	// Re-add A to pending state so ParseMessage passes. Deterministic — see
	// the CALL_RESULT sibling test's comment (Finding 1).
	state.AddPendingRequest(requestID, reqA)
	require.True(t, state.HasPendingRequest())

	// Second CALL_ERROR for A: CompleteRequest(A) false → handler NOT fired.
	err = client.ocppMessageHandler(callErrorJSON)
	require.NoError(t, err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&errorCalls),
		"second CALL_ERROR for A must NOT invoke errorHandler (CompleteRequest returned false)")

	// B untouched.
	assert.Equal(t, 1, q.Size())
	el := q.Peek()
	require.NotNil(t, el)
	bundle, ok := el.(RequestBundle)
	require.True(t, ok)
	assert.Equal(t, requestB, bundle.Call.UniqueId, "front must be B")

	// B can complete.
	state.AddPendingRequest(requestB, bundleB.Call.Payload)
	ok2 := d.CompleteRequest(requestB)
	assert.True(t, ok2)
}

// ============================================================================
// Test 4 — Callback-steal prevention (the pre-existing-race guard)
// (Finding 1 fix: pump kept for realism, race closed via a hooked ClientState)
// ============================================================================

// e1aBlockAfterGetState wraps a ClientState so a GetPendingRequest call for a
// specific requestID can be forced to block AFTER computing its real result,
// until a test-controlled signal fires.
//
// Finding 1: with a real pump running, a "late" response's ParseMessage
// pending-check and a concurrent pump-driven redispatch of the next queued
// request can race arbitrarily — a MISSING CompleteRequest gate could then
// falsely appear to pass, because the pending-check itself (unrelated to the
// gate) discards the late message first, so the gate is never even reached.
// Blocking the pending-check's return — AFTER it has already computed its
// true/false result, but BEFORE that result is handed back to ParseMessage —
// until the test has deterministically advanced the queue lets the test
// PROVE the late response reaches the gate, and that the gate call itself
// runs strictly after the queue has already moved on to a different front.
//
// The blockRequestID field is guarded by mu because E1c's pump calls
// GetPendingRequest every loop iteration, making reuse of this wrapper in
// E1c tests a latent -race without synchronization.
type e1aBlockAfterGetState struct {
	ClientState
	mu             sync.Mutex
	blockRequestID string
	entered        chan struct{}
	release        chan struct{}
	once           sync.Once
}

func newE1aBlockAfterGetState(inner ClientState) *e1aBlockAfterGetState {
	return &e1aBlockAfterGetState{
		ClientState: inner,
		entered:     make(chan struct{}),
		release:     make(chan struct{}),
	}
}

// setBlockRequestID arms the block for the given requestID.
func (s *e1aBlockAfterGetState) setBlockRequestID(id string) {
	s.mu.Lock()
	s.blockRequestID = id
	s.mu.Unlock()
}

func (s *e1aBlockAfterGetState) GetPendingRequest(requestID string) (ocpp.Request, bool) {
	req, ok := s.ClientState.GetPendingRequest(requestID)
	s.mu.Lock()
	blockID := s.blockRequestID
	s.mu.Unlock()
	if requestID == blockID {
		s.once.Do(func() { close(s.entered) })
		<-s.release
	}
	return req, ok
}

// TestE1aCallbackStealPreventionLateCallResult tests that with request A
// in-flight and request B queued, after A is canceled, a late CALL_RESULT for
// A does NOT steal B's callback. ocppMessageHandler itself must still return
// nil — the discard is not surfaced as a protocol-level error (Finding 11:
// this file has no wired Errors()-style channel/API at the ocppj.Client
// level to inspect, so the assertion is deliberately narrowed to exactly
// what is checked, rather than a broader "no spurious error" claim).
func TestE1aCallbackStealPreventionLateCallResult(t *testing.T) {
	realState := NewClientState()
	blockState := newE1aBlockAfterGetState(realState)

	d, q, network, endpoint := e1aNewDispatcherWithState(t, 10, blockState)

	sent := make(chan struct{}, 8)
	network.setOnWrite(func(data []byte) error {
		sent <- struct{}{}
		return nil
	})

	// Construct the Client BEFORE starting the pump — NewClient writes
	// dispatcher fields (SetPendingRequestState/SetNetworkClient) without
	// synchronization, so an already-running pump could -race with those
	// writes.
	client := NewClient("e1a-steal", network, d, blockState, ocpp.NewProfile("mock", &e1aMockFeature{}))

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()

	// --- Request A: dispatch it ---
	bundleA, requestA := e1aNewBundle(t, endpoint, "a")
	require.NoError(t, d.SendRequest(bundleA))
	select {
	case <-sent:
	case <-time.After(e1aBound):
		t.Fatal("timed out waiting for A to be dispatched")
	}
	require.True(t, realState.HasPendingRequest())

	// --- Request B: queue it behind A ---
	bundleB, requestB := e1aNewBundle(t, endpoint, "b")
	require.NoError(t, d.SendRequest(bundleB))
	assert.Equal(t, 2, q.Size(), "queue must hold A (pending) and B (queued)")

	var responseCalls int32
	client.SetResponseHandler(func(response ocpp.Response, reqID string) {
		atomic.AddInt32(&responseCalls, 1)
	})

	// Arm the block: the NEXT GetPendingRequest(A) call (made by ParseMessage
	// when handling the "late" response below) will compute its real result
	// — true, since A is still genuinely pending right now — and then block,
	// held open until this test explicitly releases it.
	blockState.setBlockRequestID(requestA)

	callResultJSON := []byte(fmt.Sprintf(`[3,"%s",{"mockValue":"late-response"}]`, requestA))

	// Deliver the "late" CALL_RESULT for A on its own goroutine.
	handlerDone := make(chan error, 1)
	go func() {
		handlerDone <- client.ocppMessageHandler(callResultJSON)
	}()

	select {
	case <-blockState.entered:
	case <-time.After(e1aBound):
		t.Fatal("timed out waiting for the late response's pending-check to block")
	}

	// NOW cancel A (simulate a timeout winning the race): CompleteRequest(A)
	// pops A for real, making B the new front. This happens strictly BEFORE
	// the blocked pending-check — and therefore the CompleteRequest gate
	// inside ocppMessageHandler — is allowed to proceed.
	ok := d.CompleteRequest(requestA)
	assert.True(t, ok, "CompleteRequest(A) must return true — A was front")
	assert.Equal(t, 1, q.Size(), "B is now the only element in the queue")
	el := q.Peek()
	require.NotNil(t, el)
	frontBundle, fbOk := el.(RequestBundle)
	require.True(t, fbOk)
	require.Equal(t, requestB, frontBundle.Call.UniqueId, "B must be front before releasing the blocked pending-check")

	// Release: the late response's pending-check (already computed true) now
	// returns, ParseMessage proceeds, and ocppMessageHandler reaches the
	// CompleteRequest(A) gate — which must now find B at the front and LOSE.
	close(blockState.release)

	select {
	case err := <-handlerDone:
		require.NoError(t, err)
	case <-time.After(e1aBound):
		t.Fatal("timed out waiting for the late response's ocppMessageHandler to finish")
	}

	assert.Equal(t, int32(0), atomic.LoadInt32(&responseCalls),
		"late CALL_RESULT for A must NOT invoke responseHandler — B's callback must not be stolen")
	assert.Equal(t, 1, q.Size(), "B must still be in the queue, untouched")

	// --- B must still be completable ---
	realState.AddPendingRequest(requestB, &e1aMockRequest{MockValue: "b"})
	ok = d.CompleteRequest(requestB)
	assert.True(t, ok, "CompleteRequest(B) must succeed — B is front")
	assert.True(t, q.IsEmpty())
}

// TestE1aCallbackStealPreventionLateCallError is the CALL_ERROR sibling of
// TestE1aCallbackStealPreventionLateCallResult — same hooked-state technique
// (Finding 1), same narrowed assertion (Finding 11).
func TestE1aCallbackStealPreventionLateCallError(t *testing.T) {
	realState := NewClientState()
	blockState := newE1aBlockAfterGetState(realState)

	d, q, network, endpoint := e1aNewDispatcherWithState(t, 10, blockState)

	sent := make(chan struct{}, 8)
	network.setOnWrite(func(data []byte) error {
		sent <- struct{}{}
		return nil
	})

	// Construct the Client BEFORE starting the pump — NewClient writes
	// dispatcher fields (SetPendingRequestState/SetNetworkClient) without
	// synchronization, so an already-running pump could -race with those
	// writes.
	client := NewClient("e1a-steal-err", network, d, blockState, ocpp.NewProfile("mock", &e1aMockFeature{}))

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()

	// Dispatch A, queue B.
	bundleA, requestA := e1aNewBundle(t, endpoint, "a")
	require.NoError(t, d.SendRequest(bundleA))
	select {
	case <-sent:
	case <-time.After(e1aBound):
		t.Fatal("timed out waiting for A to be dispatched")
	}
	require.True(t, realState.HasPendingRequest())

	bundleB, requestB := e1aNewBundle(t, endpoint, "b")
	require.NoError(t, d.SendRequest(bundleB))
	assert.Equal(t, 2, q.Size())

	var errorCalls int32
	client.SetErrorHandler(func(err *ocpp.Error, details interface{}) {
		atomic.AddInt32(&errorCalls, 1)
	})

	blockState.setBlockRequestID(requestA)

	callErrorJSON := []byte(fmt.Sprintf(`[4,"%s","GenericError","late-error",{}]`, requestA))

	handlerDone := make(chan error, 1)
	go func() {
		handlerDone <- client.ocppMessageHandler(callErrorJSON)
	}()

	select {
	case <-blockState.entered:
	case <-time.After(e1aBound):
		t.Fatal("timed out waiting for the late error's pending-check to block")
	}

	// Cancel A.
	ok := d.CompleteRequest(requestA)
	assert.True(t, ok)
	assert.Equal(t, 1, q.Size())
	el := q.Peek()
	require.NotNil(t, el)
	frontBundle, fbOk := el.(RequestBundle)
	require.True(t, fbOk)
	require.Equal(t, requestB, frontBundle.Call.UniqueId, "B must be front before releasing the blocked pending-check")

	close(blockState.release)

	select {
	case err := <-handlerDone:
		require.NoError(t, err)
	case <-time.After(e1aBound):
		t.Fatal("timed out waiting for the late error's ocppMessageHandler to finish")
	}

	assert.Equal(t, int32(0), atomic.LoadInt32(&errorCalls),
		"late CALL_ERROR for A must NOT invoke errorHandler — B's callback must not be stolen")
	assert.Equal(t, 1, q.Size(), "B must still be in the queue, untouched")

	// B still usable.
	realState.AddPendingRequest(requestB, &e1aMockRequest{MockValue: "b"})
	ok = d.CompleteRequest(requestB)
	assert.True(t, ok)
	assert.True(t, q.IsEmpty())
}

// ============================================================================
// Test 4b — call-site ownership gate: write-error path suppresses
// fireRequestCancel when CompleteRequest loses (Finding 8)
// ============================================================================

// TestE1aWriteErrorCallSiteFireRequestCancelSuppressedWhenCompleteRequestLoses
// deterministically forces the write-error call site's own CompleteRequest
// call (dispatchNextRequest's `if err != nil { ... }` branch,
// dispatcher.go:356) to LOSE its ownership race, and asserts that
// onRequestCanceled is therefore NOT invoked for that id — pinning the A2
// contract ("Update the pump call sites ... to fire cancel only when it
// returns true").
//
// Mechanism: the fake network's Write call blocks (synchronously, on the
// pump goroutine, inside dispatchNextRequest — after AddPendingRequest has
// already run) until released. While blocked, the test goroutine externally
// wins the race for the SAME requestID via a direct CompleteRequest call,
// simulating a genuine response arriving concurrently with the eventual
// write failure. Releasing Write then lets the write-error call site attempt
// its own CompleteRequest(id) for a request that is now already popped —
// which must return false and therefore must NOT call fireRequestCancel.
func TestE1aWriteErrorCallSiteFireRequestCancelSuppressedWhenCompleteRequestLoses(t *testing.T) {
	d, state, q, network, endpoint := e1aNewDispatcher(t, 10)

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	network.setOnWrite(func(data []byte) error {
		once.Do(func() { close(entered) })
		<-release
		return errors.New("simulated write failure")
	})

	canceledIDs := make(chan string, 4)
	d.SetOnRequestCanceled(func(requestID string, request ocpp.Request, err *ocpp.Error) {
		canceledIDs <- requestID
	})

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()

	bundle, requestID := e1aNewBundle(t, endpoint, "race")
	require.NoError(t, d.SendRequest(bundle))

	select {
	case <-entered:
	case <-time.After(e1aBound):
		t.Fatal("timed out waiting for the pump to enter the blocking Write call")
	}
	// dispatchNextRequest calls AddPendingRequest BEFORE Write, so the
	// request is genuinely pending and genuinely front right now.
	require.True(t, state.HasPendingRequest())

	// Externally WIN the race for this same request while the pump is still
	// blocked inside Write.
	ok := d.CompleteRequest(requestID)
	require.True(t, ok, "external CompleteRequest must win — it runs while the write-error call site is still blocked in Write")
	require.True(t, q.IsEmpty())

	// Release the blocked Write call: it returns its error, and
	// dispatchNextRequest's own write-error call site now attempts
	// CompleteRequest(requestID) for the SAME id — which must LOSE (already
	// popped) and therefore must NOT invoke fireRequestCancel.
	close(release)

	select {
	case rid := <-canceledIDs:
		t.Fatalf("write-error call site fired cancel for %s despite losing its own CompleteRequest race", rid)
	case <-time.After(e1aSilenceBound):
	}

	// NOTE (Finding 8, residual gap): this deterministically pins the
	// write-error call site only. The dispatcher timeout call site
	// (messagePump's `<-d.timer.C` branch, dispatcher.go:~291-300) uses the
	// identical `if d.CompleteRequest(id) { fireRequestCancel(...) }` gating
	// pattern per the PR-E1a spec, but forcing ITS specific
	// Peek-then-CompleteRequest TOCTOU window open deterministically
	// (analogous to d2BlockingPopQueue in d2_token_identity_test.go) was not
	// attempted here. A dedicated timeout-path race harness remains a
	// residual gap for a follow-up.
}

// ============================================================================
// Test 5 — Wrong-request identity: stale completion does not remove B
// ============================================================================

func TestE1aWrongRequestIdentityStaleCompletionDoesNotRemoveB(t *testing.T) {
	d, state, q, network, endpoint := e1aNewDispatcher(t, 10)

	sent := make(chan struct{}, 8)
	network.setOnWrite(func(data []byte) error {
		sent <- struct{}{}
		return nil
	})

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()

	bundleA, requestA := e1aNewBundle(t, endpoint, "a")
	require.NoError(t, d.SendRequest(bundleA))
	select {
	case <-sent:
	case <-time.After(e1aBound):
		t.Fatal("timed out waiting for A to be dispatched")
	}
	require.True(t, state.HasPendingRequest())

	// Queue B behind A.
	bundleB, requestB := e1aNewBundle(t, endpoint, "b")
	require.NoError(t, d.SendRequest(bundleB))
	assert.Equal(t, 2, q.Size())

	// A completes normally — it IS the front, so CompleteRequest(A) → true.
	// NOTE: the pump may have already dispatched B by this point (A's
	// completion send a readiness wakeup), so HasPendingRequest() can
	// legitimately be TRUE for B — we do NOT assert it here.
	ok := d.CompleteRequest(requestA)
	assert.True(t, ok, "CompleteRequest(A) must return true on first call")
	assert.Equal(t, 1, q.Size(), "B must be the only remaining element")

	// Now B is the front. A stale completion for A must return false and NOT
	// remove B.
	ok = d.CompleteRequest(requestA)
	assert.False(t, ok, "stale CompleteRequest(A) must return false — B is front, not A")
	assert.Equal(t, 1, q.Size(), "B must NOT have been removed by stale completion of A")

	// Verify B's identity: the front must be B.
	el := q.Peek()
	require.NotNil(t, el, "queue must not be empty")
	bundle, ok2 := el.(RequestBundle)
	require.True(t, ok2)
	assert.Equal(t, requestB, bundle.Call.UniqueId, "front must be B, not something else")

	// B can be completed normally.
	state.AddPendingRequest(requestB, bundleB.Call.Payload)
	ok = d.CompleteRequest(requestB)
	assert.True(t, ok, "CompleteRequest(B) must return true")
	assert.True(t, q.IsEmpty())
}

// ============================================================================
// Test 5b — Completion→readiness→dispatch-next progress chain
// ============================================================================

// TestE1aCompletionWakesPumpToDispatchNext verifies that completing the pending
// front request sends a readiness wakeup that causes the pump to dispatch the
// next queued request within a bounded time. This guards against a regression
// where CompleteRequest pops the front but fails to signal readiness — the
// suite would otherwise pass (the queue is correct, the pending state is
// correct) but the second request would never be dispatched, hanging forever
// in production.
func TestE1aCompletionWakesPumpToDispatchNext(t *testing.T) {
	d, state, _, network, endpoint := e1aNewDispatcher(t, 10)

	sent := make(chan struct{}, 8)
	network.setOnWrite(func(data []byte) error {
		sent <- struct{}{}
		return nil
	})

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()

	// Dispatch A.
	bundleA, requestA := e1aNewBundle(t, endpoint, "a")
	require.NoError(t, d.SendRequest(bundleA))
	select {
	case <-sent:
	case <-time.After(e1aBound):
		t.Fatal("timed out waiting for A to be dispatched")
	}
	require.True(t, state.HasPendingRequest())

	// Queue B behind A — B sits in the queue because A is pending.
	bundleB, _ := e1aNewBundle(t, endpoint, "b")
	require.NoError(t, d.SendRequest(bundleB))

	// Complete A. This must pop A, clear its pending state, and — critically —
	// send a readiness wakeup to the pump so B gets dispatched.
	ok := d.CompleteRequest(requestA)
	require.True(t, ok, "CompleteRequest(A) must return true — A was front")

	// Now B must be dispatched BY THE PUMP within e1aBound. If the
	// completion did not wake the pump, this select hangs forever.
	select {
	case <-sent:
		// B was dispatched — the completion→readiness→dispatch chain is intact.
	case <-time.After(e1aBound):
		t.Fatal("B was never dispatched after completing A — completion did not wake the pump to dispatch the next request")
	}

	// B must now be the pending request.
	require.True(t, state.HasPendingRequest(), "B must be pending after dispatch")
}

// ============================================================================
// Test 6 — Pause/resume no double-dispatch
// ============================================================================

// TestE1aPauseResumeNoDoubleDispatch verifies that after pausing the
// dispatcher, canceling the pending request A, queuing B, and resuming,
// exactly ONE network write happens for B (not two). The readiness rework
// (coalesced, non-blocking readyForDispatch + level-based pump) must prevent
// double-dispatch from a redundant wakeup.
//
// Finding 4: writeCount was previously checked immediately after observing
// B's first write, which is too early — a stale readiness could still cause
// a buggy second write to land right AFTER that premature assertion, and the
// test would falsely pass. Fixed by waiting a full silence window (proving
// NO second write follows) BEFORE asserting the final count, and by raising
// SetTimeout well above any window used in this test, so the assertion is
// isolated from timeout behavior entirely.
func TestE1aPauseResumeNoDoubleDispatch(t *testing.T) {
	d, state, q, network, endpoint := e1aNewCountingDispatcher(t, 10)

	sent := make(chan struct{}, 8)
	network.setOnWrite(func(data []byte) error {
		sent <- struct{}{}
		return nil
	})

	d.SetTimeout(5 * time.Second) // well above any window used below
	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()

	// Dispatch A.
	bundleA, requestA := e1aNewBundle(t, endpoint, "a")
	require.NoError(t, d.SendRequest(bundleA))
	select {
	case <-sent:
	case <-time.After(e1aBound):
		t.Fatal("timed out waiting for A to be dispatched")
	}
	require.True(t, state.HasPendingRequest())
	assert.Equal(t, 1, network.writeCount(), "one write for A")

	// Pause the dispatcher.
	d.Pause()
	assert.True(t, d.IsPaused())

	// Cancel/complete A while paused.
	ok := d.CompleteRequest(requestA)
	assert.True(t, ok, "CompleteRequest(A) must return true")
	assert.False(t, state.HasPendingRequest())
	assert.True(t, q.IsEmpty())

	// Queue B while paused.
	bundleB, _ := e1aNewBundle(t, endpoint, "b")
	require.NoError(t, d.SendRequest(bundleB))
	assert.Equal(t, 1, q.Size(), "B is queued but not dispatched")

	// Give the pump time to prove it's NOT dispatching B while paused.
	select {
	case <-sent:
		t.Fatal("B must NOT be dispatched while dispatcher is paused")
	case <-time.After(e1aSilenceBound):
	}

	// Resume: the pump must dispatch exactly ONE more write (for B).
	d.Resume()
	assert.False(t, d.IsPaused())

	select {
	case <-sent:
	case <-time.After(e1aBound):
		t.Fatal("timed out waiting for B to be dispatched after Resume")
	}

	// Finding 4: prove no SECOND write follows by waiting a full silence
	// window BEFORE the final assertion — not immediately after observing
	// the first one.
	select {
	case <-sent:
		t.Fatal("unexpected second write for B — double-dispatch from a stale/redundant readiness wakeup")
	case <-time.After(e1aSilenceBound):
	}

	// Assert exactly ONE additional write (total = 2: one for A, one for B).
	assert.Equal(t, 2, network.writeCount(), "exactly one write for B after resume — no double-dispatch")
	assert.True(t, state.HasPendingRequest(), "B must be pending after dispatch")
}

// TestE1aRedundantWakeupDoesNotDoubleDispatch pins the LEVEL-BASED pump
// (Finding 5) directly: with A dispatched and genuinely pending, a REDUNDANT
// readiness wakeup must not cause a second write, because the pump
// re-derives authoritative dispatch state (HasPendingRequest()) every
// iteration instead of trusting the wakeup payload (A3's "condition-variable
// pattern" design).
//
// A spurious extra wakeup while a request is already pending has no clean
// public-API trigger: Resume() while a request is pending only resets the
// timer and never sends on readyForDispatch (see dispatcher.go Resume), so
// there is no way to inject a redundant wakeup through Pause/Resume alone.
// This test instead injects directly on the dispatcher's private readiness
// channel — the SAME white-box technique already used throughout
// d2_token_identity_test.go for pinning stale/duplicate-token semantics on
// the server dispatcher (that file's own comment documents this precedent).
//
// This line references d.readyForDispatch at its CURRENT declared type
// (chan bool). If PR-E1a's implementation changes the channel's element type
// (e.g. to chan struct{}, per A3's coalesced-send sketch), only this literal
// needs a matching update — no other test logic changes (same convention as
// d2_token_identity_test.go's file-level comment about timerC/readyForDispatch
// element shapes).
func TestE1aRedundantWakeupDoesNotDoubleDispatch(t *testing.T) {
	d, state, _, network, endpoint := e1aNewCountingDispatcher(t, 10)

	sent := make(chan struct{}, 8)
	network.setOnWrite(func(data []byte) error {
		sent <- struct{}{}
		return nil
	})

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()

	bundleA, _ := e1aNewBundle(t, endpoint, "a")
	require.NoError(t, d.SendRequest(bundleA))
	select {
	case <-sent:
	case <-time.After(e1aBound):
		t.Fatal("timed out waiting for A to be dispatched")
	}
	require.True(t, state.HasPendingRequest())
	assert.Equal(t, 1, network.writeCount())

	// Inject a redundant readiness wakeup directly.
	select {
	case d.readyForDispatch <- true:
	case <-time.After(e1aBound):
		t.Fatal("could not inject redundant readiness wakeup")
	}

	// No second write must follow: A is still pending, so the level-based
	// pump must ignore this wakeup entirely.
	select {
	case <-sent:
		t.Fatal("redundant wakeup caused a second write while A was still pending")
	case <-time.After(e1aSilenceBound):
	}
	assert.Equal(t, 1, network.writeCount(), "redundant wakeup must not cause a second dispatch")
	assert.True(t, state.HasPendingRequest())
}

// TestE1aCompleteRequestReadinessSendIsNonBlocking deterministically pins
// A3's "coalesced, non-blocking readiness" requirement (Finding 6) — the
// self-send-deadlock fix. The pump is never started, so nothing ever drains
// d.readyForDispatch (cap-1). CompleteRequest(A) wins and fills that buffer
// with its own readiness signal. CompleteRequest(B) must then ALSO win (B is
// now front) and must return promptly despite the buffer already being full:
// against a blocking `d.readyForDispatch <- true` send this would hang
// forever (nothing ever drains it); the required non-blocking,
// drop-if-buffered send must let it return well within a short bound.
func TestE1aCompleteRequestReadinessSendIsNonBlocking(t *testing.T) {
	d, state, q, _, endpoint := e1aNewDispatcher(t, 10)
	// No d.Start() — the pump does NOT run.

	bundleA, requestA := e1aNewBundle(t, endpoint, "a")
	bundleB, requestB := e1aNewBundle(t, endpoint, "b")
	require.NoError(t, q.Push(bundleA))
	require.NoError(t, q.Push(bundleB))
	state.AddPendingRequest(requestA, &e1aMockRequest{MockValue: "a"})

	// CompleteRequest(A): A is genuinely front, wins, pops A, and — per A3 —
	// sends a coalesced readiness signal. With nothing running to drain
	// d.readyForDispatch (cap-1), this fills the buffer.
	ok := d.CompleteRequest(requestA)
	require.True(t, ok, "CompleteRequest(A) must win — A was front")
	require.Equal(t, 1, q.Size(), "B must now be the only element")

	state.AddPendingRequest(requestB, &e1aMockRequest{MockValue: "b"})

	// CompleteRequest(B): B is now front, must also win. Its own end-of-call
	// readiness send hits an ALREADY-FULL buffer (nothing ever drained A's
	// signal). A blocking send here would hang forever; the required
	// non-blocking, drop-if-buffered send must let this return promptly
	// regardless.
	completeBDone := make(chan bool, 1)
	go func() {
		completeBDone <- d.CompleteRequest(requestB)
	}()

	select {
	case okB := <-completeBDone:
		assert.True(t, okB, "CompleteRequest(B) must win — B was front")
	case <-time.After(e1aBound):
		t.Fatal("CompleteRequest(B) did not return within the bound — readiness send appears to be BLOCKING against an already-full buffer with nothing draining it (self-send-deadlock regression)")
	}

	assert.True(t, q.IsEmpty())
	assert.False(t, state.HasPendingRequest())
}

// ============================================================================
// Test 7 — Stop-vs-completion exactly-once (DrainAll racing PopIf)
// ============================================================================

// TestE1aStopVsCompletionExactlyOnce verifies the exactly-once invariant when
// Stop's DrainAll races a CompleteRequest for the same in-flight request.
//
// DrainAll and PopIf (inside CompleteRequest) serialize on the same queue
// mutex per the spec, so the outcome is ordered but not pre-determined. This
// test runs CompleteRequest and Stop concurrently and relies on the real
// queue mutex to serialize them. It does NOT assert which side wins — it only
// asserts that exactly one terminal outcome happens for the request: either
// CompleteRequest returns true (popped via PopIf) XOR the onRequestCanceled
// callback fires once (DrainAll cancelled it). Never both, never zero, never
// two. The queue must be empty after both goroutines finish.
//
// Finding 3: the original single-shot version of this test could pass even
// with weak/no synchronization, because the CompleteRequest goroutine may
// simply always finish before Stop reaches DrainAll (no real contention).
// Strengthened with (a) a start-barrier — both goroutines block on a shared
// channel closed right before the race, so they are released together and
// actually contend for the queue mutex — and (b) 50 repeated iterations,
// each with a fresh dispatcher, asserting the invariant every time. The
// deterministic lock-based proof is TestE1aPopIfSerializesWithDrainAllOnQueueMutex
// (Finding 2); this test strengthens the probabilistic, end-to-end version.
func TestE1aStopVsCompletionExactlyOnce(t *testing.T) {
	const iterations = 50
	for i := 0; i < iterations; i++ {
		i := i
		t.Run(fmt.Sprintf("iteration_%d", i), func(t *testing.T) {
			d, state, q, network, endpoint := e1aNewDispatcher(t, 10)

			sent := make(chan struct{}, 8)
			network.setOnWrite(func(data []byte) error {
				sent <- struct{}{}
				return nil
			})

			var cancelCount int32
			d.SetOnRequestCanceled(func(requestID string, request ocpp.Request, err *ocpp.Error) {
				atomic.AddInt32(&cancelCount, 1)
			})

			d.Start()
			require.True(t, d.IsRunning())

			bundle, requestID := e1aNewBundle(t, endpoint, "x")
			require.NoError(t, d.SendRequest(bundle))
			select {
			case <-sent:
			case <-time.After(e1aBound):
				t.Fatal("timed out waiting for request to be dispatched")
			}
			require.True(t, state.HasPendingRequest())

			// Start-barrier: both goroutines wait on this shared channel and
			// are released together, right before the race, so they actually
			// contend for the queue mutex instead of one side habitually
			// finishing first.
			start := make(chan struct{})

			var completeReturnedTrue bool
			completeDone := make(chan struct{})
			go func() {
				<-start
				completeReturnedTrue = d.CompleteRequest(requestID)
				close(completeDone)
			}()

			stopDone := make(chan struct{})
			go func() {
				<-start
				d.Stop()
				close(stopDone)
			}()

			close(start)

			select {
			case <-completeDone:
			case <-time.After(e1aBound):
				t.Fatal("CompleteRequest did not return")
			}
			select {
			case <-stopDone:
			case <-time.After(e1aBound):
				t.Fatal("Stop did not return")
			}

			cancelCnt := atomic.LoadInt32(&cancelCount)

			// cancelCount must be 0 or 1 — never 2 (double delivery).
			assert.True(t, cancelCnt == 0 || cancelCnt == 1,
				"cancelCount must be 0 or 1, got %d", cancelCnt)

			// Exactly-one invariant: CompleteRequest returned true XOR cancel fired.
			assert.True(t, completeReturnedTrue != (cancelCnt == 1),
				"exactly-one terminal outcome violated: CompleteRequest returned %v, cancelCount=%d",
				completeReturnedTrue, cancelCnt)

			// Queue must be empty — the request was either popped or drained.
			assert.True(t, q.IsEmpty(), "queue must be empty after both CompleteRequest and Stop finish")
		})
	}
}

// ============================================================================
// Test 8 — Error semantics: ErrRequestCanceled + Cause/Unwrap
// ============================================================================

// TestE1aRequestCanceledErrorWrapsContextCanceled verifies that
// newRequestCanceledError with context.Canceled can be unwrapped to
// context.Canceled via errors.Is.
func TestE1aRequestCanceledErrorWrapsContextCanceled(t *testing.T) {
	err := newRequestCanceledError("test-msg-id", context.Canceled)
	require.NotNil(t, err)

	assert.True(t, errors.Is(err, context.Canceled),
		"request-canceled error caused by context.Canceled must match context.Canceled")
}

// TestE1aRequestCanceledErrorWrapsContextDeadlineExceeded verifies that
// newRequestCanceledError with context.DeadlineExceeded matches it.
func TestE1aRequestCanceledErrorWrapsContextDeadlineExceeded(t *testing.T) {
	err := newRequestCanceledError("test-msg-id", context.DeadlineExceeded)
	require.NotNil(t, err)

	assert.True(t, errors.Is(err, context.DeadlineExceeded),
		"request-canceled error caused by context.DeadlineExceeded must match context.DeadlineExceeded")
}

// TestE1aRequestCanceledErrorMatchesErrRequestCanceled verifies that
// errors.Is on the new sentinel works.
func TestE1aRequestCanceledErrorMatchesErrRequestCanceled(t *testing.T) {
	err := newRequestCanceledError("test-msg-id", context.Canceled)
	require.NotNil(t, err)

	assert.True(t, errors.Is(err, ErrRequestCanceled),
		"request-canceled error must match ErrRequestCanceled sentinel")
}

// TestE1aRequestCanceledErrorFields asserts ALL fields of
// newRequestCanceledError (Finding 10), not just the errors.Is outcome:
// Code, Description, MessageId, and Cause == the passed context error.
func TestE1aRequestCanceledErrorFields(t *testing.T) {
	cause := context.Canceled
	err := newRequestCanceledError("test-msg-id", cause)
	require.NotNil(t, err)

	assert.Equal(t, GenericError, err.Code, "Code must be GenericError per the spec")
	assert.Equal(t, cause.Error(), err.Description, "Description must be the cause's message")
	assert.Equal(t, "test-msg-id", err.MessageId)
	assert.Equal(t, cause, err.Cause, "Cause must be exactly the passed context error")
}

// TestE1aRequestCanceledErrorDoesNotMatchOtherSentinels is a REGRESSION guard:
// the new canceled-error marker must not collide with existing sentinels.
//
// Finding 9: the original version of this test asserted things like
// errors.Is(ErrRequestTimeout, ErrRequestTimeout), which passes vacuously by
// pointer identity — Error.Is/Unwrap never actually run for that call.
// Replaced with FRESH, freshly-constructed errors matched against their
// sentinels via the real construction+matching path, and removed the
// duplicated negative checks.
func TestE1aRequestCanceledErrorDoesNotMatchOtherSentinels(t *testing.T) {
	cancelErr := newRequestCanceledError("x", context.Canceled)

	timeoutErr := newRequestTimeoutError("y")
	assert.True(t, errors.Is(timeoutErr, ErrRequestTimeout),
		"a freshly constructed request-timeout error must match ErrRequestTimeout")

	stoppedErr := newDispatcherStoppedError("z")
	assert.True(t, errors.Is(stoppedErr, ErrDispatcherStopped),
		"a freshly constructed dispatcher-stopped error must match ErrDispatcherStopped")

	// Cross-sentinel negatives.
	assert.False(t, errors.Is(cancelErr, ErrRequestTimeout),
		"request-canceled error must NOT match ErrRequestTimeout")
	assert.False(t, errors.Is(cancelErr, ErrDispatcherStopped),
		"request-canceled error must NOT match ErrDispatcherStopped")
	assert.False(t, errors.Is(cancelErr, ErrLocalTransport),
		"request-canceled error must NOT match ErrLocalTransport")
	assert.False(t, errors.Is(timeoutErr, ErrRequestCanceled),
		"request-timeout error must NOT match ErrRequestCanceled")
	assert.False(t, errors.Is(stoppedErr, ErrRequestCanceled),
		"dispatcher-stopped error must NOT match ErrRequestCanceled")
}
