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
	//
	// Returns true if this call atomically popped the matching request from the queue
	// (i.e. it "owns" the completion), false if the request was already completed
	// or the front element does not match the given ID.
	CompleteRequest(requestID string) bool
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

// pumpPending is the pump-local token for an in-flight dispatched request.
// The messagePump arms a ctx.Done() select arm from this token so an
// in-flight request can be canceled before a response arrives.
type pumpPending struct {
	id      string
	action  string
	ctx     context.Context
	payload ocpp.Request
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

	var pending pumpPending // pump-local in-flight token; zero when none in flight

	for {
		// Reconcile the local in-flight token against authoritative state.
		// The coalesced readyForDispatch carries no id, so a response that
		// completed the request off-pump is detected here.
		if pending.id != "" {
			if _, ok := d.pendingRequestState.GetPendingRequest(pending.id); !ok {
				pending = pumpPending{}
			}
		}

		// Arm the ctx-cancel select arm only when there is an in-flight request
		// with its own context. context.Background().Done() is nil, so the arm
		// remains inert (blocks forever) for a ctx-less send.
		var pendingDone <-chan struct{}
		if pending.id != "" && pending.ctx != nil {
			pendingDone = pending.ctx.Done()
		}

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
				// Clear pending state before firing the cancel callbacks below, so a
				// late inbound response arriving mid-drain cannot pass ParseMessage's
				// pending-check and reach the (now drained, PopIf-losing) completion
				// path after Stop has already taken ownership of the request.
				d.pendingRequestState.ClearPendingRequests()
				for _, el := range outstanding {
					bundle, ok := el.(RequestBundle)
					if !ok || bundle.Call == nil {
						continue
					}
					d.fireRequestCancel(bundle.Call.Action, bundle.Call.UniqueId, bundle.Call.Payload,
						newDispatcherStoppedError(bundle.Call.UniqueId))
				}
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
					if d.CompleteRequest(bundle.Call.UniqueId) {
						d.fireRequestCancel(bundle.Call.Action, bundle.Call.UniqueId, bundle.Call.Payload,
							newRequestTimeoutError(bundle.Call.UniqueId))
					}
				}
			}
			// No request is currently pending -> set timer to high number
			d.timer.Reset(defaultTimeoutTick)
			pending = pumpPending{}
		case rdy = <-d.readyForDispatch:
			// Ready flag set, keep going
		case <-pendingDone:
			// This in-flight request's ctx fired. Cancel iff still pending+front
			// (identity + atomic via CompleteRequest).
			if _, ok := d.pendingRequestState.GetPendingRequest(pending.id); ok {
				if d.CompleteRequest(pending.id) {
					d.fireRequestCancel(pending.action, pending.id, pending.payload,
						newRequestCanceledError(pending.id, pending.ctx.Err()))
				}
			}
			pending = pumpPending{}
		}

		// Check if dispatcher is paused
		if d.IsPaused() {
			// Ignore dispatch events as long as dispatcher is paused
			continue
		}

		// Only dispatch request if able to send, queue isn't empty, and no request
		// is currently in-flight (level-based check — re-derived every iteration).
		if rdy && !d.requestQueue.IsEmpty() && !d.pendingRequestState.HasPendingRequest() {
			if p, dispatched := d.dispatchNextRequest(); dispatched {
				pending = p
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

func (d *DefaultClientDispatcher) dispatchNextRequest() (pumpPending, bool) {
	// Get first element in queue
	el := d.requestQueue.Peek()
	bundle, ok := el.(RequestBundle)
	if !ok || bundle.Call == nil {
		log.Errorf("failed to dispatch next request; nil Call attribute")
		return pumpPending{}, false
	}

	if bundle.Data == nil {
		log.Errorf("failed to dispatch next request; nil Data attribute")
		return pumpPending{}, false
	}

	ctx := bundleCtx(bundle)
	// Pre-write drop: if the ctx already fired (e.g. it expired while
	// queued during a disconnect), drop it and return so the coalesced
	// readiness re-enters dispatch for the next front on the following
	// pump iteration. One front per call — no inner loop.
	if ctx.Err() != nil {
		if d.CompleteRequest(bundle.Call.UniqueId) {
			d.fireRequestCancel(bundle.Call.Action, bundle.Call.UniqueId, bundle.Call.Payload,
				newRequestCanceledError(bundle.Call.UniqueId, ctx.Err()))
		}
		return pumpPending{}, false
	}

	jsonMessage := bundle.Data
	d.pendingRequestState.AddPendingRequest(bundle.Call.UniqueId, bundle.Call.Payload)
	// Attempt to send over network
	err := d.network.Write(jsonMessage)
	if err != nil {
		// TODO: handle retransmission instead of skipping request altogether
		if d.CompleteRequest(bundle.Call.GetUniqueId()) {
			d.fireRequestCancel(bundle.Call.Action, bundle.Call.GetUniqueId(), bundle.Call.Payload,
				NewLocalTransportError(InternalError, err.Error(), bundle.Call.UniqueId))
		}
		// Write error: canceled, nothing live to track.
		return pumpPending{}, true
	}
	log.Infof("dispatched request %s to server", bundle.Call.UniqueId)
	log.Debugf("sent JSON message to server: %s", string(jsonMessage))
	return pumpPending{id: bundle.Call.UniqueId, action: bundle.Call.Action, ctx: ctx, payload: bundle.Call.Payload}, true
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
		select {
		case d.readyForDispatch <- true:
		default:
		}
	}
}

func (d *DefaultClientDispatcher) CompleteRequest(requestId string) bool {
	el, ok := d.requestQueue.PopIf(func(el interface{}) bool {
		bundle, ok := el.(RequestBundle)
		return ok && bundle.Call != nil && bundle.Call.UniqueId == requestId
	})
	if ok {
		bundle := el.(RequestBundle)
		d.pendingRequestState.DeletePendingRequest(bundle.Call.UniqueId)
		log.Debugf("removed request %v from front of queue", bundle.Call.UniqueId)
		// Signal that next message in queue may be sent (non-blocking, coalesced).
		select {
		case d.readyForDispatch <- true:
		default:
		}
		return true
	}
	// PopIf did not pop: the queue is empty or the front element did not match
	// the requested ID. This call did not win completion ownership, so it must
	// report false — the caller must NOT deliver a handler or fire a cancel
	// (single-winner ownership / callback-steal prevention).
	return false
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
	//
	// Returns true if this call atomically popped the matching request from that
	// client's queue (i.e. it "owns" the completion), false if the request was
	// already completed or the front element does not match the given ID.
	CompleteRequest(clientID string, requestID string) bool
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
	requestChannel      chan serverDispatchRequest
	readyForDispatch    chan serverReadyToken
	pendingRequestState ServerState
	timeout             time.Duration
	timerC              chan serverTimeoutToken
	// cancelC carries serverCancelToken notifications from a per-request
	// watcher (waitForTimeout) when the CALLER's context (as opposed to the
	// internal timeout-tracking clientCtx watched via timerC) fires. Buffered
	// cap 10, matching timerC (B4).
	cancelC         chan serverCancelToken
	running         bool
	stoppedC        chan struct{}
	doneC           chan struct{}
	onRequestCancel CanceledRequestHandler
	onHandlerPanic  func(HandlerPanic)
	network         ws.Server
	mutex           sync.RWMutex
}

type serverDispatchRequest struct {
	clientID  string
	deleteAck chan struct{}
}

type serverReadyToken struct {
	clientID  string
	requestID string
}

type serverTimeoutToken struct {
	clientID string
	ctx      context.Context
}

// serverCancelToken is posted by a per-request watcher (waitForTimeout) onto
// cancelC when the CALLER's context (SendRequestCtx's ctx, distinct from the
// internal timeout-tracking clientCtx) fires. requestID lets the pump's
// identity guard (dispatchedRequestIDMap[clientID] == requestID) detect a
// stale token for a request that has already completed by some other path.
type serverCancelToken struct {
	clientID  string
	requestID string
	ctx       context.Context
}

// dispatchStatus reports the outcome of a single dispatchNextRequest call, so
// the pump can decide whether it needs to loop the dispatch step for the
// same client (see dispatchCompletedNoWrite below / spec MAJOR-2).
type dispatchStatus int

const (
	// dispatchIdle: nothing was dispatched and nothing was completed (no
	// queue for clientID, or a malformed/missing queue front). The pump must
	// not retry — retrying would spin forever on the same broken entry.
	dispatchIdle dispatchStatus = iota
	// dispatchWritten: the request was written to the network and is now
	// in-flight (pending state set, and a timeout watcher spawned if active).
	dispatchWritten
	// dispatchCompletedNoWrite: the request was popped/completed WITHOUT ever
	// being written (Write returned an error). There is no readyForDispatch
	// self-send driving re-entry to this client's next queued request (the
	// A1 fix forbids that blocking self-send from the pump goroutine), so the
	// pump must loop the dispatch step itself while the client's queue
	// remains non-empty. Bounded by queue length: each dispatchCompletedNoWrite
	// pops exactly one entry.
	dispatchCompletedNoWrite
)

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
		readyForDispatch: make(chan serverReadyToken, 1),
		timeout:          defaultMessageTimeout,
	}
	d.pendingRequestState = NewServerState(&sync.RWMutex{})
	return d
}

