package ocppj_test

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/enesismail/ocpp-go/ocpp"
	"github.com/enesismail/ocpp-go/ocppj"
	"github.com/enesismail/ocpp-go/ws"
)

// This file tests the S1 panic-isolation behavior described in
// tasks/S1-panic-isolation.md: a panic in a user-provided handler (request,
// response, error, connect or disconnect) must be recovered at the ocppj
// layer, never crash the endpoint/process, and never affect other
// clients/messages. It binds against the not-yet-implemented API:
// ocppj.HandlerPanic, ocppj.HandlerKind (and its constants), and
// (*Client|*Server).SetOnHandlerPanic. Until that API lands, this package
// will fail to build/vet with undefined-symbol errors - that is expected
// (red-first TDD).

// panicWaitTimeout bounds how long a test waits for an async signal before
// failing, so a broken implementation produces a fast, deterministic test
// failure instead of a hang. Using channels + this deadline (rather than
// time.Sleep polling) keeps the tests race-safe and non-flaky.
const panicWaitTimeout = 2 * time.Second

// ----------------------------------------------------------------------
// Client: requestHandler / responseHandler / errorHandler panics
// ----------------------------------------------------------------------

// TestClientRequestHandlerPanicRecovered asserts that a panicking
// requestHandler is recovered (the client survives), the registered
// SetOnHandlerPanic callback fires exactly once with Kind=request and the
// right Action/RequestID/ClientID/Value/Stack, and a subsequent inbound CALL
// is still delivered to a freshly-set requestHandler.
func (suite *OcppJTestSuite) TestClientRequestHandlerPanicRecovered() {
	t := suite.T()
	mockUniqueId := "5001"
	mockValue := "someValue"
	mockCall := fmt.Sprintf(`[2,"%v","%v",{"mockValue":"%v"}]`, mockUniqueId, MockFeatureName, mockValue)
	panicValue := "boom: request handler panic"

	panicC := make(chan ocppj.HandlerPanic, 4)
	suite.chargePoint.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		panicC <- hp
	})
	suite.chargePoint.SetRequestHandler(func(request ocpp.Request, requestId string, action string) {
		panic(panicValue)
	})
	suite.mockClient.On("Start", mock.AnythingOfType("string")).Return(nil)
	suite.mockClient.On("Write", mock.Anything).Return(nil)
	require.NoError(t, suite.chargePoint.Start("someUrl"))

	// Deliver a CALL whose handler panics. This must not crash the test process.
	err := suite.mockClient.MessageHandler([]byte(mockCall))
	assert.Nil(t, err)

	var hp ocppj.HandlerPanic
	select {
	case hp = <-panicC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for panic callback")
	}
	assert.Equal(t, ocppj.RequestHandlerKind, hp.Kind)
	assert.Equal(t, MockFeatureName, hp.Action)
	assert.Equal(t, mockUniqueId, hp.RequestID)
	assert.Equal(t, "", hp.ClientID)
	assert.Equal(t, panicValue, hp.Value)
	assert.NotEmpty(t, hp.Stack)

	// The callback must fire exactly once for this single panic.
	select {
	case extra := <-panicC:
		t.Fatalf("callback fired more than once: %+v", extra)
	default:
	}

	// A subsequent inbound CALL must still be delivered to a fresh handler.
	secondUniqueId := "5002"
	deliveredC := make(chan string, 1)
	suite.chargePoint.SetRequestHandler(func(request ocpp.Request, requestId string, action string) {
		deliveredC <- requestId
	})
	secondCall := fmt.Sprintf(`[2,"%v","%v",{"mockValue":"%v"}]`, secondUniqueId, MockFeatureName, mockValue)
	err = suite.mockClient.MessageHandler([]byte(secondCall))
	assert.Nil(t, err)
	select {
	case id := <-deliveredC:
		assert.Equal(t, secondUniqueId, id)
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for subsequent message delivery")
	}
}

