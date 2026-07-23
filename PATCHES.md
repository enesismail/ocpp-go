# Fork-local patches (divergence ledger)

This fork (`github.com/enesismail/ocpp-go`) carries deliberate edits on top of the
upstream lineage. They are **intentional, not cruft** — when a future upstream merge,
refactor, or re-vendor conflicts on these lines, keep them.

Each entry is guarded by a test so a silent drop turns into a **red build in this fork**
before it can propagate to a consumer. Keep this ledger in sync whenever a new
fork-local edit lands.

## Request-timeout sentinel

A local dispatcher request-timeout and a server-sent `CALLERROR` both otherwise surface
as `*ocpp.Error{Code: GenericError}` and are indistinguishable. A downstream consumer
relies on telling them apart (e.g. to attribute latency correctly), so the timeout error
carries an internal `Marker` tag that `errors.Is` can match.

| File:line | Symbol | Why keep it |
|-----------|--------|-------------|
| `ocpp/ocpp.go:41` | `Marker string` field on `Error` | carries the tag that makes a timeout error identifiable |
| `ocpp/ocpp.go:58-63` | `func (err Error) Is(target error) bool` | matches on `Marker`; returns false when the target has no marker, so it never over-matches |
| `ocppj/ocppj.go:22` | `const requestTimeoutMarker = "ocppj/request-timeout"` | the tag value |
| `ocppj/ocppj.go:25` | `var ErrRequestTimeout = &ocpp.Error{Marker: requestTimeoutMarker}` | the sentinel callers match against with `errors.Is` |
| `ocppj/ocppj.go:27` | `func newRequestTimeoutError(messageID string) *ocpp.Error` | constructs a tagged timeout error |
| `ocppj/dispatcher.go:293` | client request-timeout path builds `newRequestTimeoutError(bundle.Call.UniqueId)` instead of a bare `GenericError` | actually emits the tag on timeout |

**Guard:** `ocppj/request_timeout_test.go` asserts the *property* (a timeout matches the
sentinel; a plain `GenericError` CALLERROR and an untagged `Error` do not), so it survives
refactors but fails the moment the sentinel is dropped or `Error.Is` is re-flattened. It
runs under the race gate in CI (`.github/workflows/gate.yaml`, added in `0df5cca`).

> Line numbers are current as of the entries above; if the API moves, update this table
> and the guard test together. The property test is the real backstop — the line numbers
> are only a navigation aid.

## Local-transport / send-failure sentinel

A locally synthesized transport/send failure and a server-sent `CALLERROR` can both
surface as `*ocpp.Error{Code: InternalError}` or `*ocpp.Error{Code: GenericError}`.
The local transport marker keeps failed writes and disconnect drains distinguishable
from genuine peer `CALLERROR`s while preserving the existing error code and text.

| File:line | Symbol | Why keep it |
|-----------|--------|-------------|
| `ocppj/ocppj.go:48` | `const localTransportMarker = "ocppj/local-transport"` | the tag value for locally synthesized transport/send failures |
| `ocppj/ocppj.go:52` | `var ErrLocalTransport = &ocpp.Error{Marker: localTransportMarker}` | the sentinel callers match against with `errors.Is` |
| `ocppj/ocppj.go:55` | `func NewLocalTransportError(code ocpp.ErrorCode, description, messageID string) *ocpp.Error` | exported code-preserving constructor used by dispatcher write failures and facade disconnect drains |
| `ocppj/dispatcher.go:133` | `func markLocalTransportError(err *ocpp.Error) *ocpp.Error` | fail-safe default for any future local cancel path that forgets an explicit marker |
| `ocppj/dispatcher.go:146` | `func (d *DefaultClientDispatcher) fireRequestCancel(...)` | client cancel choke-point: nil-check, panic recovery, and local marker backstop stay structural instead of per-site convention |
| `ocppj/dispatcher.go:607` | `func (d *DefaultServerDispatcher) fireRequestCancel(...)` | server cancel choke-point: nil-check, panic recovery, and local marker backstop stay structural instead of per-site convention |
| `ocppj/dispatcher.go:352` | client write-failure path calls `NewLocalTransportError(InternalError, err.Error(), bundle.Call.UniqueId)` | preserves existing write-failure payload while tagging it as local transport |
| `ocppj/dispatcher.go:742` | server timeout path calls `newRequestTimeoutError(bundle.Call.GetUniqueId())` | client/server asymmetry fix: server timeouts now match `ErrRequestTimeout`, not `ErrLocalTransport` |
| `ocppj/dispatcher.go:802` | server write-failure path calls `NewLocalTransportError(InternalError, err.Error(), bundle.Call.UniqueId)` | preserves existing write-failure payload while tagging it as local transport |
| `ocpp1.6/central_system.go:508` | disconnect-drain callback uses `ocppj.NewLocalTransportError(ocppj.GenericError, "client disconnected, no response received from client", "")` | preserves the facade's existing disconnect error while tagging it as local transport |
| `ocpp2.0.1/csms.go:757` | disconnect-drain callback uses `ocppj.NewLocalTransportError(ocppj.GenericError, "client disconnected, no response received from client", "")` | preserves the facade's existing disconnect error while tagging it as local transport |

