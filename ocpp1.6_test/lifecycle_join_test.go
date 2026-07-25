package ocpp16_test

// PR-L1 (tasks/facade-lifecycle-hardening.md) RED-FIRST test suite for the
// OCPP 1.6 charge-point (client) facade's generation handshake / join.
//
// This file covers the tests from spec §Tests that exercise EXISTING API
// (Stop(), Errors(), Start()) and therefore COMPILE against today's tree,
// failing only at RUNTIME. The StopCtx tests (which reference an API that
// does not exist yet) live in the sibling
// lifecycle_join_stopctx_test.go, which is COMPILE-RED - kept separate so
// the runtime reds below remain independently observable.
//
// Reused helpers (same package, defined in lifecycle_shutdown_test.go /
// inbound_ordering_test.go): l2Bound, l2BoundedStop, l2CountGoroutinesByStack,
// l2WaitForGoroutineCountAtMost, l2WaitForGoroutineCountAtLeast,
// startStandaloneChargePoint, sequentialMessageIds, heartbeatCallResultJson,
// waitOrFail.

import (
	"fmt"
	"sync"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/enesismail/ocpp-go/ocpp"
	"github.com/enesismail/ocpp-go/ocpp1.6/core"
	"github.com/enesismail/ocpp-go/ocppj"
)

// ============================================================================
// Test 1 - restart hygiene: INTENTIONALLY NOT IMPLEMENTED.
// ============================================================================
//
// A TestL1RestartHygieneNoCrossGenerationDelivery test was written (a
// Start->Stop loop under concurrent traffic, asserting no two callbacks ever
// execute concurrently - callbacks run inline on the sole handler, so that is
// equivalent to two handlers co-draining) and then DELETED, for two measured
// reasons:
//
//  1. It could not produce a RED. Measured 6/6 green under -race pre-fix, even
//     after fixing a TOCTOU in its max-concurrency counter: the co-drain window
//     needs a handler delayed past a full Stop+Start, which this harness does
//     not open. A test that cannot fail pre-fix is not red-first coverage.
//  2. Post-fix its invariant is implied by TestL1HandlerJoinedAfterStop: if
//     Stop() joins the handler, only one handler is ever alive, so two
//     callbacks cannot run concurrently.
//
// Earlier versions also (a) used a "callback fired after the generation counter
// advanced" proxy that counted LEGITIMATE Stop-time cancellations and so would
// have stayed red against a correct fix, and (b) asserted absolute
// goroutine counts, which are unreachable in a full-suite run (the protocol
// suites Start ~64 times vs ~17 Stops with no TearDownTest) and would have been
// a permanent FALSE RED. If this is ever revived, it must avoid all three
// traps.

// ============================================================================
// Test 2 - handler joined (spec §Tests item 2, pins §2's join).
// ============================================================================

