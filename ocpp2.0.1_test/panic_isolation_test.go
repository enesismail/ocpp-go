package ocpp2_test

import (
	"fmt"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/enesismail/ocpp-go/ocpp"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/availability"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/provisioning"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/types"
	"github.com/enesismail/ocpp-go/ocppj"
)

// This file covers the S6 gap described in
// tasks/s6-ocpp201-panic-isolation.md: the ocpp2.0.1 facade has the identical
// unguarded-goroutine crash class that S6 fixes, mirroring the already-merged
// ocpp1.6 facade fix (see ocpp1.6_test/panic_isolation_test.go, the proven
// template these tests mirror). The ocpp2.0.1 csms (server) facade
// (ocpp2.0.1/csms.go) runs each inbound handler/callback in its OWN goroutine,
// and the ocpp2.0.1 chargingStation (client) facade
// (ocpp2.0.1/charging_station.go) runs each async response/error callback on
// its own long-lived asyncCallbackHandler goroutine. Neither is reachable by
// the ocppj-layer read-loop recover(). These tests drive a panic through the
// full ocpp2.0.1 facade (not just the raw ocppj layer) and assert it is
// recovered, reported via SetOnHandlerPanic, and - for an inbound request -
// answered with a CALL ERROR(InternalError) so the peer isn't left to time
// out.

// panicWaitTimeout bounds how long a test waits for an async signal before
// failing, so a broken implementation produces a fast, deterministic test
// failure instead of a hang.
const panicWaitTimeout = 2 * time.Second

// TestServerRequestHandlerPanicRecoveredFacade asserts that a panic in a
// server-side (CSMS) request handler - which the S6 facade fix runs in its
// own goroutine - is recovered: the process survives, SetOnHandlerPanic fires
// with Kind=request and the right chargingStationID/Action/RequestID, and the
// charging station receives a CALL ERROR(InternalError) for the request
// instead of being left to time out.
func (suite *OcppV2TestSuite) TestServerRequestHandlerPanicRecoveredFacade() {
	t := suite.T()
	wsId := "test_id"
	messageId := defaultMessageId
	wsUrl := "someUrl"
	reason := provisioning.BootReasonPowerUp
	chargePointModel := "model1"
	chargePointVendor := "ABL"
	panicValue := "boom: OnBootNotification panic"
	requestJson := fmt.Sprintf(`[2,"%v","%v",{"reason":"%v","chargingStation":{"model":"%v","vendorName":"%v"}}]`, messageId, provisioning.BootNotificationFeatureName, reason, chargePointModel, chargePointVendor)
	errorDescription := "internal error while handling request"
	errorJson := fmt.Sprintf(`[4,"%v","%v","%v",{}]`, messageId, ocppj.InternalError, errorDescription)
	channel := NewMockWebSocket(wsId)

	// The registered provisioning handler panics instead of producing a
	// confirmation.
	handler := &MockCSMSProvisioningHandler{}
	handler.On("OnBootNotification", mock.AnythingOfType("string"), mock.Anything).Run(func(args mock.Arguments) {
		panic(panicValue)
	})

	panicC := make(chan ocppj.HandlerPanic, 4)
	suite.csms.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		panicC <- hp
	})

	// The CSMS must reply to the charging station with a CALL
	// ERROR(InternalError) in place of the crashed response.
	setupDefaultCSMSHandlers(suite, expectedCSMSOptions{clientId: wsId, rawWrittenMessage: []byte(errorJson), forwardWrittenMessage: true}, handler)
	setupDefaultChargingStationHandlers(suite, expectedChargingStationOptions{serverUrl: wsUrl, clientId: wsId, createChannelOnStart: true, channel: channel, rawWrittenMessage: []byte(requestJson), forwardWrittenMessage: true})

	errC := make(chan *ocpp.Error, 1)
	suite.ocppjClient.SetErrorHandler(func(err *ocpp.Error, details interface{}) {
		errC <- err
	})

	// Start
	suite.csms.Start(8887, "somePath")
	err := suite.chargingStation.Start(wsUrl)
	require.Nil(t, err)

	// The charging station sends a BootNotification through the real dispatch
	// queue. The CSMS handler panics, is recovered, and replies with a
	// CALL ERROR(InternalError) that the charging station's error handler
	// receives via completion ownership.
	err = suite.ocppjClient.SendRequest(provisioning.NewBootNotificationRequest(reason, chargePointModel, chargePointVendor))
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
	assert.Equal(t, provisioning.BootNotificationFeatureName, hp.Action)
	assert.Equal(t, messageId, hp.RequestID)
	assert.Equal(t, panicValue, hp.Value)
	assert.NotEmpty(t, hp.Stack)

	// The callback must fire exactly once for this single panic.
	select {
	case extra := <-panicC:
		t.Fatalf("callback fired more than once: %+v", extra)
	default:
	}

	// 2. The charging station must receive the auto CALL ERROR(InternalError)
	// for the request whose handler panicked.
	select {
	case ocppErr := <-errC:
		assert.Equal(t, ocppj.InternalError, ocppErr.Code)
		assert.Equal(t, errorDescription, ocppErr.Description)
		assert.Equal(t, messageId, ocppErr.MessageId)
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for the CALL ERROR on the charging station")
	}
}

