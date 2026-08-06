package ocppj

import (
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/enesismail/ocpp-go/ocpp"
	"github.com/enesismail/ocpp-go/ws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type ec1CancelEvent struct {
	clientID  string
	requestID string
	request   ocpp.Request
	err       *ocpp.Error
}

func ec1NewServer(t *testing.T) (*DefaultServerDispatcher, ServerState, *FIFOQueueMap, *d2FakeServer, *Server) {
	t.Helper()
	d, state, queueMap, network, _ := d2NewDispatcher(t)
	server := NewServer(network, d, state, ocpp.NewProfile("d2mock", &d2MockFeature{}))
	return d, state, queueMap.(*FIFOQueueMap), network, server
}

func ec1WaitForCancel(t *testing.T, canceled <-chan ec1CancelEvent, count int) []ec1CancelEvent {
	t.Helper()
	result := make([]ec1CancelEvent, 0, count)
	for len(result) < count {
		select {
		case event := <-canceled:
			result = append(result, event)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for stop-cancel %d of %d", len(result)+1, count)
		}
	}
	return result
}

func ec1StopWithWatchdog(t *testing.T, d *DefaultServerDispatcher) {
	t.Helper()
	stopDone := make(chan struct{})
	go func() {
		d.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop hung")
	}
}

// TestEC1ServerStopCancelsPendingRequest covers test 1: a dispatched request
// remains in its client queue until Stop drains it and receives exactly one
// ErrDispatcherStopped cancellation with its original payload.
func TestEC1ServerStopCancelsPendingRequest(t *testing.T) {
	d, state, queueMap, network, endpoint := ec1NewServer(t)
	clientID := "ec1-client-1"
	written := make(chan struct{}, 1)
	network.setOnWrite(func(string, []byte) error {
		written <- struct{}{}
		return nil
	})
	canceled := make(chan ec1CancelEvent, 1)
	d.SetOnRequestCanceled(func(cID, rID string, request ocpp.Request, err *ocpp.Error) {
		canceled <- ec1CancelEvent{clientID: cID, requestID: rID, request: request, err: err}
	})

	d.Start()
	d.CreateClient(clientID)
	bundle, requestID := d2NewBundle(t, endpoint, "pending")
	require.NoError(t, d.SendRequest(clientID, bundle))
	select {
	case <-written:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the request to be dispatched")
	}
	require.True(t, state.HasPendingRequest(clientID))

	ec1StopWithWatchdog(t, d)
	event := ec1WaitForCancel(t, canceled, 1)[0]
	assert.Equal(t, clientID, event.clientID)
	assert.Equal(t, requestID, event.requestID)
	assert.Equal(t, bundle.Call.Payload, event.request)
	assert.True(t, errors.Is(event.err, ErrDispatcherStopped))
	assert.False(t, state.HasPendingRequest(clientID))
	_, ok := queueMap.Get(clientID)
	assert.False(t, ok, "stop must detach the client queue map")
}

// TestEC1ServerStopCancelsAllClients covers test 2: front and queued bundles
// are drained FIFO for multiple clients and preserve the client attribution.
func TestEC1ServerStopCancelsAllClients(t *testing.T) {
	d, _, queueMap, network, endpoint := ec1NewServer(t)
	clientIDs := []string{"ec1-client-a", "ec1-client-b"}
	written := make(chan string, 2)
	network.setOnWrite(func(clientID string, _ []byte) error {
		written <- clientID
		return nil
	})
	canceled := make(chan ec1CancelEvent, 4)
	d.SetOnRequestCanceled(func(cID, rID string, request ocpp.Request, err *ocpp.Error) {
		canceled <- ec1CancelEvent{clientID: cID, requestID: rID, request: request, err: err}
	})

	d.Start()
	idsByClient := map[string][]string{}
	for _, clientID := range clientIDs {
		d.CreateClient(clientID)
		front, frontID := d2NewBundle(t, endpoint, clientID+"-front")
		queued, queuedID := d2NewBundle(t, endpoint, clientID+"-queued")
		idsByClient[clientID] = []string{frontID, queuedID}
		require.NoError(t, d.SendRequest(clientID, front))
		require.NoError(t, d.SendRequest(clientID, queued))
	}
	for range clientIDs {
		select {
		case <-written:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for both front requests to be dispatched")
		}
	}

	ec1StopWithWatchdog(t, d)
	events := ec1WaitForCancel(t, canceled, 4)
	seen := map[string]bool{}
	for _, event := range events {
		assert.True(t, errors.Is(event.err, ErrDispatcherStopped))
		key := fmt.Sprintf("%s/%s", event.clientID, event.requestID)
		assert.False(t, seen[key], "duplicate stop-cancel for %s", key)
		seen[key] = true
	}
	for clientID, ids := range idsByClient {
		for _, requestID := range ids {
			assert.True(t, seen[fmt.Sprintf("%s/%s", clientID, requestID)])
		}
		_, ok := queueMap.Get(clientID)
		assert.False(t, ok, "stop must detach the client queue map")
	}
}

