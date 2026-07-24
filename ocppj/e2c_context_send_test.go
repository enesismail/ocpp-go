package ocppj

// PR-E2c (tasks/e2-server-context-aware-send.md, "## C. E2c - the feature")
// RED-FIRST test suite - the SERVER mirror of the already-merged CLIENT-side
// E1c (ocppj/e1c_context_send_test.go). E2a (server completion ownership,
// tasks/e2-server-context-aware-send.md SS A) is ALREADY MERGED on master
// (PR #33) - this file rides that machinery (completeRequestOwned,
// CompleteRequest -> bool, the generation-pinned waitForTimeout(clientID,
// clientCtx, stoppedC, timerC) watcher with its B2 shutdown-safe send and B3
// pinned-channel-parameters shape).
//
// This file lives in `package ocppj` (not `ocppj_test`) so it can reach
// DefaultServerDispatcher's unexported pump internals exactly like
// e2a_completion_ownership_test.go and d2_token_identity_test.go. It reuses
// the e2a fakes (e2aServer, e2aChannel, e2aMockRequest/e2aMockFeature,
// e2aNewDispatcher, e2aNewBundle, e2aBlockingPeekQueue) defined in
// e2a_completion_ownership_test.go, same package.
//
// RED-FIRST discipline: every test below references the PR-E2c surface
// exactly as the spec (SS C) names it. Against today's codebase:
//   - Server.SendRequestCtx does not exist (C1)
//   - DefaultServerDispatcher.cancelC does not exist (C2/B4)
//   - serverCancelToken does not exist (C2)
//   - waitForTimeout's signature does not carry a requestID/userCtx/cancelC
//     - it only watches the internal timeout clientCtx, never a caller ctx
//     (B1/B2/B3 generalization for the cancel arm)
//   - dispatchNextRequest never inspects bundleCtx(bundle).Err() at all (C3
//     pre-write drop is entirely new for the server; unlike the client side,
//     nothing here exists yet)
//   - the dispatch-block re-entry loop (today only driven by
//     dispatchCompletedNoWrite, the MAJOR-2 write-error case) never loops for
//     a C3 pre-write drop
//
// This whole file is EXPECTED to fail compilation because of the d.cancelC /
// serverCancelToken / generalized waitForTimeout / Server.SendRequestCtx
// references below - that IS the intended red state pinning the PR-E2c
// contract. Several tests are ALSO independently runtime-red (they compile
// against a HYPOTHETICAL fixed signature but exercise behavior - the pre-
// write drop, the cancel-delivery arm - that produces no observable effect
// today), each documented at its own definition.
//
// NAMING ASSUMPTIONS (flagged once here, matching the convention of e2a's
// A6.6 and d2_token_identity_test.go's file-level comments about their own
// literal shapes - if the real implementation picks different names/shapes,
// only the call sites below need a matching update):
//   - type serverCancelToken struct { clientID, requestID string; ctx context.Context }
//     buffered on a new field `d.cancelC chan serverCancelToken` (cap 10, B4).
//   - the generalized watcher keeps the name `waitForTimeout` but grows two
//     new parameters and one new return-independent arm:
//     waitForTimeout(clientID, requestID string, clientCtx clientTimeoutContext,
//         userCtx context.Context, stoppedC chan struct{},
//         timerC chan serverTimeoutToken, cancelC chan serverCancelToken)
//     - userCtx is bundleCtx(bundle) (the caller's ctx, distinct from the
//     internal timeout-tracking clientCtx.ctx); requestID is needed because,
//     unlike the timeout arm (which is intrinsically racing exactly one
//     dispatched-request-per-client and is content with a bare clientID +
//     ctx-identity check), the cancel token needs the pump's
//     dispatchedRequestIDMap[clientID] == tok.requestID identity guard (C2
//     step 1) to detect staleness.
//   - Server.SendRequestCtx(ctx context.Context, clientID string, request
//     ocpp.Request) (string, error) - C1, ctx-first, nil ctx == Background.
//
// Spec tests implemented: C7.1, .2 (+.15 flush), .3, .4, .5, .6, .7, .8, .9,
// .10, .11, .12, .14. (.13 is folded into E2-0's own suite per the spec; not
// reproduced here.)

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/enesismail/ocpp-go/ocpp"
)

// e2cBundleWithCtx builds a fresh RequestBundle/id pair via e2aNewBundle
// (same package, defined in e2a_completion_ownership_test.go) and sets its
// Ctx field - RequestBundle.Ctx already exists (E1c, shared client/server
// struct), so this alone is not part of the not-yet-existing E2c surface.
func e2cBundleWithCtx(t *testing.T, endpoint *Server, value string, ctx context.Context) (RequestBundle, string) {
	t.Helper()
	b, id := e2aNewBundle(t, endpoint, value)
	b.Ctx = ctx
	return b, id
}

const e2cBound = 2 * time.Second               // generous bound for events that MUST happen
const e2cSilenceBound = 500 * time.Millisecond // window to prove an event does NOT happen

// e2cCountGoroutinesByStack / e2cWaitForGoroutineCount let a test track ONE
// specific goroutine kind (here, the watchdog watcher, by its function name
// appearing in the stack dump) without being flaky under `go test -race
// -count=N`, where unrelated goroutines elsewhere in the process start and
// exit independently of the code under test. Mirrors
// ocpp2.0.1_test/stop_lifecycle_test.go's countGoroutinesByStack /
// waitForGoroutineCount (not importable here - different module path/
// package - so a package-local copy).
func e2cCountGoroutinesByStack(substr string) int {
	size := 64 * 1024
	for {
		buf := make([]byte, size)
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), substr)
		}
		size *= 2
		if size > 64*1024*1024 {
			return strings.Count(string(buf[:n]), substr)
		}
	}
}