// TestServerResponseCallbackPanicRecoveredFacade asserts that a panic in the
// user-provided response callback for a CSMS-initiated request
// (ChangeAvailability) - which the S6 facade fix runs in its own goroutine -
// is recovered: the process survives and SetOnHandlerPanic fires with
// Kind=response and the right clientID/Action/RequestID.
func (suite *OcppV2TestSuite) TestServerResponseCallbackPanicRecoveredFacade() {
	t := suite.T()
	wsId := "test_id"
	messageId := defaultMessageId
	wsUrl := "someUrl"
	operationalStatus := availability.OperationalStatusOperative
	status := availability.ChangeAvailabilityStatusAccepted
	panicValue := "boom: ChangeAvailability response callback panic"
	requestJson := fmt.Sprintf(`[2,"%v","%v",{"operationalStatus":"%v"}]`, messageId, availability.ChangeAvailabilityFeatureName, operationalStatus)
	responseJson := fmt.Sprintf(`[3,"%v",{"status":"%v"}]`, messageId, status)
	changeAvailabilityConfirmation := availability.NewChangeAvailabilityResponse(status)
	channel := NewMockWebSocket(wsId)

	panicC := make(chan ocppj.HandlerPanic, 4)
	suite.csms.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		panicC <- hp
	})

	handler := &MockChargingStationAvailabilityHandler{}
	handler.On("OnChangeAvailability", mock.Anything).Return(changeAvailabilityConfirmation, nil).Run(func(args mock.Arguments) {
		request, ok := args.Get(0).(*availability.ChangeAvailabilityRequest)
		require.NotNil(t, request)
		require.True(t, ok)
		assert.Equal(t, operationalStatus, request.OperationalStatus)
	})
	setupDefaultCSMSHandlers(suite, expectedCSMSOptions{clientId: wsId, rawWrittenMessage: []byte(requestJson), forwardWrittenMessage: true})
	setupDefaultChargingStationHandlers(suite, expectedChargingStationOptions{serverUrl: wsUrl, clientId: wsId, createChannelOnStart: true, channel: channel, rawWrittenMessage: []byte(responseJson), forwardWrittenMessage: true}, handler)

	// Run test
	suite.csms.Start(8887, "somePath")
	err := suite.chargingStation.Start(wsUrl)
	require.Nil(t, err)
	err = suite.csms.ChangeAvailability(wsId, func(confirmation *availability.ChangeAvailabilityResponse, err error) {
		// This response callback panics; the S6 facade fix must recover it in
		// its own goroutine without crashing the process.
		panic(panicValue)
	}, operationalStatus)
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
	assert.Equal(t, availability.ChangeAvailabilityFeatureName, hp.Action)
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
// user-provided response callback for a charging-station-initiated request
// (BootNotification) - invoked inside the long-lived asyncCallbackHandler
// goroutine (ocpp2.0.1/charging_station.go) - is recovered: the process
// survives and SetOnHandlerPanic fires with Kind=response and the right
// Action, exactly once. Crucially, it also proves that asyncCallbackHandler's
// loop SURVIVES the panic: a second request sent afterwards, whose callback
// does not panic, must still have its callback invoked. This distinguishes
// the correct per-callback IIFE guard from a broken top-level defer, which
// would silently kill the loop after the first panic and leave the second
// callback never invoked.
//
// Note: per the S6 spec, hp.RequestID is intentionally NOT asserted here -
// responseHandler is a chan ocpp.Response carrying no request id at that
// point, exactly mirroring the merged ocpp1.6 charge_point.go behavior.
func (suite *OcppV2TestSuite) TestClientResponseCallbackPanicRecoveredFacade() {
	t := suite.T()
	wsId := "test_id"
	wsUrl := "someUrl"
	reason := provisioning.BootReasonPowerUp
	chargePointModel := "model1"
	chargePointVendor := "ABL"
	interval := 60
	registrationStatus := provisioning.RegistrationStatusAccepted
	currentTime := types.NewDateTime(time.Now())
	bootNotificationConfirmation := provisioning.NewBootNotificationResponse(currentTime, interval, registrationStatus)
	panicValue := "boom: BootNotification response callback panic"
	channel := NewMockWebSocket(wsId)

	panicC := make(chan ocppj.HandlerPanic, 4)
	suite.chargingStation.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		panicC <- hp
	})

	handler := &MockCSMSProvisioningHandler{}
	handler.On("OnBootNotification", mock.AnythingOfType("string"), mock.Anything).Return(bootNotificationConfirmation, nil)

	setupDefaultCSMSHandlers(suite, expectedCSMSOptions{clientId: wsId, forwardWrittenMessage: true}, handler)
	setupDefaultChargingStationHandlers(suite, expectedChargingStationOptions{serverUrl: wsUrl, clientId: wsId, createChannelOnStart: true, channel: channel, forwardWrittenMessage: true})

	// Run test
	suite.csms.Start(8887, "somePath")
	err := suite.chargingStation.Start(wsUrl)
	require.Nil(t, err)

	// 1. Send a BootNotification whose response callback panics when the
	// confirmation arrives inside asyncCallbackHandler.
	err = suite.chargingStation.SendRequestAsync(provisioning.NewBootNotificationRequest(reason, chargePointModel, chargePointVendor), func(confirmation ocpp.Response, err error) {
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
	assert.Equal(t, "", hp.ClientID)
	assert.Equal(t, provisioning.BootNotificationFeatureName, hp.Action)
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
	err = suite.chargingStation.SendRequestAsync(provisioning.NewBootNotificationRequest(reason, chargePointModel, chargePointVendor), func(confirmation ocpp.Response, err error) {
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

// TestServerErrorCallbackPanicRecoveredFacade asserts that a panic in the
// user-provided response callback for a CSMS-initiated request
// (ChangeAvailability) - invoked with (nil, err) from inside the
// handleIncomingError goroutine (csms.go:1034) when the charging station
// replies with a CALL ERROR instead of a confirmation - is recovered: the
// process survives and SetOnHandlerPanic fires with Kind=error and the right
// clientID/RequestID. This closes the coverage gap left by
// TestServerResponseCallbackPanicRecoveredFacade above, which only drives the
// (response, nil) callback path and would pass even if the error-callback
// goroutine (a separate, unguarded `go callback(nil, err)` site) were never
// guarded.
func (suite *OcppV2TestSuite) TestServerErrorCallbackPanicRecoveredFacade() {
	t := suite.T()
	wsId := "test_id"
	messageId := defaultMessageId
	wsUrl := "someUrl"
	operationalStatus := availability.OperationalStatusOperative
	panicValue := "boom: ChangeAvailability error callback panic"
	requestJson := fmt.Sprintf(`[2,"%v","%v",{"operationalStatus":"%v"}]`, messageId, availability.ChangeAvailabilityFeatureName, operationalStatus)
	channel := NewMockWebSocket(wsId)

	panicC := make(chan ocppj.HandlerPanic, 4)
	suite.csms.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		panicC <- hp
	})

	// The charging station's registered handler returns an *ocpp.Error
	// instead of a confirmation, so - per ocpp2.0.1/charging_station.go:
	// 558-565 - the station replies with that exact CALL ERROR. On the CSMS
	// side, this is delivered to the response callback as (nil, err) from
	// inside handleIncomingError's own goroutine.
	handler := &MockChargingStationAvailabilityHandler{}
	handler.On("OnChangeAvailability", mock.Anything).Return((*availability.ChangeAvailabilityResponse)(nil), ocpp.NewError(ocppj.GenericError, "boom-desc", "")).Run(func(args mock.Arguments) {
		request, ok := args.Get(0).(*availability.ChangeAvailabilityRequest)
		require.NotNil(t, request)
		require.True(t, ok)
		assert.Equal(t, operationalStatus, request.OperationalStatus)
	})
	setupDefaultCSMSHandlers(suite, expectedCSMSOptions{clientId: wsId, rawWrittenMessage: []byte(requestJson), forwardWrittenMessage: true})
	// Do not assert the exact CALL ERROR frame here (its description/code are
	// incidental to this test); just forward it on to the CSMS.
	setupDefaultChargingStationHandlers(suite, expectedChargingStationOptions{serverUrl: wsUrl, clientId: wsId, createChannelOnStart: true, channel: channel, forwardWrittenMessage: true}, handler)

	// Run test
	suite.csms.Start(8887, "somePath")
	err := suite.chargingStation.Start(wsUrl)
	require.Nil(t, err)
	err = suite.csms.ChangeAvailability(wsId, func(confirmation *availability.ChangeAvailabilityResponse, err error) {
		// The charging station replied with a CALL ERROR, so this callback
		// receives (nil, err). It panics; the S6 facade fix must recover it
		// in its own goroutine (handleIncomingError) without crashing the
		// process.
		panic(panicValue)
	}, operationalStatus)
	require.Nil(t, err)

	// The panic must be recovered (no crash) and reported with Kind=error.
	var hp ocppj.HandlerPanic
	select {
	case hp = <-panicC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for panic callback")
	}
	assert.Equal(t, ocppj.ErrorHandlerKind, hp.Kind)
	assert.Equal(t, wsId, hp.ClientID)
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

// TestServerCanceledCallbackPanicRecoveredFacade asserts that a panic in the
// user-provided response callback for a CSMS-initiated request
// (ChangeAvailability), invoked with (nil, err) when the request is canceled
// by the dispatcher's own request timeout - the identical unguarded
// `go callback(nil, err)` shape as handleIncomingError, but in
// handleCanceledRequest (csms.go:1043) - is recovered: the process survives
// and SetOnHandlerPanic fires with Kind=error and the right
// clientID/RequestID.
//
// The dispatcher timeout is configured directly on suite.serverDispatcher
// (an ocppj.ServerDispatcher, exposed on the suite and configurable before
// Start, exactly as ocppj/callback_panic_test.go's
// TestServerCancelHandlerPanicRecoveredOnTimeout already does at the raw
// ocppj layer) - this is the only mechanism in this harness that can drive a
// facade-level cancel/timeout. The charging station never receives (and so
// never responds to) the request, so the dispatcher's own timeout - not a
// simulated peer action - is what cancels it.
func (suite *OcppV2TestSuite) TestServerCanceledCallbackPanicRecoveredFacade() {
	t := suite.T()
	wsId := "test_id"
	messageId := defaultMessageId
	wsUrl := "someUrl"
	operationalStatus := availability.OperationalStatusOperative
	panicValue := "boom: ChangeAvailability canceled callback panic"
	requestJson := fmt.Sprintf(`[2,"%v","%v",{"operationalStatus":"%v"}]`, messageId, availability.ChangeAvailabilityFeatureName, operationalStatus)
	channel := NewMockWebSocket(wsId)

	// Configure a short dispatcher timeout BEFORE starting the CSMS (per
	// ServerDispatcher.SetTimeout's contract), so the CSMS-initiated request
	// below times out instead of waiting for a response this test
	// deliberately never delivers.
	suite.serverDispatcher.SetTimeout(300 * time.Millisecond)

	panicC := make(chan ocppj.HandlerPanic, 4)
	suite.csms.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		panicC <- hp
	})

	// The request is written to the network (and its bytes asserted below),
	// but deliberately NOT forwarded to the charging station, so no response
	// or error ever completes it - letting the dispatcher's own timeout
	// cancel it.
	setupDefaultCSMSHandlers(suite, expectedCSMSOptions{clientId: wsId, rawWrittenMessage: []byte(requestJson), forwardWrittenMessage: false})
	setupDefaultChargingStationHandlers(suite, expectedChargingStationOptions{serverUrl: wsUrl, clientId: wsId, createChannelOnStart: true, channel: channel})

	// Run test
	suite.csms.Start(8887, "somePath")
	err := suite.chargingStation.Start(wsUrl)
	require.Nil(t, err)
	err = suite.csms.ChangeAvailability(wsId, func(confirmation *availability.ChangeAvailabilityResponse, err error) {
		// The request is canceled (times out) before any response arrives,
		// so this callback receives (nil, err). It panics; the S6 facade fix
		// must recover it in its own goroutine (handleCanceledRequest)
		// without crashing the process.
		panic(panicValue)
	}, operationalStatus)
	require.Nil(t, err)

	// The panic must be recovered (no crash) and reported with Kind=error.
	var hp ocppj.HandlerPanic
	select {
	case hp = <-panicC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for panic callback")
	}
	assert.Equal(t, ocppj.ErrorHandlerKind, hp.Kind)
	assert.Equal(t, wsId, hp.ClientID)
	// The canceled-request guard (csms.go:1043) reports the feature name, unlike
	// the error-callback guard (csms.go:1034) which reports "" — assert it to
	// catch a swap/copy-paste between the two ErrorHandlerKind sites.
	assert.Equal(t, availability.ChangeAvailabilityFeatureName, hp.Action)
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

// TestClientErrorCallbackPanicRecoveredFacade asserts that a panic in the
// user-provided callback for a charging-station-initiated request that receives
// a CALL ERROR — invoked inside the long-lived asyncCallbackHandler goroutine's
// ERROR branch (ocpp2.0.1/charging_station.go, `case protoError := <-cs.errorHandler`)
// — is recovered. This is the symmetric client-side companion of the server
// error-callback test: an impl guarding only the confirmation branch would leave
// this crash vector open. Unlike the confirmation branch, the error branch DOES
// carry the request id (protoError.(*ocpp.Error).MessageId), so RequestID is
// asserted. The loop must survive (a second request's callback still fires).
func (suite *OcppV2TestSuite) TestClientErrorCallbackPanicRecoveredFacade() {
	t := suite.T()
	wsId := "test_id"
	wsUrl := "someUrl"
	reason := provisioning.BootReasonPowerUp
	chargePointModel := "model1"
	chargePointVendor := "ABL"
	panicValue := "boom: BootNotification error callback panic"
	channel := NewMockWebSocket(wsId)

	panicC := make(chan ocppj.HandlerPanic, 4)
	suite.chargingStation.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		panicC <- hp
	})

	// The CSMS handler returns an *ocpp.Error, so the charging station receives a
	// CALL ERROR and its async callback is invoked with (nil, err) on the error
	// branch of asyncCallbackHandler.
	handler := &MockCSMSProvisioningHandler{}
	handler.On("OnBootNotification", mock.AnythingOfType("string"), mock.Anything).
		Return((*provisioning.BootNotificationResponse)(nil), ocpp.NewError(ocppj.GenericError, "boom-desc", ""))

	setupDefaultCSMSHandlers(suite, expectedCSMSOptions{clientId: wsId, forwardWrittenMessage: true}, handler)
	setupDefaultChargingStationHandlers(suite, expectedChargingStationOptions{serverUrl: wsUrl, clientId: wsId, createChannelOnStart: true, channel: channel, forwardWrittenMessage: true})

	// Run test
	suite.csms.Start(8887, "somePath")
	err := suite.chargingStation.Start(wsUrl)
	require.Nil(t, err)

	// 1. Send a BootNotification whose callback panics when the CALL ERROR
	// arrives inside asyncCallbackHandler's error branch.
	err = suite.chargingStation.SendRequestAsync(provisioning.NewBootNotificationRequest(reason, chargePointModel, chargePointVendor), func(confirmation ocpp.Response, err error) {
		panic(panicValue)
	})
	require.Nil(t, err)

	var hp ocppj.HandlerPanic
	select {
	case hp = <-panicC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for panic callback")
	}
	assert.Equal(t, ocppj.ErrorHandlerKind, hp.Kind)
	assert.Equal(t, "", hp.ClientID)
	assert.Equal(t, defaultMessageId, hp.RequestID)
	assert.Equal(t, panicValue, hp.Value)
	assert.NotEmpty(t, hp.Stack)

	// The callback must fire exactly once for this single panic.
	select {
	case extra := <-panicC:
		t.Fatalf("callback fired more than once: %+v", extra)
	default:
	}

	// 2. Crucial: prove asyncCallbackHandler's loop survived the error-branch
	// panic. A second request's callback must still be invoked.
	secondCallbackC := make(chan struct{}, 1)
	err = suite.chargingStation.SendRequestAsync(provisioning.NewBootNotificationRequest(reason, chargePointModel, chargePointVendor), func(confirmation ocpp.Response, err error) {
		secondCallbackC <- struct{}{}
	})
	require.Nil(t, err)

	select {
	case <-secondCallbackC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for the second callback; asyncCallbackHandler loop did not survive the error-branch panic")
	}
}
