package ocpp2

// PR-E1c (tasks/e1c-context-aware-send.md) RED-FIRST white-box test.
//
// Test 7 — 2.0.1 callback-leak on Stop (regression guard for the required
// clearCallbacks mirror).
//
// This file lives in `package ocpp2` (not `ocpp2_test`) so it can reach
// the unexported `cs.callbacks` field of the `chargingStation` struct.
//
// RED-FIRST discipline: this test references the PR-E1c contract that
// cs.clearCallbacks() must be called on the stopC arm of
// asyncCallbackHandler — a mirror of 1.6's clearCallbacks(). Against
// today's codebase:
//   - The 2.0.1 stopC arm just `return`s (does NOT drain callbacks)
//   - No clearCallbacks method exists on chargingStation
//
// This test is EXPECTED to fail (the post-stop Dequeue returns true —
// the callback is still registered) without the clearCallbacks mirror,
// and pass with it — that IS the intended red state.
//
// Blocker fixes from codex review:
//   - No real network dial (previous draft called ws.NewClient() +
//     cs.Start("ws://localhost:9999/...") which did a real dial → abort).
//   - No destructive Eventually poll (previous draft used Dequeue in the
//     Eventually predicate, which DESTRUCTIVELY removed the callback it
//     was checking → false-pass). Now uses a SINGLE definitive check.
//
// Spec test implemented: 7.

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/enesismail/ocpp-go/internal/callbackqueue"
	"github.com/enesismail/ocpp-go/ocpp"
)

const e1cClearCallbacksBound = 2 * time.Second

// TestE1cClearCallbacksOnStop is the white-box regression guard for the
// required 2.0.1 clearCallbacks mirror (spec test 7).
//
// It constructs a minimal chargingStation with no network client, seeds
// cs.callbacks directly with a no-op callback (no rail event pending),
// launches asyncCallbackHandler, triggers the stopC path exactly as Stop()
// would, waits for the handler to exit, and does a SINGLE definitive
// Dequeue check — not an Eventually poll.
//
// Without the clearCallbacks mirror (today's code), the seeded callback
// survives because the stopC arm just returns without draining. With the
// mirror, close(stopC) causes the handler's stopC arm to call
// cs.clearCallbacks() (the Dequeue-drain loop, mirroring 1.6's
// charge_point.go:441), which drains the callback → Dequeue returns false.
func TestE1cClearCallbacksOnStop(t *testing.T) {
	cs := &chargingStation{
		callbacks:       callbackqueue.New(),
		responseHandler: make(chan responseEnvelope, 1),
		errorHandler:    make(chan error, 1),
		stopOnce:        &sync.Once{},
		// client is intentionally nil — the handler's stopC arm does not
		// dereference it, and no rail event will arrive to trigger the
		// responseHandler/errorHandler branches.
	}
	stopC := make(chan struct{}, 1)
	cs.storeStopC(stopC)

	// Seed a callback with a no-op try: the callback is registered but no
	// request is actually sent, so no response/error event will ever arrive
	// in the handler to dequeue it. Only clearCallbacks() on the stopC arm
	// can drain it.
	err := cs.callbacks.TryQueue("main",
		func() (string, error) { return "Heartbeat", nil },
		func(ocpp.Response, error) {})
	require.NoError(t, err, "TryQueue must succeed")

	// Launch the callback handler (same goroutine pattern as Start).
	handlerDone := make(chan struct{})
	go func() {
		cs.asyncCallbackHandler(stopC)
		close(handlerDone)
	}()

	// Trigger exactly what Stop() triggers (minus the network client.Stop()):
	// close stopC via stopOnce.
	cs.stopOnce.Do(func() { close(stopC) })

	// Wait for the handler to exit.
	select {
	case <-handlerDone:
	case <-time.After(e1cClearCallbacksBound):
		t.Fatal("asyncCallbackHandler did not exit after stopC closed")
	}

	// SINGLE definitive check (not a poll): if the clearCallbacks() mirror
	// exists, the handler drained callbacks on the stopC arm. If not, the
	// seeded callback is still registered.
	_, ok := cs.callbacks.Dequeue("main", "Heartbeat")
	assert.False(t, ok, "callbacks must be drained on stop via the clearCallbacks() mirror on the stopC arm")
}
