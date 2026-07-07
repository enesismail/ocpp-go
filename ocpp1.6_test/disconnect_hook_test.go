package ocpp16_test

import (
	"errors"
	"time"

	"github.com/enesismail/ocpp-go/ocppj"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func (suite *OcppV16TestSuite) TestChargePointOnDisconnectedHandlerFiresOnUnexpectedDrop() {
	t := suite.T()
	wsId := "test_id"
	wsUrl := "someUrl"
	dropErr := errors.New("mock disconnect")
	disconnectedC := make(chan error, 1)

	suite.chargePoint.SetOnDisconnectedHandler(func(err error) {
		disconnectedC <- err
	})
	setupDefaultChargePointHandlers(suite, nil, expectedChargePointOptions{serverUrl: wsUrl, clientId: wsId})

	err := suite.chargePoint.Start(wsUrl)
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

func (suite *OcppV16TestSuite) TestChargePointOnReconnectedHandlerFires() {
	t := suite.T()
	wsId := "test_id"
	wsUrl := "someUrl"
	reconnectedC := make(chan struct{}, 1)

	suite.chargePoint.SetOnReconnectedHandler(func() {
		reconnectedC <- struct{}{}
	})
	setupDefaultChargePointHandlers(suite, nil, expectedChargePointOptions{serverUrl: wsUrl, clientId: wsId})

	err := suite.chargePoint.Start(wsUrl)
	require.Nil(t, err)
	require.NotNil(t, suite.mockWsClient.ReconnectedHandler)

	suite.mockWsClient.ReconnectedHandler()

	select {
	case <-reconnectedC:
	case <-time.After(panicWaitTimeout):
		t.Fatal("timed out waiting for reconnect handler")
	}
}

func (suite *OcppV16TestSuite) TestChargePointOnDisconnectedHandlerNotFiredOnGracefulStop() {
	t := suite.T()
	wsId := "test_id"
	wsUrl := "someUrl"
	disconnectedC := make(chan error, 1)

	suite.chargePoint.SetOnDisconnectedHandler(func(err error) {
		disconnectedC <- err
	})
	setupDefaultChargePointHandlers(suite, nil, expectedChargePointOptions{serverUrl: wsUrl, clientId: wsId})
	suite.mockWsClient.On("IsConnected").Return(true)
	suite.mockWsClient.On("Stop").Return().Run(func(args mock.Arguments) {
		suite.mockWsClient.DisconnectedHandler(nil)
	})

	err := suite.chargePoint.Start(wsUrl)
	require.Nil(t, err)

	stoppedC := make(chan struct{})
	go func() {
		suite.chargePoint.Stop()
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

func (suite *OcppV16TestSuite) TestChargePointOnDisconnectedHandlerPanicRecovered() {
	t := suite.T()
	wsId := "test_id"
	wsUrl := "someUrl"
	panicValue := "boom: disconnect hook panic"
	panicC := make(chan ocppj.HandlerPanic, 1)

	suite.chargePoint.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		panicC <- hp
	})
	suite.chargePoint.SetOnDisconnectedHandler(func(err error) {
		panic(panicValue)
	})
	setupDefaultChargePointHandlers(suite, nil, expectedChargePointOptions{serverUrl: wsUrl, clientId: wsId})

	err := suite.chargePoint.Start(wsUrl)
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
