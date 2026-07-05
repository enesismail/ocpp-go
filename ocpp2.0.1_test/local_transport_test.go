package ocpp2_test

import (
	"errors"
	"time"

	"github.com/enesismail/ocpp-go/ocpp"
	ocpp2 "github.com/enesismail/ocpp-go/ocpp2.0.1"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/availability"
	"github.com/enesismail/ocpp-go/ocppj"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// disconnectDrainWaitTimeout bounds how long this test waits for an async
// signal before failing, so a broken implementation (or a broken test)
// produces a fast, deterministic failure instead of hanging the suite. No
// time.Sleep is used anywhere in this file for synchronization.
const disconnectDrainWaitTimeout = 5 * time.Second

// TestChargingStationDisconnectDrainIsLocalTransport is test 5 from
// tasks/b1-b2-local-transport-markers.md for the 2.0.1 CSMS facade: a pending
// SendRequestAsync callback, drained because the charging station
// disconnected before any response arrived, must be classified
// ocppj.ErrLocalTransport. The drain (ocpp2.0.1/csms.go:757, inside the
// wrapper registered by SetChargingStationDisconnectedHandler) only runs at
// all if a disconnect handler was registered - required here - otherwise the
// pending callback would be dropped silently and there would be nothing to
// classify. Today (pre-B2) the drain builds an untagged
// ocpp.NewError(ocppj.GenericError, ...), so this assertion is expected to
// fail (red) until B2 tags it. This test deliberately references the
// not-yet-existing ocppj.ErrLocalTransport sentinel, so this package fails to
// compile until B1 lands; that is the intended red-first state.
func (suite *OcppV2TestSuite) TestChargingStationDisconnectDrainIsLocalTransport() {
	t := suite.T()
	wsId := "test_id"
	wsUrl := "someUrl"
	channel := NewMockWebSocket(wsId)

	setupDefaultChargingStationHandlers(suite, expectedChargingStationOptions{serverUrl: wsUrl, clientId: wsId, createChannelOnStart: true, channel: channel})
	setupDefaultCSMSHandlers(suite, expectedCSMSOptions{clientId: wsId})

	// REQUIRED: the disconnect-drain only runs inside a registered handler
	// (see the spec's "Server disconnect-drain precondition"). We also use
	// this handler to observe that the drain path actually ran.
	disconnectedC := make(chan struct{}, 1)
	suite.csms.SetChargingStationDisconnectedHandler(func(chargingStation ocpp2.ChargingStationConnection) {
		disconnectedC <- struct{}{}
	})

	// Start
	suite.csms.Start(8887, "somePath")
	// Deferred immediately after Start so the CSMS dispatcher is stopped on
	// every exit path (including a t.Fatal/timeout below), not just the
	// success path - otherwise its dispatcher goroutine leaks into later
	// suite tests.
	defer suite.csms.Stop()
	err := suite.chargingStation.Start(wsUrl)
	require.Nil(t, err)
	// The client endpoint isn't covered by the above: no existing test in
	// this suite calls chargingStation.Stop(), and
	// setupDefaultChargingStationHandlers doesn't stub "Stop"/"IsConnected" on
	// the mock. Stub them here and stop it too - it runs a second, independent
	// dispatcher goroutine that would otherwise leak the same way. IsConnected
	// must report false so ocppj.Client.Stop() returns immediately instead of
	// blocking on a disconnected-handler signal this mock never fires.
	suite.mockWsClient.On("IsConnected").Return(false)
	suite.mockWsClient.On("Stop").Return()
	defer suite.chargingStation.Stop()

	type asyncResult struct {
		response ocpp.Response
		err      error
	}
	resultC := make(chan asyncResult, 1)
	request := availability.NewChangeAvailabilityRequest(availability.OperationalStatusOperative)
	err = suite.csms.SendRequestAsync(wsId, request, func(response ocpp.Response, err error) {
		resultC <- asyncResult{response: response, err: err}
	})
	require.Nil(t, err)

	// Simulate the charging station disconnecting before any response ever
	// arrives: the drain fires inside the handler registered above.
	suite.mockWsServer.DisconnectedClientHandler(channel)

	select {
	case <-disconnectedC:
	case <-time.After(disconnectDrainWaitTimeout):
		t.Fatal("timed out waiting for the disconnect handler to run")
	}

	select {
	case result := <-resultC:
		require.Nil(t, result.response)
		require.NotNil(t, result.err)
		ocppErr, ok := result.err.(*ocpp.Error)
		require.True(t, ok)
		assert.True(t, errors.Is(ocppErr, ocppj.ErrLocalTransport), "expected the disconnect-drained callback error to be ocppj.ErrLocalTransport, got %#v", ocppErr)
	case <-time.After(disconnectDrainWaitTimeout):
		t.Fatal("timed out waiting for the drained SendRequestAsync callback")
	}
}
