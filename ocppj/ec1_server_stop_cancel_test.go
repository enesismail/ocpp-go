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

// ec1PhotoFinishOrder selects how a photo-finish iteration orders Stop against
// the inbound CALL_RESULT.
type ec1PhotoFinishOrder int

const (
	// ec1PhotoStaggered starts both with a random sub-millisecond offset: the
	// genuine race, whose winner is up to the scheduler.
	ec1PhotoStaggered ec1PhotoFinishOrder = iota
	// ec1PhotoStopFirst delivers the CALL_RESULT only after Stop has returned.
	ec1PhotoStopFirst
	// ec1PhotoResultFirst issues Stop only after the CALL_RESULT is handled.
	ec1PhotoResultFirst
)

func ec1Await(t *testing.T, what string, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// ec1RunPhotoFinish races one request's completion against Stop in the given
// order and returns (responseWins, stopCancels) for that single request. It
// asserts the exactly-once invariant itself, so every ordering is covered by it.
func ec1RunPhotoFinish(t *testing.T, name string, order ec1PhotoFinishOrder) (int32, int32) {
	t.Helper()
	d, _, _, network, server := ec1NewServer(t)
	clientID := "ec1-photo-" + name
	written := make(chan struct{}, 1)
	network.setOnWrite(func(string, []byte) error {
		written <- struct{}{}
		return nil
	})
	var responseCount int32
	var cancelCount int32
	d.SetOnRequestCanceled(func(string, string, ocpp.Request, *ocpp.Error) {
		atomic.AddInt32(&cancelCount, 1)
	})
	server.SetResponseHandler(func(ws.Channel, ocpp.Response, string) {
		atomic.AddInt32(&responseCount, 1)
	})

	d.Start()
	d.CreateClient(clientID)
	bundle, requestID := d2NewBundle(t, server, "photo-finish")
	require.NoError(t, d.SendRequest(clientID, bundle))
	ec1Await(t, name+": request dispatch", written)

	result := []byte(fmt.Sprintf(`[3,"%s",{"mockValue":"response"}]`, requestID))
	resultDone := make(chan struct{})
	stopDone := make(chan struct{})
	deliverResult := func() {
		_ = server.ocppMessageHandler(&e2aChannel{id: clientID}, result)
		close(resultDone)
	}
	stop := func() {
		d.Stop()
		close(stopDone)
	}
	switch order {
	case ec1PhotoStaggered:
		go func() {
			time.Sleep(time.Duration(rand.Intn(400)) * time.Microsecond)
			stop()
		}()
		go deliverResult()
	case ec1PhotoStopFirst:
		go stop()
		ec1Await(t, name+": Stop", stopDone)
		go deliverResult()
	case ec1PhotoResultFirst:
		go deliverResult()
		ec1Await(t, name+": late result path", resultDone)
		go stop()
	}
	ec1Await(t, name+": late result path", resultDone)
	// The Stop watchdog is mandatory: if the pre-existing cap-1
	// readyForDispatch leak wins the race, this must fail cleanly instead of
	// pinning the test process.
	ec1Await(t, name+": Stop (possible readyForDispatch leak)", stopDone)

	responses := atomic.LoadInt32(&responseCount)
	cancels := atomic.LoadInt32(&cancelCount)
	assert.Equal(t, int32(1), responses+cancels, "%s: request must have exactly one terminal outcome", name)
	return responses, cancels
}

// TestEC1ServerStopCompletePhotoFinish covers test 3: a request completing
// off-pump while Stop drains must resolve to exactly one of {response handler,
// stop cancel}, never both and never neither.
//
// Branch coverage is deterministic rather than statistical. The staggered phase
// exercises the genuine race and asserts the exactly-once invariant on every
// iteration, but which branch wins there is the scheduler's choice: stop-wins
// were measured at roughly 7-10% per iteration, so demanding that the staggered
// phase alone produce at least one of each leaves a ~0.93^N tail of legitimate
// runs that fail — about 1 in 1400 at N=100, and worse under -race, which skews
// the timing. Raising N only thins that tail. The two ordered iterations below
// remove it: each pins its ordering with a happens-before edge, so both branches
// are guaranteed to be observed no matter how the staggered phase falls.
func TestEC1ServerStopCompletePhotoFinish(t *testing.T) {
	const staggeredIterations = 100
	var responses, cancels int32
	for iteration := 0; iteration < staggeredIterations; iteration++ {
		r, c := ec1RunPhotoFinish(t, fmt.Sprintf("staggered-%d", iteration), ec1PhotoStaggered)
		responses += r
		cancels += c
	}
	t.Logf("staggered photo-finish branch counts: response-wins=%d stop-cancels=%d", responses, cancels)

	r, c := ec1RunPhotoFinish(t, "stop-first", ec1PhotoStopFirst)
	responses += r
	cancels += c
	assert.Equal(t, int32(1), c, "a CALL_RESULT offered after Stop returned must resolve as a stop-cancel")

	r, c = ec1RunPhotoFinish(t, "result-first", ec1PhotoResultFirst)
	responses += r
	cancels += c
	assert.Equal(t, int32(1), r, "a CALL_RESULT handled before Stop was issued must resolve as a response")

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

// TestEC1ServerStopRecoversPanickingCancel covers test 5. The Errors() assertion
// lives at the facade layer, where the error channel exists — see
// TestEC3StopDrainPanicReportedOnErrors. This raw-endpoint test is unchanged by
// that routing.
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

// ec1GatedQueueMap parks the first GetOrCreate for gateClient until the test
// releases it, so a test can hold CreateClient inside its queue creation while
// a concurrent Stop runs. DrainAll is implemented so the dispatcher still takes
// the draining stop arm rather than the legacy clear-only fallback.
type ec1GatedQueueMap struct {
	inner      *FIFOQueueMap
	gateClient string
	reached    chan struct{}
	release    chan struct{}
	once       sync.Once
}

func (m *ec1GatedQueueMap) Init() { m.inner.Init() }
func (m *ec1GatedQueueMap) Get(clientID string) (RequestQueue, bool) {
	return m.inner.Get(clientID)
}
func (m *ec1GatedQueueMap) GetOrCreate(clientID string) RequestQueue {
	if clientID == m.gateClient {
		m.once.Do(func() {
			close(m.reached)
			<-m.release
		})
	}
	return m.inner.GetOrCreate(clientID)
}
func (m *ec1GatedQueueMap) Remove(clientID string) { m.inner.Remove(clientID) }
func (m *ec1GatedQueueMap) Add(clientID string, queue RequestQueue) {
	m.inner.Add(clientID, queue)
}
func (m *ec1GatedQueueMap) DrainAll() map[string]RequestQueue { return m.inner.DrainAll() }

// TestEC1StopDrainCannotBeOutrunByCreateClient pins that a queue created by
// CreateClient can never appear in the map after the stop drain has detached it.
// The gated queue map reproduces the interleaving deterministically: the running
// check has already passed when Stop begins, and the queue creation completes
// only afterwards.
//
// If the two steps are not atomic, the reinserted queue is invisible to the
// drain and outlives generation 1. SendRequest pushes before its own running
// check, so a caller that is told its send failed still leaves its bundle at the
// head of that surviving queue, and the next generation's pump dispatches it.
// The assertion is on exactly that observable: the first thing generation 2
// writes to the network must be generation 2's own request.
func TestEC1StopDrainCannotBeOutrunByCreateClient(t *testing.T) {
	clientID := "ec1-create-vs-stop"
	queueMap := &ec1GatedQueueMap{
		inner:      NewFIFOQueueMap(10),
		gateClient: clientID,
		reached:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	d := NewDefaultServerDispatcher(queueMap)
	d.SetPendingRequestState(NewServerState(&sync.RWMutex{}))
	network := &d2FakeServer{}
	d.SetNetworkServer(network)
	endpoint := &Server{}
	endpoint.AddProfile(ocpp.NewProfile("d2mock", &d2MockFeature{}))
	writes := make(chan string, 8)
	network.setOnWrite(func(_ string, data []byte) error {
		writes <- string(data)
		return nil
	})

	d.Start()
	createDone := make(chan struct{})
	go func() {
		d.CreateClient(clientID)
		close(createDone)
	}()
	select {
	case <-queueMap.reached:
	case <-time.After(2 * time.Second):
		t.Fatal("CreateClient never reached the queue map")
	}

	stopDone := make(chan struct{})
	go func() {
		d.Stop()
		close(stopDone)
	}()
	// Long enough for an unsynchronised Stop to run its whole teardown, drain
	// included, while the queue creation is still parked. When the two steps are
	// atomic, Stop is instead blocked on the read lock the parked CreateClient
	// still holds, and this sleep only delays it.
	time.Sleep(100 * time.Millisecond)
	close(queueMap.release)
	select {
	case <-createDone:
	case <-time.After(2 * time.Second):
		t.Fatal("CreateClient never returned")
	}
	select {
	case <-stopDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop hung")
	}

	// Generation 1 leftover: a send the stopped dispatcher rejects.
	stale, staleID := d2NewBundle(t, endpoint, "stale-generation-1")
	require.Error(t, d.SendRequest(clientID, stale), "a stopped dispatcher must reject sends")

	d.Start()
	defer d.Stop()
	d.CreateClient(clientID)
	fresh, freshID := d2NewBundle(t, endpoint, "fresh-generation-2")
	require.NoError(t, d.SendRequest(clientID, fresh))
	select {
	case data := <-writes:
		assert.NotContains(t, data, staleID,
			"generation 2 dispatched a generation-1 request whose SendRequest had returned an error")
		assert.Contains(t, data, freshID, "generation 2 must dispatch its own request first")
	case <-time.After(2 * time.Second):
		t.Fatal("generation 2 dispatched nothing")
	}
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
