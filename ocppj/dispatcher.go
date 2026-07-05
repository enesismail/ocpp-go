package ocppj

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/enesismail/ocpp-go/ocpp"
	"github.com/enesismail/ocpp-go/ws"
)

// ClientDispatcher contains the state and logic for handling outgoing messages on a client endpoint.
// This allows the ocpp-j layer to delegate queueing and processing logic to an external entity.
//
// The dispatcher writes outgoing messages directly to the networking layer, using a previously set websocket client.
//
// A ClientState needs to be passed to the dispatcher, before starting it.
// The dispatcher is in charge of managing pending requests while handling the request flow.
type ClientDispatcher interface {
	// Starts the dispatcher. Depending on the implementation, this may
	// start a dedicated goroutine or simply allocate the necessary state.
	Start()
	// Sets the maximum timeout to be considered after sending a request.
	// If a response to the request is not received within the specified period, the request
	// is discarded and an error is returned to the caller.
	//
	// The timeout is reset upon a disconnection/reconnection.
	//
	// This function must be called before starting the dispatcher, otherwise it may lead to unexpected behavior.
	SetTimeout(timeout time.Duration)
	// Returns true, if the dispatcher is currently running, false otherwise.
	// If the dispatcher is paused, the function still returns true.
	IsRunning() bool
	// Returns true, if the dispatcher is currently paused, false otherwise.
	// If the dispatcher is not running at all, the function will still return false.
	IsPaused() bool
	// Dispatches a request. Depending on the implementation, this may first queue a request
	// and process it later, asynchronously, or write it directly to the networking layer.
	//
	// If no network client was set, or the request couldn't be processed, an error is returned.
	SendRequest(req RequestBundle) error
	// Notifies the dispatcher that a request has been completed (i.e. a response was received).
	// The dispatcher takes care of removing the request marked by the requestID from
	// the pending requests. It will then attempt to process the next queued request.
	CompleteRequest(requestID string)
	// Sets a callback to be invoked when a request gets canceled, due to network timeouts or internal errors.
	// The callback passes the original message ID and request struct of the failed request, along with an error.
	//
	// Calling Stop on the dispatcher triggers this callback for every request still outstanding
	// at that point, with an error matching ErrDispatcherStopped.
	//
	// If no callback is set, a request will still be removed from the dispatcher when a timeout occurs.
	SetOnRequestCanceled(cb func(requestID string, request ocpp.Request, err *ocpp.Error))
	// Sets the network client, so the dispatcher may send requests using the networking layer directly.
	//
	// This needs to be set before calling the Start method. If not, sending requests will fail.
	SetNetworkClient(client ws.Client)
	// Sets the state manager for pending requests in the dispatcher.
	//
	// The state should only be accessed by the dispatcher while running.
	SetPendingRequestState(stateHandler ClientState)
	// Stops a running dispatcher. This will clear all state and empty the internal queues.
	//
	// If an onRequestCanceled callback is set, it is triggered for every request still
	// outstanding at Stop time, with an error matching ErrDispatcherStopped.
	Stop()
	// Notifies that an external event (typically network-related) should pause
	// the dispatcher. Internal timers will be stopped an no further requests
	// will be set to pending. You may keep enqueuing requests.
	// Use the Resume method for re-starting the dispatcher.
	Pause()
	// Undoes a previous pause operation, restarting internal timers and the
	// regular request flow.
	//
	// If there was a pending request before pausing the dispatcher, a response/timeout
	// for this request shall be awaited anew.
	Resume()
}

// pendingRequest is used internally for associating metadata to a pending Request.
type pendingRequest struct {
	request ocpp.Request
}

// DefaultClientDispatcher is a default implementation of the ClientDispatcher interface.
//
// The dispatcher implements the ClientState as well for simplicity.
// Access to pending requests is thread-safe.
type DefaultClientDispatcher struct {
	requestQueue        RequestQueue
	requestChannel      chan bool
	readyForDispatch    chan bool
	doneC               chan struct{}
	running             bool
	pendingRequestState ClientState
	network             ws.Client
	mutex               sync.RWMutex
	onRequestCancel     func(requestID string, request ocpp.Request, err *ocpp.Error)
	onHandlerPanic      func(HandlerPanic)
	timer               *time.Timer
	// paused is accessed atomically (0 = running, 1 = paused) so the message
	// pump can check it every loop iteration without acquiring d.mutex. Reading
	// it under d.mutex would let a pending Stop() (write Lock) starve the pump's
	// RLock, deadlocking against SendRequest calls that hold RLock while blocked
	// on the buffered channel send.
	paused         int32
	timeout        time.Duration
	timeoutOnPause bool
}

