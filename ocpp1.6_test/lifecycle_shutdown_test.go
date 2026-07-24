package ocpp16_test

// PR-L2 (tasks/facade-lifecycle-hardening.md, "## PR-L2 — make every
// facade-channel producer shutdown-preemptible") RED-FIRST test suite for the
// OCPP 1.6 charge-point (client) facade.
//
// This file pins ONLY PR-L2's scope: every facade-channel producer
// (onRequestTimeout's cancel hook, the forwarding closures wired in
// NewChargePoint, and error()) must become preemptible against stopC, and
// Stop() must be reordered to close stopC before client.Stop(). It does NOT
// test PR-L1 (StopCtx, the generation handshake/join, errC close-after-join,
// the L3 residual guard) - that surface does not exist yet.
//
// RED-first discipline: every test below either hangs or panics against
// today's code. Since Stop() hangs forever / panics in the RED state, every
// blocking call is watchdog-bounded (l2BoundedStop) so a regression fails
// fast instead of wedging the test binary - see the package doc comment on
// l2BoundedStop.
//
// Reused helpers from ocpp1.6_test/inbound_ordering_test.go (same package):
// startStandaloneChargePoint, sequentialMessageIds, heartbeatCallResultJson,
// waitOrFail.

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/enesismail/ocpp-go/internal/testhooks"
	"github.com/enesismail/ocpp-go/ocpp"
	"github.com/enesismail/ocpp-go/ocpp1.6/core"
	"github.com/enesismail/ocpp-go/ocppj"
)

// l2Bound is the bounded deadline for every PR-L2 test in this file. Every
// blocking assertion races this timeout so a hang (today's RED state) fails
// the single test cleanly instead of wedging the whole `go test` run.
const l2Bound = 5 * time.Second

// stopper is the minimal interface l2BoundedStop needs.
// ocpp16.ChargePoint satisfies it.
type stopper interface{ Stop() }

// l2BoundedStop calls s.Stop() on its own goroutine and waits up to l2Bound
// for it to return. A panic inside Stop() is recovered and reported via
// t.Fatalf (a clean assertion failure), instead of crashing the test binary;
// a Stop() that never returns fails via t.Fatal instead of hanging the
// suite. Mirrors ocpp2.0.1_test/stop_lifecycle_test.go's boundedStop.
func l2BoundedStop(t *testing.T, s stopper) {
	t.Helper()
	done := make(chan interface{}, 1)
	go func() {
		defer func() { done <- recover() }()
		s.Stop()
	}()
	select {
	case r := <-done:
		if r != nil {
			t.Fatalf("Stop() panicked: %v", r)
		}
	case <-time.After(l2Bound):
		t.Fatal("Stop() did not return within the bounded deadline (deadlock)")
	}
}

// l2CountGoroutinesByStack returns the number of currently running goroutines
// whose stack trace contains substr. Unlike runtime.NumGoroutine() (a raw,
// process-wide count), this lets a test track one specific goroutine kind
// without being flaky from unrelated goroutines elsewhere in the suite.
// Mirrors ocpp2.0.1_test/stop_lifecycle_test.go's countGoroutinesByStack.
func l2CountGoroutinesByStack(substr string) int {
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

// l2WaitForGoroutineCountAtMost polls (bounded by l2Bound) until the number
// of goroutines whose stack trace contains substr is AT MOST want, failing
// via t.Fatal if the deadline elapses first. Used for "no leak" assertions:
// runtime.Stack counts process-wide, so an exact `== want` wait can flake on
// unrelated churn from other tests; `<= want` keeps the invariant this file
// actually cares about. Mirrors ocppj/e2c_context_send_test.go's
// e2cWaitForGoroutineCountAtMost.
func l2WaitForGoroutineCountAtMost(t *testing.T, substr string, want int) {
	t.Helper()
	deadline := time.After(l2Bound)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if got := l2CountGoroutinesByStack(substr); got <= want {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatalf("timed out waiting for goroutine count matching %q to reach at most %d (currently %d)",
				substr, want, l2CountGoroutinesByStack(substr))
		}
	}
}

// l2WaitForGoroutineCountAtLeast polls (bounded by l2Bound) until the number
// of goroutines whose stack trace contains substr is AT LEAST want, failing
// via t.Fatal if the deadline elapses first. The mirror image of
// l2WaitForGoroutineCountAtMost's "no leak" check: used to positively
// confirm a goroutine has reached a specific blocked state (e.g. parked
// inside a channel send) before the test proceeds to act on that state.
func l2WaitForGoroutineCountAtLeast(t *testing.T, substr string, want int) {
	t.Helper()
	deadline := time.After(l2Bound)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if got := l2CountGoroutinesByStack(substr); got >= want {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatalf("timed out waiting for goroutine count matching %q to reach at least %d (currently %d)",
				substr, want, l2CountGoroutinesByStack(substr))
		}
	}
}

