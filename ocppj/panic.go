package ocppj

import (
	"fmt"
	"runtime/debug"
)

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

// HandlerPanicError wraps a recovered handler panic for delivery on a facade's
// Errors() channel. Match it with errors.As to recover the details:
//
//	var hpe *ocppj.HandlerPanicError
//	if errors.As(err, &hpe) { … hpe.Panic.Kind, hpe.Panic.ClientID … }
//
// The stack is deliberately NOT part of Error(); read it from Panic.Stack.
//
// No Unwrap or Is methods: Panic.Value is data, not a wrapped cause; errors.As
// is the only matching contract.
type HandlerPanicError struct {
	Panic HandlerPanic
}

func (e *HandlerPanicError) Error() string {
	return fmt.Sprintf("recovered panic in %s handler (client %q, action %q, request %q): %v",
		e.Panic.Kind, e.Panic.ClientID, e.Panic.Action, e.Panic.RequestID, e.Panic.Value)
}

// SetOnHandlerPanic registers a callback invoked when a user handler panics.
//
// The callback runs synchronously on whichever goroutine recovered the panic:
// the read loop for message handlers, and — for a canceled-request handler
// panic — the dispatcher's messagePump goroutine. As with an onRequestCanceled
// callback, it therefore must NOT call Stop() or block on a full SendRequest,
// or it may deadlock the pump. It should also not panic. Set it before Start.
//
// This REPLACES any previously registered callback. The ocpp1.6 and ocpp2.0.1
// client facades install their own callback here during construction, to route
// recovered panics to the facade's Errors() channel; calling this on an
// endpoint already handed to NewChargePoint/NewChargingStation removes that
// routing. Register the hook on the facade instead, or read the existing one
// with OnHandlerPanic and chain it. A hook registered here BEFORE the facade is
// constructed is preserved and chained by the facade's wrapper.
func (c *Client) SetOnHandlerPanic(handler func(HandlerPanic)) {
	c.onHandlerPanic = handler
	if d, ok := c.dispatcher.(*DefaultClientDispatcher); ok {
		d.onHandlerPanic = handler
	}
}

// OnHandlerPanic returns the callback registered with SetOnHandlerPanic, or nil
// if none is registered. It exists so code that installs its own hook on an
// endpoint can CHAIN a hook the caller registered earlier instead of silently
// replacing it — the ocpp1.6 and ocpp2.0.1 facade constructors do exactly that
// when routing recovered panics to Errors(). Read it before installing.
func (c *Client) OnHandlerPanic() func(HandlerPanic) { return c.onHandlerPanic }

// SetOnHandlerPanic registers a callback invoked when a user handler panics.
//
// The callback runs synchronously on whichever goroutine recovered the panic:
// the read loop for message handlers; a raw CanceledRequestHandler panic is
// recovered on the dispatcher's messagePump goroutine as CancelHandlerKind;
// and a server-facade canceled-request callback (or its no-callback body) is
// recovered on the facade's dedicated cancel goroutine as ErrorHandlerKind.
// A raw onRequestCanceled callback therefore must NOT call Stop() or block on
// a full SendRequest, or it may deadlock the pump. It should also not panic.
// Set it before Start.
//
// This REPLACES any previously registered callback. The ocpp1.6 and ocpp2.0.1
// server facades install their own callback here during construction, to route
// recovered panics to the facade's Errors() channel; calling this on an
// endpoint already handed to NewCentralSystem/NewCSMS removes that routing.
// Register the hook on the facade instead, or read the existing one with
// OnHandlerPanic and chain it. A hook registered here BEFORE the facade is
// constructed is preserved and chained by the facade's wrapper.
func (s *Server) SetOnHandlerPanic(handler func(HandlerPanic)) {
	s.onHandlerPanic = handler
	if d, ok := s.dispatcher.(*DefaultServerDispatcher); ok {
		d.onHandlerPanic = handler
	}
}

// OnHandlerPanic returns the callback registered with SetOnHandlerPanic, or nil
// if none is registered. See (*Client).OnHandlerPanic.
func (s *Server) OnHandlerPanic() func(HandlerPanic) { return s.onHandlerPanic }

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
