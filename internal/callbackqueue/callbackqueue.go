package callbackqueue

import (
	"sync"

	"github.com/enesismail/ocpp-go/ocpp"
)

// RequestType identifies the kind of request a callback is waiting on (the OCPP
// feature name). It lets a response be routed to the callback registered for the
// same feature, instead of blindly to the oldest pending callback (which could be
// for a different type and trigger an interface-conversion panic on the response).
type RequestType string

type callbackEntry struct {
	requestType RequestType
	callback    func(confirmation ocpp.Response, err error)
}

// CallbackQueue stores, per client id, the pending response callbacks in
// insertion order. Ordering matters: the typed path (Dequeue with a request type)
// returns the oldest callback of that type (FIFO within a type), while the
// untyped path (Dequeue with "") returns the single oldest callback regardless of
// type — used by CALL_ERROR handling (a protocol error carries no feature name)
// and by disconnect draining.
type CallbackQueue struct {
	callbacksMutex sync.RWMutex
	callbacks      map[string][]callbackEntry
}

func New() CallbackQueue {
	return CallbackQueue{
		callbacks: make(map[string][]callbackEntry),
	}
}

func (cq *CallbackQueue) TryQueue(id string, requestType RequestType, try func() error, callback func(confirmation ocpp.Response, err error)) error {
	cq.callbacksMutex.Lock()
	defer cq.callbacksMutex.Unlock()

	cq.callbacks[id] = append(cq.callbacks[id], callbackEntry{requestType: requestType, callback: callback})

	if err := try(); err != nil {
		// Roll back ONLY the entry we just appended (it is the last one), leaving
		// any earlier pending callbacks for this client intact.
		entries := cq.callbacks[id]
		entries = entries[:len(entries)-1]
		if len(entries) == 0 {
			delete(cq.callbacks, id)
		} else {
			cq.callbacks[id] = entries
		}
		return err
	}

	return nil
}

// Dequeue removes and returns the next pending callback for the given client id.
// If requestType is non-empty, it returns the oldest callback registered for that
// type (or false if none). If requestType is "", it returns the single oldest
// callback regardless of type (FIFO), which the CALL_ERROR and disconnect-drain
// paths rely on since they have no feature name. Returns false if no callback is
// pending.
func (cq *CallbackQueue) Dequeue(id string, requestType RequestType) (func(confirmation ocpp.Response, err error), bool) {
	cq.callbacksMutex.Lock()
	defer cq.callbacksMutex.Unlock()

	entries, ok := cq.callbacks[id]
	if !ok || len(entries) == 0 {
		return nil, false
	}

	idx := -1
	if requestType == "" {
		// Oldest pending callback, any type.
		idx = 0
	} else {
		// Oldest pending callback of the requested type.
		for i := range entries {
			if entries[i].requestType == requestType {
				idx = i
				break
			}
		}
		if idx == -1 {
			return nil, false
		}
	}

	cb := entries[idx].callback
	entries = append(entries[:idx], entries[idx+1:]...)
	if len(entries) == 0 {
		delete(cq.callbacks, id)
	} else {
		cq.callbacks[id] = entries
	}
	return cb, true
}
