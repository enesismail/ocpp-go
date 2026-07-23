package ocpp16_test

import (
	"fmt"
	"time"

	"github.com/enesismail/ocpp-go/ocpp"
	"github.com/enesismail/ocpp-go/ocpp1.6/core"
	"github.com/enesismail/ocpp-go/ocpp1.6/types"
	"github.com/enesismail/ocpp-go/ocppj"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// This file covers the S1 part 2 gap described in
// tasks/S1-facade-goroutine-fix.md: the ocpp1.6 central-system (server) facade
// runs each inbound handler/callback in its OWN goroutine
// (ocpp1.6/central_system.go), so the ocppj-layer recover() on the read loop
// cannot reach a panic there. These tests drive a panic through the full
// ocpp1.6 facade (not just the raw ocppj layer, which the existing S1 tests
// already cover) and assert it is recovered, reported via SetOnHandlerPanic,
// and - for an inbound request - answered with a CALL ERROR(InternalError) so
// the peer isn't left to time out.

// panicWaitTimeout bounds how long a test waits for an async signal before
// failing, so a broken implementation produces a fast, deterministic test
// failure instead of a hang.
const panicWaitTimeout = 2 * time.Second

// TestServerRequestHandlerPanicRecoveredFacade asserts that a panic in a
// server-side (central system) request handler - which the S1 facade fix runs
// in its own goroutine - is recovered: the process survives, SetOnHandlerPanic
// fires with Kind=request and the right clientID/Action/RequestID, and the
// charge point receives a CALL ERROR(InternalError) for the request instead
// of being left to time out.
func (suite *OcppV16TestSuite) TestServerRequestHandlerPanicRecoveredFacade() {
	t := suite.T()
	wsId := "test_id"
	messageId := defaultMessageId
	wsUrl := "someUrl"
	chargePointModel := "model1"
	chargePointVendor := "ABL"
	panicValue := "boom: OnBootNotification panic"
	requestJson := fmt.Sprintf(`[2,"%v","%v",{"chargePointModel":"%v","chargePointVendor":"%v"}]`, messageId, core.BootNotificationFeatureName, chargePointModel, chargePointVendor)
	errorDescription := "internal error while handling request"
	errorJson := fmt.Sprintf(`[4,"%v","%v","%v",{}]`, messageId, ocppj.InternalError, errorDescription)
	channel := NewMockWebSocket(wsId)

	// The registered core handler panics instead of producing a confirmation.
	coreListener := &MockCentralSystemCoreListener{}
	coreListener.On("OnBootNotification", mock.AnythingOfType("string"), mock.Anything).Run(func(args mock.Arguments) {
		panic(panicValue)
	})

	panicC := make(chan ocppj.HandlerPanic, 4)
	suite.centralSystem.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		panicC <- hp
	})

	// The central system must reply to the charge point with a CALL
	// ERROR(InternalError) in place of the crashed response.
	setupDefaultCentralSystemHandlers(suite, coreListener, expectedCentralSystemOptions{clientId: wsId, rawWrittenMessage: []byte(errorJson), forwardWrittenMessage: true})
	setupDefaultChargePointHandlers(suite, nil, expectedChargePointOptions{serverUrl: wsUrl, clientId: wsId, createChannelOnStart: true, channel: channel, rawWrittenMessage: []byte(requestJson), forwardWrittenMessage: true})

	errC := make(chan *ocpp.Error, 1)
	suite.ocppjChargePoint.SetErrorHandler(func(err *ocpp.Error, details interface{}) {
		errC <- err
	})

	// Start
	suite.centralSystem.Start(8887, "somePath")
	err := suite.chargePoint.Start(wsUrl)
	require.Nil(t, err)

	// The charge point sends a BootNotification through the real dispatch queue.
	// The central system's handler panics, is recovered, and replies with a
	// CALL ERROR(InternalError) that the charge point's error handler receives
	// via completion ownership.
	_, err = suite.ocppjChargePoint.SendRequest(core.NewBootNotificationRequest(chargePointModel, chargePointVendor))
	assert.Nil(t, err)

	// 1. The panic must be recovered (no crash) and reported.
	var hp ocppj.HandlerPanic
	select {
	case hp = <-panicC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for panic callback")
	}
	assert.Equal(t, ocppj.RequestHandlerKind, hp.Kind)
	assert.Equal(t, wsId, hp.ClientID)
	assert.Equal(t, core.BootNotificationFeatureName, hp.Action)
	assert.Equal(t, messageId, hp.RequestID)
	assert.Equal(t, panicValue, hp.Value)
	assert.NotEmpty(t, hp.Stack)

	// The callback must fire exactly once for this single panic.
	select {
	case extra := <-panicC:
		t.Fatalf("callback fired more than once: %+v", extra)
	default:
	}

	// 2. The charge point must receive the auto CALL ERROR(InternalError) for
	// the request whose handler panicked.
	select {
	case ocppErr := <-errC:
		assert.Equal(t, ocppj.InternalError, ocppErr.Code)
		assert.Equal(t, errorDescription, ocppErr.Description)
		assert.Equal(t, messageId, ocppErr.MessageId)
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for the CALL ERROR on the charge point")
	}
}

