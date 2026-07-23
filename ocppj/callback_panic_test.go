package ocppj_test

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/enesismail/ocpp-go/ocpp"
	"github.com/enesismail/ocpp-go/ocppj"
	"github.com/enesismail/ocpp-go/ws"
)

// This file tests the S1 part 5 panic-isolation behavior described in
// tasks/s1-5-fix.md: the three remaining raw-ocppj user-callback panic
// vectors that parts 1-4 (PR #8) deliberately deferred -
//
//  1. onRequestCancel on the dispatcher messagePump (both the timeout and the
//     write-failure trigger paths, for both the client and server dispatcher),
//  2. the client's onDisconnectedHandler/onReconnectedHandler, and
//  3. invalidMessageHook (client and server), including the pristine-original
//     restore-on-panic guarantee when the hook mutates the passed-in
//     *ocpp.Error before panicking.
//
// It binds against the not-yet-implemented ocppj.CancelHandlerKind,
// ocppj.ReconnectHandlerKind and ocppj.InvalidMessageHandlerKind constants.
// Until the corresponding production guards land, this package will fail to
// build/vet with undefined-symbol errors for exactly those three
// identifiers - that is expected (red-first TDD).
//
// As with panic_isolation_test.go, synchronization relies exclusively on
// buffered channels and explicit deadlines (panicWaitTimeout, defined there),
// never on time.Sleep.

// ----------------------------------------------------------------------
// Vector 1: onRequestCancel on the dispatcher messagePump
// ----------------------------------------------------------------------

// TestClientCancelHandlerPanicRecoveredOnTimeout drives a client request
// timeout (via a short dispatcher timeout and a request that never gets a
// response) with a panicking SetOnRequestCanceled handler registered. It
// asserts the panic is reported via SetOnHandlerPanic with
// Kind=CancelHandlerKind and the correct action/request id, and - crucially -
// that the messagePump goroutine survived: a subsequent SendRequest still
// dispatches (i.e. the guard must be scoped per-call, not to the whole pump
// loop's lifetime).
func (suite *OcppJTestSuite) TestClientCancelHandlerPanicRecoveredOnTimeout() {
	t := suite.T()
	panicC := make(chan ocppj.HandlerPanic, 4)
	writeC := make(chan []byte, 4)

	panicValue := "boom: cancel handler panic (client timeout)"
	suite.chargePoint.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		panicC <- hp
	})
	suite.chargePoint.SetOnRequestCanceled(func(requestId string, request ocpp.Request, err *ocpp.Error) {
		panic(panicValue)
	})
	suite.clientDispatcher.SetTimeout(300 * time.Millisecond)
	suite.mockClient.On("Start", mock.AnythingOfType("string")).Return(nil)
	// Forward the raw written frame to the test goroutine; parse it there
	// (ParseCall may call t.FailNow, which must not run on the pump goroutine).
	suite.mockClient.On("Write", mock.Anything).Run(func(args mock.Arguments) {
		writeC <- args.Get(0).([]byte)
	}).Return(nil)
	require.NoError(t, suite.chargePoint.Start("someUrl"))

	req := newMockRequest("testValue")
	_, e2sendErr := suite.chargePoint.SendRequest(req)
	require.NoError(t, e2sendErr)

	var data []byte
	select {
	case data = <-writeC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for the initial request to be dispatched")
	}
	call := ParseCall(&suite.chargePoint.Endpoint, suite.chargePoint.RequestState, string(data), t)

	var hp ocppj.HandlerPanic
	select {
	case hp = <-panicC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for panic callback")
	}
	assert.Equal(t, ocppj.CancelHandlerKind, hp.Kind)
	assert.Equal(t, MockFeatureName, hp.Action)
	assert.Equal(t, call.UniqueId, hp.RequestID)
	assert.Equal(t, "", hp.ClientID)
	assert.Equal(t, panicValue, hp.Value)
	assert.NotEmpty(t, hp.Stack)

	// The pump must have survived: a subsequent request still dispatches.
	req2 := newMockRequest("testValue2")
	_, e2sendErr = suite.chargePoint.SendRequest(req2)
	require.NoError(t, e2sendErr)
	select {
	case <-writeC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for the subsequent request to dispatch after the cancel-handler panic")
	}
}