// TestClientResponseHandlerPanicRecovered mirrors the request-handler case
// for a panicking responseHandler (Kind=response). Action is expected to be
// "" for this Kind: a CALL_RESULT does not carry an Action, and the design
// principle restricts recovered context to what's locally available at the
// invocation site (callResult.UniqueId), not a backfilled lookup.
func (suite *OcppJTestSuite) TestClientResponseHandlerPanicRecovered() {
	t := suite.T()
	mockUniqueId := "5101"
	mockValue := "someValue"
	pendingRequest := newMockRequest("testValue")
	mockCallResult := fmt.Sprintf(`[3,"%v",{"mockValue":"%v"}]`, mockUniqueId, mockValue)
	panicValue := fmt.Errorf("boom: response handler panic")

	panicC := make(chan ocppj.HandlerPanic, 4)
	suite.chargePoint.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		panicC <- hp
	})
	suite.chargePoint.SetResponseHandler(func(confirmation ocpp.Response, requestId string) {
		panic(panicValue)
	})
	suite.mockClient.On("Start", mock.AnythingOfType("string")).Return(nil)
	suite.mockClient.On("Write", mock.Anything).Return(nil)
	require.NoError(t, suite.chargePoint.Start("someUrl"))
	suite.chargePoint.RequestState.AddPendingRequest(mockUniqueId, pendingRequest)

	err := suite.mockClient.MessageHandler([]byte(mockCallResult))
	assert.Nil(t, err)

	var hp ocppj.HandlerPanic
	select {
	case hp = <-panicC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for panic callback")
	}
	assert.Equal(t, ocppj.ResponseHandlerKind, hp.Kind)
	assert.Equal(t, "", hp.Action)
	assert.Equal(t, mockUniqueId, hp.RequestID)
	assert.Equal(t, "", hp.ClientID)
	assert.Equal(t, panicValue, hp.Value)
	assert.NotEmpty(t, hp.Stack)

	// The callback must fire exactly once for this single panic.
	select {
	case extra := <-panicC:
		t.Fatalf("callback fired more than once: %+v", extra)
	default:
	}

	// A subsequent CALL_RESULT must still be delivered to a fresh response handler.
	// The first pending request slot must be freed explicitly first: clientState's
	// AddPendingRequest is a no-op while a request is already pending (it supports
	// only one in-flight request at a time), and dispatcher.CompleteRequest - which
	// would normally free the slot - returns early here because this test drives
	// RequestState directly without ever pushing a bundle onto the client's request
	// queue (mirroring TestChargePointCallResultHandler), so CompleteRequest finds
	// an empty queue and never calls DeletePendingRequest. Without this explicit
	// delete, the second AddPendingRequest below would be silently ignored and the
	// second CALL_RESULT would be rejected before reaching the handler, timing out
	// even against a correct implementation.
	suite.chargePoint.RequestState.DeletePendingRequest(mockUniqueId)
	secondUniqueId := "5102"
	suite.chargePoint.RequestState.AddPendingRequest(secondUniqueId, newMockRequest("testValue2"))
	deliveredC := make(chan string, 1)
	suite.chargePoint.SetResponseHandler(func(confirmation ocpp.Response, requestId string) {
		deliveredC <- requestId
	})
	secondCallResult := fmt.Sprintf(`[3,"%v",{"mockValue":"%v"}]`, secondUniqueId, mockValue)
	err = suite.mockClient.MessageHandler([]byte(secondCallResult))
	assert.Nil(t, err)
	select {
	case id := <-deliveredC:
		assert.Equal(t, secondUniqueId, id)
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for subsequent response delivery")
	}
}

// TestClientErrorHandlerPanicRecovered mirrors the above for a panicking
// errorHandler (Kind=error). A non-string panic value is used to prove Value
// round-trips any type, not just strings.
func (suite *OcppJTestSuite) TestClientErrorHandlerPanicRecovered() {
	t := suite.T()
	mockUniqueId := "5201"
	mockErrorCode := ocppj.GenericError
	mockErrorDescription := "Mock Description"
	mockValue := "someValue"
	pendingRequest := newMockRequest("testValue")
	mockCallError := fmt.Sprintf(`[4,"%v","%v","%v",{"details":"%v"}]`, mockUniqueId, mockErrorCode, mockErrorDescription, mockValue)
	panicValue := 42

	panicC := make(chan ocppj.HandlerPanic, 4)
	suite.chargePoint.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		panicC <- hp
	})
	suite.chargePoint.SetErrorHandler(func(err *ocpp.Error, details interface{}) {
		panic(panicValue)
	})
	suite.mockClient.On("Start", mock.AnythingOfType("string")).Return(nil)
	suite.mockClient.On("Write", mock.Anything).Return(nil)
	require.NoError(t, suite.chargePoint.Start("someUrl"))
	suite.chargePoint.RequestState.AddPendingRequest(mockUniqueId, pendingRequest)

	err := suite.mockClient.MessageHandler([]byte(mockCallError))
	assert.Nil(t, err)

	var hp ocppj.HandlerPanic
	select {
	case hp = <-panicC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for panic callback")
	}
	assert.Equal(t, ocppj.ErrorHandlerKind, hp.Kind)
	assert.Equal(t, "", hp.Action)
	assert.Equal(t, mockUniqueId, hp.RequestID)
	assert.Equal(t, "", hp.ClientID)
	assert.Equal(t, panicValue, hp.Value)
	assert.NotEmpty(t, hp.Stack)

	// The callback must fire exactly once for this single panic.
	select {
	case extra := <-panicC:
		t.Fatalf("callback fired more than once: %+v", extra)
	default:
	}

	// A subsequent CALL_ERROR must still be delivered to a fresh error handler.
	// See the comment in TestClientResponseHandlerPanicRecovered: the first
	// pending request slot must be freed explicitly, since CompleteRequest never
	// ran against a populated queue in this test and AddPendingRequest is a no-op
	// while a request is already pending.
	suite.chargePoint.RequestState.DeletePendingRequest(mockUniqueId)
	secondUniqueId := "5202"
	suite.chargePoint.RequestState.AddPendingRequest(secondUniqueId, newMockRequest("testValue2"))
	deliveredC := make(chan string, 1)
	suite.chargePoint.SetErrorHandler(func(err *ocpp.Error, details interface{}) {
		deliveredC <- err.MessageId
	})
	secondCallError := fmt.Sprintf(`[4,"%v","%v","%v",{"details":"%v"}]`, secondUniqueId, mockErrorCode, mockErrorDescription, mockValue)
	err = suite.mockClient.MessageHandler([]byte(secondCallError))
	assert.Nil(t, err)
	select {
	case id := <-deliveredC:
		assert.Equal(t, secondUniqueId, id)
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for subsequent error delivery")
	}
}