const (
	defaultTimeoutTick    = 24 * time.Hour
	defaultMessageTimeout = 30 * time.Second
)

// NewDefaultClientDispatcher creates a new DefaultClientDispatcher struct.
func NewDefaultClientDispatcher(queue RequestQueue) *DefaultClientDispatcher {
	return &DefaultClientDispatcher{
		requestQueue:        queue,
		requestChannel:      nil,
		readyForDispatch:    make(chan bool, 1),
		pendingRequestState: NewClientState(),
		timeout:             defaultMessageTimeout,
	}
}

func (d *DefaultClientDispatcher) SetOnRequestCanceled(cb func(requestID string, request ocpp.Request, err *ocpp.Error)) {
	d.onRequestCancel = cb
}

// markLocalTransportError is the fail-safe backstop for the fireRequestCancel choke-point: it
// stamps the local-transport marker onto an otherwise-unmarked cancel error, so any (current or
// future) dispatcher cancel path classifies as local rather than masquerading as a server
// CALLERROR. It MUTATES err in place; callers must pass a uniquely-owned, freshly-constructed
// error (never a shared/package-level sentinel). It is a no-op on an already-marked error.
func markLocalTransportError(err *ocpp.Error) *ocpp.Error {
	if err != nil && err.Marker == "" {
		err.Marker = localTransportMarker
	}
	return err
}

func (d *DefaultClientDispatcher) recoverCancelCallback(action, requestID string) {
	if v := recover(); v != nil {
		reportHandlerPanic(v, CancelHandlerKind, "", action, requestID, d.onHandlerPanic, nil)
	}
}

func (d *DefaultClientDispatcher) fireRequestCancel(action, requestID string, request ocpp.Request, err *ocpp.Error) {
	if d.onRequestCancel == nil {
		return
	}
	func() {
		defer d.recoverCancelCallback(action, requestID)
		d.onRequestCancel(requestID, request, markLocalTransportError(err))
	}()
}

func (d *DefaultClientDispatcher) SetTimeout(timeout time.Duration) {
	d.timeout = timeout
}

// SetTimeoutOnPause enables or disables timeout behavior while the dispatcher is paused.
// It is opt-in and defaults to false. When enabled, a request that is pending
// when the dispatcher is paused still times out after the real SetTimeout value
// instead of being parked at the internal 24h tick. This method is deliberately
// not part of the ClientDispatcher interface to avoid a breaking interface change.
// This function must be called before starting the dispatcher, otherwise it may lead to unexpected behavior.
func (d *DefaultClientDispatcher) SetTimeoutOnPause(enabled bool) {
	d.timeoutOnPause = enabled
}

func (d *DefaultClientDispatcher) Start() {
	d.mutex.Lock()
	defer d.mutex.Unlock()
	d.requestChannel = make(chan bool, 1)
	d.doneC = make(chan struct{})
	d.running = true
	d.timer = time.NewTimer(defaultTimeoutTick) // Default to 24 hours tick
	go d.messagePump()
}

func (d *DefaultClientDispatcher) IsRunning() bool {
	d.mutex.RLock()
	defer d.mutex.RUnlock()
	return d.requestChannel != nil
}

func (d *DefaultClientDispatcher) IsPaused() bool {
	return atomic.LoadInt32(&d.paused) != 0
}

// Stop signals the dispatcher to stop and blocks until its messagePump goroutine
// has exited, so no dispatcher goroutine outlives the call. It is safe to call
// more than once and before Start. It must not be called from within an
// onRequestCancel callback (which runs on the messagePump goroutine), as that
// would wait for the pump to exit from the pump itself.
func (d *DefaultClientDispatcher) Stop() {
	d.mutex.Lock()
	// Guard on `running` (set synchronously in Start/Stop) rather than on
	// requestChannel, which the pump only nils on exit — otherwise two concurrent
	// Stop() calls could both pass the check and double-close the channel.
	if !d.running {
		d.mutex.Unlock()
		return
	}
	d.running = false
	close(d.requestChannel)
	done := d.doneC
	d.mutex.Unlock()
	// Wait for messagePump to actually exit so no goroutine outlives Stop().
	<-done
}