func (d *DefaultServerDispatcher) Start() {
	d.mutex.Lock()
	defer d.mutex.Unlock()
	d.requestChannel = make(chan serverDispatchRequest, 20)
	d.timerC = make(chan serverTimeoutToken, 10)
	d.cancelC = make(chan serverCancelToken, 10)
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
		d.requestChannel <- serverDispatchRequest{clientID: clientID}
	}
	d.mutex.RUnlock()
}

func (d *DefaultServerDispatcher) DeleteClientAndWait(clientID string) {
	d.queueMap.Remove(clientID)
	ack := make(chan struct{})
	d.mutex.RLock()
	if !d.running {
		d.mutex.RUnlock()
		return
	}
	requestChannel := d.requestChannel
	doneC := d.doneC
	requestChannel <- serverDispatchRequest{clientID: clientID, deleteAck: ack}
	d.mutex.RUnlock()

	select {
	case <-ack:
	case <-doneC:
	case <-time.After(2 * time.Second):
	}
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
	d.requestChannel <- serverDispatchRequest{clientID: clientID}
	return nil
}

// requestPump processes new outgoing requests for each client and makes sure they are processed sequentially.
// This method is executed by a dedicated coroutine as soon as the server is started and runs indefinitely.
func (d *DefaultServerDispatcher) messagePump() {
	defer close(d.doneC)
	var clientID string
	var ok bool
	var rdy bool
	var req serverDispatchRequest
	var readyTok serverReadyToken
	var timeoutTok serverTimeoutToken
	var clientCtx clientTimeoutContext
	var clientQueue RequestQueue
	clientContextMap := map[string]clientTimeoutContext{} // Empty at the beginning
	dispatchedRequestIDMap := map[string]string{}

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
		case req = <-reqCh:
			clientID = req.clientID
			// Check whether there is a request queue for the specified client
			clientQueue, ok = d.queueMap.Get(clientID)
			if !ok {
				// No client queue found (client was removed)
				// Deleting and canceling the context
				clientCtx = clientContextMap[clientID]
				delete(clientContextMap, clientID)
				delete(dispatchedRequestIDMap, clientID)
				if clientCtx.ctx != nil {
					clientCtx.cancel()
				}
				if req.deleteAck != nil {
					close(req.deleteAck)
				}
				continue
			}
			if req.deleteAck != nil {
				clientCtx = clientContextMap[clientID]
				delete(clientContextMap, clientID)
				delete(dispatchedRequestIDMap, clientID)
				if clientCtx.ctx != nil {
					clientCtx.cancel()
				}
				close(req.deleteAck)
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
		case timeoutTok, ok = <-d.timerC:
			// Timeout elapsed
			if !ok {
				continue
			}
			clientID = timeoutTok.clientID
			clientCtx = clientContextMap[clientID]
			if clientCtx.ctx != timeoutTok.ctx {
				continue
			}
			timedOutRequestID := dispatchedRequestIDMap[clientID]
			// Canceling timeout context
			log.Debugf("timeout for client %v, canceling message", clientID)
			if clientCtx.isActive() {
				clientCtx.cancel()
			}
			clientContextMap[clientID] = clientTimeoutContext{}
			delete(dispatchedRequestIDMap, clientID)
			if d.pendingRequestState.HasPendingRequest(clientID) {
				// Current request for client timed out. Complete it via the
				// shared atomic primitive (on-pump, non-signaling: it does
				// NOT send to readyForDispatch, since messagePump is the sole
				// reader of that channel and sending to it here could
				// self-deadlock if the buffer is already full — A1 fix). The
				// atomic PopIf underneath means only one of {timeout,
				// response, write-error} can ever win this completion — A2
				// fix: a losing call here (front already popped by a racing
				// completion) is a no-op, not a double-pop.
				bundle, won := d.completeRequestOwned(clientID, timedOutRequestID)
				if !won {
					continue
				}
				log.Debugf("completed request %s for %s", bundle.Call.GetUniqueId(), clientID)
				// Mark this client as ready for its next queued request
				q, found := d.queueMap.Get(clientID)
				if found {
					clientQueue = q
				} else {
					clientQueue = nil
				}
				rdy = true
				log.Infof("request %v for %v timed out", bundle.Call.GetUniqueId(), clientID)
				d.fireRequestCancel(clientID, bundle.Call.Action, bundle.Call.GetUniqueId(), bundle.Call.Payload,
					newRequestTimeoutError(bundle.Call.GetUniqueId()))
			} else {
				q, found := d.queueMap.Get(clientID)
				if found {
					clientQueue = q
					rdy = true
				}
			}
		case readyTok = <-d.readyForDispatch:
			clientID = readyTok.clientID
			dispatchedRequestID := dispatchedRequestIDMap[clientID]
			if dispatchedRequestID != "" && dispatchedRequestID != readyTok.requestID {
				continue
			}
			// Cancel previous timeout (if any)
			clientCtx, ok = clientContextMap[clientID]
			if ok && clientCtx.isActive() {
				clientCtx.cancel()
				clientContextMap[clientID] = clientTimeoutContext{}
			}
			delete(dispatchedRequestIDMap, clientID)
			// client can now transmit again
			clientQueue, ok = d.queueMap.Get(clientID)
			if ok {
				// Ready to transmit
				rdy = true
			}
			log.Debugf("%v ready to transmit again", clientID)
		case cancelTok := <-d.cancelC:
			// A dispatched request's CALLER context (as opposed to the
			// internal timeout-tracking clientCtx handled by the timerC arm
			// above) fired. Structurally identical to the timeout arm:
			// identity guard, then completeRequestOwned (on-pump,
			// non-signaling — the A1/A4 invariant), then only the WINNER
			// advances client state and fires the cancel notification (§C2).
			clientID = cancelTok.clientID
			if dispatchedRequestIDMap[clientID] != cancelTok.requestID {
				// Stale token: the request already completed by some other
				// path (response, timeout, disconnect) before this cancel
				// was delivered. No-op. Nulling the loop-persistent rdy/
				// clientQueue here is defensive hygiene, NOT load-bearing: a
				// continue targets the enclosing for and skips this iteration's
				// dispatch guard entirely, and every arm that later falls
				// through to the guard reassigns rdy first — so no stale value
				// can reach it. Kept for local clarity in a dense pump.
				rdy = false
				clientQueue = nil
				continue
			}
			bundle, won := d.completeRequestOwned(clientID, cancelTok.requestID)
			if !won {
				// Lost the atomic completion race (e.g. a genuine
				// CALL_RESULT popped the front first). The winner's own path
				// already advances client state; nothing to do here. The rdy/
				// clientQueue nulling is defensive hygiene for the same reason
				// as the stale-token path above (continue skips the guard).
				rdy = false
				clientQueue = nil
				continue
			}
			clientCtx = clientContextMap[clientID]
			if clientCtx.isActive() {
				clientCtx.cancel()
			}
			clientContextMap[clientID] = clientTimeoutContext{}
			delete(dispatchedRequestIDMap, clientID)
			clientQueue, ok = d.queueMap.Get(clientID)
			if !ok {
				clientQueue = nil
			}
			rdy = true
			log.Infof("request %v for %v canceled by caller context", bundle.Call.GetUniqueId(), clientID)
			d.fireRequestCancel(clientID, bundle.Call.Action, bundle.Call.GetUniqueId(), bundle.Call.Payload,
				newRequestCanceledError(bundle.Call.GetUniqueId(), cancelTok.ctx.Err()))
		}

		// Only dispatch request if able to send, queue isn't empty, and no
		// request is currently pending for this client. The HasPendingRequest
		// check is the A3 fix (mirrors the client pump's identical guard):
		// without it, SetTimeout(0) never activates clientCtx, so `rdy` is
		// permanently true and a second SendRequest for the same client would
		// re-write the still in-flight front to the wire, since
		// dispatchNextRequest only Peeks.
		if rdy && clientQueue != nil && !clientQueue.IsEmpty() && !d.pendingRequestState.HasPendingRequest(clientID) {
			// Send request & set new context. Looped (bounded by queue
			// length) so a write-error completion — which pops/completes a
			// request WITHOUT writing and therefore without any
			// readyForDispatch self-send to drive re-entry — still reaches
			// this client's next queued request with no external stimulus
			// (spec MAJOR-2).
			for {
				var requestID string
				var status dispatchStatus
				var userCtx context.Context
				clientCtx, requestID, userCtx, status = d.dispatchNextRequest(clientID)
				clientContextMap[clientID] = clientCtx
				if requestID != "" && d.pendingRequestState.HasPendingRequest(clientID) {
					dispatchedRequestIDMap[clientID] = requestID
				} else {
					delete(dispatchedRequestIDMap, clientID)
				}
				if clientCtx.isActive() {
					go d.waitForTimeout(clientID, requestID, clientCtx, userCtx, d.stoppedC, d.timerC, d.cancelC)
				}
				// Update ready state
				rdy = false
				if status != dispatchCompletedNoWrite {
					break
				}
				q, found := d.queueMap.Get(clientID)
				if !found || q.IsEmpty() {
					break
				}
			}
		}
	}
}

