# 10 — xBlaz3kx/ocpp-go deep comparison & adoption list

**Date:** 2026-07-02 · **Refs:** ours `origin/master` @ `50150fb` · theirs `xblaz/main` @ `f8e6269` (fetched as remote `xblaz`) · upstream `b22429c` (= merge-base, fully stale — 0 new commits since our fork base)

**Method:** 7 parallel area agents over the full `origin/master..xblaz/main` delta (73 non-merge commits, 447 files), adversarial verification of top candidates, coverage critic over the whole diff. All 73 commits accounted for.

---

## Headline conclusions

1. **Upstream is dead as a source; xblaz is the only living fork** (last commit 2026-07-01, tags to v0.24.0). But **nothing cherry-picks cleanly**: they renamed the module (`github.com/xBlaz3kx/ocpp-go`), bumped to `go 1.25`, and wove `pkg/errors`, `samber/lo`, `agrison/go-commons-lang`, and OTel/grpc/pyroscope into **core** files (`ocppj/server.go`, `ws/server.go`, dispatcher). Every adoption is a **line-level manual port**, never a file-level pick.
2. **Our fork is strictly ahead on concurrency correctness.** Their ws layer lacks our done-channel (their `WriteManual`/`Close` can block forever under RLock), locks inside `writePump` (writer-starvation deadlock pattern), has an unsynchronized client `webSocket` field, and their HEAD contains a live server `Stop()` deadlock (`defer s.connMutex.Lock()` at their `ws/server.go:396`). Their `-race` CI branch (`ci/test-with-race`) was **never merged** — their main doesn't run `-race`. Nothing in their thread-safety work catches anything our Phase 2a missed.
3. **Their flagship callback registry ≈ our hardened callback queue.** Registry keyed by `(clientID, requestID)` vs our ordered per-client slice: under the one-in-flight dispatcher invariant, observable behavior is identical; theirs breaks the ocppj public API (`SendRequest` returns `(uniqueId, error)`), loses FIFO ordering on disconnect drain, and their own unmerged `fix/callback*` branches show it needed repeated repair. Keep ours.
4. **The comparison did surface real gaps on OUR side** — the two P1 items below (ws logger race, dispatcher SendRequest-vs-Stop race) are fixes to *our* code, found by studying their diff. Both adversarially verified against our actual sources.
5. Treat their `fix:` commits skeptically: of those examined, a84f16d has a false premise + introduces a deadlock and a duplicate-ID window; their `Size()` "perf" fix iterates a live map outside the lock (fatal under load); the registry drain drops ordering.

---

## Prioritized adoption list

Effort: **S** < ½ day · **M** 1–2 days · **L** > 2 days (incl. our gate + review overhead).

### Tier 1 — do now (correctness / CI honesty, all S)