// TestClientCancelHandlerPanicRecoveredOnWriteFailure mirrors the above for
// the write-failure trigger path (dispatcher.go's dispatchNextRequest, a
// distinct guarded call site from the timeout path).
func (suite *OcppJTestSuite) TestClientCancelHandlerPanicRecoveredOnWriteFailure() {
	t := suite.T()
	panicC := make(chan ocppj.HandlerPanic, 4)
	callIDC := make(chan string, 1)
	writeC := make(chan []byte, 4)

	panicValue := "boom: cancel handler panic (client write failure)"
	suite.chargePoint.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		panicC <- hp
	})
	suite.chargePoint.SetOnRequestCanceled(func(requestId string, request ocpp.Request, err *ocpp.Error) {
		panic(panicValue)
	})
	suite.mockClient.On("Start", mock.AnythingOfType("string")).Return(nil)
	// First write fails, triggering the cancel path. Registered with Once() so
	// the fallback expectation below (unlimited, successful) takes over for
	// any later writes - this is what proves the pump survived.
	suite.mockClient.On("Write", mock.Anything).Return(fmt.Errorf("networkError")).Once().Run(func(args mock.Arguments) {
		// Runs on the pump goroutine: capture the dispatched id without any
		// t.FailNow-based assertion; the test goroutine asserts it below.
		var id string
		if el := suite.clientRequestQueue.Peek(); el != nil {
			if bundle, ok := el.(ocppj.RequestBundle); ok && bundle.Call != nil {
				id = bundle.Call.GetUniqueId()
			}
		}
		callIDC <- id
	})
	suite.mockClient.On("Write", mock.Anything).Run(func(args mock.Arguments) {
		writeC <- args.Get(0).([]byte)
	}).Return(nil)
	require.NoError(t, suite.chargePoint.Start("someUrl"))

	req := newMockRequest("testValue")
	_, e2sendErr := suite.chargePoint.SendRequest(req)
	require.NoError(t, e2sendErr)

	var requestID string
	select {
	case requestID = <-callIDC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for the failing write")
	}
	require.NotEmpty(t, requestID, "the failing write must have observed the dispatched request in the queue")

	var hp ocppj.HandlerPanic
	select {
	case hp = <-panicC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for panic callback")
	}
	assert.Equal(t, ocppj.CancelHandlerKind, hp.Kind)
	assert.Equal(t, MockFeatureName, hp.Action)
	assert.Equal(t, requestID, hp.RequestID)
	assert.Equal(t, "", hp.ClientID)
	assert.Equal(t, panicValue, hp.Value)
	assert.NotEmpty(t, hp.Stack)

	// The pump must have survived: a subsequent request still dispatches
	// (this time successfully, per the fallback Write expectation above).
	req2 := newMockRequest("testValue2")
	_, e2sendErr = suite.chargePoint.SendRequest(req2)
	require.NoError(t, e2sendErr)
	select {
	case <-writeC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for the subsequent request to dispatch after the cancel-handler panic")
	}
}