func (d *DefaultServerDispatcher) dispatchNextRequest(clientID string) (clientCtx clientTimeoutContext, requestID string, userCtx context.Context, status dispatchStatus) {
	// Get first element in queue
	q, ok := d.queueMap.Get(clientID)
	if !ok {
		log.Errorf("failed to dispatch next request for %s, no request queue available", clientID)
		return clientCtx, "", nil, dispatchIdle
	}
	el := q.Peek()
	bundle, _ := el.(RequestBundle)
	if bundle.Call == nil {
		log.Errorf("failed to dispatch next request for %s; nil Call attribute", clientID)
		return clientCtx, "", nil, dispatchIdle
	}

	if bundle.Data == nil {
		log.Errorf("failed to dispatch next request for %s; nil Data attribute", clientID)
		return clientCtx, "", nil, dispatchIdle
	}

	userCtx = bundleCtx(bundle)
	// C3 pre-write drop: a request whose caller ctx already fired (e.g. it
	// was canceled/expired while still queued behind an in-flight front) is
	// never written to the wire. Complete it via the same atomic, non-
	// signaling primitive the write-error path below uses, and report
	// dispatchCompletedNoWrite so messagePump's bounded drain loop advances
	// to the next queued front on this same re-entry (spec MAJOR-2 loop,
	// reused here for C3 per the spec).
	if userCtx.Err() != nil {
		callID := bundle.Call.GetUniqueId()
		if _, won := d.completeRequestOwned(clientID, callID); won {
			d.fireRequestCancel(clientID, bundle.Call.Action, callID, bundle.Call.Payload,
				newRequestCanceledError(callID, userCtx.Err()))
		}
		return clientTimeoutContext{}, "", nil, dispatchCompletedNoWrite
	}

	jsonMessage := bundle.Data
	callID := bundle.Call.GetUniqueId()
	d.pendingRequestState.AddPendingRequest(clientID, callID, bundle.Call.Payload)
	err := d.network.Write(clientID, jsonMessage)
	if err != nil {
		log.Errorf("error while sending message: %v", err)
		// TODO: handle retransmission instead of removing pending request
		// On-pump completion: use the non-signaling primitive directly (A1
		// fix) instead of the public, signaling CompleteRequest — calling
		// that from the pump goroutine risks a self-deadlock on the cap-1
		// readyForDispatch if an off-pump completion already filled it.
		if _, won := d.completeRequestOwned(clientID, callID); won {
			d.fireRequestCancel(clientID, bundle.Call.Action, bundle.Call.GetUniqueId(), bundle.Call.Payload,
				NewLocalTransportError(InternalError, err.Error(), bundle.Call.UniqueId))
		}
		// Nothing was written; the caller (messagePump) loops the dispatch
		// step for this client while its queue remains non-empty (MAJOR-2).
		return clientTimeoutContext{}, "", nil, dispatchCompletedNoWrite
	}
	// Create the internal timeout-tracking context. Always created when
	// d.timeout > 0 (unchanged). B1: ALSO created (via WithCancel, since
	// there is no real deadline) when d.timeout == 0 but the bundle's own
	// ctx is cancelable (Done() != nil) — this is what makes every existing
	// completion path (the readyToken arm's clientCtx.cancel(), the
	// DeleteClient branch) reap the watcher spawned for this request on
	// normal completion, instead of leaking it. A ctx-less send (Background,
	// Done() == nil) with d.timeout == 0 still creates no clientCtx and
	// spawns no watcher — the common case stays free of a per-request
	// goroutine.
	if d.timeout > 0 {
		ctx, cancel := context.WithTimeout(context.TODO(), d.timeout)
		clientCtx = clientTimeoutContext{ctx: ctx, cancel: cancel}
	} else if userCtx.Done() != nil {
		ctx, cancel := context.WithCancel(context.TODO())
		clientCtx = clientTimeoutContext{ctx: ctx, cancel: cancel}
	}
	log.Infof("dispatched request %s for %s", callID, clientID)
	log.Debugf("sent JSON message to %s: %s", clientID, string(jsonMessage))
	return clientCtx, callID, userCtx, dispatchWritten
}

