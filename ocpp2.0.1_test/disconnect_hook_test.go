package ocpp2_test

import (
	"errors"
	"time"

	"github.com/enesismail/ocpp-go/ocppj"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func (suite *OcppV2TestSuite) TestChargingStationOnDisconnectedHandlerFiresOnUnexpectedDrop() {
	t := suite.T()
	wsId := "test_id"
	wsUrl := "someUrl"
	dropErr := errors.New("mock disconnect")
	disconnectedC := make(chan error, 1)

	suite.chargingStation.SetOnDisconnectedHandler(func(err error) {
		disconnectedC <- err
	})
	setupDefaultChargingStationHandlers(suite, expectedChargingStationOptions{serverUrl: wsUrl, clientId: wsId})

	err := suite.chargingStation.Start(wsUrl)
	require.Nil(t, err)
	require.NotNil(t, suite.mockWsClient.DisconnectedHandler)

	suite.mockWsClient.DisconnectedHandler(dropErr)

	select {
	case err := <-disconnectedC:
		assert.Same(t, dropErr, err)
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for disconnect handler")
	}
	select {
	case extra := <-disconnectedC:
		t.Fatalf("disconnect handler fired more than once: %v", extra)
	default:
	}
}

func (suite *OcppV2TestSuite) TestChargingStationOnReconnectedHandlerFires() {
	t := suite.T()
	wsId := "test_id"
	wsUrl := "someUrl"
	reconnectedC := make(chan struct{}, 1)

	suite.chargingStation.SetOnReconnectedHandler(func() {
		reconnectedC <- struct{}{}
	})
	setupDefaultChargingStationHandlers(suite, expectedChargingStationOptions{serverUrl: wsUrl, clientId: wsId})

	err := suite.chargingStation.Start(wsUrl)
	require.Nil(t, err)
	require.NotNil(t, suite.mockWsClient.ReconnectedHandler)

	// StartWithRetries can fire onReconnected on the initial successful connect;
	// this facade-wiring test drives only the captured ws handler.
	suite.mockWsClient.ReconnectedHandler()

	select {
	case <-reconnectedC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for reconnect handler")
	}
}

func (suite *OcppV2TestSuite) TestChargingStationOnDisconnectedHandlerNotFiredOnGracefulStop() {
	t := suite.T()
	wsId := "test_id"
	wsUrl := "someUrl"
	disconnectedC := make(chan error, 1)

	suite.chargingStation.SetOnDisconnectedHandler(func(err error) {
		disconnectedC <- err
	})
	setupDefaultChargingStationHandlers(suite, expectedChargingStationOptions{serverUrl: wsUrl, clientId: wsId})
	suite.mockWsClient.On("IsConnected").Return(true)
	suite.mockWsClient.On("Stop").Return().Run(func(args mock.Arguments) {
		suite.mockWsClient.DisconnectedHandler(nil)
	})

	err := suite.chargingStation.Start(wsUrl)
	require.Nil(t, err)

	stoppedC := make(chan struct{})
	go func() {
		suite.chargingStation.Stop()
		close(stoppedC)
	}()

	select {
	case <-stoppedC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for Stop")
	}
	select {
	case err := <-disconnectedC:
		t.Fatalf("disconnect handler fired during graceful Stop: %v", err)
	default:
	}
}

func (suite *OcppV2TestSuite) TestChargingStationOnDisconnectedHandlerPanicRecovered() {
	t := suite.T()
	wsId := "test_id"
	wsUrl := "someUrl"
	panicValue := "boom: disconnect hook panic"
	panicC := make(chan ocppj.HandlerPanic, 1)

	suite.chargingStation.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		panicC <- hp
	})
	suite.chargingStation.SetOnDisconnectedHandler(func(err error) {
		panic(panicValue)
	})
	setupDefaultChargingStationHandlers(suite, expectedChargingStationOptions{serverUrl: wsUrl, clientId: wsId})

	err := suite.chargingStation.Start(wsUrl)
	require.Nil(t, err)
	require.NotNil(t, suite.mockWsClient.DisconnectedHandler)

	suite.mockWsClient.DisconnectedHandler(errors.New("mock disconnect"))

	select {
	case hp := <-panicC:
		assert.Equal(t, ocppj.DisconnectHandlerKind, hp.Kind)
		assert.Equal(t, panicValue, hp.Value)
		assert.NotEmpty(t, hp.Stack)
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for panic callback")
	}
}