**Guard:** `ocppj/local_transport_test.go`, `ocpp1.6_test/local_transport_test.go`,
and `ocpp2.0.1_test/local_transport_test.go` assert the sentinel property and the
dispatcher/facade paths that must carry the marker, including the server timeout
asymmetry fix.

> Line numbers are current as of the entries above; if the API moves, update this table
> and the guard tests together. The property test is the real backstop — the line numbers
> are only a navigation aid.

## Sentinel version-parity guards (2.0.1 client facade)

Both sentinels above live in shared `ocppj` and are set on the shared client dispatcher
cancel path, so they are **version-agnostic** — a 2.0.1 `chargingStation` uses the same
dispatcher as a 1.6 charge point. There is no production line to keep here; this is a
**test-surface** guard that the 2.0.1 CLIENT facade preserves the markers end-to-end (a
future 2.0.1-facade refactor that reconstructs or strips the `*ocpp.Error` would otherwise
go uncaught).

**Guard:** `ocpp2.0.1_test/request_timeout_test.go` drives a `chargingStation.SendRequestAsync`
and asserts the callback error rides through unchanged — a dispatcher timeout matches
`ErrRequestTimeout` (and not `ErrLocalTransport`), and a local write failure matches
`ErrLocalTransport`. The 1.6 client facade is guarded by `ocpp1.6_test/local_transport_test.go`;
the server timeout is guarded at the ocppj layer (`ocppj/local_transport_test.go`).

## Inbound read limit

