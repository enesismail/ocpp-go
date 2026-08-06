package ocpp16_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/enesismail/ocpp-go/ocpp1.6/core"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const (
	ec2ServerErrorBurst = 18 // errChanCapacity + 2
	ec2ServerErrorWait  = 2 * time.Second
	// ec2OuterWatchdog bounds a whole test body that is run on its own
	// goroutine, so a regressed (blocking error()) build fails fast instead of
	// wedging the test binary. Generous relative to ec2ServerErrorWait: the
	// body contains several of those inner waits back to back.
	ec2OuterWatchdog = 5 * ec2ServerErrorWait
)

func startEC2CentralSystem(suite *OcppV16TestSuite, writeC chan string) {
	suite.mockWsServer.On("Start", mock.AnythingOfType("int"), mock.AnythingOfType("string")).Return(nil)
	suite.mockWsServer.On("Stop").Return()
	suite.mockWsServer.On("Write", mock.AnythingOfType("string"), mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		writeC <- args.String(0)
	})
	suite.centralSystem.Start(8887, "somePath")
}

func connectEC2ChargePoints(suite *OcppV16TestSuite, ids []string) {
	for _, id := range ids {
		suite.mockWsServer.NewClientHandler(NewMockWebSocket(id))
	}
}

func waitForEC2Write(t *testing.T, writeC <-chan string, clientID string) {
	t.Helper()
	select {
	case got := <-writeC:
		require.Equal(t, clientID, got)
	case <-time.After(ec2ServerErrorWait):
		t.Fatalf("timed out waiting for a request for client %s to be written", clientID)
	}
}

func completeEC2Probe(t *testing.T, suite *OcppV16TestSuite, writeC <-chan string, clientID string) {
	t.Helper()
	requestID, err := suite.ocppjCentralSystem.SendRequest(clientID, core.NewHeartbeatRequest())
	require.NoError(t, err)
	waitForEC2Write(t, writeC, clientID)
	require.True(t, suite.serverDispatcher.CompleteRequest(clientID, requestID))
}

func stopEC2CentralSystem(t *testing.T, suite *OcppV16TestSuite) {
	t.Helper()
	doneC := make(chan struct{})
	go func() {
		suite.centralSystem.Stop()
		close(doneC)
	}()
	select {
	case <-doneC:
	case <-time.After(ec2ServerErrorWait):
		t.Fatal("central system Stop did not return")
	}
}

func (suite *OcppV16TestSuite) TestServerErrorsNonBlockingWhenUndrained() {
	t := suite.T()
	// The whole body runs on its own goroutine behind an outer watchdog: a
	// regressed (blocking error()) build can wedge the websocket read path, and
	// every inner bound would have to be reached in order for the test to end.
	// The watchdog makes the failure fast and structural, independent of which
	// inner assertion happens to notice the wedge first. The websocket-read
	// shape below is the decisive EC2 framing; EC-D1's new test owns the former
	// dispatcher-pump deadlock. Assertions inside the
	// goroutine still fail the test (testify marks it failed); a t.Fatalf there
	// runs runtime.Goexit, which unwinds through the deferred close(testDoneC)
	// and releases the select below rather than continuing past the failure.
	testDoneC := make(chan struct{})
	go func() {
		defer close(testDoneC)

		writeC := make(chan string, 64)
		startEC2CentralSystem(suite, writeC)

		ids := make([]string, ec2ServerErrorBurst+1)
		for i := range ids {
			ids[i] = fmt.Sprintf("ec2-pump-%d", i)
		}
		connectEC2ChargePoints(suite, ids)

		// Consume the connection notifications before arming the burst. The probe
		// requests also prove each queue is ready without using a sleep as a gate.
		for _, id := range ids {
			completeEC2Probe(t, suite, writeC, id)
		}

		// Arming an undrained channel is the precondition for the pump wedge.
		_ = suite.centralSystem.Errors()
		for _, id := range ids[:ec2ServerErrorBurst] {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err := suite.ocppjCentralSystem.SendRequestCtx(ctx, id, core.NewHeartbeatRequest())
			require.NoError(t, err)
		}

		// The final request belongs to a different client. It must still reach the
		// websocket after the canceled-request burst has filled the error buffer.
		_, err := suite.ocppjCentralSystem.SendRequest(ids[ec2ServerErrorBurst], core.NewHeartbeatRequest())
		require.NoError(t, err)
		waitForEC2Write(t, writeC, ids[ec2ServerErrorBurst])

		// Primary EC2 shape: the same non-blocking contract must hold when an error
		// is reported from the websocket read path, whose blocking send can
		// head-of-line block that client's reads.
		readID := ids[0]
		readChannel := NewMockWebSocket(readID)
		for i := 0; i < 2; i++ {
			requestID, err := suite.ocppjCentralSystem.SendRequest(readID, core.NewHeartbeatRequest())
			require.NoError(t, err)
			waitForEC2Write(t, writeC, readID)
			doneC := make(chan error, 1)
			go func() {
				doneC <- suite.mockWsServer.MessageHandler(readChannel, []byte(fmt.Sprintf(`[4,"%s","GenericError","read error",{}]`, requestID)))
			}()
			select {
			case err := <-doneC:
				require.NoError(t, err)
			case <-time.After(ec2ServerErrorWait):
				t.Fatal("websocket read path remained blocked while reporting an error")
			}
		}

		stopEC2CentralSystem(t, suite)
	}()

	select {
	case <-testDoneC:
	case <-time.After(ec2OuterWatchdog):
		t.Fatal("test timed out - websocket read path likely wedged by blocking error() send")
	}
}