func (d *DefaultClientDispatcher) SetNetworkClient(client ws.Client) {
	d.network = client
}

func (d *DefaultClientDispatcher) SetPendingRequestState(state ClientState) {
	d.pendingRequestState = state
}

func (d *DefaultClientDispatcher) SendRequest(req RequestBundle) error {
	if d.network == nil {
		return fmt.Errorf("cannot SendRequest, no network client was set")
	}
	if err := d.requestQueue.Push(req); err != nil {
		return err
	}
	d.mutex.RLock()
	defer d.mutex.RUnlock()
	if !d.running {
		return fmt.Errorf("cannot send request %v, dispatcher not running", req.Call.UniqueId)
	}
	d.requestChannel <- true
	return nil
}

func (d *DefaultClientDispatcher) messagePump() {
	defer close(d.doneC)
	rdy := true // Ready to transmit at the beginning

	// Capture the request channel once, WITHOUT taking d.mutex. It is assigned
	// in Start before this goroutine is launched (the `go` statement establishes
	// a happens-before edge, so the read is data-race-free) and is only ever
	// reassigned to nil by this goroutine itself on exit. The pump must not take
	// d.mutex anywhere in its loop: doing so (even once) would deadlock against a
	// concurrent Stop(). Stop holds the write Lock while a SendRequest, having
	// passed the running check under RLock, blocks on the buffered channel send;
	// once Stop's Lock is pending Go blocks all new RLock acquisitions (writer
	// preference), so a pump that needed RLock could never drain that send.
	reqCh := d.requestChannel

	for {
		select {
		case _, ok := <-reqCh:
			// New request was posted
			if !ok {
				var outstanding []interface{}
				if drainer, dok := d.requestQueue.(interface{ DrainAll() []interface{} }); dok {
					// Atomic detach so a concurrent CompleteRequest can't mis-pop an
					// intermediate front (see FIFOClientQueue.DrainAll).
					outstanding = drainer.DrainAll()
				} else {
					for !d.requestQueue.IsEmpty() {
						outstanding = append(outstanding, d.requestQueue.Pop())
					}
				}
				for _, el := range outstanding {
					bundle, ok := el.(RequestBundle)
					if !ok || bundle.Call == nil {
						continue
					}
					d.fireRequestCancel(bundle.Call.Action, bundle.Call.UniqueId, bundle.Call.Payload,
						newDispatcherStoppedError(bundle.Call.UniqueId))
				}
				d.pendingRequestState.ClearPendingRequests()
				d.requestQueue.Init()
				d.mutex.Lock()
				d.requestChannel = nil
				d.mutex.Unlock()
				return
			}
		case _, ok := <-d.timer.C:
			// Timeout elapsed
			if !ok {
				continue
			}
			if d.pendingRequestState.HasPendingRequest() {
				// Current request timed out. Removing request and triggering cancel callback.
				// Guard against the queue and pending state being out of sync (nil/empty
				// queue or a non-RequestBundle element) to avoid a nil-deref on bundle.Call.
				el := d.requestQueue.Peek()
				if bundle, ok := el.(RequestBundle); ok && bundle.Call != nil {
					d.CompleteRequest(bundle.Call.UniqueId)
					d.fireRequestCancel(bundle.Call.Action, bundle.Call.UniqueId, bundle.Call.Payload,
						newRequestTimeoutError(bundle.Call.UniqueId))
				}
			}
			// No request is currently pending -> set timer to high number
			d.timer.Reset(defaultTimeoutTick)
		case rdy = <-d.readyForDispatch:
			// Ready flag set, keep going
		}

		// Check if dispatcher is paused
		if d.IsPaused() {
			// Ignore dispatch events as long as dispatcher is paused
			continue
		}

		// Only dispatch request if able to send and request queue isn't empty
		if rdy && !d.requestQueue.IsEmpty() {
			if d.dispatchNextRequest() {
				rdy = false
				// Set timer. Non-blocking drain: d.timer.C has other receivers
				// (Pause and Resume), so a fire from the previous request can be
				// stolen by a concurrent Pause/Resume between Stop() returning
				// false and the receive here — a blocking <-d.timer.C would then
				// freeze the pump until the next fire (up to 24h), hanging Stop().
				// Matches the Pause()/Resume() drains.
				if !d.timer.Stop() {
					select {
					case <-d.timer.C:
					default:
					}
				}
				d.timer.Reset(d.timeout)
			}
		}
	}
}