// TestEC1ServerStopCompletePhotoFinish covers test 3. The outer watchdog is
// mandatory: if the pre-existing cap-1 readyForDispatch leak wins the race,
// the test must fail cleanly instead of pinning the test process.
func TestEC1ServerStopCompletePhotoFinish(t *testing.T) {
	const iterations = 100
	var totalResponseCount int32
	var totalCancelCount int32
	for iteration := 0; iteration < iterations; iteration++ {
		d, _, _, network, server := ec1NewServer(t)
		clientID := fmt.Sprintf("ec1-photo-%d", iteration)
		written := make(chan struct{}, 1)
		network.setOnWrite(func(string, []byte) error {
			written <- struct{}{}
			return nil
		})
		var responseCount int32
		var cancelCount int32
		d.SetOnRequestCanceled(func(string, string, ocpp.Request, *ocpp.Error) {
			atomic.AddInt32(&cancelCount, 1)
			atomic.AddInt32(&totalCancelCount, 1)
		})
		server.SetResponseHandler(func(ws.Channel, ocpp.Response, string) {
			atomic.AddInt32(&responseCount, 1)
			atomic.AddInt32(&totalResponseCount, 1)
		})

		d.Start()
		d.CreateClient(clientID)
		bundle, requestID := d2NewBundle(t, server, "photo-finish")
		require.NoError(t, d.SendRequest(clientID, bundle))
		select {
		case <-written:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for photo-finish request")
		}

		resultDone := make(chan struct{})
		stopDone := make(chan struct{})
		go func() {
			time.Sleep(time.Duration(rand.Intn(400)) * time.Microsecond)
			d.Stop()
			close(stopDone)
		}()
		go func() {
			_ = server.ocppMessageHandler(&e2aChannel{id: clientID}, []byte(fmt.Sprintf(`[3,"%s",{"mockValue":"response"}]`, requestID)))
			close(resultDone)
		}()
		select {
		case <-resultDone:
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: late result path hung", iteration)
		}
		select {
		case <-stopDone:
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: Stop hung; possible readyForDispatch leak", iteration)
		}
		assert.Equal(t, int32(1), atomic.LoadInt32(&responseCount)+atomic.LoadInt32(&cancelCount), "iteration %d request must have one terminal outcome", iteration)
	}
	responses := atomic.LoadInt32(&totalResponseCount)
	cancels := atomic.LoadInt32(&totalCancelCount)
	t.Logf("photo-finish branch counts: response-wins=%d stop-cancels=%d", responses, cancels)
	assert.GreaterOrEqual(t, responses, int32(1), "photo-finish must observe at least one response-win")
	assert.GreaterOrEqual(t, cancels, int32(1), "photo-finish must observe at least one stop-cancel")
}

// TestEC1ServerStopRejectsLateCallResult covers test 4 through ocppj.Server:
// the ParseMessage pending gate rejects post-Stop responses, so delivery is
// suppressed.
func TestEC1ServerStopRejectsLateCallResult(t *testing.T) {
	d, state, _, network, server := ec1NewServer(t)
	clientID := "ec1-late-result"
	written := make(chan struct{}, 1)
	network.setOnWrite(func(string, []byte) error {
		written <- struct{}{}
		return nil
	})
	canceled := make(chan ec1CancelEvent, 1)
	d.SetOnRequestCanceled(func(cID, rID string, request ocpp.Request, err *ocpp.Error) {
		canceled <- ec1CancelEvent{clientID: cID, requestID: rID, request: request, err: err}
	})
	var responses int32
	server.SetResponseHandler(func(ws.Channel, ocpp.Response, string) {
		atomic.AddInt32(&responses, 1)
	})

	d.Start()
	d.CreateClient(clientID)
	bundle, requestID := d2NewBundle(t, server, "late-result")
	require.NoError(t, d.SendRequest(clientID, bundle))
	select {
	case <-written:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for late-result request")
	}
	ec1StopWithWatchdog(t, d)
	ec1WaitForCancel(t, canceled, 1)
	require.False(t, state.HasPendingRequest(clientID))

	require.NoError(t, server.ocppMessageHandler(&e2aChannel{id: clientID}, []byte(fmt.Sprintf(`[3,"%s",{"mockValue":"late"}]`, requestID))))
	assert.Equal(t, int32(0), atomic.LoadInt32(&responses), "late CALL_RESULT must not reach responseHandler")
}

