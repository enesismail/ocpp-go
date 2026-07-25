package ocpp16

// PR-L1 (tasks/facade-lifecycle-hardening.md §2, §Tests item 7) RED-FIRST
// white-box unit test - the L3 residual guard.
//
// This file lives in `package ocpp16` (not `ocpp16_test`), alongside
// context_awaitresult_test.go, for the same reason that file does: it needs
// the unexported chargePoint.awaitCtxResult helper and the package-level
// asyncResponse type.
//
// Spec §Tests item 7's ⚠ warning: a black-box (facade-level) test cannot
// reliably distinguish "the response was lost to the L3 tie this guard
// fixes" from "the response was legitimately dropped by an L2-preemptible
// producer racing Stop()" - both look identical from the outside ("client
// stopped" instead of the response). This white-box test sidesteps that
// ambiguity by calling awaitCtxResult directly with hand-built channels and
// racing the write/close against the goroutine's OWN startup - see the
// mechanism note below for why this must be a genuine, unsynchronized race
// rather than a "write then close back-to-back" sequence.
//
// MECHANISM (empirically verified, not just theorized - see the PR-L1
// test-writing task's final report for the experiment): Go's select, once a
// goroutine is actually PARKED waiting on it, resolves the wakeup as a
// "first committer wins" CAS race, NOT a fresh pseudo-random re-pick among
// now-ready arms. So writing to asyncResponseC and then closing stopC
// back-to-back, from the SAME goroutine, AFTER first confirming (via a
// handshake channel or bounded yields) that the awaiting goroutine is
// already parked, deterministically makes the asyncResponseC arm win EVERY
// TIME - it was already registered as the winner before stopC's close ever
// reaches it. That approach was tried first and empirically measured at
// 0/20000 stopC wins - it cannot exercise this bug at all.
//
// The uniform-pseudo-random selection Go's spec describes only applies when
// select's FIRST (non-blocking) readiness scan already finds multiple ready
// cases - i.e. when asyncResponseC's send and stopC's close both complete
// BEFORE the awaiting goroutine's select statement evaluates them, not
// after it has already parked. Reaching that requires a genuine,
// unsynchronized scheduling race between this goroutine spawning +
// reaching its select, and the racing goroutine's write+close - which is
// exactly what this test does (no handshake, no yield: spawn, then
// immediately write and close). Empirically this wins for asyncResponseC
// the overwhelming majority of the time regardless of the fix (Go
// goroutines start fast), but a genuine tie - the awaiting goroutine's
// pre-check finding asyncResponseC empty and its second select's first scan
// then finding BOTH ready - occurs often enough to observe reliably across
// many iterations, ESPECIALLY under `-race` (which was empirically ~10-20x
// more likely to expose it in the isolated experiment, presumably from the
// added per-access instrumentation widening the scheduling window). Per
// spec §Tests: "-race -count=3 for the timing-sensitive ones" - run this
// test that way for a meaningful signal; a plain, non-race run may
// occasionally show zero stopC wins even against unfixed code.
//
// RED (today): awaitCtxResult's stopC arm returns the "stopped" error
// unconditionally, with no re-check of asyncResponseC first - so an
// iteration that lands the genuine tie fails. GREEN once spec §2 item 4's
// re-check lands: the stopC arm always finds (and returns) the ready
// response instead, on every iteration, regardless of timing.
//
// There is deliberately NO companion black-box test. One was written using the
// testhooks.ChargePointResponse seam and then DELETED: the hook fires before the
// forwarding closure's own preemptible select (v16.go), so even a
// hook-synchronised delivery must still survive two legitimate stopC drop points
// (the closure arm, then the handler arm before the callback writes
// asyncResponseC) - making "L3 tie lost" and "L2 drop working as intended"
// observationally identical end-to-end. It was measurably order-dependent
// (failed in-suite, passed 5/5 isolated). This white-box test is the sole
// item-7 coverage for this facade, which is spec §Tests item 7's own escape
// hatch.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// HIT RATE - read before changing the iteration count or trusting a green run:
//
//   - Under -race the tie lands roughly 1 iteration in 6000-8000, so a
//     30000-iteration run expects ~4-5 hits and `-race -count=3` expects ~12.
//     That is the GATE and it is reliable.
//   - WITHOUT -race the tie is ~10-20x rarer; a plain green here proves
//     NOTHING. Do not treat plain `go test` as coverage for this defect.
//
// Why it cannot be made deterministic: the tie requires the select to FIRST
// EVALUATE with both arms already ready. Any handshake that confirms the
// goroutine is parked before racing write-vs-close makes the response arm win
// 100% of the time - a parked-select wakeup resolves first-committer-wins, it
// does not re-pick randomly (measured 0/20000). And pre-loading asyncResponseC
// with stopC already closed does NOT work either: awaitCtxResult's
// non-blocking pre-check consumes the response before the second select is
// ever reached, so the stopC arm under test never runs. Hence the
// unsynchronized race plus a high iteration count.
func TestL1L3ResidualStopCArmRechecksAsyncResponseC(t *testing.T) {
	const iterations = 30000
	for i := 0; i < iterations; i++ {
		cp := &chargePoint{}
		stopC := make(chan struct{})
		cp.storeStopC(stopC)
		asyncResponseC := make(chan asyncResponse, 1)
		resultC := make(chan asyncResponse, 1)

		go func() {
			r, e := cp.awaitCtxResult(context.Background(), "L3Test", asyncResponseC, cp.loadStopC())
			resultC <- asyncResponse{r: r, e: e}
		}()

		// Deliberately UNSYNCHRONIZED with the goroutine above - see the
		// file doc comment's mechanism note for why any synchronization
		// here (even a handshake confirming it has started) would bias the
		// race away from ever exercising the bug.
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
