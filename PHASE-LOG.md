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

## External code reviews (read-only, independent)

Two external CLIs reviewed the Phase 0+1 diff against `master` (read-only; **no code changed**):

- **Gemini CLI** — verdict **"Sound."** No regressions; confirmed the #399 ⊕ #412 union, the #396
  subsumption, #414 channel ordering, and the #391 atomics. Surfaced the `DefaultClientDispatcher`
  nil-deref now logged as Phase-2 item 6.
- **Codex CLI** (`codex exec`, read-only sandbox) — **no findings against this branch.** Independently
  confirmed: the dispatcher union completes the timeout **inline** (`q.Pop` + `DeletePendingRequest` +
  `clientQueue=q` + `rdy=true`) instead of calling `CompleteRequest`, whose `readyForDispatch` send
  (`dispatcher.go:694`) would self-deadlock `messagePump`; the nil guards are ordered before the
  `bundle.Call`/`bundle.Data` derefs with `continue` preventing stale fallthrough; the #396 skip; the
  `ws/websocket.go` cleanup ordering (close `done` before the exclusive lock — `websocket.go:384` — with
  single-owner cleanup so no new double-close); and the #391/#413 fixes. It corroborated that the residual
  `-race` failures are **pre-existing and unconnected to this branch**, explicitly naming Phase-2 items
  **1** (dispatcher goroutine join / test mocks), **2** (logger guard), and **3** (ws `errC` sync).

**Three independent reviews — adversarial workflow + Gemini + Codex — agree:** no regression introduced, and
the Phase-2 candidates below are confirmed-real and sound to tackle once Phase 0/1 is in place.

## Residual findings — out of Phase-1 scope (Phase-2 candidates)

`go test ./... -race` remains red due to **pre-existing** issues, none introduced by Phase 0/1 (reproduced at
baseline), and none addressed by the 6-PR Phase-1 set. Items 1–3 were independently corroborated by both
external reviews; 4–6 are latent/robustness items surfaced during review:
1. **ocppj test-lifecycle races** *(Gemini + Codex confirmed)* — a dispatcher `messagePump` goroutine outlives
   its test and races the next test's `SetupTest` on mock counters / shared suite state. Fix: make dispatcher
   `Stop()` synchronously join its goroutine (e.g. a `sync.WaitGroup` / done-ack on the pump), and/or make the
   test mocks goroutine-safe.
2. **ocppj `log` global race** *(Gemini + Codex confirmed)* — `SetLogger()` (ocppj.go:43) writes the package
   global `log` while dispatcher goroutines read it (`TestLogger`). Not covered by #391 (which only atomicized
   validation/escape). Fix: guard `log` behind a mutex or store it in an `atomic.Value`/`atomic.Pointer`.
3. **ws library races** *(Gemini + Codex confirmed)* — `client.errC` lazy init/read (Errors() write at
   client.go:399 vs readPump `error()` read at client.go:426), and `webSocket.run/readPump/writePump` field
   access. Separate from the #414 deadlock fix. Fix: synchronize the error-channel state (init under lock /
   guard the field) and audit the `webSocket` field accesses.
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
fork**; library `go test -race` red only on the pre-existing, out-of-scope races above (Phase-2). **Three
independent reviews — the adversarial multi-agent workflow plus the Gemini and Codex CLIs (both read-only) —
found no introduced regression** and corroborated the Phase-2 candidate list.

**Two environment realities** the investigation hadn't captured (it never ran `-race` on the full suite and
never reached `TestWebSockets`): the `-race` baseline was already red (pre-existing races, not the assumed
Phase-1 targets), and `ws` integration tests need port `8887` free (a running `csms` blocked them; resolved by
freeing the port). Both handled per owner direction.

**Not pushed.** (Phase 2a below was executed subsequently; Phase 2b rework / Phase 3 breaking not started.)

---

# Phase 2a — `-race` cleanup (the residual pre-existing races)

Goal: drive `go test ./... -race` to **0 data races** (the bar deferred from Phase 1), without
changing exported API or introducing deadlocks. The races were pre-existing (reproduced at the
Phase-1 baseline) — `ocppj` 10 race blocks, `ws` ~25 — split across library globals/lifecycle and
race-prone test harnesses. **Branch `fork-main`, local commits only — not pushed.**

Gate cadence: library `go build`/`go vet`/non-race `go test`/`-race` after each change; full
`scripts/gate.sh` (both consumer projects) at the end.

