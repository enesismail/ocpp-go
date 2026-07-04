package callbackqueue

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/enesismail/ocpp-go/ocpp"
)

func TestCallbackQueue_ConcurrentProduceConsumeConservesCallbacks(t *testing.T) {
	queue := New()
	q := &queue

	ids := []string{"client-0", "client-1", "client-2", "client-3"}
	requestTypes := []RequestType{"BootNotification", "Heartbeat", "StatusNotification"}

	const producers = 8
	const perProducer = 200
	const consumers = 6
	const wantQueued = int64(producers * perProducer)

	var queued int64
	var dequeued int64
	var producerWG sync.WaitGroup
	var consumerWG sync.WaitGroup
	var producersDone atomic.Bool

	producerWG.Add(producers)
	for g := 0; g < producers; g++ {
		g := g
		go func() {
			defer producerWG.Done()
			for i := 0; i < perProducer; i++ {
				id := ids[(g+i)%len(ids)]
				requestType := requestTypes[(g*perProducer+i)%len(requestTypes)]
				if err := q.TryQueue(id, requestType, ok, func(ocpp.Response, error) {}); err != nil {
					t.Errorf("TryQueue returned unexpected error: %v", err)
					return
				}
				atomic.AddInt64(&queued, 1)
			}
		}()
	}

	consumerWG.Add(consumers)
	for c := 0; c < consumers; c++ {
		go func() {
			defer consumerWG.Done()
			for {
				found := false
				for _, id := range ids {
					for _, requestType := range requestTypes {
						if cb, ok := q.Dequeue(id, requestType); ok {
							cb(nil, nil)
							atomic.AddInt64(&dequeued, 1)
							found = true
						}
					}
				}
				if found {
					continue
				}
				if producersDone.Load() && atomic.LoadInt64(&dequeued) == atomic.LoadInt64(&queued) {
					return
				}
				runtime.Gosched()
			}
		}()
	}

	producerWG.Wait()
	producersDone.Store(true)
	consumerWG.Wait()

	if got := atomic.LoadInt64(&queued); got != wantQueued {
		t.Fatalf("queued count = %d, want %d", got, wantQueued)
	}
	if got := atomic.LoadInt64(&dequeued); got != wantQueued {
		t.Fatalf("dequeued count = %d, want %d", got, wantQueued)
	}
}

func TestCallbackQueue_ConcurrentRollbackConservesSuccessfulCallbacks(t *testing.T) {
	queue := New()
	q := &queue

	ids := []string{"client-0", "client-1", "client-2"}
	requestTypes := []RequestType{"MeterValues", "Heartbeat", "Authorize"}
	errSendFailed := errors.New("send failed")

	const producers = 8
	const perProducer = 200

	var queued int64
	var failed int64
	var dequeued int64
	var failedDequeued int64
	var producerWG sync.WaitGroup

	producerWG.Add(producers)
	for g := 0; g < producers; g++ {
		g := g
		go func() {
			defer producerWG.Done()
			for i := 0; i < perProducer; i++ {
				seq := g*perProducer + i
				id := ids[seq%len(ids)]
				requestType := requestTypes[seq%len(requestTypes)]
				shouldFail := seq%5 == 0
				cb := func(ocpp.Response, error) {}
				try := ok
				if shouldFail {
					try = func() error { return errSendFailed }
					cb = func(ocpp.Response, error) { atomic.AddInt64(&failedDequeued, 1) }
				}

				err := q.TryQueue(id, requestType, try, cb)
				if shouldFail {
					if !errors.Is(err, errSendFailed) {
						t.Errorf("failed TryQueue error = %v, want %v", err, errSendFailed)
						return
					}
					atomic.AddInt64(&failed, 1)
					continue
				}
				if err != nil {
					t.Errorf("successful TryQueue returned error: %v", err)
					return
				}
				atomic.AddInt64(&queued, 1)
			}
		}()
	}
	producerWG.Wait()

	for _, id := range ids {
		for _, requestType := range requestTypes {
			for {
				cb, ok := q.Dequeue(id, requestType)
				if !ok {
					break
				}
				cb(nil, nil)
				atomic.AddInt64(&dequeued, 1)
			}
		}
	}

	if got := atomic.LoadInt64(&failedDequeued); got != 0 {
		t.Fatalf("dequeued %d rolled-back callbacks", got)
	}
	if got := atomic.LoadInt64(&queued); got != atomic.LoadInt64(&dequeued) {
		t.Fatalf("dequeued count = %d, want successfully queued count %d", atomic.LoadInt64(&dequeued), got)
	}
	wantFailed := int64((producers*perProducer + 4) / 5)
	if got := atomic.LoadInt64(&failed); got != wantFailed {
		t.Fatalf("failed count = %d, want %d", got, wantFailed)
	}
}

func TestCallbackQueue_ConcurrentClientIsolation(t *testing.T) {
	queue := New()
	q := &queue

	requestTypes := []RequestType{"BootNotification", "Heartbeat", "StatusNotification"}

	const clients = 8
	const perClient = 200

	var queued [clients]int64
	var dequeued [clients]int64
	var wg sync.WaitGroup

	wg.Add(clients)
	for client := 0; client < clients; client++ {
		client := client
		go func() {
			defer wg.Done()
			id := clientID(client)
			for i := 0; i < perClient; i++ {
				requestType := requestTypes[i%len(requestTypes)]
				if err := q.TryQueue(id, requestType, ok, func(ocpp.Response, error) {}); err != nil {
					t.Errorf("TryQueue returned unexpected error for %s: %v", id, err)
					return
				}
				atomic.AddInt64(&queued[client], 1)
			}
		}()
	}
	wg.Wait()

	wg.Add(clients)
	for client := 0; client < clients; client++ {
		client := client
		go func() {
			defer wg.Done()
			id := clientID(client)
			for _, requestType := range requestTypes {
				for {
					cb, ok := q.Dequeue(id, requestType)
					if !ok {
						break
					}
					cb(nil, nil)
					atomic.AddInt64(&dequeued[client], 1)
				}
			}
		}()
	}
	wg.Wait()

	for client := 0; client < clients; client++ {
		if got, want := atomic.LoadInt64(&dequeued[client]), atomic.LoadInt64(&queued[client]); got != want {
			t.Fatalf("client %s dequeued count = %d, want queued count %d", clientID(client), got, want)
		}
	}
}

func clientID(i int) string {
	return "client-" + string(rune('0'+i))
}
