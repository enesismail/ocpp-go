package ocpp2_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/enesismail/ocpp-go/ocpp"
	ocpp2 "github.com/enesismail/ocpp-go/ocpp2.0.1"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/availability"
	"github.com/enesismail/ocpp-go/ocppj"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const ecd1Wait = 2 * time.Second

func saturateECD1FacadeSenders(t *testing.T, csms ocpp2.CSMS, ids []string, releaseWedgeWrite <-chan struct{}) {
	t.Helper()
	// This issues SendRequestAsync calls one at a time, round-robin across the
	// filler clients, each on its own goroutine, until one fails to return within
	// settleBound. TryQueue serialises every sender on callbacksMutex, so the
	// first non-returning send is parked on the full requestChannel while holding
	// that mutex. Returned errors are expected and must not fail the test: q.Push
	// fails before the channel send when a per-client queue (cap queueCapacity)
	// fills, while the constant message-id generator can report
	// ErrDuplicateCallback after try() has already pushed and signalled. Only a
	// "dispatcher not running" error means the precondition collapsed. The
	// release channel stays open throughout; closing it would release the wedge
	// and let the pump drain a signal between sends.
	const (
		settleBound = 300 * time.Millisecond
		maxSends    = 100
	)
	// requestChannel is cap 20 and each per-client queue is cap 10. One filler
	// can contribute only about 10 channel signals, so four fillers provide
	// about 40 against the 20 buffered plus one parked send required here.
	// releaseWedgeWrite must remain open for the whole loop or the pump can
	// drain a signal between sends and invalidate the saturation proof.
	_ = releaseWedgeWrite
	var observedErrors []error
	for i := 0; i < maxSends; i++ {
		clientID := ids[i%len(ids)]
		doneC := make(chan error, 1)
		go func() {
			doneC <- csms.SendRequestAsync(clientID, availability.NewChangeAvailabilityRequest(availability.OperationalStatusOperative), func(ocpp.Response, error) {})
		}()
		select {
		case err := <-doneC:
			if err != nil {
				observedErrors = append(observedErrors, err)
				if strings.Contains(err.Error(), "dispatcher not running") {
					t.Fatalf("facade sender reported dispatcher stopped before saturation: %v", err)
				}
			}
		case <-time.After(settleBound):
			return
		}
	}
	t.Fatalf("sent %d facade requests without parking a sender; errors: %v", maxSends, observedErrors)
}