func e2cWaitForGoroutineCount(t *testing.T, substr string, want int, bound time.Duration) {
	t.Helper()
	deadline := time.After(bound)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if got := e2cCountGoroutinesByStack(substr); got == want {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatalf("timed out waiting for goroutine count matching %q to reach %d (currently %d)",
				substr, want, e2cCountGoroutinesByStack(substr))
		}
	}
}

// e2cWaitForGoroutineCountAtMost polls until the goroutine count matching
// substr is AT MOST want, rather than exactly want. Used for the "no leak"
// (return-to-baseline) assertions: runtime.Stack counts process-wide, so
// under `go test -race -count=N` an unrelated, already-in-flight stale
// watcher from a PRIOR iteration/test can still be draining on its own
// schedule and briefly push the count below the captured baseline - an exact
// `== want` wait would then never match and time out (false-red) on a
// correct implementation. Polling for `<= want` keeps the invariant this
// test actually cares about (no NET leak caused by THIS test) while
// tolerating that unrelated churn.
func e2cWaitForGoroutineCountAtMost(t *testing.T, substr string, want int, bound time.Duration) {
	t.Helper()
	deadline := time.After(bound)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if got := e2cCountGoroutinesByStack(substr); got <= want {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatalf("timed out waiting for goroutine count matching %q to reach at most %d (currently %d)",
				substr, want, e2cCountGoroutinesByStack(substr))
		}
	}
}

// ============================================================================
// C7.1 - Cancel a DISPATCHED request => callback fires once with an error
// matching both ErrRequestCanceled and context.Canceled; no response handler
// runs.
// ============================================================================

// TestE2cCancelDispatchedRequestExactlyOnce is RUNTIME-red against today's
// pump: nothing observes a dispatched request's bundle ctx at all, so
// cancelA() below has zero effect and onRequestCanceled never fires - the
// bounded wait times out.
func TestE2cCancelDispatchedRequestExactlyOnce(t *testing.T) {
	d, state, _, network, endpoint := e2aNewDispatcher(t)
	clientID := "e2c-t1-client"

	written := make(chan string, 8)
	network.setOnWrite(func(cID string, data []byte) error {
		written <- cID
		return nil
	})

	canceled := make(chan string, 8)
	var cancelErrs []*ocpp.Error
	var mu sync.Mutex
	d.SetOnRequestCanceled(func(cID, rID string, req ocpp.Request, err *ocpp.Error) {
		mu.Lock()
		cancelErrs = append(cancelErrs, err)
		mu.Unlock()
		canceled <- rID
	})

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()
	d.CreateClient(clientID)

	ctxA, cancelA := context.WithCancel(context.Background())
	bundleA, idA := e2cBundleWithCtx(t, endpoint, "dispatched-a", ctxA)
	require.NoError(t, d.SendRequest(clientID, bundleA))

	select {
	case <-written:
	case <-time.After(e2cBound):
		t.Fatal("A was not dispatched")
	}
	require.True(t, state.HasPendingRequest(clientID))

	cancelA()

	select {
	case rid := <-canceled:
		assert.Equal(t, idA, rid, "canceled request must be A")
	case <-time.After(e2cBound):
		t.Fatal("PR-E2c REGRESSION: onRequestCanceled never fired for a canceled dispatched request - the cancelC pump arm does not exist yet")
	}
	select {
	case rid := <-canceled:
		t.Fatalf("onRequestCanceled fired more than once: second ID=%s", rid)
	case <-time.After(e2cSilenceBound):
	}

	mu.Lock()
	require.Len(t, cancelErrs, 1, "exactly one cancel error")
	cancelErr := cancelErrs[0]
	mu.Unlock()
	assert.True(t, errors.Is(cancelErr, ErrRequestCanceled), "cancel error must match ErrRequestCanceled")
	assert.True(t, errors.Is(cancelErr, context.Canceled), "cancel error must match context.Canceled")

	// "No response handler runs": a late off-pump completion attempt for the
	// same id must lose (the cancel already owns the completion).
	assert.False(t, d.CompleteRequest(clientID, idA), "a late completion for A must lose - the cancel already won ownership")
	assert.False(t, state.HasPendingRequest(clientID))
}

// ============================================================================
// C7.2 + C7.15 - Cancel a QUEUED (behind an in-flight) request => never
// written to the wire; the front is flushed to completion FIRST so "never
// written" is a decisive assertion, not a vacuous one (C7.15).
// ============================================================================

