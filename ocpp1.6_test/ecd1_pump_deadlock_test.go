package ocpp16_test

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
	ocpp16 "github.com/enesismail/ocpp-go/ocpp1.6"
	"github.com/enesismail/ocpp-go/ocpp1.6/core"
	"github.com/enesismail/ocpp-go/ocppj"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const ecd1Wait = 2 * time.Second

// saturateECD1FacadeSenders issues facade sends one at a time, round-robin
// across the filler clients, each on its own goroutine, until one fails to
// return within settleBound. TryQueue serialises every sender on callbacksMutex,
// so the first non-returning send is a sender parked on the full requestChannel
// while holding callbacksMutex. Returned errors are expected: a per-client
// queue can fill before its channel signal, and the suite's constant generator
// can report ErrDuplicateCallback after a successful signal. Only a
// "dispatcher not running" error invalidates the precondition.
func saturateECD1FacadeSenders(t *testing.T, centralSystem ocpp16.CentralSystem, ids []string, releaseWedgeWrite <-chan struct{}) {
	t.Helper()
	const (
		settleBound = 300 * time.Millisecond
		maxSends    = 100
	)

	// requestChannel is cap 20 and each per-client queue is cap 10. A single
	// client can therefore stop contributing signals at roughly 10 pushes;
	// four fillers provide roughly 40 channel-reaching sends, enough to park
	// one sender after 20 buffered signals. The release channel stays open for
	// this whole loop: closing it would release the wedge and let the pump drain
	// a signal between sends, invalidating the saturation proof.
	_ = releaseWedgeWrite
	var observedErrors []error
	for i := 0; i < maxSends; i++ {
		clientID := ids[i%len(ids)]
		doneC := make(chan error, 1)
		go func() {
			doneC <- centralSystem.SendRequestAsync(clientID,
				core.NewChangeAvailabilityRequest(1, core.AvailabilityTypeOperative),
				func(ocpp.Response, error) {})
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

// TestECD1PumpSurvivesCancelWithSaturatedRequestChannel is intentionally a
// standalone test with its own endpoint. On the unchanged build the parked
// sender and pump can leak for the life of the test binary, so sharing a
// testify suite would poison later tests. The four dispatcher cancel sites
// are timer expiry (:912), caller context cancellation (:985), pre-write drop
// (:1061), and websocket Write failure (:1079). This test chooses :1079: the
// cancel fires in the same pump iteration as the wedged Write, before a select
// can drain another buffered signal.
func TestECD1PumpSurvivesCancelWithSaturatedRequestChannel(t *testing.T) {
	testDoneC := make(chan struct{})
	go func() {
		defer close(testDoneC)

		mockServer := &MockWebsocketServer{}
		dispatcher := ocppj.NewDefaultServerDispatcher(ocppj.NewFIFOQueueMap(queueCapacity))
		endpoint := ocppj.NewServer(mockServer, dispatcher, nil, core.Profile)
		centralSystem := ocpp16.NewCentralSystem(endpoint, mockServer)
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

		centralSystem.Start(8887, "somePath")
		for _, clientID := range append(append([]string{wedgeID}, fillerIDs...), freshID) {
			mockServer.NewClientHandler(NewMockWebSocket(clientID))
		}
		var callbackCount int32
		callbackC := make(chan error, 2)
		err := centralSystem.SendRequestAsync(wedgeID, core.NewChangeAvailabilityRequest(1, core.AvailabilityTypeOperative), func(_ ocpp.Response, err error) {
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

		// Keep releaseWedgeWrite OPEN while saturateECD1FacadeSenders runs. The
		// request channel's capacity is 20, while each client queue is 10; four
		// fillers make the round-robin ceiling about 40 channel-reaching sends.
		saturateECD1FacadeSenders(t, centralSystem, fillerIDs, releaseWedgeWrite)
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

		_, err = endpoint.SendRequest(freshID, core.NewHeartbeatRequest())
		require.NoError(t, err)
		select {
		case <-freshWriteC:
		case <-time.After(ecd1Wait):
			t.Errorf("pump did not write a fresh client's request after the cancel")
		}

		senderDoneC := make(chan error, 1)
		go func() {
			senderDoneC <- centralSystem.SendRequestAsync(fillerIDs[0], core.NewChangeAvailabilityRequest(1, core.AvailabilityTypeInoperative), func(ocpp.Response, error) {})
		}()
		select {
		case <-senderDoneC:
		case <-time.After(ecd1Wait):
			t.Errorf("subsequent facade sender did not return")
		}

		// Stop must pass dispatcher.Stop's d.mutex.Lock barrier (:700), not
		// merely reach the message-pump join at :710.
		stopDoneC := make(chan struct{})
		go func() {
			centralSystem.Stop()
			close(stopDoneC)
		}()
		select {
		case <-stopDoneC:
		case <-time.After(ecd1Wait):
			t.Errorf("central system Stop did not return")
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

func (suite *OcppV16TestSuite) TestECD1CancelCallbackExactlyOnceOffPump() {
	t := suite.T()
	timeoutID := "ecd1-timeout"
	dupID := "ecd1-duplicate"
	suite.serverDispatcher.SetTimeout(300 * time.Millisecond)
	suite.mockWsServer.On("Start", mock.AnythingOfType("int"), mock.AnythingOfType("string")).Return(nil)
	suite.mockWsServer.On("Stop").Return()
	suite.mockWsServer.On("Write", dupID, mock.Anything).Return(fmt.Errorf("write failed for duplicate request"))
	suite.mockWsServer.On("Write", mock.AnythingOfType("string"), mock.Anything).Return(nil).Run(func(args mock.Arguments) {})
	suite.centralSystem.Start(8887, "somePath")
	defer suite.centralSystem.Stop()
	suite.mockWsServer.NewClientHandler(NewMockWebSocket(timeoutID))
	suite.mockWsServer.NewClientHandler(NewMockWebSocket(dupID))

	var timeoutCount int32
	timeoutC := make(chan error, 2)
	err := suite.centralSystem.SendRequestAsync(timeoutID, core.NewChangeAvailabilityRequest(1, core.AvailabilityTypeOperative), func(_ ocpp.Response, err error) {
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
	err = suite.centralSystem.SendRequestAsync(dupID, core.NewChangeAvailabilityRequest(1, core.AvailabilityTypeOperative), func(_ ocpp.Response, err error) {
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
	// Load-bearing barrier: Dequeue precedes the callback on both builds, so
	// callback delivery proves (dupID, "1234") is released before reuse.
	assertECD1NoCallback(t, duplicateC, 300*time.Millisecond)
	assert.Equal(t, int32(1), atomic.LoadInt32(&duplicateCount))

	errC := suite.centralSystem.Errors()
	_, err = suite.ocppjCentralSystem.SendRequest(dupID, core.NewChangeAvailabilityRequest(1, core.AvailabilityTypeInoperative))
	require.NoError(t, err)
	deadline := time.NewTimer(ecd1Wait)
	defer deadline.Stop()
	errorCount := 0
	for {
		select {
		case report := <-errC:
			if strings.Contains(report.Error(), "no handler available for canceled request") {
				errorCount++
			}
		case <-deadline.C:
			require.Equal(t, 1, errorCount)
			return
		}
		if errorCount == 1 {
			assertECD1NoErrors(t, errC, 300*time.Millisecond)
			require.Equal(t, 1, errorCount)
			return
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

func (suite *OcppV16TestSuite) TestECD1CancelNoCallbackReportsOnErrors() {
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
	suite.centralSystem.Start(8887, "somePath")
	defer suite.centralSystem.Stop()
	suite.mockWsServer.NewClientHandler(NewMockWebSocket(rawID))
	suite.mockWsServer.NewClientHandler(NewMockWebSocket(freshID))

	errC := suite.centralSystem.Errors()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := suite.ocppjCentralSystem.SendRequestCtx(ctx, rawID, core.NewHeartbeatRequest())
	require.NoError(t, err)
	select {
	case report := <-errC:
		require.Contains(t, report.Error(), "no handler available for canceled request")
	case <-time.After(ecd1Wait):
		t.Fatal("timed out waiting for no-callback cancellation report")
	}

	_, err = suite.ocppjCentralSystem.SendRequest(freshID, core.NewHeartbeatRequest())
	require.NoError(t, err)
	select {
	case got := <-writesC:
		require.Equal(t, freshID, got)
	case <-time.After(ecd1Wait):
		t.Fatal("dispatcher pump did not write after no-callback cancellation")
	}
}

func (suite *OcppV16TestSuite) TestECD1NilPayloadCancelWithRegisteredCallbackDoesNotCrash() {
	t := suite.T()
	panicC := make(chan ocppj.HandlerPanic, 2)
	suite.centralSystem.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) { panicC <- hp })
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
	suite.centralSystem.Start(8887, "somePath")
	defer suite.centralSystem.Stop()
	suite.mockWsServer.NewClientHandler(NewMockWebSocket(rawID))
	suite.mockWsServer.NewClientHandler(NewMockWebSocket(nilID))
	suite.mockWsServer.NewClientHandler(NewMockWebSocket(freshID))

	// Companion branch: without a registered facade callback, master recovers
	// the nil-payload feature-name panic on the pump as CancelHandlerKind. The
	// fixed build hoists the name and reports the no-callback error without any
	// panic. This branch is already safe on master and makes the reachability
	// split explicit.
	err := suite.serverDispatcher.SendRequest(rawID, ocppj.RequestBundle{
		Call: &ocppj.Call{MessageTypeId: ocppj.CALL, UniqueId: defaultMessageId, Action: core.HeartbeatFeatureName, Payload: nil},
		Data: []byte(`[2,"1234","Heartbeat",{}]`),
	})
	require.NoError(t, err)
	select {
	case hp := <-panicC:
		require.Equal(t, ocppj.CancelHandlerKind, hp.Kind)
	case <-time.After(300 * time.Millisecond):
	}

	// The raw nil-payload bundle must be queued before the facade send. The
	// constant suite generator makes both request IDs "1234", creating the
	// collision that makes this latent defense-in-depth path constructible;
	// ordinary facade sends cannot register a nil request themselves.
	err = suite.serverDispatcher.SendRequest(nilID, ocppj.RequestBundle{
		Call: &ocppj.Call{MessageTypeId: ocppj.CALL, UniqueId: defaultMessageId, Action: core.HeartbeatFeatureName, Payload: nil},
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
	err = suite.centralSystem.SendRequestAsync(nilID, core.NewChangeAvailabilityRequest(1, core.AvailabilityTypeOperative), func(_ ocpp.Response, err error) {
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

	_, err = suite.ocppjCentralSystem.SendRequest(freshID, core.NewHeartbeatRequest())
	require.NoError(t, err)
	select {
	case <-freshWriteC:
	case <-time.After(ecd1Wait):
		t.Fatal("dispatcher pump did not remain live after nil-payload cancellation")
	}
}