// l2PinHandlerWithTwoOutstanding sends R0 (pinning the sole
// asyncCallbackHandler drainer on a callback that blocks on gateC) and then
// two more requests, R1 (dispatched) and R2 (queued behind it), that are
// never answered - they stay outstanding in the dispatcher until something
// cancels them. idPrefix must be unique per test (feeds
// sequentialMessageIds/heartbeatCallResultJson for R0's id). Returns once
// R0's callback is confirmed pinned.
func l2PinHandlerWithTwoOutstanding(suite *OcppV16TestSuite, writeC chan []byte, idPrefix string, gateC chan struct{}) {
	t := suite.T()
	pinnedC := make(chan struct{})
	err := suite.chargePoint.SendRequestAsync(core.NewHeartbeatRequest(), func(confirmation ocpp.Response, err error) {
		close(pinnedC)
		<-gateC
	})
	require.NoError(t, err)

	select {
	case <-writeC:
	case <-time.After(l2Bound):
		t.Fatal("timed out waiting for R0 to be written")
	}
	err = suite.mockWsClient.MessageHandler([]byte(heartbeatCallResultJson(idPrefix + "-0")))
	require.NoError(t, err)
	waitOrFail(suite, pinnedC, "timed out waiting for the async handler to be pinned on R0's callback")

	// R1: dispatched (becomes the in-flight request).
	err = suite.chargePoint.SendRequestAsync(core.NewHeartbeatRequest(), func(confirmation ocpp.Response, err error) {})
	require.NoError(t, err)
	select {
	case <-writeC:
	case <-time.After(l2Bound):
		t.Fatal("timed out waiting for R1 to be written")
	}

	// R2: queued behind R1 (the dispatcher only ever has one request
	// in-flight at a time). Never written, never answered.
	err = suite.chargePoint.SendRequestAsync(core.NewHeartbeatRequest(), func(confirmation ocpp.Response, err error) {})
	require.NoError(t, err)
}

// ============================================================================
// Test 1 - L2 cancel-hook deadlock (spec §L2, PR-L2 item 2's onRequestTimeout
// site).
// ============================================================================

// TestL2ShutdownCancelHookDeadlock pins the spec's confirmed end-to-end L2 mechanism:
// "a blocking/slow user callback wedges the pump ⇒ client.Stop() never
// returns ⇒ facade Stop() hangs forever" (spec §L2, line 41), specifically
// the "pump wedged in the stop-drain loop" variant (dispatcher.go:290-321).
//
// Recipe: R0's callback pins the sole asyncCallbackHandler drainer. R1
// (dispatched) and R2 (queued) are left outstanding. Calling Stop() closes
// the dispatcher's request channel; messagePump's drain-and-cancel loop
// (ocppj/dispatcher.go:293-321) then fires onRequestTimeout for EVERY
// outstanding request, SEQUENTIALLY, on the messagePump goroutine itself.
// onRequestTimeout's blocking send (charge_point.go:77, `cp.incoming <-
// ...`, cap 1) succeeds for the first outstanding request (buffer empty) but
// BLOCKS FOREVER for the second (buffer full, no reader - the sole drainer
// is pinned in R0's callback). messagePump therefore never reaches
// close(d.doneC), so DefaultClientDispatcher.Stop()'s unconditional <-done
// wait (ocppj/dispatcher.go:228) never returns, dispatcher.Stop() (called
// from ocppj.Client.Stop(), client.go:169) never returns, and neither does
// chargePoint.Stop() (charge_point.go:542).
//
// RED (today): Stop() hangs - the watchdog fires with "did not return within
// the bounded deadline".
func (suite *OcppV16TestSuite) TestL2ShutdownCancelHookDeadlock() {
	t := suite.T()
	suite.mockWsClient.On("Stop").Return()
	suite.mockWsClient.On("IsConnected").Return(false)

	writeC := make(chan []byte, 8)
	ocppj.SetMessageIdGenerator(sequentialMessageIds("l2t1"))
	defer func() { ocppj.SetMessageIdGenerator(suite.messageIdGenerator.generateId) }()

	startStandaloneChargePoint(suite, writeC)

	gateC := make(chan struct{})
	// Release the blocked callback on the way out, whatever happens above,
	// so a pinned asyncCallbackHandler goroutine cannot wedge the rest of the
	// suite even if this test fails.
	defer close(gateC)

	l2PinHandlerWithTwoOutstanding(suite, writeC, "l2t1", gateC)

	l2BoundedStop(t, suite.chargePoint)
}

// ============================================================================
// Test 1b - L2 cancel-hook deadlock, variant (a): pump PRE-wedged in the
// TIMEOUT arm before Stop() is ever called (spec §L2, line 41: "Both wedge
// variants confirmed (pump pre-wedged in the timeout arm; pump wedged in the
// stop-drain loop dispatcher.go:309-316)"). TestL2ShutdownCancelHookDeadlock
// above only pins variant (b) - the wedge Stop()'s OWN stop-drain loop
// manufactures. This test pins variant (a): the same terminal deadlock,
// reached without Stop() ever having run.
// ============================================================================