// TestClientHandlerPanicRecoveredWithoutCallback asserts the default path
// (no SetOnHandlerPanic callback registered): the panic must still be
// recovered, and the read loop must still survive for later messages.
func (suite *OcppJTestSuite) TestClientHandlerPanicRecoveredWithoutCallback() {
	t := suite.T()
	mockUniqueId := "5301"
	mockValue := "someValue"
	mockCall := fmt.Sprintf(`[2,"%v","%v",{"mockValue":"%v"}]`, mockUniqueId, MockFeatureName, mockValue)

	// Deliberately do NOT call SetOnHandlerPanic: the default (nil callback)
	// path must still recover and must not let the panic escape this test.
	suite.chargePoint.SetRequestHandler(func(request ocpp.Request, requestId string, action string) {
		panic("boom: default path panic")
	})
	suite.mockClient.On("Start", mock.AnythingOfType("string")).Return(nil)
	suite.mockClient.On("Write", mock.Anything).Return(nil)
	require.NoError(t, suite.chargePoint.Start("someUrl"))

	require.NotPanics(t, func() {
		err := suite.mockClient.MessageHandler([]byte(mockCall))
		assert.Nil(t, err)
	})

	// Subsequent inbound message must still be delivered to a fresh handler.
	secondUniqueId := "5302"
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

// TestClientRequestHandlerPanicRecoveredCallbackAlsoPanics asserts that the
// SetOnHandlerPanic callback invocation is itself guarded: if the registered
// callback panics (in addition to the user request handler panicking), the
// client still does not crash, and the read loop survives - a subsequent
// inbound message is still delivered.
func (suite *OcppJTestSuite) TestClientRequestHandlerPanicRecoveredCallbackAlsoPanics() {
	t := suite.T()
	mockUniqueId := "5501"
	mockValue := "someValue"
	mockCall := fmt.Sprintf(`[2,"%v","%v",{"mockValue":"%v"}]`, mockUniqueId, MockFeatureName, mockValue)

	callbackRanC := make(chan struct{}, 1)
	suite.chargePoint.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		// Signal before panicking, so the test can still confirm the callback ran.
		callbackRanC <- struct{}{}
		panic("boom: panic callback itself panics")
	})
	suite.chargePoint.SetRequestHandler(func(request ocpp.Request, requestId string, action string) {
		panic("boom: request handler panic")
	})
	suite.mockClient.On("Start", mock.AnythingOfType("string")).Return(nil)
	suite.mockClient.On("Write", mock.Anything).Return(nil)
	require.NoError(t, suite.chargePoint.Start("someUrl"))

	require.NotPanics(t, func() {
		err := suite.mockClient.MessageHandler([]byte(mockCall))
		assert.Nil(t, err)
	})

	select {
	case <-callbackRanC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for panic callback to run")
	}

	// The read loop must have survived both the handler panic and the
	// callback's own panic: a subsequent inbound CALL is still delivered.
	secondUniqueId := "5502"
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

// TestClientNonPanicPathUnaffectedByHandlerPanicGuard asserts that, with a
// panic callback registered, a normal (non-panicking) handler still runs and
// its effects are observed, and the panic callback never fires.
func (suite *OcppJTestSuite) TestClientNonPanicPathUnaffectedByHandlerPanicGuard() {
	t := suite.T()
	mockUniqueId := "5401"
	mockValue := "someValue"
	mockCall := fmt.Sprintf(`[2,"%v","%v",{"mockValue":"%v"}]`, mockUniqueId, MockFeatureName, mockValue)

	panicC := make(chan ocppj.HandlerPanic, 4)
	handledC := make(chan string, 1)
	suite.chargePoint.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		panicC <- hp
	})
	suite.chargePoint.SetRequestHandler(func(request ocpp.Request, requestId string, action string) {
		handledC <- requestId
	})
	suite.mockClient.On("Start", mock.AnythingOfType("string")).Return(nil)
	suite.mockClient.On("Write", mock.Anything).Return(nil)
	require.NoError(t, suite.chargePoint.Start("someUrl"))

	err := suite.mockClient.MessageHandler([]byte(mockCall))
	assert.Nil(t, err)

	select {
	case id := <-handledC:
		assert.Equal(t, mockUniqueId, id)
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for normal handler invocation")
	}
	select {
	case hp := <-panicC:
		t.Fatalf("panic callback must not fire on the non-panic path, got: %+v", hp)
	case <-time.After(200 * time.Millisecond):
		// expected: no panic callback fired
	}
}