// TestServerCancelHandlerPanicRecoveredOnTimeout is the server-side analogue
// of TestClientCancelHandlerPanicRecoveredOnTimeout: a panicking canceled-
// request handler must be recovered, reported with the client id attached,
// and must not kill the shared server messagePump - a subsequent request to
// the SAME client still dispatches.
func (suite *OcppJTestSuite) TestServerCancelHandlerPanicRecoveredOnTimeout() {
	t := suite.T()
	clientID := "cancelClientTimeout"
	panicC := make(chan ocppj.HandlerPanic, 4)
	writeC := make(chan []byte, 4)

	suite.centralSystem.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		panicC <- hp
	})
	suite.centralSystem.SetCanceledRequestHandler(func(cID string, requestID string, request ocpp.Request, err *ocpp.Error) {
		panic("boom: cancel handler panic (server timeout)")
	})
	suite.serverDispatcher.SetTimeout(300 * time.Millisecond)
	suite.mockServer.On("Start", mock.AnythingOfType("int"), mock.AnythingOfType("string")).Return()
	suite.mockServer.On("Write", clientID, mock.Anything).Run(func(args mock.Arguments) {
		writeC <- args.Get(1).([]byte)
	}).Return(nil)
	suite.centralSystem.Start(8887, "somePath")
	suite.serverDispatcher.CreateClient(clientID)

	req := newMockRequest("testValue")
	_, e2sendErr := suite.centralSystem.SendRequest(clientID, req)
	require.NoError(t, e2sendErr)

	var data []byte
	select {
	case data = <-writeC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for the initial request to be dispatched")
	}
	state := suite.centralSystem.RequestState.GetClientState(clientID)
	call := ParseCall(&suite.centralSystem.Endpoint, state, string(data), t)

	var hp ocppj.HandlerPanic
	select {
	case hp = <-panicC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for panic callback")
	}
	assert.Equal(t, ocppj.CancelHandlerKind, hp.Kind)
	assert.Equal(t, clientID, hp.ClientID)
	assert.Equal(t, MockFeatureName, hp.Action)
	assert.Equal(t, call.UniqueId, hp.RequestID)
	assert.NotEmpty(t, hp.Stack)

	// The shared pump must have survived: a subsequent request to the same
	// client still dispatches.
	req2 := newMockRequest("testValue2")
	_, e2sendErr = suite.centralSystem.SendRequest(clientID, req2)
	require.NoError(t, e2sendErr)
	select {
	case <-writeC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for the subsequent request to dispatch after the cancel-handler panic")
	}
}

// TestServerCancelHandlerPanicRecoveredOnWriteFailure mirrors the above for
// the server's write-failure trigger path (dispatcher.go's
// dispatchNextRequest for the server dispatcher).
func (suite *OcppJTestSuite) TestServerCancelHandlerPanicRecoveredOnWriteFailure() {
	t := suite.T()
	clientID := "cancelClientWriteFailure"
	panicC := make(chan ocppj.HandlerPanic, 4)
	callIDC := make(chan string, 1)
	writeC := make(chan []byte, 4)

	suite.centralSystem.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		panicC <- hp
	})
	suite.centralSystem.SetCanceledRequestHandler(func(cID string, requestID string, request ocpp.Request, err *ocpp.Error) {
		panic("boom: cancel handler panic (server write failure)")
	})
	suite.mockServer.On("Start", mock.AnythingOfType("int"), mock.AnythingOfType("string")).Return()
	// First write fails, triggering the cancel path. Registered with Once() so
	// the fallback expectation below (unlimited, successful) takes over for
	// any later writes - this is what proves the pump survived.
	suite.mockServer.On("Write", clientID, mock.Anything).Return(fmt.Errorf("networkError")).Once().Run(func(args mock.Arguments) {
		// Runs on the pump goroutine: capture the dispatched id without any
		// t.FailNow-based assertion; the test goroutine asserts it below.
		var id string
		if q, ok := suite.serverRequestMap.Get(clientID); ok {
			if el := q.Peek(); el != nil {
				if bundle, ok := el.(ocppj.RequestBundle); ok && bundle.Call != nil {
					id = bundle.Call.GetUniqueId()
				}
			}
		}
		callIDC <- id
	})
	suite.mockServer.On("Write", clientID, mock.Anything).Run(func(args mock.Arguments) {
		writeC <- args.Get(1).([]byte)
	}).Return(nil)
	suite.centralSystem.Start(8887, "somePath")
	suite.serverDispatcher.CreateClient(clientID)

	req := newMockRequest("testValue")
	_, e2sendErr := suite.centralSystem.SendRequest(clientID, req)
	require.NoError(t, e2sendErr)

	var requestID string
	select {
	case requestID = <-callIDC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for the failing write")
	}
	require.NotEmpty(t, requestID, "the failing write must have observed the dispatched request in the queue")

	var hp ocppj.HandlerPanic
	select {
	case hp = <-panicC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for panic callback")
	}
	assert.Equal(t, ocppj.CancelHandlerKind, hp.Kind)
	assert.Equal(t, clientID, hp.ClientID)
	assert.Equal(t, MockFeatureName, hp.Action)
	assert.Equal(t, requestID, hp.RequestID)
	assert.NotEmpty(t, hp.Stack)

	// The shared pump must have survived: a subsequent request to the same
	// client still dispatches (this time successfully).
	req2 := newMockRequest("testValue2")
	_, e2sendErr = suite.centralSystem.SendRequest(clientID, req2)
	require.NoError(t, e2sendErr)
	select {
	case <-writeC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for the subsequent request to dispatch after the cancel-handler panic")
	}
}