// TestL2ShutdownCancelHookDeadlockTimeoutArm configures a short dispatcher
// request timeout (before Start, per ClientDispatcher.SetTimeout's
// contract), then reuses l2PinHandlerWithTwoOutstanding's exact R0/R1/R2
// recipe - but instead of relying on Stop()'s stop-drain loop to cancel R1
// and R2, this test lets the dispatcher's OWN short timeout do it: R1
// (dispatched, unanswered) times out first and its cancel send
// (onRequestTimeout, charge_point.go:77, into cp.incoming, cap 1) succeeds
// immediately (buffer empty - the drainer already dequeued R0's response
// before pinning). R2 is then auto-dispatched (ocppj's CompleteRequest
// signals readyForDispatch, dispatcher.go:484-495) and ALSO times out; ITS
// cancel send finds the buffer still full (R1's, never drained - the sole
// drainer is permanently pinned in R0's callback) and BLOCKS the messagePump
// goroutine right there, inside the timer arm (dispatcher.go:323-341),
// entirely BEFORE this test calls Stop(). Only once that pre-Stop wedge is
// positively confirmed (a goroutine-stack poll for onRequestTimeout's
// blocked frame) does the test call Stop().
//
// RED (today): Stop() hangs - the same terminal mechanism as
// TestL2ShutdownCancelHookDeadlock (DefaultClientDispatcher.Stop()'s
// unconditional <-done wait, dispatcher.go:227-228, never returns because
// messagePump never reaches close(d.doneC)), but reached via the OTHER named
// wedge site.
func (suite *OcppV16TestSuite) TestL2ShutdownCancelHookDeadlockTimeoutArm() {
	t := suite.T()
	suite.mockWsClient.On("Stop").Return()
	suite.mockWsClient.On("IsConnected").Return(false)

	// Short dispatcher timeout, set before Start per SetTimeout's contract.
	// 450ms (not ~150ms): the timer arms at R0's dispatch, so a test goroutine
	// stalled between R0's write and its CALL_RESULT delivery would let R0
	// itself time out, drop its pending state, and turn the delivery into a
	// confusing ParseMessage error. 450ms keeps that margin on loaded CI under
	// -race while the wedge still forms at ~2x timeout, well inside l2Bound.
	// No restoration needed: SetupTest rebuilds the dispatcher per test.
	suite.clientDispatcher.SetTimeout(450 * time.Millisecond)

	writeC := make(chan []byte, 8)
	ocppj.SetMessageIdGenerator(sequentialMessageIds("l2t1b"))
	defer func() { ocppj.SetMessageIdGenerator(suite.messageIdGenerator.generateId) }()

	startStandaloneChargePoint(suite, writeC)

	gateC := make(chan struct{})
	defer close(gateC)

	l2PinHandlerWithTwoOutstanding(suite, writeC, "l2t1b", gateC)

	// R1 and R2 are outstanding and will never be answered. Left alone, the
	// dispatcher's own short timeout cancels each in turn as it becomes the
	// current in-flight request, wedging the pump inside onRequestTimeout's
	// blocking send for R2 - confirm that BEFORE calling Stop(), so the RED
	// this test pins is unambiguously the pre-Stop timeout-arm wedge, not a
	// Stop()-manufactured one.
	l2WaitForGoroutineCountAtLeast(t, "(*chargePoint).onRequestTimeout(", 1)

	l2BoundedStop(t, suite.chargePoint)
}

// ============================================================================
// Test 2 - error() shutdown-preemptibility (spec §L2 PR-L2 item 2, "error()
// on both facades").
// ============================================================================