// TestL1HandlerJoinedAfterStop pins spec §2: after Stop() returns, the
// asyncCallbackHandler goroutine spawned by Start() must be GONE - not
// merely "eventually" gone, but joined before Stop() itself returns.
//
// Recipe: pin the handler inside a callback blocked on a gate. Call Stop()
// on its own goroutine and race it against a short window:
//   - If Stop() returns within the short window (RED, today): it did so
//     WITHOUT waiting for the pinned handler, which must therefore still be
//     observably running - proving Stop() does not join.
//   - If Stop() does NOT return within the short window (GREEN, once PR-L1's
//     join lands): it is correctly blocked waiting on the handler. Release
//     the gate, let Stop() finish, and confirm the handler is now gone.
//
// This design never wedges the watchdog in either state: the RED branch
// fails fast (the short window itself is the bound), and the GREEN branch
// always eventually releases the gate before waiting further.
func (suite *OcppV16TestSuite) TestL1HandlerJoinedAfterStop() {
	t := suite.T()
	suite.mockWsClient.On("Stop").Return()
	suite.mockWsClient.On("IsConnected").Return(false)

	writeC := make(chan []byte, 8)
	ocppj.SetMessageIdGenerator(sequentialMessageIds("l1join2"))
	defer func() { ocppj.SetMessageIdGenerator(suite.messageIdGenerator.generateId) }()

	// Delta-based from here: the protocol suites leak dozens of handler
	// goroutines (many Starts, few Stops, no TearDownTest), so absolute
	// counts like 0 are unreachable in a full-suite run and would be a
	// permanent FALSE RED against a correct implementation.
	joinBaseline := l2CountGoroutinesByStack("(*chargePoint).asyncCallbackHandler(")
	startStandaloneChargePoint(suite, writeC)

	gateC := make(chan struct{})
	var gateOnce sync.Once
	closeGate := func() { gateOnce.Do(func() { close(gateC) }) }
	defer closeGate()

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
	err = suite.mockWsClient.MessageHandler([]byte(heartbeatCallResultJson("l1join2-0")))
	require.NoError(t, err)
	waitOrFail(suite, pinnedC, "timed out waiting for the async handler to be pinned on R0's callback")

	l2WaitForGoroutineCountAtLeast(t, "(*chargePoint).asyncCallbackHandler(", joinBaseline+1)

	stopDoneC := make(chan struct{})
	go func() {
		defer close(stopDoneC)
		suite.chargePoint.Stop()
	}()

	// shortWindow bounds "did Stop() return WITHOUT joining?". 500ms is >10x
	// the plausible close(stopC)->handler-wakeup latency on any CI runner we
	// target, so a correct (joining) implementation blocks past it and a
	// non-joining one returns well inside it. Note the failure direction: a
	// correct (joining) implementation can NEVER trip the RED branch, so this
	// window cannot produce a false RED. A pathologically slow RED-state Stop
	// would instead fall into the join branch and produce a false GREEN - so
	// do not 'raise the window' chasing a phantom false red.
	const shortWindow = 500 * time.Millisecond
	select {
	case <-stopDoneC:
		if got := l2CountGoroutinesByStack("(*chargePoint).asyncCallbackHandler("); got < joinBaseline+1 {
			t.Fatalf("Stop() returned early AND the handler is already gone (got %d matching goroutines) - the assertion below would be vacuous", got)
		}
		t.Fatal("Stop() returned before the pinned handler goroutine exited - the join is missing (RED: today's Stop() does not wait for asyncCallbackHandler to finish)")
	case <-time.After(shortWindow):
		// GREEN path: Stop() is correctly blocked on the join. Release the
		// pinned callback so both it and Stop() can finish.
		closeGate()
		select {
		case <-stopDoneC:
		case <-time.After(l2Bound):
			t.Fatal("Stop() did not return after releasing the pinned handler (deadlock)")
		}
		l2WaitForGoroutineCountAtMost(t, "(*chargePoint).asyncCallbackHandler(", joinBaseline)
	}
}

// ============================================================================
// Test 3 - Stop-before-Start / Stop-after-failed-Start both return (spec
// §Tests item 3, pins §2's pre-closed handlerDone requirement).
// ============================================================================

// NOTE - Stop()-before-Start() is NOT re-pinned here: it is already covered by
// lifecycle_shutdown_test.go's TestL2ShutdownStopBeforeStartDoesNotPanic, and
// duplicating merged PR-L2 coverage is explicitly out of scope.
//
// It is worth recording WHY that existing guard matters to PR-L1, though: PR-L1
// introduces a new way for it to regress. If handlerDone were initialized only
// in Start (nil until then), a join doing
// `select { case <-handlerDone: case <-ctx.Done(): }` with context.Background()
// would select on two nil channels and hang forever. Spec §2 therefore mandates
// handlerDone be initialized PRE-CLOSED in the CONSTRUCTOR; the existing guard
// is what catches a regression of that.

// TestL1StopAfterFailedStartReturns is a forward-looking regression guard
// (like the sibling test above): it already passes today - Start()'s error
// path never spawns asyncCallbackHandler (charge_point.go's `if err == nil
// { go cp.asyncCallbackHandler() }` guard), so there is nothing for a
// (not-yet-existing) join to wait on. It is pinned here because it is the
// variant spec §2's "not only in Start's error path" phrasing calls out by
// name: once PR-L1 adds handlerDone/the join, this path MUST NOT become a
// hang - the pre-closed-in-constructor initialization is what keeps it that
// way.
func (suite *OcppV16TestSuite) TestL1StopAfterFailedStartReturns() {
	t := suite.T()
	suite.mockWsClient.On("Start", mock.AnythingOfType("string")).Return(fmt.Errorf("simulated start failure"))
	suite.mockWsClient.On("Stop").Return()
	suite.mockWsClient.On("IsConnected").Return(false)

	err := suite.chargePoint.Start("someUrl")
	require.Error(t, err)

	l2BoundedStop(t, suite.chargePoint)
}