// TestE2cCancelQueuedRequestNeverWrittenFlushFront is RUNTIME-red against
// today's pump: dispatchNextRequest never checks bundleCtx(bundle).Err(), so
// once A completes and the pump reaches B, B is written regardless of its
// canceled ctx (the C3 pre-write drop does not exist).
func TestE2cCancelQueuedRequestNeverWrittenFlushFront(t *testing.T) {
	d, state, queueMap, network, endpoint := e2aNewDispatcher(t)
	clientID := "e2c-t2-client"

	written := make(chan string, 8)
	network.setOnWrite(func(cID string, data []byte) error {
		written <- cID
		return nil
	})

	canceled := make(chan string, 8)
	d.SetOnRequestCanceled(func(cID, rID string, req ocpp.Request, err *ocpp.Error) {
		canceled <- rID
	})

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()
	d.CreateClient(clientID)

	// Front A: no cancel; default (positive) timeout keeps it in-flight and
	// blocks B from being dispatched.
	bundleA, idA := e2cBundleWithCtx(t, endpoint, "queued-a", context.Background())
	require.NoError(t, d.SendRequest(clientID, bundleA))
	select {
	case <-written:
	case <-time.After(e2cBound):
		t.Fatal("A was not dispatched")
	}

	// Queue B behind A, with a cancelable ctx.
	ctxB, cancelB := context.WithCancel(context.Background())
	bundleB, idB := e2cBundleWithCtx(t, endpoint, "queued-b", ctxB)
	require.NoError(t, d.SendRequest(clientID, bundleB))

	qc, ok := queueMap.Get(clientID)
	require.True(t, ok)
	require.Equal(t, 2, qc.Size(), "A (pending) + B (queued) must both be in the queue")

	// Cancel B while still queued (not yet decisive - B is behind an
	// in-flight front regardless of whether the cancel mechanism exists).
	cancelB()
	select {
	case cID := <-written:
		t.Fatalf("unexpected write for %s while A is still in-flight", cID)
	case <-time.After(e2cSilenceBound):
	}

	// Flush (C7.15): complete A (a genuine CALL_RESULT), letting the pump
	// advance to B. THIS is what makes "B never written" decisive - without
	// it, B trivially never gets written regardless of whether C3 exists at
	// all, since it's still sitting behind an in-flight front.
	require.True(t, d.CompleteRequest(clientID, idA))

	select {
	case rid := <-canceled:
		assert.Equal(t, idB, rid, "B's cancellation must be delivered")
	case cID := <-written:
		t.Fatalf("PR-E2c REGRESSION: canceled queued request %s was written to the wire for client %s - the C3 pre-write drop does not exist yet", idB, cID)
	case <-time.After(e2cBound):
		t.Fatal("neither B's cancellation nor a write was observed after flushing A")
	}
	select {
	case cID := <-written:
		t.Fatalf("unexpected extra write for %s after B was already canceled/dropped", cID)
	case <-time.After(e2cSilenceBound):
	}

	assert.False(t, state.HasPendingRequest(clientID))
	assert.True(t, qc.IsEmpty())
}

// ============================================================================
// C7.3 - Cancel racing a genuine CALL_RESULT => EXACTLY ONE of
// {callback-with-response, callback-with-cancel-error}; looped under -race.
// (The E2a-dependent guarantee: completeRequestOwned's atomic PopIf is the
// single-winner basis for this.)
// ============================================================================

// TestE2cCancelRaceGenuineCallResultExactlyOnce forces the interleaving
// deterministically via e2aBlockingPeekQueue (defined in
// e2a_completion_ownership_test.go, same package): the cancel path (via the
// watcher -> cancelC -> pump's completeRequestOwned) and an off-pump
// CompleteRequest (simulating a genuine CALL_RESULT) both race to PopIf the
// SAME request; only the chronologically-first is allowed to block (post-pop)
// so the second's predicate is provably evaluated against the already-popped
// queue, not just favored by luck. Looped so the whole deterministic
// construction is exercised repeatedly under `go test -race -count=N` per the
// spec's gate (SS D: "-count=10 on ... C7.3").
//
// On today's code this test is a FALSE GREEN in isolation (cancelA() has no
// observable effect at all, so only the off-pump CompleteRequest goroutine
// ever reaches blockingQueue.entered, and total is trivially 1) - it does not
// independently prove anything pre-fix. It is included as the exactly-once
// regression pin for AFTER the cancelC mechanism exists (this whole file
// fails to compile until then, via the d.cancelC reference below and
// elsewhere in this file, which is the actual RED signal). The final
// assertion on cap(d.cancelC) ties this test directly to the not-yet-existing
// field (B4).
func TestE2cCancelRaceGenuineCallResultExactlyOnce(t *testing.T) {
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

			clientID := fmt.Sprintf("e2c-t3-client-%d", i)
			blockingQueue := newE2aBlockingPeekQueue(NewFIFOClientQueue(10))
			queueMap.Add(clientID, blockingQueue)

			// M1 (findings/e2c-tests-deepseek-review.md): this must be a plain
			// `defer`, registered AFTER `defer d.Stop()` below - NOT a
			// t.Cleanup. t.Cleanup callbacks only run once the whole test
			// function has returned, i.e. AFTER every defer, including
			// `defer d.Stop()`. If a fatal fires while the pump is frozen
			// inside blockingQueue (post-pop, awaiting release), `defer
			// d.Stop()` would block forever waiting for the pump to notice
			// shutdown - and the Cleanup that would unblock it (by closing
			// blockingQueue.release) would never even run: a double-hang.
			// Deferring it here instead, AFTER `defer d.Stop()` is
			// registered, means Go's LIFO defer order runs THIS release
			// first (unblocking the pump) and only then d.Stop() (which can
			// now actually complete). The sync.Once guard still lets this
			// deferred release and the normal-path releaseBlockingQueue()
			// call near the end of the test coexist without a double-close
			// panic.
			var releaseOnce sync.Once
			releaseBlockingQueue := func() { releaseOnce.Do(func() { close(blockingQueue.release) }) }

			written := make(chan string, 8)
			network.setOnWrite(func(cID string, data []byte) error {
				written <- cID
				return nil
			})

			var responseWon, cancelWon int32
			d.SetOnRequestCanceled(func(cID, rID string, req ocpp.Request, err *ocpp.Error) {
				atomic.AddInt32(&cancelWon, 1)
			})

			d.Start()
			require.True(t, d.IsRunning())
			defer d.Stop()
			defer releaseBlockingQueue()

			ctxA, cancelA := context.WithCancel(context.Background())
			bundleA, idA := e2cBundleWithCtx(t, endpoint, "race-a", ctxA)
			require.NoError(t, d.SendRequest(clientID, bundleA))

			select {
			case <-written:
			case <-time.After(e2cBound):
				t.Fatal("A was not dispatched")
			}
			require.True(t, state.HasPendingRequest(clientID))

			// Arm only now: A's own dispatch Peek already happened above.
			blockingQueue.arm()

			startBarrier := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-startBarrier
				cancelA()
			}()
			go func() {
				defer wg.Done()
				<-startBarrier
				if d.CompleteRequest(clientID, idA) {
					atomic.AddInt32(&responseWon, 1)
				}
			}()
			close(startBarrier)

			select {
			case <-blockingQueue.entered:
			case <-time.After(e2cBound):
				t.Fatal("timed out waiting for the race's first completion to reach (and block in) the queue")
			}
			// Bounded window for the OTHER side to also genuinely attempt its
			// own completion while the first is frozen post-pop (mirrors
			// e2a's A6.2 margin rationale).
			time.Sleep(50 * time.Millisecond)
			releaseBlockingQueue()

			// C7.3 BLOCKER (findings/e2c-tests-deepseek-review.md): wg only
			// covers cancelA() RETURNING and the losing goroutine's
			// CompleteRequest call - it does NOT cover the cancel-wins path's
			// actual delivery, which happens asynchronously on the pump
			// (watcher -> cancelC -> fireRequestCancel -> the
			// SetOnRequestCanceled callback that increments cancelWon) well
			// after cancelA() itself has returned. A bare wg.Wait() followed
			// by an immediate atomic.Load can therefore observe cancelWon
			// still at 0 - total==0 - and false-red a correct implementation
			// purely on a scheduling race, not a real bug. require.Eventually
			// polls until the async delivery has actually landed instead of
			// assuming wg.Wait() already implies it.
			wg.Wait()

			require.Eventually(t, func() bool {
				total := atomic.LoadInt32(&responseWon) + atomic.LoadInt32(&cancelWon)
				return total == 1
			}, e2cBound, 5*time.Millisecond,
				"exactly one of {response-completion, cancel} must win the race (got responseWon=%d cancelWon=%d)",
				atomic.LoadInt32(&responseWon), atomic.LoadInt32(&cancelWon))

			// Bounded negative check: once settled at exactly one winner, it
			// must never move away from that (no double-delivery of either
			// side) within a silence window.
			require.Never(t, func() bool {
				total := atomic.LoadInt32(&responseWon) + atomic.LoadInt32(&cancelWon)
				return total != 1
			}, e2cSilenceBound, 5*time.Millisecond,
				"total delivery count moved away from exactly 1 after settling - double-delivery (got responseWon=%d cancelWon=%d)",
				atomic.LoadInt32(&responseWon), atomic.LoadInt32(&cancelWon))

			assert.Equal(t, 10, cap(d.cancelC), "cancelC must be buffered with capacity 10, matching timerC (B4)")
		})
	}
}

