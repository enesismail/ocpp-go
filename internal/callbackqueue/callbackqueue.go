package callbackqueue

import (
	"errors"
	"sync"

	"github.com/enesismail/ocpp-go/ocpp"
)

// ErrDuplicateCallback is returned by TryQueue when a callback is already
// registered for the same (clientID, requestID) pair.
//
// Note: rejection happens AFTER try() has already sent the message, so the
// request is on the wire but has no callback — its response will land on the
// "no handler available" path. This is defense-in-depth against silently
// overwriting an existing callback, not a caller-actionable error path.
// Unreachable in practice with the default (random) message-ID generator;
// reachable only via a poorly chosen custom SetMessageIdGenerator.
var ErrDuplicateCallback = errors.New("duplicate callback for request id")

// ErrEmptyRequestID is returned by TryQueue when try() yields an empty request
// ID. Registering under "" would make the callback aliasable by any later
// message with an empty MessageId; same defense-in-depth class as
// ErrDuplicateCallback, unreachable with the default message-ID generator.
var ErrEmptyRequestID = errors.New("empty request id, cannot register callback")

// CallbackQueue stores, per client id, pending response callbacks keyed by
// the OCPP message ID (UniqueId). Every consumer path — response, CALL_ERROR,
// cancel, and drain — reaches a callback only through callbacksMutex, and
// TryQueue registers the callback before releasing the mutex, so an early
// response blocks in Dequeue rather than racing registration.
type CallbackQueue struct {
	callbacksMutex sync.Mutex
	callbacks      map[string]map[string]func(ocpp.Response, error) // clientID -> requestID -> callback
}

// New returns an initialized CallbackQueue.
func New() CallbackQueue {
	return CallbackQueue{
		callbacks: make(map[string]map[string]func(ocpp.Response, error)),
	}
}

// TryQueue registers a callback for (id, requestID) after executing try() to
// obtain the requestID. try() runs inside the lock so a concurrent Dequeue
// blocks until registration completes. If try() returns an error, nothing is
// registered — no rollback is needed. If a callback is already registered for
// (id, requestID), ErrDuplicateCallback is returned and the existing callback
// is NOT overwritten.
func (cq *CallbackQueue) TryQueue(id string, try func() (string, error), callback func(ocpp.Response, error)) error {
	cq.callbacksMutex.Lock()
	defer cq.callbacksMutex.Unlock()

	requestID, err := try()
	if err != nil {
		return err
	}
	// Never register under an empty requestID: doing so would let any later
	// response/error carrying an empty MessageId alias to this callback — the
	// exact one-key-wide hole the ID keying exists to close. Unreachable with the
	// default generator; reachable only via a broken custom SetMessageIdGenerator.
	if requestID == "" {
		return ErrEmptyRequestID
	}

	if _, ok := cq.callbacks[id]; !ok {
		cq.callbacks[id] = make(map[string]func(ocpp.Response, error))
	} else if _, ok := cq.callbacks[id][requestID]; ok {
		return ErrDuplicateCallback
	}

	cq.callbacks[id][requestID] = callback
	return nil
}

// Dequeue removes and returns the callback for (id, requestID). If found, the
// inner map entry is deleted; if the inner map becomes empty, the client's
// outer map entry is also deleted. Returns (nil, false) when no callback is
// registered for the given (id, requestID).
func (cq *CallbackQueue) Dequeue(id, requestID string) (func(ocpp.Response, error), bool) {
	cq.callbacksMutex.Lock()
	defer cq.callbacksMutex.Unlock()

	inner, ok := cq.callbacks[id]
	if !ok {
		return nil, false
	}

	cb, ok := inner[requestID]
	if !ok {
		return nil, false
	}

	delete(inner, requestID)
	if len(inner) == 0 {
		delete(cq.callbacks, id)
	}

	return cb, true
}

// DrainAll collects every pending callback for id into a slice, then deletes
// the client's outer map entry. Callers must not depend on callback order — Go
// map iteration is randomized. Returns nil when the client has no entries.
func (cq *CallbackQueue) DrainAll(id string) []func(ocpp.Response, error) {
	cq.callbacksMutex.Lock()
	defer cq.callbacksMutex.Unlock()

	inner, ok := cq.callbacks[id]
	if !ok {
		return nil
	}

	result := make([]func(ocpp.Response, error), 0, len(inner))
	for _, cb := range inner {
		result = append(result, cb)
	}

	delete(cq.callbacks, id)
	return result
}