// TestEC1ServerStopRecoversPanickingCancel covers test 5. The Errors()
// assertion is intentionally absent: that routing belongs to EC3, not EC1.
func TestEC1ServerStopRecoversPanickingCancel(t *testing.T) {
	d, _, queueMap, network, server := ec1NewServer(t)
	clientID := "ec1-panic"
	written := make(chan struct{}, 1)
	network.setOnWrite(func(string, []byte) error {
		written <- struct{}{}
		return nil
	})
	var cancelCount int32
	d.SetOnRequestCanceled(func(string, string, ocpp.Request, *ocpp.Error) {
		atomic.AddInt32(&cancelCount, 1)
		panic("ec1 cancel callback panic")
	})
	server.SetOnHandlerPanic(func(HandlerPanic) {})

	d.Start()
	d.CreateClient(clientID)
	front, _ := d2NewBundle(t, server, "panic-front")
	queued, _ := d2NewBundle(t, server, "panic-queued")
	require.NoError(t, d.SendRequest(clientID, front))
	select {
	case <-written:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for panic test request")
	}
	require.NoError(t, d.SendRequest(clientID, queued))

	stopDone := make(chan struct{})
	go func() {
		d.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop hung after a panicking cancel callback")
	}
	assert.Equal(t, int32(2), atomic.LoadInt32(&cancelCount))
	_, ok := queueMap.Get(clientID)
	assert.False(t, ok, "stop must clear the legacy queue map")
}

// ec1LegacyQueueMap deliberately implements only ServerQueueMap. Test 6
// pins the compatibility fallback: custom maps without DrainAll keep the
// legacy clear-only stop behavior and must not receive speculative cancels.
type ec1LegacyQueueMap struct {
	inner *FIFOQueueMap
}

func (m *ec1LegacyQueueMap) Init() { m.inner.Init() }
func (m *ec1LegacyQueueMap) Get(clientID string) (RequestQueue, bool) {
	return m.inner.Get(clientID)
}
func (m *ec1LegacyQueueMap) GetOrCreate(clientID string) RequestQueue {
	return m.inner.GetOrCreate(clientID)
}
func (m *ec1LegacyQueueMap) Remove(clientID string) { m.inner.Remove(clientID) }
func (m *ec1LegacyQueueMap) Add(clientID string, queue RequestQueue) {
	m.inner.Add(clientID, queue)
}

func TestEC1CustomServerQueueMapKeepsLegacyStop(t *testing.T) {
	legacyMap := &ec1LegacyQueueMap{inner: NewFIFOQueueMap(10)}
	d := NewDefaultServerDispatcher(legacyMap)
	state := NewServerState(&sync.RWMutex{})
	d.SetPendingRequestState(state)
	network := &d2FakeServer{}
	d.SetNetworkServer(network)
	endpoint := &Server{}
	endpoint.AddProfile(ocpp.NewProfile("d2mock", &d2MockFeature{}))
	clientID := "ec1-legacy-map"
	written := make(chan struct{}, 1)
	network.setOnWrite(func(string, []byte) error {
		written <- struct{}{}
		return nil
	})
	var cancelCount int32
	d.SetOnRequestCanceled(func(string, string, ocpp.Request, *ocpp.Error) {
		atomic.AddInt32(&cancelCount, 1)
	})

	d.Start()
	d.CreateClient(clientID)
	bundle, _ := d2NewBundle(t, endpoint, "legacy")
	require.NoError(t, d.SendRequest(clientID, bundle))
	select {
	case <-written:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for legacy-map request")
	}
	ec1StopWithWatchdog(t, d)
	assert.Equal(t, int32(0), atomic.LoadInt32(&cancelCount))
}