// ----------------------------------------------------------------------
// Vector 2: client onDisconnectedHandler / onReconnectedHandler
// ----------------------------------------------------------------------

// TestClientDisconnectedAndReconnectedHandlerPanicRecovered registers
// panicking SetOnDisconnectedHandler/SetOnReconnectedHandler callbacks and
// drives a disconnect followed by a reconnect through the mock client, the
// same way the existing client tests do (suite.mockClient.DisconnectedHandler
// / ReconnectedHandler). It asserts: recovery (no crash), the panic callback
// fires with Kind=DisconnectHandlerKind then Kind=ReconnectHandlerKind, and -
// crucially - that the dispatcher's Pause()/Resume() still actually ran
// around the panicking handlers: the dispatcher is paused right after the
// disconnect and unpaused (and still able to dispatch a request) right after
// the reconnect.
func (suite *OcppJTestSuite) TestClientDisconnectedAndReconnectedHandlerPanicRecovered() {
	t := suite.T()
	disconnectError := fmt.Errorf("some disconnect error")
	panicC := make(chan ocppj.HandlerPanic, 4)
	writeC := make(chan []byte, 4)
	// Observed-from-inside-the-handler paused state, captured before each
	// handler panics. Buffered so the capture-then-panic sequence never
	// blocks. The reconnect capture is what enforces scoping: a properly-scoped
	// per-handler recover (spec shape: IIFE around just the handler) leaves
	// Resume() to run AFTER the handler, so the reconnect handler must observe
	// IsPaused()==true; a broad recover that spans Resume() too (or one that
	// runs Resume() before the handler) fails this. The disconnect capture
	// (IsPaused()==true) documents/confirms the Pause()-before-handler ordering
	// (behaviorally identical for scoped vs broad on that leg, but a useful
	// invariant to pin).
	disconnectPausedC := make(chan bool, 1)
	reconnectPausedC := make(chan bool, 1)

	suite.chargePoint.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		panicC <- hp
	})
	suite.chargePoint.SetOnDisconnectedHandler(func(err error) {
		disconnectPausedC <- suite.clientDispatcher.IsPaused()
		panic("boom: disconnect handler panic")
	})
	suite.chargePoint.SetOnReconnectedHandler(func() {
		reconnectPausedC <- suite.clientDispatcher.IsPaused()
		panic("boom: reconnect handler panic")
	})
	suite.mockClient.On("Start", mock.AnythingOfType("string")).Return(nil)
	suite.mockClient.On("Write", mock.Anything).Run(func(args mock.Arguments) {
		writeC <- args.Get(0).([]byte)
	}).Return(nil)
	require.NoError(t, suite.chargePoint.Start("someUrl"))
	require.False(t, suite.clientDispatcher.IsPaused())

	require.NotPanics(t, func() {
		suite.mockClient.DisconnectedHandler(disconnectError)
	})

	var hp ocppj.HandlerPanic
	select {
	case hp = <-panicC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for disconnect panic callback")
	}
	assert.Equal(t, ocppj.DisconnectHandlerKind, hp.Kind)
	assert.Equal(t, "", hp.ClientID)
	assert.Equal(t, "", hp.Action)
	assert.Equal(t, "", hp.RequestID)
	assert.NotEmpty(t, hp.Stack)

	// The disconnect handler itself must have observed the dispatcher already
	// paused - proving Pause() ran, and completed, before the handler (and
	// thus before the panic), not merely before some broader recover exits.
	select {
	case paused := <-disconnectPausedC:
		assert.True(t, paused, "dispatcher must already be paused when the disconnect handler observes it")
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for the disconnect handler's observed paused state")
	}

	// Pause() (which runs before the handler is invoked) must have completed
	// despite the handler panic.
	assert.True(t, suite.clientDispatcher.IsPaused())

	require.NotPanics(t, func() {
		suite.mockClient.ReconnectedHandler()
	})

	select {
	case hp = <-panicC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for reconnect panic callback")
	}
	assert.Equal(t, ocppj.ReconnectHandlerKind, hp.Kind)
	assert.Equal(t, "", hp.ClientID)
	assert.Equal(t, "", hp.Action)
	assert.Equal(t, "", hp.RequestID)
	assert.NotEmpty(t, hp.Stack)

	// The reconnect handler itself must have observed the dispatcher STILL
	// paused - proving the handler runs (and panics) before Resume(), i.e.
	// the recover is scoped to just the handler and does not also swallow
	// (or preempt) the Resume() call that follows it.
	select {
	case paused := <-reconnectPausedC:
		assert.True(t, paused, "dispatcher must still be paused when the reconnect handler observes it (Resume() runs after the handler)")
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for the reconnect handler's observed paused state")
	}

	// Resume() (which runs after the handler is invoked) must have completed
	// despite the handler panic: the dispatcher is unpaused and still usable.
	assert.False(t, suite.clientDispatcher.IsPaused())
	req := newMockRequest("testValue")
	_, e2sendErr := suite.chargePoint.SendRequest(req)
	require.NoError(t, e2sendErr)
	select {
	case <-writeC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for dispatch after reconnect")
	}
}