// TestL2ShutdownErrorSendPreemptible parks the asyncCallbackHandler goroutine
// INSIDE error() itself (blocked on the `cp.errC <- err` send), then calls
// Stop(), and asserts the handler goroutine is RELEASED within the watchdog.
//
// This REPLACES the former TestL2ShutdownErrorsNeverDrainedStopStillReturns,
// which was a DUPLICATE of TestL2ShutdownCancelHookDeadlock in disguise: its
// RED came entirely from the cancel-hook pump wedge (two outstanding,
// never-answered requests forcing onRequestTimeout's send to block) - the
// pre-seeded error just sat, inert, in errC's cap-1 buffer. That test would
// have PASSED even if error() were NEVER made preemptible, leaving PR-L2's
// mandated "error() on both facades" requirement with ZERO coverage. This
// test's arrangement deliberately contains NO outstanding/cancelable
// requests at all - nothing here can wedge via the cancel-hook mechanism, so
// a RED here can only mean error() itself is non-preemptible.
//
// Recipe (spec §L2 PR-L2 item 2 - "error() on both facades"): obtain
// Errors() and never drain it; drive TWO no-callback errors via the "no
// handler available" path (mirrors the old test's phantom-pending-request
// recipe, done twice). The first (err1) lands in errC's empty cap-1 buffer
// without blocking (asyncCallbackHandler's own cp.error(err1) call is a
// non-blocking buffered send). The cap-1 cp.incoming channel that carries
// both phantom deliveries to the handler naturally sequences the two: the
// second phantom's send onto cp.incoming can only succeed once the handler
// has dequeued the first, and single-goroutine program order guarantees the
// handler's cp.error(err1) call (its very next statement) completes before
// the handler loops back to dequeue the second - so by the time it processes
// err2, err1 is unconditionally already sitting in errC. err2's
// `cp.error(err2)` call therefore blocks: errC already holds err1, unread,
// and nothing drains it. The handler is now parked INSIDE error() - exactly
// the scenario PR-L2 item 2 requires be preemptible - confirmed positively
// via a goroutine-stack poll for error()'s blocked frame BEFORE Stop() is
// called, so the RED reason is unambiguous.
//
// HISTORICAL NOTE (this test was skipped until PR-L2 landed): before PR-L2,
// chargePoint.Stop() unconditionally did `close(cp.errC); cp.errC = nil`. A
// handler already parked on `cp.errC <- err2` when that close ran would be
// panicked immediately (send on closed channel) - on a goroutine spawned
// internally by production Start(), which this black-box test package cannot
// wrap in a recover, so it crashed the shared `go test` binary rather than
// failing one test. PR-L2 item 5 drops that close, which is what makes this
// recipe as safe here as it always was on 2.0.1 (whose Stop() never closed
// errC). Do NOT reintroduce an unconditional errC close without re-skipping
// this test: PR-L1 reinstates it only as close-after-SUCCESSFUL-join, which
// by construction cannot race a parked sender.
//
// RED (pre-PR-L2, once enabled): the handler goroutine is never released -
// l2WaitForGoroutineCountAtMost times out via t.Fatal. GREEN once error()
// selects on stopC.
func (suite *OcppV16TestSuite) TestL2ShutdownErrorSendPreemptible() {
	t := suite.T()

	suite.mockWsClient.On("Stop").Return()
	suite.mockWsClient.On("IsConnected").Return(false)

	writeC := make(chan []byte, 8)
	ocppj.SetMessageIdGenerator(sequentialMessageIds("l2t2"))
	defer func() { ocppj.SetMessageIdGenerator(suite.messageIdGenerator.generateId) }()

	startStandaloneChargePoint(suite, writeC)

	// Errors() obtained, never drained.
	_ = suite.chargePoint.Errors()

	// First no-callback error: lands in cp.errC's empty cap-1 buffer without
	// blocking. Safe to deliver synchronously - cp.incoming and cp.errC both
	// start empty, so nothing on this path can block yet.
	phantomID1 := "l2t2-phantom-1"
	suite.ocppjChargePoint.RequestState.AddPendingRequest(phantomID1, core.NewHeartbeatRequest())
	require.NoError(t, suite.clientRequestQueue.Push(ocppj.RequestBundle{Call: &ocppj.Call{UniqueId: phantomID1}}))
	errorJson1 := fmt.Sprintf(`[4,"%v","%v","%v",{}]`, phantomID1, ocppj.GenericError, "no callback registered 1")
	err := suite.mockWsClient.MessageHandler([]byte(errorJson1))
	require.NoError(t, err)

	// Second no-callback error: delivered on its own goroutine since its
	// send onto cp.incoming (cap 1) may transiently block until the handler
	// has dequeued the first message - never call this synchronously and
	// unbounded from the test goroutine. Once delivered, the handler
	// dequeues it, finds no callback, and calls cp.error(err2) - which
	// blocks (errC already holds err1, unread, single-goroutine program
	// order guarantees err1's send completed first). The handler is now
	// permanently parked INSIDE error().
	phantomID2 := "l2t2-phantom-2"
	suite.ocppjChargePoint.RequestState.AddPendingRequest(phantomID2, core.NewHeartbeatRequest())
	require.NoError(t, suite.clientRequestQueue.Push(ocppj.RequestBundle{Call: &ocppj.Call{UniqueId: phantomID2}}))
	errorJson2 := fmt.Sprintf(`[4,"%v","%v","%v",{}]`, phantomID2, ocppj.GenericError, "no callback registered 2")
	delivered2C := make(chan struct{})
	go func() {
		defer close(delivered2C)
		_ = suite.mockWsClient.MessageHandler([]byte(errorJson2))
	}()
	select {
	case <-delivered2C:
	case <-time.After(l2Bound):
		t.Fatal("timed out delivering the second no-callback error onto cp.incoming")
	}

	// Positive confirmation the handler is parked INSIDE error() - and, by
	// construction (no request was ever dispatched/timed out/canceled in
	// this test), NOT wedged via the cancel-hook mechanism.
	l2WaitForGoroutineCountAtLeast(t, "(*chargePoint).error(", 1)

	l2BoundedStop(t, suite.chargePoint)

	// The parked handler goroutine must be released - only a preemptible
	// `select { case cp.errC <- err: case <-stopC: }` inside error() can
	// unwedge it.
	l2WaitForGoroutineCountAtMost(t, "(*chargePoint).error(", 0)
}