func (d *DefaultClientDispatcher) dispatchNextRequest() bool {
	// Get first element in queue
	el := d.requestQueue.Peek()
	bundle, ok := el.(RequestBundle)
	if !ok || bundle.Call == nil {
		log.Errorf("failed to dispatch next request; nil Call attribute")
		return false
	}

	if bundle.Data == nil {
		log.Errorf("failed to dispatch next request; nil Data attribute")
		return false
	}

	jsonMessage := bundle.Data
	d.pendingRequestState.AddPendingRequest(bundle.Call.UniqueId, bundle.Call.Payload)
	// Attempt to send over network
	err := d.network.Write(jsonMessage)
	if err != nil {
		// TODO: handle retransmission instead of skipping request altogether
		d.CompleteRequest(bundle.Call.GetUniqueId())
		d.fireRequestCancel(bundle.Call.Action, bundle.Call.GetUniqueId(), bundle.Call.Payload,
			NewLocalTransportError(InternalError, err.Error(), bundle.Call.UniqueId))
	}
	log.Infof("dispatched request %s to server", bundle.Call.UniqueId)
	log.Debugf("sent JSON message to server: %s", string(jsonMessage))
	return true
}

func (d *DefaultClientDispatcher) Pause() {
	d.mutex.Lock()
	defer d.mutex.Unlock()
	// Non-blocking drain: the pump goroutine is also a consumer of d.timer.C, so
	// if the request-timeout timer fired and the pump already received the tick
	// (e.g. a request times out at the same instant a disconnect triggers Pause),
	// Stop() returns false but the channel is empty — a blocking <-d.timer.C would
	// then hang here while holding d.mutex, wedging the dispatcher. Same reasoning
	// as Resume().
	if !d.timer.Stop() {
		select {
		case <-d.timer.C:
		default:
		}
	}
	tick := defaultTimeoutTick
	if d.timeoutOnPause && d.timeout > 0 {
		tick = d.timeout
	}
	d.timer.Reset(tick)
	atomic.StoreInt32(&d.paused, 1)
}

func (d *DefaultClientDispatcher) Resume() {
	atomic.StoreInt32(&d.paused, 0)
	if d.pendingRequestState.HasPendingRequest() {
		// There is a pending request already. Awaiting response, before dispatching new requests.
		// Stop-and-drain before Reset: with SetTimeoutOnPause the paused timer runs at the real
		// (short) d.timeout and may have already fired while paused, leaving a stale tick buffered
		// in d.timer.C. A bare Reset would not clear it, so the pump would read the stale fire and
		// cancel this request instead of granting a fresh window. Non-blocking drain because the
		// pump is also a consumer of d.timer.C (a blocking receive could hang if the pump won the read).
		// Per the Resume contract ("a response/timeout shall be awaited anew"), an opt-in pause
		// timeout that elapsed but was not yet delivered is intentionally superseded by this fresh
		// window; whether the pump delivers that pending timeout first or Resume supersedes it is a
		// benign ordering race (both outcomes honor the contract).
		if !d.timer.Stop() {
			select {
			case <-d.timer.C:
			default:
			}
		}
		d.timer.Reset(d.timeout)
	} else {
		// Can dispatch a new request. Notifying message pump.
		d.readyForDispatch <- true
	}
}

func (d *DefaultClientDispatcher) CompleteRequest(requestId string) {
	el := d.requestQueue.Peek()
	if el == nil {
		log.Errorf("attempting to pop front of queue, but queue is empty")
		return
	}
	bundle, _ := el.(RequestBundle)
	if bundle.Call.UniqueId != requestId {
		log.Errorf("internal state mismatch: received response for %v but expected response for %v", requestId, bundle.Call.UniqueId)
		return
	}
	d.requestQueue.Pop()
	d.pendingRequestState.DeletePendingRequest(requestId)
	log.Debugf("removed request %v from front of queue", bundle.Call.UniqueId)
	// Signal that next message in queue may be sent
	d.readyForDispatch <- true
}

