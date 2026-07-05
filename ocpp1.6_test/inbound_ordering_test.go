package ocpp16_test

import (
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/enesismail/ocpp-go/ocpp"
	"github.com/enesismail/ocpp-go/ocpp1.6/core"
	"github.com/enesismail/ocpp-go/ocpp1.6/localauth"
	"github.com/enesismail/ocpp-go/ocpp1.6/types"
	"github.com/enesismail/ocpp-go/ocppj"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// This file covers tasks/s3-inbound-ordering.md (S3 — client facade: preserve
// wire order between an inbound CALL and a preceding CALL_RESULT/CALL_ERROR).
//
// Today (pre-S3), ocpp1.6/charge_point.go dispatches a CALL_RESULT/CALL_ERROR
// for the charge point's own requests on the long-lived asyncCallbackHandler
// goroutine, while a server-initiated CALL (RemoteStartTransaction,
// RemoteStopTransaction, ...) is handled INLINE on whatever goroutine
// delivers the incoming websocket message (the ws read loop in production;
// the calling goroutine in this test harness, via
// MockWebsocketClient.MessageHandler). There is no ordering guarantee
// between the two paths, so a CALL that arrives on the wire AFTER a
// CALL_RESULT can be handled BEFORE that result's response callback fires
// (upstream issue #184). S3 fixes this by routing all three kinds of
// inbound event (response, error, request) through the SAME ordered channel,
// drained by the single asyncCallbackHandler goroutine.
//
// TestOrderingResponseBeforeInboundCall (test 1) is the RED test: it pins the
// asyncCallbackHandler goroutine on an earlier response's callback so that a
// following response + inbound CALL are both "in flight" while that goroutine
// cannot yet drain either one, then observes the actual order in which their
// user-visible effects occur. It fails against the current (pre-S3)
// implementation and is expected to pass once S3 lands.
//
// The remaining three tests guard against a botched S3 implementation:
// inbound requests must still be handled correctly (test 2), panic isolation
// on the (post-S3, moved) request-handling path must be preserved (test 3),
// and the existing response/error dispatch must show no regression (test 4).

// orderRecorder is a mutex-guarded ordered log. It lets the ordering test
// observe, from the test goroutine, the relative order in which two
// concurrently-processed events (a response callback and an inbound request
// handler) actually ran, without any of the recording sites calling into
// testify (which would be unsafe from a non-test goroutine).
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

// inboundOrderingWaitTimeout bounds how long a test waits for an async
// signal before failing, so a broken implementation (or a broken test)
// produces a fast, deterministic failure instead of hanging the suite.
const inboundOrderingWaitTimeout = 5 * time.Second

// startStandaloneChargePoint starts suite.chargePoint directly against the
// mocked websocket client, with no central-system mock in the loop. This lets
// a test fully control the exact bytes "received" by the charge point (via
// suite.mockWsClient.MessageHandler) and observe exactly what it writes back,
// without a second mock automatically forwarding messages. If writeC is
// non-nil, every raw message the charge point writes (every outgoing
// CALL/CALL_RESULT/CALL_ERROR) is pushed to it, in write order.
func startStandaloneChargePoint(suite *OcppV16TestSuite, writeC chan []byte) {
	t := suite.T()
	suite.mockWsClient.On("Start", mock.AnythingOfType("string")).Return(nil)
	suite.mockWsClient.On("Write", mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		data := args.Get(0).([]byte)
		if writeC != nil {
			cp := make([]byte, len(data))
			copy(cp, data)
			writeC <- cp
		}
	})
	err := suite.chargePoint.Start("someUrl")
	require.Nil(t, err)
}

// sequentialMessageIds returns a message-ID generator producing a fresh,
// distinguishable id ("<prefix>-0", "<prefix>-1", ...) on every call, so that
// multiple requests pending at different times in the same test can be
// individually addressed by the CALL_RESULT the test crafts for each of them.
func sequentialMessageIds(prefix string) func() string {
	n := -1
	return func() string {
		n++
		return fmt.Sprintf("%s-%d", prefix, n)
	}
}