// ----------------------------------------------------------------------
// Vector 3: invalidMessageHook (client + server)
// ----------------------------------------------------------------------

// newInvalidButParseableIDMessage builds a CALL frame whose OCPP-J unique id
// is still extractable, but whose payload fails to unmarshal into the
// registered MockRequest type (mockValue expects a string, not a number).
// This is the "malformed-but-parseable-id" trigger for invalidMessageHook,
// mirroring TestChargePointInvalidMessageHook/TestCentralSystemInvalidMessageHook.
func newInvalidButParseableIDMessage(messageID string) string {
	mockPayload := map[string]interface{}{
		"mockValue": float64(1234),
	}
	serializedPayload, _ := json.Marshal(mockPayload)
	return fmt.Sprintf(`[2,"%v","%s",%v]`, messageID, MockFeatureName, string(serializedPayload))
}

// invalidMessageOriginalDescription is the (stable, Go-json-package-derived)
// description of the ocpp.Error produced when parsing
// newInvalidButParseableIDMessage's payload, matching the existing
// TestChargePointInvalidMessageHook/TestCentralSystemInvalidMessageHook
// expectations.
const invalidMessageOriginalDescription = "json: cannot unmarshal number into Go struct field MockRequest.mockValue of type string"

// TestClientInvalidMessageHookPanicRecovered registers a panicking
// SetInvalidMessageHook and feeds a malformed-but-parseable-id frame through
// the mock client's MessageHandler. It asserts recovery, that the panic is
// reported with Kind=InvalidMessageHandlerKind and the parsed message id, and
// that the read loop survived: a subsequent valid CALL is still delivered to
// a fresh request handler.
func (suite *OcppJTestSuite) TestClientInvalidMessageHookPanicRecovered() {
	t := suite.T()
	mockID := "8001"
	panicValue := "boom: invalid message hook panic"
	invalidMessage := newInvalidButParseableIDMessage(mockID)

	panicC := make(chan ocppj.HandlerPanic, 4)
	suite.chargePoint.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		panicC <- hp
	})
	suite.chargePoint.SetInvalidMessageHook(func(err *ocpp.Error, rawMessage string, parsedFields []interface{}) *ocpp.Error {
		panic(panicValue)
	})
	suite.mockClient.On("Start", mock.AnythingOfType("string")).Return(nil)
	suite.mockClient.On("Write", mock.Anything).Return(nil)
	require.NoError(t, suite.chargePoint.Start("someUrl"))

	require.NotPanics(t, func() {
		err := suite.mockClient.MessageHandler([]byte(invalidMessage))
		_, ok := err.(*ocpp.Error)
		assert.True(t, ok, "expected the resulting error from an invalid message to be an *ocpp.Error")
	})

	var hp ocppj.HandlerPanic
	select {
	case hp = <-panicC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for panic callback")
	}
	assert.Equal(t, ocppj.InvalidMessageHandlerKind, hp.Kind)
	assert.Equal(t, "", hp.ClientID)
	assert.Equal(t, "", hp.Action)
	assert.Equal(t, mockID, hp.RequestID)
	assert.Equal(t, panicValue, hp.Value)
	assert.NotEmpty(t, hp.Stack)

	// The read loop must have survived: a subsequent valid CALL is still
	// delivered to a fresh request handler.
	secondUniqueId := "8002"
	mockValue := "someValue"
	deliveredC := make(chan string, 1)
	suite.chargePoint.SetRequestHandler(func(request ocpp.Request, requestId string, action string) {
		deliveredC <- requestId
	})
	secondCall := fmt.Sprintf(`[2,"%v","%v",{"mockValue":"%v"}]`, secondUniqueId, MockFeatureName, mockValue)
	err := suite.mockClient.MessageHandler([]byte(secondCall))
	assert.Nil(t, err)
	select {
	case id := <-deliveredC:
		assert.Equal(t, secondUniqueId, id)
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for subsequent message delivery")
	}
}