// ServerDispatcher contains the state and logic for handling outgoing messages on a server endpoint.
// This allows the ocpp-j layer to delegate queueing and processing logic to an external entity.
//
// The dispatcher writes outgoing messages directly to the networking layer, using a previously set websocket server.
//
// A ClientState needs to be passed to the dispatcher, before starting it.
// The dispatcher is in charge of managing all pending requests to clients, while handling the request flow.
type ServerDispatcher interface {
	// Starts the dispatcher. Depending on the implementation, this may
	// start a dedicated goroutine or simply allocate the necessary state.
	Start()
	// Returns true, if the dispatcher is currently running, false otherwise.
	// If the dispatcher is paused, the function still returns true.
	IsRunning() bool
	// Sets the maximum timeout to be considered after sending a request.
	// If a response to the request is not received within the specified period, the request
	// is discarded and an error is returned to the caller.
	//
	// One timeout per client runs in the background.
	// The timeout is reset whenever a response comes in, the connection is closed, or the server is stopped.
	//
	// This function must be called before starting the dispatcher, otherwise it may lead to unexpected behavior.
	SetTimeout(timeout time.Duration)
	// Dispatches a request for a specific client. Depending on the implementation, this may first queue
	// a request and process it later (asynchronously), or write it directly to the networking layer.
	//
	// If no network server was set, or the request couldn't be processed, an error is returned.
	SendRequest(clientID string, req RequestBundle) error
	// Notifies the dispatcher that a request has been completed (i.e. a response was received),
	// for a specific client.
	// The dispatcher takes care of removing the request marked by the requestID from
	// that client's pending requests. It will then attempt to process the next queued request.
	CompleteRequest(clientID string, requestID string)
	// Sets a callback to be invoked when a request gets canceled, due to network timeouts.
	// The callback passes the original client ID, message ID, and request struct of the failed request,
	// along with an error.
	//
	// Calling Stop on the dispatcher will not trigger this callback.
	//
	// If no callback is set, a request will still be removed from the dispatcher when a timeout occurs.
	SetOnRequestCanceled(cb CanceledRequestHandler)
	// Sets the network server, so the dispatcher may send requests using the networking layer directly.
	//
	// This needs to be set before calling the Start method. If not, sending requests will fail.
	SetNetworkServer(server ws.Server)
	// Sets the state manager for pending requests in the dispatcher.
	//
	// The state should only be accessed by the dispatcher while running.
	SetPendingRequestState(stateHandler ServerState)
	// Stops a running dispatcher. This will clear all state and empty the internal queues.
	//
	// If an onRequestCanceled callback is set, it won't be triggered by stopping the dispatcher.
	Stop()
	// Notifies that it is now possible to dispatch requests for a new client.
	//
	// Internal queues are created and requests for the client are now accepted.
	CreateClient(clientID string)
	// Notifies that a client was invalidated (typically caused by a network event).
	//
	// The dispatcher will stop dispatching requests for that specific client.
	// Internal queues for that client are cleared and no further requests will be accepted.
	// Undelivered pending requests are also cleared.
	// The OnRequestCanceled callback will be invoked for each discarded request.
	DeleteClient(clientID string)
}

// DefaultServerDispatcher is a default implementation of the ServerDispatcher interface.
//
// The dispatcher implements the ClientState as well for simplicity.
// Access to pending requests is thread-safe.
type DefaultServerDispatcher struct {
	queueMap            ServerQueueMap
	requestChannel      chan string
	readyForDispatch    chan string
	pendingRequestState ServerState
	timeout             time.Duration
	timerC              chan string
	running             bool
	stoppedC            chan struct{}
	doneC               chan struct{}
	onRequestCancel     CanceledRequestHandler
	onHandlerPanic      func(HandlerPanic)
	network             ws.Server
	mutex               sync.RWMutex
}

// Handler function to be invoked when a request gets canceled (either due to timeout or to other external factors).
type CanceledRequestHandler func(clientID string, requestID string, request ocpp.Request, err *ocpp.Error)