// ============================================================================
// Test 3 - readPump-leak regression (spec §L2 "wider hazard" / fable MAJOR-2).
// ============================================================================

// TestL2ShutdownReadPumpForwardingLeak pins the spec's "wider hazard" (§L2, lines
// 43): the forwarding closures wired in NewChargePoint (v16.go:237-248) do
// blocking cap-1 sends on cp.incoming and run on the ws readPump goroutine
// (simulated here by whichever goroutine calls
// suite.mockWsClient.MessageHandler). Nothing joins that goroutine anywhere
// in today's Stop() - so if the sole drainer is unavailable while TWO
// responses are in flight, the second forwarder blocks forever: a permanent
// goroutine leak, independent of whether Stop() itself returns.
//
// Recipe: R0 pins asyncCallbackHandler on a blocking callback (so it can
// never again drain cp.incoming - the strongest form of "the drainer is
// unavailable", stronger than a clean stopC-triggered exit, which a single
// in-flight response would not leak against since a cap-1 buffer absorbs one
// send regardless of whether a reader exists). R1's and R2's CALL_RESULTs
// are then each paused, via testhooks.ChargePointResponse, exactly before
// their forwarding closure's `cp.incoming <-` send (v16.go:238-240
// documents this seam). Once BOTH are confirmed paused there, they are
// released together: one send fills the empty cap-1 buffer and returns; the
// other finds it full, with no reader ever coming, and blocks forever.
//
// RED (today): after Stop() (which itself returns - it does not depend on
// the readPump/handler at all) and release, the leaked goroutine's count
// does not return to baseline within the watchdog - l2WaitForGoroutineCountAtMost
// times out via t.Fatal.
func (suite *OcppV16TestSuite) TestL2ShutdownReadPumpForwardingLeak() {
	t := suite.T()
	suite.mockWsClient.On("Stop").Return()
	suite.mockWsClient.On("IsConnected").Return(false)

	testhooks.ChargePointResponse = nil
	defer func() { testhooks.ChargePointResponse = nil }()

	// Baseline BEFORE any goroutine from this test exists.
	baseline := l2CountGoroutinesByStack("NewChargePoint.func")

	writeC := make(chan []byte, 8)
	ocppj.SetMessageIdGenerator(sequentialMessageIds("l2t3"))
	defer func() { ocppj.SetMessageIdGenerator(suite.messageIdGenerator.generateId) }()

	startStandaloneChargePoint(suite, writeC)

	gateC := make(chan struct{})
	defer close(gateC)

	// --- R0: pin the sole drainer forever (in today's code - no preemption exists). ---
	pinnedC := make(chan struct{})
	err := suite.chargePoint.SendRequestAsync(core.NewHeartbeatRequest(), func(confirmation ocpp.Response, err error) {
		close(pinnedC)
		<-gateC
	})
	require.NoError(t, err)
	select {
	case <-writeC:
	case <-time.After(l2Bound):
		t.Fatal("timed out waiting for R0 to be written")
	}
	err = suite.mockWsClient.MessageHandler([]byte(heartbeatCallResultJson("l2t3-0")))
	require.NoError(t, err)
	waitOrFail(suite, pinnedC, "timed out waiting for the async handler to be pinned on R0's callback")

	// --- R1 and R2: two more in-flight requests, sequentially dispatched. ---
	err = suite.chargePoint.SendRequestAsync(core.NewHeartbeatRequest(), func(confirmation ocpp.Response, err error) {})
	require.NoError(t, err)
	select {
	case <-writeC:
	case <-time.After(l2Bound):
		t.Fatal("timed out waiting for R1 to be written")
	}

	// Pause every response delivery at the forwarding closure, exactly
	// before its cp.incoming send, and report back on enteredC.
	enteredC := make(chan struct{}, 2)
	releaseC := make(chan struct{})
	testhooks.ChargePointResponse = func(confirmation ocpp.Response, requestId string) {
		enteredC <- struct{}{}
		<-releaseC
	}

	r1DoneC := make(chan struct{})
	go func() {
		defer close(r1DoneC)
		_ = suite.mockWsClient.MessageHandler([]byte(heartbeatCallResultJson("l2t3-1")))
	}()
	waitOrFail(suite, enteredC, "timed out waiting for R1's forward to reach the paused hook")

	// R1 is now paused BEFORE its cp.incoming send, so from the dispatcher's
	// perspective R1 already completed (CompleteRequest runs before the
	// facade's response handler is invoked) - R2 gets auto-dispatched.
	err = suite.chargePoint.SendRequestAsync(core.NewHeartbeatRequest(), func(confirmation ocpp.Response, err error) {})
	require.NoError(t, err)
	select {
	case <-writeC:
	case <-time.After(l2Bound):
		t.Fatal("timed out waiting for R2 to be written")
	}

	r2DoneC := make(chan struct{})
	go func() {
		defer close(r2DoneC)
		_ = suite.mockWsClient.MessageHandler([]byte(heartbeatCallResultJson("l2t3-2")))
	}()
	waitOrFail(suite, enteredC, "timed out waiting for R2's forward to reach the paused hook")

	// Positive control: confirm the stack-substring filter actually matches
	// something before relying on it for the RED assertion below. Both R1
	// and R2 are paused inside NewChargePoint's forwarding closures right
	// now, so this must find at least one - if a future refactor
	// renames/extracts those closures, this filter would match nothing and
	// the leak assertion below would pass VACUOUSLY (0 <= baseline is
	// trivially true).
	if got := l2CountGoroutinesByStack("NewChargePoint.func"); got < 1 {
		t.Fatalf("positive control failed: goroutine stack filter %q matched nothing (got %d) - the filter is stale", "NewChargePoint.func", got)
	}

	// Both readPump-analog goroutines are now paused, before either has
	// touched cp.incoming. Stop the charge point first (it does not depend
	// on the readPump or the handler at all, so it must return promptly
	// regardless of what happens to R1/R2 below).
	l2BoundedStop(t, suite.chargePoint)

	// Release both paused forwards together: they race cp.incoming (cap 1,
	// currently empty - asyncCallbackHandler has been permanently pinned in
	// R0's callback this whole test and never drained anything else). One
	// send fills the buffer and returns; the other finds it full, with no
	// reader ever coming (the drainer is gone forever), and blocks forever.
	close(releaseC)

	// Exactly one of the two wins the single buffer slot and returns; WHICH one
	// is a genuine race, so wait on EITHER. (Selecting on r1DoneC alone would
	// spuriously report "neither returned" on the ~50% of runs where R2 wins,
	// and burn the full bound doing it.)
	select {
	case <-r1DoneC:
	case <-r2DoneC:
	case <-time.After(l2Bound):
		t.Fatal("neither R1 nor R2's forward returned - expected exactly one of them to")
	}

	// Exactly one of r1DoneC/r2DoneC must remain pending forever (today):
	// assert the goroutine count for this closure family does NOT settle
	// back to baseline. l2WaitForGoroutineCountAtMost expects <= baseline
	// within the bound and fails via t.Fatal if it never gets there - which
	// is the RED signal for this defect (a permanent leak never reaches
	// baseline).
	l2WaitForGoroutineCountAtMost(t, "NewChargePoint.func", baseline)
}