// waitForTimeout watches a single dispatched request's clientCtx (the
// internal timeout-tracking context, always present when this goroutine is
// spawned — see the B1 comment in dispatchNextRequest) and userCtx (the
// caller-supplied context passed to SendRequestCtx, or context.Background()
// for a ctx-less send, whose nil Done() keeps that arm permanently inert).
// On a genuine deadline expiry it notifies messagePump via timerC; on a
// caller cancellation it notifies messagePump via cancelC. clientID and
// requestID identify the watched request for the pump's identity guards
// (dispatchedRequestIDMap[clientID] == requestID), since — unlike the
// timeout arm, which is intrinsically racing exactly one dispatched request
// per client and is content with a bare ctx-identity check — the cancel
// token needs its own requestID to detect staleness.
//
// stoppedC/timerC/cancelC are passed as PARAMETERS (not read from
// d.stoppedC/d.timerC/d.cancelC) so this goroutine stays pinned to the
// generation it was spawned under (B3): watcher goroutines are never joined
// by Stop(), and Start() reassigns those fields on every generation, so
// reading them dynamically here would both race Start()'s reassignment and
// risk a stale watcher posting into a NEW generation's channel after a
// Stop->Start cycle.
func (d *DefaultServerDispatcher) waitForTimeout(clientID, requestID string, clientCtx clientTimeoutContext, userCtx context.Context, stoppedC chan struct{}, timerC chan serverTimeoutToken, cancelC chan serverCancelToken) {
	defer clientCtx.cancel()
	log.Debugf("started timeout timer for %s", clientID)
	select {
	case <-clientCtx.ctx.Done():
		err := clientCtx.ctx.Err()
		if err == context.DeadlineExceeded {
			// Timeout triggered, notifying messagePump. Shutdown-safe send
			// (B2): select against the SAME (pinned) generation's stoppedC,
			// so a stale watcher always has an escape regardless of timerC's
			// buffer state, instead of a bare guarded send that can park
			// forever once the buffer fills after a Stop->Start cycle.
			select {
			case timerC <- serverTimeoutToken{clientID: clientID, ctx: clientCtx.ctx}:
			case <-stoppedC:
			}
		} else {
			// clientCtx was explicitly canceled — either by a completion
			// path reaping this watcher on normal response/cancel-win (B1),
			// or by DeleteClient. Not a real timeout: nothing to post.
			log.Debugf("timeout canceled for %s", clientID)
		}
	case <-userCtx.Done():
		// The CALLER's context fired (SendRequestCtx's ctx.Cancel/deadline).
		// Same shutdown-safe send shape as the timeout arm (B2).
		select {
		case cancelC <- serverCancelToken{clientID: clientID, requestID: requestID, ctx: userCtx}:
		case <-stoppedC:
		}
	case <-stoppedC:
		// server was stopped, every pending timeout/cancel watch gets dropped
	}
}

