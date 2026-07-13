package ocpp2_test

import (
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/enesismail/ocpp-go/ocpp2.0.1/availability"
)

// e1bBound is the bounded deadline for PR-E1b tests. Every blocking assertion
// uses a select with this timeout so a missing (or broken) fix cannot hang
// the suite.
const e1bBound = 2 * time.Second

// stopper is the minimal interface boundedStop needs. ocpp2.ChargingStation
// satisfies it without this file having to import the ocpp2.0.1 package
// just for a type name.
type stopper interface {
	Stop()
}

// boundedStop calls s.Stop() on its own goroutine and waits for it to return
// within e1bBound before letting the test continue.
//
// This is FIX 2 from the PR-E1b test-file review: Stop() itself is not
// deadline-bounded anywhere in production code, so a deadlocking
// implementation would hang the whole test binary instead of failing a
// single test. A panic inside Stop() is recovered and reported via
// t.Fatalf, so it also surfaces as a clean test failure instead of crashing
// the process.
func boundedStop(t *testing.T, s stopper) {
	t.Helper()
	done := make(chan interface{}, 1)
	go func() {
		defer func() {
			done <- recover()
		}()
		s.Stop()
	}()
	select {
	case r := <-done:
		if r != nil {
			t.Fatalf("Stop() panicked: %v", r)
		}
	case <-time.After(e1bBound):
		t.Fatal("Stop() did not return within the bounded deadline (deadlock)")
	}
}

// countGoroutinesByStack returns the number of currently running goroutines
// whose stack trace contains substr. Unlike runtime.NumGoroutine() (a raw,
// process-wide count), this lets a test track ONE specific goroutine (here,
// chargingStation's asyncCallbackHandler) without being flaky under
// `-race -count=N`, where unrelated goroutines elsewhere in the suite start
// and exit independently of the code under test.
func countGoroutinesByStack(substr string) int {
	size := 64 * 1024
	for {
		buf := make([]byte, size)
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), substr)
		}
		size *= 2
		if size > 64*1024*1024 {
			// Safety cap: report on a possibly-truncated dump rather than
			// growing forever.
			return strings.Count(string(buf[:n]), substr)
		}
	}
}

// waitForGoroutineCount polls (bounded by e1bBound) until the number of
// goroutines whose stack trace contains substr equals want, failing the
// test cleanly via t.Fatal if the deadline elapses first — so a goroutine
// leak (or a slow start) cannot hang the suite.
func waitForGoroutineCount(t *testing.T, substr string, want int) {
	t.Helper()
	deadline := time.After(e1bBound)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if got := countGoroutinesByStack(substr); got == want {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatalf("timed out waiting for goroutine count matching %q to reach %d (currently %d)",
				substr, want, countGoroutinesByStack(substr))
		}
	}
}