| # | Commit | Change | Kind |
|---|---|---|---|
| 2a.1 | `6a39af1` | `ocppj` package logger `log` is now a stable `*atomicLogger` whose delegate swaps under a `RWMutex` (non-generic — stays on the go 1.16 directive); all `log.Xxx` call sites unchanged. Fixes the `SetLogger`-vs-read race. | library |
| 2a.2 | `8b5dc82` | `ocppj` `DefaultServerDispatcher` **and** `DefaultClientDispatcher` `Stop()` now JOIN the `messagePump` goroutine (close stop signal, capture `doneC`, release mutex, `<-doneC`; pump does `defer close(doneC)`) so no goroutine outlives `Stop()`. Plus a client-timeout nil-guard (skip cancel when `Peek()` is nil/non-bundle) — the client-side analog of #399. | library |
| 2a.3 | `abc8a12` | `ocppj` test synchronization: `ServerDispatcherTestSuite.TearDownTest` stops the dispatcher (joined) so its goroutine can't race the next `SetupTest`; channel hand-off of `callID`; atomic `sentMessages`; goroutine-local `err` in parallel-send loops. | test |
| 2a.4 | `0aa358b` | `ws` data races eliminated: eager-init `errC` + non-blocking guarded send + close-once (`errClosed`, never niled → no send-on-closed/double-close); new client `RWMutex` guarding `webSocket`/`errC`; server lifecycle fields (`httpServer`/`addr`/router) guarded, lock released before `Serve`/`Shutdown`; `getReadTimeout`/`writePump` read `cfg`/`log` under `RLock`. **`Stop()` made concurrency-safe** (replaced the `reconnectC` check-then-send — two concurrent `Stop()`s could deadlock — with a non-blocking send). `Errors()` docs updated for eager/lossy semantics. No exported API change; #414 cleanup ordering intact. | library |
| 2a.5 | `b514e41` | `ws` test synchronization (TearDownTest drain, dedicated channels vs shared counters, cfg under mutex, goroutine-local err, connection-count under `connMutex`). | test |
| 2a.6 | `ac6cbcc` | `ws` tests bind an **OS-assigned free port** (`serverPort` is now a var probing `:0`) so `-race` runs reliably without freeing 8887 (no more csms collision, no cross-run bind clashes); hardened two pre-existing flakes (nil `*CloseError` guard in `TestUnsupportedSubProtocol`; buffer the unread `disconnectedServerC` to `numClients`). | test |
| 2a.7 | `af5df88` | `scripts/gate.sh`: drop the vendor dir in the disposable scratch copy so a consumer that now **vendors** the fork is built/tested against the live fork tree (fixes "inconsistent vendoring"). | ci |
| 2a.8 | `8a26827` | Doc: `Stop()` is blocking/idempotent and must not be called from an `onRequestCancel` callback (review note). | docs |

## Phase 2a gate — final state

| Target | build | vet | `go test` (non-race) | `go test -race` |
|---|---|---|---|---|
| **library** | ✓ | ✓ | ✓ | ✅ **0 races** — `ocppj` (×3), `ws` (×5, dynamic port, csms still up), `ocpp1.6_test`/`ocpp2.0.1_test` |
| **CSMS consumer** (replace→fork) | ✓ | ✓ | ✓ | ✅ green |
| **simulator consumer** (replace→fork) | ✓ | ✓ | ✓ | ✅ green |

`scripts/gate.sh` (both projects via `GATE_PROJECT_DIRS`): **GATE: ALL GREEN** — and now runnable with csms running, thanks to the dynamic test port.

## Adversarial review (read-only, multi-agent)

- **`ws` concurrency fix — 3 reviewers** (deadlock / correctness / behavior+API): no deadlock introduced (lock-ordering sound; never holds a lock across a channel send or `Serve`/`Shutdown`); no exported-API change; #414 ordering preserved; data races genuinely removed (baseline 23–26 → 0). Surfaced and FIXED a pre-existing concurrent-`Stop()` `reconnectC` deadlock and the eager/lossy `Errors()` doc gap.
- **`ocppj` `Stop()`-join + log + nil-guard — 2 reviewers**: **SAFE** / **SAFE-WITH-NOTES**. The `<-done` join has no reachable deadlock — `Stop()` is never called from the pump or a cancel/handler callback (all callers are the external control goroutine), the pump always reaches `defer close(doneC)` (its only blocking ops drain during the join given the existing Stop ordering), double-Stop is guarded, and Stop-before-Start short-circuits before touching `doneC`. atomicLogger delegates correctly (defaults to VoidLogger); the client nil-guard only converts a latent nil-deref into a safe skip (mirrors the pre-existing server guard).

