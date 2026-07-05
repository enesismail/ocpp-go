package ocpp2_test

import (
	"errors"
	"time"

	"github.com/enesismail/ocpp-go/ocpp"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/availability"
	"github.com/enesismail/ocpp-go/ocppj"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file guards that the shared, version-agnostic ocppj sentinels
// (ErrRequestTimeout, ErrLocalTransport) ride through the 2.0.1 charging
// station (client) facade unchanged: a dispatcher-timeout must classify as
// ErrRequestTimeout (and NOT ErrLocalTransport), while a local network write
// failure must classify as ErrLocalTransport. Both are regression guards -
// they are expected to PASS today, mirroring the established
// disconnectDrainWaitTimeout bounded-select pattern from
// local_transport_test.go in this same package (no time.Sleep is used
// anywhere in this file for synchronization).

// TestChargingStationRequestTimeoutIsErrRequestTimeout drives
// chargingStation.SendRequestAsync with a short client-dispatcher timeout,
// a mock Write that succeeds (request is actually sent), and no response
// ever delivered. The dispatcher's own timeout must cancel the pending
// request and classify the resulting callback error as
// ocppj.ErrRequestTimeout - and NOT ocppj.ErrLocalTransport.
func (suite *OcppV2TestSuite) TestChargingStationRequestTimeoutIsErrRequestTimeout() {
	t := suite.T()
	wsId := "test_id"
	wsUrl := "someUrl"

	// Configure a short dispatcher timeout BEFORE Start (per
	// ClientDispatcher.SetTimeout's contract), so the request below times
	// out quickly instead of waiting for a response this test deliberately
	// never delivers.
	suite.clientDispatcher.SetTimeout(200 * time.Millisecond)

	// writeReturnArgument left at its zero value (nil): Write succeeds, so
	// the request is actually sent and only the dispatcher's own timeout
	// cancels it.
	setupDefaultChargingStationHandlers(suite, expectedChargingStationOptions{serverUrl: wsUrl, clientId: wsId})

	err := suite.chargingStation.Start(wsUrl)
	require.Nil(t, err)
	// No existing test in this suite calls chargingStation.Stop(), and
	// setupDefaultChargingStationHandlers doesn't stub "Stop"/"IsConnected"
	// on the mock. Stub them here and stop it too - it runs a dispatcher
	// goroutine that would otherwise leak into later suite tests.
	// IsConnected must report false so ocppj.Client.Stop() returns
	// immediately instead of blocking on a disconnected-handler signal this
	// mock never fires.
	suite.mockWsClient.On("IsConnected").Return(false)
	suite.mockWsClient.On("Stop").Return()
	defer suite.chargingStation.Stop()

	type asyncResult struct {
		response ocpp.Response
		err      error
	}
	resultC := make(chan asyncResult, 1)
	// Heartbeat is sent BY the charging station (unlike ChangeAvailability,
	// which is CSMS-initiated and would be rejected here as "unsupported
	// action ... on charging station, cannot send request").
	request := availability.NewHeartbeatRequest()
	err = suite.chargingStation.SendRequestAsync(request, func(response ocpp.Response, err error) {
		resultC <- asyncResult{response: response, err: err}
	})
	require.Nil(t, err)

	select {
	case result := <-resultC:
		require.Nil(t, result.response)
		require.NotNil(t, result.err)
		ocppErr, ok := result.err.(*ocpp.Error)
		require.True(t, ok)
		assert.True(t, errors.Is(ocppErr, ocppj.ErrRequestTimeout), "expected the timed-out callback error to be ocppj.ErrRequestTimeout, got %#v", ocppErr)
		assert.False(t, errors.Is(ocppErr, ocppj.ErrLocalTransport), "a request-timeout error must NOT be classified as ErrLocalTransport")
	case <-time.After(disconnectDrainWaitTimeout):
		t.Fatal("timed out waiting for the timed-out SendRequestAsync callback")
	}
}

// TestChargingStationWriteFailureIsErrLocalTransport drives
// chargingStation.SendRequestAsync where the mock Write returns an error
// (a local network/transport failure). The resulting callback error must be
// classified ocppj.ErrLocalTransport.
func (suite *OcppV2TestSuite) TestChargingStationWriteFailureIsErrLocalTransport() {
	t := suite.T()
	wsId := "test_id"
	wsUrl := "someUrl"

	setupDefaultChargingStationHandlers(suite, expectedChargingStationOptions{serverUrl: wsUrl, clientId: wsId, writeReturnArgument: errors.New("mock write error")})

	err := suite.chargingStation.Start(wsUrl)
	require.Nil(t, err)
	// See the comment in TestChargingStationRequestTimeoutIsErrRequestTimeout
	// above for why this stub/stop/defer sequence is required.
	suite.mockWsClient.On("IsConnected").Return(false)
	suite.mockWsClient.On("Stop").Return()
	defer suite.chargingStation.Stop()

	type asyncResult struct {
		response ocpp.Response
		err      error
	}
	resultC := make(chan asyncResult, 1)
	// Heartbeat is sent BY the charging station (unlike ChangeAvailability,
	// which is CSMS-initiated and would be rejected here as "unsupported
	// action ... on charging station, cannot send request").
	request := availability.NewHeartbeatRequest()
	err = suite.chargingStation.SendRequestAsync(request, func(response ocpp.Response, err error) {
		resultC <- asyncResult{response: response, err: err}
	})
	require.Nil(t, err)

	select {
	case result := <-resultC:
		require.Nil(t, result.response)
		require.NotNil(t, result.err)
		ocppErr, ok := result.err.(*ocpp.Error)
		require.True(t, ok)
		assert.True(t, errors.Is(ocppErr, ocppj.ErrLocalTransport), "expected the write-failure callback error to be ocppj.ErrLocalTransport, got %#v", ocppErr)
	case <-time.After(disconnectDrainWaitTimeout):
		t.Fatal("timed out waiting for the write-failure SendRequestAsync callback")
	}
}
