# Fork development — Phase 0 + Phase 1 execution log

Branch: `fork-main` · base: upstream master `b22429c` + fork README/merge (`7b22c26`).
Authorship preserved via `git cherry-pick` (original author retained); fork-original
fixes authored by the repo owner. **Nothing pushed** — local commits only.

Gate checklist (run after each step):
`go build ./...` · `go vet ./...` · `go test ./...` · `go test ./... -race` ·
both consumer projects build+vet+test+`-race` against the fork (via `scripts/gate.sh`, paths supplied through `GATE_PROJECT_DIRS`).

> **Two facts the upstream investigation (`../ocpp-go1/findings`) did not capture**, because it
> never reached them: it ran `go test ./...` **without** `-race`, and the old `ws` TearDownSuite
> panic crashed the test binary during `TestNetworkErrors`, so `TestWebSockets` never ran.
>
> 1. **`go test ./... -race` is red at baseline** — pre-existing data races in `ocppj` and `ws`
>    that *are* the Phase-1 targets (#412/#399/#396/#391, and ws concurrency for #414). Per owner
>    decision, **`-race` green is the post-Phase-1 target**; the Phase-0 green bar is build + vet +
>    non-race tests.
> 2. **`ws` integration tests bind `serverPort = 8887`.** A running `csms` on 8887 makes them 404 and
>    hang. Resolved by freeing the port (owner stopped `csms`); with 8887 free `go test ./ws/` is green.

## Phase 0 — Foundation

| # | Commit | Change | Author credited | Gate result |
|---|---|---|---|---|
| 0.1 | `b811abc` | `fix(ws/mocks)`: delete 4 stale mocks (`mock_WsServer/WsClient/ClientOpt/ServerOpt`) referencing removed `ws.WebSocket` | repo owner (fork-original) | `go build ./...` exit 0 ✓ |
| 0.1b | `326ff0d` | `fix(ocppj/mocks)`: drop unused `MockMessage` (mockery expecter `MarshalJSON` tripped vet `stdmethods`; latent since v0.19.0) | repo owner (fork-original) | `go vet ./...` clean ✓ |
| 0.2 | `ab0ad78` | GetCompositeSchedule `StatusInfo` nil-deref: attach `StatusInfo` to mocked confirmation (cherry-pick of #381 `d66986eb35`; **not** #418) | **dwibudut** (#381) | `ocpp2.0.1_test` green ✓ |
| 0.3 | `239b100` | `test(ws)`: probe toxiproxy in SetupSuite + `Skip` when unreachable; nil-guard TearDownSuite — clean skip instead of panic | repo owner (fork-original) | `TestNetworkErrors` SKIP, no panic ✓ |
| 0.4 | `923f1a1` | `ci`: add `scripts/gate.sh` (library + replace-based project gate via disposable scratch copies) and `.github/workflows/gate.yaml` | repo owner (fork-original) | harness runs; projects green ✓ |

(Earlier: `1110bd4` `chore: ignore tasks/ and .DS_Store`.)

## Phase 0 gate — baseline captured BEFORE any Phase-1 fix

| Target | `go build` | `go vet` | `go test` (non-race) | `go test -race` |
|---|---|---|---|---|
| **library** | ✓ exit 0 | ✓ clean | ✓ green (toxiproxy skipped) | ✗ **34 race warnings** in `ocppj` + `ws` (Phase-1 targets); `ocpp1.6_test`/`ocpp2.0.1_test` pass |
| **CSMS consumer project** (replace→fork) | ✓ | ✓ | ✓ `internal/csms` ok | ✓ green |
| **simulator consumer project** (replace→fork) | ✓ | ✓ | ✓ orchestrator/ocpp16/scenario/statemachine/tests ok | ✓ green |

Both sibling repos verified pristine after the gate (no `replace` left in their `go.mod`; scratch copies removed).

Baseline `-race` race sites (the oracle Phase 1 must drive to green):
`DefaultServerDispatcher` Start/messagePump/dispatchNextRequest, client dispatcher, `serverState`/`clientState`
locking, global `validationEnabled`/`EscapeHTML`/`Validate`, and `ws` connection write/cleanup.

## Phase 1 — Verified-clean stability fixes (in order; gate after each)

Gate cadence: **library gate (build/vet/non-race `go test`/`-race` delta) after every fix**;
**full project gate (`scripts/gate.sh`, both projects) at the ws boundary (#414) and as the final
gate** — none of #412–#396 change exported API, so the projects' compile/unit results don't vary
between the internal ocppj fixes. Non-race `go test ./...` must stay green after every fix.

| # | PR | Commit | Author | Conflict | Gate |
|---|---|---|---|---|---|
| 1.1 | #414 ws write/cleanup deadlock | `96bd223` | **shiv3** | clean | build/vet ✓ · non-race ✓ · projects ✓ · `-race`: pre-existing ws races remain (see note) |
| 1.2 | #412 server-dispatcher deadlock | `f1416b5` | **Jacob Smullyan** | clean | build/vet ✓ · non-race ✓ · regression test `TestServerDispatcherConcurrentTimeoutDeadlock` passes · `-race`: ocppj races persist (see note) |
| 1.3 | #413 nil-profile guard (CALL_RESULT) | `2bd568b` | **Rishabh Vaish** | clean | build/vet ✓ · non-race ✓ · new test `TestParseMessageCallResultUnsupportedProfile` passes |
| 1.4 | #399 dispatcher nil-checks | `71538f6` | **xBlaz3kx** | **resolved** in `dispatcher.go` `messagePump` (union: #399 nil-guards + #412 inline completion; `GetUniqueId()`) | build/vet ✓ · non-race ✓ · `TestServerDispatcherConcurrentTimeoutDeadlock` + `TestServerDispatcherTimeout` pass |
| 1.5a | #391 make validation atomic | `f389d7c` | **xBlaz3kx** | clean | build/vet ✓ · non-race ✓ |
| 1.5b | #391 make html escaping atomic | `7bbb74e` | **xBlaz3kx** | clean | build/vet ✓ · non-race ✓ · `TestValidationSuite` race-clean under `-short -race` |
| 1.6 | #396 read-lock state locking | — **SKIPPED** | xBlaz3kx (credited) | **subsumed by #412** | cherry-pick is empty: #412's `state.go` already contains all of #396's `Lock→RLock` conversions (a strict superset, incl. the `GetClientState` fast-path). No commit created; the optimization is present via #412. |

> **Important `-race` finding (revised).** On inspection, the baseline `-race` warnings are **not** what the
> 6 Phase-1 PRs fix. The PRs fix **deadlocks** (#414 ws, #412 dispatcher) and add **defensive guards/atomics**
> (#413, #399, #391) — correctness/liveness fixes validated by the **non-race** suite + their own regression
> tests, **not** by the race detector. The actual `go test ./... -race` warnings come from **pre-existing
> issues outside the PR set**:
> - **ws (library):** `client.errC` written by `Errors()` vs read by the readPump `error()` (client.go:400 vs :426); `webSocket.run/readPump/writePump` field access.
> - **ocppj (library global):** `log` written by `SetLogger()` (ocppj.go:43) vs concurrent reads (`TestLogger`).
> - **ocppj (test harness):** `MockWebsocketServer/Client` counters (ocppj_test.go:73/152/842/885/886) touched by a lingering dispatcher goroutine vs the next test's `SetupTest` — i.e. `Stop()` doesn't synchronously join its goroutine.
>
> None of these are addressed by #413/#399/#391/#396, so **`go test ./... -race` stays red after Phase 1**.
> They are logged as **Phase-2 candidates** (a `log`-atomic/locking fix, ws `errC` sync, and a synchronous
> dispatcher `Stop()` / goroutine-safe test mocks). #391 still correctly atomicizes the validation/escape
> globals (their race only manifests under `-short`, which gates `TestValidationSuite`).

## Phase 1 gate — final state

| Target | `go build` | `go vet` | `go test` (non-race) | `go test -race` |
|---|---|---|---|---|
| **library** | ✓ exit 0 | ✓ clean | ✓ green (toxiproxy skipped) | `ocpp1.6_test`/`ocpp2.0.1_test` pass; `ocppj`+`ws` show 34 **pre-existing** races (unchanged from baseline; see Phase-2 below) |
| **CSMS consumer project** (replace→fork) | ✓ | ✓ | ✓ | ✓ green |
| **simulator consumer project** (replace→fork) | ✓ | ✓ | ✓ | ✓ green |

Both sibling repos re-verified pristine after the final gate (no `replace` left; scratch copies removed).

## Adversarial verification (multi-agent workflow)

A 6-agent read-only review of the Phase 0+1 commits (conflict resolution, #396 subsumption, #414, #412,
#413/#391/Phase-0). **Overall: concerns-only — no introduced regression.** Two sub-reviewer "bug" verdicts
were investigated and found to be **false positives**, confirmed by direct code inspection and the `-race`
detector:
- **#414 readPump `forceCloseC` send** — not a send-on-closed-channel risk: the send runs under `w.mutex.RLock`
  guarding a `connection != nil` check, while `cleanup()` closes the channel and nils the connection under the
  exclusive `Lock`.
- **#412 `serverState.DeletePendingRequest` using `RLock`** — correct: that mutex only guards the
  `pendingRequestState` **map** (read here); the `clientState.requestID` mutation is independently protected by
  `clientState`'s own exclusive `Lock` (state.go:69). `-race` reports zero races touching `state.go`.

The conflict resolution (#399 ⊕ #412 union), #396 subsumption, and #413/#391/Phase-0 commits all hold up.

## Residual findings — out of Phase-1 scope (Phase-2 candidates)

`go test ./... -race` remains red due to **pre-existing** issues, none introduced by Phase 0/1 (reproduced at
baseline), and none addressed by the 6-PR Phase-1 set:
1. **ocppj test-lifecycle races** — a dispatcher `messagePump` goroutine outlives its test and races the next
   test's `SetupTest` on mock counters / shared suite state. Fix: make dispatcher `Stop()` synchronously join
   its goroutine, and/or make the test mocks goroutine-safe.
2. **ocppj `log` global race** — `SetLogger()` (ocppj.go:43) writes the package global `log` while dispatcher
   goroutines read it (`TestLogger`). Not covered by #391 (which only atomicized validation/escape). Fix: guard/atomic `log`.
3. **ws library races** — `client.errC` (Errors() write vs readPump error() read), `webSocket.run/readPump/writePump`
   field access. Separate from the #414 deadlock fix.
4. **ws `forceCloseC` channel-send asymmetry** — reviewed as not a live bug, but the send pattern is not
   `done`-guarded like WriteManual/Close; a latent robustness item.
5. **`.mockery.yaml` stale `ws` section** — still lists the removed `WsClient`/`WsServer` interfaces; harmless
   for build (mocks are checked in) but should be reconciled when mock regeneration tooling is revisited.
6. **`DefaultClientDispatcher` nil-deref (`ocppj/dispatcher.go:208`)** — #399 added nil-guards to the *server*
   dispatcher's `messagePump`, but the *client* dispatcher's timeout path can still nil-deref
   `bundle.Call.UniqueId` if `el` / `bundle.Call` is nil. Pre-existing and a faithful match to upstream scope
   (not introduced here) — the client-side analog of #399's server-side guards. Surfaced by an independent
   Gemini CLI review of Phase 0+1.

## Summary

**Phase 0 (foundation) — done, committed:** restored `go build ./...` (deleted 4 stale ws/mocks), made
`go vet ./...` clean (dropped unused `MockMessage`), fixed the `ocpp2.0.1` GetCompositeSchedule `StatusInfo`
nil-deref (cherry-pick #381/dwibudut), made the `ws` toxiproxy suite skip cleanly instead of panicking, and
added the validation gate (`scripts/gate.sh` + `.github/workflows/gate.yaml`) with a captured baseline.

**Phase 1 (verified-clean stability fixes) — done, committed, authorship preserved:** #414 (shiv3), #412
(Jacob Smullyan), #413 (Rishabh Vaish), #399 (xBlaz3kx; conflict resolved as a union with #412), #391 (xBlaz3kx,
2 commits). **#396 skipped — fully subsumed by #412** (credit xBlaz3kx; the read-lock optimization is present
via #412).

**Gate result:** library build/vet/non-race tests green; **both projects build/vet/test/-race green against the
fork**; library `go test -race` red only on the pre-existing, out-of-scope races above (Phase-2). Adversarial
review found no introduced regression.

**Two environment realities** the investigation hadn't captured (it never ran `-race` on the full suite and
never reached `TestWebSockets`): the `-race` baseline was already red (pre-existing races, not the assumed
Phase-1 targets), and `ws` integration tests need port `8887` free (a running `csms` blocked them; resolved by
freeing the port). Both handled per owner direction.

**Not pushed. Phase 2 (rework) and Phase 3 (breaking) not started**, per scope.
