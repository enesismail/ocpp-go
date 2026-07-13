package ocppj

// PR-E1c (tasks/e1c-context-aware-send.md) RED-FIRST test suite.
//
// This file lives in `package ocppj` (not `ocppj_test`) so it can reach
// the unexported newRequestCanceledError constructor, DefaultClientDispatcher's
// concrete fields, and FIFOClientQueue's internals — exactly like
// e1a_completion_ownership_test.go. It reuses the same e1a fake network
// helpers (e1aClient, e1aCountingClient, e1aMockFeature, etc.) defined in
// that file.
//
// RED-FIRST discipline: every test below references the PR-E1c surface exactly
// as the spec names it. Against today's codebase:
//   - Client.SendRequestCtx does not exist
//   - RequestBundle.Ctx does not exist
//   - dispatchNextRequest returns bool (not (pumpPending, bool))
//   - pumpPending struct does not exist
//   - The in-flight ctx-cancel pump arm does not exist
//
// This whole file is EXPECTED to fail compilation — that IS the intended red
// state pinning the PR-E1c contract.
//
// Spec tests implemented: 1, 2, 3, 3b, 5, 6.

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/enesismail/ocpp-go/ocpp"
)

// ============================================================================
// Dedicated message-ID recorder for E1c tests (deterministic, distinct)
// ============================================================================

// e1cIDRecorder is a deterministic message-ID generator that records every
// generated ID so tests can capture the REAL dispatched ID via nth() after
// SendRequestCtx returns (which calls CreateCall internally, generating a
// fresh ID — the test MUST NOT call CreateCall just to capture an id).
type e1cIDRecorder struct {
	mu  sync.Mutex
	n   int
	ids []string
}

func (r *e1cIDRecorder) gen() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := fmt.Sprintf("e1c-req-%d", r.n)
	r.n++
	r.ids = append(r.ids, id)
	return id
}

// nth returns the i-th generated ID (0-indexed). Must only be called after
// at least i+1 IDs have been generated.
func (r *e1cIDRecorder) nth(i int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ids[i]
}

// e1cSetIDGeneratorRestoringDefault installs rec.gen as the message-ID
// generator and returns a function that restores the documented default
// (rand.Uint32). There is no getter for the current generator, so a true
// save/restore is not possible — this helper restores to the default.
func e1cSetIDGeneratorRestoringDefault(t *testing.T, rec *e1cIDRecorder) func() {
	t.Helper()
	SetMessageIdGenerator(rec.gen)
	return func() {
		SetMessageIdGenerator(func() string {
			return fmt.Sprintf("%d", rand.Uint32())
		})
	}
}

// e1cBound is the bounded deadline for E1c tests. Every blocking assertion
// uses a select with this timeout so a missing (or broken) fix cannot hang.
const e1cBound = 2 * time.Second
const e1cSilenceBound = 300 * time.Millisecond

// ============================================================================
// Test 1 — Pre-write drop (spec test 1)
// ============================================================================

