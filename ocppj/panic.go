package ocppj

import "runtime/debug"

// HandlerKind identifies which user handler panicked.
type HandlerKind string

const (
	RequestHandlerKind        HandlerKind = "request"
	ResponseHandlerKind       HandlerKind = "response"
	ErrorHandlerKind          HandlerKind = "error"
	ConnectHandlerKind        HandlerKind = "connect"
	DisconnectHandlerKind     HandlerKind = "disconnect"
	CancelHandlerKind         HandlerKind = "cancel"
	ReconnectHandlerKind      HandlerKind = "reconnect"
	InvalidMessageHandlerKind HandlerKind = "invalid-message"
)

// HandlerPanic describes a panic recovered from a user-provided handler.
type HandlerPanic struct {
	ClientID  string      // remote client id; "" for the client-side endpoint
	Kind      HandlerKind // which handler panicked
	Action    string      // OCPP action when known (request/response/error); "" otherwise
	RequestID string      // message unique id when known; "" otherwise
	Value     interface{} // the value passed to panic()
	Stack     []byte      // debug.Stack() captured at recovery
}

// SetOnHandlerPanic registers a callback invoked when a user handler panics.
//
// The callback runs synchronously on whichever goroutine recovered the panic:
// the read loop for message handlers, and — for a canceled-request handler
// panic — the dispatcher's messagePump goroutine. As with an onRequestCanceled
// callback, it therefore must NOT call Stop() or block on a full SendRequest,
// or it may deadlock the pump. It should also not panic. Set it before Start.
func (c *Client) SetOnHandlerPanic(handler func(HandlerPanic)) {
	c.onHandlerPanic = handler
	if d, ok := c.dispatcher.(*DefaultClientDispatcher); ok {
		d.onHandlerPanic = handler
	}
}

// SetOnHandlerPanic registers a callback invoked when a user handler panics.
//
// The callback runs synchronously on whichever goroutine recovered the panic:
// the read loop for message handlers, and — for a canceled-request handler
// panic — the dispatcher's messagePump goroutine. As with an onRequestCanceled
// callback, it therefore must NOT call Stop() or block on a full SendRequest,
// or it may deadlock the pump. It should also not panic. Set it before Start.
func (s *Server) SetOnHandlerPanic(handler func(HandlerPanic)) {
	s.onHandlerPanic = handler
	if d, ok := s.dispatcher.(*DefaultServerDispatcher); ok {
		d.onHandlerPanic = handler
	}
}

// recoverHandler is deferred around a user-provided handler invocation. When the
// handler panics it recovers (so the panic never crosses the read-loop
// goroutine), logs it, notifies the panic callback (if any), and runs
// afterRecover (if any). afterRecover is used at the request sites to send a
// CALL ERROR back to the peer, so a caller awaiting a response is not left to
// time out. The callback and afterRecover are each guarded so neither can
// re-crash the endpoint. On the non-panic path (recover() == nil) it returns
// immediately, leaving behavior unchanged.
func recoverHandler(kind HandlerKind, clientID string, action string, requestID string, cb func(HandlerPanic), afterRecover func()) {
	value := recover()
	if value == nil {
		return
	}
	reportHandlerPanic(value, kind, clientID, action, requestID, cb, afterRecover)
}

// reportHandlerPanic logs a recovered handler panic, notifies the panic callback
// (guarded), and runs afterRecover (guarded). value must be the non-nil result
// of recover().
func reportHandlerPanic(value interface{}, kind HandlerKind, clientID, action, requestID string, cb func(HandlerPanic), afterRecover func()) {
	stack := debug.Stack()
	log.Errorf("recovered panic in %s handler: %v\n%s", kind, value, stack)

	if cb != nil {
		hp := HandlerPanic{
			ClientID:  clientID,
			Kind:      kind,
			Action:    action,
			RequestID: requestID,
			Value:     value,
			Stack:     stack,
		}
		guardedCall(func() { cb(hp) }, "handler panic callback")
	}

	if afterRecover != nil {
		guardedCall(afterRecover, "handler panic response")
	}
}

// RecoverPanicGoroutine is deferred at the top of a goroutine that runs a
// user-provided handler or callback OUTSIDE the read loop (as the ocpp1.6 facade
// does). It recovers a panic there — which the read-loop recover cannot reach —
// logs it, notifies the OnHandlerPanic callback, and, when sendCallError is true
// (i.e. an inbound request awaiting a response), sends a CALL ERROR(InternalError)
// to the peer. Usage: `defer server.RecoverPanicGoroutine(kind, clientID, action, requestID, true)`.
func (s *Server) RecoverPanicGoroutine(kind HandlerKind, clientID, action, requestID string, sendCallError bool) {
	value := recover()
	if value == nil {
		return
	}
	var after func()
	if sendCallError {
		after = func() {
			_ = s.SendError(clientID, requestID, InternalError, "internal error while handling request", nil)
		}
	}
	reportHandlerPanic(value, kind, clientID, action, requestID, s.onHandlerPanic, after)
}

// RecoverPanicGoroutine is the client-side analogue (the client SendError takes
// no clientID). Recover is per-goroutine; ocpp1.6 facade callbacks and handlers
// run on a dedicated goroutine and use this guard for response, error, and
// inbound request dispatch.
func (c *Client) RecoverPanicGoroutine(kind HandlerKind, action, requestID string, sendCallError bool) {
	value := recover()
	if value == nil {
		return
	}
	var after func()
	if sendCallError {
		after = func() {
			_ = c.SendError(requestID, InternalError, "internal error while handling request", nil)
		}
	}
	reportHandlerPanic(value, kind, "", action, requestID, c.onHandlerPanic, after)
}

// guardedCall runs fn, recovering and logging any panic so it cannot escape into
// the caller's (read-loop) goroutine.
func guardedCall(fn func(), what string) {
	defer func() {
		if v := recover(); v != nil {
			log.Errorf("recovered panic in %s: %v\n%s", what, v, debug.Stack())
		}
	}()
	fn()
}
