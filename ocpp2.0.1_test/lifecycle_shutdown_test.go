package ocpp2_test

// PR-L2 (tasks/facade-lifecycle-hardening.md, "## PR-L2 — make every
// facade-channel producer shutdown-preemptible") RED-FIRST test suite for the
// OCPP 2.0.1 charging-station (client) facade.
//
// This file pins ONLY PR-L2's scope: every facade-channel producer
// (onRequestTimeout's cancel hook, the forwarding closures wired in
// NewChargingStation, and error()) must become preemptible against stopC,
// and Stop() must be reordered to close stopC before client.Stop(). It does
// NOT test PR-L1 (StopCtx, the generation handshake/join, errC close-after-
// join, the L3 residual guard) - that surface does not exist yet.
//
// RED-first discipline: every test below either hangs today, so every
// blocking call is watchdog-bounded (via the existing boundedStop helper
// from ocpp2.0.1_test/stop_lifecycle_test.go, same package) so a regression
// fails fast instead of wedging the test binary.
//
// Reused from ocpp2.0.1_test/stop_lifecycle_test.go (same package,
// pre-existing E1b infra): the `stopper` interface, `boundedStop`,
// `e1bBound`, `countGoroutinesByStack`. This file defines its own
// l2WaitForGoroutineCountAtMost/AtLeast (at-most/at-least semantics, 5s
// bound) rather than reusing that file's waitForGoroutineCount (exact-match,
// 2s bound) - more robust to unrelated goroutine churn on loaded CI, and
// matches the sibling 1.6 file's helpers of the same name.
//
// 2.0.1 vs 1.6 asymmetry that shapes this file (spec §Verified current
// state and §PR-L1 item 4): 2.0.1's Stop() (charging_station.go:718-725)
// NEVER closes cs.errC — only 1.6 does (unconditionally, today). That means
// the "park the handler inside error()" scenario (TestL2ShutdownErrorSendPreemptible
// below) is safe to run ENABLED here - there is no close(cs.errC) for a
// parked sender to race, unlike the sibling 1.6 file's t.Skip()-ed version of
// the same test.

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/enesismail/ocpp-go/ocpp"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/availability"
	"github.com/enesismail/ocpp-go/ocppj"
)

// l2SequentialMessageIds returns a message-ID generator producing a fresh,
// distinguishable id ("<prefix>-0", "<prefix>-1", ...) on every call, so
// multiple requests pending at different times in one test can each be
// individually addressed by the CALL_RESULT/CALL_ERROR the test crafts for
// it. Mirrors ocpp1.6_test/inbound_ordering_test.go's sequentialMessageIds
// (not importable here — different package).
func l2SequentialMessageIds(prefix string) func() string {
	n := -1
	return func() string {
		n++
		return fmt.Sprintf("%s-%d", prefix, n)
	}
}

// l2HeartbeatCallResultJson builds a CALL_RESULT payload for a
// HeartbeatRequest sent by the charging station, addressed to the given
// (already-pending) request id.
func l2HeartbeatCallResultJson(id string) string {
	return fmt.Sprintf(`[3,"%v",{"currentTime":"%v"}]`, id, time.Now().Format(time.RFC3339))
}

// l2WaitOrFail receives from c, failing the test with msg if the wait
// exceeds e1bBound. Used instead of time.Sleep for every synchronization
// point in this file.
func l2WaitOrFail(t *testing.T, c <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(e1bBound):
		t.Fatal(msg)
	}
}

// l2StartStandaloneChargingStation starts suite.chargingStation directly
// against the mocked websocket client, with no CSMS mock in the loop, and
// mocks in everything Stop() needs. writtenC (if non-nil) receives every raw
// outgoing message, in write order.
func l2StartStandaloneChargingStation(suite *OcppV2TestSuite, writtenC chan []byte) {
	t := suite.T()
	suite.mockWsClient.On("Start", mock.AnythingOfType("string")).Return(nil)
	suite.mockWsClient.On("Write", mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		if writtenC != nil {
			data := args.Get(0).([]byte)
			cp := make([]byte, len(data))
			copy(cp, data)
			writtenC <- cp
		}
	})
	suite.mockWsClient.On("Stop").Return()
	suite.mockWsClient.On("IsConnected").Return(false)

	err := suite.chargingStation.Start("someUrl")
	require.NoError(t, err)
}