// TestStopUnblocksBlockedSyncSendRequest is the primary RED-first guard for
// PR-E1b. It verifies that chargingStation.Stop() unblocks a synchronous
// SendRequest that is blocked waiting for a response that never arrives.
//
// The test arranges for the dispatcher's Stop-drain path NOT to fire a
// cancel (by calling CompleteRequest first to pop the request from the
// queue), so the ONLY mechanism that can unblock SendRequest is the stopC
// select arm added by PR-E1b. Against the current code (no stopC arm), the
// goroutine blocks forever and the bounded deadline fires → RED.
//
// The "request was written" signal is a deterministic channel written to
// from the mock Write's Run closure — NOT a sleep. A sleep-based version
// would let CompleteRequest silently no-op (log+return, no pop) if the
// dispatcher hadn't actually dispatched yet; Stop's drain would then still
// find the request in the queue, fire a cancel, and unblock SendRequest via
// the EXISTING drain->errorHandler->callback path regardless of whether the
// stopC arm exists — a false pass that would hide a missing fix. That is
// why Write is set up here directly instead of via
// setupDefaultChargingStationHandlers' built-in (unsignalled) Write mock.
func (suite *OcppV2TestSuite) TestStopUnblocksBlockedSyncSendRequest() {
	t := suite.T()
	wsURL := "someUrl"

	suite.mockWsClient.On("Start", mock.AnythingOfType("string")).Return(nil)

	// Write succeeds but never forwards a response, so SendRequest blocks on
	// its internal <-asyncResponseC. The Run closure signals `written`
	// (non-blocking) so the test can wait deterministically for dispatch.
	written := make(chan struct{}, 1)
	suite.mockWsClient.On("Write", mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		select {
		case written <- struct{}{}:
		default:
		}
	})

	// Mocks needed for the Stop path. IsConnected=false avoids client.Stop()
	// blocking on <-cleanupC waiting for a disconnect signal.
	suite.mockWsClient.On("Stop").Return()
	suite.mockWsClient.On("IsConnected").Return(false)

	// Start the charging station.
	err := suite.chargingStation.Start(wsURL)
	require.Nil(t, err)
	// Capture station locally to avoid racing suite field access from the
	// goroutine below.
	station := suite.chargingStation

	// --- Send a request in a goroutine; it will block ---
	type sendResult struct {
		resp interface{}
		err  error
	}
	resultC := make(chan sendResult, 1)
	go func() {
		resp, err := station.SendRequest(availability.NewHeartbeatRequest())
		resultC <- sendResult{resp: resp, err: err}
	}()

	// Deterministically wait for the request to actually be written (i.e.
	// dispatched to the front of the queue and marked as the pending
	// request), instead of guessing with a sleep. If this never fires, the
	// test fails cleanly rather than hanging the suite.
	select {
	case <-written:
	case <-time.After(e1bBound):
		t.Fatal("request was never written — dispatcher never dispatched the request")
	}

	// --- Pop the request from the dispatcher queue via CompleteRequest ---
	// The Heartbeat above used the suite's pinned message-id generator, so
	// its UniqueId is defaultMessageId, matching the front-of-queue entry.
	// This simulates a scenario where the dispatcher has completed the
	// request (e.g. a response raced in) but the facade callback was never
	// invoked. After this, the dispatcher Stop-drain will find an empty
	// queue and therefore NOT fire a cancel — the ONLY thing left that can
	// unblock the send goroutine is the stopC arm added by PR-E1b.
	suite.clientDispatcher.CompleteRequest(defaultMessageId)

	// --- Stop the charging station (bounded: must not hang the suite) ---
	boundedStop(t, station)

	// --- Assert SendRequest unblocks within the bounded deadline ---
	select {
	case result := <-resultC:
		assert.NotNil(t, result.err, "SendRequest should return an error after Stop")
		assert.Nil(t, result.resp, "SendRequest should not return a response after Stop")
	case <-time.After(e1bBound):
		t.Fatal("SendRequest did not unblock on Stop — stopC arm missing (PR-E1b RED)")
	}
}