func (suite *OcppV16TestSuite) TestServerErrorsSameChannelConcurrent() {
	t := suite.T()
	writeC := make(chan string, 1)
	startEC2CentralSystem(suite, writeC)
	clientID := "ec2-concurrent"
	connectEC2ChargePoints(suite, []string{clientID})

	const n = 64
	results := make([]<-chan error, n)
	readyC := make(chan struct{}, n)
	startC := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			readyC <- struct{}{}
			<-startC
			results[i] = suite.centralSystem.Errors()
		}(i)
	}
	for i := 0; i < n; i++ {
		<-readyC
	}

	firedC := make(chan error, 1)
	go func() {
		<-startC
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := suite.ocppjCentralSystem.SendRequestCtx(ctx, clientID, core.NewHeartbeatRequest())
		firedC <- err
	}()
	close(startC)
	wg.Wait()
	require.NoError(t, <-firedC)

	first := results[0]
	for i := 1; i < n; i++ {
		require.True(t, first == results[i], "Errors() call %d returned a different channel", i)
	}
	stopEC2CentralSystem(t, suite)
}

func (suite *OcppV16TestSuite) TestServerErrorsDelivered() {
	t := suite.T()
	writeC := make(chan string, 1)
	startEC2CentralSystem(suite, writeC)
	clientID := "ec2-delivered"
	connectEC2ChargePoints(suite, []string{clientID})
	errorsC := suite.centralSystem.Errors()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := suite.ocppjCentralSystem.SendRequestCtx(ctx, clientID, core.NewHeartbeatRequest())
	require.NoError(t, err)
	select {
	case reported := <-errorsC:
		require.Error(t, reported)
	case <-time.After(ec2ServerErrorWait):
		t.Fatal("timed out waiting for the canceled-request error")
	}
	stopEC2CentralSystem(t, suite)
}

func (suite *OcppV16TestSuite) TestServerErrorsPreArmDelivery() {
	t := suite.T()
	writeC := make(chan string, 1)
	startEC2CentralSystem(suite, writeC)
	clientID := "ec2-prearm"
	connectEC2ChargePoints(suite, []string{clientID})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := suite.ocppjCentralSystem.SendRequestCtx(ctx, clientID, core.NewHeartbeatRequest())
	require.NoError(t, err)
	_, err = suite.ocppjCentralSystem.SendRequest(clientID, core.NewHeartbeatRequest())
	require.NoError(t, err)
	waitForEC2Write(t, writeC, clientID)

	errorsC := suite.centralSystem.Errors()
	select {
	case reported := <-errorsC:
		require.Error(t, reported)
	case <-time.After(ec2ServerErrorWait):
		t.Fatal("timed out waiting for the pre-arm canceled-request error")
	}
	stopEC2CentralSystem(t, suite)
}