// TestE1cPreWriteDropAlreadyCanceledCtx verifies that SendRequestCtx with an
// already-canceled context:
//  1. Does NOT produce a network write for that request.
//  2. Fires onRequestCanceled exactly once, with an error matching both
//     ErrRequestCanceled AND context.Canceled.
//  3. A following live-ctx request IS still written.
//
// The spec locks out a fast-fail at submit: the canceled bundle MUST reach
// the dispatcher pre-write drop (C3a), not be rejected by SendRequestCtx.
func TestE1cPreWriteDropAlreadyCanceledCtx(t *testing.T) {
	rec := &e1cIDRecorder{}
	restoreGen := e1cSetIDGeneratorRestoringDefault(t, rec)
	defer restoreGen()

	d, state, _, network, endpoint := e1aNewCountingDispatcher(t, 10)
	endpoint.dispatcher = d // wire SendRequestCtx to the dispatcher under test (e1a helper leaves it nil)

	canceled := make(chan string, 8)
	var cancelErrs []*ocpp.Error
	var cancelMu sync.Mutex
	d.SetOnRequestCanceled(func(requestID string, request ocpp.Request, err *ocpp.Error) {
		cancelMu.Lock()
		cancelErrs = append(cancelErrs, err)
		cancelMu.Unlock()
		canceled <- requestID
	})

	// Accept writes so B goes through.
	network.setOnWrite(func(data []byte) error { return nil })

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()

	// --- Request A: already-canceled ctx ---
	ctxA, cancelA := context.WithCancel(context.Background())
	cancelA() // cancel immediately

	reqA := &e1aMockRequest{MockValue: "pre-write-a"}

	// PR-E1c: SendRequestCtx exists and enqueues with Ctx set.
	// SendRequestCtx calls CreateCall INTERNALLY, generating a fresh id.
	// Capture the REAL dispatched id via the recorder.
	err := endpoint.SendRequestCtx(ctxA, reqA)
	require.NoError(t, err, "SendRequestCtx must NOT fast-fail on an already-canceled context")
	idA := rec.nth(0)

	// --- Request B: live ctx ---
	reqB := &e1aMockRequest{MockValue: "pre-write-b"}
	err = endpoint.SendRequestCtx(context.Background(), reqB)
	require.NoError(t, err)

	// Verify A's cancel fired exactly once.
	select {
	case rid := <-canceled:
		assert.Equal(t, idA, rid, "canceled request must be A")
	case <-time.After(e1cBound):
		t.Fatal("timed out waiting for onRequestCanceled for A")
	}
	// No second cancel.
	select {
	case rid := <-canceled:
		t.Fatalf("onRequestCanceled fired more than once: second ID=%s", rid)
	case <-time.After(e1cSilenceBound):
	}

	// Verify the cancel error matches both ErrRequestCanceled and context.Canceled.
	cancelMu.Lock()
	require.Len(t, cancelErrs, 1, "exactly one cancel error")
	cancelErr := cancelErrs[0]
	cancelMu.Unlock()
	assert.True(t, errors.Is(cancelErr, ErrRequestCanceled), "cancel error must match ErrRequestCanceled")
	assert.True(t, errors.Is(cancelErr, context.Canceled), "cancel error must match context.Canceled")

	// Verify exactly one write happened (for B). The canceled A must NOT
	// have been written.
	assert.Equal(t, 1, network.writeCount(), "exactly one write: B was dispatched, A was dropped pre-write")

	// Verify the pending request is B.
	assert.True(t, state.HasPendingRequest())
	_, ok := state.GetPendingRequest(idA)
	assert.False(t, ok, "A must NOT be pending — it was dropped pre-write")
}

// ============================================================================
// Test 2 — In-flight cancel (spec test 2)
// ============================================================================