// TestServerResponseCallbackPanicRecoveredFacade asserts that a panic in the
// user-provided response callback for a central-system-initiated request
// (ChangeAvailability) - which the S1 facade fix runs in its own goroutine -
// is recovered: the process survives and SetOnHandlerPanic fires with
// Kind=response and the right clientID/Action/RequestID.
func (suite *OcppV16TestSuite) TestServerResponseCallbackPanicRecoveredFacade() {
	t := suite.T()
	wsId := "test_id"
	messageId := defaultMessageId
	wsUrl := "someUrl"
	connectorId := 1
	availabilityType := core.AvailabilityTypeOperative
	status := core.AvailabilityStatusAccepted
	panicValue := "boom: ChangeAvailability response callback panic"
	requestJson := fmt.Sprintf(`[2,"%v","%v",{"connectorId":%v,"type":"%v"}]`, messageId, core.ChangeAvailabilityFeatureName, connectorId, availabilityType)
	responseJson := fmt.Sprintf(`[3,"%v",{"status":"%v"}]`, messageId, status)
	changeAvailabilityConfirmation := core.NewChangeAvailabilityConfirmation(status)
	channel := NewMockWebSocket(wsId)

	panicC := make(chan ocppj.HandlerPanic, 4)
	suite.centralSystem.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		panicC <- hp
	})

	coreListener := &MockChargePointCoreListener{}
	coreListener.On("OnChangeAvailability", mock.Anything).Return(changeAvailabilityConfirmation, nil).Run(func(args mock.Arguments) {
		request, ok := args.Get(0).(*core.ChangeAvailabilityRequest)
		require.NotNil(t, request)
		require.True(t, ok)
		assert.Equal(t, connectorId, request.ConnectorId)
		assert.Equal(t, availabilityType, request.Type)
	})
	setupDefaultCentralSystemHandlers(suite, nil, expectedCentralSystemOptions{clientId: wsId, rawWrittenMessage: []byte(requestJson), forwardWrittenMessage: true})
	setupDefaultChargePointHandlers(suite, coreListener, expectedChargePointOptions{serverUrl: wsUrl, clientId: wsId, createChannelOnStart: true, channel: channel, rawWrittenMessage: []byte(responseJson), forwardWrittenMessage: true})

	// Run test
	suite.centralSystem.Start(8887, "somePath")
	err := suite.chargePoint.Start(wsUrl)
	require.Nil(t, err)
	err = suite.centralSystem.ChangeAvailability(wsId, func(confirmation *core.ChangeAvailabilityConfirmation, err error) {
		// This response callback panics; the S1 facade fix must recover it in
		// its own goroutine without crashing the process.
		panic(panicValue)
	}, connectorId, availabilityType)
	require.Nil(t, err)

	// The panic must be recovered (no crash) and reported with Kind=response.
	var hp ocppj.HandlerPanic
	select {
	case hp = <-panicC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for panic callback")
	}
	assert.Equal(t, ocppj.ResponseHandlerKind, hp.Kind)
	assert.Equal(t, wsId, hp.ClientID)
	assert.Equal(t, core.ChangeAvailabilityFeatureName, hp.Action)
	assert.Equal(t, messageId, hp.RequestID)
	assert.Equal(t, panicValue, hp.Value)
	assert.NotEmpty(t, hp.Stack)

	// The callback must fire exactly once for this single panic.
	select {
	case extra := <-panicC:
		t.Fatalf("callback fired more than once: %+v", extra)
	default:
	}
}

