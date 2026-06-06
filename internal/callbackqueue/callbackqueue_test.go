package callbackqueue

import (
	"fmt"
	"testing"

	"github.com/lorenzodonini/ocpp-go/ocpp"
	"github.com/stretchr/testify/assert"
)

// tagCB returns a callback that records its tag into got when invoked, so a test
// can tell which queued callback was dequeued.
func tagCB(got *string, tag string) func(ocpp.Response, error) {
	return func(ocpp.Response, error) { *got = tag }
}

func ok() error { return nil }

// Typed Dequeue returns the callback registered for that request type (and FIFO
// within a type), independent of insertion order across types.
func TestCallbackQueue_DequeueByType(t *testing.T) {
	cq := New()
	var got string
	assert.NoError(t, cq.TryQueue("c1", "A", ok, tagCB(&got, "A")))
	assert.NoError(t, cq.TryQueue("c1", "B", ok, tagCB(&got, "B")))

	cb, found := cq.Dequeue("c1", "B")
	assert.True(t, found)
	cb(nil, nil)
	assert.Equal(t, "B", got)

	cb, found = cq.Dequeue("c1", "A")
	assert.True(t, found)
	cb(nil, nil)
	assert.Equal(t, "A", got)

	_, found = cq.Dequeue("c1", "A")
	assert.False(t, found)
	assert.NotContains(t, cq.callbacks, "c1", "client entry should be cleaned up when empty")
}

// Untyped Dequeue ("") returns the single oldest callback regardless of type
// (FIFO) — the CALL_ERROR / disconnect-drain behavior.
func TestCallbackQueue_DequeueEmptyTypeIsFIFO(t *testing.T) {
	cq := New()
	var got string
	assert.NoError(t, cq.TryQueue("c1", "A", ok, tagCB(&got, "first-A")))
	assert.NoError(t, cq.TryQueue("c1", "B", ok, tagCB(&got, "second-B")))

	cb, found := cq.Dequeue("c1", "")
	assert.True(t, found)
	cb(nil, nil)
	assert.Equal(t, "first-A", got, "untyped Dequeue must return the oldest callback regardless of type")

	cb, found = cq.Dequeue("c1", "")
	assert.True(t, found)
	cb(nil, nil)
	assert.Equal(t, "second-B", got)

	_, found = cq.Dequeue("c1", "")
	assert.False(t, found)
}

// A failed TryQueue must roll back ONLY the just-appended callback, leaving an
// earlier same-type callback intact.
func TestCallbackQueue_TryQueueRollbackKeepsSiblings(t *testing.T) {
	cq := New()
	var got string
	assert.NoError(t, cq.TryQueue("c1", "A", ok, tagCB(&got, "kept")))
	err := cq.TryQueue("c1", "A", func() error { return fmt.Errorf("send failed") }, tagCB(&got, "rolledback"))
	assert.Error(t, err)

	cb, found := cq.Dequeue("c1", "A")
	assert.True(t, found)
	cb(nil, nil)
	assert.Equal(t, "kept", got, "the earlier callback must survive a sibling's rollback")

	_, found = cq.Dequeue("c1", "A")
	assert.False(t, found)
	assert.NotContains(t, cq.callbacks, "c1")
}

func TestCallbackQueue_DequeueMissing(t *testing.T) {
	cq := New()
	_, found := cq.Dequeue("nope", "")
	assert.False(t, found)
	_, found = cq.Dequeue("nope", "A")
	assert.False(t, found)

	assert.NoError(t, cq.TryQueue("c1", "A", ok, func(ocpp.Response, error) {}))
	_, found = cq.Dequeue("c1", "B")
	assert.False(t, found, "a type with no pending callback must not match another type")
}