// ----------------------------------------------------------------------
// Server: requestHandler / responseHandler / errorHandler panics
// (cross-client isolation)
// ----------------------------------------------------------------------

// TestServerRequestHandlerPanicRecoveredCrossClientIsolation asserts that a
// panicking requestHandler for client A is recovered, the panic callback
// fires with ClientID=A, and a message from client B is still handled
// normally (isolation across clients). It also asserts that client A's own
// read path was not torn down by the recovery: after panicking once, a
// second message from client A is still handled normally.
func (suite *OcppJTestSuite) TestServerRequestHandlerPanicRecoveredCrossClientIsolation() {
	t := suite.T()
	clientA := "clientA"
	clientB := "clientB"
	mockUniqueIdA := "6001"
	mockUniqueIdB := "6002"
	secondUniqueIdA := "6003"
	mockValue := "someValue"
	panicValueA := "boom: request handler panic for client A"
	channelA := NewMockWebSocket(clientA)
	channelB := NewMockWebSocket(clientB)
	callA := fmt.Sprintf(`[2,"%v","%v",{"mockValue":"%v"}]`, mockUniqueIdA, MockFeatureName, mockValue)
	callB := fmt.Sprintf(`[2,"%v","%v",{"mockValue":"%v"}]`, mockUniqueIdB, MockFeatureName, mockValue)
	secondCallA := fmt.Sprintf(`[2,"%v","%v",{"mockValue":"%v"}]`, secondUniqueIdA, MockFeatureName, mockValue)

	panicC := make(chan ocppj.HandlerPanic, 4)
	handledBC := make(chan string, 1)
	handledA2C := make(chan string, 1)
	var clientAPanicked int32
	suite.centralSystem.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		panicC <- hp
	})
	suite.centralSystem.SetRequestHandler(func(client ws.Channel, request ocpp.Request, requestId string, action string) {
		if client.ID() == clientA {
			if atomic.CompareAndSwapInt32(&clientAPanicked, 0, 1) {
				panic(panicValueA)
			}
			handledA2C <- requestId
			return
		}
		handledBC <- requestId
	})
	// A panicking request handler now auto-replies with a CALL ERROR, so the
	// server writes to the peer during recovery.
	suite.mockServer.On("Write", mock.AnythingOfType("string"), mock.Anything).Return(nil)
	suite.mockServer.On("Start", mock.AnythingOfType("int"), mock.AnythingOfType("string")).Return()
	suite.centralSystem.Start(8887, "somePath")
	suite.serverDispatcher.CreateClient(clientA)
	suite.serverDispatcher.CreateClient(clientB)

	// Client A's handler panics; the server must survive.
	err := suite.mockServer.MessageHandler(channelA, []byte(callA))
	assert.Nil(t, err)

	var hp ocppj.HandlerPanic
	select {
	case hp = <-panicC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for panic callback")
	}
	assert.Equal(t, clientA, hp.ClientID)
	assert.Equal(t, ocppj.RequestHandlerKind, hp.Kind)
	assert.Equal(t, MockFeatureName, hp.Action)
	assert.Equal(t, mockUniqueIdA, hp.RequestID)
	assert.Equal(t, panicValueA, hp.Value)
	assert.NotEmpty(t, hp.Stack)

	// The callback must fire exactly once for this single panic.
	select {
	case extra := <-panicC:
		t.Fatalf("callback fired more than once: %+v", extra)
	default:
	}

	// Client A's server-side state must survive its handler panic. The
	// A-second-message check below is not sufficient on its own: an incoming
	// CALL needs no pending state, so a broken recover that also tore down A
	// (e.g. DeleteClient) would still pass it. Assert A's dispatcher state
	// explicitly.
	_, okA := suite.serverRequestMap.Get(clientA)
	assert.True(t, okA, "client A's server state must survive its handler panic")

	// Client B must still be handled normally (cross-client isolation).
	err = suite.mockServer.MessageHandler(channelB, []byte(callB))
	assert.Nil(t, err)
	select {
	case id := <-handledBC:
		assert.Equal(t, mockUniqueIdB, id)
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for client B's message to be handled")
	}

	// Client A's own read path must still work after its handler panicked
	// once: a second, non-panicking message from client A is still handled,
	// proving the recovery did not tear down client A's connection/read loop.
	err = suite.mockServer.MessageHandler(channelA, []byte(secondCallA))
	assert.Nil(t, err)
	select {
	case id := <-handledA2C:
		assert.Equal(t, secondUniqueIdA, id)
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for client A's second message to be handled")
	}
}