// ============================================================================
// Test 3b/3c - the REMAINING forwarding-closure sites (PR-L2 item 2 mandates
// preemptibility at ALL of v16.go:241 (response), :244 (error) and :247
// (request); the leak test above pins only :241). Without these, an
// implementation that converts only the response closure goes fully green
// while an inbound CALL_ERROR or CALL racing Stop() still leaks the readPump
// forever - the exact "trade a hang for a leak" failure §L2 names.
//
// All three 1.6 closures feed the same cap-1 cp.incoming, so the recipe is
// identical: pin the sole drainer, let message #1 fill the buffer, then block
// message #2's closure forever.
// ============================================================================

// TestL2ShutdownErrorForwardingLeak pins the ERROR forwarding closure
// (v16.go:244). RED (today): its `cp.incoming <- ...` has no stopC arm, so
// the second delivery's goroutine never returns and never reaches baseline.
func (suite *OcppV16TestSuite) TestL2ShutdownErrorForwardingLeak() {
	t := suite.T()
	suite.mockWsClient.On("Stop").Return()
	suite.mockWsClient.On("IsConnected").Return(false)

	baseline := l2CountGoroutinesByStack("NewChargePoint.func")

	writeC := make(chan []byte, 8)
	ocppj.SetMessageIdGenerator(sequentialMessageIds("l2t3b"))
	defer func() { ocppj.SetMessageIdGenerator(suite.messageIdGenerator.generateId) }()

	startStandaloneChargePoint(suite, writeC)

	gateC := make(chan struct{})
	defer close(gateC)

	// R0 pins the sole drainer, so cp.incoming is never read again.
	pinnedC := make(chan struct{})
	err := suite.chargePoint.SendRequestAsync(core.NewHeartbeatRequest(), func(confirmation ocpp.Response, err error) {
		close(pinnedC)
		<-gateC
	})
	require.NoError(t, err)
	select {
	case <-writeC:
	case <-time.After(l2Bound):
		t.Fatal("timed out waiting for R0 to be written")
	}
	err = suite.mockWsClient.MessageHandler([]byte(heartbeatCallResultJson("l2t3b-0")))
	require.NoError(t, err)
	waitOrFail(suite, pinnedC, "timed out waiting for the async handler to be pinned on R0's callback")

	// Phantom CALL_ERROR #1: its ERROR closure fills the now-empty cp.incoming
	// and returns.
	phantomID1 := "l2t3b-phantom-1"
	suite.ocppjChargePoint.RequestState.AddPendingRequest(phantomID1, core.NewHeartbeatRequest())
	require.NoError(t, suite.clientRequestQueue.Push(ocppj.RequestBundle{Call: &ocppj.Call{UniqueId: phantomID1}}))
	err = suite.mockWsClient.MessageHandler([]byte(fmt.Sprintf(`[4,"%v","%v","%v",{}]`, phantomID1, ocppj.GenericError, "leak 1")))
	require.NoError(t, err)

	// Phantom CALL_ERROR #2: its ERROR closure finds cp.incoming full with no
	// reader ever coming, and blocks forever.
	phantomID2 := "l2t3b-phantom-2"
	suite.ocppjChargePoint.RequestState.AddPendingRequest(phantomID2, core.NewHeartbeatRequest())
	require.NoError(t, suite.clientRequestQueue.Push(ocppj.RequestBundle{Call: &ocppj.Call{UniqueId: phantomID2}}))
	blockedC := make(chan struct{})
	go func() {
		defer close(blockedC)
		_ = suite.mockWsClient.MessageHandler([]byte(fmt.Sprintf(`[4,"%v","%v","%v",{}]`, phantomID2, ocppj.GenericError, "leak 2")))
	}()

	// Load-bearing (mirrors the response-closure test): wait for #2's forward
	// to actually be parked before Stop(), else Stop()'s dispatcher drain can
	// clear the pending id first and the delivery fails fast without ever
	// attempting the forward. This also DOUBLES AS THE POSITIVE CONTROL for
	// the stack filter: if a refactor renames/extracts the forwarding closures
	// the filter matches nothing, and this fails (false-red, safe) rather than
	// the leak assertion below passing vacuously.
	l2WaitForGoroutineCountAtLeast(t, "NewChargePoint.func", baseline+1)

	l2BoundedStop(t, suite.chargePoint)
	l2WaitForGoroutineCountAtMost(t, "NewChargePoint.func", baseline)
	select {
	case <-blockedC:
	case <-time.After(l2Bound):
		t.Fatal("the blocked error forward never returned even though the goroutine count claims it settled")
	}
}

