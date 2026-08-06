package ocpp16_test

import (
	"errors"
	"sync/atomic"
	"time"

	"github.com/enesismail/ocpp-go/ocpp"
	"github.com/enesismail/ocpp-go/ocpp1.6/core"
	"github.com/enesismail/ocpp-go/ocppj"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func (suite *OcppV16TestSuite) TestEC1CentralSystemStopCancelsOnce() {
	t := suite.T()
	clientID := "ec1-v16-stop"
	suite.mockWsServer.On("Start", mock.AnythingOfType("int"), mock.AnythingOfType("string")).Return(nil)
	suite.mockWsServer.On("Stop").Return().Maybe()
	suite.mockWsServer.On("Write", clientID, mock.Anything).Return(nil)
	suite.centralSystem.Start(8887, "somePath")
	suite.mockWsServer.NewClientHandler(NewMockWebSocket(clientID))

	callbackC := make(chan error, 2)
	var callbackCount int32
	err := suite.centralSystem.SendRequestAsync(clientID, core.NewChangeAvailabilityRequest(1, core.AvailabilityTypeOperative), func(_ ocpp.Response, err error) {
		atomic.AddInt32(&callbackCount, 1)
		callbackC <- err
	})
	require.NoError(t, err)

	suite.centralSystem.Stop()
	select {
	case callbackErr := <-callbackC:
		require.Error(t, callbackErr)
		require.True(t, errors.Is(callbackErr, ocppj.ErrDispatcherStopped) || errors.Is(callbackErr, ocppj.ErrLocalTransport), "unexpected terminal error: %v", callbackErr)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the Stop cancellation callback")
	}
	suite.centralSystem.Stop()
	select {
	case extra := <-callbackC:
		t.Fatalf("Stop delivered the callback more than once: %v", extra)
	case <-time.After(300 * time.Millisecond):
	}
	require.Equal(t, int32(1), atomic.LoadInt32(&callbackCount))
}

// TestEC1CentralSystemStopStormVsSendRequestAsync pins test 8's
// write-lock-before-close(stoppedC) barrier only. The requestChannel is
// saturated while a SendRequestAsync sender is parked under RLock; Stop must
// wait for that sender and not deadlock once the blocked Write is released.
// The unlock-before-join barrier is unpinnable by a storm post-EC-D1 because
// facade callbacks run off the dispatcher pump.
func (suite *OcppV16TestSuite) TestEC1CentralSystemStopStormVsSendRequestAsync() {
	t := suite.T()
	wedgeID := "ec1-v16-wedge"
	fillerIDs := []string{"ec1-v16-filler-0", "ec1-v16-filler-1", "ec1-v16-filler-2", "ec1-v16-filler-3"}
	releaseWrite := make(chan struct{})
	enteredWrite := make(chan struct{}, 1)
	var enteredOnce int32
	suite.mockWsServer.On("Start", mock.AnythingOfType("int"), mock.AnythingOfType("string")).Return(nil)
	suite.mockWsServer.On("Stop").Return().Maybe()
	suite.mockWsServer.On("Write", wedgeID, mock.Anything).Return(nil).Run(func(mock.Arguments) {
		if atomic.CompareAndSwapInt32(&enteredOnce, 0, 1) {
			enteredWrite <- struct{}{}
		}
		<-releaseWrite
	})
	suite.mockWsServer.On("Write", mock.AnythingOfType("string"), mock.Anything).Return(nil)
	suite.centralSystem.Start(8887, "somePath")
	for _, clientID := range append([]string{wedgeID}, fillerIDs...) {
		suite.mockWsServer.NewClientHandler(NewMockWebSocket(clientID))
	}
	require.NoError(t, suite.centralSystem.SendRequestAsync(wedgeID, core.NewChangeAvailabilityRequest(1, core.AvailabilityTypeOperative), func(ocpp.Response, error) {}))
	select {
	case <-enteredWrite:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the saturated Write")
	}

	saturateECD1FacadeSenders(t, suite.centralSystem, fillerIDs, releaseWrite)
	stopDone := make(chan struct{})
	go func() {
		suite.centralSystem.Stop()
		close(stopDone)
	}()
	close(releaseWrite)
	select {
	case <-stopDone:
	case <-time.After(4 * time.Second):
		t.Fatal("Stop deadlocked during the SendRequestAsync storm")
	}
}
