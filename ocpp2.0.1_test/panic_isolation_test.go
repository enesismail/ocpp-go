package ocpp2_test

import (
	"fmt"
	"sync/atomic"
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
	_, err = suite.ocppjClient.SendRequest(provisioning.NewBootNotificationRequest(reason, chargePointModel, chargePointVendor))
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
// the incomingResponse arm of cs.incoming's switch passes an empty string
// as RecoverPanicGoroutine's requestID argument, regardless of what
// incoming.requestID holds, exactly mirroring the merged ocpp1.6
// charge_point.go behavior.
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
// ERROR branch (ocpp2.0.1/charging_station.go, `case incomingError:` of the
// switch on cs.incoming's kind) — is recovered. This is the symmetric client-side
// companion of the server error-callback test: an impl guarding only the
// confirmation branch would leave this crash vector open. Unlike the
// confirmation branch, the error branch DOES carry the request id
// (protoError.(*ocpp.Error).MessageId), so RequestID is asserted. The loop
// must survive (a second request's callback still fires).
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

// TestClientRequestHandlerPanicRecoveredFacade guards the CRITICAL HAZARD
// documented in tasks/s3b-2.0.1-ordering-unification.md ("THE CRITICAL
// HAZARD - do not miss this"): moving handleIncomingRequest off the ws read
// goroutine (which S3b's channel-unification change does) silently removes
// its panic isolation unless the new request arm in asyncCallbackHandler
// adds its own guard - specifically
// `defer cs.client.RecoverPanicGoroutine(ocppj.RequestHandlerKind, msg.action, msg.requestID, true)`.
//
// IMPORTANT: this test is expected to be GREEN TODAY, and that is correct,
// not a mistake. Today ocpp2.0.1/v2.go:387 wires
// SetRequestHandler(cs.handleIncomingRequest) DIRECTLY, so the request is
// still invoked from inside ocppj's own read-loop guard
// (ocppj/client.go:336-342's recoverHandler, wrapping the CALL branch of
// ocppMessageHandler) - a panic there is already recovered, reported, and
// answered with a CALL ERROR by ocppj itself, with no facade involvement at
// all. This test is therefore a REGRESSION GUARD, not a red-first test: its
// job is to fail LATER, the moment S3b moves handleIncomingRequest onto the
// facade's own goroutine (via the unified cs.incoming channel) without also
// adding the RecoverPanicGoroutine guard the spec mandates. That omission
// is called out in the spec as "the spec's single biggest hazard" and would
// silently reintroduce exactly the crash class S6 was created to fix - a
// charging station whose request handler panics taking down the process.
//
// All FOUR assertions below matter and must stay together: (a) the process
// must not crash, (b) SetOnHandlerPanic must report Kind=RequestHandlerKind
// with the correct Action/RequestID, (c) the peer must still receive a
// CALL ERROR(InternalError) on the wire, and (d) the drain loop must survive
// so later messages are still handled. A version that only checked (a)/(b)
// would go green post-change even with sendCallError wired to false, silently
// leaving the CSMS to time out on every panicking request - (c) is what
// exercises that argument. A version that checked (a)/(b)/(c) but not (d)
// would go green on a guard that recovers correctly yet kills the drain loop
// - see (d)'s comment for that exact botch.
func (suite *OcppV2TestSuite) TestClientRequestHandlerPanicRecoveredFacade() {
	t := suite.T()
	writtenC := make(chan []byte, 8)
	// No deterministic message-ID generator here: this test sends no outbound
	// request, so nothing ever calls the generator. Both inbound CALLs carry
	// explicit ids ("reqpanic-call-1"/"-2") and the CALL ERROR and CALL RESULT
	// echo those back, so the assertions below do not depend on generated ids.
	l2StartStandaloneChargingStation(suite, writtenC)

	panicValue := "boom: OnChangeAvailability panic"
	messageId := "reqpanic-call-1"
	operationalStatus := availability.OperationalStatusOperative

	// Deliberately NOT a testify mock - see lightweightAvailabilityListener's
	// doc comment (inbound_ordering_test.go) for why.
	//
	// Only the FIRST inbound request panics; the second is served normally,
	// so step (d) below can prove the request-handling path survived. The
	// counter is atomic because the two invocations are not guaranteed to
	// share a goroutine across the change: today both run inline on the
	// delivering goroutine, but post-S3b both run on asyncCallbackHandler's.
	var callCount atomic.Int32
	changeAvailabilityResponse := availability.NewChangeAvailabilityResponse(availability.ChangeAvailabilityStatusAccepted)
	listener := &lightweightAvailabilityListener{
		onChangeAvailability: func(request *availability.ChangeAvailabilityRequest) (*availability.ChangeAvailabilityResponse, error) {
			if callCount.Add(1) == 1 {
				panic(panicValue)
			}
			return changeAvailabilityResponse, nil
		},
	}
	suite.chargingStation.SetAvailabilityHandler(listener)

	panicC := make(chan ocppj.HandlerPanic, 1)
	suite.chargingStation.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		panicC <- hp
	})

	callJson := fmt.Sprintf(`[2,"%v","%v",{"operationalStatus":"%v"}]`, messageId, availability.ChangeAvailabilityFeatureName, operationalStatus)
	err := suite.mockWsClient.MessageHandler([]byte(callJson))
	require.NoError(t, err)

	// (a) + (b): the panic must be recovered (no crash) and reported.
	var hp ocppj.HandlerPanic
	select {
	case hp = <-panicC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for panic callback")
	}
	assert.Equal(t, ocppj.RequestHandlerKind, hp.Kind)
	assert.Equal(t, "", hp.ClientID)
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

	// (c): the charging station must still reply with the auto CALL
	// ERROR(InternalError) in place of the crashed response, so the peer is
	// not left awaiting one.
	errorDescription := "internal error while handling request"
	expectedErrorJson := []byte(fmt.Sprintf(`[4,"%v","%v","%v",{}]`, messageId, ocppj.InternalError, errorDescription))
	select {
	case written := <-writtenC:
		assert.Equal(t, expectedErrorJson, written)
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for the auto CALL ERROR")
	}

	// (d) CRUCIAL: prove the request-handling path SURVIVED the panic. A
	// second, non-panicking inbound CALL must still be handled and answered.
	// Mirrors the step the 1.6 original labels "Crucial"
	// (ocpp1.6_test/inbound_ordering_test.go:504-523) and the same probe in
	// this file's response/error siblings (:263-275, :504-516).
	//
	// Without this, a guard that recovers correctly but KILLS THE DRAIN LOOP
	// passes (a)+(b)+(c) and ships a silently-dead facade. The concrete
	// botch is writing the spec's mandated guard without its
	// immediately-invoked wrapper:
	//
	//	case incomingRequest:
	//	    defer cs.client.RecoverPanicGoroutine(...)   // WRONG: function-scoped
	//	    cs.handleIncomingRequest(...)
	//
	// A defer in a case arm belongs to asyncCallbackHandler, not the arm, so
	// the panic unwinds the WHOLE handler: recover() stops it (with correct
	// action/requestID, since defer args evaluate at registration - so (b)
	// still passes) and emits the CALLERROR (c still passes), then the
	// function RETURNS and `defer close(handlerDone)` fires. Every later
	// response, error and inbound CALL for this station is then dropped
	// silently. It is deterministic, so -count=N cannot catch it, and each
	// suite test gets a fresh station so nothing downstream notices either.
	//
	// Post-change a dead loop leaves this CALL sitting in cs.incoming with
	// nothing to drain it, so the wait below times out at panicWaitTimeout -
	// a bounded red, not a hang.
	secondMessageId := "reqpanic-call-2"
	secondCallJson := fmt.Sprintf(`[2,"%v","%v",{"operationalStatus":"%v"}]`, secondMessageId, availability.ChangeAvailabilityFeatureName, operationalStatus)
	err = suite.mockWsClient.MessageHandler([]byte(secondCallJson))
	require.NoError(t, err)

	expectedResultJson := []byte(fmt.Sprintf(`[3,"%v",{"status":"%v"}]`, secondMessageId, availability.ChangeAvailabilityStatusAccepted))
	select {
	case written := <-writtenC:
		assert.Equal(t, expectedResultJson, written)
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for the second request's response; the request-handling path did not survive the panic")
	}
	assert.Equal(t, int32(2), callCount.Load(), "the availability handler must have been invoked exactly twice")
}