// ============================================================================
// Test 5 - generation orphan, 1.6 Start path (spec §Tests item 5).
// ============================================================================

// TestL1GenerationOrphanStartWithoutStopDoesNotLeak pins spec §2's
// generation-orphan clause for ocpp1.6.Start: a second Start() call without
// an intervening Stop() must not leak the first generation's handler
// goroutine.
//
// RED (today): Start() unconditionally reassigns stopC/stopOnce and spawns
// a new handler (charge_point.go's Start). Generation-1's stopC becomes
// unreachable by any future Stop() (which closes only loadStopC()'s CURRENT
// value via the accessor) - so generation-1's handler is parked forever on
// a channel nobody will ever close, and Stop() only ever joins/closes
// generation-2. Deterministic; no scheduling luck required.
//
// The assertion is the spec's OBSERVABLE CONTRACT for the decided remedy
// ("close the previous generation first"): after the second Start there is AT
// MOST ONE handler alive. Note what is deliberately NOT asserted - that BOTH
// generations are briefly alive (baseline+2). That is a RED-state-only
// observable, not an invariant: under the decided remedy generation-1 is
// closed and joined by the second Start, so a correct implementation never
// reaches baseline+2 and asserting it would fail the very fix this test
// exists to demand.
//
// RED-state leak note: in today's code generation-1's handler is unreachable
// BY CONSTRUCTION (its stopC was overwritten), so this test cannot clean it
// up - that unreachability IS the defect being pinned. The leaked goroutine
// shares a stack signature with the ones sibling tests count, but every such
// assertion here and in lifecycle_shutdown_test.go is delta-based against a
// per-test baseline, so a constant bias is harmless. Post-fix there is no
// leak and the point is moot.
func (suite *OcppV16TestSuite) TestL1GenerationOrphanStartWithoutStopDoesNotLeak() {
	t := suite.T()
	wsURL := "someUrl"
	suite.mockWsClient.On("Start", mock.AnythingOfType("string")).Return(nil)
	suite.mockWsClient.On("Stop").Return()
	suite.mockWsClient.On("IsConnected").Return(false)

	baseline := l2CountGoroutinesByStack("(*chargePoint).asyncCallbackHandler(")

	err := suite.chargePoint.Start(wsURL)
	require.NoError(t, err)
	l2WaitForGoroutineCountAtLeast(t, "(*chargePoint).asyncCallbackHandler(", baseline+1)

	// Generation 2, deliberately WITHOUT an intervening Stop().
	//
	// No assertion here on purpose. "At most one handler alive" would be RACY
	// (immediately after Start returns, generation-2's goroutine may not be
	// scheduled yet, so the poll can observe the pre-spawn count and pass
	// vacuously), and "both alive" (baseline+2) would be RED-state-only and
	// would fail the very fix this test demands. The single sound observable is
	// the post-Stop one below: a correct remedy leaves NOTHING behind.
	err = suite.chargePoint.Start(wsURL)
	require.NoError(t, err)

	l2BoundedStop(t, suite.chargePoint)

	l2WaitForGoroutineCountAtMost(t, "(*chargePoint).asyncCallbackHandler(", baseline)
}