// TestE1cInFlightCancelLateResponseNoDoubleDelivery verifies that:
//  1. Dispatching A (non-blocking Write so the pump keeps running),
//     then canceling A's ctx fires onRequestCanceled once with ErrRequestCanceled.
//  2. A subsequently-arriving late CALL_RESULT for A does NOT double-deliver
//     (the callback is owned exactly-once via CompleteRequest).
//  3. The late response for A does NOT cancel a now-front B.
//
// Uses DISTINCT explicit message IDs for A and B — never relies on id reuse.
func TestE1cInFlightCancelLateResponseNoDoubleDelivery(t *testing.T) {
	rec := &e1cIDRecorder{}
	restoreGen := e1cSetIDGeneratorRestoringDefault(t, rec)
	defer restoreGen()

	d, state, q, network, endpoint := e1aNewDispatcher(t, 10)
	endpoint.dispatcher = d // wire SendRequestCtx to the dispatcher under test (e1a helper leaves it nil)

	// Non-blocking Write: records each dispatch so the pump is NEVER frozen.
	// The pump calls Write INLINE on its single goroutine, so a blocking
	// Write deadlocks the pump AND any subsequent Stop().
	writtenC := make(chan string, 8)
	network.setOnWrite(func(data []byte) error {
		writtenC <- string(data)
		return nil
	})

	canceled := make(chan string, 8)
	d.SetOnRequestCanceled(func(requestID string, request ocpp.Request, err *ocpp.Error) {
		canceled <- requestID
	})

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()

	// --- Request A with a cancelable ctx ---
	ctxA, cancelA := context.WithCancel(context.Background())
	reqA := &e1aMockRequest{MockValue: "inflight-a"}

	err := endpoint.SendRequestCtx(ctxA, reqA)
	require.NoError(t, err)

	// Wait for A to be dispatched (written). After this, A is in-flight/pending
	// and the pump has armed the pendingDone ctx arm.
	select {
	case <-writtenC:
	case <-time.After(e1cBound):
		t.Fatal("A was not dispatched")
	}
	idA := rec.nth(0)

	// --- Queue B behind A (live ctx) ---
	reqB := &e1aMockRequest{MockValue: "inflight-b"}
	err = endpoint.SendRequestCtx(context.Background(), reqB)
	require.NoError(t, err)
	idB := rec.nth(1)
	require.NotEqual(t, idA, idB, "message IDs must be distinct")

	assert.Equal(t, 2, q.Size(), "A (pending) + B (queued) must both be in the queue")

	// --- Cancel A's ctx ---
	cancelA()

	// onRequestCanceled must fire exactly once for A.
	select {
	case rid := <-canceled:
		assert.Equal(t, idA, rid, "canceled request must be A")
	case <-time.After(e1cBound):
		t.Fatal("timed out waiting for onRequestCanceled for A after ctx cancel")
	}
	select {
	case rid := <-canceled:
		t.Fatalf("onRequestCanceled fired more than once: second ID=%s", rid)
	case <-time.After(e1cSilenceBound):
	}

	// A must be gone from pending state (cancel popped it).
	_, ok := state.GetPendingRequest(idA)
	assert.False(t, ok, "A must NOT be pending after ctx-cancel")

	// --- Late completion for A must return false (E1a ownership already consumed) ---
	ok = d.CompleteRequest(idA)
	assert.False(t, ok, "CompleteRequest(idA) must return false — E1a ownership was already consumed by the cancel")

	// B must still be front, untouched — its onRequestCanceled never fired.
	assert.Equal(t, 1, q.Size(), "only B must remain in the queue")
	front := q.Peek()
	require.NotNil(t, front)
	frontBundle, fbOk := front.(RequestBundle)
	require.True(t, fbOk)
	assert.Equal(t, idB, frontBundle.Call.UniqueId, "B must be the front after A is canceled")

	// B must NOT have been canceled.
	select {
	case rid := <-canceled:
		t.Fatalf("B was unexpectedly canceled: ID=%s", rid)
	case <-time.After(e1cSilenceBound):
	}
}

// ============================================================================
// Test 3 — Queued-during-pause drop (spec test 3)
// ============================================================================

