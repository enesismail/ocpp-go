package ocpp2_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/enesismail/ocpp-go/ocpp"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/availability"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/types"
	"github.com/enesismail/ocpp-go/ocppj"
)

// This file covers tasks/s3b-2.0.1-ordering-unification.md ("S3b" - the
// remaining half of the long-deferred "S3 follow-up" that brings the
// ocpp2.0.1 charging-station facade to the same inbound-ordering guarantee
// the merged 1.6 S3 fix gave ocpp1.6).
//
// Relationship to the 1.6 original (ocpp1.6_test/inbound_ordering_test.go) -
// stated precisely, because "it's just a port" would be wrong:
//   - PORTED from it: the orderRecorder, the awaitBoundedYields method and its
//     determinism caveat, TestOrderingResponseBeforeInboundCall (1.6 :230),
//     and the two round-trip tests (1.6 :530, :574). Read that file for the
//     fully-derived rationale. 2.0.1's availability.ChangeAvailability stands
//     in for 1.6's core.RemoteStopTransaction as "some inbound CSMS->station
//     CALL".
//   - 2.0.1-ORIGINAL, with no 1.6 counterpart: TestOrderingErrorBeforeInboundCall
//     (the CALL_ERROR leg) and TestOrderingInboundCallOccupiesTheDrainer (the
//     structural blocking property).
//   - DELIBERATELY DIFFERENT from 1.6: R1's answer is delivered on its own
//     goroutine behind a bounded wait so a capacity-0 implementation fails
//     with a diagnostic, where 1.6 delivers inline and explicitly declines to
//     tolerate cap 0; and every gate here is released via sync.Once + defer so
//     no failure path can leak a pinned drainer into the rest of the suite.
//
// Today (pre-S3b), ocpp2.0.1/v2.go wires SetRequestHandler(cs.handleIncomingRequest)
// DIRECTLY, so an inbound CALL is handled INLINE on whichever goroutine
// delivers it (the ws read goroutine in production; the calling goroutine in
// this test harness, via MockWebsocketClient.MessageHandler) - independent
// of whatever cs.responseHandler/cs.errorHandler are doing. A CALL_RESULT for
// the station's own outbound request, by contrast, is queued to
// cs.responseHandler and dispatched by the single long-lived
// asyncCallbackHandler goroutine. There is no ordering guarantee between the
// two paths, so a CALL that arrives on the wire AFTER a CALL_RESULT can be
// handled BEFORE that result's response callback fires. S3b fixes this by
// routing all three kinds of inbound event (response, error, request)
// through the SAME ordered channel (cs.incoming), drained by the single
// asyncCallbackHandler goroutine - exactly mirroring the already-merged 1.6
// S3 fix.
//
// TestOrderingResponseBeforeInboundCall (spec "Tests to add" item 1) is the
// RED test. TestNoRegressionResponseRoundTrip and
// TestFacadeErrorCallbackRoundTrip (item 4) are cheap insurance that the
// channel merge does not regress the pre-existing response/error arms - both
// are expected GREEN today and green after.

// orderRecorder is a mutex-guarded ordered log. It lets the ordering test
// observe, from the test goroutine, the relative order in which two
// concurrently-processed events (a response callback and an inbound request
// handler) actually ran, without any of the recording sites calling into
// testify (which would be unsafe from a non-test goroutine). Mirrors
// ocpp1.6_test/inbound_ordering_test.go's orderRecorder (not importable here
// - different package).
type orderRecorder struct {
	mu    sync.Mutex
	order []string
}

func (r *orderRecorder) append(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.order = append(r.order, event)
}