// Utility struct for passing a client context around and cancel pending requests.
type clientTimeoutContext struct {
	ctx    context.Context
	cancel func()
}

func (c clientTimeoutContext) isActive() bool {
	return c.cancel != nil
}

// NewDefaultServerDispatcher creates a new DefaultServerDispatcher struct.
func NewDefaultServerDispatcher(queueMap ServerQueueMap) *DefaultServerDispatcher {
	d := &DefaultServerDispatcher{
		queueMap:         queueMap,
		requestChannel:   nil,
		readyForDispatch: make(chan string, 1),
		timeout:          defaultMessageTimeout,
	}
	d.pendingRequestState = NewServerState(&sync.RWMutex{})
	return d
}

func (d *DefaultServerDispatcher) Start() {
	d.mutex.Lock()
	defer d.mutex.Unlock()
	d.requestChannel = make(chan string, 20)
	d.timerC = make(chan string, 10)
	d.stoppedC = make(chan struct{}, 1)
	d.doneC = make(chan struct{})
	d.running = true
	go d.messagePump()
}

func (d *DefaultServerDispatcher) IsRunning() bool {
	d.mutex.RLock()
	defer d.mutex.RUnlock()
	return d.running
}

// Stop signals the dispatcher to stop and blocks until its messagePump goroutine
// has exited, so no dispatcher goroutine outlives the call. It is safe to call
// more than once and before Start. It must not be called from within an
// onRequestCancel callback (which runs on the messagePump goroutine), as that
// would wait for the pump to exit from the pump itself.
func (d *DefaultServerDispatcher) Stop() {
	d.mutex.Lock()
	if !d.running {
		d.mutex.Unlock()
		return
	}
	d.running = false
	close(d.stoppedC)
	done := d.doneC
	d.mutex.Unlock()
	// Wait for messagePump to actually exit so no goroutine outlives Stop().
	<-done
}

func (d *DefaultServerDispatcher) SetTimeout(timeout time.Duration) {
	d.timeout = timeout
}

func (d *DefaultServerDispatcher) CreateClient(clientID string) {
	if d.IsRunning() {
		_ = d.queueMap.GetOrCreate(clientID)
	}
}

func (d *DefaultServerDispatcher) DeleteClient(clientID string) {
	d.queueMap.Remove(clientID)
	d.mutex.RLock()
	if d.running {
		d.requestChannel <- clientID
	}
	d.mutex.RUnlock()
}

func (d *DefaultServerDispatcher) SetNetworkServer(server ws.Server) {
	d.network = server
}

func (d *DefaultServerDispatcher) SetOnRequestCanceled(cb CanceledRequestHandler) {
	d.onRequestCancel = cb
}

func (d *DefaultServerDispatcher) recoverCancelCallback(clientID, action, requestID string) {
	if v := recover(); v != nil {
		reportHandlerPanic(v, CancelHandlerKind, clientID, action, requestID, d.onHandlerPanic, nil)
	}
}

func (d *DefaultServerDispatcher) fireRequestCancel(clientID, action, requestID string, request ocpp.Request, err *ocpp.Error) {
	if d.onRequestCancel == nil {
		return
	}
	func() {
		defer d.recoverCancelCallback(clientID, action, requestID)
		d.onRequestCancel(clientID, requestID, request, markLocalTransportError(err))
	}()
}

func (d *DefaultServerDispatcher) SetPendingRequestState(state ServerState) {
	d.pendingRequestState = state
}

func (d *DefaultServerDispatcher) SendRequest(clientID string, req RequestBundle) error {
	if d.network == nil {
		return fmt.Errorf("cannot send request %v, no network server was set", req.Call.UniqueId)
	}
	q, ok := d.queueMap.Get(clientID)
	if !ok {
		return fmt.Errorf("cannot send request %s, no client %s exists", req.Call.UniqueId, clientID)
	}
	if err := q.Push(req); err != nil {
		return err
	}
	d.mutex.RLock()
	defer d.mutex.RUnlock()
	if !d.running {
		return fmt.Errorf("cannot send request %v, dispatcher not running", req.Call.UniqueId)
	}
	d.requestChannel <- clientID
	return nil
}