// l2PinHandlerWithTwoOutstanding sends R0 (pinning the sole
// asyncCallbackHandler drainer on a callback that blocks on gateC) and then
// two more requests, R1 (dispatched) and R2 (queued behind it), that are
// never answered - they stay outstanding in the dispatcher until something
// cancels them. idPrefix must be unique per test. Returns once R0's callback
// is confirmed pinned.
func l2PinHandlerWithTwoOutstanding(suite *OcppV2TestSuite, writtenC chan []byte, idPrefix string, gateC chan struct{}) {
	t := suite.T()
	pinnedC := make(chan struct{})
	err := suite.chargingStation.SendRequestAsync(availability.NewHeartbeatRequest(), func(response ocpp.Response, err error) {
		close(pinnedC)
		<-gateC
	})
	require.NoError(t, err)

	l2WaitForWrite(t, writtenC, "timed out waiting for R0 to be written")
	err = suite.mockWsClient.MessageHandler([]byte(l2HeartbeatCallResultJson(idPrefix + "-0")))
	require.NoError(t, err)
	l2WaitOrFail(t, pinnedC, "timed out waiting for the async handler to be pinned on R0's callback")

	// R1: dispatched (becomes the in-flight request).
	err = suite.chargingStation.SendRequestAsync(availability.NewHeartbeatRequest(), func(response ocpp.Response, err error) {})
	require.NoError(t, err)
	l2WaitForWrite(t, writtenC, "timed out waiting for R1 to be written")

	// R2: queued behind R1. Never written, never answered.
	err = suite.chargingStation.SendRequestAsync(availability.NewHeartbeatRequest(), func(response ocpp.Response, err error) {})
	require.NoError(t, err)
}

// l2WaitForWrite receives one value from writtenC, failing the test with msg
// if the wait exceeds e1bBound.
func l2WaitForWrite(t *testing.T, writtenC chan []byte, msg string) {
	t.Helper()
	select {
	case <-writtenC:
	case <-time.After(e1bBound):
		t.Fatal(msg)
	}
}

// l2Bound is the bounded deadline for the goroutine-count waits in this
// file (Fix 3 hardening pass): mirrors the sibling 1.6 file's l2Bound. Used
// instead of stop_lifecycle_test.go's e1bBound (2s) for goroutine-count
// polling specifically, since at-most/at-least semantics with more headroom
// are more robust to unrelated goroutine churn on loaded CI than an
// exact-match wait with a tighter bound.
const l2Bound = 5 * time.Second

// l2WaitForGoroutineCountAtMost polls (bounded by l2Bound) until the number
// of goroutines whose stack trace contains substr is AT MOST want, failing
// via t.Fatal if the deadline elapses first. Used for "no leak" assertions:
// an exact `== want` wait (stop_lifecycle_test.go's waitForGoroutineCount)
// can flake on unrelated goroutine churn from other tests; `<= want` keeps
// the invariant this file actually cares about. Mirrors the sibling 1.6
// file's helper of the same name.
func l2WaitForGoroutineCountAtMost(t *testing.T, substr string, want int) {
	t.Helper()
	deadline := time.After(l2Bound)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if got := countGoroutinesByStack(substr); got <= want {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatalf("timed out waiting for goroutine count matching %q to reach at most %d (currently %d)",
				substr, want, countGoroutinesByStack(substr))
		}
	}
}

// l2WaitForGoroutineCountAtLeast polls (bounded by l2Bound) until the number
// of goroutines whose stack trace contains substr is AT LEAST want, failing
// via t.Fatal if the deadline elapses first. The mirror image of
// l2WaitForGoroutineCountAtMost's "no leak" check: used to positively
// confirm a goroutine has reached a specific blocked state (e.g. parked
// inside a channel send, or wedged inside a cancel hook) before the test
// proceeds to act on that state. Mirrors the sibling 1.6 file's helper of
// the same name.
func l2WaitForGoroutineCountAtLeast(t *testing.T, substr string, want int) {
	t.Helper()
	deadline := time.After(l2Bound)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if got := countGoroutinesByStack(substr); got >= want {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatalf("timed out waiting for goroutine count matching %q to reach at least %d (currently %d)",
				substr, want, countGoroutinesByStack(substr))
		}
	}
}