func (r *orderRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// lightweightAvailabilityListener is a minimal availability.ChargingStationHandler
// implementation, deliberately NOT a testify mock: recording an inbound
// request's arrival must not carry incidental reflection/argument-matching
// overhead that has nothing to do with the property under test (ordering, in
// TestOrderingResponseBeforeInboundCall; a controlled panic, in
// panic_isolation_test.go's TestClientRequestHandlerPanicRecoveredFacade).
// availability.ChargingStationHandler has exactly one method, so - unlike
// 1.6's lightweightCoreListener - there are no other methods to stub with
// "unexpected call" failures.
type lightweightAvailabilityListener struct {
	onChangeAvailability func(request *availability.ChangeAvailabilityRequest) (*availability.ChangeAvailabilityResponse, error)
}

func (l *lightweightAvailabilityListener) OnChangeAvailability(request *availability.ChangeAvailabilityRequest) (*availability.ChangeAvailabilityResponse, error) {
	return l.onChangeAvailability(request)
}

// awaitBoundedYields cooperatively yields the processor (runtime.Gosched, NOT
// a wall-clock sleep) up to maxYields times, returning as soon as c is ready.
// Ported verbatim from ocpp1.6_test/inbound_ordering_test.go:129-143 (not
// importable here - different package); see
// TestOrderingResponseBeforeInboundCall below for the determinism caveat
// this exists to manage.
func awaitBoundedYields(c <-chan struct{}, maxYields int) {
	for i := 0; i < maxYields; i++ {
		select {
		case <-c:
			return
		default:
			runtime.Gosched()
		}
	}
}

// TestOrderingResponseBeforeInboundCall is spec item 1 - the point of S3b -
// ported from ocpp1.6_test/inbound_ordering_test.go's test of the same name
// (:230).
//
// Recipe:
//
//  1. The charging station sends R0 (a Heartbeat). Its CALL_RESULT is
//     delivered, so R0's response callback starts running on the
//     asyncCallbackHandler goroutine; that callback records "R0" and then
//     blocks on a test-controlled gate, pinning the goroutine.
//  2. The charging station sends R1 (another Heartbeat). R1's CALL_RESULT is
//     delivered on the (simulated) read goroutine, immediately followed by a
//     CSMS-initiated CALL (ChangeAvailability) delivered on its own goroutine
//     (delivering it inline could deadlock a correct post-S3b
//     implementation, which applies backpressure once cs.incoming fills up
//     while the consumer is pinned). R1's callback records "R1-response";
//     the ChangeAvailability handler records "request".
//  3. The gate is opened. The test waits (via channels, never time.Sleep)
//     until BOTH "R1-response" and "request" have been recorded, then asserts
//     the observed order.
//
// Pre-S3b, delivering the CALL runs handleIncomingRequest inline on whichever
// goroutine delivers it - independent of the gate - so "request" is recorded
// essentially immediately, while "R1-response" cannot be recorded until the
// gate opens and the pinned asyncCallbackHandler goroutine is freed to drain
// R1's parked confirmation. The expected (post-S3b) order
// ["R0", "R1-response", "request"] is therefore NOT what today's code
// produces: this test is expected to FAIL against the current implementation
// and to PASS once S3b unifies the response/error/request dispatch onto one
// ordered channel (cs.incoming).
func (suite *OcppV2TestSuite) TestOrderingResponseBeforeInboundCall() {
	t := suite.T()
	recorder := &orderRecorder{}
	writtenC := make(chan []byte, 8)
	ocppj.SetMessageIdGenerator(l2SequentialMessageIds("ord"))
	defer func() { ocppj.SetMessageIdGenerator(suite.messageIdGenerator.generateId) }()

	l2StartStandaloneChargingStation(suite, writtenC)
	// Captured once, on the test goroutine: the delivery goroutines below may
	// outlive a t.Fatal, and suite.mockWsClient is REASSIGNED by SetupTest
	// (ocpp2_test.go:1121), so reading the field from a leaked goroutine would
	// race with the next test's setup. Holding the pointer confines them to
	// this test's mock.
	wsClient := suite.mockWsClient

	changeAvailabilityConfirmation := availability.NewChangeAvailabilityResponse(availability.ChangeAvailabilityStatusAccepted)
	requestRecordedC := make(chan struct{})
	// Deliberately NOT a testify mock: recording "request" must not carry any
	// incidental argument-matching/reflection overhead that could tilt the
	// race this test is trying to observe. See the bounded-yield wait below
	// for why that race is, in practice, heavily biased toward observing the
	// pre-S3b ordering (not a hard guarantee, but far from a coin flip).
	listener := &lightweightAvailabilityListener{
		onChangeAvailability: func(request *availability.ChangeAvailabilityRequest) (*availability.ChangeAvailabilityResponse, error) {
			recorder.append("request")
			close(requestRecordedC)
			return changeAvailabilityConfirmation, nil
		},
	}
	suite.chargingStation.SetAvailabilityHandler(listener)

	// --- (a) send R0 and pin the async goroutine on its response callback ---
	// gateOnce/defer so an early t.Fatal below cannot leak the pinned
	// asyncCallbackHandler goroutine into the rest of the suite - and so the
	// bounded waits below fail CLEANLY rather than wedging (see the cap-0 note
	// at R1's CALL_RESULT delivery, which depends on this release).
	gateC := make(chan struct{})
	var gateOnce sync.Once
	closeGate := func() { gateOnce.Do(func() { close(gateC) }) }
	defer closeGate()

	pinnedC := make(chan struct{})
	err := suite.chargingStation.SendRequestAsync(availability.NewHeartbeatRequest(), func(response ocpp.Response, err error) {
		recorder.append("R0")
		close(pinnedC)
		<-gateC
	})
	require.NoError(t, err)

	l2WaitForWrite(t, writtenC, "timed out waiting for R0 to be written (never became a genuinely pending request)")
	// R0 was assigned the first id from our deterministic generator: the
	// client dispatcher only ever has one request in flight at a time, so R0
	// got "ord-0" and (below) R1 will get "ord-1". Delivered inline: the
	// drainer is idle at this point, so this send cannot block at any capacity.
	err = wsClient.MessageHandler([]byte(l2HeartbeatCallResultJson("ord-0")))
	require.NoError(t, err)

	l2WaitOrFail(t, pinnedC, "timed out waiting for the async goroutine to be pinned on R0's response callback")

	// --- (b) send R1; deliver its CALL_RESULT, immediately followed by an inbound CALL ---
	respDoneC := make(chan struct{})
	err = suite.chargingStation.SendRequestAsync(availability.NewHeartbeatRequest(), func(response ocpp.Response, err error) {
		recorder.append("R1-response")
		close(respDoneC)
	})
	require.NoError(t, err)

	l2WaitForWrite(t, writtenC, "timed out waiting for R1 to be written (never became a genuinely pending request)")
	// R1's CALL_RESULT must be TAKEN UP before the CALL is delivered, so the
	// wire order under test is result-then-call. That relies on the inbound
	// channel having capacity >= 1 (post-S3b cs.incoming, mirroring 1.6's
	// "keep a small buffer, ~ current cap 1"; pre-S3b it lands in
	// cs.responseHandler, already cap 1) - the drainer is pinned on R0's gate
	// and cannot receive.
	//
	// Delivered on its own goroutine, NOT inline, purely so a capacity-0
	// implementation FAILS rather than WEDGES: an unbuffered send would block
	// forever on the goroutine that has to open the gate, hanging the test with
	// no diagnostic until the package timeout. On its own goroutine the wait
	// below times out at l2Bound and says why; the deferred closeGate then
	// releases the drainer, which drains the parked result and lets this
	// goroutine finish.
	respDeliveredC := make(chan struct{})
	go func() {
		_ = wsClient.MessageHandler([]byte(l2HeartbeatCallResultJson("ord-1")))
		close(respDeliveredC)
	}()
	select {
	case <-respDeliveredC:
	case <-time.After(l2Bound):
		t.Fatal("timed out delivering R1's CALL_RESULT: the inbound channel did not accept it while the drainer was pinned, so it has capacity 0 - S3b requires a small buffer (cap 1), matching the merged cs.incoming and 1.6's cp.incoming")
	}

	changeAvailabilityCallJson := fmt.Sprintf(`[2,"%v","%v",{"operationalStatus":"%v"}]`, "ord-changeavail", availability.ChangeAvailabilityFeatureName, availability.OperationalStatusOperative)
	reqDoneC := make(chan struct{})
	var reqErr error
	go func() {
		// Deliver the CALL on its own goroutine: a correct (post-S3b)
		// implementation applies backpressure on this call once cs.incoming
		// fills up behind the still-pinned goroutine, so this call may
		// legitimately block until the gate below is opened - it must not be
		// made inline on the goroutine that needs to open that gate.
		reqErr = wsClient.MessageHandler([]byte(changeAvailabilityCallJson))
		close(reqDoneC)
	}()

	// Give the (independent-of-the-gate, pre-S3b) CALL delivery above an
	// overwhelming chance to finish BEFORE the gate is opened, without ever
	// blocking indefinitely (which would deadlock a correct post-S3b
	// implementation, where this delivery cannot complete until the gate
	// opens). Right now the asyncCallbackHandler goroutine is genuinely
	// parked on a receive from the still-open gate and cannot make ANY
	// progress (in particular it cannot record "R1-response") until the gate
	// opens, so a bounded number of cooperative yields either (a) observes
	// requestRecordedC fire - meaning "request" was recorded while the other
	// side was still provably parked, i.e. pre-S3b behavior - or (b) exhausts
	// the budget because the CALL delivery is genuinely blocked (post-S3b
	// backpressure), in which case it is safe to open the gate and let the
	// shared channel's FIFO order decide.
	//
	// Determinism caveat, carried verbatim (in substance) from
	// ocpp1.6_test/inbound_ordering_test.go:332-341: this is a strong
	// PRACTICAL bound - empirically stable on 1.6 (15/15 runs failed
	// pre-S3) and independent of the shared channel's buffer capacity - but
	// NOT a formal guarantee: Go does not promise a runnable goroutine will
	// be scheduled within any fixed number of Gosched calls, so in principle
	// the yield budget could exhaust before a pre-S3b CALL delivery
	// finishes, producing a rare false green. We accept that in exchange for
	// never risking a hang on a correct (post-S3b) implementation; a
	// wall-clock sleep would trade this for a different unproven assumption
	// (how long is "long enough") while also slowing down every test run.
	// Run this test at -count=10 or higher to get a practical read on the
	// failure rate.
	awaitBoundedYields(requestRecordedC, 100000)

	// (c) open the gate.
	closeGate()

	l2WaitOrFail(t, respDoneC, "timed out waiting for R1's response callback")
	l2WaitOrFail(t, reqDoneC, "timed out waiting for the inbound CALL to be handled")
	assert.NoError(t, reqErr)

	assert.Equal(t, []string{"R0", "R1-response", "request"}, recorder.snapshot())
}

// TestNoRegressionResponseRoundTrip is spec item 4, ported from
// ocpp1.6_test/inbound_ordering_test.go's test of the same name (:530): a
// normal request/response round trip (the charging station sends a request,
// the "CSMS" replies, and the response callback fires with the right
// confirmation and no error) still works, unaffected by the S3b channel
// merge. Expected GREEN both before and after the change.
func (suite *OcppV2TestSuite) TestNoRegressionResponseRoundTrip() {
	t := suite.T()
	writtenC := make(chan []byte, 8)
	l2StartStandaloneChargingStation(suite, writtenC)

	currentTime := types.NewDateTime(time.Now())

	type asyncResult struct {
		confirmation ocpp.Response
		err          error
	}
	resultC := make(chan asyncResult, 1)
	err := suite.chargingStation.SendRequestAsync(availability.NewHeartbeatRequest(), func(response ocpp.Response, err error) {
		resultC <- asyncResult{confirmation: response, err: err}
	})
	require.NoError(t, err)

	l2WaitForWrite(t, writtenC, "timed out waiting for the heartbeat request to be written")

	callResultJson := fmt.Sprintf(`[3,"%v",{"currentTime":"%v"}]`, defaultMessageId, currentTime.FormatTimestamp())
	err = suite.mockWsClient.MessageHandler([]byte(callResultJson))
	require.NoError(t, err)

	select {
	case result := <-resultC:
		require.NoError(t, result.err)
		require.NotNil(t, result.confirmation)
		hbConfirmation, ok := result.confirmation.(*availability.HeartbeatResponse)
		require.True(t, ok)
		assertDateTimeEquality(t, currentTime, &hbConfirmation.CurrentTime)
	case <-time.After(l2Bound):
		t.Fatal("timed out waiting for the response callback")
	}
}

// TestFacadeErrorCallbackRoundTrip is spec item 4, ported from
// ocpp1.6_test/inbound_ordering_test.go's test of the same name (:574):
// guards the facade's default CALL_ERROR callback path - ocppj's error
// handler must feed the facade's inbound channel (cs.incoming, kind
// incomingError), and asyncCallbackHandler must invoke the original
// SendRequestAsync callback as callback(nil, err).
// Do not replace the ocppj client's error handler here; doing so would
// bypass the facade channel this test covers. Expected GREEN both before and
// after the change.
func (suite *OcppV2TestSuite) TestFacadeErrorCallbackRoundTrip() {
	t := suite.T()
	writtenC := make(chan []byte, 8)
	l2StartStandaloneChargingStation(suite, writtenC)

	type asyncResult struct {
		confirmation ocpp.Response
		err          error
	}
	resultC := make(chan asyncResult, 1)
	err := suite.chargingStation.SendRequestAsync(availability.NewHeartbeatRequest(), func(response ocpp.Response, err error) {
		resultC <- asyncResult{confirmation: response, err: err}
	})
	require.NoError(t, err)

	l2WaitForWrite(t, writtenC, "timed out waiting for the heartbeat request to be written")

	code := ocppj.GenericError
	description := "server rejected heartbeat"
	callErrorJson := fmt.Sprintf(`[4,"%v","%v","%v",{}]`, defaultMessageId, code, description)
	err = suite.mockWsClient.MessageHandler([]byte(callErrorJson))
	require.NoError(t, err)

	select {
	case result := <-resultC:
		require.Nil(t, result.confirmation)
		require.NotNil(t, result.err)
		ocppErr, ok := result.err.(*ocpp.Error)
		require.True(t, ok)
		assert.Equal(t, code, ocppErr.Code)
		assert.Equal(t, description, ocppErr.Description)
		assert.Equal(t, defaultMessageId, ocppErr.MessageId)
	case <-time.After(l2Bound):
		t.Fatal("timed out waiting for the error callback")
	}
}

// TestOrderingErrorBeforeInboundCall is the CALL_ERROR leg of the guarantee
// S3b claims. TestOrderingResponseBeforeInboundCall above pins only
// CALL_RESULT-before-CALL; the spec's promise is that inbound
// CALL/CALL_RESULT/CALL_ERROR are all handled in wire order, so without this
// test the PR would claim a three-way guarantee backed by a one-way test.
//
// It exists specifically to reject a HALF-DONE merge: unifying responses and
// requests onto cs.incoming while leaving cs.errorHandler as a separate
// select arm. Nothing forces errorHandler's removal (the white-box
// construction in ocpp2.0.1/context_clearcallbacks_test.go can be adapted
// half-way and still compile), that shape passes every other test in this
// file, and it leaves the CALL_ERROR-vs-CALL wire order broken - the same
// defect class S3b exists to fix, just on the error leg.
//
// Same recipe as the response test, with R1 answered by a CALL_ERROR instead
// of a CALL_RESULT. EXPECTED RED TODAY for the same reason: today the CALL is
// handled inline on the delivering goroutine while the sole drainer is
// pinned, so "request" is recorded before "R1-error".
func (suite *OcppV2TestSuite) TestOrderingErrorBeforeInboundCall() {
	t := suite.T()
	recorder := &orderRecorder{}
	writtenC := make(chan []byte, 8)
	ocppj.SetMessageIdGenerator(l2SequentialMessageIds("orderr"))
	defer func() { ocppj.SetMessageIdGenerator(suite.messageIdGenerator.generateId) }()

	l2StartStandaloneChargingStation(suite, writtenC)
	// See TestOrderingResponseBeforeInboundCall for why this is captured on the
	// test goroutine rather than read from the suite inside a goroutine.
	wsClient := suite.mockWsClient

	changeAvailabilityResponse := availability.NewChangeAvailabilityResponse(availability.ChangeAvailabilityStatusAccepted)
	requestRecordedC := make(chan struct{})
	listener := &lightweightAvailabilityListener{
		onChangeAvailability: func(request *availability.ChangeAvailabilityRequest) (*availability.ChangeAvailabilityResponse, error) {
			recorder.append("request")
			close(requestRecordedC)
			return changeAvailabilityResponse, nil
		},
	}
	suite.chargingStation.SetAvailabilityHandler(listener)

	// --- (a) send R0 and pin the async goroutine on its response callback ---
	// gateOnce/defer so an early t.Fatal below cannot leak the pinned
	// asyncCallbackHandler goroutine into the rest of the suite.
	gateC := make(chan struct{})
	var gateOnce sync.Once
	closeGate := func() { gateOnce.Do(func() { close(gateC) }) }
	defer closeGate()

	pinnedC := make(chan struct{})
	err := suite.chargingStation.SendRequestAsync(availability.NewHeartbeatRequest(), func(response ocpp.Response, err error) {
		recorder.append("R0")
		close(pinnedC)
		<-gateC
	})
	require.NoError(t, err)

	l2WaitForWrite(t, writtenC, "timed out waiting for R0 to be written")
	// Inline is safe here at any capacity: the drainer is still idle.
	err = wsClient.MessageHandler([]byte(l2HeartbeatCallResultJson("orderr-0")))
	require.NoError(t, err)
	l2WaitOrFail(t, pinnedC, "timed out waiting for the async goroutine to be pinned on R0's response callback")

	// --- (b) send R1; answer it with a CALL_ERROR, then deliver an inbound CALL ---
	errDoneC := make(chan struct{})
	err = suite.chargingStation.SendRequestAsync(availability.NewHeartbeatRequest(), func(response ocpp.Response, err error) {
		recorder.append("R1-error")
		close(errDoneC)
	})
	require.NoError(t, err)

	l2WaitForWrite(t, writtenC, "timed out waiting for R1 to be written")
	// Lands in cs.incoming (cap 1, empty - R0's response was already
	// dequeued when its callback pinned). This send completes without the
	// pinned consumer, so long as the channel is buffered; delivered on its
	// own goroutine so a capacity-0 implementation fails cleanly instead of wedging the test
	// goroutine that has to open the gate. See the same note (with the full
	// rationale) in TestOrderingResponseBeforeInboundCall.
	callErrorJson := fmt.Sprintf(`[4,"%v","%v","%v",{}]`, "orderr-1", ocppj.GenericError, "csms rejected heartbeat")
	errDeliveredC := make(chan struct{})
	go func() {
		_ = wsClient.MessageHandler([]byte(callErrorJson))
		close(errDeliveredC)
	}()
	select {
	case <-errDeliveredC:
	case <-time.After(l2Bound):
		t.Fatal("timed out delivering R1's CALL_ERROR: the inbound channel did not accept it while the drainer was pinned, so it has capacity 0 - S3b requires a small buffer (cap 1), matching the merged cs.incoming and 1.6's cp.incoming")
	}

	changeAvailabilityCallJson := fmt.Sprintf(`[2,"%v","%v",{"operationalStatus":"%v"}]`, "orderr-changeavail", availability.ChangeAvailabilityFeatureName, availability.OperationalStatusOperative)
	reqDoneC := make(chan struct{})
	var reqErr error
	go func() {
		// Own goroutine: post-S3b this delivery blocks on the now-full
		// cs.incoming until the gate below opens, so it must not run on the
		// goroutine that has to open that gate.
		reqErr = wsClient.MessageHandler([]byte(changeAvailabilityCallJson))
		close(reqDoneC)
	}()

	// See TestOrderingResponseBeforeInboundCall's determinism caveat - it
	// applies verbatim here. The drainer is provably parked on the still-open
	// gate, so an early return means "request" was recorded while nothing
	// could possibly have recorded "R1-error" (pre-S3b behaviour); budget
	// exhaustion means the delivery is genuinely blocked (post-S3b
	// backpressure) and it is safe to open the gate.
	awaitBoundedYields(requestRecordedC, 100000)

	closeGate()

	l2WaitOrFail(t, errDoneC, "timed out waiting for R1's error callback")
	l2WaitOrFail(t, reqDoneC, "timed out waiting for the inbound CALL to be handled")
	assert.NoError(t, reqErr)

	assert.Equal(t, []string{"R0", "R1-error", "request"}, recorder.snapshot())
}

// TestOrderingInboundCallOccupiesTheDrainer pins the STRUCTURAL property that
// makes S3b's guarantee hold, rather than an observed ordering: after the
// change, handling an inbound CALL occupies the one-and-only
// asyncCallbackHandler goroutine, so a CALL_RESULT that arrives later cannot
// be delivered until that handler returns.
//
// This is the test that rejects the wrong implementations the two
// ordering tests above cannot distinguish, because those two only ever
// observe the final recorded order:
//
//   - Two channels plus a drainer whose select PREFERS the response arm. It
//     satisfies "response before request" 100% of the time while still
//     violating wire order whenever a CALL precedes a CALL_RESULT. (An
//     unbiased two-arm select would flake and get caught by -count; a biased
//     one never fails.)
//   - A unified channel whose request arm spawns a goroutine per inbound CALL
//     (`case incomingRequest: go func(){...}()`). Ordering of the DISPATCH is
//     preserved, so the two tests above pass, but handling is concurrent
//     again - which is the whole defect.
//
// Both are killed here by blocking semantics rather than scheduling luck: if
// the request handler does not hold the drainer, the later response WILL be
// recorded while the handler is parked, and the negative assertion fails.
//
// EXPECTED RED TODAY: the inbound CALL is handled inline on the delivering
// goroutine, entirely independent of the drainer, so R1's response callback
// runs while the request handler is parked.
func (suite *OcppV2TestSuite) TestOrderingInboundCallOccupiesTheDrainer() {
	t := suite.T()
	recorder := &orderRecorder{}
	writtenC := make(chan []byte, 8)
	ocppj.SetMessageIdGenerator(l2SequentialMessageIds("ordocc"))
	defer func() { ocppj.SetMessageIdGenerator(suite.messageIdGenerator.generateId) }()

	l2StartStandaloneChargingStation(suite, writtenC)
	// See TestOrderingResponseBeforeInboundCall for why this is captured on the
	// test goroutine rather than read from the suite inside a goroutine.
	wsClient := suite.mockWsClient

	// Two gates: gate0 pins the drainer on R0's callback; reqGate pins
	// whatever goroutine ends up running the inbound request handler. Both
	// released via sync.Once + defer so no failure path leaks a goroutine.
	gate0C := make(chan struct{})
	var gate0Once sync.Once
	closeGate0 := func() { gate0Once.Do(func() { close(gate0C) }) }
	defer closeGate0()

	reqGateC := make(chan struct{})
	var reqGateOnce sync.Once
	closeReqGate := func() { reqGateOnce.Do(func() { close(reqGateC) }) }
	defer closeReqGate()

	changeAvailabilityResponse := availability.NewChangeAvailabilityResponse(availability.ChangeAvailabilityStatusAccepted)
	requestPinnedC := make(chan struct{})
	listener := &lightweightAvailabilityListener{
		onChangeAvailability: func(request *availability.ChangeAvailabilityRequest) (*availability.ChangeAvailabilityResponse, error) {
			recorder.append("request")
			close(requestPinnedC)
			<-reqGateC
			return changeAvailabilityResponse, nil
		},
	}
	suite.chargingStation.SetAvailabilityHandler(listener)

	// --- (a) pin the drainer on R0's callback ---
	pinnedC := make(chan struct{})
	err := suite.chargingStation.SendRequestAsync(availability.NewHeartbeatRequest(), func(response ocpp.Response, err error) {
		recorder.append("R0")
		close(pinnedC)
		<-gate0C
	})
	require.NoError(t, err)

	l2WaitForWrite(t, writtenC, "timed out waiting for R0 to be written")
	// Inline is safe here at any capacity: the drainer is still idle.
	err = wsClient.MessageHandler([]byte(l2HeartbeatCallResultJson("ordocc-0")))
	require.NoError(t, err)
	l2WaitOrFail(t, pinnedC, "timed out waiting for the async goroutine to be pinned on R0's response callback")

	// --- (b) deliver the inbound CALL FIRST, then R1's CALL_RESULT ---
	// Post-S3b the CALL fills the empty cs.incoming and returns without being
	// handled (the drainer is still pinned on R0). Today it is handled inline
	// here and parks on reqGate, so this delivery must be on its own
	// goroutine in BOTH regimes.
	changeAvailabilityCallJson := fmt.Sprintf(`[2,"%v","%v",{"operationalStatus":"%v"}]`, "ordocc-changeavail", availability.ChangeAvailabilityFeatureName, availability.OperationalStatusOperative)
	reqDoneC := make(chan struct{})
	var reqErr error
	go func() {
		reqErr = wsClient.MessageHandler([]byte(changeAvailabilityCallJson))
		close(reqDoneC)
	}()

	// Load-bearing: the CALL must be taken up by the delivery path BEFORE
	// R1's CALL_RESULT is delivered. Post-S3b both compete for the same empty
	// cap-1 cs.incoming slot, and if the result won it the drainer would take
	// the result first and deliver R1's callback - a FALSE RED against a
	// correct implementation. "Taken up" looks different in each regime, so
	// wait for whichever arrives: today the CALL is handled inline and parks
	// the handler (requestPinnedC); post-S3b the forward returns as soon as
	// it is buffered (reqDoneC).
	select {
	case <-requestPinnedC:
	case <-reqDoneC:
	case <-time.After(l2Bound):
		t.Fatal("timed out waiting for the inbound CALL to be taken up by the delivery path")
	}

	respDoneC := make(chan struct{})
	err = suite.chargingStation.SendRequestAsync(availability.NewHeartbeatRequest(), func(response ocpp.Response, err error) {
		recorder.append("R1-response")
		close(respDoneC)
	})
	require.NoError(t, err)
	l2WaitForWrite(t, writtenC, "timed out waiting for R1 to be written")

	respDeliveredC := make(chan struct{})
	go func() {
		// Own goroutine: post-S3b cs.incoming already holds the CALL, so this
		// forward blocks until the drainer takes it.
		_ = wsClient.MessageHandler([]byte(l2HeartbeatCallResultJson("ordocc-1")))
		close(respDeliveredC)
	}()

	// --- (c) release the drainer, then wait for the request handler to be pinned ---
	// Order matters: post-S3b the request handler cannot start until the
	// drainer is free, so waiting for requestPinnedC BEFORE opening gate0
	// would deadlock a correct implementation.
	closeGate0()
	l2WaitOrFail(t, requestPinnedC, "timed out waiting for the inbound request handler to be pinned")

	// --- (d) THE ASSERTION. With the request handler parked, R1's response
	// must NOT have been delivered - post-S3b the handler holds the only
	// goroutine that could deliver it. Bounded yields give a wrong
	// implementation every chance to deliver it (same caveat as the tests
	// above: a strong practical bound, not a formal one), then we require
	// that it did not.
	awaitBoundedYields(respDoneC, 100000)
	select {
	case <-respDoneC:
		t.Fatal("R1's response callback ran while the inbound request handler was still parked: handling the request did not occupy the sole asyncCallbackHandler goroutine, so inbound CALLs and CALL_RESULTs are still handled concurrently")
	default:
	}
	assert.Equal(t, []string{"R0", "request"}, recorder.snapshot(), "only R0 and the request may have been recorded while the request handler is parked")

	// --- (e) release the request handler; everything drains in order ---
	closeReqGate()
	l2WaitOrFail(t, reqDoneC, "timed out waiting for the inbound CALL to be handled")
	l2WaitOrFail(t, respDeliveredC, "timed out waiting for R1's CALL_RESULT to be delivered")
	l2WaitOrFail(t, respDoneC, "timed out waiting for R1's response callback")
	assert.NoError(t, reqErr)

	assert.Equal(t, []string{"R0", "request", "R1-response"}, recorder.snapshot())
}

// TestStopCtxUnwindsSyncSendFromRequestHandler pins the escape hatch that the
// SendRequest/SendRequestCtx godoc (v2.go) promises, and which nothing else
// covers: a synchronous SendRequest issued from INSIDE an inbound request
// handler cannot complete, because the only goroutine that could deliver its
// response is the one running the handler - so StopCtx must be able to unwind
// the whole chain.
//
// Why this needs its own test. S3b moved inbound request handling onto the
// single asyncCallbackHandler goroutine, which made this anti-pattern strictly
// worse: BEFORE, the handler ran on the ws readPump while the drainer sat idle,
// so the ocppj dispatcher's request timeout (defaultMessageTimeout, 30s) was
// still deliverable - onRequestTimeout forwarded a "Request timed out"
// GenericError, the idle drainer routed it by MessageId, and SendRequest
// returned an error. AFTER, that timeout error has no reader, so stopC (or
// ctx, for SendRequestCtx) is the ONLY thing that can release the send. That
// makes the unwind chain load-bearing where it previously was not, and it runs
// through four separate mechanisms that a future change could each break:
// awaitCtxResult's stopC arm, the handler returning, the drainer reaching its
// own stopC arm, and StopCtx's join on handlerDone.
//
// The assertions are deliberately end-to-end rather than just "StopCtx
// returned": a build where the drainer never exits would still let StopCtx
// return ctx.Err() on a bounded context, so requiring nil (a SUCCESSFUL join)
// is what proves the chain completed. Bounded throughout - a regressed build
// fails on the deadline instead of hanging.
func (suite *OcppV2TestSuite) TestStopCtxUnwindsSyncSendFromRequestHandler() {
	t := suite.T()
	writtenC := make(chan []byte, 8)
	ocppj.SetMessageIdGenerator(l2SequentialMessageIds("unwind"))
	defer func() { ocppj.SetMessageIdGenerator(suite.messageIdGenerator.generateId) }()

	l2StartStandaloneChargingStation(suite, writtenC)

	// The handler performs the forbidden synchronous send and records whatever
	// it eventually returns. sendReturnedC signals that the send unblocked, so
	// the assertion below can distinguish "StopCtx released it" from "StopCtx
	// returned while the send is still stuck".
	inHandlerC := make(chan struct{})
	sendReturnedC := make(chan struct{})
	var sendErr error
	changeAvailabilityResponse := availability.NewChangeAvailabilityResponse(availability.ChangeAvailabilityStatusAccepted)
	listener := &lightweightAvailabilityListener{
		onChangeAvailability: func(request *availability.ChangeAvailabilityRequest) (*availability.ChangeAvailabilityResponse, error) {
			close(inHandlerC)
			_, sendErr = suite.chargingStation.SendRequest(availability.NewHeartbeatRequest())
			close(sendReturnedC)
			return changeAvailabilityResponse, nil
		},
	}
	suite.chargingStation.SetAvailabilityHandler(listener)

	// Deliver the inbound CALL. The forwarding closure only buffers it into the
	// empty cap-1 cs.incoming and returns, so this does not block the caller;
	// the drainer then picks it up and enters the handler.
	changeAvailabilityCallJson := fmt.Sprintf(`[2,"%v","%v",{"operationalStatus":"%v"}]`, "unwind-changeavail", availability.ChangeAvailabilityFeatureName, availability.OperationalStatusOperative)
	err := suite.mockWsClient.MessageHandler([]byte(changeAvailabilityCallJson))
	require.NoError(t, err)

	l2WaitOrFail(t, inHandlerC, "timed out waiting for the inbound request handler to run")
	// Wait for the heartbeat to reach the wire: that proves the inner send got
	// all the way through dispatch and is genuinely parked awaiting a response,
	// rather than having failed early (which would make the assertions vacuous).
	l2WaitForWrite(t, writtenC, "timed out waiting for the handler's inner heartbeat to be written")

	// StopCtx with a bounded context: on a regressed build this returns
	// DeadlineExceeded rather than hanging the suite.
	ctx, cancel := context.WithTimeout(context.Background(), l2Bound)
	defer cancel()
	stopErr := suite.chargingStation.StopCtx(ctx)

	l2WaitOrFail(t, sendReturnedC, "timed out waiting for the handler's inner SendRequest to be released by StopCtx")
	require.NoError(t, stopErr, "StopCtx must complete a SUCCESSFUL join: the released handler returns, the drainer then reaches its own stopC arm and closes handlerDone")
	require.Error(t, sendErr, "the inner synchronous SendRequest must fail, not succeed - no goroutine could have delivered its response")
	assert.Contains(t, sendErr.Error(), "charging station stopped while awaiting response",
		"the inner send must report the stopC escape documented on SendRequest, not some other failure")
}