// completeRequestOwned atomically pops the front of clientID's queue iff it
// is requestID (via the client queue's atomic PopIf), and clears pending
// state. Returns the popped bundle and whether THIS call won ownership. It
// does NOT signal readyForDispatch — callers choose: on-pump callers (the
// timeout arm, the write-error path) set clientQueue/rdy locally instead,
// since messagePump is the sole reader of readyForDispatch and a blocking
// self-send from the pump goroutine would risk a self-deadlock (A1); the
// public, off-pump CompleteRequest signals after a win.
//
// The atomic PopIf is also the A2 fix: only one of {timeout, response,
// write-error} can ever win a given request's completion, so the next queued
// request is never silently discarded by a non-atomic Peek-then-Pop race.
func (d *DefaultServerDispatcher) completeRequestOwned(clientID, requestID string) (RequestBundle, bool) {
	q, ok := d.queueMap.Get(clientID)
	if !ok {
		log.Errorf("attempting to complete request for client %v, but no matching queue found", clientID)
		return RequestBundle{}, false
	}
	el, popped := q.PopIf(func(el interface{}) bool {
		bundle, ok := el.(RequestBundle)
		return ok && bundle.Call != nil && bundle.Call.GetUniqueId() == requestID
	})
	if !popped {
		return RequestBundle{}, false
	}
	bundle := el.(RequestBundle)
	d.pendingRequestState.DeletePendingRequest(clientID, requestID)
	log.Debugf("completed request %s for %s", requestID, clientID)
	return bundle, true
}