// ============================================================================
// Test 1 - L2 cancel-hook deadlock (spec §L2, PR-L2 item 2's onRequestTimeout
// site — 2.0.1 routes it through cs.errorHandler, not cp.incoming).
// ============================================================================

// TestL2ShutdownCancelHookDeadlock mirrors the sibling 1.6 test exactly, but on
// 2.0.1's structurally different (post-S3-unification-less) plumbing:
// onRequestTimeout (charging_station.go:87-89) sends DIRECTLY to
// cs.errorHandler (cap 1), not through a unified incoming channel. R0's
// callback pins the sole asyncCallbackHandler drainer (charging_station.go:
// 618-652). R1 (dispatched) and R2 (queued) are left outstanding. Stop()
// closes the dispatcher's request channel; messagePump's drain-and-cancel
// loop (ocppj/dispatcher.go:293-321) fires onRequestTimeout for BOTH
// outstanding requests, sequentially, on the messagePump goroutine. The
// first send into cs.errorHandler succeeds (buffer empty); the second BLOCKS
// FOREVER (buffer full, no reader - the drainer is pinned). messagePump
// never reaches close(d.doneC), so ocppj.Client.Stop()'s unconditional
// <-done wait never returns, and neither does chargingStation.Stop()
// (charging_station.go:718).
//
// RED (today): Stop() hangs - boundedStop's watchdog fires.
func (suite *OcppV2TestSuite) TestL2ShutdownCancelHookDeadlock() {
	t := suite.T()
	writtenC := make(chan []byte, 8)
	ocppj.SetMessageIdGenerator(l2SequentialMessageIds("l2t1"))
	defer func() { ocppj.SetMessageIdGenerator(suite.messageIdGenerator.generateId) }()

	l2StartStandaloneChargingStation(suite, writtenC)

	gateC := make(chan struct{})
	// Release the blocked callback on the way out, whatever happens above,
	// so a pinned asyncCallbackHandler goroutine cannot wedge the rest of
	// the suite even if this test fails.
	defer close(gateC)

	l2PinHandlerWithTwoOutstanding(suite, writtenC, "l2t1", gateC)

	boundedStop(t, suite.chargingStation)
}

// ============================================================================
// Test 1b - L2 cancel-hook deadlock, variant (a): pump PRE-wedged in the
// TIMEOUT arm before Stop() is ever called (spec §L2, line 41: "Both wedge
// variants confirmed (pump pre-wedged in the timeout arm; pump wedged in the
// stop-drain loop dispatcher.go:309-316)"). TestL2ShutdownCancelHookDeadlock
// above only pins variant (b) - the wedge Stop()'s OWN stop-drain loop
// manufactures. This test pins variant (a): the same terminal deadlock,
// reached without Stop() ever having run. Mirrors the sibling 1.6 file's
// test of the same name.
// ============================================================================