// TestServerInvalidMessageHookPanicRecovered is the server-side analogue of
// TestClientInvalidMessageHookPanicRecovered.
func (suite *OcppJTestSuite) TestServerInvalidMessageHookPanicRecovered() {
	t := suite.T()
	clientID := "invalidMsgClient"
	channel := NewMockWebSocket(clientID)
	mockID := "8101"
	panicValue := "boom: server invalid message hook panic"
	invalidMessage := newInvalidButParseableIDMessage(mockID)

	panicC := make(chan ocppj.HandlerPanic, 4)
	suite.centralSystem.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		panicC <- hp
	})
	suite.centralSystem.SetInvalidMessageHook(func(client ws.Channel, err *ocpp.Error, rawJson string, parsedFields []interface{}) *ocpp.Error {
		panic(panicValue)
	})
	suite.mockServer.On("Start", mock.AnythingOfType("int"), mock.AnythingOfType("string")).Return(nil)
	suite.mockServer.On("Write", mock.AnythingOfType("string"), mock.Anything).Return(nil)
	suite.centralSystem.Start(8887, "somePath")
	suite.serverDispatcher.CreateClient(clientID)

	require.NotPanics(t, func() {
		err := suite.mockServer.MessageHandler(channel, []byte(invalidMessage))
		_, ok := err.(*ocpp.Error)
		assert.True(t, ok, "expected the resulting error from an invalid message to be an *ocpp.Error")
	})

	var hp ocppj.HandlerPanic
	select {
	case hp = <-panicC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for panic callback")
	}
	assert.Equal(t, ocppj.InvalidMessageHandlerKind, hp.Kind)
	assert.Equal(t, clientID, hp.ClientID)
	assert.Equal(t, "", hp.Action)
	assert.Equal(t, mockID, hp.RequestID)
	assert.Equal(t, panicValue, hp.Value)
	assert.NotEmpty(t, hp.Stack)

	// The read loop must have survived: a subsequent valid CALL from the same
	// client is still delivered to a fresh request handler.
	secondUniqueId := "8102"
	mockValue := "someValue"
	deliveredC := make(chan string, 1)
	suite.centralSystem.SetRequestHandler(func(client ws.Channel, request ocpp.Request, requestId string, action string) {
		deliveredC <- requestId
	})
	secondCall := fmt.Sprintf(`[2,"%v","%v",{"mockValue":"%v"}]`, secondUniqueId, MockFeatureName, mockValue)
	err := suite.mockServer.MessageHandler(channel, []byte(secondCall))
	assert.Nil(t, err)
	select {
	case id := <-deliveredC:
		assert.Equal(t, secondUniqueId, id)
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for subsequent message delivery")
	}
}