// heartbeatCallResultJson builds a CALL_RESULT payload for a HeartbeatRequest
// sent by the charge point, addressed to the given (already-pending) request
// id.
func heartbeatCallResultJson(id string) string {
	return fmt.Sprintf(`[3,"%v",{"currentTime":"%v"}]`, id, types.NewDateTime(time.Now()).FormatTimestamp())
}

// waitOrFail receives from c, failing the test with msg if the wait exceeds
// inboundOrderingWaitTimeout. Used instead of time.Sleep for every
// synchronization point in this file.
func waitOrFail(suite *OcppV16TestSuite, c <-chan struct{}, msg string) {
	t := suite.T()
	select {
	case <-c:
	case <-time.After(inboundOrderingWaitTimeout):
		t.Fatal(msg)
	}
}

// awaitBoundedYields cooperatively yields the processor (runtime.Gosched, NOT
// a wall-clock sleep) up to maxYields times, returning as soon as c is ready.
// It gives a concurrently-runnable, CPU-bound goroutine an overwhelming - but
// NOT formally absolute - chance to finish before the caller gives up
// waiting: Go does not guarantee a runnable goroutine gets scheduled within
// any fixed number of Gosched calls, so in principle the budget could be
// exhausted before that goroutine completes. It never blocks indefinitely if
// c can never become ready (e.g. because its producer is legitimately parked
// elsewhere). See TestOrderingResponseBeforeInboundCall for why this bounded,
// cooperative-yield wait - rather than a wall-clock sleep - is the right
// tool there despite not being a hard guarantee: it never blocks a correct
// (post-S3) implementation, which parks delivery until the test's gate
// opens, while still giving a pre-S3 implementation's immediately-runnable
// goroutine every practical chance to complete first.
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

// lightweightCoreListener is a minimal core.ChargePointHandler implementation
// used only by the ordering test, deliberately NOT a testify mock: recording
// "request" must not carry incidental reflection/argument-matching overhead
// that has nothing to do with the ordering property under test. Only
// OnRemoteStopTransaction is exercised; the rest satisfy the interface and
// are not expected to be called.
type lightweightCoreListener struct {
	onRemoteStopTransaction func(request *core.RemoteStopTransactionRequest) (*core.RemoteStopTransactionConfirmation, error)
}

func (l *lightweightCoreListener) OnChangeAvailability(*core.ChangeAvailabilityRequest) (*core.ChangeAvailabilityConfirmation, error) {
	return nil, fmt.Errorf("unexpected call to OnChangeAvailability")
}

func (l *lightweightCoreListener) OnChangeConfiguration(*core.ChangeConfigurationRequest) (*core.ChangeConfigurationConfirmation, error) {
	return nil, fmt.Errorf("unexpected call to OnChangeConfiguration")
}

func (l *lightweightCoreListener) OnClearCache(*core.ClearCacheRequest) (*core.ClearCacheConfirmation, error) {
	return nil, fmt.Errorf("unexpected call to OnClearCache")
}

func (l *lightweightCoreListener) OnDataTransfer(*core.DataTransferRequest) (*core.DataTransferConfirmation, error) {
	return nil, fmt.Errorf("unexpected call to OnDataTransfer")
}

func (l *lightweightCoreListener) OnGetConfiguration(*core.GetConfigurationRequest) (*core.GetConfigurationConfirmation, error) {
	return nil, fmt.Errorf("unexpected call to OnGetConfiguration")
}

func (l *lightweightCoreListener) OnRemoteStartTransaction(*core.RemoteStartTransactionRequest) (*core.RemoteStartTransactionConfirmation, error) {
	return nil, fmt.Errorf("unexpected call to OnRemoteStartTransaction")
}

func (l *lightweightCoreListener) OnRemoteStopTransaction(request *core.RemoteStopTransactionRequest) (*core.RemoteStopTransactionConfirmation, error) {
	return l.onRemoteStopTransaction(request)
}

func (l *lightweightCoreListener) OnReset(*core.ResetRequest) (*core.ResetConfirmation, error) {
	return nil, fmt.Errorf("unexpected call to OnReset")
}