// TestServerResponseHandlerPanicRecoveredCrossClientIsolation mirrors the
// above for a panicking responseHandler. The panic value is an error (rather
// than a string, as used for the request-handler test) to prove Value
// round-trips any type. It also asserts that client A's own read path was
// not torn down: after panicking once, a second CALL_RESULT from client A is
// still handled.
func (suite *OcppJTestSuite) TestServerResponseHandlerPanicRecoveredCrossClientIsolation() {
	t := suite.T()
	clientA := "clientA"
	clientB := "clientB"
	mockUniqueIdA := "6101"
	mockUniqueIdB := "6102"
	secondUniqueIdA := "6103"
	mockValue := "someValue"
	panicValueA := fmt.Errorf("boom: response handler panic for client A")
	channelA := NewMockWebSocket(clientA)
	channelB := NewMockWebSocket(clientB)
	callResultA := fmt.Sprintf(`[3,"%v",{"mockValue":"%v"}]`, mockUniqueIdA, mockValue)
	callResultB := fmt.Sprintf(`[3,"%v",{"mockValue":"%v"}]`, mockUniqueIdB, mockValue)

	panicC := make(chan ocppj.HandlerPanic, 4)
	handledBC := make(chan string, 1)
	handledA2C := make(chan string, 1)
	var clientAPanicked int32
	suite.centralSystem.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		panicC <- hp
	})
	suite.centralSystem.SetResponseHandler(func(client ws.Channel, confirmation ocpp.Response, requestId string) {
		if client.ID() == clientA {
			if atomic.CompareAndSwapInt32(&clientAPanicked, 0, 1) {
				panic(panicValueA)
			}
			handledA2C <- requestId
			return
		}
		handledBC <- requestId
	})
	// A completed CALL_RESULT frees A's queue slot and signals the dispatcher
	// pump, which may dispatch A's next queued bundle (the second pending
	// request added below) and Write it out. Register a permissive Write
	// expectation so that (possibly async) dispatch cannot panic the pump
	// goroutine on an unexpected call.
	suite.mockServer.On("Write", mock.AnythingOfType("string"), mock.Anything).Return(nil)
	suite.mockServer.On("Start", mock.AnythingOfType("int"), mock.AnythingOfType("string")).Return()
	suite.centralSystem.Start(8887, "somePath")
	suite.serverDispatcher.CreateClient(clientA)
	suite.serverDispatcher.CreateClient(clientB)
	addMockPendingRequest(suite, newMockRequest("reqA"), mockUniqueIdA, clientA)
	addMockPendingRequest(suite, newMockRequest("reqB"), mockUniqueIdB, clientB)

	err := suite.mockServer.MessageHandler(channelA, []byte(callResultA))
	assert.Nil(t, err)

	var hp ocppj.HandlerPanic
	select {
	case hp = <-panicC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for panic callback")
	}
	assert.Equal(t, clientA, hp.ClientID)
	assert.Equal(t, ocppj.ResponseHandlerKind, hp.Kind)
	assert.Equal(t, "", hp.Action)
	assert.Equal(t, mockUniqueIdA, hp.RequestID)
	assert.Equal(t, panicValueA, hp.Value)
	assert.NotEmpty(t, hp.Stack)

	// The callback must fire exactly once for this single panic.
	select {
	case extra := <-panicC:
		t.Fatalf("callback fired more than once: %+v", extra)
	default:
	}

	// Client A's server-side state must survive its handler panic (asserted
	// before the second A message re-creates it via addMockPendingRequest).
	_, okA := suite.serverRequestMap.Get(clientA)
	assert.True(t, okA, "client A's server state must survive its handler panic")

	err = suite.mockServer.MessageHandler(channelB, []byte(callResultB))
	assert.Nil(t, err)
	select {
	case id := <-handledBC:
		assert.Equal(t, mockUniqueIdB, id)
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for client B's message to be handled")
	}

	// Client A's own read path must still work after its handler panicked
	// once: a second, non-panicking CALL_RESULT from client A is still
	// handled, proving the recovery did not tear down the connection.
	addMockPendingRequest(suite, newMockRequest("reqA2"), secondUniqueIdA, clientA)
	callResultA2 := fmt.Sprintf(`[3,"%v",{"mockValue":"%v"}]`, secondUniqueIdA, mockValue)
	err = suite.mockServer.MessageHandler(channelA, []byte(callResultA2))
	assert.Nil(t, err)
	select {
	case id := <-handledA2C:
		assert.Equal(t, secondUniqueIdA, id)
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for client A's second message to be handled")
	}
}