func TestECD1PumpSurvivesCancelWithSaturatedRequestChannel(t *testing.T) {
	testDoneC := make(chan struct{})
	go func() {
		defer close(testDoneC)
		mockServer := &MockWebsocketServer{}
		dispatcher := ocppj.NewDefaultServerDispatcher(ocppj.NewFIFOQueueMap(queueCapacity))
		endpoint := ocppj.NewServer(mockServer, dispatcher, nil, availability.Profile)
		csms := ocpp2.NewCSMS(endpoint, mockServer)
		wedgeID := "ecd1-wedge"
		freshID := "ecd1-fresh"
		fillerIDs := []string{"ecd1-filler-0", "ecd1-filler-1", "ecd1-filler-2", "ecd1-filler-3"}
		enteredWedgeWrite := make(chan struct{}, 1)
		releaseWedgeWrite := make(chan struct{})
		freshWriteC := make(chan struct{}, 1)
		var wedgeWriteOnce sync.Once
		mockServer.On("Start", mock.AnythingOfType("int"), mock.AnythingOfType("string")).Return(nil)
		mockServer.On("Stop").Return()
		mockServer.On("Write", wedgeID, mock.Anything).Return(fmt.Errorf("wedged write failed")).Run(func(args mock.Arguments) {
			wedgeWriteOnce.Do(func() { close(enteredWedgeWrite) })
			<-releaseWedgeWrite
		})
		mockServer.On("Write", mock.AnythingOfType("string"), mock.Anything).Return(nil).Run(func(args mock.Arguments) {
			if args.String(0) == freshID {
				select {
				case freshWriteC <- struct{}{}:
				default:
				}
			}
		})
		csms.Start(8887, "somePath")
		for _, clientID := range append(append([]string{wedgeID}, fillerIDs...), freshID) {
			mockServer.NewClientHandler(NewMockWebSocket(clientID))
		}
		var callbackCount int32
		callbackC := make(chan error, 2)
		err := csms.SendRequestAsync(wedgeID, availability.NewChangeAvailabilityRequest(availability.OperationalStatusOperative), func(_ ocpp.Response, err error) {
			atomic.AddInt32(&callbackCount, 1)
			callbackC <- err
		})
		require.NoError(t, err)
		select {
		case <-enteredWedgeWrite:
		case <-time.After(ecd1Wait):
			t.Errorf("timed out waiting for wedged Write")
			return
		}
		saturateECD1FacadeSenders(t, csms, fillerIDs, releaseWedgeWrite)
		close(releaseWedgeWrite)
		select {
		case callbackErr := <-callbackC:
			if !errors.Is(callbackErr, ocppj.ErrLocalTransport) {
				t.Errorf("wedged request callback error = %v, want ErrLocalTransport", callbackErr)
			}
		case <-time.After(ecd1Wait):
			t.Errorf("wedged request callback did not fire")
		}
		select {
		case extra := <-callbackC:
			t.Errorf("wedged request callback fired more than once: %v", extra)
		case <-time.After(300 * time.Millisecond):
		}
		if got := atomic.LoadInt32(&callbackCount); got != 1 {
			t.Errorf("wedged request callback count = %d, want 1", got)
		}
		_, err = endpoint.SendRequest(freshID, availability.NewHeartbeatRequest())
		require.NoError(t, err)
		select {
		case <-freshWriteC:
		case <-time.After(ecd1Wait):
			t.Errorf("pump did not write a fresh client's request after the cancel")
		}
		senderDoneC := make(chan error, 1)
		go func() {
			senderDoneC <- csms.SendRequestAsync(fillerIDs[0], availability.NewChangeAvailabilityRequest(availability.OperationalStatusInoperative), func(ocpp.Response, error) {})
		}()
		select {
		case <-senderDoneC:
		case <-time.After(ecd1Wait):
			t.Errorf("subsequent facade sender did not return")
		}
		stopDoneC := make(chan struct{})
		go func() {
			csms.Stop()
			close(stopDoneC)
		}()
		select {
		case <-stopDoneC:
		case <-time.After(ecd1Wait):
			t.Errorf("CSMS Stop did not return")
		}
	}()
	select {
	case <-testDoneC:
	case <-time.After(6 * time.Second):
		t.Fatal("test timed out - pump/callbackqueue deadlock survived the outer watchdog")
	}
}

func assertECD1NoCallback(t *testing.T, callbackC <-chan error, wait time.Duration) {
	t.Helper()
	select {
	case err := <-callbackC:
		t.Fatalf("unexpected second callback: %v", err)
	case <-time.After(wait):
	}
}