func (l *lightweightCoreListener) OnUnlockConnector(*core.UnlockConnectorRequest) (*core.UnlockConnectorConfirmation, error) {
	return nil, fmt.Errorf("unexpected call to OnUnlockConnector")
}

// TestOrderingResponseBeforeInboundCall is test 1 from
// tasks/s3-inbound-ordering.md: deterministic ordering, made possible by
// pinning the async goroutine.
//
// Recipe:
//
//  1. The charge point sends R0 (a Heartbeat). Its CALL_RESULT is delivered,
//     so R0's response callback starts running on the asyncCallbackHandler
//     goroutine; that callback records "R0" and then blocks on a
//     test-controlled gate, pinning the goroutine.
//  2. The charge point sends R1 (another Heartbeat). R1's CALL_RESULT is
//     delivered on the (simulated) read goroutine, immediately followed by a
//     server-initiated CALL (RemoteStopTransaction) delivered on its own
//     goroutine (delivering it inline could deadlock a correct S3
//     implementation, which applies backpressure once its shared channel
//     fills up while the consumer is pinned — see the spec's concurrency
//     caveats). R1's callback records "R1-response"; the RemoteStop handler
//     records "request".
//  3. The gate is opened. The test waits (via channels, never time.Sleep)
//     until BOTH "R1-response" and "request" have been recorded, then asserts
//     the observed order.
//
// Pre-S3, delivering the CALL runs handleIncomingRequest inline on whichever
// goroutine delivers it — independent of the gate — so "request" is recorded
// essentially immediately, while "R1-response" cannot be recorded until the
// gate opens and the pinned asyncCallbackHandler goroutine is freed to drain
// R1's parked confirmation. The expected (post-S3) order ["R0", "R1-response",
// "request"] is therefore NOT what today's code produces: this test is
// expected to FAIL against the current implementation and to PASS once S3
// unifies the response/error/request dispatch onto one ordered channel.
func (suite *OcppV16TestSuite) TestOrderingResponseBeforeInboundCall() {
	t := suite.T()
	recorder := &orderRecorder{}
	writeC := make(chan []byte, 8)
	ocppj.SetMessageIdGenerator(sequentialMessageIds("ord"))

	startStandaloneChargePoint(suite, writeC)

	transactionId := 42
	remoteStopConfirmation := core.NewRemoteStopTransactionConfirmation(types.RemoteStartStopStatusAccepted)
	requestRecordedC := make(chan struct{})
	// Deliberately NOT a testify mock: recording "request" must not carry any
	// incidental argument-matching/reflection overhead that could tilt the
	// race this test is trying to observe. See the bounded-yield wait below
	// for why that race is, in practice, heavily biased toward observing the
	// pre-S3 ordering (not a hard guarantee, but far from a coin flip).
	coreListener := &lightweightCoreListener{
		onRemoteStopTransaction: func(request *core.RemoteStopTransactionRequest) (*core.RemoteStopTransactionConfirmation, error) {
			recorder.append("request")
			close(requestRecordedC)
			return remoteStopConfirmation, nil
		},
	}
	suite.chargePoint.SetCoreHandler(coreListener)

	// --- (a) send R0 and pin the async goroutine on its response callback ---
	gateC := make(chan struct{})
	pinnedC := make(chan struct{})
	err := suite.chargePoint.SendRequestAsync(core.NewHeartbeatRequest(), func(confirmation ocpp.Response, err error) {
		recorder.append("R0")
		close(pinnedC)
		<-gateC
	})
	require.Nil(t, err)

	var r0Bytes []byte
	select {
	case r0Bytes = <-writeC:
	case <-time.After(inboundOrderingWaitTimeout):
		t.Fatal("timed out waiting for R0 to be written (never became a genuinely pending request)")
	}
	require.NotEmpty(t, r0Bytes)
	// R0 was assigned the first id from our deterministic generator: the
	// client dispatcher only ever has one request in flight at a time, so
	// R0 got "ord-0" and (below) R1 will get "ord-1".
	err = suite.mockWsClient.MessageHandler([]byte(heartbeatCallResultJson("ord-0")))
	require.Nil(t, err)

	waitOrFail(suite, pinnedC, "timed out waiting for the async goroutine to be pinned on R0's response callback")

	// --- (b) send R1; deliver its CALL_RESULT, immediately followed by an inbound CALL ---
	respDoneC := make(chan struct{})
	err = suite.chargePoint.SendRequestAsync(core.NewHeartbeatRequest(), func(confirmation ocpp.Response, err error) {
		recorder.append("R1-response")
		close(respDoneC)
	})
	require.Nil(t, err)

	var r1Bytes []byte
	select {
	case r1Bytes = <-writeC:
	case <-time.After(inboundOrderingWaitTimeout):
		t.Fatal("timed out waiting for R1 to be written (never became a genuinely pending request)")
	}
	require.NotEmpty(t, r1Bytes)
	// This delivers R1's CALL_RESULT synchronously on the test goroutine
	// while asyncCallbackHandler is still pinned on R0's gate. That assumes
	// S3's shared inbound channel has capacity >= 1 (the spec's design:
	// "keep a small buffer, ~ current cap 1") so this send can complete
	// without waiting for the pinned consumer. An unbuffered shared channel
	// would block here until the gate opens below - out of the spec's
	// design, so not something this test needs to tolerate.
	err = suite.mockWsClient.MessageHandler([]byte(heartbeatCallResultJson("ord-1")))
	require.Nil(t, err)

	remoteStopCallJson := fmt.Sprintf(`[2,"ord-remotestop","%v",{"transactionId":%v}]`, core.RemoteStopTransactionFeatureName, transactionId)
	reqDoneC := make(chan struct{})
	var reqErr error
	go func() {
		// Deliver the CALL on its own goroutine: a correct (post-S3)
		// implementation applies backpressure on this call once its shared
		// channel fills up behind the still-pinned goroutine, so this call
		// may legitimately block until the gate below is opened - it must
		// not be made inline on the goroutine that needs to open that gate.
		reqErr = suite.mockWsClient.MessageHandler([]byte(remoteStopCallJson))
		close(reqDoneC)
	}()

	// Give the (independent-of-the-gate, pre-S3) CALL delivery above an
	// overwhelming chance to finish BEFORE the gate is opened, without ever
	// blocking indefinitely (which would deadlock a correct S3
	// implementation, where this delivery cannot complete until the gate
	// opens). Right now the asyncCallbackHandler goroutine is genuinely
	// parked on a receive from the still-open gate and cannot make ANY
	// progress (in particular it cannot record "R1-response") until the gate
	// opens, so a bounded number of cooperative yields either (a) observes
	// requestRecordedC fire - meaning "request" was recorded while the other
	// side was still provably parked, i.e. pre-S3 behavior - or (b) exhausts
	// the budget because the CALL delivery is genuinely blocked (post-S3
	// backpressure), in which case it is safe to open the gate and let the
	// shared channel's FIFO order decide.
	//
	// This is a strong PRACTICAL bound - empirically stable (15/15 runs fail
	// pre-S3) and independent of the shared channel's buffer capacity - but
	// not a formal guarantee: Go does not promise a runnable goroutine will
	// be scheduled within any fixed number of Gosched calls, so in principle
	// the yield budget could exhaust before a pre-S3 CALL delivery finishes,
	// producing a rare false green. We accept that in exchange for never
	// risking a hang on a correct (post-S3) implementation; a wall-clock
	// sleep would trade this for a different unproven assumption (how long
	// is "long enough") while also slowing down every test run.
	awaitBoundedYields(requestRecordedC, 100000)

	// (c) open the gate.
	close(gateC)

	waitOrFail(suite, respDoneC, "timed out waiting for R1's response callback")
	waitOrFail(suite, reqDoneC, "timed out waiting for the inbound CALL to be handled")
	assert.Nil(t, reqErr)

	assert.Equal(t, []string{"R0", "R1-response", "request"}, recorder.snapshot())
}