// TestL2ShutdownCancelHookDeadlockTimeoutArm configures a short dispatcher
// request timeout (before Start, per ClientDispatcher.SetTimeout's
// contract), then reuses l2PinHandlerWithTwoOutstanding's exact R0/R1/R2
// recipe - but instead of relying on Stop()'s stop-drain loop to cancel R1
// and R2, this test lets the dispatcher's OWN short timeout do it: R1
// (dispatched, unanswered) times out first and its cancel send
// (onRequestTimeout, charging_station.go:87-89, into cs.errorHandler, cap 1)
// succeeds immediately (buffer empty - the drainer already dequeued R0's
// response before pinning). R2 is then auto-dispatched (ocppj's
// CompleteRequest signals readyForDispatch, dispatcher.go:484-495) and ALSO
// times out; ITS cancel send finds the buffer still full (R1's, never
// drained - the sole drainer is permanently pinned in R0's callback) and
// BLOCKS the messagePump goroutine right there, inside the timer arm
// (dispatcher.go:323-341), entirely BEFORE this test calls Stop(). Only once
// that pre-Stop wedge is positively confirmed (a goroutine-stack poll for
// onRequestTimeout's blocked frame) does the test call Stop().
//
// RED (today): Stop() hangs - the same terminal mechanism as
// TestL2ShutdownCancelHookDeadlock, reached via the OTHER named wedge site.
func (suite *OcppV2TestSuite) TestL2ShutdownCancelHookDeadlockTimeoutArm() {
	t := suite.T()

	// Short dispatcher timeout, set before Start per SetTimeout's contract.
	// 450ms (not ~150ms): the timer arms at R0's dispatch, so a test goroutine
	// stalled between R0's write and its CALL_RESULT delivery would let R0
	// itself time out, drop its pending state, and turn the delivery into a
	// confusing ParseMessage error. 450ms keeps that margin on loaded CI under
	// -race while the wedge still forms at ~2x timeout, well inside l2Bound.
	// No restoration needed: SetupTest rebuilds the dispatcher per test.
	suite.clientDispatcher.SetTimeout(450 * time.Millisecond)

	writtenC := make(chan []byte, 8)
	ocppj.SetMessageIdGenerator(l2SequentialMessageIds("l2t1b"))
	defer func() { ocppj.SetMessageIdGenerator(suite.messageIdGenerator.generateId) }()

	l2StartStandaloneChargingStation(suite, writtenC)

	gateC := make(chan struct{})
	defer close(gateC)

	l2PinHandlerWithTwoOutstanding(suite, writtenC, "l2t1b", gateC)

	// R1 and R2 are outstanding and will never be answered. Left alone, the
	// dispatcher's own short timeout cancels each in turn as it becomes the
	// current in-flight request, wedging the pump inside onRequestTimeout's
	// blocking send for R2 - confirm that BEFORE calling Stop(), so the RED
	// this test pins is unambiguously the pre-Stop timeout-arm wedge, not a
	// Stop()-manufactured one.
	l2WaitForGoroutineCountAtLeast(t, "(*chargingStation).onRequestTimeout(", 1)

	boundedStop(t, suite.chargingStation)
}

// ============================================================================
// Test 2 - error() shutdown-preemptibility (spec §L2 PR-L2 item 2, "error()
// on both facades").
// ============================================================================