// TestServerErrorHandlerPanicRecoveredCrossClientIsolation mirrors the above
// for a panicking errorHandler. The panic value is an int (rather than a
// string or error, as used for the other two server tests) to prove Value
// round-trips any type. It also asserts that client A's own read path was
// not torn down: after panicking once, a second CALL_ERROR from client A is
// still handled.
func (suite *OcppJTestSuite) TestServerErrorHandlerPanicRecoveredCrossClientIsolation() {
	t := suite.T()
	clientA := "clientA"
	clientB := "clientB"
	mockUniqueIdA := "6201"
	mockUniqueIdB := "6202"
	secondUniqueIdA := "6203"
	mockErrorCode := ocppj.GenericError
	mockErrorDescription := "Mock Description"
	mockValue := "someValue"
	panicValueA := 77
	channelA := NewMockWebSocket(clientA)
	channelB := NewMockWebSocket(clientB)
	callErrorA := fmt.Sprintf(`[4,"%v","%v","%v",{"details":"%v"}]`, mockUniqueIdA, mockErrorCode, mockErrorDescription, mockValue)
	callErrorB := fmt.Sprintf(`[4,"%v","%v","%v",{"details":"%v"}]`, mockUniqueIdB, mockErrorCode, mockErrorDescription, mockValue)
	secondCallErrorA := fmt.Sprintf(`[4,"%v","%v","%v",{"details":"%v"}]`, secondUniqueIdA, mockErrorCode, mockErrorDescription, mockValue)

	panicC := make(chan ocppj.HandlerPanic, 4)
	handledBC := make(chan string, 1)
	handledA2C := make(chan string, 1)
	var clientAPanicked int32
	suite.centralSystem.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		panicC <- hp
	})
	suite.centralSystem.SetErrorHandler(func(client ws.Channel, err *ocpp.Error, details interface{}) {
		if client.ID() == clientA {
			if atomic.CompareAndSwapInt32(&clientAPanicked, 0, 1) {
				panic(panicValueA)
			}
			handledA2C <- err.MessageId
			return
		}
		handledBC <- err.MessageId
	})
	suite.mockServer.On("Write", mock.AnythingOfType("string"), mock.Anything).Return(nil)
	suite.mockServer.On("Start", mock.AnythingOfType("int"), mock.AnythingOfType("string")).Return(nil)
	suite.centralSystem.Start(8887, "somePath")
	suite.serverDispatcher.CreateClient(clientA)
	suite.serverDispatcher.CreateClient(clientB)
	addMockPendingRequest(suite, newMockRequest("reqA"), mockUniqueIdA, clientA)
	addMockPendingRequest(suite, newMockRequest("reqB"), mockUniqueIdB, clientB)

	err := suite.mockServer.MessageHandler(channelA, []byte(callErrorA))
	assert.Nil(t, err)

	var hp ocppj.HandlerPanic
	select {
	case hp = <-panicC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for panic callback")
	}
	assert.Equal(t, clientA, hp.ClientID)
	assert.Equal(t, ocppj.ErrorHandlerKind, hp.Kind)
	assert.Equal(t, "", hp.Action)
	assert.Equal(t, mockUniqueIdA, hp.RequestID)
	assert.Equal(t, panicValueA, hp.Value)
	assert.NotEmpty(t, hp.Stack)

	// The callback must fire exactly once for this single panic.
	select {
	case extra := <-panicC:
		t.Fatalf("callback fired more than once: %+v", extra)
	default:
	}

	// Client A's server-side state must survive its handler panic (asserted
	// before the second A message re-creates it via addMockPendingRequest).
	_, okA := suite.serverRequestMap.Get(clientA)
	assert.True(t, okA, "client A's server state must survive its handler panic")

	err = suite.mockServer.MessageHandler(channelB, []byte(callErrorB))
	assert.Nil(t, err)
	select {
	case id := <-handledBC:
		assert.Equal(t, mockUniqueIdB, id)
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for client B's message to be handled")
	}

	// Client A's own read path must still work after its handler panicked
	// once: a second, non-panicking CALL_ERROR from client A is still
	// handled, proving the recovery did not tear down the connection.
	addMockPendingRequest(suite, newMockRequest("reqA2"), secondUniqueIdA, clientA)
	err = suite.mockServer.MessageHandler(channelA, []byte(secondCallErrorA))
	assert.Nil(t, err)
	select {
	case id := <-handledA2C:
		assert.Equal(t, secondUniqueIdA, id)
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for client A's second message to be handled")
	}
}

// ----------------------------------------------------------------------
// Server: newClientHandler (connect) / disconnectedClientHandler (disconnect)
// ----------------------------------------------------------------------

// TestServerConnectHandlerPanicRecovered asserts that a panicking
// newClientHandler is recovered, the callback fires with Kind=connect, the
// connecting client's ID, and no Action/RequestID; and that the connection
// bookkeeping (which runs before the handler is invoked) was not skipped.
func (suite *OcppJTestSuite) TestServerConnectHandlerPanicRecovered() {
	t := suite.T()
	mockClientID := "connectClient"
	channel := NewMockWebSocket(mockClientID)
	panicValue := "boom: connect handler panic"

	panicC := make(chan ocppj.HandlerPanic, 4)
	suite.centralSystem.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		panicC <- hp
	})
	suite.centralSystem.SetNewClientHandler(func(client ws.Channel) {
		panic(panicValue)
	})
	suite.mockServer.On("Start", mock.AnythingOfType("int"), mock.AnythingOfType("string")).Return()
	suite.centralSystem.Start(8887, "somePath")

	require.NotPanics(t, func() {
		suite.mockServer.NewClientHandler(channel)
	})

	var hp ocppj.HandlerPanic
	select {
	case hp = <-panicC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for panic callback")
	}
	assert.Equal(t, mockClientID, hp.ClientID)
	assert.Equal(t, ocppj.ConnectHandlerKind, hp.Kind)
	assert.Equal(t, "", hp.Action)
	assert.Equal(t, "", hp.RequestID)
	assert.Equal(t, panicValue, hp.Value)
	assert.NotEmpty(t, hp.Stack)

	// Client state must still have been created despite the handler panic.
	_, ok := suite.serverRequestMap.Get(mockClientID)
	assert.True(t, ok)
}