func (suite *OcppV2TestSuite) TestECD1CancelCallbackExactlyOnceOffPump() {
	t := suite.T()
	timeoutID := "ecd1-timeout"
	dupID := "ecd1-duplicate"
	suite.serverDispatcher.SetTimeout(300 * time.Millisecond)
	suite.mockWsServer.On("Start", mock.AnythingOfType("int"), mock.AnythingOfType("string")).Return(nil)
	suite.mockWsServer.On("Stop").Return()
	suite.mockWsServer.On("Write", dupID, mock.Anything).Return(fmt.Errorf("write failed for duplicate request"))
	suite.mockWsServer.On("Write", mock.AnythingOfType("string"), mock.Anything).Return(nil).Run(func(args mock.Arguments) {})
	suite.csms.Start(8887, "somePath")
	defer suite.csms.Stop()
	suite.mockWsServer.NewClientHandler(NewMockWebSocket(timeoutID))
	suite.mockWsServer.NewClientHandler(NewMockWebSocket(dupID))
	var timeoutCount int32
	timeoutC := make(chan error, 2)
	err := suite.csms.SendRequestAsync(timeoutID, availability.NewChangeAvailabilityRequest(availability.OperationalStatusOperative), func(_ ocpp.Response, err error) {
		atomic.AddInt32(&timeoutCount, 1)
		timeoutC <- err
	})
	require.NoError(t, err)
	select {
	case err := <-timeoutC:
		require.True(t, errors.Is(err, ocppj.ErrRequestTimeout), "timeout callback error = %v", err)
	case <-time.After(ecd1Wait):
		t.Fatal("timed out waiting for the real dispatcher timeout callback")
	}
	assertECD1NoCallback(t, timeoutC, 300*time.Millisecond)
	assert.Equal(t, int32(1), atomic.LoadInt32(&timeoutCount))

	var duplicateCount int32
	duplicateC := make(chan error, 2)
	err = suite.csms.SendRequestAsync(dupID, availability.NewChangeAvailabilityRequest(availability.OperationalStatusOperative), func(_ ocpp.Response, err error) {
		atomic.AddInt32(&duplicateCount, 1)
		duplicateC <- err
	})
	require.NoError(t, err)
	select {
	case err := <-duplicateC:
		require.True(t, errors.Is(err, ocppj.ErrLocalTransport), "duplicate callback error = %v", err)
	case <-time.After(ecd1Wait):
		t.Fatal("timed out waiting for the write-error callback")
	}
	// This barrier is load-bearing: Dequeue precedes callback delivery, so the
	// constant-generator key (dupID, "1234") is released before it is reused.
	assertECD1NoCallback(t, duplicateC, 300*time.Millisecond)
	assert.Equal(t, int32(1), atomic.LoadInt32(&duplicateCount))
	errC := suite.csms.Errors()
	_, err = suite.ocppjServer.SendRequest(dupID, availability.NewChangeAvailabilityRequest(availability.OperationalStatusInoperative))
	require.NoError(t, err)
	deadline := time.NewTimer(ecd1Wait)
	defer deadline.Stop()
	for {
		select {
		case report := <-errC:
			if strings.Contains(report.Error(), "no handler available for canceled request") {
				assertECD1NoErrors(t, errC, 300*time.Millisecond)
				return
			}
		case <-deadline.C:
			t.Fatal("timed out waiting for no-callback Errors report")
		}
	}
}

func assertECD1NoErrors(t *testing.T, errC <-chan error, wait time.Duration) {
	t.Helper()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case err := <-errC:
			if strings.Contains(err.Error(), "no handler available for canceled request") {
				t.Fatalf("duplicate no-callback Errors report: %v", err)
			}
		case <-timer.C:
			return
		}
	}
}

func (suite *OcppV2TestSuite) TestECD1CancelNoCallbackReportsOnErrors() {
	t := suite.T()
	rawID := "ecd1-raw-cancel"
	freshID := "ecd1-raw-fresh"
	writesC := make(chan string, 8)
	suite.mockWsServer.On("Start", mock.AnythingOfType("int"), mock.AnythingOfType("string")).Return(nil)
	suite.mockWsServer.On("Stop").Return()
	suite.mockWsServer.On("Write", mock.AnythingOfType("string"), mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		select {
		case writesC <- args.String(0):
		default:
		}
	})
	suite.csms.Start(8887, "somePath")
	defer suite.csms.Stop()
	suite.mockWsServer.NewClientHandler(NewMockWebSocket(rawID))
	suite.mockWsServer.NewClientHandler(NewMockWebSocket(freshID))
	errC := suite.csms.Errors()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := suite.ocppjServer.SendRequestCtx(ctx, rawID, availability.NewHeartbeatRequest())
	require.NoError(t, err)
	select {
	case report := <-errC:
		require.Contains(t, report.Error(), "no handler available for canceled request")
	case <-time.After(ecd1Wait):
		t.Fatal("timed out waiting for no-callback cancellation report")
	}
	_, err = suite.ocppjServer.SendRequest(freshID, availability.NewHeartbeatRequest())
	require.NoError(t, err)
	select {
	case got := <-writesC:
		require.Equal(t, freshID, got)
	case <-time.After(ecd1Wait):
		t.Fatal("dispatcher pump did not write after no-callback cancellation")
	}
}