// requestPump processes new outgoing requests for each client and makes sure they are processed sequentially.
// This method is executed by a dedicated coroutine as soon as the server is started and runs indefinitely.
func (d *DefaultServerDispatcher) messagePump() {
	defer close(d.doneC)
	var clientID string
	var ok bool
	var rdy bool
	var clientCtx clientTimeoutContext
	var clientQueue RequestQueue
	clientContextMap := map[string]clientTimeoutContext{} // Empty at the beginning

	// Capture the request channel once, WITHOUT taking d.mutex. It is assigned in
	// Start before this goroutine is launched (the `go` statement establishes a
	// happens-before edge, so the read is data-race-free) and is never reassigned
	// during the pump's lifetime. The pump must not take d.mutex anywhere in its
	// loop: doing so (even once) would deadlock against a concurrent Stop(). Stop
	// holds the write Lock while a SendRequest/DeleteClient, having passed the
	// running check under RLock, blocks on the buffered channel send; once Stop's
	// Lock is pending Go blocks all new RLock acquisitions (writer preference), so
	// a pump that needed RLock could never drain that send.
	reqCh := d.requestChannel

	// Dispatcher Loop
	for {
		select {
		case <-d.stoppedC:
			// server was stopped
			d.queueMap.Init()
			log.Info("stopped processing requests")
			return
		case clientID = <-reqCh:
			// Check whether there is a request queue for the specified client
			clientQueue, ok = d.queueMap.Get(clientID)
			if !ok {
				// No client queue found (client was removed)
				// Deleting and canceling the context
				clientCtx = clientContextMap[clientID]
				delete(clientContextMap, clientID)
				if clientCtx.ctx != nil {
					clientCtx.cancel()
				}
				continue
			}
			// Check whether we can transmit to client
			clientCtx, ok = clientContextMap[clientID]
			if !ok {
				// First request for this client, ready to transmit
				rdy = true
			} else {
				// If there is no active context, the client is ready to transmit
				rdy = !clientCtx.isActive()
			}
		case clientID, ok = <-d.timerC:
			// Timeout elapsed
			if !ok {
				continue
			}
			// Canceling timeout context
			log.Debugf("timeout for client %v, canceling message", clientID)
			clientCtx = clientContextMap[clientID]
			if clientCtx.isActive() {
				clientCtx.cancel()
				clientContextMap[clientID] = clientTimeoutContext{}
			}
			if d.pendingRequestState.HasPendingRequest(clientID) {
				// Current request for client timed out. Removing request and triggering cancel callback
				q, found := d.queueMap.Get(clientID)
				if !found {
					// Possible race condition: queue was already removed
					log.Errorf("dispatcher timeout for client %s triggered, but no request queue found", clientID)
					continue
				}
				el := q.Peek()
				if el == nil {
					// Should never happen
					log.Error("dispatcher timeout for client %s triggered, but no pending request found", clientID)
					continue
				}

				bundle, _ := el.(RequestBundle)
				if bundle.Call == nil {
					log.Errorf("dispatcher timeout for client %s failed; nil Call attribute", clientID)
					continue
				}

				if bundle.Data == nil {
					log.Errorf("dispatcher timeout for client for %s; nil Data attribute", clientID)
					continue
				}

				// Complete the request inline instead of calling CompleteRequest,
				// which sends to readyForDispatch. Since messagePump is the sole
				// reader of that channel, sending to it here would self-deadlock
				// if the buffer is already full from a previous iteration.
				q.Pop()
				d.pendingRequestState.DeletePendingRequest(clientID, bundle.Call.GetUniqueId())
				log.Debugf("completed request %s for %s", bundle.Call.GetUniqueId(), clientID)
				// Mark this client as ready for its next queued request
				clientQueue = q
				rdy = true
				log.Infof("request %v for %v timed out", bundle.Call.GetUniqueId(), clientID)
				d.fireRequestCancel(clientID, bundle.Call.Action, bundle.Call.GetUniqueId(), bundle.Call.Payload,
					newRequestTimeoutError(bundle.Call.GetUniqueId()))
			}
		case clientID = <-d.readyForDispatch:
			// Cancel previous timeout (if any)
			clientCtx, ok = clientContextMap[clientID]
			if ok && clientCtx.isActive() {
				clientCtx.cancel()
				clientContextMap[clientID] = clientTimeoutContext{}
			}
			// client can now transmit again
			clientQueue, ok = d.queueMap.Get(clientID)
			if ok {
				// Ready to transmit
				rdy = true
			}
			log.Debugf("%v ready to transmit again", clientID)
		}

		// Only dispatch request if able to send and request queue isn't empty
		if rdy && clientQueue != nil && !clientQueue.IsEmpty() {
			// Send request & set new context
			clientCtx = d.dispatchNextRequest(clientID)
			clientContextMap[clientID] = clientCtx
			if clientCtx.isActive() {
				go d.waitForTimeout(clientID, clientCtx)
			}
			// Update ready state
			rdy = false
		}
	}
}