// ============================================================================
// C7.4 - Stale cancel token (ctx fires after the request already completed)
// => no-op; the NEXT request is unaffected. (Identity-guard regression.)
// ============================================================================

// TestE2cStaleCancelTokenNoopNextRequestUnaffected is the server-side mirror
// of e1c's MINOR-3 (TestE1cOffPumpCompletionThenStaleCtxCancel), extended
// with the C7.4-specific "next request unaffected" check.
func TestE2cStaleCancelTokenNoopNextRequestUnaffected(t *testing.T) {
	d, state, _, network, endpoint := e2aNewDispatcher(t)
	clientID := "e2c-t4-client"

	written := make(chan string, 8)
	network.setOnWrite(func(cID string, data []byte) error {
		written <- cID
		return nil
	})
	canceled := make(chan string, 8)
	d.SetOnRequestCanceled(func(cID, rID string, req ocpp.Request, err *ocpp.Error) {
		canceled <- rID
	})

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()
	d.CreateClient(clientID)

	ctxA, cancelA := context.WithCancel(context.Background())
	bundleA, idA := e2cBundleWithCtx(t, endpoint, "stale-a", ctxA)
	require.NoError(t, d.SendRequest(clientID, bundleA))
	select {
	case <-written:
	case <-time.After(e2cBound):
		t.Fatal("A was not dispatched")
	}

	// Off-pump completion FIRST (a genuine response), before A's ctx is ever
	// canceled.
	require.True(t, d.CompleteRequest(clientID, idA), "off-pump CompleteRequest(A) must win while A is in-flight")
	_, pending := state.GetClientState(clientID).GetPendingRequest(idA)
	require.False(t, pending)

	// NOW cancel A's (stale) ctx. The identity guard
	// (dispatchedRequestIDMap[clientID] == tok.requestID) must suppress
	// this - A is no longer the dispatched request for clientID.
	cancelA()
	select {
	case rid := <-canceled:
		t.Fatalf("stale ctx cancel delivered for already-completed request %s", rid)
	case <-time.After(e2cSilenceBound):
	}

	// The NEXT request for the SAME client must be entirely unaffected.
	bundleB, idB := e2cBundleWithCtx(t, endpoint, "stale-b", context.Background())
	require.NoError(t, d.SendRequest(clientID, bundleB))
	select {
	case cID := <-written:
		assert.Equal(t, clientID, cID)
	case <-time.After(e2cBound):
		t.Fatal("next request B was never dispatched after the stale cancel - possible identity-guard regression wedged the client")
	}
	require.True(t, state.HasPendingRequest(clientID))
	assert.True(t, d.CompleteRequest(clientID, idB))
}