// TestInboundRequestHandledEndToEnd is test 2: an inbound server CALL still
// reaches the right handler and produces the correct CALL_RESULT, and the
// "not supported" branch (a recognised action whose profile handler was
// never registered) is still answered with the correct CALL_ERROR.
func (suite *OcppV16TestSuite) TestInboundRequestHandledEndToEnd() {
	t := suite.T()
	writeC := make(chan []byte, 8)
	startStandaloneChargePoint(suite, writeC)

	// 1. A supported inbound request reaches the registered handler and the
	// charge point answers with the matching CALL_RESULT.
	messageId := "e2e-1"
	idTag := "12345"
	connectorId := newInt(1)
	status := types.RemoteStartStopStatusAccepted
	startConfirmation := core.NewRemoteStartTransactionConfirmation(status)

	// requestC only CAPTURES the request seen by the mock's .Run callback.
	// That callback runs inline on the read/calling goroutine today, but
	// under S3 the request handler runs on the asyncCallbackHandler
	// goroutine - calling testify's require/assert there would be unsafe
	// (t.FailNow/t.Errorf off the test goroutine can corrupt *testing.T
	// state or be silently swallowed). So we only capture here and assert
	// on the captured value back on the test goroutine below.
	requestC := make(chan *core.RemoteStartTransactionRequest, 1)
	coreListener := &MockChargePointCoreListener{}
	coreListener.On("OnRemoteStartTransaction", mock.Anything).Return(startConfirmation, nil).Run(func(args mock.Arguments) {
		request, _ := args.Get(0).(*core.RemoteStartTransactionRequest)
		requestC <- request
	})
	suite.chargePoint.SetCoreHandler(coreListener)

	callJson := fmt.Sprintf(`[2,"%v","%v",{"connectorId":%v,"idTag":"%v"}]`, messageId, core.RemoteStartTransactionFeatureName, *connectorId, idTag)
	err := suite.mockWsClient.MessageHandler([]byte(callJson))
	require.Nil(t, err)

	// The mock's .Run callback (and thus the send on requestC) executes
	// before OnRemoteStartTransaction returns its confirmation, which is
	// before the CALL_RESULT below is computed and written - so this receive
	// cannot race the writeC receive that follows.
	select {
	case request := <-requestC:
		require.NotNil(t, request)
		assert.Equal(t, idTag, request.IdTag)
		require.NotNil(t, request.ConnectorId)
		assert.Equal(t, *connectorId, *request.ConnectorId)
	case <-time.After(inboundOrderingWaitTimeout):
		t.Fatal("timed out waiting for the captured RemoteStartTransactionRequest")
	}

	// expectedResultJson asserts the exact wire bytes of the CALL_RESULT
	// (including the marshaled status text) - precise, but brittle to
	// unrelated wording/formatting changes; kept as-is since it is stable
	// today.
	expectedResultJson := []byte(fmt.Sprintf(`[3,"%v",{"status":"%v"}]`, messageId, status))
	select {
	case written := <-writeC:
		assert.Equal(t, expectedResultJson, written)
	case <-time.After(inboundOrderingWaitTimeout):
		t.Fatal("timed out waiting for the CALL RESULT")
	}

	// 2. Cheap coverage of the "not supported" branch: GetLocalListVersion's
	// profile is registered on this endpoint (SetupTest wires every
	// profile), but no local-auth-list handler was ever set, so the request
	// must be rejected with NotSupported instead of being silently dropped.
	secondMessageId := "e2e-2"
	getListJson := fmt.Sprintf(`[2,"%v","%v",{}]`, secondMessageId, localauth.GetLocalListVersionFeatureName)
	err = suite.mockWsClient.MessageHandler([]byte(getListJson))
	require.Nil(t, err)

	// Exact wire bytes again, including the marshaled error description text;
	// see the note on expectedResultJson above.
	errorDescription := fmt.Sprintf("unsupported action %v on charge point", localauth.GetLocalListVersionFeatureName)
	expectedErrorJson := []byte(fmt.Sprintf(`[4,"%v","%v","%v",{}]`, secondMessageId, ocppj.NotSupported, errorDescription))
	select {
	case written := <-writeC:
		assert.Equal(t, expectedErrorJson, written)
	case <-time.After(inboundOrderingWaitTimeout):
		t.Fatal("timed out waiting for the NotSupported CALL ERROR")
	}
}