// TestE1cQueuedDuringPauseDrop verifies that when a ctx fires while the
// dispatcher is paused, on Resume the request is dropped (not written).
func TestE1cQueuedDuringPauseDrop(t *testing.T) {
	rec := &e1cIDRecorder{}
	restoreGen := e1cSetIDGeneratorRestoringDefault(t, rec)
	defer restoreGen()

	d, state, q, network, endpoint := e1aNewCountingDispatcher(t, 10)
	endpoint.dispatcher = d // wire SendRequestCtx to the dispatcher under test (e1a helper leaves it nil)

	// Accept writes normally.
	network.setOnWrite(func(data []byte) error { return nil })

	canceled := make(chan string, 8)
	d.SetOnRequestCanceled(func(requestID string, request ocpp.Request, err *ocpp.Error) {
		canceled <- requestID
	})

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()

	// Pause the dispatcher first — nothing dispatched.
	d.Pause()
	require.True(t, d.IsPaused())

	// Enqueue A with a cancelable ctx.
	ctxA, cancelA := context.WithCancel(context.Background())
	reqA := &e1aMockRequest{MockValue: "qdp-a"}

	err := endpoint.SendRequestCtx(ctxA, reqA)
	require.NoError(t, err)
	idA := rec.nth(0)
	assert.Equal(t, 1, q.Size(), "A must be queued while paused")

	// Cancel A's ctx while paused.
	cancelA()

	// onRequestCanceled must NOT fire while paused (nothing dispatched yet,
	// and the in-flight arm requires dispatched+pending which A is not).
	select {
	case rid := <-canceled:
		t.Fatalf("onRequestCanceled fired while paused for queued (not-dispatched) request %s", rid)
	case <-time.After(e1cSilenceBound):
	}

	// Resume the dispatcher.
	d.Resume()
	assert.False(t, d.IsPaused())

	// Now A's pre-write drop must fire the cancel.
	select {
	case rid := <-canceled:
		assert.Equal(t, idA, rid, "canceled request must be A")
	case <-time.After(e1cBound):
		t.Fatal("timed out waiting for onRequestCanceled for A after Resume")
	}

	// No second cancel.
	select {
	case rid := <-canceled:
		t.Fatalf("onRequestCanceled fired more than once after Resume: second ID=%s", rid)
	case <-time.After(e1cSilenceBound):
	}

	// A must NOT have been written (zero writes total).
	assert.Equal(t, 0, network.writeCount(), "A must not be written — it was dropped pre-write on Resume")
	assert.True(t, q.IsEmpty(), "queue must be empty after A was dropped")
	assert.False(t, state.HasPendingRequest(), "no request must be pending")
}

// ============================================================================
// Test 3b — In-flight cancel while paused (spec test 3b)
// ============================================================================

// TestE1cInFlightCancelWhilePaused verifies the distinct in-flight-while-paused
// path: dispatch A with a cancelable ctx, Pause, cancel A's ctx ⇒
// onRequestCanceled fires with ErrRequestCanceled WHILE STILL PAUSED
// (the ctx arm is live during pause — the IsPaused() check is post-select);
// Resume ⇒ next front B is dispatched.
//
// This test uses the REAL pump ctx arm — it does NOT manually call
// CompleteRequest or fireRequestCancel.
func TestE1cInFlightCancelWhilePaused(t *testing.T) {
	rec := &e1cIDRecorder{}
	restoreGen := e1cSetIDGeneratorRestoringDefault(t, rec)
	defer restoreGen()

	d, state, q, network, endpoint := e1aNewCountingDispatcher(t, 10)
	endpoint.dispatcher = d // wire SendRequestCtx to the dispatcher under test (e1a helper leaves it nil)

	// Non-blocking Write so the pump never freezes.
	writtenC := make(chan string, 8)
	network.setOnWrite(func(data []byte) error {
		writtenC <- string(data)
		return nil
	})

	canceled := make(chan string, 8)
	d.SetOnRequestCanceled(func(requestID string, request ocpp.Request, err *ocpp.Error) {
		canceled <- requestID
	})

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()

	// --- Dispatch A with a cancelable ctx ---
	ctxA, cancelA := context.WithCancel(context.Background())
	reqA := &e1aMockRequest{MockValue: "ifp-a"}

	err := endpoint.SendRequestCtx(ctxA, reqA)
	require.NoError(t, err)

	// Wait for A to be dispatched (written). A is now in-flight/pending.
	select {
	case <-writtenC:
	case <-time.After(e1cBound):
		t.Fatal("A was not dispatched")
	}
	idA := rec.nth(0)

	// --- Queue B behind A ---
	reqB := &e1aMockRequest{MockValue: "ifp-b"}
	err = endpoint.SendRequestCtx(context.Background(), reqB)
	require.NoError(t, err)
	idB := rec.nth(1)
	require.NotEqual(t, idA, idB)
	assert.Equal(t, 2, q.Size(), "A (pending) + B (queued)")

	// --- Pause the dispatcher while A is in-flight ---
	d.Pause()
	require.True(t, d.IsPaused())

	// --- Cancel A's ctx while paused ---
	// The pump's ctx arm is LIVE during pause (it's a select arm, and the
	// IsPaused() check is post-select). The arm fires CompleteRequest(idA)
	// and then fireRequestCancel — all on the pump goroutine.
	cancelA()

	// onRequestCanceled must fire WHILE STILL PAUSED.
	select {
	case rid := <-canceled:
		assert.Equal(t, idA, rid, "canceled request must be A")
	case <-time.After(e1cBound):
		t.Fatal("timed out waiting for onRequestCanceled for A — ctx arm must fire while paused")
	}

	// B must still be front, not canceled.
	assert.Equal(t, 1, q.Size(), "B must still be in the queue after A canceled while paused")
	front := q.Peek()
	require.NotNil(t, front)
	frontBundle, fbOk := front.(RequestBundle)
	require.True(t, fbOk)
	assert.Equal(t, idB, frontBundle.Call.UniqueId, "B must be the front")

	// --- Resume: B must be dispatched ---
	d.Resume()
	assert.False(t, d.IsPaused())

	// B must be dispatched by the pump.
	select {
	case <-writtenC:
		// B was dispatched.
	case <-time.After(e1cBound):
		t.Fatal("B was never dispatched after Resume")
	}

	// B must now be pending.
	require.True(t, state.HasPendingRequest(), "B must be pending after dispatch")

	// Clean up B.
	ok := d.CompleteRequest(idB)
	assert.True(t, ok, "CompleteRequest(B) must succeed")
}