// TestL2ShutdownRequestForwardingLeak pins the REQUEST forwarding closure
// (v16.go:247) with two inbound CALLs. RED (today): the second CALL's
// goroutine blocks forever on cp.incoming and never reaches baseline.
func (suite *OcppV16TestSuite) TestL2ShutdownRequestForwardingLeak() {
	t := suite.T()
	suite.mockWsClient.On("Stop").Return()
	suite.mockWsClient.On("IsConnected").Return(false)

	baseline := l2CountGoroutinesByStack("NewChargePoint.func")

	writeC := make(chan []byte, 8)
	ocppj.SetMessageIdGenerator(sequentialMessageIds("l2t3c"))
	defer func() { ocppj.SetMessageIdGenerator(suite.messageIdGenerator.generateId) }()

	startStandaloneChargePoint(suite, writeC)

	gateC := make(chan struct{})
	defer close(gateC)

	pinnedC := make(chan struct{})
	err := suite.chargePoint.SendRequestAsync(core.NewHeartbeatRequest(), func(confirmation ocpp.Response, err error) {
		close(pinnedC)
		<-gateC
	})
	require.NoError(t, err)
	select {
	case <-writeC:
	case <-time.After(l2Bound):
		t.Fatal("timed out waiting for R0 to be written")
	}
	err = suite.mockWsClient.MessageHandler([]byte(heartbeatCallResultJson("l2t3c-0")))
	require.NoError(t, err)
	waitOrFail(suite, pinnedC, "timed out waiting for the async handler to be pinned on R0's callback")

	// Inbound CALL #1: its REQUEST closure fills cp.incoming and returns.
	err = suite.mockWsClient.MessageHandler([]byte(fmt.Sprintf(`[2,"%v","%v",{}]`, "l2t3c-call-1", core.ClearCacheFeatureName)))
	require.NoError(t, err)

	// Inbound CALL #2: its REQUEST closure finds cp.incoming full, forever.
	blockedC := make(chan struct{})
	go func() {
		defer close(blockedC)
		_ = suite.mockWsClient.MessageHandler([]byte(fmt.Sprintf(`[2,"%v","%v",{}]`, "l2t3c-call-2", core.ClearCacheFeatureName)))
	}()
	// Park-wait before Stop() (see the error-closure test for why this is
	// load-bearing); doubles as the stale-stack-filter positive control.
	l2WaitForGoroutineCountAtLeast(t, "NewChargePoint.func", baseline+1)

	l2BoundedStop(t, suite.chargePoint)
	l2WaitForGoroutineCountAtMost(t, "NewChargePoint.func", baseline)
	select {
	case <-blockedC:
	case <-time.After(l2Bound):
		t.Fatal("the blocked request forward never returned even though the goroutine count claims it settled")
	}
}