// TestClientInvalidMessageHookPanicRestoresPristineOriginalOnMutation covers
// the snapshot/restore-on-panic behavior: the panicking hook first mutates
// the passed-in *ocpp.Error (overwriting its Description with a sentinel)
// and then panics. The CALL ERROR the client ultimately writes to the peer
// must carry the ORIGINAL, unmutated description - not the sentinel the hook
// tried to inject - proving the guard restores a snapshot rather than merely
// falling back to some non-nil result.
func (suite *OcppJTestSuite) TestClientInvalidMessageHookPanicRestoresPristineOriginalOnMutation() {
	t := suite.T()
	mockID := "8201"
	sentinelCode := ocpp.ErrorCode("SentinelCode_MUST_NOT_BE_SENT")
	sentinelMessageId := "SENTINEL_ID_MUST_NOT_BE_SENT"
	sentinelDescription := "MUTATED BY PANICKING HOOK - MUST NOT BE SENT"
	invalidMessage := newInvalidButParseableIDMessage(mockID)
	// The original error code the parse of newInvalidButParseableIDMessage's
	// payload produces, per TestChargePointInvalidMessageHook.
	originalCode := ocppj.FormatErrorType(suite.chargePoint)

	panicC := make(chan ocppj.HandlerPanic, 4)
	writtenC := make(chan []byte, 4)
	suite.chargePoint.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		panicC <- hp
	})
	suite.chargePoint.SetInvalidMessageHook(func(err *ocpp.Error, rawMessage string, parsedFields []interface{}) *ocpp.Error {
		// Mutate every mutable field of the passed-in error before panicking.
		// A correct guard must restore the WHOLE pristine original from a
		// snapshot, not leave any of these mutations in place - a guard that
		// only restores (say) Description would still leak the sentinel
		// Code/MessageId below.
		err.Code = sentinelCode
		err.MessageId = sentinelMessageId
		err.Description = sentinelDescription
		panic("boom: invalid message hook panic after mutating the error")
	})
	suite.mockClient.On("Start", mock.AnythingOfType("string")).Return(nil)
	suite.mockClient.On("Write", mock.Anything).Run(func(args mock.Arguments) {
		writtenC <- args.Get(0).([]byte)
	}).Return(nil)
	require.NoError(t, suite.chargePoint.Start("someUrl"))

	require.NotPanics(t, func() {
		_ = suite.mockClient.MessageHandler([]byte(invalidMessage))
	})

	var hp ocppj.HandlerPanic
	select {
	case hp = <-panicC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for panic callback")
	}
	assert.Equal(t, ocppj.InvalidMessageHandlerKind, hp.Kind)
	assert.Equal(t, "", hp.ClientID)
	assert.Equal(t, mockID, hp.RequestID)

	var data []byte
	select {
	case data = <-writtenC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for the resulting CALL ERROR to be sent")
	}
	var frame []interface{}
	require.NoError(t, json.Unmarshal(data, &frame))
	require.GreaterOrEqual(t, len(frame), 4, "CALL ERROR frame must have at least 4 fields")

	id, ok := frame[1].(string)
	require.True(t, ok)
	assert.NotEqual(t, sentinelMessageId, id, "the panicking hook's mutation of MessageId must not leak into the outgoing error")
	assert.Equal(t, mockID, id)

	code, ok := frame[2].(string)
	require.True(t, ok)
	assert.NotEqual(t, string(sentinelCode), code, "the panicking hook's mutation of Code must not leak into the outgoing error")
	assert.Equal(t, string(originalCode), code)

	description, ok := frame[3].(string)
	require.True(t, ok)
	assert.NotEqual(t, sentinelDescription, description, "the panicking hook's mutation of Description must not leak into the outgoing error")
	assert.Equal(t, invalidMessageOriginalDescription, description)
}