// ============================================================================
// Test 5 — Ctx-less regression (spec test 5)
// ============================================================================

// TestE1cCtxLessSendRegression verifies that a ctx-less send (SendRequest,
// which delegates to SendRequestCtx with context.Background()) still dispatches
// and can be completed normally. The new ctx arm must be inert: Background().Done()
// is a nil channel, so the pump's pendingDone select arm blocks forever.
func TestE1cCtxLessSendRegression(t *testing.T) {
	d, state, q, network, endpoint := e1aNewDispatcher(t, 10)

	sent := make(chan struct{}, 8)
	network.setOnWrite(func(data []byte) error {
		sent <- struct{}{}
		return nil
	})

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()

	bundleA, requestA := e1aNewBundle(t, endpoint, "ctxless")
	// Use the original SendRequest (not SendRequestCtx) — it must still work.
	err := d.SendRequest(bundleA)
	require.NoError(t, err)

	select {
	case <-sent:
	case <-time.After(e1cBound):
		t.Fatal("timed out waiting for ctx-less request to be dispatched")
	}
	require.True(t, state.HasPendingRequest(), "ctx-less request must be pending after dispatch")

	// Complete normally.
	ok := d.CompleteRequest(requestA)
	assert.True(t, ok, "CompleteRequest must succeed for ctx-less request")
	assert.True(t, q.IsEmpty(), "queue must be empty after completion")
	assert.False(t, state.HasPendingRequest(), "no request must be pending after completion")
}

// ============================================================================
// Test 6 — Stop vs cancel exactly-once (spec test 6)
// ============================================================================