// TestStopWithOutstandingRequestsDoesNotDeadlock verifies that Stop() itself
// returns promptly, and unblocks BOTH callers, when multiple requests are
// still outstanding in the dispatcher queue at the moment Stop is called —
// unlike the other tests in this file, which pop/empty the queue first. Do
// NOT pre-empty the queue here: the outstanding requests are the point.
//
// This specifically guards the close(cs.stopC)-AFTER-cs.client.Stop()
// ordering: the dispatcher's Stop-drain fires a cancel for EVERY outstanding
// request, and each cancel does a BLOCKING send to the cap-1 cs.errorHandler
// channel. With two or more outstanding requests, the second (and later)
// blocking sends can only complete if asyncCallbackHandler is still alive to
// drain the channel. If a broken implementation closed cs.stopC BEFORE
// cs.client.Stop() (instead of after), the handler could exit early via its
// `case <-cs.stopC: return` arm, and the second blocking send would then
// wedge forever with no reader — hanging Stop() itself, not just the
// caller's SendRequest. A single outstanding request would NOT catch this
// (a lone send to an empty cap-1 channel never needs a reader to succeed),
// which is why this test deliberately queues two.
//
// NOTE on expected status: today (pre-PR-E1b), Stop() never touches stopC at
// all, so this specific ordering bug cannot yet manifest — the existing
// drain -> errorHandler -> asyncCallbackHandler -> callback chain already
// works, and both requests unblock normally. This test is therefore a
// forward-looking regression guard (like TestStopBeforeStartDoesNotPanic /
// TestDoubleStopDoesNotPanic below): it is expected to PASS today, and it
// exists to catch a specific wrong-order PR-E1b implementation.
func (suite *OcppV2TestSuite) TestStopWithOutstandingRequestsDoesNotDeadlock() {
	t := suite.T()
	wsURL := "someUrl"

	suite.mockWsClient.On("Start", mock.AnythingOfType("string")).Return(nil)

	// Write succeeds but never forwards a response, so every SendRequest
	// blocks. The written channel deterministically confirms the FIRST
	// request reaches the front of the queue and is dispatched; the second
	// request is never written before Stop (the dispatcher only dispatches
	// one request at a time) and stays queued behind it — exactly the
	// "outstanding requests" scenario this test needs.
	written := make(chan struct{}, 1)
	suite.mockWsClient.On("Write", mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		select {
		case written <- struct{}{}:
		default:
		}
	})

	suite.mockWsClient.On("Stop").Return()
	suite.mockWsClient.On("IsConnected").Return(false)

	err := suite.chargingStation.Start(wsURL)
	require.Nil(t, err)
	station := suite.chargingStation

	type sendResult struct {
		resp interface{}
		err  error
	}
	results := make(chan sendResult, 2)
	sendOne := func() {
		resp, err := station.SendRequest(availability.NewHeartbeatRequest())
		results <- sendResult{resp: resp, err: err}
	}

	go sendOne()

	select {
	case <-written:
	case <-time.After(e1bBound):
		t.Fatal("first request was never written")
	}

	go sendOne()

	// Give the second request a moment to reach the (unpopped, still-queued)
	// dispatcher queue behind the first. At this point the dispatcher pump
	// is idle: it dispatched the first request and has nothing else to do
	// until CompleteRequest/Resume/Stop, so there is no concurrent consumer
	// racing to drain the queue early. The only uncertainty is
	// goroutine-scheduling latency for a few fast, in-memory, uncontended
	// calls (a queue Push and a buffered channel send), which this
	// generously outlasts. There is no production hook to observe the push
	// directly without touching production code (out of scope for this
	// file), so the vacuous-pass guard below acts as a cheap, safe backstop
	// in case this window is ever too tight.
	time.Sleep(100 * time.Millisecond)

	// --- Stop with BOTH requests still outstanding ---
	boundedStop(t, station)

	// --- Both SendRequests must unblock within the bounded deadline ---
	for i := 0; i < 2; i++ {
		select {
		case result := <-results:
			assert.NotNil(t, result.err, "SendRequest should return an error after Stop")
			assert.Nil(t, result.resp, "SendRequest should not return a response after Stop")
			if result.err != nil {
				// Guard against a vacuous pass: if a request never actually
				// reached the dispatcher queue before Stop ran (e.g. a
				// scheduling fluke in the settle window above), it would
				// fail fast with this EXACT pre-existing "not started"
				// error from ocppj.Client.SendRequest instead of
				// exercising the outstanding-request drain path this test
				// targets.
				assert.NotEqual(t, "ocppj client is not started, couldn't send request", result.err.Error(),
					"a request appears to have raced ahead of Stop instead of being queued — test setup did not exercise 2 outstanding requests")
			}
		case <-time.After(e1bBound):
			t.Fatalf("SendRequest #%d did not unblock on Stop", i+1)
		}
	}
}

// TestStopBeforeStartDoesNotPanic is a NEGATIVE regression guard, not a
// standalone RED-first test: it already passes today, because production
// Stop() closes nothing yet. It exists to reject a naive, unguarded
// close(cs.stopC) once PR-E1b adds it — calling Stop() before Start() means
// cs.stopC is nil, and close(nil) panics without a nil-check guard.
func (suite *OcppV2TestSuite) TestStopBeforeStartDoesNotPanic() {
	t := suite.T()

	// Mocks needed for client.Stop() even when Start was never called.
	suite.mockWsClient.On("Stop").Return()
	suite.mockWsClient.On("IsConnected").Return(false)

	// Must not panic, and must not hang.
	boundedStop(t, suite.chargingStation)
}