// ============================================================================
// C7.5 - Already-canceled ctx passed at send time => never written; cancel
// delivered. (C3 pre-write drop.)
// ============================================================================

// TestE2cAlreadyCanceledCtxNeverWrittenPreWriteDrop is RUNTIME-red: today's
// dispatchNextRequest never inspects bundleCtx(bundle).Err(), so an
// already-canceled bundle is written exactly like any other.
func TestE2cAlreadyCanceledCtxNeverWrittenPreWriteDrop(t *testing.T) {
	d, state, _, network, endpoint := e2aNewDispatcher(t)
	clientID := "e2c-t5-client"

	written := make(chan string, 8)
	network.setOnWrite(func(cID string, data []byte) error {
		written <- cID
		return nil
	})
	canceled := make(chan string, 8)
	var cancelErrs []*ocpp.Error
	var mu sync.Mutex
	d.SetOnRequestCanceled(func(cID, rID string, req ocpp.Request, err *ocpp.Error) {
		mu.Lock()
		cancelErrs = append(cancelErrs, err)
		mu.Unlock()
		canceled <- rID
	})

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()
	d.CreateClient(clientID)

	ctxA, cancelA := context.WithCancel(context.Background())
	cancelA() // already canceled BEFORE it is ever sent
	bundleA, idA := e2cBundleWithCtx(t, endpoint, "already-canceled", ctxA)
	require.NoError(t, d.SendRequest(clientID, bundleA))

	select {
	case rid := <-canceled:
		assert.Equal(t, idA, rid)
	case cID := <-written:
		t.Fatalf("PR-E2c REGRESSION: an already-canceled ctx was written to the wire for client %s - the C3 pre-write drop does not exist yet", cID)
	case <-time.After(e2cBound):
		t.Fatal("timed out waiting for the pre-write drop to fire onRequestCanceled")
	}
	select {
	case cID := <-written:
		t.Fatalf("unexpected write for %s", cID)
	case <-time.After(e2cSilenceBound):
	}

	mu.Lock()
	require.Len(t, cancelErrs, 1)
	cancelErr := cancelErrs[0]
	mu.Unlock()
	assert.True(t, errors.Is(cancelErr, ErrRequestCanceled))
	assert.True(t, errors.Is(cancelErr, context.Canceled))
	assert.False(t, state.HasPendingRequest(clientID))
}

// ============================================================================
// C7.6 - SetTimeout(0) + cancelable ctx => watcher still spawned and cancel
// still works. (B1.)
// ============================================================================

// TestE2cSetTimeoutZeroWatcherSpawnedCancelWorks is RUNTIME-red: today the
// watcher only spawns `if clientCtx.isActive()` (dispatcher.go ~:950), which
// with SetTimeout(0) is never true regardless of the bundle's ctx - so no
// watcher goroutine is ever spawned, and cancelA() below has no effect.
func TestE2cSetTimeoutZeroWatcherSpawnedCancelWorks(t *testing.T) {
	d, state, _, network, endpoint := e2aNewDispatcher(t)
	d.SetTimeout(0)
	clientID := "e2c-t6-client"

	written := make(chan string, 8)
	network.setOnWrite(func(cID string, data []byte) error {
		written <- cID
		return nil
	})
	canceled := make(chan string, 8)
	d.SetOnRequestCanceled(func(cID, rID string, req ocpp.Request, err *ocpp.Error) {
		canceled <- rID
	})

	baseline := e2cCountGoroutinesByStack("waitForTimeout")

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()
	d.CreateClient(clientID)

	ctxA, cancelA := context.WithCancel(context.Background())
	bundleA, idA := e2cBundleWithCtx(t, endpoint, "t0-a", ctxA)
	require.NoError(t, d.SendRequest(clientID, bundleA))
	select {
	case <-written:
	case <-time.After(e2cBound):
		t.Fatal("A was not dispatched")
	}
	require.True(t, state.HasPendingRequest(clientID))

	// B1: the watcher must be spawned even though SetTimeout(0) never
	// activates the internal timeout-tracking clientCtx by itself, because
	// the bundle's ctx is cancelable (Done() != nil).
	e2cWaitForGoroutineCount(t, "waitForTimeout", baseline+1, e2cBound)

	cancelA()
	select {
	case rid := <-canceled:
		assert.Equal(t, idA, rid)
	case <-time.After(e2cBound):
		t.Fatal("PR-E2c REGRESSION (B1): SetTimeout(0) with a cancelable ctx never fired a cancel - no watcher was spawned")
	}
}

// ============================================================================
// C7.7 - SendRequestCtx(nil, ...) behaves as Background; SendRequest / typed
// helpers unchanged. (Dispatcher-level pin of C1; the facade-level mirror
// lives in ocpp1.6_test / ocpp2.0.1_test.)
// ============================================================================

