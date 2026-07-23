package callbackqueue

// E2-0 (tasks/e2-0-requestid-keyed-callbacks.md) unit test suite.
//
// This file lives in `package callbackqueue` (not `callbackqueue_test`) so it
// can reach the unexported CallbackQueue.callbacks and callbacksMutex — exactly
// like the existing callbackqueue_test.go and concurrency_test.go.
//
// Written red-first against the pre-E2-0 API; now green. It pins the ID-keyed
// contract: TryQueue(id, try func() (string, error), callback), Dequeue(id,
// requestID), DrainAll(id), ErrDuplicateCallback — with RequestType/callbackEntry
// deleted and callbacksMutex a plain sync.Mutex.
//
// Spec tests implemented: 4, 5, 6, 7, 8.

import (
	"errors"
	"runtime"
	"testing"

	"github.com/enesismail/ocpp-go/ocpp"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// e2tagCB returns a callback that records its tag into got when invoked.
func e2tagCB(got *string, tag string) func(ocpp.Response, error) {
	return func(ocpp.Response, error) { *got = tag }
}

// e2tryOK is a try function that succeeds with the given requestID.
func e2tryOK(id string) func() (string, error) {
	return func() (string, error) { return id, nil }
}

// e2tryErr is a try function that fails with the given error.
func e2tryErr(err error) func() (string, error) {
	return func() (string, error) { return "", err }
}

// ============================================================================
// Test 4 — Correct pairing when responses are processed out of order
// ============================================================================

// TestE2_0_DequeueOutOfOrderSameClient verifies that with two callbacks
// registered for the same client (no "type" distinction in the new API —
// matching is by exact requestID), dequeueing the SECOND callback's requestID
// first returns the correct callback and leaves the first untouched.
//
// This is a callbackqueue unit test, not a facade e2e test. A facade e2e
// version would false-pass under FIFO single-in-flight dispatch: R2 is not
// written until R1 completes, so a real peer cannot produce out-of-order
// responses and the test would pass on master too (per spec §8 test 4 note).
func TestE2_0_DequeueOutOfOrderSameClient(t *testing.T) {
	cq := New()
	var got string

	require.NoError(t, cq.TryQueue("c1", e2tryOK("req-A"), e2tagCB(&got, "A")))
	require.NoError(t, cq.TryQueue("c1", e2tryOK("req-B"), e2tagCB(&got, "B")))

	// Dequeue the second first.
	cb, found := cq.Dequeue("c1", "req-B")
	require.True(t, found)
	cb(nil, nil)
	assert.Equal(t, "B", got)

	// The first must still be there.
	cb, found = cq.Dequeue("c1", "req-A")
	require.True(t, found)
	cb(nil, nil)
	assert.Equal(t, "A", got)

	// Both consumed — client entry cleaned up.
	_, found = cq.Dequeue("c1", "req-A")
	assert.False(t, found)
	_, found = cq.Dequeue("c1", "req-B")
	assert.False(t, found)
	assertNotContainsOuter(t, &cq, "c1")
}

// ============================================================================
// Test 5 — try() failure ⇒ no callback registered, no leak
// ============================================================================

// TestE2_0_TryQueueFailureNoLeak verifies that when try() returns an error,
// no callback is registered and no entry is left for the client — the
// rollback path is deleted in E2-0 because registration happens AFTER try().
func TestE2_0_TryQueueFailureNoLeak(t *testing.T) {
	cq := New()
	var got string
	sendErr := errors.New("send failed")

	// First, a successful registration for the same client — must survive.
	require.NoError(t, cq.TryQueue("c1", e2tryOK("req-ok"), e2tagCB(&got, "ok")))

	// Then a failed registration — must NOT overwrite or leak.
	err := cq.TryQueue("c1", e2tryErr(sendErr), e2tagCB(&got, "should-not-fire"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, sendErr))

	// The successful callback must still be present.
	cb, found := cq.Dequeue("c1", "req-ok")
	require.True(t, found, "the earlier successful callback must survive a failed TryQueue")
	cb(nil, nil)
	assert.Equal(t, "ok", got)

	// The failed registration must not leave a callback for the failed ID.
	_, found = cq.Dequeue("c1", "req-failed")
	assert.False(t, found, "a failed TryQueue must not register a callback")

	// The failed try() returned "" — must not leave a stray entry under "".
	_, found = cq.Dequeue("c1", "")
	assert.False(t, found, "a failed TryQueue must not register under its own (empty) returned ID")

	// Client entry cleaned up after the last callback is dequeued.
	_, found = cq.Dequeue("c1", "req-ok")
	assert.False(t, found)
	assertNotContainsOuter(t, &cq, "c1")
}

// ============================================================================
// Test 6 — Dequeue racing TryQueue blocks until registration
// ============================================================================

// TestE2_0_DequeueBlocksUntilRegistration verifies the lock invariant from
// spec §3: a Dequeue that races TryQueue blocks until registration completes
// (callbacksMutex serializes both), so an early response is not lost.
//
// try() spawns a goroutine that calls Dequeue(id, knownID), then returns
// (knownID, nil). The spawned goroutine's Dequeue blocks on callbacksMutex
// until TryQueue's try returns and the mutex is released. After TryQueue
// returns, the spawned goroutine acquires the mutex and finds the callback.
//
// This is a callbackqueue unit test — the right level per spec §8 test 6.
func TestE2_0_DequeueBlocksUntilRegistration(t *testing.T) {
	cq := New()
	var got string

	knownID := "req-race"
	var dequeueResult struct {
		cb    func(ocpp.Response, error)
		found bool
	}
	dequeueDone := make(chan struct{})
	started := make(chan struct{})

	try := func() (string, error) {
		// Deterministic §3 probe: callbacksMutex MUST be held for the whole of
		// try() — a TryLock that succeeds here proves an implementation that
		// registers outside the lock, the exact race this test guards against.
		require.False(t, cq.callbacksMutex.TryLock(),
			"callbacksMutex must be held for the full duration of try()")
		// Spawn a Dequeue that races registration, then hand it real scheduling
		// opportunities to reach callbacksMutex.Lock WHILE the lock is held. The
		// prior version returned immediately after `go`, so the goroutine
		// typically hadn't started and the test degenerated to the sequential
		// path (test 4). The handshake + yields make the race genuine.
		go func() {
			close(started)
			dequeueResult.cb, dequeueResult.found = cq.Dequeue("c1", knownID)
			close(dequeueDone)
		}()
		<-started
		for i := 0; i < 100; i++ {
			runtime.Gosched()
		}
		return knownID, nil
	}

	require.NoError(t, cq.TryQueue("c1", try, e2tagCB(&got, "race-winner")))

	// The Dequeue goroutine must have found the callback.
	<-dequeueDone
	require.True(t, dequeueResult.found, "racing Dequeue must find the callback registered by TryQueue")
	dequeueResult.cb(nil, nil)
	assert.Equal(t, "race-winner", got)

	// The callback was consumed — client entry cleaned up.
	_, found := cq.Dequeue("c1", knownID)
	assert.False(t, found)
	assertNotContainsOuter(t, &cq, "c1")
}

// ============================================================================
// Test 7 — Disconnect drain via DrainAll
// ============================================================================

// TestE2_0_DrainAllReturnsEveryCallbackExactlyOnce verifies that DrainAll:
//  1. Returns every pending callback.
//  2. Deletes the client's outer map entry.
//  3. A subsequent register/dequeue cycle works (no empty entries accumulate).
//  4. Makes NO ordering assertion (Go map iteration is randomized, §2, §7).
func TestE2_0_DrainAllReturnsEveryCallbackExactlyOnce(t *testing.T) {
	cq := New()

	// Register 3 distinguishable callbacks for client "c1", each counting its
	// own invocations — so "exactly once" is checked by identity, not just by
	// the length of the returned slice (a DrainAll that returned one callback
	// twice and dropped another would pass a length-only check).
	counts := map[string]int{}
	require.NoError(t, cq.TryQueue("c1", e2tryOK("a"), func(ocpp.Response, error) { counts["a"]++ }))
	require.NoError(t, cq.TryQueue("c1", e2tryOK("b"), func(ocpp.Response, error) { counts["b"]++ }))
	require.NoError(t, cq.TryQueue("c1", e2tryOK("c"), func(ocpp.Response, error) { counts["c"]++ }))

	disconnectErr := errors.New("client disconnected")
	drained := cq.DrainAll("c1")

	// 1. Every callback returned exactly once.
	assert.Len(t, drained, 3, "DrainAll must return all 3 pending callbacks")

	// Invoke each drained callback; each registered callback must fire exactly once.
	for _, cb := range drained {
		cb(nil, disconnectErr)
	}
	assert.Equal(t, map[string]int{"a": 1, "b": 1, "c": 1}, counts,
		"DrainAll must return each registered callback exactly once")

	// 2. The client's outer map entry is deleted.
	assertNotContainsOuter(t, &cq, "c1")

	// 3. A subsequent register/dequeue cycle works.
	require.NoError(t, cq.TryQueue("c1", e2tryOK("d"), func(ocpp.Response, error) {}))
	cb, found := cq.Dequeue("c1", "d")
	require.True(t, found, "a new callback must be reachable after DrainAll")
	cb(nil, nil)
	_, found = cq.Dequeue("c1", "d")
	assert.False(t, found)
	assertNotContainsOuter(t, &cq, "c1")

	// 4. DrainAll on a client with no entries returns empty, no panic.
	empty := cq.DrainAll("c1")
	assert.Empty(t, empty, "DrainAll on a missing client must return no callbacks")
}

// ============================================================================
// Test 8 — Duplicate-ID registration is rejected
// ============================================================================

// TestE2_0_DuplicateIDRejected verifies that registering a second callback
// under the same (client, requestID) pair is rejected, not silently
// overwritten (§6).
//
// At the callbackqueue level, the test forces a collision by passing a try
// function that returns the same ID twice. (A facade-level test would force
// a collision by mutating ocppj.messageIdGenerator — see the facade tests.)
// The register method must detect the duplicate and return an error.
//
// This test does NOT run in parallel — it need not (no global mutation),
// but the constraint is documented so a future facade-level variant of this
// test can coexist safely.
func TestE2_0_DuplicateIDRejected(t *testing.T) {
	// This is the callbackqueue-level variant: it forces a duplicate ID
	// directly via try(), never touching the package-global messageIdGenerator,
	// so it needs no generator save/restore (unlike a facade-level variant).
	cq := New()
	var got string

	// First registration with "dup-id" succeeds.
	require.NoError(t, cq.TryQueue("c1", e2tryOK("dup-id"), e2tagCB(&got, "first")))

	// Second registration with the same ID must be rejected.
	err := cq.TryQueue("c1", e2tryOK("dup-id"), e2tagCB(&got, "second-should-never-fire"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrDuplicateCallback),
		"duplicate ID registration must return ErrDuplicateCallback")

	// The first callback must still be there, intact.
	cb, found := cq.Dequeue("c1", "dup-id")
	require.True(t, found, "the first callback must survive duplicate rejection")
	cb(nil, nil)
	assert.Equal(t, "first", got, "the first callback must not be overwritten")

	// No second callback.
	_, found = cq.Dequeue("c1", "dup-id")
	assert.False(t, found)
	assertNotContainsOuter(t, &cq, "c1")
}

// ============================================================================
// Helpers
// ============================================================================

// assertNotContainsOuter asserts that cq.callbacks does not contain the
// given client id (the outer map entry is deleted).
func assertNotContainsOuter(t *testing.T, cq *CallbackQueue, id string) {
	t.Helper()
	cq.callbacksMutex.Lock()
	defer cq.callbacksMutex.Unlock()
	_, ok := cq.callbacks[id]
	assert.False(t, ok, "client %q must not have an outer map entry", id)
}
