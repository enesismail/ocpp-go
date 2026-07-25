package ocpp2_test

// PR-L1 (tasks/facade-lifecycle-hardening.md) RED-FIRST test suite for the
// OCPP 2.0.1 charging-station (client) facade's generation handshake / join.
//
// This file covers the tests from spec §Tests that exercise EXISTING API
// (Stop(), Errors(), Start(), StartWithRetries()) and therefore COMPILE
// against today's tree, failing only at RUNTIME. The StopCtx tests (which
// reference an API that does not exist yet) live in the sibling
// lifecycle_join_stopctx_test.go, which is COMPILE-RED - kept separate so
// the runtime reds below remain independently observable.
//
// Reused helpers (same package, defined in lifecycle_shutdown_test.go /
// stop_lifecycle_test.go): l2Bound, boundedStop, countGoroutinesByStack,
// l2WaitForGoroutineCountAtMost, l2WaitForGoroutineCountAtLeast,
// l2StartStandaloneChargingStation, l2SequentialMessageIds,
// l2HeartbeatCallResultJson, l2WaitOrFail, l2WaitForWrite, e1bBound, stopper.

import (
	"fmt"
	"sync"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/enesismail/ocpp-go/ocpp"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/availability"
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

// TestL1HandlerJoinedAfterStop mirrors the sibling 1.6 test exactly (see
// its doc comment for the full race-then-branch rationale): pin the handler
// on a gated callback, race Stop() against a short window, and either (RED,
// today) observe Stop() returning early while the handler is still alive,
// or (GREEN, once PR-L1's join lands) observe Stop() correctly blocked,
// release the gate, and confirm the handler is then gone.
func (suite *OcppV2TestSuite) TestL1HandlerJoinedAfterStop() {
	t := suite.T()
	writtenC := make(chan []byte, 8)
	ocppj.SetMessageIdGenerator(l2SequentialMessageIds("l1join2"))
	defer func() { ocppj.SetMessageIdGenerator(suite.messageIdGenerator.generateId) }()

	// Delta-based from here: the protocol suites leak dozens of handler
	// goroutines (many Starts, few Stops, no TearDownTest), so absolute
	// counts like 0 are unreachable in a full-suite run and would be a
	// permanent FALSE RED against a correct implementation.
	joinBaseline := countGoroutinesByStack("(*chargingStation).asyncCallbackHandler(")
	l2StartStandaloneChargingStation(suite, writtenC)

	gateC := make(chan struct{})
	var gateOnce sync.Once
	closeGate := func() { gateOnce.Do(func() { close(gateC) }) }
	defer closeGate()

	pinnedC := make(chan struct{})
	err := suite.chargingStation.SendRequestAsync(availability.NewHeartbeatRequest(), func(response ocpp.Response, err error) {
		close(pinnedC)
		<-gateC
	})
	require.NoError(t, err)
	l2WaitForWrite(t, writtenC, "timed out waiting for R0 to be written")
	err = suite.mockWsClient.MessageHandler([]byte(l2HeartbeatCallResultJson("l1join2-0")))
	require.NoError(t, err)
	l2WaitOrFail(t, pinnedC, "timed out waiting for the async handler to be pinned on R0's callback")

	l2WaitForGoroutineCountAtLeast(t, "(*chargingStation).asyncCallbackHandler(", joinBaseline+1)

	stopDoneC := make(chan struct{})
	go func() {
		defer close(stopDoneC)
		suite.chargingStation.Stop()
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
		if got := countGoroutinesByStack("(*chargingStation).asyncCallbackHandler("); got < joinBaseline+1 {
			t.Fatalf("Stop() returned early AND the handler is already gone (got %d matching goroutines) - the assertion below would be vacuous", got)
		}
		t.Fatal("Stop() returned before the pinned handler goroutine exited - the join is missing (RED: today's Stop() does not wait for asyncCallbackHandler to finish)")
	case <-time.After(shortWindow):
		closeGate()
		select {
		case <-stopDoneC:
		case <-time.After(l2Bound):
			t.Fatal("Stop() did not return after releasing the pinned handler (deadlock)")
		}
		l2WaitForGoroutineCountAtMost(t, "(*chargingStation).asyncCallbackHandler(", joinBaseline)
	}
}

// ============================================================================
// Test 3 - Stop-before-Start / Stop-after-failed-Start both return (spec
// §Tests item 3, pins §2's pre-closed handlerDone requirement).
// ============================================================================

// NOTE - Stop()-before-Start() is NOT re-pinned here: already covered by
// lifecycle_shutdown_test.go's TestL2ShutdownStopBeforeStartDoesNotPanicParity
// (and stop_lifecycle_test.go), and duplicating merged PR-L2 coverage is out of
// scope. See the sibling 1.6 file's note for why that guard matters to PR-L1
// (handlerDone must be pre-closed in the CONSTRUCTOR, or a Background-bounded
// join selects on two nil channels and hangs).

// TestL1StopAfterFailedStartReturns is a forward-looking regression guard
// (see the sibling 1.6 file's test of the same name for the full
// rationale): already passes today - Start()'s error path never spawns
// asyncCallbackHandler on 2.0.1 either (charging_station.go's Start has the
// same `if err == nil { go cs.asyncCallbackHandler(stopC) }` guard as
// 1.6), so there is nothing for a (not-yet-existing) join to wait on. The
// invariant under test (Stop() must not hang once PR-L1's join is keyed off
// a pre-closed-in-constructor handlerDone) is identical to 1.6's.
func (suite *OcppV2TestSuite) TestL1StopAfterFailedStartReturns() {
	t := suite.T()
	suite.mockWsClient.On("Start", mock.AnythingOfType("string")).Return(fmt.Errorf("simulated start failure"))
	suite.mockWsClient.On("Stop").Return()
	suite.mockWsClient.On("IsConnected").Return(false)

	err := suite.chargingStation.Start("someUrl")
	require.Error(t, err)

	boundedStop(t, suite.chargingStation)
}

// ============================================================================
// Test 5 - generation orphan, BOTH 2.0.1 Start paths (spec §Tests item 5:
// "all three paths" - ocpp1.6.Start is covered by the sibling 1.6 file;
// this file covers ocpp2.0.1.Start and ocpp2.0.1.StartWithRetries).
// ============================================================================

// TestL1GenerationOrphanStartWithoutStopDoesNotLeak pins spec §2's
// generation-orphan clause for ocpp2.0.1.Start - see the sibling 1.6 file's
// test of the same name for the full mechanism (identical: Start()
// unconditionally reassigns stopC/stopOnce and spawns a new handler,
// orphaning the previous generation's).
func (suite *OcppV2TestSuite) TestL1GenerationOrphanStartWithoutStopDoesNotLeak() {
	t := suite.T()
	wsURL := "someUrl"
	suite.mockWsClient.On("Start", mock.AnythingOfType("string")).Return(nil)
	suite.mockWsClient.On("Stop").Return()
	suite.mockWsClient.On("IsConnected").Return(false)

	baseline := countGoroutinesByStack("(*chargingStation).asyncCallbackHandler(")

	err := suite.chargingStation.Start(wsURL)
	require.NoError(t, err)
	l2WaitForGoroutineCountAtLeast(t, "(*chargingStation).asyncCallbackHandler(", baseline+1)

	// No assertion here on purpose: "at most one alive" is RACY (generation-2's
	// goroutine may not be scheduled yet, so the poll can pass vacuously) and
	// "both alive" (baseline+2) is a RED-state-only observable a correct fix
	// never reaches. The sound observable is the post-Stop one below.
	err = suite.chargingStation.Start(wsURL)
	require.NoError(t, err)

	boundedStop(t, suite.chargingStation)

	l2WaitForGoroutineCountAtMost(t, "(*chargingStation).asyncCallbackHandler(", baseline)
}

// l1MockStartWithRetries equips MockWebsocketClient (defined in
// ocpp2_test.go, the shared suite scaffold) with a StartWithRetries
// override. The scaffold does not define one - ws.Client is embedded there
// as a bare, zero-value interface for methods nobody has mocked yet, so an
// unmocked call promotes straight to that nil interface and panics (nil
// pointer dereference) rather than going through testify's mock.Mock
// machinery. Go permits a type's methods to be split across files within
// one package, so this is added here rather than editing the shared
// scaffold file (out of scope for this test-only change).
func (websocketClient *MockWebsocketClient) StartWithRetries(url string) {
	websocketClient.MethodCalled("StartWithRetries", url)
}

// TestL1GenerationOrphanStartWithRetriesWithoutStopDoesNotLeak is the
// StartWithRetries variant of the test above - spec §2 names
// ocpp2.0.1.StartWithRetries (charging_station.go) explicitly as the third
// of the "all three" Start* paths that must not orphan a generation.
func (suite *OcppV2TestSuite) TestL1GenerationOrphanStartWithRetriesWithoutStopDoesNotLeak() {
	t := suite.T()
	wsURL := "someUrl"
	suite.mockWsClient.On("StartWithRetries", mock.AnythingOfType("string")).Return()
	suite.mockWsClient.On("Stop").Return()
	suite.mockWsClient.On("IsConnected").Return(false)

	baseline := countGoroutinesByStack("(*chargingStation).asyncCallbackHandler(")

	suite.chargingStation.StartWithRetries(wsURL)
	l2WaitForGoroutineCountAtLeast(t, "(*chargingStation).asyncCallbackHandler(", baseline+1)

	// Same contract as the Start() variant; same reason for no intermediate
	// assertion. (StartWithRetries returns void - precisely why the decided
	// remedy is close-previous rather than reject-second-start: a rejection
	// cannot be reported through this signature.)
	suite.chargingStation.StartWithRetries(wsURL)

	boundedStop(t, suite.chargingStation)

	l2WaitForGoroutineCountAtMost(t, "(*chargingStation).asyncCallbackHandler(", baseline)
}

// TestL1GenerationOrphanStartWithRetriesJoinsPreviousGeneration pins the OTHER
// HALF of spec §2's remedy on the void-signature path - see the sibling 1.6
// test TestL1GenerationOrphanSecondStartJoinsPreviousGeneration for the full
// rationale (leak-counting cannot distinguish close-only from close-AND-join,
// and only close-AND-join prevents generation-1's clearCallbacks() from
// draining generation-2's callbacks).
//
// StartWithRetries is deliberately the facade chosen here: its void signature
// is what made "close the previous generation" the decided remedy over
// "reject a second start".
//
// RED (today): StartWithRetries neither closes nor joins the previous
// generation, so the second call returns at once.
func (suite *OcppV2TestSuite) TestL1GenerationOrphanStartWithRetriesJoinsPreviousGeneration() {
	t := suite.T()
	suite.mockWsClient.On("StartWithRetries", mock.AnythingOfType("string")).Return()
	suite.mockWsClient.On("Stop").Return()
	suite.mockWsClient.On("IsConnected").Return(false)

	writtenC := make(chan []byte, 8)
	ocppj.SetMessageIdGenerator(l2SequentialMessageIds("l1orphjoin"))
	defer func() { ocppj.SetMessageIdGenerator(suite.messageIdGenerator.generateId) }()

	l2StartStandaloneChargingStation(suite, writtenC)

	gateC := make(chan struct{})
	var gateOnce sync.Once
	closeGate := func() { gateOnce.Do(func() { close(gateC) }) }
	defer closeGate()

	pinnedC := make(chan struct{})
	err := suite.chargingStation.SendRequestAsync(availability.NewHeartbeatRequest(), func(response ocpp.Response, err error) {
		close(pinnedC)
		<-gateC
	})
	require.NoError(t, err)
	l2WaitForWrite(t, writtenC, "timed out waiting for R0 to be written")
	err = suite.mockWsClient.MessageHandler([]byte(l2HeartbeatCallResultJson("l1orphjoin-0")))
	require.NoError(t, err)
	l2WaitOrFail(t, pinnedC, "timed out waiting for generation-1's handler to be pinned")

	secondStartDoneC := make(chan struct{})
	go func() {
		defer close(secondStartDoneC)
		suite.chargingStation.StartWithRetries("someUrl")
	}()

	// Failure direction: a correct (joining) implementation cannot trip RED.
	const shortWindow = 500 * time.Millisecond
	select {
	case <-secondStartDoneC:
		t.Fatal("the second StartWithRetries() returned WITHOUT waiting for the pinned generation-1 handler - it neither closed nor joined the previous generation (spec §2's remedy is close-AND-join)")
	case <-time.After(shortWindow):
		closeGate()
		select {
		case <-secondStartDoneC:
		case <-time.After(e1bBound):
			t.Fatal("the second StartWithRetries() did not return after releasing the pinned handler (deadlock)")
		}
	}

	boundedStop(t, suite.chargingStation)
}

// ============================================================================
// Test 6 - Errors() lazy-creation race (spec §Tests item 6, pins §5).
// ============================================================================

// TestL1ErrorsLazyCreationRaceSameChannel mirrors the sibling 1.6 test -
// see its doc comment for the full rationale.
//
// -race-ONLY BY DESIGN: a plain `go test` green proves nothing here (the
// lost-channel assertion is probabilistic without the race detector). `-race`
// is the gate.
func (suite *OcppV2TestSuite) TestL1ErrorsLazyCreationRaceSameChannel() {
	t := suite.T()
	writtenC := make(chan []byte, 8)
	l2StartStandaloneChargingStation(suite, writtenC)
	defer boundedStop(t, suite.chargingStation)

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
			results[i] = suite.chargingStation.Errors()
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