| # | Item | Verdict | Why | Effort notes |
|---|------|---------|-----|--------------|
| 1 | **Guard `ws` package logger global** (our-side gap) | adopt P1 | Our Phase 2a wrapped the ocppj `log` global in `atomicLogger` but missed the identical global in `ws` (`ws/websocket.go:45`, bare write in `SetLogger`, read unsynchronized from connection goroutines). `SetLogger` after `Start()` is a real data race. **Verified** line-by-line. | Copy the ~50-line `atomicLogger` pattern from `ocppj/ocppj.go`; zero API change; existing `-race` suite covers it. |
| 2 | **Dispatcher `SendRequest` guard vs `Stop()` race** | adapt P1 | Our `DefaultClientDispatcher.SendRequest` does `RLock; d.requestChannel <- true` with no running/nil check. Window after `Stop()`: send on closed channel (panic) or send on nil channel (blocks forever holding RLock → deadlocks later Start/Stop). The one genuine race in our dispatcher their diff points at. **Verified** (server-side variant overstated — our pump re-inits the queueMap; client-side real). | Few lines in `ocppj/dispatcher.go` + a `-race` regression test. Their code is only the pointer — write our own guard. Sequence after the uncommitted local dispatcher edits land. |
| 3 | **CI failure-masking fix in `docker-compose.test.yaml`** | adopt P2* | Our `unit_test` service runs 3 × `go test` + 2 × `sed` in one block without `set -e`: a failing `go test` is masked by the trailing seds → container exits 0 → **Test workflow goes green on broken tests and publishes their coverage to goveralls**. Only the separate Gate workflow saves us. **Verified** (integration job is NOT masked — single command + `--abort-on-container-exit` propagates). | Add `set -e; set -o pipefail` to the bash block (+ `--exit-code-from` in Makefile/workflow invocations). Do NOT copy their compose files (xBlaz3kx coverpkg paths, drops ocpp1.6_test). Budget for the possibility it surfaces a hidden red test. |
| 4 | **Run `go test ./... -race` in public CI** (idea from their unmerged `ci/test-with-race`) | adapt P2 | We paid for the full `-race` cleanup in Phase 2a but `gate.yaml` still only race-*builds*, with a stale "master has pre-existing races" comment. One line protects the whole investment publicly. (Ironically their own branch was never merged — we'd be ahead of them here too.) | One-line change to `.github/workflows/gate.yaml` + delete stale comment. Watch shared-runner timing on the ws suite for a few runs. |

\* #3 was analyst-P1, verifier-P2; kept in Tier 1 because it's trivially cheap and gates everything else we port.

### Tier 2 — cheap, real value (S each; batch as one PR)

| # | Item | Verdict | Why | Effort notes |
|---|------|---------|-----|--------------|
| 5 | **Opt-in websocket compression** (permessage-deflate) | adopt P2 | Server gains `WithCompression` ServerOpt (upgrader currently has NO compression knob — unreachable today); client gets sugar over existing `AddOption`. OCPP-J JSON (MeterValues bursts) compresses well — relevant for charge points on cellular. Default off; gorilla v1.5.3 identical on both forks. | ~20 lines in `ws/server.go`/`ws/client.go`, no new deps, no directive bump. Write a REAL echo test with compression on both ends (their test only checks the handshake). Don't copy their `NewServer` restructuring. |
| 6 | **`dispatchNextRequest` nil/cast guards** (client + server dispatchers) | adapt P2 | Found by their benchmarking: if `Peek()` returns nil in a race window, the blank type-assert yields a zero `RequestBundle` → panic on `bundle.Call.UniqueId`. Our `CompleteRequest` guards nil; `dispatchNextRequest` doesn't. Unproven reachability in our net state (hence not P0) — cheap insurance for an unattended charge point. | ~15 lines/function, written fresh (their patch silently returns; pair ours with `Errorf` + think through `readyForDispatch`). |
| 7 | **Concurrency tests for `internal/callbackqueue`** | adapt P2 | Their registry ships a 741-line suite incl. concurrent hammer tests; our hand-rolled queue has 93 lines, single-threaded. Port the test *shapes* (N goroutines TryQueue/Dequeue/drain racing per client; rollback under concurrent failures) onto our semantics, under `-race`. | Test-only, ~150–250 lines. Worst case it finds a real bug — that's the point. |
| 8 | **`ocpp` package Profile unit tests** | adapt P2 | Table-driven suite for `NewProfile/AddFeature/SupportsFeature/GetFeature/ParseRequest/ParseResponse` — a package we have zero direct tests for. **Reject** the companion breaking change (unexporting `Profile.Features`). | In-package file, no import rewrite; swap ~6 `features` → `Features` refs. Land the in-flight ocpp.go error-marker work first, then extend suite to cover it. |
| 9 | **ocppj micro-benchmarks** (queue/state/dispatcher) | adopt P2 | We have zero benchmarks; these cover exactly the machinery we keep hardening — before/after numbers for Phase 3. All helper symbols exist in our tree (upstream heritage). | 3 test files: import rewrite + 2 constructor-arg deletions. Bench of Start/Stop cycles may stress our Stop-joins-pump path — treat findings as signal. |
| 10 | **Lean dependency bumps** (logrus 1.9.4, testify 1.11.1, validator.v9 9.31, universal-translator 0.18.1, env 11.4) | adopt P2 | No CVEs forcing it, but logrus 1.9.x fixes Entry data races, and currency shrinks future diff noise. **Verified empirically: no `go 1.16` directive bump forced** (dep directives constrain the toolchain, not our directive). | `go get` + `go mod tidy` + full gate incl. both consumers. testify 1.9→1.11 tightened some assertion semantics — run everything. |
| 11 | **`ServerQueueMap.Size()/SizePerClient()`** (rewrite, not port) | adapt P2 | The one dep-free extractable from their otel work: per-charge-point outgoing-queue depth for the CSMS's own metrics. **Their impl is racy** (iterates live map outside lock) and uses `maps.Copy` (needs go ≥1.21). | ~40 lines written fresh, holding RLock across iteration. Interface addition breaks external implementers — acceptable for pinned consumers. Only do it when the server product actually wants the metric. |

### Tier 3 — worthwhile later / on demand

| Item | Verdict | Notes |
|------|---------|-------|
| **`ocpp1.6/config_manager` port** | keys.go: adopt P2/S · store/manager: adapt P2/M | Typed config-key constants + mandatory-key sets per profile are clean (zero deps). The manager needs surgery: strip `samber/lo` (~5 call sites → loops) + `go-commons-lang` (1 `IsEmpty`), fix a dead-error defer + their `// todo`s, and note it's NOT wired to `OnGetConfiguration`/`OnChangeConfiguration` (xblaz's own example never uses it; no `GetConfigurationMaxKeys`/`ConfigurationStatus` mapping). Port only if the charge-point product wants to replace its app-level config handling. |
| **Metrics-hooks interface seam** | defer P2/M | The *idea* from their otel work: tiny `OnClientConnected/OnMessageSent(...)` hook interface, no-op default, consumers plug their own stack. Wait for an actual consumer requirement; watch callback-under-lock hazards. |
| **Benchmark-regression CI** | defer P3/M | github-action-benchmark plumbing; took them 8 fixup commits to stabilize. Only after #9 proves useful. |
| **Mockery v3 migration** | defer P3/M | Theirs is **broken**: their `.mockery.yml` keys resolve to upstream v0.19.0 module-cache source, so all 57 generated mocks mock upstream interfaces, not their fork's (their autogen CI never validated it). Ours (curated v2.51.0 config) is healthier. Revisit only when mockery v2 ages out. |