// TestServerDisconnectHandlerPanicRecovered asserts that a panicking
// disconnectedClientHandler is recovered, the callback fires with
// Kind=disconnect, the disconnecting client's ID, and no Action/RequestID.
func (suite *OcppJTestSuite) TestServerDisconnectHandlerPanicRecovered() {
	t := suite.T()
	mockClientID := "disconnectClient"
	channel := NewMockWebSocket(mockClientID)
	panicValue := "boom: disconnect handler panic"

	connectedC := make(chan struct{}, 1)
	panicC := make(chan ocppj.HandlerPanic, 4)
	suite.centralSystem.SetNewClientHandler(func(client ws.Channel) {
		connectedC <- struct{}{}
	})
	suite.centralSystem.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		panicC <- hp
	})
	suite.centralSystem.SetDisconnectedClientHandler(func(client ws.Channel) {
		panic(panicValue)
	})
	suite.mockServer.On("Start", mock.AnythingOfType("int"), mock.AnythingOfType("string")).Return()
	suite.centralSystem.Start(8887, "somePath")
	suite.mockServer.NewClientHandler(channel)
	select {
	case <-connectedC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for connect handler")
	}

	require.NotPanics(t, func() {
		suite.mockServer.DisconnectedClientHandler(channel)
	})

	var hp ocppj.HandlerPanic
	select {
	case hp = <-panicC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for panic callback")
	}
	assert.Equal(t, mockClientID, hp.ClientID)
	assert.Equal(t, ocppj.DisconnectHandlerKind, hp.Kind)
	assert.Equal(t, "", hp.Action)
	assert.Equal(t, "", hp.RequestID)
	assert.Equal(t, panicValue, hp.Value)
	assert.NotEmpty(t, hp.Stack)
}

// TestServerHandlerPanicRecoveredWithoutCallback asserts the server's
// default path (no SetOnHandlerPanic callback registered): the panic for
// client A must still be recovered, and client B must still be handled.
func (suite *OcppJTestSuite) TestServerHandlerPanicRecoveredWithoutCallback() {
	t := suite.T()
	clientA := "clientA"
	clientB := "clientB"
	mockUniqueIdA := "6301"
	mockUniqueIdB := "6302"
	mockValue := "someValue"
	channelA := NewMockWebSocket(clientA)
	channelB := NewMockWebSocket(clientB)
	callA := fmt.Sprintf(`[2,"%v","%v",{"mockValue":"%v"}]`, mockUniqueIdA, MockFeatureName, mockValue)
	callB := fmt.Sprintf(`[2,"%v","%v",{"mockValue":"%v"}]`, mockUniqueIdB, MockFeatureName, mockValue)

	handledBC := make(chan string, 1)
	// Deliberately do NOT call SetOnHandlerPanic.
	suite.centralSystem.SetRequestHandler(func(client ws.Channel, request ocpp.Request, requestId string, action string) {
		if client.ID() == clientA {
			panic("boom: default path panic")
		}
		handledBC <- requestId
	})
	// A panicking request handler now auto-replies with a CALL ERROR.
	suite.mockServer.On("Write", mock.AnythingOfType("string"), mock.Anything).Return(nil)
	suite.mockServer.On("Start", mock.AnythingOfType("int"), mock.AnythingOfType("string")).Return()
	suite.centralSystem.Start(8887, "somePath")
	suite.serverDispatcher.CreateClient(clientA)
	suite.serverDispatcher.CreateClient(clientB)

	require.NotPanics(t, func() {
		err := suite.mockServer.MessageHandler(channelA, []byte(callA))
		assert.Nil(t, err)
	})

	err := suite.mockServer.MessageHandler(channelB, []byte(callB))
	assert.Nil(t, err)
	select {
	case id := <-handledBC:
		assert.Equal(t, mockUniqueIdB, id)
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for client B's message to be handled")
	}
}