// TestE2cSendRequestCtxNilBehavesAsBackground references the not-yet-existing
// Server.SendRequestCtx directly - this is one of the file's primary
// compile-red anchors for C1.
func TestE2cSendRequestCtxNilBehavesAsBackground(t *testing.T) {
	d, state, _, network, endpoint := e2aNewDispatcher(t)
	endpoint.dispatcher = d // wire the endpoint to the dispatcher under test (e2a helper leaves it nil)
	clientID := "e2c-t7-client"

	written := make(chan string, 8)
	network.setOnWrite(func(cID string, data []byte) error {
		written <- cID
		return nil
	})

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()
	d.CreateClient(clientID)

	// PR-E2c: Server.SendRequestCtx(ctx, clientID, request) (string, error) -
	// C1. nil ctx must behave as context.Background().
	idA, err := endpoint.SendRequestCtx(nil, clientID, &e2aMockRequest{MockValue: "nil-ctx"})
	require.NoError(t, err)
	require.NotEmpty(t, idA)

	select {
	case cID := <-written:
		assert.Equal(t, clientID, cID)
	case <-time.After(e2cBound):
		t.Fatal("nil-ctx SendRequestCtx was never dispatched")
	}
	require.True(t, state.HasPendingRequest(clientID))
	assert.True(t, d.CompleteRequest(clientID, idA))

	// SendRequest (ctx-less) must still work unchanged.
	idB, err := endpoint.SendRequest(clientID, &e2aMockRequest{MockValue: "regression"})
	require.NoError(t, err)
	select {
	case cID := <-written:
		assert.Equal(t, clientID, cID)
	case <-time.After(e2cBound):
		t.Fatal("SendRequest regressed")
	}
	assert.True(t, d.CompleteRequest(clientID, idB))
}

// ============================================================================
// C7.8 - Multi-client isolation: canceling client A's request does not
// disturb client B's in-flight request.
// ============================================================================

func TestE2cMultiClientIsolation(t *testing.T) {
	d, state, _, network, endpoint := e2aNewDispatcher(t)
	clientA := "e2c-t8-clientA"
	clientB := "e2c-t8-clientB"

	written := make(chan string, 8)
	network.setOnWrite(func(cID string, data []byte) error {
		written <- cID
		return nil
	})
	canceled := make(chan string, 8)
	d.SetOnRequestCanceled(func(cID, rID string, req ocpp.Request, err *ocpp.Error) {
		canceled <- rID
	})

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()
	d.CreateClient(clientA)
	d.CreateClient(clientB)

	ctxA, cancelA := context.WithCancel(context.Background())
	bundleA, idA := e2cBundleWithCtx(t, endpoint, "iso-a", ctxA)
	require.NoError(t, d.SendRequest(clientA, bundleA))

	bundleB, idB := e2cBundleWithCtx(t, endpoint, "iso-b", context.Background())
	require.NoError(t, d.SendRequest(clientB, bundleB))

	gotA, gotB := false, false
	for i := 0; i < 2; i++ {
		select {
		case cID := <-written:
			if cID == clientA {
				gotA = true
			}
			if cID == clientB {
				gotB = true
			}
		case <-time.After(e2cBound):
			t.Fatal("timed out waiting for both A and B to dispatch")
		}
	}
	require.True(t, gotA && gotB)
	require.True(t, state.HasPendingRequest(clientA))
	require.True(t, state.HasPendingRequest(clientB))

	cancelA()
	select {
	case rid := <-canceled:
		assert.Equal(t, idA, rid)
	case <-time.After(e2cBound):
		t.Fatal("PR-E2c REGRESSION: client A's cancel never fired")
	}
	select {
	case rid := <-canceled:
		t.Fatalf("unexpected extra cancel: %s", rid)
	case <-time.After(e2cSilenceBound):
	}

	// Client B must be completely undisturbed by A's cancel.
	require.True(t, state.HasPendingRequest(clientB))
	assert.True(t, d.CompleteRequest(clientB, idB))
	assert.False(t, state.HasPendingRequest(clientA))
}

// ============================================================================
// C7.9 - Cancel during Stop: dispatch with a user ctx, Stop(), THEN cancel =>
// the watcher exits via stoppedC, no send on a dead channel, no goroutine
// leak. (B2.)
// ============================================================================

func TestE2cCancelDuringStopNoDeadSendNoLeak(t *testing.T) {
	d, state, _, network, endpoint := e2aNewDispatcher(t)
	clientID := "e2c-t9-client"

	written := make(chan string, 8)
	network.setOnWrite(func(cID string, data []byte) error {
		written <- cID
		return nil
	})

	baseline := e2cCountGoroutinesByStack("waitForTimeout")

	d.Start()
	require.True(t, d.IsRunning())
	d.CreateClient(clientID)

	ctxA, cancelA := context.WithCancel(context.Background())
	bundleA, _ := e2cBundleWithCtx(t, endpoint, "stop-a", ctxA)
	require.NoError(t, d.SendRequest(clientID, bundleA))
	select {
	case <-written:
	case <-time.After(e2cBound):
		t.Fatal("A was not dispatched")
	}
	require.True(t, state.HasPendingRequest(clientID))
	e2cWaitForGoroutineCount(t, "waitForTimeout", baseline+1, e2cBound)

	// Stop FIRST.
	d.Stop()
	require.False(t, d.IsRunning())

	// THEN cancel. On a correct implementation the watcher's own
	// shutdown-safe send (`select { case cancelC <- tok: case <-stoppedC: }`,
	// B2 generalized to the cancel arm) lets it exit via the already-closed
	// stoppedC instead of blocking forever on a cancelC nobody reads anymore
	// (the pump is gone). The decisive assertion is the goroutine count not
	// exceeding baseline below (no NET leak - see e2cWaitForGoroutineCountAtMost).
	cancelA()

	e2cWaitForGoroutineCountAtMost(t, "waitForTimeout", baseline, e2cBound)
}

// ============================================================================
// C7.10 - SetTimeout(0) + cancelable ctx => no nil-cancel panic in the cancel
// path (C2 step 3's defensive guard); pump remains responsive afterward.
// ============================================================================