// ============================================================================
// Test 4 - 1.6 parity guards (spec §Verified current state: "Double Stop() /
// Stop() before Start() panics" on 1.6; 2.0.1 already has stopOnce + nil
// guards). PR-L2 item 4 folds these into the reorder.
// ============================================================================

// TestL2ShutdownDoubleStopDoesNotPanic guards that a second Stop() after a first does
// not panic. RED (today): charge_point.go:543 `close(cp.stopC)` runs
// unconditionally on every Stop() call - no sync.Once/nil guard - so a
// second Stop() closes an already-closed channel and panics.
func (suite *OcppV16TestSuite) TestL2ShutdownDoubleStopDoesNotPanic() {
	t := suite.T()
	wsID := "test_id"
	wsURL := "someUrl"
	channel := NewMockWebSocket(wsID)

	setupDefaultChargePointHandlers(suite, nil, expectedChargePointOptions{
		serverUrl:             wsURL,
		clientId:              wsID,
		createChannelOnStart:  false,
		channel:               channel,
		writeReturnArgument:   nil,
		forwardWrittenMessage: false,
	})
	suite.mockWsClient.On("Stop").Return()
	suite.mockWsClient.On("IsConnected").Return(false)

	err := suite.chargePoint.Start(wsURL)
	require.NoError(t, err)

	l2BoundedStop(t, suite.chargePoint)
	l2BoundedStop(t, suite.chargePoint)
}

// TestL2ShutdownStopBeforeStartDoesNotPanic guards that Stop() before any Start()
// does not panic. RED (today): cp.stopC is nil until Start() assigns it
// (charge_point.go:532), and Stop() (:543) does `close(cp.stopC)`
// unconditionally - close(nil) panics.
func (suite *OcppV16TestSuite) TestL2ShutdownStopBeforeStartDoesNotPanic() {
	t := suite.T()
	suite.mockWsClient.On("Stop").Return()
	suite.mockWsClient.On("IsConnected").Return(false)

	l2BoundedStop(t, suite.chargePoint)
}

// ============================================================================
// Test 5 - restart / stopC field race (spec §PR-L2 item 1: "a synchronized
// stopC accessor... Prefer atomic.Value for stopC specifically").
// ============================================================================

// TestL2ShutdownRestartStopCRace loops Start -> Stop while a concurrent goroutine
// keeps sending requests that read cp.stopC (charge_point.go:362,
// SendRequestCtx's `cp.stopC` argument to awaitCtxResult) - racing Start's
// reassignment (:532, `cp.stopC = make(chan struct{}, 1)`) and Stop's close
// (:543). Must be run with `go test -race` to surface the field race;
// without -race this test is expected to complete without incident (a data
// race is undefined behavior, not a guaranteed crash).
//
// -race-ONLY BY DESIGN: this test passes trivially without `-race` (a data
// race is undefined behavior, not a guaranteed failure - the race detector
// is the only thing that can observe it). A green result from a plain
// `go test` run (no `-race`) proves NOTHING about this test's actual
// subject; only `go test -race` is a meaningful signal here.
//
// Bounded and deterministic: the traffic goroutine issues one blocking
// SendRequest at a time (never spawns unbounded goroutines), and every
// blocked SendRequest is released either by a fast dispatcher-not-running
// error or by the current generation's Stop() closing stopC - both of which
// happen within the loop's own cadence. The whole test is watchdog-bounded so
// a genuine deadlock (as opposed to a benign race) fails cleanly.
func (suite *OcppV16TestSuite) TestL2ShutdownRestartStopCRace() {
	t := suite.T()
	wsURL := "someUrl"
	suite.mockWsClient.On("Start", mock.AnythingOfType("string")).Return(nil)
	suite.mockWsClient.On("Write", mock.Anything).Return(nil)
	suite.mockWsClient.On("Stop").Return()
	suite.mockWsClient.On("IsConnected").Return(false)

	const iterations = 20

	trafficStop := make(chan struct{})
	var trafficWG sync.WaitGroup
	trafficWG.Add(1)
	go func() {
		defer trafficWG.Done()
		for {
			select {
			case <-trafficStop:
				return
			default:
			}
			_, _ = suite.chargePoint.SendRequest(core.NewHeartbeatRequest())
		}
	}()

	watchdogDone := make(chan struct{})
	go func() {
		defer close(watchdogDone)
		for i := 0; i < iterations; i++ {
			_ = suite.chargePoint.Start(wsURL)
			suite.chargePoint.Stop()
		}
	}()

	select {
	case <-watchdogDone:
	case <-time.After(l2Bound * 4):
		t.Fatal("Start/Stop loop did not complete within the bounded deadline")
	}
	close(trafficStop)

	waitC := make(chan struct{})
	go func() {
		trafficWG.Wait()
		close(waitC)
	}()
	select {
	case <-waitC:
	case <-time.After(l2Bound):
		t.Fatal("traffic goroutine did not exit within the bounded deadline")
	}
}
