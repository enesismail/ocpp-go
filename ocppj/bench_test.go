package ocppj_test

import (
	"strconv"
	"sync"
	"testing"

	"github.com/enesismail/ocpp-go/ocppj"
)

func BenchmarkFIFOClientQueuePushPop(b *testing.B) {
	q := ocppj.NewFIFOClientQueue(1)
	element := ocppj.RequestBundle{}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := q.Push(element); err != nil {
			b.Fatalf("Push failed: %v", err)
		}
		if popped := q.Pop(); popped == nil {
			b.Fatal("Pop returned nil")
		}
	}
}

func BenchmarkFIFOClientQueueFillDrain(b *testing.B) {
	const capacity = 64
	q := ocppj.NewFIFOClientQueue(capacity)
	element := ocppj.RequestBundle{}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		for j := 0; j < capacity; j++ {
			if err := q.Push(element); err != nil {
				b.Fatalf("Push failed at index %d: %v", j, err)
			}
		}
		if !q.IsFull() {
			b.Fatal("queue was not full after filling to capacity")
		}
		for j := 0; j < capacity; j++ {
			if popped := q.Pop(); popped == nil {
				b.Fatalf("Pop returned nil at index %d", j)
			}
		}
		if !q.IsEmpty() {
			b.Fatal("queue was not empty after draining")
		}
	}
}

func BenchmarkClientStateAddGetDelete(b *testing.B) {
	state := ocppj.NewClientState()
	req := newMockRequest("value")

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		requestID := strconv.Itoa(i)
		state.AddPendingRequest(requestID, req)
		if _, ok := state.GetPendingRequest(requestID); !ok {
			b.Fatal("pending request was not found")
		}
		if !state.HasPendingRequest() {
			b.Fatal("state had no pending request after add")
		}
		state.DeletePendingRequest(requestID)
	}
}

func BenchmarkServerStateAddDelete(b *testing.B) {
	state := ocppj.NewServerState(&sync.RWMutex{})
	req := newMockRequest("value")
	clientIDs := []string{"client-0", "client-1", "client-2", "client-3"}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		clientID := clientIDs[i%len(clientIDs)]
		requestID := strconv.Itoa(i)
		state.AddPendingRequest(clientID, requestID, req)
		if !state.HasPendingRequest(clientID) {
			b.Fatal("state had no pending request after add")
		}
		state.DeletePendingRequest(clientID, requestID)
	}
}