// TestClientResponseCallbackPanicRecoveredFacade asserts that a panic in the
// user-provided response callback for a charge-point-initiated request
// (BootNotification) - invoked inside the long-lived asyncCallbackHandler
// goroutine (ocpp1.6/charge_point.go) - is recovered: the process survives
// and SetOnHandlerPanic fires with Kind=response and the right Action,
// exactly once. Crucially, it also proves that asyncCallbackHandler's loop
// SURVIVES the panic: a second request sent afterwards, whose callback does
// not panic, must still have its callback invoked. This distinguishes the
// correct per-callback IIFE guard from a broken top-level defer, which would
// silently kill the loop after the first panic and leave the second
// callback never invoked.
func (suite *OcppV16TestSuite) TestClientResponseCallbackPanicRecoveredFacade() {
	t := suite.T()
	wsId := "test_id"
	wsUrl := "someUrl"
	chargePointModel := "model1"
	chargePointVendor := "ABL"
	interval := 60
	registrationStatus := core.RegistrationStatusAccepted
	currentTime := types.NewDateTime(time.Now())
	bootNotificationConfirmation := core.NewBootNotificationConfirmation(currentTime, interval, registrationStatus)
	panicValue := "boom: BootNotification response callback panic"
	channel := NewMockWebSocket(wsId)

	panicC := make(chan ocppj.HandlerPanic, 4)
	suite.chargePoint.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		panicC <- hp
	})

	coreListener := &MockCentralSystemCoreListener{}
	coreListener.On("OnBootNotification", mock.AnythingOfType("string"), mock.Anything).Return(bootNotificationConfirmation, nil)

	setupDefaultCentralSystemHandlers(suite, coreListener, expectedCentralSystemOptions{clientId: wsId, forwardWrittenMessage: true})
	setupDefaultChargePointHandlers(suite, nil, expectedChargePointOptions{serverUrl: wsUrl, clientId: wsId, createChannelOnStart: true, channel: channel, forwardWrittenMessage: true})

	// Run test
	suite.centralSystem.Start(8887, "somePath")
	err := suite.chargePoint.Start(wsUrl)
	require.Nil(t, err)

	// 1. Send a BootNotification whose response callback panics when the
	// confirmation arrives inside asyncCallbackHandler.
	err = suite.chargePoint.SendRequestAsync(core.NewBootNotificationRequest(chargePointModel, chargePointVendor), func(confirmation ocpp.Response, err error) {
		panic(panicValue)
	})
	require.Nil(t, err)

	// The panic must be recovered (no crash) and reported with Kind=response.
	var hp ocppj.HandlerPanic
	select {
	case hp = <-panicC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for panic callback")
	}
	assert.Equal(t, ocppj.ResponseHandlerKind, hp.Kind)
	assert.Equal(t, core.BootNotificationFeatureName, hp.Action)
	assert.Equal(t, panicValue, hp.Value)
	assert.NotEmpty(t, hp.Stack)

	// The callback must fire exactly once for this single panic.
	select {
	case extra := <-panicC:
		t.Fatalf("callback fired more than once: %+v", extra)
	default:
	}

	// 2. Crucial: prove asyncCallbackHandler's loop survived. Send a second
	// request whose (non-panicking) callback must still be invoked.
	secondCallbackC := make(chan ocpp.Response, 1)
	err = suite.chargePoint.SendRequestAsync(core.NewBootNotificationRequest(chargePointModel, chargePointVendor), func(confirmation ocpp.Response, err error) {
		secondCallbackC <- confirmation
	})
	require.Nil(t, err)

	select {
	case confirmation := <-secondCallbackC:
		assert.NotNil(t, confirmation)
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for the second callback; asyncCallbackHandler loop did not survive the panic")
	}
}