// TestE2cSetTimeoutZeroCancelPathNoPanicPumpSurvives proves the pump did not
// panic/die by requiring it to still dispatch and complete a SECOND request
// normally after the cancel - messagePump has no per-iteration recover, so a
// panic in the cancel arm would silently kill the single shared pump
// goroutine and every subsequent SendRequest would hang forever.
func TestE2cSetTimeoutZeroCancelPathNoPanicPumpSurvives(t *testing.T) {
	d, state, _, network, endpoint := e2aNewDispatcher(t)
	d.SetTimeout(0)
	clientID := "e2c-t10-client"

	written := make(chan string, 8)
	network.setOnWrite(func(cID string, data []byte) error {
		written <- cID
		return nil
	})
	canceled := make(chan string, 8)
	d.SetOnRequestCanceled(func(cID, rID string, req ocpp.Request, err *ocpp.Error) {
		canceled <- rID
	})

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()
	d.CreateClient(clientID)

	ctxA, cancelA := context.WithCancel(context.Background())
	bundleA, idA := e2cBundleWithCtx(t, endpoint, "t10-a", ctxA)
	require.NoError(t, d.SendRequest(clientID, bundleA))
	select {
	case <-written:
	case <-time.After(e2cBound):
		t.Fatal("A was not dispatched")
	}

	cancelA()
	select {
	case rid := <-canceled:
		assert.Equal(t, idA, rid)
	case <-time.After(e2cBound):
		t.Fatal("PR-E2c REGRESSION: SetTimeout(0) cancel never fired")
	}

	bundleB, idB := e2cBundleWithCtx(t, endpoint, "t10-b", context.Background())
	require.NoError(t, d.SendRequest(clientID, bundleB))
	select {
	case cID := <-written:
		assert.Equal(t, clientID, cID)
	case <-time.After(e2cBound):
		t.Fatal("pump did not survive the SetTimeout(0) cancel path (possible nil-cancel panic) - B was never dispatched")
	}
	require.True(t, state.HasPendingRequest(clientID))
	assert.True(t, d.CompleteRequest(clientID, idB))
}

// ============================================================================
// C7.11 - Stop() -> Start() -> cancel a request from the OLD generation =>
// no cross-generation delivery, clean under -race. (B3.)
// ============================================================================

// TestE2cCrossGenerationCancelNoDelivery mirrors e2a's
// TestE2aB2B3StopStartOldGenerationWatcherNoCrossDelivery (A6.6) but races
// the userCtx cancel arm instead of the timeout arm. See the file-level
// NAMING ASSUMPTIONS comment for the waitForTimeout call shape used below.
func TestE2cCrossGenerationCancelNoDelivery(t *testing.T) {
	queueMap := NewFIFOQueueMap(10)
	d := NewDefaultServerDispatcher(queueMap)
	var mutex sync.RWMutex
	state := NewServerState(&mutex)
	d.SetPendingRequestState(state)
	network := &e2aServer{}
	d.SetNetworkServer(network)

	d.Start()
	require.True(t, d.IsRunning())

	gen1StoppedC := d.stoppedC
	gen1TimerC := d.timerC
	gen1CancelC := d.cancelC // PR-E2c: new field, does not exist yet

	clientID := "e2c-t11-client"
	requestID := "e2c-t11-req"

	innerCtx, innerCancel := context.WithCancel(context.Background())
	clientCtx := clientTimeoutContext{ctx: innerCtx, cancel: innerCancel}
	userCtx, userCancel := context.WithCancel(context.Background())
	defer userCancel()

	watcherDone := make(chan struct{})
	go func() {
		d.waitForTimeout(clientID, requestID, clientCtx, userCtx, gen1StoppedC, gen1TimerC, gen1CancelC)
		close(watcherDone)
	}()

	// Stop generation 1, then immediately start generation 2 - reassigning
	// d.stoppedC/d.timerC/d.cancelC to fresh instances. The watcher above
	// must remain pinned to the OLD (gen1) channels regardless of this swap.
	d.Stop()
	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()

	// Cancel the OLD generation's user ctx. It must NOT deliver anything on
	// the NEW generation's d.cancelC.
	userCancel()

	select {
	case tok := <-d.cancelC:
		t.Fatalf("cross-generation delivery: stale watcher posted %+v onto the NEW generation's cancelC", tok)
	case <-time.After(400 * time.Millisecond):
		// expected: nothing arrives on the new generation's cancelC.
	}

	select {
	case <-watcherDone:
	case <-time.After(e2cBound):
		t.Fatal("stale watcher goroutine never exited - leaked, parked forever on stale generation-pinned channels")
	}
}

// ============================================================================
// C7.12 - Cancel of an in-flight request arriving while the pump is
// mid-drain-loop for the SAME client => both resolve, exactly once each,
// correct order. (C3 x C2 interaction.)
// ============================================================================