// TestEC1StopCancelCallbackMayReadDispatcherState pins the unlock-before-join
// barrier: the stop drain runs user code on the pump inside Stop's join
// window, so an RLock in that callback would deadlock against a held write
// lock if Stop joined before unlocking it.
func TestEC1StopCancelCallbackMayReadDispatcherState(t *testing.T) {
	d, _, _, network, endpoint := ec1NewServer(t)
	clientID := "ec1-callback-state"
	written := make(chan struct{}, 1)
	network.setOnWrite(func(string, []byte) error {
		written <- struct{}{}
		return nil
	})
	callbackRead := make(chan bool, 1)
	d.SetOnRequestCanceled(func(string, string, ocpp.Request, *ocpp.Error) {
		callbackRead <- d.IsRunning()
	})

	d.Start()
	d.CreateClient(clientID)
	bundle, _ := d2NewBundle(t, endpoint, "callback-state")
	require.NoError(t, d.SendRequest(clientID, bundle))
	select {
	case <-written:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for callback-state request")
	}
	ec1StopWithWatchdog(t, d)
	select {
	case <-callbackRead:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for callback to read dispatcher state")
	}
}

type ec1ProbeQueue struct {
	RequestQueue
	pushed int32
}

func (q *ec1ProbeQueue) Push(element interface{}) error {
	atomic.StoreInt32(&q.pushed, 1)
	return q.RequestQueue.Push(element)
}

// TestEC1SendErrorAndStopCancelProbe statistically probes the documented
// SendRequest-error/Stop-cancel enqueue race. Either terminal path may win,
// and both may be observed as the returned send error plus one stop cancel.
func TestEC1SendErrorAndStopCancelProbe(t *testing.T) {
	const iterations = 200
	var bothCount int
	for iteration := 0; iteration < iterations; iteration++ {
		d, _, queueMap, network, endpoint := ec1NewServer(t)
		clientID := fmt.Sprintf("ec1-probe-%d", iteration)
		probeQueue := &ec1ProbeQueue{RequestQueue: NewFIFOClientQueue(10)}
		queueMap.Add(clientID, probeQueue)
		network.setOnWrite(func(string, []byte) error { return nil })
		canceled := make(chan *ocpp.Error, 2)
		d.SetOnRequestCanceled(func(_ string, _ string, _ ocpp.Request, err *ocpp.Error) {
			canceled <- err
		})

		d.Start()
		d.CreateClient(clientID)
		bundle, requestID := d2NewBundle(t, endpoint, "send-stop-probe")
		sendDone := make(chan error, 1)
		go func() {
			time.Sleep(time.Duration(rand.Intn(400)) * time.Microsecond)
			sendDone <- d.SendRequest(clientID, bundle)
		}()
		stopDone := make(chan struct{})
		go func() {
			time.Sleep(time.Duration(rand.Intn(400)) * time.Microsecond)
			d.Stop()
			close(stopDone)
		}()

		var sendErr error
		select {
		case sendErr = <-sendDone:
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: SendRequest hung", iteration)
		}
		select {
		case <-stopDone:
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: Stop hung", iteration)
		}

		var events []*ocpp.Error
	Drain:
		for {
			select {
			case err := <-canceled:
				events = append(events, err)
			default:
				break Drain
			}
		}
		assert.LessOrEqual(t, len(events), 1, "request %s received duplicate cancel callbacks", requestID)
		if len(events) != 0 {
			assert.Equal(t, int32(1), atomic.LoadInt32(&probeQueue.pushed), "request %s was canceled without being pushed", requestID)
			assert.True(t, errors.Is(events[0], ErrDispatcherStopped), "unexpected cancel error for %s: %v", requestID, events[0])
		}
		sendFailed := sendErr != nil
		canceledByStop := len(events) == 1
		assert.True(t, sendFailed || canceledByStop, "iteration %d produced neither documented outcome", iteration)
		if sendFailed && canceledByStop {
			bothCount++
		}
	}
	t.Logf("send-error/stop-cancel probe: both-count=%d/%d", bothCount, iterations)
}

func TestEC1FIFOQueueMapDrainAllIsOptionalAtomicDetach(t *testing.T) {
	queueMap := NewFIFOQueueMap(10)
	queue := NewFIFOClientQueue(10)
	require.NoError(t, queue.Push("first"))
	queueMap.Add("client", queue)
	drainer, ok := interface{}(queueMap).(interface {
		DrainAll() map[string]RequestQueue
	})
	require.True(t, ok, "FIFOQueueMap must expose the optional concrete DrainAll method")
	detached := drainer.DrainAll()
	assert.Same(t, queue, detached["client"])
	_, stillPresent := queueMap.Get("client")
	assert.False(t, stillPresent)
	assert.Equal(t, "first", detached["client"].Pop())
}