// TestServerInvalidMessageHookPanicRestoresPristineOriginalOnMutation is the
// server-side analogue of
// TestClientInvalidMessageHookPanicRestoresPristineOriginalOnMutation.
func (suite *OcppJTestSuite) TestServerInvalidMessageHookPanicRestoresPristineOriginalOnMutation() {
	t := suite.T()
	clientID := "invalidMsgMutateClient"
	channel := NewMockWebSocket(clientID)
	mockID := "8301"
	sentinelCode := ocpp.ErrorCode("SentinelCode_MUST_NOT_BE_SENT")
	sentinelMessageId := "SENTINEL_ID_MUST_NOT_BE_SENT"
	sentinelDescription := "MUTATED BY PANICKING HOOK - MUST NOT BE SENT"
	invalidMessage := newInvalidButParseableIDMessage(mockID)
	// The original error code the parse of newInvalidButParseableIDMessage's
	// payload produces, per TestCentralSystemInvalidMessageHook.
	originalCode := ocppj.FormatErrorType(suite.centralSystem)

	panicC := make(chan ocppj.HandlerPanic, 4)
	writtenC := make(chan []byte, 4)
	suite.centralSystem.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		panicC <- hp
	})
	suite.centralSystem.SetInvalidMessageHook(func(client ws.Channel, err *ocpp.Error, rawJson string, parsedFields []interface{}) *ocpp.Error {
		// Mutate every mutable field of the passed-in error before panicking.
		// A correct guard must restore the WHOLE pristine original from a
		// snapshot, not leave any of these mutations in place - a guard that
		// only restores (say) Description would still leak the sentinel
		// Code/MessageId below.
		err.Code = sentinelCode
		err.MessageId = sentinelMessageId
		err.Description = sentinelDescription
		panic("boom: server invalid message hook panic after mutating the error")
	})
	suite.mockServer.On("Start", mock.AnythingOfType("int"), mock.AnythingOfType("string")).Return(nil)
	suite.mockServer.On("Write", clientID, mock.Anything).Run(func(args mock.Arguments) {
		writtenC <- args.Get(1).([]byte)
	}).Return(nil)
	suite.centralSystem.Start(8887, "somePath")
	suite.serverDispatcher.CreateClient(clientID)

	require.NotPanics(t, func() {
		_ = suite.mockServer.MessageHandler(channel, []byte(invalidMessage))
	})

	var hp ocppj.HandlerPanic
	select {
	case hp = <-panicC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for panic callback")
	}
	assert.Equal(t, ocppj.InvalidMessageHandlerKind, hp.Kind)
	assert.Equal(t, clientID, hp.ClientID)
	assert.Equal(t, mockID, hp.RequestID)

	var data []byte
	select {
	case data = <-writtenC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for the resulting CALL ERROR to be sent")
	}
	var frame []interface{}
	require.NoError(t, json.Unmarshal(data, &frame))
	require.GreaterOrEqual(t, len(frame), 4, "CALL ERROR frame must have at least 4 fields")

	id, ok := frame[1].(string)
	require.True(t, ok)
	assert.NotEqual(t, sentinelMessageId, id, "the panicking hook's mutation of MessageId must not leak into the outgoing error")
	assert.Equal(t, mockID, id)

	code, ok := frame[2].(string)
	require.True(t, ok)
	assert.NotEqual(t, string(sentinelCode), code, "the panicking hook's mutation of Code must not leak into the outgoing error")
	assert.Equal(t, string(originalCode), code)

	description, ok := frame[3].(string)
	require.True(t, ok)
	assert.NotEqual(t, sentinelDescription, description, "the panicking hook's mutation of Description must not leak into the outgoing error")
	assert.Equal(t, invalidMessageOriginalDescription, description)
}