// TestE1cStopVsCancelExactlyOneTerminalError verifies that racing Stop and
// ctx-cancel yields exactly ONE terminal error (either ErrRequestCanceled or
// ErrDispatcherStopped), never a double delivery.
//
// Uses non-blocking Write so the pump is never frozen, then races Stop()
// and cancelA() each in their own goroutine after a start-barrier.
func TestE1cStopVsCancelExactlyOneTerminalError(t *testing.T) {
	rec := &e1cIDRecorder{}
	restoreGen := e1cSetIDGeneratorRestoringDefault(t, rec)
	defer restoreGen()

	d, state, _, network, endpoint := e1aNewDispatcher(t, 10)
	endpoint.dispatcher = d // wire SendRequestCtx to the dispatcher under test (e1a helper leaves it nil)

	// Non-blocking Write so the pump is never frozen.
	writtenC := make(chan string, 8)
	network.setOnWrite(func(data []byte) error {
		writtenC <- string(data)
		return nil
	})

	var cancelCount int32
	var termErr *ocpp.Error
	var termMu sync.Mutex
	d.SetOnRequestCanceled(func(requestID string, request ocpp.Request, err *ocpp.Error) {
		termMu.Lock()
		termErr = err
		termMu.Unlock()
		atomic.AddInt32(&cancelCount, 1)
	})

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop() // safety net for an early t.Fatal; Stop is idempotent (guards on running)

	ctxA, cancelA := context.WithCancel(context.Background())
	reqA := &e1aMockRequest{MockValue: "stop-vs-cancel"}

	err := endpoint.SendRequestCtx(ctxA, reqA)
	require.NoError(t, err)
	idA := rec.nth(0)

	// Wait for A to be dispatched (written).
	select {
	case <-writtenC:
	case <-time.After(e1cBound):
		t.Fatal("A was not dispatched")
	}

	// Verify A is pending.
	_, ok := state.GetPendingRequest(idA)
	require.True(t, ok, "A must be pending after dispatch")

	// Start-barrier: release both Stop and ctx-cancel simultaneously.
	startBarrier := make(chan struct{})

	var stopDone sync.WaitGroup
	stopDone.Add(1)
	go func() {
		defer stopDone.Done()
		<-startBarrier
		d.Stop()
	}()

	var cancelDone sync.WaitGroup
	cancelDone.Add(1)
	go func() {
		defer cancelDone.Done()
		<-startBarrier
		cancelA()
	}()

	close(startBarrier)
	stopDone.Wait()
	cancelDone.Wait()

	// Exactly ONE terminal callback must have fired.
	count := atomic.LoadInt32(&cancelCount)
	assert.Equal(t, int32(1), count,
		"exactly one onRequestCanceled must fire (Stop vs cancel race), got %d", count)

	// And that one error must be a real terminal sentinel (either the ctx-cancel
	// or the dispatcher-stopped one) — never a bare/empty error. A ctx-ignoring
	// passthrough would still deliver ErrDispatcherStopped here, so this does not
	// prove the ctx arm on its own (tests 2/3b do), but it does pin the taxonomy.
	termMu.Lock()
	gotErr := termErr
	termMu.Unlock()
	require.NotNil(t, gotErr, "a terminal error must have been delivered")
	assert.True(t,
		errors.Is(gotErr, ErrRequestCanceled) || errors.Is(gotErr, ErrDispatcherStopped),
		"terminal error must match ErrRequestCanceled or ErrDispatcherStopped, got %v", gotErr)
}

// ============================================================================
// MINOR-3 (sonnet review) — off-pump completion, THEN stale ctx cancel
// ============================================================================

// TestE1cOffPumpCompletionThenStaleCtxCancel covers the direction test 2 does
// NOT: a request completed OFF-pump (a response, via CompleteRequest) whose ctx
// is THEN canceled must not deliver a spurious cancel. The pump-local pending
// token goes stale while its ctx.Done() stays armed for a future select
// iteration; the pendingDone arm's GetPendingRequest guard and the top-of-loop
// reconciliation must both suppress it.
func TestE1cOffPumpCompletionThenStaleCtxCancel(t *testing.T) {
	rec := &e1cIDRecorder{}
	restoreGen := e1cSetIDGeneratorRestoringDefault(t, rec)
	defer restoreGen()

	d, state, _, network, endpoint := e1aNewDispatcher(t, 10)
	endpoint.dispatcher = d // wire SendRequestCtx to the dispatcher under test (e1a helper leaves it nil)

	writtenC := make(chan string, 8)
	network.setOnWrite(func(data []byte) error { writtenC <- string(data); return nil })

	canceled := make(chan string, 8)
	d.SetOnRequestCanceled(func(requestID string, request ocpp.Request, err *ocpp.Error) {
		canceled <- requestID
	})

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()

	ctxA, cancelA := context.WithCancel(context.Background())
	require.NoError(t, endpoint.SendRequestCtx(ctxA, &e1aMockRequest{MockValue: "offpump-a"}))
	select {
	case <-writtenC:
	case <-time.After(e1cBound):
		t.Fatal("A was not dispatched")
	}
	idA := rec.nth(0)

	// Off-pump completion: a response arriving via the read goroutine calls
	// CompleteRequest, which pops A + clears pending + coalesced-signals readiness.
	require.True(t, d.CompleteRequest(idA), "off-pump CompleteRequest(A) must win while A is in-flight")
	_, pending := state.GetPendingRequest(idA)
	require.False(t, pending, "A must no longer be pending after off-pump completion")

	// NOW cancel A's (stale) ctx — the in-flight arm must NOT fire a cancel.
	cancelA()
	select {
	case rid := <-canceled:
		t.Fatalf("stale ctx cancel delivered for already-completed request %s", rid)
	case <-time.After(e1cSilenceBound):
		// good — no spurious cancel
	}
}