// TestServerRequestHandlerPanicRecoveredCallbackAlsoPanics asserts that the
// server's SetOnHandlerPanic callback invocation is itself guarded: if the
// callback panics (in addition to the request handler panicking for client
// A), the server still does not crash, and client B is still handled
// normally.
func (suite *OcppJTestSuite) TestServerRequestHandlerPanicRecoveredCallbackAlsoPanics() {
	t := suite.T()
	clientA := "clientA"
	clientB := "clientB"
	mockUniqueIdA := "6501"
	mockUniqueIdB := "6502"
	mockValue := "someValue"
	channelA := NewMockWebSocket(clientA)
	channelB := NewMockWebSocket(clientB)
	callA := fmt.Sprintf(`[2,"%v","%v",{"mockValue":"%v"}]`, mockUniqueIdA, MockFeatureName, mockValue)
	callB := fmt.Sprintf(`[2,"%v","%v",{"mockValue":"%v"}]`, mockUniqueIdB, MockFeatureName, mockValue)

	callbackRanC := make(chan struct{}, 1)
	handledBC := make(chan string, 1)
	suite.centralSystem.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		// Signal before panicking, so the test can still confirm the callback ran.
		callbackRanC <- struct{}{}
		panic("boom: server panic callback itself panics")
	})
	suite.centralSystem.SetRequestHandler(func(client ws.Channel, request ocpp.Request, requestId string, action string) {
		if client.ID() == clientA {
			panic("boom: request handler panic for client A")
		}
		handledBC <- requestId
	})
	// A panicking request handler now auto-replies with a CALL ERROR.
	suite.mockServer.On("Write", mock.AnythingOfType("string"), mock.Anything).Return(nil)
	suite.mockServer.On("Start", mock.AnythingOfType("int"), mock.AnythingOfType("string")).Return()
	suite.centralSystem.Start(8887, "somePath")
	suite.serverDispatcher.CreateClient(clientA)
	suite.serverDispatcher.CreateClient(clientB)

	require.NotPanics(t, func() {
		err := suite.mockServer.MessageHandler(channelA, []byte(callA))
		assert.Nil(t, err)
	})

	select {
	case <-callbackRanC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for panic callback to run")
	}

	// The server must have survived both the handler panic and the
	// callback's own panic: client B is still handled normally.
	err := suite.mockServer.MessageHandler(channelB, []byte(callB))
	assert.Nil(t, err)
	select {
	case id := <-handledBC:
		assert.Equal(t, mockUniqueIdB, id)
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for client B's message to be handled")
	}
}

// ----------------------------------------------------------------------
// Request-handler panic -> automatic CALL ERROR reply (issue #205)
// ----------------------------------------------------------------------

// assertCallError parses a raw OCPP-J frame and asserts it is a CALL ERROR
// (message type 4) for the given unique id carrying the InternalError code.
func assertCallError(t require.TestingT, data []byte, wantUniqueID string) {
	var frame []interface{}
	require.NoError(t, json.Unmarshal(data, &frame))
	require.GreaterOrEqual(t, len(frame), 4, "CALL ERROR frame must have at least 4 fields")
	assert.EqualValues(t, ocppj.CALL_ERROR, frame[0], "message type must be CALL_ERROR (4)")
	assert.Equal(t, wantUniqueID, frame[1], "CALL ERROR must carry the request's unique id")
	assert.Equal(t, string(ocppj.InternalError), frame[2], "CALL ERROR code must be InternalError")
}

// TestClientRequestHandlerPanicSendsCallError asserts that when the client's
// request handler panics, the client automatically replies to the server with a
// CALL ERROR (InternalError) for that request, so the server is not left
// awaiting a response that will never come.
func (suite *OcppJTestSuite) TestClientRequestHandlerPanicSendsCallError() {
	t := suite.T()
	mockUniqueId := "7001"
	mockValue := "someValue"
	mockCall := fmt.Sprintf(`[2,"%v","%v",{"mockValue":"%v"}]`, mockUniqueId, MockFeatureName, mockValue)

	writtenC := make(chan []byte, 1)
	suite.chargePoint.SetRequestHandler(func(request ocpp.Request, requestId string, action string) {
		panic("boom: request handler panic")
	})
	suite.mockClient.On("Start", mock.AnythingOfType("string")).Return(nil)
	suite.mockClient.On("Write", mock.Anything).Run(func(args mock.Arguments) {
		writtenC <- args.Get(0).([]byte)
	}).Return(nil)
	require.NoError(t, suite.chargePoint.Start("someUrl"))

	err := suite.mockClient.MessageHandler([]byte(mockCall))
	assert.Nil(t, err)

	select {
	case data := <-writtenC:
		assertCallError(t, data, mockUniqueId)
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for the automatic CALL ERROR")
	}
}

// TestServerRequestHandlerPanicSendsCallError asserts that when the server's
// request handler panics, the server automatically replies to that charge point
// with a CALL ERROR (InternalError) for the request.
func (suite *OcppJTestSuite) TestServerRequestHandlerPanicSendsCallError() {
	t := suite.T()
	clientID := "panicClient"
	mockUniqueId := "7101"
	mockValue := "someValue"
	channel := NewMockWebSocket(clientID)
	mockCall := fmt.Sprintf(`[2,"%v","%v",{"mockValue":"%v"}]`, mockUniqueId, MockFeatureName, mockValue)

	writtenC := make(chan []byte, 1)
	suite.centralSystem.SetRequestHandler(func(client ws.Channel, request ocpp.Request, requestId string, action string) {
		panic("boom: request handler panic")
	})
	suite.mockServer.On("Start", mock.AnythingOfType("int"), mock.AnythingOfType("string")).Return()
	suite.mockServer.On("Write", clientID, mock.Anything).Run(func(args mock.Arguments) {
		writtenC <- args.Get(1).([]byte)
	}).Return(nil)
	suite.centralSystem.Start(8887, "somePath")
	suite.serverDispatcher.CreateClient(clientID)

	err := suite.mockServer.MessageHandler(channel, []byte(mockCall))
	assert.Nil(t, err)

	select {
	case data := <-writtenC:
		assertCallError(t, data, mockUniqueId)
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for the automatic CALL ERROR")
	}
}
