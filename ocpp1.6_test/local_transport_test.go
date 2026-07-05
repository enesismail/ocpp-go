package ocpp16_test

import (
	"errors"
	"fmt"
	"time"

	"github.com/enesismail/ocpp-go/ocpp"
	ocpp16 "github.com/enesismail/ocpp-go/ocpp1.6"
	"github.com/enesismail/ocpp-go/ocpp1.6/core"
	"github.com/enesismail/ocpp-go/ocppj"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file covers tests 5 and 6 (the facade-level tests) from
// tasks/b1-b2-local-transport-markers.md (B1/B2 — error-origin classification
// markers). Both tests deliberately reference the not-yet-existing
// ocppj.ErrLocalTransport sentinel, so this package fails to compile until B1
// lands; that is the intended red-first state.

// TestChargePointDisconnectDrainIsLocalTransport is test 5 (1.6 CSMS facade):
// a pending SendRequestAsync callback, drained because the charge point
// disconnected before any response arrived, must be classified
// ocppj.ErrLocalTransport. The drain (ocpp1.6/central_system.go:508, inside
// the wrapper registered by SetChargePointDisconnectedHandler) only runs at
// all if a disconnect handler was registered - required here - otherwise the
// pending callback would be dropped silently and there would be nothing to
// classify. Today (pre-B2) the drain builds an untagged
// ocpp.NewError(ocppj.GenericError, ...), so this assertion is expected to
// fail (red) until B2 tags it.
func (suite *OcppV16TestSuite) TestChargePointDisconnectDrainIsLocalTransport() {
	t := suite.T()
	wsId := "test_id"
	wsUrl := "someUrl"
	channel := NewMockWebSocket(wsId)

	setupDefaultChargePointHandlers(suite, nil, expectedChargePointOptions{serverUrl: wsUrl, clientId: wsId, createChannelOnStart: true, channel: channel})
	coreListener := &MockCentralSystemCoreListener{}
	setupDefaultCentralSystemHandlers(suite, coreListener, expectedCentralSystemOptions{clientId: wsId})

	// REQUIRED: the disconnect-drain only runs inside a registered handler
	// (see the spec's "Server disconnect-drain precondition"). We also use
	// this handler to observe that the drain path actually ran.
	disconnectedC := make(chan struct{}, 1)
	suite.centralSystem.SetChargePointDisconnectedHandler(func(chargePoint ocpp16.ChargePointConnection) {
		disconnectedC <- struct{}{}
	})

	// Start
	suite.centralSystem.Start(8887, "somePath")
	// Deferred immediately after Start so the CSMS dispatcher is stopped on
	// every exit path (including a t.Fatal/timeout below), not just the
	// success path - otherwise its dispatcher goroutine leaks into later
	// suite tests.
	defer suite.centralSystem.Stop()
	err := suite.chargePoint.Start(wsUrl)
	require.Nil(t, err)
	// The client endpoint isn't covered by the above: no existing test in
	// this suite calls chargePoint.Stop(), and setupDefaultChargePointHandlers
	// doesn't stub "Stop"/"IsConnected" on the mock. Stub them here and stop
	// it too - it runs a second, independent dispatcher goroutine that would
	// otherwise leak the same way. IsConnected must report false so
	// ocppj.Client.Stop() returns immediately instead of blocking on a
	// disconnected-handler signal this mock never fires.
	suite.mockWsClient.On("IsConnected").Return(false)
	suite.mockWsClient.On("Stop").Return()
	defer suite.chargePoint.Stop()

	type asyncResult struct {
		confirmation ocpp.Response
		err          error
	}
	resultC := make(chan asyncResult, 1)
	request := core.NewChangeAvailabilityRequest(1, core.AvailabilityTypeOperative)
	err = suite.centralSystem.SendRequestAsync(wsId, request, func(confirmation ocpp.Response, err error) {
		resultC <- asyncResult{confirmation: confirmation, err: err}
	})
	require.Nil(t, err)

	// Simulate the charge point disconnecting before any response ever
	// arrives: the drain fires inside the handler registered above.
	suite.mockWsServer.DisconnectedClientHandler(channel)

	select {
	case <-disconnectedC:
	case <-time.After(inboundOrderingWaitTimeout):
		t.Fatal("timed out waiting for the disconnect handler to run")
	}

	select {
	case result := <-resultC:
		require.Nil(t, result.confirmation)
		require.NotNil(t, result.err)
		ocppErr, ok := result.err.(*ocpp.Error)
		require.True(t, ok)
		assert.True(t, errors.Is(ocppErr, ocppj.ErrLocalTransport), "expected the disconnect-drained callback error to be ocppj.ErrLocalTransport, got %#v", ocppErr)
	case <-time.After(inboundOrderingWaitTimeout):
		t.Fatal("timed out waiting for the drained SendRequestAsync callback")
	}
}

// TestClientCallErrorStaysGenericServerError is test 6 (client side): a
// genuine CALL_ERROR delivered off the wire must NOT match ErrLocalTransport
// or ErrRequestTimeout - it stays an empty-marker error whose Code/
// Description/MessageId are the peer's, exactly as before B1/B2. This proves
// a local-transport error and a wire error are now distinguishable. Mirrors
// TestFacadeErrorCallbackRoundTrip (S3, inbound_ordering_test.go); do not
// replace the ocppj client's error handler here, for the same reason noted
// there - doing so would bypass the facade channel this test covers.
func (suite *OcppV16TestSuite) TestClientCallErrorStaysGenericServerError() {
	t := suite.T()
	writeC := make(chan []byte, 8)
	startStandaloneChargePoint(suite, writeC)
	// Stop the charge point on every exit path (including a t.Fatal/timeout
	// below), not just the success path - otherwise its dispatcher goroutine
	// leaks into later suite tests. startStandaloneChargePoint only stubs
	// "Start"/"Write" on the mock, so stub "Stop"/"IsConnected" here too;
	// IsConnected must report false so ocppj.Client.Stop() returns immediately
	// instead of blocking on a disconnected-handler signal this mock never
	// fires.
	suite.mockWsClient.On("IsConnected").Return(false)
	suite.mockWsClient.On("Stop").Return()
	defer suite.chargePoint.Stop()

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
		assert.False(t, errors.Is(ocppErr, ocppj.ErrLocalTransport), "a genuine server CALL_ERROR must NOT match ErrLocalTransport")
		assert.False(t, errors.Is(ocppErr, ocppj.ErrRequestTimeout), "a genuine server CALL_ERROR must NOT match ErrRequestTimeout")
	case <-time.After(inboundOrderingWaitTimeout):
		t.Fatal("timed out waiting for the error callback")
	}
}