// ============================================================================
// MINOR-4 (sonnet review) — N>1 cascading pre-write drops
// ============================================================================

// TestE1cCascadingPreWriteDrops exercises the spec's "N expired fronts drop in
// N iterations, one cancel each" claim at N>1 (tests 1/3 only cover N=1):
// enqueue 3 already-canceled-ctx requests + 1 live behind them while paused,
// Resume, and assert all 3 drop (3 cancels, 0 writes) then the live one dispatches.
func TestE1cCascadingPreWriteDrops(t *testing.T) {
	rec := &e1cIDRecorder{}
	restoreGen := e1cSetIDGeneratorRestoringDefault(t, rec)
	defer restoreGen()

	d, state, q, network, endpoint := e1aNewCountingDispatcher(t, 10)
	endpoint.dispatcher = d // wire SendRequestCtx to the dispatcher under test (e1a helper leaves it nil)

	network.setOnWrite(func(data []byte) error { return nil })

	canceled := make(chan string, 8)
	d.SetOnRequestCanceled(func(requestID string, request ocpp.Request, err *ocpp.Error) {
		canceled <- requestID
	})

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()

	// Pause so everything enqueues behind the pump without dispatching.
	d.Pause()
	require.True(t, d.IsPaused())

	const nDrop = 3
	droppedIDs := make(map[string]bool, nDrop)
	for i := 0; i < nDrop; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		require.NoError(t, endpoint.SendRequestCtx(ctx, &e1aMockRequest{MockValue: fmt.Sprintf("drop-%d", i)}))
		droppedIDs[rec.nth(i)] = false
	}
	require.NoError(t, endpoint.SendRequestCtx(context.Background(), &e1aMockRequest{MockValue: "live"}))
	liveID := rec.nth(nDrop)
	require.Equal(t, nDrop+1, q.Size(), "all requests must be queued while paused")

	// Resume: the pump drops the 3 expired fronts (one per iteration, each
	// CompleteRequest's coalesced readiness driving the next) then dispatches live.
	d.Resume()

	for i := 0; i < nDrop; i++ {
		select {
		case rid := <-canceled:
			_, expected := droppedIDs[rid]
			require.True(t, expected, "unexpected cancel for %s", rid)
			droppedIDs[rid] = true
		case <-time.After(e1cBound):
			t.Fatalf("timed out waiting for cancel #%d", i+1)
		}
	}
	for id, got := range droppedIDs {
		assert.True(t, got, "canceled request %s must have fired onRequestCanceled", id)
	}
	// No extra cancels — the live request must NOT be canceled.
	select {
	case rid := <-canceled:
		t.Fatalf("unexpected extra cancel: %s", rid)
	case <-time.After(e1cSilenceBound):
	}
	assert.Equal(t, 1, network.writeCount(), "only the live request must be written")
	_, ok := state.GetPendingRequest(liveID)
	assert.True(t, ok, "the live request must be pending after dispatch")

	d.CompleteRequest(liveID) // cleanup
}