// TestE2cCancelInFlightDuringSameClientDrainLoop pins C7.12. Deterministic
// construction (the "seam" the spec calls for): B's ctx is canceled BEFORE it
// is ever enqueued, removing any timing dependency - the instant A's
// cancellation lets the pump re-enter the dispatch block for this client, B
// is unconditionally already-expired and must be dropped by the SAME
// "loop-the-dispatch-step" mechanism dispatchNextRequest already uses for the
// MAJOR-2 write-error re-entry (C3 reuses that loop for pre-write drops), all
// within the same dispatch-block re-entry that follows A's cancel.
func TestE2cCancelInFlightDuringSameClientDrainLoop(t *testing.T) {
	d, state, _, network, endpoint := e2aNewDispatcher(t)
	clientID := "e2c-t12-client"

	written := make(chan string, 8)
	network.setOnWrite(func(cID string, data []byte) error {
		written <- cID
		return nil
	})

	var mu sync.Mutex
	var order []string
	canceled := make(chan struct{}, 8)
	d.SetOnRequestCanceled(func(cID, rID string, req ocpp.Request, err *ocpp.Error) {
		mu.Lock()
		order = append(order, rID)
		mu.Unlock()
		canceled <- struct{}{}
	})

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()
	d.CreateClient(clientID)

	ctxA, cancelA := context.WithCancel(context.Background())
	bundleA, idA := e2cBundleWithCtx(t, endpoint, "drain-a", ctxA)
	require.NoError(t, d.SendRequest(clientID, bundleA))
	select {
	case <-written:
	case <-time.After(e2cBound):
		t.Fatal("A was not dispatched")
	}

	ctxB, cancelB := context.WithCancel(context.Background())
	cancelB() // already expired BEFORE being enqueued - the deterministic seam
	bundleB, idB := e2cBundleWithCtx(t, endpoint, "drain-b", ctxB)
	require.NoError(t, d.SendRequest(clientID, bundleB))

	bundleC, idC := e2cBundleWithCtx(t, endpoint, "drain-c", context.Background())
	require.NoError(t, d.SendRequest(clientID, bundleC))

	// Cancel A: this resolves A's own in-flight cancel AND - in the SAME
	// dispatch-loop re-entry that follows - drops B's already-expired front
	// via C3, before C is finally dispatched.
	cancelA()

	for i := 0; i < 2; i++ {
		select {
		case <-canceled:
		case <-time.After(e2cBound):
			t.Fatalf("timed out waiting for cancel #%d of 2 (A then B)", i+1)
		}
	}

	mu.Lock()
	gotOrder := append([]string(nil), order...)
	mu.Unlock()
	require.Equal(t, []string{idA, idB}, gotOrder,
		"A's in-flight cancel must resolve before the drain-loop's drop of B - correct order, exactly once each")

	select {
	case <-canceled:
		t.Fatal("unexpected third cancel notification")
	case <-time.After(e2cSilenceBound):
	}

	select {
	case cID := <-written:
		assert.Equal(t, clientID, cID)
	case <-time.After(e2cBound):
		t.Fatal("C was never dispatched after A/B resolved")
	}
	require.True(t, state.HasPendingRequest(clientID))
	assert.True(t, d.CompleteRequest(clientID, idC))

	// m4 (findings/e2c-tests-deepseek-review.md): make "B never written"
	// EXPLICIT rather than relying only on the cancel-count silence window
	// above - `written` (used above) only ever carries the CLIENT id, not
	// the per-request id, so a spurious write of B would be indistinguishable
	// there from C's legitimate write (both come from the same clientID).
	// e2aServer.writesSnapshot() records the raw wire bytes for every write
	// (network.setOnWrite's callback runs AFTER the append in e2aServer.Write
	// - see e2a_completion_ownership_test.go), and each bundle's Data is its
	// MarshalJSON'd Call, which embeds the message's unique id - mirrors how
	// TestE2aSetTimeoutZeroWritesFrontExactlyOnce (e2a_completion_ownership_test.go)
	// decisively distinguishes re-sent ids the same way.
	for _, w := range network.writesSnapshot() {
		assert.NotContains(t, string(w.data), idB, "canceled request B must never be written to the wire")
	}
}

// ============================================================================
// C7.14 - No watcher leak on NORMAL completion under SetTimeout(0) +
// cancelable ctx: dispatch, respond, assert goroutine count returns to
// baseline. (B1; C7.9/10 only cover spawn-and-cancel, never
// spawn-and-complete.)
// ============================================================================

// TestE2cNoWatcherLeakOnNormalCompletion is RUNTIME-red today for a subtler
// reason than "no watcher spawns": even if a watcher spawn condition existed,
// B1's rationale is specifically that reaping on normal completion (the
// readyToken arm's clientCtx.cancel(), guarded by isActive()) requires
// clientCtx to have been constructed (not left zero) whenever the bundle ctx
// is cancelable, precisely so this path reaps it. Without that, a completed
// request under SetTimeout(0) leaks one goroutine per completion.
func TestE2cNoWatcherLeakOnNormalCompletion(t *testing.T) {
	d, state, _, network, endpoint := e2aNewDispatcher(t)
	d.SetTimeout(0)
	clientID := "e2c-t14-client"

	written := make(chan string, 8)
	network.setOnWrite(func(cID string, data []byte) error {
		written <- cID
		return nil
	})

	baseline := e2cCountGoroutinesByStack("waitForTimeout")

	d.Start()
	require.True(t, d.IsRunning())
	defer d.Stop()
	d.CreateClient(clientID)

	ctxA, cancel := context.WithCancel(context.Background())
	defer cancel() // never fired in this test - this is the NORMAL-completion path
	bundleA, idA := e2cBundleWithCtx(t, endpoint, "t14-a", ctxA)
	require.NoError(t, d.SendRequest(clientID, bundleA))
	select {
	case <-written:
	case <-time.After(e2cBound):
		t.Fatal("A was not dispatched")
	}
	require.True(t, state.HasPendingRequest(clientID))

	// B1: the watcher must have spawned (SetTimeout(0), cancelable ctx).
	e2cWaitForGoroutineCount(t, "waitForTimeout", baseline+1, e2cBound)

	// Complete NORMALLY (a genuine response) - the ctx is never canceled.
	require.True(t, d.CompleteRequest(clientID, idA))

	// The watcher must be reaped by the NORMAL completion path, not just by
	// an eventual cancel/timeout. Not-exceeding-baseline (no NET leak), not an
	// exact match - see e2cWaitForGoroutineCountAtMost.
	e2cWaitForGoroutineCountAtMost(t, "waitForTimeout", baseline, e2cBound)
}
