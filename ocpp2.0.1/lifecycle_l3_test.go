package ocpp2

// PR-L1 (tasks/facade-lifecycle-hardening.md §2, §Tests item 7) RED-FIRST
// white-box unit test - the L3 residual guard.
//
// This file lives in `package ocpp2` (not `ocpp2_test`), alongside
// context_awaitresult_test.go, for the same reason that file does: it needs
// the unexported chargingStation.awaitCtxResult helper and the
// package-level asyncResponse type.
//
// Spec §Tests item 7's ⚠ warning: 2.0.1 has NO equivalent to 1.6's
// testhooks.ChargePointResponse seam, and adding one is explicitly out of
// scope for PR-L1 ("Do NOT add a production test-hook to 2.0.1" - see this
// repo's PR-L1 test-writing task description). The spec permits restricting
// test 7 to the unit-level awaitCtxResult seam on this facade - this file
// is that restriction. It mirrors ocpp1.6/lifecycle_l3_test.go exactly;
// see that file's doc comment for the full mechanism note (empirically
// verified there) on why the race below is deliberately UNSYNCHRONIZED
// rather than "write then close back-to-back after confirming the goroutine
// has parked" - the latter was tried first and measured at 0 stopC wins in
// 20000 iterations, because Go's select resolves a wakeup on an
// ALREADY-PARKED goroutine as a first-committer-wins race, not a fresh
// pseudo-random re-pick. The genuine tie this test needs only occurs when
// asyncResponseC's send and stopC's close both complete before the
// goroutine's select first evaluates them - which requires letting the
// goroutine's own startup race unsynchronized against this goroutine's
// write+close. Per spec §Tests: "-race -count=3 for the timing-sensitive
// ones" - run this test that way for a meaningful signal (empirically
// ~10-20x more likely to expose the tie than a plain run, in the isolated
// experiment behind this file).
//
// RED (today): awaitCtxResult's stopC arm returns the "stopped" error
// unconditionally, with no re-check of asyncResponseC first - an iteration
// that lands the genuine tie fails. GREEN once spec §2 item 4's re-check
// lands: the stopC arm always finds (and returns) the ready response
// instead, on every iteration, regardless of timing.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// HIT RATE: under -race the tie lands roughly 1 iteration in 6000-8000
// (~4-5 hits per 30000-iteration run; ~12 under `-race -count=3`, which is the
// GATE). WITHOUT -race it is ~10-20x rarer, so a plain `go test` green proves
// NOTHING for this defect. It cannot be made deterministic - see the sibling
// ocpp1.6/lifecycle_l3_test.go doc comment for why (parked-select wakeups
// resolve first-committer-wins, and pre-loading asyncResponseC is consumed by
// awaitCtxResult's non-blocking pre-check before the stopC arm is reached).
//
// This facade has NO testhooks seam, which is why item 7's coverage is
// white-box here rather than end-to-end; the spec forbids adding a production
// hook for testing.
func TestL1L3ResidualStopCArmRechecksAsyncResponseC(t *testing.T) {
	const iterations = 30000
	for i := 0; i < iterations; i++ {
		cs := &chargingStation{}
		stopC := make(chan struct{})
		cs.storeStopC(stopC)
		asyncResponseC := make(chan asyncResponse, 1)
		resultC := make(chan asyncResponse, 1)

		go func() {
			r, e := cs.awaitCtxResult(context.Background(), "L3Test", asyncResponseC, cs.loadStopC())
			resultC <- asyncResponse{r: r, e: e}
		}()

		// Deliberately UNSYNCHRONIZED with the goroutine above - see the
		// file doc comment / the sibling ocpp1.6 file's mechanism note.
		want := e1cTestResponse{}
		asyncResponseC <- asyncResponse{r: want, e: nil}
		close(stopC)

		select {
		case res := <-resultC:
			require.NoErrorf(t, res.e, "iteration %d: expected the pre-delivered response, got error %v", i, res.e)
			require.Equalf(t, want, res.r, "iteration %d: expected the pre-delivered response", i)
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: awaitCtxResult did not return", i)
		}
	}
}