### Residual (not data races; out of `-race` scope)
- A consumer that calls `dispatcher.Stop()` from inside its own `onRequestCancel` callback would self-deadlock on the join — documented (2a.8); unreachable via the library's own OCPP client/server.
- Pre-existing ws test-suite flakiness beyond the two hardened cases (sleep-based startup sync) can still theoretically flake under heavy `-race` load; not observed across the runs above.

**Net: `go test ./... -race` is green for the library and both consumer projects. Phase 2b (rework PRs
#406/#191/#387/#376) executed next; Phase 3 (breaking #373/#343) not started.**

---

# Phase 2b — rework PRs (selective adoption)

These four PRs were flagged in the plan as needing rework (selective cherry-pick / rebase / rewrite /
optional). After inspecting each against current master, the outcome was **adopt 2, skip 2** — the two skips
are justified, not deferrals of value. **Branch `fork-main`, local commits only — not pushed.**

| PR | Decision | Commit | Author | Notes |
|---|---|---|---|---|
| **#406** callback-queue panic | **ADOPTED (selective core)** | `3a39a23` | **acv (qosmotec)** | Took only the real fix — key callbacks by request type (id + feature name) so a delayed/timed-out response can't dequeue & type-assert against the wrong request's callback (interface-conversion panic); empty-queue returns `(nil,false)` instead of panicking; the 4 endpoint callers pass the feature name. **Excluded** the bundled scope creep: the ocpp1.6 SampledValue/MeterValue validation loosening (breaks 3 tests), the client dispatcher timeout-tick change (24h→2min), the `AddPendingRequest` signature change, and new `ocppj` panics. `internal/callbackqueue` is internal, so no exported-API impact. |
| **#376** duplicate-connection behavior | **ADOPTED (opt-in, safety-adapted)** | `ba29636` | **trond nordheim** | `SetDuplicateConnectionBehavior(KeepCurrent\|KeepNew)`; default `KeepCurrent` keeps existing behavior. Adapted for safety: the displaced connection is captured under `connMutex` but **Closed after releasing the lock** and via the webSocket's concurrency-safe `Close` (not its raw connection); `handleDisconnect` now deletes map entries **by identity** so a stale disconnect can't evict the replacement. KeepNew integration test omitted (two same-ID auto-reconnecting clients = inherent reconnect ping-pong, unstable under `-race`); default path stays covered by `TestClientDuplicateConnection`. |
| **#387** connection removal | **SKIPPED** | — | (xBlaz3kx-adjacent) | Redundant: current master already removes connections on disconnect (`handleDisconnect` `delete(s.connections, …)` under the lock). The PR's only net change is a redundant immediate-delete plus a `defer s.connMutex.Lock()` (should be `Unlock`) **deadlock bug**. Net value ≈ nil. |
| **#191** UseNumber JSON decode | **SKIPPED / deferred** | — | — | Would prevent float64 rounding of integers **>2⁵³** in the generic parse — *theoretical* for OCPP (realistic meter Wh ~10⁹ ≪ 9×10¹⁵). Needs a real rebase of the hot `ParseMessage`/`ParseRawJsonMessage` path (`Unmarshal`→`Decoder.UseNumber`, `arr[0].(float64)`→`json.Number`); hot-path regression risk outweighs the theoretical payoff. Recorded as a deferred item. |

## Phase 2b gate — final state

`scripts/gate.sh` (`GATE_PROJECT_DIRS` = both consumers): **GATE: ALL GREEN** — library build/vet/`-race`
(`ocpp1.6_test`/`ocpp2.0.1_test`/`ocppj`/`ws`) and **both consumer projects build/vet/test/`-race`** against the
fork. #406 changed the `internal/callbackqueue` API used by the endpoints; the consumers compile + pass against
it. `go test ./ws/ -race` is reliably green at `-count=1`.

### Residual (pre-existing, not introduced by Phase 2)
- `go test ./ws/ -race -count=2+` can **intermittently** fail with "connection refused" (0 data races) — the
  pre-existing sleep-based-startup + shared-port harness fragility. The OS-assigned port (2a.6) removed the csms
  collision but each test still reuses one port with a 100ms sleep before dialing. **Proper fix (deferred):** give
  each test its own ephemeral port and wait on `server.Addr()` instead of sleeping (~25 call sites). Does not
  affect the `-count=1` gate.

**Phase 2 complete: 2a (`-race` cleanup) + 2b (#406 selective, #376 opt-in; #387/#191 skipped with rationale).
Not pushed. Phase 3 (breaking #373/#343) not started.**