### Skip (decided, with reasons)

- **Callback registry** (their #57 flagship) — equivalent-or-worse vs our hardened queue; breaks ocppj API; loses drain ordering; their own fix branches show instability. Our queue stays.
- **Their ws thread-safety net state** — strictly weaker than ours (no done channel, pump-under-lock, unsynchronized client socket, upstream Stop bugs we already fixed). Never port ws files wholesale.
- **Connection-removal fix (a84f16d)** — false premise (delete already existed), plus their HEAD carries a `defer s.connMutex.Lock()` server-Stop deadlock and an eager-delete duplicate-ID window that cuts against our post-#376 reject-duplicate stance.
- **Constructor `(T, error)` refactor + struct-logger refactor** — breaking for a drop-in soft fork; the panics "fixed" are dead/misuse-only in our tree; the only real error path exists to serve their otel dep; entangled with logger params. Our race-safe `atomicLogger` keeps the upstream API.
- **`Size()` perf change as-committed** — imports a fatal concurrent-map-iteration race (see #11 for the rewrite).
- **Server/client state read-lock work** — superseded; their net state regressed to coarser locks than ours.
- **Full OTel + pyroscope + k6 + LGTM stack** — dep policy (otel×5, grpc, pyroscope); library stays lean.
- **Suite-assertion sweep, CI caching, repo chores, docs restructure** — churn, no behavior.
- **`feat/ocpp-21` (OCPP 2.1)** — WIP, never merged into their own main, +15k lines, no consumer need.

---

## Cross-cutting port rules (from the coverage critic)

1. **Every** xblaz file pick must be audited for: module rename, `pkg/errors` (archived; use `fmt.Errorf %w`), `samber/lo`, `go-commons-lang`, otel imports, constructor churn (logger params, `(T, error)` returns, `SendRequest → (string, error)`).
2. **`go 1.16` directive gate:** their code uses `maps.Copy` (go1.21) and generics-era stdlib; port by rewrite, or make the directive bump a deliberate standalone decision. (Our tree already needs toolchain ≥1.19 for `atomic.Bool` — directive and toolchain are separate questions.)
3. Their diff splits `ocppj/dispatcher.go` into two files — **diff functions, not files**.
4. ~11 unmerged `fix/*` branches on their side (callback locking, nil checks, state locks) are unfinished work — check them again in 6 months for anything that graduates to main.

## Suggested execution

Tier 1 (#1–4) as one small "hardening + CI honesty" PR — ~1 day including gate.
Tier 2 (#5–10) as a second PR — ~1.5–2 days. #11 and Tier 3 on consumer demand.