func (d *DefaultServerDispatcher) dispatchNextRequest(clientID string) (clientCtx clientTimeoutContext) {
	// Get first element in queue
	q, ok := d.queueMap.Get(clientID)
	if !ok {
		log.Errorf("failed to dispatch next request for %s, no request queue available", clientID)
		return
	}
	el := q.Peek()
	bundle, _ := el.(RequestBundle)
	if bundle.Call == nil {
		log.Errorf("failed to dispatch next request for %s; nil Call attribute", clientID)
		return
	}

	if bundle.Data == nil {
		log.Errorf("failed to dispatch next request for %s; nil Data attribute", clientID)
		return
	}

	jsonMessage := bundle.Data
	callID := bundle.Call.GetUniqueId()
	d.pendingRequestState.AddPendingRequest(clientID, callID, bundle.Call.Payload)
	err := d.network.Write(clientID, jsonMessage)
	if err != nil {
		log.Errorf("error while sending message: %v", err)
		// TODO: handle retransmission instead of removing pending request
		d.CompleteRequest(clientID, callID)
		d.fireRequestCancel(clientID, bundle.Call.Action, bundle.Call.GetUniqueId(), bundle.Call.Payload,
			NewLocalTransportError(InternalError, err.Error(), bundle.Call.UniqueId))
		return
	}
	// Create and return context (only if timeout is set)
	if d.timeout > 0 {
		ctx, cancel := context.WithTimeout(context.TODO(), d.timeout)
		clientCtx = clientTimeoutContext{ctx: ctx, cancel: cancel}
	}
	log.Infof("dispatched request %s for %s", callID, clientID)
	log.Debugf("sent JSON message to %s: %s", clientID, string(jsonMessage))
	return
}

func (d *DefaultServerDispatcher) waitForTimeout(clientID string, clientCtx clientTimeoutContext) {
	defer clientCtx.cancel()
	log.Debugf("started timeout timer for %s", clientID)
	select {
	case <-clientCtx.ctx.Done():
		err := clientCtx.ctx.Err()
		if err == context.DeadlineExceeded {
			// Timeout triggered, notifying messagePump.
			// Check running state under lock, but release before the channel
			// send. Holding RLock during a potentially blocking send can cause
			// a deadlock: if timerC is full, this goroutine blocks while
			// holding RLock, preventing any Lock() caller from proceeding.
			d.mutex.RLock()
			running := d.running
			d.mutex.RUnlock()
			if running {
				d.timerC <- clientID
			}
		} else {
			log.Debugf("timeout canceled for %s", clientID)
		}
	case <-d.stoppedC:
		// server was stopped, every pending timeout gets canceled
	}
}

func (d *DefaultServerDispatcher) CompleteRequest(clientID string, requestID string) {
	q, ok := d.queueMap.Get(clientID)
	if !ok {
		log.Errorf("attempting to complete request for client %v, but no matching queue found", clientID)
		return
	}
	el := q.Peek()
	if el == nil {
		log.Errorf("attempting to pop front of queue, but queue is empty")
		return
	}
	bundle, _ := el.(RequestBundle)
	callID := bundle.Call.GetUniqueId()
	if callID != requestID {
		log.Errorf("internal state mismatch: processing response for %v but expected response for %v", requestID, callID)
		return
	}
	q.Pop()
	d.pendingRequestState.DeletePendingRequest(clientID, requestID)
	log.Debugf("completed request %s for %s", callID, clientID)
	// Signal that next message in queue may be sent
	d.readyForDispatch <- clientID
}