// TestInboundRequestHandlerPanicRecovered is test 3: a panicking inbound
// request handler is recovered, reported via SetOnHandlerPanic with
// Kind=RequestHandlerKind and the right Action/RequestID, a CALL
// ERROR(InternalError) is sent back, and the request-handling path survives
// to correctly process a subsequent inbound request.
//
// Today this is guarded inline at the ocppj layer
// (ocppj/client.go ocppMessageHandler's CALL case, via recoverHandler). S3
// moves the incoming-request dispatch onto the asyncCallbackHandler
// goroutine, so the guard must move with it — this test must keep passing
// unmodified after that change.
func (suite *OcppV16TestSuite) TestInboundRequestHandlerPanicRecovered() {
	t := suite.T()
	writeC := make(chan []byte, 8)
	startStandaloneChargePoint(suite, writeC)

	panicValue := "boom: OnRemoteStartTransaction panic"
	messageId := "panic-req-1"
	idTag := "12345"

	coreListener := &MockChargePointCoreListener{}
	coreListener.On("OnRemoteStartTransaction", mock.Anything).Run(func(args mock.Arguments) {
		panic(panicValue)
	})
	suite.chargePoint.SetCoreHandler(coreListener)

	panicC := make(chan ocppj.HandlerPanic, 1)
	suite.chargePoint.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		panicC <- hp
	})

	callJson := fmt.Sprintf(`[2,"%v","%v",{"idTag":"%v"}]`, messageId, core.RemoteStartTransactionFeatureName, idTag)
	err := suite.mockWsClient.MessageHandler([]byte(callJson))
	require.Nil(t, err)

	// 1. The panic must be recovered (no crash) and reported.
	var hp ocppj.HandlerPanic
	select {
	case hp = <-panicC:
	case <-time.After(inboundOrderingWaitTimeout):
		t.Fatal("timed out waiting for panic callback")
	}
	assert.Equal(t, ocppj.RequestHandlerKind, hp.Kind)
	assert.Equal(t, core.RemoteStartTransactionFeatureName, hp.Action)
	assert.Equal(t, messageId, hp.RequestID)
	assert.Equal(t, panicValue, hp.Value)
	assert.NotEmpty(t, hp.Stack)

	// The callback must fire exactly once for this single panic.
	select {
	case extra := <-panicC:
		t.Fatalf("callback fired more than once: %+v", extra)
	default:
	}

	// 2. The charge point must reply with the auto CALL ERROR(InternalError)
	// in place of the crashed response. Exact wire bytes again, including the
	// fixed auto-CALLERROR description text; see the note on
	// expectedResultJson in TestInboundRequestHandledEndToEnd.
	errorDescription := "internal error while handling request"
	expectedErrorJson := []byte(fmt.Sprintf(`[4,"%v","%v","%v",{}]`, messageId, ocppj.InternalError, errorDescription))
	select {
	case written := <-writeC:
		assert.Equal(t, expectedErrorJson, written)
	case <-time.After(inboundOrderingWaitTimeout):
		t.Fatal("timed out waiting for the auto CALL ERROR")
	}

	// 3. Crucial: prove the request-handling path survived the panic. A
	// second, non-panicking inbound request must still be handled correctly.
	transactionId := 7
	status := types.RemoteStartStopStatusAccepted
	stopConfirmation := core.NewRemoteStopTransactionConfirmation(status)
	coreListener.On("OnRemoteStopTransaction", mock.Anything).Return(stopConfirmation, nil)

	secondMessageId := "panic-req-2"
	stopCallJson := fmt.Sprintf(`[2,"%v","%v",{"transactionId":%v}]`, secondMessageId, core.RemoteStopTransactionFeatureName, transactionId)
	err = suite.mockWsClient.MessageHandler([]byte(stopCallJson))
	require.Nil(t, err)

	// Exact wire bytes once more (see the note above).
	expectedStopResultJson := []byte(fmt.Sprintf(`[3,"%v",{"status":"%v"}]`, secondMessageId, status))
	select {
	case written := <-writeC:
		assert.Equal(t, expectedStopResultJson, written)
	case <-time.After(inboundOrderingWaitTimeout):
		t.Fatal("timed out waiting for the second request's response; the request-handling path did not survive the panic")
	}
}