// TestL2ShutdownErrorSendPreemptible parks the asyncCallbackHandler goroutine
// INSIDE error() itself (blocked on the `cs.errC <- err` send), then calls
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
// callback available" path (mirrors the old test's phantom-pending-request
// recipe, done twice). The first (err1) lands in errC's empty cap-1 buffer
// without blocking (asyncCallbackHandler's own cs.error(err1) call is a
// non-blocking buffered send). The cap-1 cs.errorHandler channel that
// carries both phantom deliveries to the handler naturally sequences the
// two: the second phantom's send onto cs.errorHandler can only succeed once
// the handler has dequeued the first, and single-goroutine program order
// guarantees the handler's cs.error(err1) call (its very next statement)
// completes before the handler loops back to dequeue the second - so by the
// time it processes err2, err1 is unconditionally already sitting in errC.
// err2's `cs.error(err2)` call therefore blocks: errC already holds err1,
// unread, and nothing drains it. The handler is now parked INSIDE error() -
// exactly the scenario PR-L2 item 2 requires be preemptible - confirmed
// positively via a goroutine-stack poll for error()'s blocked frame BEFORE
// Stop() is called, so the RED reason is unambiguous.
//
// SAFE TO RUN ENABLED HERE (unlike the sibling 1.6 file's t.Skip()-ed
// version of this same test): chargingStation.Stop() (charging_station.go:
// 718-725) NEVER closes cs.errC, so a handler parked on `cs.errC <- err2` is
// never raced by a concurrent close - Stop() returning (once PR-L2 makes
// error() preemptible) is what releases it, not a channel-closure panic.
//
// RED (today): the handler goroutine never gets released -
// l2WaitForGoroutineCountAtMost times out via t.Fatal.
func (suite *OcppV2TestSuite) TestL2ShutdownErrorSendPreemptible() {
	t := suite.T()
	writtenC := make(chan []byte, 8)
	ocppj.SetMessageIdGenerator(l2SequentialMessageIds("l2t2"))
	defer func() { ocppj.SetMessageIdGenerator(suite.messageIdGenerator.generateId) }()

	l2StartStandaloneChargingStation(suite, writtenC)

	// Errors() obtained, never drained.
	_ = suite.chargingStation.Errors()

	// First no-callback error: lands in cs.errC's empty cap-1 buffer without
	// blocking. Safe to deliver synchronously - cs.errorHandler and cs.errC
	// both start empty, so nothing on this path can block yet.
	phantomID1 := "l2t2-phantom-1"
	suite.ocppjClient.RequestState.AddPendingRequest(phantomID1, availability.NewHeartbeatRequest())
	require.NoError(t, suite.clientRequestQueue.Push(ocppj.RequestBundle{Call: &ocppj.Call{UniqueId: phantomID1}}))
	errorJson1 := fmt.Sprintf(`[4,"%v","%v","%v",{}]`, phantomID1, ocppj.GenericError, "no callback registered 1")
	err := suite.mockWsClient.MessageHandler([]byte(errorJson1))
	require.NoError(t, err)

	// Second no-callback error: delivered on its own goroutine since its
	// send onto cs.errorHandler (cap 1) may transiently block until the
	// handler has dequeued the first message - never call this synchronously
	// and unbounded from the test goroutine. Once delivered, the handler
	// dequeues it, finds no callback, and calls cs.error(err2) - which
	// blocks (errC already holds err1, unread, single-goroutine program
	// order guarantees err1's send completed first). The handler is now
	// permanently parked INSIDE error().
	phantomID2 := "l2t2-phantom-2"
	suite.ocppjClient.RequestState.AddPendingRequest(phantomID2, availability.NewHeartbeatRequest())
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
		t.Fatal("timed out delivering the second no-callback error onto cs.errorHandler")
	}

	// Positive confirmation the handler is parked INSIDE error() - and, by
	// construction (no request was ever dispatched/timed out/canceled in
	// this test), NOT wedged via the cancel-hook mechanism.
	l2WaitForGoroutineCountAtLeast(t, "(*chargingStation).error(", 1)

	boundedStop(t, suite.chargingStation)

	// The parked handler goroutine must be released - only a preemptible
	// `select { case cs.errC <- err: case <-stopC: }` inside error() can
	// unwedge it.
	l2WaitForGoroutineCountAtMost(t, "(*chargingStation).error(", 0)
}

// ============================================================================
// Test 3 - readPump-leak regression (spec §L2 "wider hazard" / fable
// MAJOR-2), targeting cs.responseHandler (v2.go:291-293).
// ============================================================================

