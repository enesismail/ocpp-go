package ocpp2_test

import (
	"errors"
	"sync/atomic"
	"time"

	"github.com/enesismail/ocpp-go/ocpp"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/availability"
	"github.com/enesismail/ocpp-go/ocppj"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func (suite *OcppV2TestSuite) TestEC1CSMSStopCancelsOnce() {
	t := suite.T()
	clientID := "ec1-v201-stop"
	suite.mockWsServer.On("Start", mock.AnythingOfType("int"), mock.AnythingOfType("string")).Return(nil)
	suite.mockWsServer.On("Stop").Return().Maybe()
	suite.mockWsServer.On("Write", clientID, mock.Anything).Return(nil)
	suite.csms.Start(8887, "somePath")
	suite.mockWsServer.NewClientHandler(NewMockWebSocket(clientID))

	callbackC := make(chan error, 2)
	var callbackCount int32
	err := suite.csms.SendRequestAsync(clientID, availability.NewChangeAvailabilityRequest(availability.OperationalStatusOperative), func(_ ocpp.Response, err error) {
		atomic.AddInt32(&callbackCount, 1)
		callbackC <- err
	})
	require.NoError(t, err)

	suite.csms.Stop()
	select {
	case callbackErr := <-callbackC:
		require.Error(t, callbackErr)
		require.True(t, errors.Is(callbackErr, ocppj.ErrDispatcherStopped) || errors.Is(callbackErr, ocppj.ErrLocalTransport), "unexpected terminal error: %v", callbackErr)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the Stop cancellation callback")
	}
	suite.csms.Stop()
	select {
	case extra := <-callbackC:
		t.Fatalf("Stop delivered the callback more than once: %v", extra)
	case <-time.After(300 * time.Millisecond):
	}
	require.Equal(t, int32(1), atomic.LoadInt32(&callbackCount))
}