// TestNoRegressionResponseRoundTrip is test 4: a normal request/response
// round trip (the charge point sends a request, the "server" replies, and
// the response callback fires with the right confirmation and no error)
// still works, unaffected by the unified dispatch.
func (suite *OcppV16TestSuite) TestNoRegressionResponseRoundTrip() {
	t := suite.T()
	writeC := make(chan []byte, 8)
	startStandaloneChargePoint(suite, writeC)

	currentTime := types.NewDateTime(time.Now())

	type asyncResult struct {
		confirmation ocpp.Response
		err          error
	}
	resultC := make(chan asyncResult, 1)
	err := suite.chargePoint.SendRequestAsync(core.NewHeartbeatRequest(), func(confirmation ocpp.Response, err error) {
		resultC <- asyncResult{confirmation: confirmation, err: err}
	})
	require.Nil(t, err)

	select {
	case <-writeC:
	case <-time.After(inboundOrderingWaitTimeout):
		t.Fatal("timed out waiting for the heartbeat request to be written")
	}

	callResultJson := fmt.Sprintf(`[3,"%v",{"currentTime":"%v"}]`, defaultMessageId, currentTime.FormatTimestamp())
	err = suite.mockWsClient.MessageHandler([]byte(callResultJson))
	require.Nil(t, err)

	select {
	case result := <-resultC:
		require.Nil(t, result.err)
		require.NotNil(t, result.confirmation)
		hbConfirmation, ok := result.confirmation.(*core.HeartbeatConfirmation)
		require.True(t, ok)
		assertDateTimeEquality(t, *currentTime, *hbConfirmation.CurrentTime)
	case <-time.After(inboundOrderingWaitTimeout):
		t.Fatal("timed out waiting for the response callback")
	}
}