// TestL2ShutdownReadPumpForwardingLeak pins the spec's "wider hazard" on 2.0.1's
// response-forwarding closure. Unlike the sibling 1.6 test, this needs no
// testhooks seam: R1's CALL_RESULT is delivered SYNCHRONOUSLY on the test
// goroutine (its forward into the empty cap-1 cs.responseHandler buffer
// completes immediately, deterministically, without any reader — a buffered
// send never needs one), which deterministically leaves that buffer full and
// unread (asyncCallbackHandler is pinned in R0's callback and can never
// drain it). R2's CALL_RESULT is then delivered on its own goroutine — its
// forward finds the buffer already full, with no reader ever coming, and
// blocks forever: a permanent goroutine leak.
//
// RED (today): after Stop() (which itself returns — it does not depend on
// the readPump-analog goroutine or the handler at all) and a bounded wait,
// the leaked goroutine's count does not return to baseline —
// l2WaitForGoroutineCountAtMost times out via t.Fatal.
func (suite *OcppV2TestSuite) TestL2ShutdownReadPumpForwardingLeak() {
	t := suite.T()

	// Baseline BEFORE any goroutine from this test exists.
	baseline := countGoroutinesByStack("NewChargingStation.func")

	writtenC := make(chan []byte, 8)
	ocppj.SetMessageIdGenerator(l2SequentialMessageIds("l2t3"))
	defer func() { ocppj.SetMessageIdGenerator(suite.messageIdGenerator.generateId) }()

	l2StartStandaloneChargingStation(suite, writtenC)

	gateC := make(chan struct{})
	defer close(gateC)

	// --- R0: pin the sole drainer forever (in today's code — no preemption exists). ---
	pinnedC := make(chan struct{})
	err := suite.chargingStation.SendRequestAsync(availability.NewHeartbeatRequest(), func(response ocpp.Response, err error) {
		close(pinnedC)
		<-gateC
	})
	require.NoError(t, err)
	l2WaitForWrite(t, writtenC, "timed out waiting for R0 to be written")
	err = suite.mockWsClient.MessageHandler([]byte(l2HeartbeatCallResultJson("l2t3-0")))
	require.NoError(t, err)
	l2WaitOrFail(t, pinnedC, "timed out waiting for the async handler to be pinned on R0's callback")

	// --- R1: dispatched, then its CALL_RESULT delivered synchronously — its
	// forward fills the empty cs.responseHandler buffer and returns without
	// ever needing a reader. ---
	err = suite.chargingStation.SendRequestAsync(availability.NewHeartbeatRequest(), func(response ocpp.Response, err error) {})
	require.NoError(t, err)
	l2WaitForWrite(t, writtenC, "timed out waiting for R1 to be written")
	err = suite.mockWsClient.MessageHandler([]byte(l2HeartbeatCallResultJson("l2t3-1")))
	require.NoError(t, err)

	// --- R2: auto-dispatched now that R1 completed at the dispatcher level
	// (CompleteRequest runs before the facade's response handler is
	// invoked). Its CALL_RESULT is delivered on its own goroutine: this
	// forward finds cs.responseHandler already full (R1's envelope, still
	// unread) and blocks forever. ---
	err = suite.chargingStation.SendRequestAsync(availability.NewHeartbeatRequest(), func(response ocpp.Response, err error) {})
	require.NoError(t, err)
	l2WaitForWrite(t, writtenC, "timed out waiting for R2 to be written")

	r2DoneC := make(chan struct{})
	go func() {
		defer close(r2DoneC)
		_ = suite.mockWsClient.MessageHandler([]byte(l2HeartbeatCallResultJson("l2t3-2")))
	}()

	// Wait for R2's forward to actually become permanently blocked (i.e. its
	// goroutine shows up in the stack dump) BEFORE calling Stop(). This is
	// load-bearing, not cosmetic: Stop()'s dispatcher drain
	// (ClearPendingRequests, ocppj/dispatcher.go:308) would otherwise race
	// R2's own CALL_RESULT processing - if the drain wins, R2's message id is
	// no longer a recognized pending request by the time ParseMessage checks
	// it, so the delivery fails fast (no forward attempted at all) instead of
	// reproducing the leak this test targets.
	l2WaitForGoroutineCountAtLeast(t, "NewChargingStation.func", baseline+1)

	// Positive control: confirm the stack-substring filter actually matches
	// something before relying on it for the RED assertion below. R2's
	// forward is blocked inside NewChargingStation's forwarding closure right
	// now, so this must find at least one - if a future refactor
	// renames/extracts that closure, this filter would match nothing and the
	// leak assertion below would pass VACUOUSLY (0 <= baseline is trivially
	// true).
	if got := countGoroutinesByStack("NewChargingStation.func"); got < 1 {
		t.Fatalf("positive control failed: goroutine stack filter %q matched nothing (got %d) - the filter is stale", "NewChargingStation.func", got)
	}

	// Stop the charging station: it does not depend on the readPump-analog
	// goroutine or the handler at all, so it must return promptly regardless
	// of R2's forward being stuck.
	boundedStop(t, suite.chargingStation)

	// RED assertion: the leaked goroutine must eventually go away (return to
	// baseline) - proof that Stop() does NOT leave a forwarder permanently
	// parked. l2WaitForGoroutineCountAtMost fails via t.Fatal if the deadline
	// elapses first, which is exactly the RED signal for this defect: today
	// nothing preempts R2's blocked send (no stopC arm on the forwarding
	// closure), so the goroutine never exits and the count never returns to
	// baseline.
	l2WaitForGoroutineCountAtMost(t, "NewChargingStation.func", baseline)

	select {
	case <-r2DoneC:
	case <-time.After(e1bBound):
		t.Fatal("R2's forward never returned even though the goroutine count claims it settled")
	}
}