// TestDoubleStopDoesNotPanic is a NEGATIVE regression guard, not a
// standalone RED-first test: it already passes today, because production
// Stop() closes nothing yet. It exists to reject a close(cs.stopC) with no
// once/flag guard once PR-E1b adds it — a second Stop() call would
// double-close the same channel and panic.
func (suite *OcppV2TestSuite) TestDoubleStopDoesNotPanic() {
	t := suite.T()
	wsID := "test_id"
	wsURL := "someUrl"
	channel := NewMockWebSocket(wsID)

	setupDefaultChargingStationHandlers(suite, expectedChargingStationOptions{
		serverUrl:             wsURL,
		clientId:              wsID,
		createChannelOnStart:  false, // No CSMS running; we only need the charging station
		channel:               channel,
		writeReturnArgument:   nil,
		forwardWrittenMessage: false,
	})

	// Mocks needed for Stop. Both calls share these expectations.
	suite.mockWsClient.On("Stop").Return()
	suite.mockWsClient.On("IsConnected").Return(false)

	// Start the charging station first.
	err := suite.chargingStation.Start(wsURL)
	require.Nil(t, err)

	// First Stop — must not panic or hang.
	boundedStop(t, suite.chargingStation)

	// Second Stop — must not panic or hang (double-close guard).
	boundedStop(t, suite.chargingStation)
}

// TestStopRestartLifecycle verifies that a full Start -> Stop -> Start ->
// Stop cycle neither panics nor hangs. Like the two NotPanics tests above,
// it is a forward-looking regression guard: it already passes today
// (production Stop() closes nothing), and it exists to reject a PR-E1b
// guard that does not RESET per Start. Start() re-creates cs.stopC on every
// call, so a permanent sync.Once (or an equivalent latch that is never
// reset) would satisfy TestDoubleStopDoesNotPanic yet silently no-op on the
// SECOND Stop() here, leaving the second cs.stopC never closed.
func (suite *OcppV2TestSuite) TestStopRestartLifecycle() {
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

	// First cycle.
	err := suite.chargingStation.Start(wsURL)
	require.Nil(t, err)
	boundedStop(t, suite.chargingStation)

	// Second cycle: Start() must re-create stopC, and Stop()'s guard must
	// allow closing THIS instance's stopC too, not just the first one.
	err = suite.chargingStation.Start(wsURL)
	require.Nil(t, err)
	boundedStop(t, suite.chargingStation)
}

// TestAsyncCallbackHandlerExitsAfterStop verifies that the
// asyncCallbackHandler goroutine exits after Stop(). PR-E1b closes stopC,
// which causes the handler's `case <-cs.stopC: return` arm to fire and the
// goroutine to exit cleanly.
//
// Instead of runtime.NumGoroutine() (a process-wide count that is flaky
// under `-race -count=N`, since unrelated goroutines elsewhere in the suite
// start and exit independently of the code under test), this counts only
// goroutines whose stack trace contains "asyncCallbackHandler" — a precise,
// robust signal for this one goroutine's lifecycle. Both waits are bounded
// by e1bBound with a clean t.Fatal on timeout, so a leak fails cleanly
// instead of hanging the suite.
//
// Against the current code (no close(stopC)), the handler survives Stop, so
// the post-Stop count never returns to baseline → RED.
func (suite *OcppV2TestSuite) TestAsyncCallbackHandlerExitsAfterStop() {
	t := suite.T()
	wsID := "test_id"
	wsURL := "someUrl"
	channel := NewMockWebSocket(wsID)

	setupDefaultChargingStationHandlers(suite, expectedChargingStationOptions{
		serverUrl:             wsURL,
		clientId:              wsID,
		createChannelOnStart:  false, // No CSMS running; we only need the charging station
		channel:               channel,
		writeReturnArgument:   nil,
		forwardWrittenMessage: false,
	})

	suite.mockWsClient.On("Stop").Return()
	suite.mockWsClient.On("IsConnected").Return(false)

	baseline := countGoroutinesByStack("asyncCallbackHandler")

	err := suite.chargingStation.Start(wsURL)
	require.Nil(t, err)

	// Bounded wait for the handler goroutine to appear after Start.
	waitForGoroutineCount(t, "asyncCallbackHandler", baseline+1)

	boundedStop(t, suite.chargingStation)

	// Bounded wait for the handler goroutine to exit after Stop.
	waitForGoroutineCount(t, "asyncCallbackHandler", baseline)
}