The `ws` layer exposes per-endpoint timeouts/auth/TLS but never bounded inbound message
size — nothing called gorilla's `conn.SetReadLimit`, so a single message was accepted at any
size (gorilla's default of 0 = no limit). This adds an **opt-in** per-message read limit so a
simulator/CSMS holding sockets to an untrusted peer can cap it. Default stays `0` (unlimited)
so behavior is unchanged unless the operator opts in. This is a **fork-original** ws-hardening
feature; upstream `ws` has no equivalent.

| File:line | Symbol | Why keep it |
|-----------|--------|-------------|
| `ws/websocket.go:131` | `ReadLimit int64` on `ServerTimeoutConfig` | public opt-in knob for inbound message size on server conns |
| `ws/websocket.go:183` | `ReadLimit int64` on `ClientTimeoutConfig` | public opt-in knob for inbound message size on client conns |
| `ws/websocket.go:272` | `ReadLimit int64` on internal `WebSocketConfig` | carries the limit from the timeout config to `newWebSocket`/`updateConfig` |
| `ws/client.go:380` | `wsCfg.ReadLimit = c.timeoutConfig.ReadLimit` before `newWebSocket` | threads the client knob without changing `NewDefaultWebSocketConfig`'s signature |
| `ws/server.go:483` | `wsCfg.ReadLimit = s.timeoutConfig.ReadLimit` before `newWebSocket` | threads the server knob without changing `NewDefaultWebSocketConfig`'s signature |
| `ws/websocket.go:425` | `if cfg.ReadLimit > 0 { w.connection.SetReadLimit(cfg.ReadLimit) }` in `updateConfig` | applies the limit at the single cfg→conn choke point; `> 0` gate keeps `0`/negative unlimited |

**Guard:** `ws/websocket_test.go` — `TestServerReadLimitExceeded` (server drops the over-limit
connection: proves the server call site threads the limit), `TestClientReadLimitExceeded`
(client surfaces `websocket.ErrReadLimit` on its disconnect handler), `TestServerReadLimitUnderLimitPasses`,
`TestReadLimitDefaultUnlimited` (default 0 delivers a large message unchanged), and
`TestClientReadLimitAppliesAfterReconnect` (a fresh dial re-applies the limit). All run under
the `-race` gate.

> Line numbers are current as of the entries above; if the API moves, update this table
> and the guard tests together. The guard tests are the real backstop — the line numbers
> are only a navigation aid.

## Server connection-lifecycle hygiene

`ws.server.handleDisconnect` used to `delete(s.connections, id)` **unconditionally** and fire
the disconnected-callback with no check that the entry was still this socket, so a stale socket
could emit a `disconnected` event observed *after* a newer `connected` event for the same ID —
making a live client look gone (the **reorder** class of upstream **#292**, evcc). This makes
removal + the callback **identity-guarded** ("delete-if-me"). Scope split, stated honestly:
- The **map-clobber** hazard (a stale socket deleting a newer same-ID entry) is **not reachable
  under the current reject-new policy** — the only deleter of an entry is `handleDisconnect`
  itself, once per socket, and a newer same-ID entry can only register after the old one is
  already gone. `delete-if-me` and the `!isCurrent` branch pin the invariant a future evict-old
  duplicate policy (**D2**, the reverted #376) requires; they are substrate, not a live fix.
- The **re-check before firing** is the branch with live value today: a reconnector that has
  finished its handshake can be parked at `connMutex.Lock` and insert between this socket's
  `delete` and its callback, so without the re-check the stale `disconnected` could still land
  after the newer `connected`.
- The duplicate-connection *policy* (reject-vs-evict, i.e. #314's half-open-reconnect case) is
  deliberately **unchanged** here — that user-visible symptom is D2, not S4.

| File:line | Symbol | Why keep it |
|-----------|--------|-------------|
| `ws/server.go:530-534` | `current, ok := s.connections[w.ID()]; isCurrent := ok && current == w` → `delete` only if `isCurrent` | delete-if-me: a stale/superseded socket must never remove a newer entry for the same ID. Unreachable under reject-new; the invariant an evict-old policy (D2) needs |
| `ws/server.go:536-542` | early `return` (+ `Debugf`) when `!isCurrent` | suppress the `disconnected` event for a socket already superseded/removed. Also substrate for D2 (unreachable under reject-new) |
| `ws/server.go:547-551` | re-check `_, superseded := s.connections[w.ID()]` before firing (outside `connMutex`) | the live-value branch: closes the delete→fire window where a lock-parked reconnector registers mid-`handleDisconnect`; the callback stays outside the lock so a handler may call `Write`/`GetChannel`/`StopConnection` without self-deadlock |

**Guard:** `ws/server_reconnect_test.go` — `TestHandleDisconnectSupersededSuppressed` + `TestHandleDisconnectDeleteIfMeNoClobber` deterministically cover the `!isCurrent` path (no clobber, no spurious event for a superseded socket); `TestHandleDisconnectNormalFiresOnce` guards against over-suppression (a normal disconnect still fires exactly once and drains the map). The second re-check branch (`:547-551`) is a documented, accepted belt-and-suspenders guard: it fires only when a reconnector already parked at `connMutex.Lock` inserts inside the small delete→re-check window, which is **not deterministically reproducible without a production test-seam** (the D2-time event-loop is the zero-window replacement). **Note for D2:** suppression means a consumer observes `connected(id)` without an intervening `disconnected(id)` — correct for the ID-keyed OCPP facades, but a consumer that *counts* connect/disconnect events would drift; inherent to any suppression design.

> Line numbers are current as of the entries above; if the API moves, update this table
> and the guard tests together. The guard tests are the real backstop — the line numbers
> are only a navigation aid.

## Duplicate-connection policy (evict-old)

This fork adds an opt-in websocket duplicate policy for the half-open reconnect class
tracked upstream as #314/#376: a new connection with the same charger ID may evict the
old websocket, but only after the old disconnect teardown has completed. Default behavior
remains reject-new (`KeepCurrent`). The evict-old policy depends on PR-0 dispatcher
token identity and delete acknowledgements, plus the S4 identity-guarded disconnect path.

| File:line | Symbol | Why keep it |
|-----------|--------|-------------|
| `ws/server.go` | `DuplicateConnectionPolicy`, `KeepCurrent`, `KeepNew`, `WithDuplicateConnectionPolicy` | public construction-time policy knob; default keeps existing reject-new behavior. The option godoc carries the security caveat that a valid/guessable ID can evict an active charger unless an auth gate proves ownership |
| `ws/server.go` | `WithDuplicateConnectionEvictionTimeout` and `duplicateEvictionTimeout` | construction-time latch timeout hook; production default is `WriteWait + 4s`, while tests can set a short bounded wait |
| `ws/server.go` | `gate map[string]int`, `registerNewConnection`, and the `handleDisconnect` gate increment/decrement | unified refcounted transition gate: rejects arrivals while a same-ID disconnect/eviction transition is in progress, covers both policies, and deletes gate keys at zero to avoid wedges/leaks |
| `ws/websocket.go` / `ws/server.go` | `webSocket.teardownDone`, `teardownOnce`, and the top-of-`handleDisconnect` latch close | per-socket teardown latch; the evictor waits outside `connMutex` until old disconnect cleanup, dispatcher delete, callback drain, and user disconnect handler have returned |
| `ws/server.go` | `handleMessage` currentness guard (`s.connections[w.ID()] == w`) | drops late inbound frames from a superseded old socket so old CALL_RESULT/CALL_ERROR frames cannot drain callbacks that belong to the replacement |
| `ocpp1.6/central_system.go` / `ocpp2.0.1/csms.go` | always-installed disconnect drain wrapper plus stored user handler field | facade callback queues drain on every disconnect even when the application did not register a disconnect handler; setters are still set-before-Start and now only store the user callback |

**Guard:** `ws/duplicate_policy_test.go` covers default reject-new, KeepNew eviction,
the natural-disconnect gate window, stale inbound drops, concurrent duplicate contenders,
barrier timeout fallback, and no-deadlock load. `ocpp1.6_test/d2_duplicate_policy_test.go`
covers facade callback drain behavior and dispatcher FIFO/token-identity invariants that
the websocket eviction latch relies on. Full websocket/facade `-race` verification needs
loopback networking and is run outside restricted sandboxes.

**Residual:** request handlers already accepted on the old socket may still send a late
CALL_RESULT/CALL_ERROR through the current same-ID websocket. That is benign wire noise
unless a charger uses colliding message IDs; eliminating it would require threading
connection identity through facade response paths and is out of scope for PR-D2.

## OCPP 1.6 encoding/validation

`ChangeConfigurationRequest.Value` carried `validate:"required"`, which rejects the Go zero
value (empty string). OCPP 1.6 defines the config `value` as **mandatory-but-may-be-empty** —
a key can legitimately be set to `""` — so `required` wrongly rejected a valid payload. The
fork drops `required` (keeps `max=500`, keeps the field a plain `string`). Consequence,
recorded honestly: with a plain `string`, an *omitted* `value` and an explicit `""` both decode
to `""`, so validation can no longer distinguish them — the fix accepts empty **and** omitted.
That trade is accepted (a breaking `*string` or a bespoke `UnmarshalJSON` would be the only ways
to keep presence enforcement; neither is worth it for a config write).

| File:line | Symbol | Why keep it |
|-----------|--------|-------------|
| `ocpp1.6/core/change_configuration.go:36` | `Value string \`json:"value" validate:"max=500"\`` (no `required`) | accepts a valid empty-string config value; length bound preserved; `Key` stays `required` |

**Guard:** `ocpp1.6_test/change_configuration_test.go` guards two *distinct* properties:
- `TestChangeConfigurationRequestValidation` pins the **validation** divergence — an explicit
  `Value:""` (and an omitted `Value`) validates while `Key` stays required and both `max` bounds
  still reject. A future re-add of `validate:"required"` on `Value` turns **this** test red.
- `TestChangeConfigurationRequestEmptyValueRoundTrip` pins the **encoding** property — an empty
  value survives the wire as `"value":""` (present, not omitted, since the field is not
  `omitempty`). It marshals/unmarshals directly and does **not** run validation, so it guards
  against a future `omitempty` being added to the tag; it would **not** catch a `required` re-add
  (that is the validation test's job).

Upstream: **#246** (@sbindzau) — no upstream fix merged; this is a fork-local 1.6-correctness edit.

> Line numbers are current as of the entries above; if the API moves, update this table
> and the guard tests together. The guard tests are the real backstop — the line numbers
> are only a navigation aid.

## ChargePoint/ChargingStation disconnect & reconnect hooks

The shared `ocppj.Client` already has disconnect/reconnect hooks, but the 1.6
`ChargePoint` and 2.0.1 `ChargingStation` facades did not expose them. This adds
the facade-level setters so embedders can observe unexpected drops and successful
redials without hand-building the raw endpoint just to reach the existing client
hooks. The setters are one-line delegations; the hook storage, sequencing, and
panic isolation remain owned by `ocppj.Client`.

| File:line | Symbol | Why keep it |
|-----------|--------|-------------|
| `ocpp1.6/v16.go:108`; `ocpp1.6/charge_point.go:306` | `ChargePoint.SetOnDisconnectedHandler` + `chargePoint` delegation | exposes the existing client unexpected-disconnect hook on the 1.6 facade |
| `ocpp1.6/v16.go:114`; `ocpp1.6/charge_point.go:315` | `ChargePoint.SetOnReconnectedHandler` + `chargePoint` delegation | exposes the existing client post-redial hook on the 1.6 facade, with the dispatcher-paused deadlock contract documented |
| `ocpp2.0.1/v2.go:149`; `ocpp2.0.1/charging_station.go:467` | `ChargingStation.SetOnDisconnectedHandler` + `chargingStation` delegation | keeps 2.0.1 facade parity for the existing unexpected-disconnect hook |
| `ocpp2.0.1/v2.go:155`; `ocpp2.0.1/charging_station.go:476` | `ChargingStation.SetOnReconnectedHandler` + `chargingStation` delegation | keeps 2.0.1 facade parity for the reconnect hook, including the `StartWithRetries` initial-connect nuance |

**Guard:** `ocpp1.6_test/disconnect_hook_test.go` and
`ocpp2.0.1_test/disconnect_hook_test.go` exercise the public facade setters for
unexpected disconnect and reconnect wiring, including graceful-stop and panic
guard behavior where applicable.

Upstream: this completes **PR #85** (@michaelbeaumont — the in-tree
`ocppj.Client` setters, also in `upstream/master`) at the facade layer, which
upstream still lacks. It resolves the still-OPEN **#288** (@sc-atompower), where
the disconnect handler appeared "not called" because the durable client hook was
not reachable from the facade and the ws-layer hook is rewired by `Start`.

> Line numbers are current as of the entries above; if the API moves, update this table
> and the guard tests together. The guard tests are the real backstop — the line numbers
> are only a navigation aid.

## Default profile exports

The facade constructors already registered a canonical default profile set, but the same
list was hand-maintained in four places and custom `ocppj` endpoint builders had no
supported install path other than copy-pasting it. This fork exports additive
`ocpp16.DefaultProfiles()` and `ocpp2.DefaultProfiles()` helpers and has the four default
constructors source their variadic profile lists from them. The helpers return a fresh
slice on every call while preserving the shared `*ocpp.Profile` elements and the existing
order. This is **fork-original**; there is no upstream issue or PR.

| File:line | Symbol | Why keep it |
|-----------|--------|-------------|
| `ocpp1.6/v16.go:43` | `func DefaultProfiles() []*ocpp.Profile` | supported public way to install the OCPP 1.6 default constructor profile set on a custom `ocppj` endpoint |
| `ocpp1.6/v16.go:212` | `NewChargePoint` uses `DefaultProfiles()...` | keeps the charge point constructor sourced from the exported single source of truth |
| `ocpp1.6/v16.go:377` | `NewCentralSystem` uses `DefaultProfiles()...` | keeps the central system constructor sourced from the exported single source of truth |
| `ocpp2.0.1/v2.go:51` | `func DefaultProfiles() []*ocpp.Profile` | supported public way to install the OCPP 2.0.1 default constructor profile set on a custom `ocppj` endpoint |
| `ocpp2.0.1/v2.go:266` | `NewChargingStation` uses `DefaultProfiles()...` | keeps the charging station constructor sourced from the exported single source of truth |
| `ocpp2.0.1/v2.go:472` | `NewCSMS` uses `DefaultProfiles()...` | keeps the CSMS constructor sourced from the exported single source of truth |

**Guard:** `ocpp1.6_test/profiles_test.go` and `ocpp2.0.1_test/profiles_test.go` assert
the exported default sets by pointer identity and order, and mutate the returned slice to
prove each call returns a fresh slice. These tests are the sole completeness backstop for
the constructor lists: the broader mock-based E2E suites inject prebuilt `ocppj`
endpoints and bypass the defaults.

> Line numbers are current as of the entries above; if the API moves, update this table
> and the guard tests together. The profile-set pointer-identity tests are the real
> backstop — the line numbers are only a navigation aid. A future upstream re-vendor of
> `v16.go` or `v2.go` may drop the export and re-inline the lists; keep this fork-local
> additive API.

## slog logging adapter

The library logs through the `logging.Logger` interface (`logging/log.go`) and ships only a
silent `VoidLogger` default, so a consumer must hand-write an adapter to route the library's
internal logs anywhere. This fork adds a ready-made bridge from `logging.Logger` to the stdlib
`log/slog` — `slogadapter.New(*slog.Logger) logging.Logger` — so `ocppj.SetLogger(...)` /
`ws.SetLogger(...)` can pipe the library's logs into a consumer's `slog` setup instead of
running at `VoidLogger`. It lives in a **leaf package** so `log/slog` is imported there only and
never enters the core (`ocppj`/`ws`) import graph. This is **fork-original** — no upstream issue
or PR. (`log/slog` requires Go 1.21; the module `go` directive was bumped `1.16`→`1.21` alongside
this — the real floor was already ≥1.19 via `atomic.Bool`, so no build tags are needed.)

| File:line | Symbol | Why keep it |
|-----------|--------|-------------|
| `logging/slogadapter/slogadapter.go:26` | `func New(logger *slog.Logger) logging.Logger` | ready-made `logging.Logger` backed by `slog`; nil → `slog.Default()` (snapshot at construction); leaf package keeps `log/slog` out of the core graph |
| `logging/slogadapter/slogadapter.go` | `slogLogger` — 6 methods (via `emit`) mapping to `slog` `Debug`/`Info`/`Error` with `fmt.Sprint`/`fmt.Sprintf`, gated on `Enabled` | print/printf → message-only slog calls (no structured attrs — the interface carries none), matching logrus print semantics; a disabled level skips formatting |
| `go.mod:3` | `go 1.21` | required by `log/slog`; the leaf package makes it the true floor only for slog users, though the directive raises it module-wide (both consumers are already 1.21+) |

**Guard:** `logging/slogadapter/slogadapter_test.go` asserts level+message routing for all six
methods, that `New(nil)` actually routes through `slog.Default()` (swaps the default to a capturing
handler and asserts the record lands there), and — via a *print* method — that args are formatted
into the message and NOT leaked as slog attributes; plus the compile-time
`var _ logging.Logger = (*slogLogger)(nil)` (the real backstop if the interface gains a method).

> A future upstream that ever adds an slog adapter would likely place it differently — keep this
> leaf-package split so `log/slog` stays out of the `ocppj`/`ws` transitive dependency set.

## Context-bounded server shutdown

The server teardown chain (`facade.Stop()` → `ocppj.Server.Stop()` → `ws.Server.Stop()`) ended at
`httpServer.Shutdown(context.TODO())` — an un-cancelable, un-deadlined shutdown, so a caller could
not bound how long teardown blocks (only an external timeout could). This fork adds an **additive**
`Shutdown(ctx context.Context) error` at each layer, mirroring `http.Server.Shutdown`: it threads
the caller's context down to `httpServer.Shutdown(ctx)` and returns the resulting error. The
existing `Stop()` is kept as the unbounded convenience — at the `ws` layer it is now
`Shutdown(context.Background())` (behavior-identical to the old `context.TODO()` path, and it still
reports any listener-close error to `Errors()`); at the `ocppj`/facade layers `Stop()` is left
unchanged so it keeps calling the wrapped `Stop()` rather than re-routing through `Shutdown`.

The `Shutdown(ctx)` **API is fork-original**, but it extends upstream's graceful-server-`Stop()`
lineage: the facade `Stop()` it parallels came from [#245](https://github.com/lorenzodonini/ocpp-go/pull/245)
(@rbright, explicitly motivated by "graceful shutdown when the application stops"), and the
connection-teardown-on-`Stop()` mechanism it threads `ctx` through came from
[#93](https://github.com/lorenzodonini/ocpp-go/pull/93) and
[#82](https://github.com/lorenzodonini/ocpp-go/pull/82) (@michaelbeaumont — #93 documents that
`http.Server.Shutdown` leaves hijacked websocket connections to the pump goroutines, the exact
behaviour this section's semantics build on). It does **not** resolve the still-open, client-side
[#143](https://github.com/lorenzodonini/ocpp-go/issues/143) (@bhatanku1 — `ChargePoint.Stop`
should return an error), which is the same theme on the opposite endpoint.

Semantics (documented on the methods): `ctx` bounds `http.Server.Shutdown`, which covers the
listeners and any *tracked* HTTP requests (`AddHttpHandler` handlers, pre-upgrade requests). It does
**not** impose a per-connection deadline on already-upgraded websockets — those are hijacked and
closed asynchronously by the existing `RegisterOnShutdown(s.stopConnections)` hook — and the `ocppj`
layer stops the dispatcher first and unconditionally (not `ctx`-aware), so `ctx` is not an
end-to-end teardown deadline. On early `ctx` expiry the error channel is closed immediately and any
later teardown errors are dropped.

| File:line | Symbol | Why keep it |
|-----------|--------|-------------|
| `ws/server.go:388` | `func (s *server) Shutdown(ctx context.Context) error` | context-bounded teardown; threads `ctx` into `httpServer.Shutdown`, reports a listener-close error to `Errors()`, returns the error; `Stop()` delegates here with `context.Background()` |
| `ws/server.go:71` | `Shutdown(ctx context.Context) error` on the `ws.Server` interface | exposes the bounded variant alongside `Stop()` |
| `ocppj/server.go:156` | `func (s *Server) Shutdown(ctx context.Context) error` | `dispatcher.Stop()` then `server.Shutdown(ctx)`; `Stop()` left unchanged as a parallel wrapper |
| `ocpp1.6/central_system.go:566` + `ocpp1.6/v16.go:362` | `CentralSystem.Shutdown(ctx)` | 1.6 facade + interface delegation to `ocppj.Server.Shutdown` |
| `ocpp2.0.1/csms.go:844` + `ocpp2.0.1/v2.go:457` | `CSMS.Shutdown(ctx)` | 2.0.1 facade + interface delegation (1.6/2.0.1 parity) |
| `ws/mocks/mock_Server.go:480` | `MockServer.Shutdown` | regenerated for the grown `ws.Server` interface (kept in mockery's alphabetical method order so `mockery` produces no diff) |

**Guard:** `ws/websocket_test.go` — `TestServerShutdownGraceful` (real server: `Shutdown(Background)`
returns nil, `Errors()` closes, the client disconnects), `TestServerStopStillTearsDown` (`Stop()`
still tears down via the delegation), `TestServerShutdownCanceledCtx` (an already-canceled ctx still
tears the server down without panic). Facade suites `ocpp1.6_test`/`ocpp2.0.1_test` —
`TestShutdownThreadsThrough` (asserts the *exact* caller `ctx` instance reaches `ws.Server.Shutdown`,
and that the dispatcher is already stopped when it does) and `TestShutdownPropagatesError` (the error
is returned unswallowed — something `Stop()` could not express). All under `-race`.

> `Stop()` is intentionally *not* re-routed through `Shutdown` at the `ocppj`/facade layers: the
> facade test suites drive a hand-written `MockWebsocketServer` that records via `MethodCalled`, and
> every existing server test sets only `.On("Stop")` — routing `Stop()` through `Shutdown` there
> would make those calls hit an unexpected `Shutdown` mock and panic. Keep the two as parallel
> wrappers.

## Context-aware send (`SendRequestCtx`)

Per-request `context.Context` on outbound client sends — a caller can cancel or deadline-bound an
individual OCPP request independently of the dispatcher's fixed `SetTimeout`. Addresses the use-case
of upstream **[#105](https://github.com/lorenzodonini/ocpp-go/pull/105)** (@michaelbeaumont) /
**[#153](https://github.com/lorenzodonini/ocpp-go/issues/153)** (@sbindzau). The API **intentionally
diverges** from #105: it is additive (ctx-less `SendRequest`/`SendRequestAsync` are preserved as
`context.Background()` wrappers) and ctx-first (`SendRequestCtx(ctx, request)`, per Go convention),
where #105 used ctx-last `SendRequestWithContext(request, ctx)` — so it *addresses the use-case of*
#105, it does not *match* its signatures.

Semantics: a ctx that fires while the request is queued is honored at dispatch (dropped, never sent
on reconnect — the #153 ask); a ctx that fires in flight cancels via the E1a completion-ownership
(`CompleteRequest`/`PopIf`), delivering an error matching both `ocppj.ErrRequestCanceled` (marker) and
`context.Canceled`/`DeadlineExceeded` (via `ocpp.Error.Cause`+`Unwrap`, reusing E1a's error surface —
no new sentinel). Exactly-once holds: a response, a timeout, a dispatcher-stop, and a ctx-cancel all
race to the single-winner `CompleteRequest`. Cancellation is best-effort and local (the peer may still
receive/process the request; its late response is discarded by `ParseMessage`'s pending-check).

**Not cleanly "additive" — two narrow source-breaking edges** (both low-risk here): `RequestBundle`
gains a `Ctx` field (breaks any downstream *unkeyed* literal — `server.go:185` was the only in-repo
one, keyed here); and `ChargePoint`/`ChargingStation` grow two methods (breaks any downstream
*implementer* — only the library's own concrete facades implement them). Call-site callers and typed
helpers are byte-identical.

| File:line | Symbol | Why keep it |
|-----------|--------|-------------|
| `ocppj/queue.go:14,18` | `RequestBundle.Ctx` + `bundleCtx()` | optional per-request ctx on the dispatch bundle; nil ⇒ `context.Background()` |
| `ocppj/client.go:205` | `Client.SendRequestCtx(ctx, request)` | ctx-carrying send; `SendRequest` delegates with `context.Background()` |
| `ocppj/ocppj.go:47` | `NewRequestCanceledError(messageID, cause)` | exported so the facades can synthesize a canceled error matching `ErrRequestCanceled`+`context.Canceled`; nil-cause-safe |
| `ocppj/dispatcher.go:387` | `dispatchNextRequest() (pumpPending, bool)` | pre-write drop of an already-fired ctx (one front per pump iteration — never a synchronous burst of on-pump cancels); returns the dispatched request's ctx token |
| `ocppj/dispatcher.go:94,345` | `pumpPending` token + `case <-pendingDone` arm | in-flight ctx-cancel: pump-local `{id,ctx,action,payload}` reconciled via `GetPendingRequest`, cancels through `CompleteRequest` (single-winner); the pump never takes `d.mutex` |
| `ocppj/server.go:185` | keyed `RequestBundle{Call:…, Data:…}` | the one positional literal the new field would break |
| `ocpp1.6/charge_point.go:342,400` + `v16.go:161,169` | `ChargePoint.SendRequestCtx`/`SendRequestAsyncCtx` | 1.6 facade + interface; ctx-less variants are `Background()` wrappers |
| `ocpp2.0.1/charging_station.go:506,565` + `v2.go:207,216` | `ChargingStation.SendRequestCtx`/`SendRequestAsyncCtx` | 2.0.1 facade + interface (1.6/2.0.1 parity) |
| `ocpp1.6/charge_point.go:370` + `ocpp2.0.1/charging_station.go:535` | `awaitCtxResult(ctx, featureName, …)` | prefer-response fast-path (a delivered response wins over an already-fired ctx); `featureName` keeps the internal/stop error strings byte-identical to pre-E1c |
| `ocpp2.0.1/charging_station.go:645` (called `:639`) | `chargingStation.clearCallbacks()` on the `stopC` arm | mirrors 1.6's `clearCallbacks` — without it a ctx/response/Stop race orphans the callback closure (and, across Stop→Start, mis-routes a later same-feature response to the stale closure) |

**Guard:** `ocppj/e1c_context_send_test.go` (pre-write drop, in-flight cancel + no-double-deliver,
queued-during-pause, in-flight-cancel-*while-paused*, ctx-less regression, Stop-vs-cancel exactly-once,
off-pump-complete-then-stale-ctx, N>1 cascading drops); `ocpp2.0.1/context_clearcallbacks_test.go`
(white-box: the `stopC` arm drains `cs.callbacks`); `ocpp{1.6,2.0.1}/context_awaitresult_test.go`
(white-box: the prefer-response fast-path, iterated 100× to defeat a plain-select 50/50 false-pass);
`ocpp{1.6,2.0.1}_test/context_send_test.go` (facade e2e: canceled-ctx error, nil-ctx == Background,
`SendRequest`/typed helpers untouched). All under `-race`.

> **Known pre-existing, deferred (see `DEFERRED.md`), NOT E1c regressions:** on-pump cancel delivery
> does a blocking send to the facade's shared channel — E1c stays one-cancel-per-iteration so it does
> not *amplify* the pre-existing blocking-callback shutdown-deadlock class; the default
> `FIFOClientQueue` is unbounded, so canceled-while-disconnected requests accumulate (active eviction
> deferred); and the 2.0.1 `clearCallbacks` mirror inherits 1.6's no-handler-join restart race (the
> join-fix must land on both). **Out of scope: E2** — server-side (CSMS) context-aware send.

## requestID-keyed callback queue

Upstream lineage — the root fix for a family the fork had so far only *mitigated*:
[#363](https://github.com/lorenzodonini/ocpp-go/issues/363) (@qosmotec — the type-keying
mitigation, now superseded), #294 ("CS confuses error responses between requests in case of
timeout"), #67 (panic when `TriggerMessage` and `Change/GetConfiguration` run concurrently).
Type-keying (`callbackqueue.RequestType`) stopped the interface-conversion *panic* but not the
*mis-pairing*, and did nothing on the CALL_ERROR path (which dequeued untyped — a CALL_ERROR
carries no feature name). Keying the callback queue by the exact OCPP message ID instead of the
feature type closes the whole family, including a live regression: E1c's pre-write ctx drop
widened a previously near-unreachable client-side race into a routinely triggerable callback
mis-pairing (two in-flight requests' callers could receive each other's result; in the
different-type case one callback could be orphaned entirely).

**Breaking — three `ocppj` signatures** (facade-level APIs — `SendRequestAsync`, the typed
helpers — are unchanged):

| File | Symbol | Change |
|------|--------|--------|
| `ocppj/client.go` | `Client.SendRequest(request) (string, error)` | was `error` |
| `ocppj/client.go` | `Client.SendRequestCtx(ctx, request) (string, error)` | was `error`; this is the one the facades call |
| `ocppj/server.go` | `Server.SendRequest(clientID, request) (string, error)` | was `error` |

All three return the generated `Call.UniqueId` on success and `""` on error. The message ID is
generated inside `CreateCall` (inside the send), so `internal/callbackqueue.TryQueue` now takes
`try func() (string, error)` returning that ID; registration happens after the send but under the
same mutex, so an early response blocks in `Dequeue` rather than racing registration.

**Behavior change 1** — a response/error whose ID matches no registered callback now hits the
"no handler available" error path instead of consuming an unrelated pending callback. This is the
fix; it surfaces latent consumer bugs as errors where they were previously silent mis-deliveries.

**Behavior change 2** — disconnect-drain order is no longer FIFO. `CallbackQueue.DrainAll` iterates
a Go map; per-client callback order on disconnect is randomized per run. Correctness-neutral (every
drained callback receives the same disconnect error) but observable to a consumer relying on order.

`ErrDuplicateCallback`: `TryQueue` rejects a second callback for the same (clientID, requestID)
rather than silently overwriting the first. Rejection happens *after* `try()` (message already on
the wire, no callback — response lands on "no handler available"): defense-in-depth against silent
overwrite, not a caller-actionable error, unreachable with the default random ID generator. It is
NOT re-exported at the facade level — deliberate, since the spec treats it as unreachable
defense-in-depth rather than something callers should `errors.Is`.

Supersedes the #363 type-keying mitigation entirely — `RequestType` and `callbackEntry` are deleted
from `internal/callbackqueue`.

**Test seam (1.6 only):** the client/server cross-delivery regression tests need to pin a goroutine
interleaving. The seam lives in an unexported `internal/testhooks` package (nil-by-default vars read
at the top of the 1.6 response closures, set only by tests) — reachable by the black-box test package
in the separate `ocpp1.6_test/` directory yet adding **zero** public API to `ocpp16`. No 2.0.1
equivalent seam was added — deemed redundant with the 1.6 pins plus the `internal/callbackqueue`
unit suite, since the production fix is symmetric across both versions (an equivalent 2.0.1 pin
*is* achievable — the same gated-hook pattern transplants — it was simply judged not to earn its
keep). The 2.0.1 client's response/error `select` over two channels remains a second inversion
source that ID-keying tolerates rather than removes: the §1a channel-merge (converging 2.0.1 onto
1.6's single-channel shape) was NOT adopted — spec-optional.

Server-side lock caveat (documented in `central_system.go`/`csms.go` godoc): one `callbackQueue`
mutex spans all connected clients; a pump wedged on one stalled client's `Write` stalls every
client's dequeue. Pre-existing, unchanged here. DEFERRED: per-client lock striping.

**Guard:** `internal/callbackqueue/e2_0_test.go` (out-of-order dequeue, try()-failure no-leak,
Dequeue-blocks-on-TryQueue race, DrainAll exactly-once + outer-map cleanup, duplicate-ID rejection);
`ocpp1.6_test/e2_0_cross_delivery_test.go` (client + server cross-delivery regressions [same- and
different-type cascade], wire CALL_ERROR routing by ID). All green under `-race`; the four facade
regression tests pass under `-race -count=10`.