// TestL2ShutdownErrorForwardingLeak pins the ERROR forwarding closure
// (v2.go:295) - the sibling of the response closure (:292) covered above.
// PR-L2 item 2 mandates preemptibility at BOTH; without this test an
// implementation converting only the response closure goes green while an
// inbound CALL_ERROR racing Stop() still leaks the readPump forever.
//
// Recipe: pin the sole drainer so cs.errorHandler is never read, let phantom
// error #1 fill its cap-1 buffer, then block phantom error #2's closure.
//
// RED (today): the blocked closure's goroutine never returns to baseline.
func (suite *OcppV2TestSuite) TestL2ShutdownErrorForwardingLeak() {
	t := suite.T()

	baseline := countGoroutinesByStack("NewChargingStation.func")

	writtenC := make(chan []byte, 8)
	ocppj.SetMessageIdGenerator(l2SequentialMessageIds("l2t3b"))
	defer func() { ocppj.SetMessageIdGenerator(suite.messageIdGenerator.generateId) }()

	l2StartStandaloneChargingStation(suite, writtenC)

	gateC := make(chan struct{})
	defer close(gateC)

	// R0 pins the sole drainer, so cs.errorHandler is never read again.
	pinnedC := make(chan struct{})
	err := suite.chargingStation.SendRequestAsync(availability.NewHeartbeatRequest(), func(response ocpp.Response, err error) {
		close(pinnedC)
		<-gateC
	})
	require.NoError(t, err)
	l2WaitForWrite(t, writtenC, "timed out waiting for R0 to be written")
	err = suite.mockWsClient.MessageHandler([]byte(l2HeartbeatCallResultJson("l2t3b-0")))
	require.NoError(t, err)
	l2WaitOrFail(t, pinnedC, "timed out waiting for the async handler to be pinned on R0's callback")

	// Phantom CALL_ERROR #1: its ERROR closure fills the empty cs.errorHandler
	// and returns.
	phantomID1 := "l2t3b-phantom-1"
	suite.ocppjClient.RequestState.AddPendingRequest(phantomID1, availability.NewHeartbeatRequest())
	require.NoError(t, suite.clientRequestQueue.Push(ocppj.RequestBundle{Call: &ocppj.Call{UniqueId: phantomID1}}))
	err = suite.mockWsClient.MessageHandler([]byte(fmt.Sprintf(`[4,"%v","%v","%v",{}]`, phantomID1, ocppj.GenericError, "leak 1")))
	require.NoError(t, err)

	// Phantom CALL_ERROR #2: its ERROR closure finds cs.errorHandler full with
	// no reader ever coming, and blocks forever.
	phantomID2 := "l2t3b-phantom-2"
	suite.ocppjClient.RequestState.AddPendingRequest(phantomID2, availability.NewHeartbeatRequest())
	require.NoError(t, suite.clientRequestQueue.Push(ocppj.RequestBundle{Call: &ocppj.Call{UniqueId: phantomID2}}))
	blockedC := make(chan struct{})
	go func() {
		defer close(blockedC)
		_ = suite.mockWsClient.MessageHandler([]byte(fmt.Sprintf(`[4,"%v","%v","%v",{}]`, phantomID2, ocppj.GenericError, "leak 2")))
	}()

	// Load-bearing (same reason as the response-closure test): park before
	// Stop(), else the dispatcher drain clears the pending id first and the
	// delivery fails fast without attempting the forward.
	l2WaitForGoroutineCountAtLeast(t, "NewChargingStation.func", baseline+1)

	boundedStop(t, suite.chargingStation)
	l2WaitForGoroutineCountAtMost(t, "NewChargingStation.func", baseline)
	select {
	case <-blockedC:
	case <-time.After(l2Bound):
		t.Fatal("the blocked error forward never returned even though the goroutine count claims it settled")
	}
}

// ============================================================================
// Test 4 - 1.6/2.0.1 parity regression guards (spec table: 2.0.1 already has
// stopOnce + nil guards; TestStopBeforeStartDoesNotPanic/
// TestDoubleStopDoesNotPanic in stop_lifecycle_test.go already pin these and
// already pass today). Renamed here (not duplicated) only to additionally
// pin them under the PR-L2 name for cross-facade parity tracking - see the
// sibling 1.6 file's TestL2ShutdownDoubleStopDoesNotPanic/
// TestL2ShutdownStopBeforeStartDoesNotPanic, which are RED on 1.6 today.
// ============================================================================