// TestFacadeErrorCallbackRoundTrip guards the facade's default CALL_ERROR
// callback path: ocppj's error handler must feed cp.errorHandler, and the
// asyncCallbackHandler must invoke the original SendRequestAsync callback as
// callback(nil, err). Do not replace the ocppj client's error handler here;
// doing so would bypass the facade channel this test covers.
func (suite *OcppV16TestSuite) TestFacadeErrorCallbackRoundTrip() {
	t := suite.T()
	writeC := make(chan []byte, 8)
	startStandaloneChargePoint(suite, writeC)

	type asyncResult struct {
		confirmation ocpp.Response
		err          error
	}
	resultC := make(chan asyncResult, 1)
	err := suite.chargePoint.SendRequestAsync(core.NewHeartbeatRequest(), func(confirmation ocpp.Response, err error) {
		resultC <- asyncResult{confirmation: confirmation, err: err}
	})
	require.Nil(t, err)

	select {
	case <-writeC:
	case <-time.After(inboundOrderingWaitTimeout):
		t.Fatal("timed out waiting for the heartbeat request to be written")
	}

	code := ocppj.GenericError
	description := "server rejected heartbeat"
	callErrorJson := fmt.Sprintf(`[4,"%v","%v","%v",{}]`, defaultMessageId, code, description)
	err = suite.mockWsClient.MessageHandler([]byte(callErrorJson))
	require.Nil(t, err)

	select {
	case result := <-resultC:
		require.Nil(t, result.confirmation)
		require.NotNil(t, result.err)
		ocppErr, ok := result.err.(*ocpp.Error)
		require.True(t, ok)
		assert.Equal(t, code, ocppErr.Code)
		assert.Equal(t, description, ocppErr.Description)
		assert.Equal(t, defaultMessageId, ocppErr.MessageId)
	case <-time.After(inboundOrderingWaitTimeout):
		t.Fatal("timed out waiting for the error callback")
	}
}
