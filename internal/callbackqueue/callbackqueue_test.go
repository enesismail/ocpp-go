package callbackqueue

import (
	"fmt"
	"testing"

	"github.com/enesismail/ocpp-go/ocpp"
	"github.com/stretchr/testify/assert"
)

// tagCB returns a callback that records its tag into got when invoked, so a test
// can tell which queued callback was dequeued.
func tagCB(got *string, tag string) func(ocpp.Response, error) {
	return func(ocpp.Response, error) { *got = tag }
}

// tryOK is a try function that succeeds with the given requestID.
func tryOK(requestID string) func() (string, error) {
	return func() (string, error) { return requestID, nil }
}

// TestCallbackQueue_DequeueByID verifies that with two callbacks registered for
// the same client under different requestIDs, dequeueing by the second ID first
// returns the correct callback and leaves the first untouched. This replaces the
// old type-keyed DequeueByType test — matching is now by exact requestID.
func TestCallbackQueue_DequeueByID(t *testing.T) {
	cq := New()
	var got string
	assert.NoError(t, cq.TryQueue("c1", tryOK("req-A"), tagCB(&got, "A")))
	assert.NoError(t, cq.TryQueue("c1", tryOK("req-B"), tagCB(&got, "B")))

	cb, found := cq.Dequeue("c1", "req-B")
	assert.True(t, found)
	cb(nil, nil)
	assert.Equal(t, "B", got)

	cb, found = cq.Dequeue("c1", "req-A")
	assert.True(t, found)
	cb(nil, nil)
	assert.Equal(t, "A", got)

	_, found = cq.Dequeue("c1", "req-A")
	assert.False(t, found)
	assertNotContainsOuter(t, &cq, "c1")
}

// TestCallbackQueue_DequeueByRequestID verifies that each callback is correctly
// retrieved by its specific requestID and that consuming all callbacks cleans up
// the client's outer map entry. This replaces the old DequeueEmptyTypeIsFIFO
// test — there is no untyped dequeue in the ID-keyed design; DrainAll (tested in
// e2_0_test.go) is the new equivalent of the disconnect-drain path.
func TestCallbackQueue_DequeueByRequestID(t *testing.T) {
	cq := New()
	var got string
	assert.NoError(t, cq.TryQueue("c1", tryOK("req-A"), tagCB(&got, "first-A")))
	assert.NoError(t, cq.TryQueue("c1", tryOK("req-B"), tagCB(&got, "second-B")))

	// Dequeue by specific requestID — each callback is exactly addressable.
	cb, found := cq.Dequeue("c1", "req-A")
	assert.True(t, found)
	cb(nil, nil)
	assert.Equal(t, "first-A", got)

	cb, found = cq.Dequeue("c1", "req-B")
	assert.True(t, found)
	cb(nil, nil)
	assert.Equal(t, "second-B", got)

	_, found = cq.Dequeue("c1", "req-A")
	assert.False(t, found)
	assertNotContainsOuter(t, &cq, "c1")
}

// TestCallbackQueue_TryQueueFailureKeepsEarlierCallbacks verifies that when
// try() returns an error, nothing is registered and earlier callbacks for the
// same client survive intact. This replaces the old rollback test — the new
// design registers AFTER try(), so there is no rollback path at all.
func TestCallbackQueue_TryQueueFailureKeepsEarlierCallbacks(t *testing.T) {
	cq := New()
	var got string
	assert.NoError(t, cq.TryQueue("c1", tryOK("req-kept"), tagCB(&got, "kept")))
	err := cq.TryQueue("c1", func() (string, error) { return "", fmt.Errorf("send failed") }, tagCB(&got, "rolledback"))
	assert.Error(t, err)

	cb, found := cq.Dequeue("c1", "req-kept")
	assert.True(t, found)
	cb(nil, nil)
	assert.Equal(t, "kept", got, "the earlier callback must survive a sibling's failure")

	_, found = cq.Dequeue("c1", "req-kept")
	assert.False(t, found)
	assertNotContainsOuter(t, &cq, "c1")
}

// TestCallbackQueue_DequeueMissing verifies that Dequeue returns false when no
// callback is registered for the given (id, requestID) pair.
func TestCallbackQueue_DequeueMissing(t *testing.T) {
	cq := New()
	_, found := cq.Dequeue("nope", "req-any")
	assert.False(t, found)

	assert.NoError(t, cq.TryQueue("c1", tryOK("req-A"), func(ocpp.Response, error) {}))
	_, found = cq.Dequeue("c1", "req-B")
	assert.False(t, found, "a requestID with no pending callback must not match another ID")
}