// TestL2ShutdownDoubleStopDoesNotPanicParity is a NEGATIVE regression guard, not a
// RED-first test: it already passes today (cs.Stop() has had a stopOnce +
// nil guard since PR-E1b). It exists to pin cross-facade parity with the
// sibling (currently RED) 1.6 guard, so a future regression on either facade
// is caught.
func (suite *OcppV2TestSuite) TestL2ShutdownDoubleStopDoesNotPanicParity() {
	t := suite.T()
	wsID := "test_id"
	wsURL := "someUrl"
	channel := NewMockWebSocket(wsID)

	setupDefaultChargingStationHandlers(suite, expectedChargingStationOptions{
		serverUrl:             wsURL,
		clientId:              wsID,
		createChannelOnStart:  false,
		channel:               channel,
		writeReturnArgument:   nil,
		forwardWrittenMessage: false,
	})
	suite.mockWsClient.On("Stop").Return()
	suite.mockWsClient.On("IsConnected").Return(false)

	err := suite.chargingStation.Start(wsURL)
	require.NoError(t, err)

	boundedStop(t, suite.chargingStation)
	boundedStop(t, suite.chargingStation)
}

// TestL2ShutdownStopBeforeStartDoesNotPanicParity is the Stop()-before-Start()
// counterpart of TestL2ShutdownDoubleStopDoesNotPanicParity - see that doc comment.
func (suite *OcppV2TestSuite) TestL2ShutdownStopBeforeStartDoesNotPanicParity() {
	t := suite.T()
	suite.mockWsClient.On("Stop").Return()
	suite.mockWsClient.On("IsConnected").Return(false)

	boundedStop(t, suite.chargingStation)
}

// ============================================================================
// Test 5 - restart / stopC field race (spec §PR-L2 item 1: "a synchronized
// stopC accessor... Prefer atomic.Value for stopC specifically").
// ============================================================================

// TestL2ShutdownRestartStopCRace loops Start -> Stop while a concurrent goroutine
// keeps sending requests that read cs.stopC (charging_station.go:533,
// `stopC := cs.stopC` inside SendRequestCtx) - racing Start's reassignment
// (:699, `cs.stopC = make(chan struct{}, 1)`) and Stop's close (via
// stopOnce, :721-723). Must be run with `go test -race` to surface the
// field race; without -race this test is expected to complete without
// incident (a data race is undefined behavior, not a guaranteed crash).
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
// happen within the loop's own cadence. The whole test is watchdog-bounded
// so a genuine deadlock (as opposed to a benign race) fails cleanly.
func (suite *OcppV2TestSuite) TestL2ShutdownRestartStopCRace() {
	t := suite.T()
	wsURL := "someUrl"
	suite.mockWsClient.On("Start", mock.AnythingOfType("string")).Return(nil)
	suite.mockWsClient.On("Write", mock.Anything).Return(nil)
	suite.mockWsClient.On("Stop").Return()
	suite.mockWsClient.On("IsConnected").Return(false)

	const iterations = 20

	trafficStop := make(chan struct{})
	trafficDone := make(chan struct{})
	go func() {
		defer close(trafficDone)
		for {
			select {
			case <-trafficStop:
				return
			default:
			}
			_, _ = suite.chargingStation.SendRequest(availability.NewHeartbeatRequest())
		}
	}()

	watchdogDone := make(chan struct{})
	go func() {
		defer close(watchdogDone)
		for i := 0; i < iterations; i++ {
			_ = suite.chargingStation.Start(wsURL)
			suite.chargingStation.Stop()
		}
	}()

	select {
	case <-watchdogDone:
	case <-time.After(e1bBound * 4):
		t.Fatal("Start/Stop loop did not complete within the bounded deadline")
	}
	close(trafficStop)

	select {
	case <-trafficDone:
	case <-time.After(e1bBound):
		t.Fatal("traffic goroutine did not exit within the bounded deadline")
	}
}
