package callbackqueue

import (
	"sync"

	"github.com/lorenzodonini/ocpp-go/ocpp"
)

type RequestType string
type CallbackQueue struct {
	callbacksMutex sync.RWMutex
	callbacks      map[string]map[RequestType][]func(confirmation ocpp.Response, err error)
}

func New() CallbackQueue {
	return CallbackQueue{
		callbacks: make(map[string]map[RequestType][]func(confirmation ocpp.Response, err error)),
	}
}

func (cq *CallbackQueue) TryQueue(id string, requestType RequestType, try func() error, callback func(confirmation ocpp.Response, err error)) error {
	cq.callbacksMutex.Lock()
	defer cq.callbacksMutex.Unlock()

	if _, ok := cq.callbacks[id]; !ok {
		cq.callbacks[id] = make(map[RequestType][]func(confirmation ocpp.Response, err error))
	}
	cq.callbacks[id][requestType] = append(cq.callbacks[id][requestType], callback)

	if err := try(); err != nil {
		// Roll back ONLY the callback we just appended — not the whole
		// request-type bucket, which may already hold earlier valid callbacks.
		cbs := cq.callbacks[id][requestType]
		if len(cbs) > 0 {
			cq.callbacks[id][requestType] = cbs[:len(cbs)-1]
		}
		if len(cq.callbacks[id][requestType]) == 0 {
			delete(cq.callbacks[id], requestType)
		}
		if len(cq.callbacks[id]) == 0 {
			delete(cq.callbacks, id)
		}
		return err
	}

	return nil
}

func (cq *CallbackQueue) Dequeue(id string, requestType RequestType) (func(confirmation ocpp.Response, err error), bool) {
	cq.callbacksMutex.Lock()
	defer cq.callbacksMutex.Unlock()

	clientCallbacks, ok := cq.callbacks[id]
	if !ok {
		return nil, false
	}

	if len(clientCallbacks) == 0 {
		//panic("Internal CallbackQueue inconsistency")
		return nil, false
	}

	requestTypeCallbacks, ok := clientCallbacks[requestType]
	if !ok {
		if requestType != "" { /* requestType known and not available... */
			return nil, false
		}
		/* requestType any, take first one... */
		for reqType, cb := range clientCallbacks {
			requestType = reqType
			requestTypeCallbacks = append(requestTypeCallbacks, cb...)
			break // only first one
		}
	}

	callback := requestTypeCallbacks[0]

	if len(requestTypeCallbacks) == 1 {
		delete(cq.callbacks[id], requestType)
		// Clean up the per-client entry once its last callback is gone, so the
		// outer map doesn't accumulate empty entries for every client ID seen.
		if len(cq.callbacks[id]) == 0 {
			delete(cq.callbacks, id)
		}
	} else {
		cq.callbacks[id][requestType] = requestTypeCallbacks[1:]
	}

	return callback, true
}