func (suite *OcppV2TestSuite) TestECD1NilPayloadCancelWithRegisteredCallbackDoesNotCrash() {
	t := suite.T()
	panicC := make(chan ocppj.HandlerPanic, 2)
	suite.csms.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) { panicC <- hp })
	rawID := "ecd1-nil-raw"
	nilID := "ecd1-nil-registered"
	freshID := "ecd1-nil-fresh"
	enteredWrite := make(chan struct{}, 1)
	releaseWrite := make(chan struct{})
	freshWriteC := make(chan struct{}, 1)
	suite.mockWsServer.On("Start", mock.AnythingOfType("int"), mock.AnythingOfType("string")).Return(nil)
	suite.mockWsServer.On("Stop").Return()
	suite.mockWsServer.On("Write", mock.AnythingOfType("string"), mock.Anything).Return(fmt.Errorf("nil payload write failed")).Run(func(args mock.Arguments) {
		switch args.String(0) {
		case rawID:
		case nilID:
			select {
			case enteredWrite <- struct{}{}:
			default:
			}
			<-releaseWrite
		case freshID:
			select {
			case freshWriteC <- struct{}{}:
			default:
			}
		}
	})
	suite.csms.Start(8887, "somePath")
	defer suite.csms.Stop()
	suite.mockWsServer.NewClientHandler(NewMockWebSocket(rawID))
	suite.mockWsServer.NewClientHandler(NewMockWebSocket(nilID))
	suite.mockWsServer.NewClientHandler(NewMockWebSocket(freshID))

	err := suite.serverDispatcher.SendRequest(rawID, ocppj.RequestBundle{
		Call: &ocppj.Call{MessageTypeId: ocppj.CALL, UniqueId: defaultMessageId, Action: availability.HeartbeatFeatureName, Payload: nil},
		Data: []byte(`[2,"1234","Heartbeat",{}]`),
	})
	require.NoError(t, err)
	// This companion branch is already safe on master: the pump recovers its
	// nil-payload panic as CancelHandlerKind. After EC-D1 it produces no panic.
	select {
	case hp := <-panicC:
		require.Equal(t, ocppj.CancelHandlerKind, hp.Kind)
	case <-time.After(300 * time.Millisecond):
	}

	// The raw nil-payload bundle is first; the suite's constant generator makes
	// its ID collide with the facade registration. This is defense-in-depth for
	// a latent hazard gated by that collision, not an ordinary facade crash.
	err = suite.serverDispatcher.SendRequest(nilID, ocppj.RequestBundle{
		Call: &ocppj.Call{MessageTypeId: ocppj.CALL, UniqueId: defaultMessageId, Action: availability.HeartbeatFeatureName, Payload: nil},
		Data: []byte(`[2,"1234","Heartbeat",{}]`),
	})
	require.NoError(t, err)
	select {
	case <-enteredWrite:
	case <-time.After(ecd1Wait):
		t.Fatal("timed out waiting for nil-payload Write")
	}
	callbackC := make(chan error, 2)
	var callbackCount int32
	err = suite.csms.SendRequestAsync(nilID, availability.NewChangeAvailabilityRequest(availability.OperationalStatusOperative), func(_ ocpp.Response, err error) {
		atomic.AddInt32(&callbackCount, 1)
		callbackC <- err
	})
	require.NoError(t, err)
	close(releaseWrite)
	select {
	case callbackErr := <-callbackC:
		require.Error(t, callbackErr)
	case <-time.After(ecd1Wait):
		t.Fatal("timed out waiting for nil-payload cancellation callback")
	}
	assertECD1NoCallback(t, callbackC, 300*time.Millisecond)
	require.Equal(t, int32(1), atomic.LoadInt32(&callbackCount))
	select {
	case hp := <-panicC:
		t.Fatalf("nil-payload facade cancellation reported a panic: %+v", hp)
	default:
	}
	_, err = suite.ocppjServer.SendRequest(freshID, availability.NewHeartbeatRequest())
	require.NoError(t, err)
	select {
	case <-freshWriteC:
	case <-time.After(ecd1Wait):
		t.Fatal("dispatcher pump did not remain live after nil-payload cancellation")
	}
}