// CompleteRequest notifies the dispatcher that a request has been completed
// for a specific client (i.e. a response/error was received off-pump, on a
// ws read goroutine). It atomically pops the request iff it is still the
// front of that client's queue (completeRequestOwned) and, only when THIS
// call won ownership, signals messagePump via the blocking readyForDispatch
// send. Returns whether this call won — callers (server.go's CALL_RESULT/
// CALL_ERROR handling) must guard firing any handler on the returned bool, so
// a losing/stale completion cannot deliver a duplicate notification.
func (d *DefaultServerDispatcher) CompleteRequest(clientID string, requestID string) bool {
	_, won := d.completeRequestOwned(clientID, requestID)
	if !won {
		return false
	}
	// Signal that next message in queue may be sent. Safe to block here: this
	// method is only ever called off-pump (server.go's read-goroutine
	// handlers); on-pump callers use completeRequestOwned directly and never
	// reach this send (the A1 self-deadlock guard).
	//
	// DEFERRED (pre-existing, not an E2a regression): readyForDispatch is cap-1,
	// so two off-pump completions for DIFFERENT clients contend — the second read
	// goroutine blocks until the pump drains one token. Bounded by a single pump
	// iteration, but that is not necessarily instantaneous: if the pump is mid
	// d.network.Write to a backed-up peer, the block lasts up to the ws WriteWait
	// (default ~10s) before that Write errors. The client dispatcher avoids this
	// with a non-blocking coalesced send; the server could adopt the same. Left
	// as-is because the block is bounded and the change is orthogonal to E2a.
	d.readyForDispatch <- serverReadyToken{clientID: clientID, requestID: requestID}
	return true
}