// TestL1GenerationOrphanSecondStartJoinsPreviousGeneration pins the OTHER HALF
// of spec §2's decided remedy: not merely "close the previous generation" but
// "reuse the §2 join for the implicit stop".
//
// Why the leak test above cannot cover this: a close-old-but-DON'T-join second
// Start() passes it (generation-1's handler exits eventually, and the post-Stop
// poll waits up to l2Bound), yet leaves live the exact hazard spec §2 cites -
// generation-1's clearCallbacks() draining generation-2's freshly registered
// callbacks, since cp.callbacks is shared across generations. Goroutine
// counting is structurally blind to that; only "did the second Start WAIT?" is
// not.
//
// Observable (directional, non-racy): pin generation-1's handler inside a gated
// callback, then call Start() again. A joining implementation BLOCKS until the
// gate is released; a close-only (or today's no-op) implementation returns
// immediately.
//
// RED (today): Start() neither closes nor joins the previous generation, so the
// second Start returns at once.
func (suite *OcppV16TestSuite) TestL1GenerationOrphanSecondStartJoinsPreviousGeneration() {
	t := suite.T()
	suite.mockWsClient.On("Start", mock.AnythingOfType("string")).Return(nil)
	suite.mockWsClient.On("Stop").Return()
	suite.mockWsClient.On("IsConnected").Return(false)

	writeC := make(chan []byte, 8)
	ocppj.SetMessageIdGenerator(sequentialMessageIds("l1orphjoin"))
	defer func() { ocppj.SetMessageIdGenerator(suite.messageIdGenerator.generateId) }()

	startStandaloneChargePoint(suite, writeC)

	gateC := make(chan struct{})
	var gateOnce sync.Once
	closeGate := func() { gateOnce.Do(func() { close(gateC) }) }
	defer closeGate()

	// Pin generation-1's handler inside a callback blocked on the gate.
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
	err = suite.mockWsClient.MessageHandler([]byte(heartbeatCallResultJson("l1orphjoin-0")))
	require.NoError(t, err)
	waitOrFail(suite, pinnedC, "timed out waiting for generation-1's handler to be pinned")

	// Generation 2, with generation-1's handler still pinned.
	secondStartDoneC := make(chan struct{})
	go func() {
		defer close(secondStartDoneC)
		_ = suite.chargePoint.Start("someUrl")
	}()

	// Same failure-direction reasoning as TestL1HandlerJoinedAfterStop: a
	// correct (joining) implementation cannot trip the RED branch.
	const shortWindow = 500 * time.Millisecond
	select {
	case <-secondStartDoneC:
		t.Fatal("the second Start() returned WITHOUT waiting for the pinned generation-1 handler - it neither closed nor joined the previous generation (spec §2's remedy is close-AND-join)")
	case <-time.After(shortWindow):
		// GREEN: correctly blocked on the implicit stop's join.
		closeGate()
		select {
		case <-secondStartDoneC:
		case <-time.After(l2Bound):
			t.Fatal("the second Start() did not return after releasing the pinned handler (deadlock)")
		}
	}

	l2BoundedStop(t, suite.chargePoint)
}

// ============================================================================
// Test 6 - Errors() lazy-creation race (spec §Tests item 6, pins §5).
// ============================================================================

// TestL1ErrorsLazyCreationRaceSameChannel pins spec §5: concurrent Errors()
// callers under -race must observe the SAME channel.
//
// RED (today, under -race): cp.errC is written unsynchronized by Errors()
// (`if cp.errC == nil { cp.errC = make(...) }`) - N goroutines racing that
// check-then-set is a data race the race detector flags outright, and (even
// without -race) two callers that both observe errC == nil before either
// writes would each construct and return a DIFFERENT channel, silently
// losing whichever one does not survive as cp.errC.
//
// -race-ONLY BY DESIGN (same convention as the merged
// TestL2ShutdownRestartStopCRace): the data race is what makes this
// deterministic, and only the race detector reports it reliably. In PLAIN mode
// the lost-channel assertion is PROBABILISTIC - measured failing roughly 1 run
// in 5 - so a plain `go test` green here proves NOTHING. `-race` is the gate.
func (suite *OcppV16TestSuite) TestL1ErrorsLazyCreationRaceSameChannel() {
	t := suite.T()
	suite.mockWsClient.On("Stop").Return()
	suite.mockWsClient.On("IsConnected").Return(false)

	writeC := make(chan []byte, 8)
	startStandaloneChargePoint(suite, writeC)
	defer l2BoundedStop(t, suite.chargePoint)

	// n=200, not 50: at 50 the plain-mode (no -race) lost-channel outcome was
	// asymmetric between facades (1.6 failed ~1 run in 5, 2.0.1 essentially
	// never), which invites the false conclusion that the race is 1.6-specific.
	// The structural defect is identical on both. -race remains the gate.
	const n = 200
	results := make([]<-chan error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = suite.chargePoint.Errors()
		}(i)
	}
	close(start)

	doneC := make(chan struct{})
	go func() {
		wg.Wait()
		close(doneC)
	}()
	select {
	case <-doneC:
	case <-time.After(l2Bound):
		t.Fatal("timed out waiting for concurrent Errors() callers to return")
	}

	first := results[0]
	for i := 1; i < n; i++ {
		require.Truef(t, first == results[i], "Errors() call %d returned a different channel than call 0 - the lazy-creation race lost one", i)
	}
}
