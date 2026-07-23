package callbackqueue

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/enesismail/ocpp-go/ocpp"
)

// produced is a (clientID, requestID) pair sent from a producer to consumers so
// they know which callback to dequeue.
type produced struct {
	clientID  string
	requestID string
}

// TestCallbackQueue_ConcurrentProduceConsumeConservesCallbacks verifies that
// concurrent producers and consumers do not lose any callbacks. Producers
// generate globally unique requestIDs and send the (clientID, requestID) pair
// over a channel; consumers dequeue by exact ID. This replaces the old
// type-keyed variant — the ID space is now per-(client,requestID) instead of
// per-(client,RequestType), so IDs must be unique.
func TestCallbackQueue_ConcurrentProduceConsumeConservesCallbacks(t *testing.T) {
	queue := New()
	q := &queue

	ids := []string{"client-0", "client-1", "client-2", "client-3"}

	const producers = 8
	const perProducer = 200
	const consumers = 6
	const wantQueued = int64(producers * perProducer)

	var queued int64
	var dequeued int64
	var producerWG sync.WaitGroup
	var consumerWG sync.WaitGroup

	ch := make(chan produced, producers*perProducer)

	producerWG.Add(producers)
	for g := 0; g < producers; g++ {
		g := g
		go func() {
			defer producerWG.Done()
			for i := 0; i < perProducer; i++ {
				id := ids[(g+i)%len(ids)]
				requestID := fmt.Sprintf("req-%d-%d", g, i)
				try := func() (string, error) { return requestID, nil }
				if err := q.TryQueue(id, try, func(ocpp.Response, error) {}); err != nil {
					t.Errorf("TryQueue returned unexpected error: %v", err)
					return
				}
				atomic.AddInt64(&queued, 1)
				ch <- produced{id, requestID}
			}
		}()
	}

	consumerWG.Add(consumers)
	for c := 0; c < consumers; c++ {
		go func() {
			defer consumerWG.Done()
			for it := range ch {
				if cb, ok := q.Dequeue(it.clientID, it.requestID); ok {
					cb(nil, nil)
					atomic.AddInt64(&dequeued, 1)
				}
			}
		}()
	}

	producerWG.Wait()
	close(ch)
	consumerWG.Wait()

	if got := atomic.LoadInt64(&queued); got != wantQueued {
		t.Fatalf("queued count = %d, want %d", got, wantQueued)
	}
	if got := atomic.LoadInt64(&dequeued); got != wantQueued {
		t.Fatalf("dequeued count = %d, want %d", got, wantQueued)
	}
}

// TestCallbackQueue_ConcurrentRollbackConservesSuccessfulCallbacks verifies
// that when some try() calls fail, their callbacks are never registered (and
// thus never dequeued), while all successful registrations survive and are
// dequeued exactly once. In the new ID-keyed design there is no rollback path —
// registration happens AFTER try(), so a failed try simply returns without
// registering anything.
func TestCallbackQueue_ConcurrentRollbackConservesSuccessfulCallbacks(t *testing.T) {
	queue := New()
	q := &queue

	ids := []string{"client-0", "client-1", "client-2"}
	errSendFailed := errors.New("send failed")

	const producers = 8
	const perProducer = 200

	var queued int64
	var failed int64
	var dequeued int64
	var failedDequeued int64
	var producerWG sync.WaitGroup

	ch := make(chan produced, producers*perProducer)

	producerWG.Add(producers)
	for g := 0; g < producers; g++ {
		g := g
		go func() {
			defer producerWG.Done()
			for i := 0; i < perProducer; i++ {
				seq := g*perProducer + i
				id := ids[seq%len(ids)]
				requestID := fmt.Sprintf("req-%d-%d", g, i)
				shouldFail := seq%5 == 0
				cb := func(ocpp.Response, error) {}
				var try func() (string, error)
				if shouldFail {
					try = func() (string, error) { return "", errSendFailed }
					cb = func(ocpp.Response, error) { atomic.AddInt64(&failedDequeued, 1) }
				} else {
					try = func() (string, error) { return requestID, nil }
				}

				err := q.TryQueue(id, try, cb)
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
				ch <- produced{id, requestID}
			}
		}()
	}
	producerWG.Wait()
	close(ch)

	for it := range ch {
		cb, ok := q.Dequeue(it.clientID, it.requestID)
		if !ok {
			t.Errorf("Dequeue failed for %s/%s", it.clientID, it.requestID)
			continue
		}
		cb(nil, nil)
		atomic.AddInt64(&dequeued, 1)
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

// TestCallbackQueue_ConcurrentClientIsolation verifies that concurrent
// producers targeting different clients do not interfere, and each client's
// callbacks are independently dequeued. Replaces the old type-keyed variant —
// matching is now by exact requestID.
func TestCallbackQueue_ConcurrentClientIsolation(t *testing.T) {
	queue := New()
	q := &queue

	const clients = 8
	const perClient = 200

	var queued [clients]int64
	var dequeued [clients]int64
	var wg sync.WaitGroup

	ch := make(chan produced, clients*perClient)

	wg.Add(clients)
	for client := 0; client < clients; client++ {
		client := client
		go func() {
			defer wg.Done()
			id := clientID(client)
			for i := 0; i < perClient; i++ {
				requestID := fmt.Sprintf("req-%d-%d", client, i)
				try := func() (string, error) { return requestID, nil }
				if err := q.TryQueue(id, try, func(ocpp.Response, error) {}); err != nil {
					t.Errorf("TryQueue returned unexpected error for %s: %v", id, err)
					return
				}
				atomic.AddInt64(&queued[client], 1)
				ch <- produced{id, requestID}
			}
		}()
	}
	wg.Wait()
	close(ch)

	for it := range ch {
		cb, ok := q.Dequeue(it.clientID, it.requestID)
		if !ok {
			t.Errorf("Dequeue failed for %s/%s", it.clientID, it.requestID)
			continue
		}
		cb(nil, nil)
		// clientID("N") returns "client-N"; extract the index.
		clientIdx := int(it.clientID[len(it.clientID)-1] - '0')
		atomic.AddInt64(&dequeued[clientIdx], 1)
	}

	for client := 0; client < clients; client++ {
		if got, want := atomic.LoadInt64(&dequeued[client]), atomic.LoadInt64(&queued[client]); got != want {
			t.Fatalf("client %s dequeued count = %d, want queued count %d", clientID(client), got, want)
		}
	}
}

func clientID(i int) string {
	return "client-" + string(rune('0'+i))
}
