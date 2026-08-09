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

The fork-original ws feature now has a facade-level entry point via
`NewDefaultCentralSystem` / `NewDefaultCSMS`, so consumers can set the limit without
importing the `ws` package. The default remains unlimited (`0`).

The option is named `WithServerReadLimit` in all three packages (`ws`, `ocpp1.6`,
`ocpp2.0.1`). The knob exists on both sides of the wire — `ClientTimeoutConfig.ReadLimit`
is already shipped — so the name is side-qualified up front, matching the existing
`WithClientTLSConfig` / `WithServerTLSConfig` pair; a client-side analogue would be
`WithClientReadLimit` in the same packages. One identifier for one knob across all
three layers.

| File:line | Symbol | Why keep it |
|-----------|--------|-------------|
| `ws/websocket.go:135` | `ReadLimit int64` on `ServerTimeoutConfig` | public opt-in knob for inbound message size on server conns |
| `ws/websocket.go:191` | `ReadLimit int64` on `ClientTimeoutConfig` | public opt-in knob for inbound message size on client conns |
| `ws/websocket.go:280` | `ReadLimit int64` on internal `WebSocketConfig` | carries the limit from the timeout config to `newWebSocket`/`updateConfig` |
| `ws/client.go:380` | `wsCfg.ReadLimit = c.timeoutConfig.ReadLimit` before `newWebSocket` | threads the client knob without changing `NewDefaultWebSocketConfig`'s signature |
| `ws/server.go:540` | `wsCfg.ReadLimit = s.timeoutConfig.ReadLimit` before `newWebSocket` | threads the server knob without changing `NewDefaultWebSocketConfig`'s signature |
| `ws/websocket.go:435` | `if cfg.ReadLimit > 0 { w.connection.SetReadLimit(cfg.ReadLimit) }` in `updateConfig` | applies the limit at the single cfg→conn choke point; `> 0` gate keeps `0`/negative unlimited |
| `ws/server.go` | `WithServerReadLimit` ServerOpt | construction-safe alternative to `SetTimeoutConfig`; cannot zero the write/ping deadlines |
| `ocpp1.6/v16.go` | `NewDefaultCentralSystem` + `CentralSystemOpt` + `WithServerReadLimit` | facade route to the knob without a `ws` import |
| `ocpp2.0.1/v2.go` | `NewDefaultCSMS` + `CSMSOpt` + `WithServerReadLimit` | facade route to the knob without a `ws` import |

**Guard:** `ws/websocket_test.go` — `TestServerReadLimitExceeded` (server drops the over-limit
connection: proves the server call site threads the limit), `TestClientReadLimitExceeded`
(client surfaces `websocket.ErrReadLimit` on its disconnect handler), `TestServerReadLimitUnderLimitPasses`,
`TestReadLimitDefaultUnlimited` (default 0 delivers a large message unchanged),
`TestClientReadLimitAppliesAfterReconnect` (a fresh dial re-applies the limit),
`TestWithServerReadLimitPreservesTimeoutDefaults`, `TestWithServerReadLimitLastWinsAndComposes`,
`TestServerReadLimitExceededViaOption`, `TestWithServerReadLimitNonPositiveIsUnlimited`;
`ocpp1.6_test/ec4_readlimit_test.go` — `TestFacadeReadLimitThroughSuppliedServer`,
`TestNewDefaultCentralSystemAppliesReadLimit`, `TestNewDefaultCentralSystemNoOptsMatchesLegacyDefault`;
`ocpp2.0.1_test/ec4_readlimit_test.go` — parity tests for `NewDefaultCSMS`. All run under
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

## OCPP 1.6 certificate-hash validator ownership

`CertificateHashData.HashAlgorithm` carried `validate:"required,hashAlgorithm"` — a validate
token no package under `ocpp1.6/` registers. The registry behind `types.Validate` is one
process-wide validator shared by both protocol versions, and ocpp2.0.1's types package happens
to register a bare `hashAlgorithm` over the same three values, so any build whose import graph
reaches ocpp2.0.1 validated this field correctly **by accident** (the full 1.6 facade is such a
build, via `ocpp1.6/logging`). A build importing a 1.6 profile package directly — e.g.
`ocpp1.6/certificates` with `ocpp1.6/types` — left the token unregistered, and validating any
certificate hash (a `DeleteCertificateRequest`, a `GetInstalledCertificateIdsResponse` carrying
at least one entry) **panicked** instead of validating:
`Undefined validation function 'hashAlgorithm'`. The fork renames the token to a 1.6-owned
`hashAlgorithm16` and registers it beside the type, following the `<name>16` convention the
1.6 tree already uses for every other cross-version-ambiguous token (`genericStatus16`,
`certificateUse16`, …). The accepted value set (`SHA256`/`SHA384`/`SHA512`) is unchanged, the
JSON property name is unchanged; only the tag's validator token and its registration move.

| File:line | Symbol | Why keep it |
|-----------|--------|-------------|
| `ocpp1.6/types/security_extension.go:94` | tag `validate:"required,hashAlgorithm16"` | 1.6 owns its own validator; no cross-version registry coupling |
| `ocpp1.6/types/security_extension.go:82,109` | `isValidHashAlgorithm` + local `init()` registration | the registration lives beside the type it validates |

**Guard:** `ocpp1.6/types/security_extension_test.go` runs in a test binary whose import graph
does **not** include ocpp2.0.1, so the field's validator must be registered by `ocpp1.6/types`
itself — reverting the registration turns the suite into the original panic, not a soft
failure. Its three cases pin: all three declared algorithms accepted, an unknown algorithm
rejected by tag `hashAlgorithm16`, an absent algorithm rejected by `required`.

Upstream: the correct registration briefly existed during the security-extension development
(commit `d2d2b0c` added `hashAlgorithm16` alongside `genericStatus16`) and was dropped in the
squashed merge of **#266**; upstream master still carries the unregistered bare token.

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

## Server completion ownership (E2a)

Server-side mirror of the client completion-ownership work (E1a). Hardens
`DefaultServerDispatcher`'s completion path, fixing three pre-existing bugs that
exist independently of any context/cancellation feature:

- **A1 — pump self-deadlock.** On a `Write` error, `dispatchNextRequest` (running
  ON the pump) called the public `CompleteRequest`, which ends in a blocking send
  to the cap-1 `readyForDispatch` — a channel the pump alone reads. If an off-pump
  completion had already filled the buffer, the pump blocked forever, stalling
  **all** clients. Plausibly related to upstream #136 (@utsavanand2, "server
  becomes unresponsive after a while") — flagged as a hypothesis, not a claim.
- **A2 — non-atomic completion.** `CompleteRequest` did `Peek`→compare→`Pop` with
  no lock spanning the steps; a response racing a timeout on the same request
  could double-pop, silently discarding the next queued request. Same family as
  #294 / #363 / #67 (callback confusion between requests) that the requestID-keyed
  callback queue only mitigated at the callback layer.
- **A3 — `SetTimeout(0)` re-dispatch.** The dispatch guard lacked a
  `HasPendingRequest` check (unlike the client), so with no timeout the in-flight
  front request was re-written on the next send.

The fix introduces an atomic `completeRequestOwned` (backed by the existing
`RequestQueue.PopIf`) that every completion site routes through; only the call
that atomically wins ownership advances the queue and fires anything.

**Breaking — one interface method:**

| File | Symbol | Change |
|------|--------|--------|
| `ocppj/dispatcher.go` | `ServerDispatcher.CompleteRequest(clientID, requestID)` | now returns `bool` (won ownership); was `void` |

Breaking only for external **implementors** of `ServerDispatcher` (not callers).
Exact precedent: E1a made the same change to `ClientDispatcher`. On-pump
completion sites use the unexported non-signaling `completeRequestOwned`; the
public `CompleteRequest` (off-pump only, from `server.go`'s ws-read-goroutine
CALL_RESULT/CALL_ERROR handlers) additionally signals `readyForDispatch` when it
wins. Those two handlers now fire `responseHandler`/`errorHandler` only on a win.

**Behavior change:** a genuine CALL_RESULT/CALL_ERROR that arrives after the
server has already removed the client's queue (a response racing a disconnect)
is now suppressed — the caller receives the facade's disconnect-drain error
instead of a late, now-ownerless response. Strictly better (exactly one
notification; previously the unconditional handler plus the facade dequeue could
double-touch), but a visible delta for a raw-`ocppj` consumer.

Also folds the write-error re-entry (the pump loops the dispatch step, bounded by
queue length, so a client's next request still dispatches after a failed write
without the removed `readyForDispatch` self-send) and generation-pinned
`waitForTimeout` (its `stoppedC`/`timerC` are passed as parameters at spawn and
its send is shutdown-safe `select`, so a stale watcher after a `Stop`→`Start`
cycle can neither race the field reassignment nor misdeliver into a new
generation).

Out of scope / DEFERRED: server `readyForDispatch` remains a blocking send
(cross-client contention, bounded; the client uses a non-blocking coalesced
send); the `deleteAck` purge lacks queue-identity awareness (a fast reconnect
inside the re-entry loop can cancel a fresh request's timeout watcher — bounded).

**Guard:** `ocppj/e2a_completion_ownership_test.go` (A1 pump-survival via a
pre-filled `readyForDispatch` + blocking writer; A2 timeout/response double-pop
race; A3 `SetTimeout(0)` exactly-one-write; the `CompleteRequest` bool contract;
the write-error same-client re-entry; B2/B3 stale-watcher no-cross-delivery /
no-leak). All green under `-race`; the timeout-race and watcher tests pass under
`-race -count=5`.

## Server-side context-aware send (`SendRequestCtx`, E2c)

Server (CSMS/central-system) mirror of the client-side `SendRequestCtx` (E1c) —
a caller passes a `context.Context` to a server→client send; canceling it
cancels the outstanding request. Rides E2a's atomic `completeRequestOwned`/
`CompleteRequest`→`bool` completion ownership as the single-winner basis for
exactly-once cancel delivery, and E2-0's requestID-keyed callback queue (no
callback aliasing possible). Addresses the same upstream use-case as E1c
(**[#105](https://github.com/lorenzodonini/ocpp-go/pull/105)** /
**[#153](https://github.com/lorenzodonini/ocpp-go/issues/153)**), server side.

Mechanism — generalizes the existing per-request timeout watcher
(`waitForTimeout`) rather than inventing a new one:

- `waitForTimeout` grows two parameters (`requestID string`, `userCtx
  context.Context`) and a new `cancelC chan serverCancelToken` (cap 10,
  matching `timerC`'s B4 bound), all passed at spawn — generation-pinned like
  `stoppedC`/`timerC` (B3). It now selects on three arms: the internal
  timeout-tracking `clientCtx.ctx.Done()` (deadline → posts to `timerC`, as
  before), the caller's `userCtx.Done()` (cancel → posts a
  `serverCancelToken{clientID, requestID, ctx}` to `cancelC`), and `stoppedC`.
  Both sends are shutdown-safe `select`s against `stoppedC` (B2).
- A `clientCtx` (the internal timeout-tracking context) is now created
  whenever `d.timeout > 0` (unchanged, `context.WithTimeout`) **or** the
  bundle's own ctx is cancelable (`Done() != nil`, via `context.WithCancel`)
  — B1. This is what lets every existing completion path (the pump's
  `readyForDispatch`-token arm, `DeleteClient`) reap a spawned watcher on
  normal completion via the same `clientCtx.cancel()` call; without it, a
  completed request under `SetTimeout(0)` would leak one goroutine per
  completion. A ctx-less send (`Background`, `Done() == nil`) with
  `SetTimeout(0)` still creates no `clientCtx` and spawns no watcher.
  `dispatchNextRequest` also gained a **pre-write drop** (C3): a queued
  request whose ctx has already fired when it reaches the front is completed
  and canceled without ever being written, reusing E2a's write-error
  drain-loop re-entry (`dispatchStatus`/`dispatchCompletedNoWrite`) so the
  pump advances to the next queued front in the same re-entry.
- A new pump arm, structurally identical to the existing timeout arm:
  identity-guards on `dispatchedRequestIDMap[clientID] == token.requestID`
  (stale ⇒ no-op), then calls the same on-pump, non-signaling
  `completeRequestOwned` E2a introduced; only the winner cancels the internal
  `clientCtx`, clears pump-local state, marks the client ready, and fires
  `onRequestCanceled` with `newRequestCanceledError(id, ctx.Err())` — the same
  error constructor E1c/E2a already use, matching both
  `ErrRequestCanceled` and `context.Canceled`/`DeadlineExceeded`.

**New public surface — one concrete method plus two additive interface
methods.** Only the two interface additions are breaking, and only for external
**implementors** of `CentralSystem`/`CSMS` (not for callers); `Server.SendRequestCtx`
is a concrete method and breaks nothing. The `ServerDispatcher`/`ClientDispatcher`
interfaces are unchanged by E2c:

| File | Symbol | Breaking? | Change |
|------|--------|-----------|--------|
| `ocppj/server.go` | `Server.SendRequestCtx(ctx, clientID, request) (string, error)` | no (concrete method) | new; `SendRequest` delegates with `context.Background()` |
| `ocpp1.6/v16.go` | `CentralSystem.SendRequestAsyncCtx(ctx, clientId, request, callback) error` | yes (implementors) | new; `SendRequestAsync` delegates with `context.Background()` |
| `ocpp2.0.1/v2.go` | `CSMS.SendRequestAsyncCtx(ctx, clientId, request, callback) error` | yes (implementors) | new; `SendRequestAsync` delegates with `context.Background()` |

Same shape/precedent as E1c's `ChargePoint`/`ChargingStation` additions. No mock
regeneration was needed: neither interface has a generated
mock in this repo (`mock_ocpp16.go`'s `CentralSystem`/`ChargePoint` entries in
`.mockery.yaml` were never actually emitted; only the unrelated
`ChargePointConnectionHandler` func-type mock exists there).

**Documented semantics (`SendRequestCtx` godoc):** canceling a request that is
still *queued* behind an in-flight one is only *delivered* when that front
request completes and this one reaches the front (or on disconnect) — with
`SetTimeout(0)` and a silent peer that delivery is unbounded (client-side
parity, not a new limitation). If a deadline and a user cancel land at
(approximately) the same instant, exactly one terminal error is delivered but
which sentinel wins is not guaranteed.

**Guard:** `ocppj/e2c_context_send_test.go` (dispatched-cancel exactly-once;
queued-cancel pre-write-drop with a flushed front; cancel-vs-genuine-CALL_RESULT
exactly-one-winner, `-race -count=10`; stale cancel token no-op; already-canceled
ctx at send time; `SetTimeout(0)` + cancelable ctx watcher spawn/cancel/no-panic/
no-leak-on-normal-completion; nil-ctx == `Background`; multi-client isolation;
cancel-during-`Stop` no dead send/no leak; `Stop`→`Start` cross-generation no
delivery; same-client in-flight-cancel-during-drain-loop ordering);
`ocpp{1.6,2.0.1}_test/e2c_context_send_test.go` (facade e2e: dispatched cancel,
queued cancel with C7.15 flush, nil-ctx == Background + `SendRequestAsync`
regression). All green under `-race`.

Out of scope / DEFERRED (unchanged from the spec, not new): no queue scanning /
eviction of canceled non-front requests; no ctx variants of the ~40 typed
helpers (use `SendRequestAsyncCtx` directly); no per-request timeout override;
the facade's `error()` on a callback-queue `Dequeue` **miss** is a non-blocking,
best-effort `errC` report (EC2 made the buffer cap 16; EC-D1 moved the
dequeue-miss path off the pump entirely).

## OCPP 1.6 configuration store/manager (`ocpp1.6/configmanager`)

New charge-point-side package: a typed OCPP 1.6 configuration store + manager,
plus two facade-wiring helpers (`OnGetConfiguration`/`OnChangeConfiguration`)
that let a consumer's `core.ChargePointHandler` answer inbound
`GetConfiguration`/`ChangeConfiguration` by one-line delegation. Additive and
non-breaking — a new package, no change to any existing type or interface.

Ported from xBlaz3kx/ocpp-go's `config_manager` (now ChargePi/ocpp-manager,
MIT) — that project's take on upstream #286. Fork changes over the source:
all external deps stripped to stdlib (no `samber/lo`, no `go-commons-lang` — no
new `go.mod` deps); the facade wiring the source lacked, incl. the full
`ConfigurationStatus` mapping and a `GetConfigurationMaxKeys` default; two
carried-defect fixes (the discarded-handler-error no-op defer → a shared atomic
apply→handler→rollback primitive, re-panic-after-rollback on a handler panic;
the unreachable ISO15118 mandatory-keys branch → an explicit `ISO15118` profile
sentinel + defaults); and concurrency hardening (every shared-state method
guarded by one non-reentrant mutex with internal unlocked helpers, deep copies
at every API boundary). In-memory/process-lifetime, case-sensitive key matching.

Guard: `ocpp1.6/configmanager/configmanager_test.go` (dep-strip equivalence,
`-race` concurrency, ISO15118 sentinel+defaults, atomic rollback on handler
error and panic, deep-copy non-aliasing, both wiring helpers + full status
mapping) + `ocpp1.6_test/configmanager_e2e_test.go` (facade e2e: a real charge
point delegating to a `ManagerV16`). All green under `-race`.

## Shutdown-preemptible facade producers (client facades)

A blocking or slow user callback could hang `Stop()` forever on both client
facades, and a `Stop()` racing an inbound message could permanently leak the
websocket read goroutine. Each facade runs ONE `asyncCallbackHandler` goroutine
that drains a cap-1 channel and invokes user callbacks inline; producers into
that channel run on OTHER goroutines — the dispatcher pump (`onRequestTimeout`,
via `fireRequestCancel`) and the ws read goroutine (the forwarding closures
wired in `NewChargePoint`/`NewChargingStation`). Those sends were unconditional,
so a stalled drain wedged the producer. Since `ocppj.Client.Stop()` waits for
the pump to exit, a wedged pump meant `Stop()` never returned; a wedged read
goroutine was never joined by anything and leaked for the process lifetime.

Every producer send into a facade-owned channel is now preempted by shutdown —
`select { case ch <- msg: case <-stopC: }` — covering `onRequestTimeout`, all
six forwarding closures (1.6 and 2.0.1 response/error/request; 2.0.1 gained its
request closure with the inbound ordering unification below) and
`error()` on both facades. `Stop()` is reordered to close `stopC` *before*
`client.Stop()`; without that the preemption would be unreachable, since
`client.Stop()` blocks on the wedged pump first. `stopC` becomes an
`atomic.Value` behind `loadStopC`/`storeStopC`: three goroutine families read it
while `Start`/`StartWithRetries` reassign it, which was a data race. The 1.6
facade also gains the `sync.Once` + nil guard 2.0.1 already had, so a double
`Stop()` or a `Stop()` before `Start()` no longer panics.

Behavior change: an inbound message still in flight when `Stop()` begins may be
dropped rather than delivered, so its callback never fires (documented on both
facades' `Stop`). This is shutdown-only — while the client is running `stopC` is
open, so each select degenerates to the blocking send it replaced. Ordering
between message classes is unaffected.

Interim: 1.6 no longer closes its `Errors()` channel on `Stop()` (matching
2.0.1), because with `error()` now preemptible a handler could otherwise take
the send arm while `Stop()` concurrently closed the channel. Both facades'
`Errors()` docstrings state the current behavior; a follow-up restores closure
as a close-after-join once the facades join their handler goroutine.

Guard: `ocpp1.6_test/lifecycle_shutdown_test.go` and
`ocpp2.0.1_test/lifecycle_shutdown_test.go` — the cancel-hook deadlock (both the
stop-drain and timeout-arm variants), a dedicated leak test per forwarding
closure, `error()` preemptibility, the double-`Stop`/`Stop`-before-`Start`
guards, and the `stopC` restart race. All watchdog-bounded so a regression fails
fast instead of wedging the suite; green under `-race -count=3`.

## Client facade generation handshake and bounded shutdown (`StopCtx`)

Neither client facade joined the async callback-handler goroutine it spawns per
`Start`. A fast `Start`→`Stop`→`Start` could therefore leave the previous
generation's handler alive alongside the new one, racing on shared state — and
the old generation's `clearCallbacks()` could drain the *new* generation's
freshly registered callbacks. A second `Start*` with no intervening `Stop` was
worse: it overwrote the fields that reach the old generation, orphaning that
handler for the process lifetime.

`Stop` now joins the handler before returning, via a per-`Start` `handlerDone`
channel closed by the handler on exit. Because a join waits on user code, an
additive `StopCtx(ctx) error` bounds it; `Stop()` is `StopCtx(context.Background())`.
On ctx expiry `StopCtx` returns `ctx.Err()` — `stopC` is already closed by then,
so a retry reaches a clean stop. `handlerDone` is pre-closed in the constructors,
so `Stop` before `Start`, and `Stop` after a failed `Start`, stay non-blocking
rather than selecting on a nil channel forever.

A second `Start*` now retires the previous generation first — closing its
`stopC` and joining its handler — uniformly across `Start` on both facades and
`StartWithRetries`. Rejecting a second start was not viable: `StartWithRetries`
returns nothing and so cannot report a rejection. Retirement reuses the same
teardown as `Stop`, including the underlying client: sharing one `ocppj.Client`
across generations otherwise leaves a new `Start` reassigning dispatcher state
while the previous message pump still reads it.

Also in this change: the 1.6 handler takes its `stopC` by parameter (closing a
residual window where a handler spawned late could bind to a later generation's
channel); `awaitCtxResult`'s `stopC` arm re-checks for an already-delivered
response before reporting the client stopped, so a completed response is not
lost to a concurrent shutdown (the `ctx.Done()` arm deliberately does not — the
caller asked to cancel); `Errors()` creation is synchronised; and both facades
document that the error channel is never closed and is process-lifetime, not
per-generation.

Behavior change: with a user callback in flight, `Stop()` now blocks until it
returns, and calling `Stop`/`StopCtx` from inside a callback self-joins and
deadlocks. Both are documented on the interfaces, along with the same caveat for
`Start`/`StartWithRetries`, which block on a restart for the same reason.

Guard: `ocpp{1.6,2.0.1}_test/lifecycle_join_test.go` and
`lifecycle_join_stopctx_test.go` (join, orphan retirement on all three start
paths, the close-AND-join contract, `StopCtx` expiry and retry, `Errors()`
race), plus white-box `ocpp{1.6,2.0.1}/lifecycle_l3_test.go` for the
response-vs-shutdown tie. The shutdown-preemption tests from the previous change
are restated in terms of the new contract — they now assert `StopCtx` returns
`DeadlineExceeded` while a callback is pinned, rather than releasing the pin,
which would have masked the very regression they exist to catch. Green under
`-race -count=3`.

## Inbound message ordering on the 2.0.1 client facade

The 2.0.1 charging-station facade drained two cap-1 channels (`responseHandler`,
`errorHandler`) with its single `asyncCallbackHandler` goroutine, but wired
`SetRequestHandler` *directly* to `handleIncomingRequest` — so an inbound CALL
ran inline on the websocket read goroutine, concurrently with, and in no defined
order relative to, the response and error callbacks. A CALL that arrived on the
wire *after* a CALL_RESULT could be handled *before* that result's callback.

All three inbound kinds now flow through one `incoming chan incomingMessage`
(cap 1) carrying a `kind` discriminator, drained by that same goroutine. This
brings 2.0.1 to parity with the 1.6 facade, which has had this shape since its
own ordering fix.

**Guarantee, stated precisely:** inbound CALL / CALL_RESULT / CALL_ERROR are
handled in **wire order**. This is not a total order over everything the facade
does — `onRequestTimeout` produces errors on the ocppj dispatcher's message pump,
an independent producer goroutine, so its errors are not ordered against inbound
frames. The 1.6 facade has the same shape and the same scope.

Behavior changes, all user-visible:

- Inbound request handlers now run on the facade's single `asyncCallbackHandler`
  goroutine, serialized with response and error callbacks — no longer concurrent
  with them, and no longer on the websocket read goroutine. Code relying on a
  request handler running in parallel with a response callback is affected.
- `Stop()`/`StopCtx` now genuinely join an in-flight request handler, and calling
  `Stop`/`StopCtx` from inside one self-deadlocks. The interface godoc already
  promised both; before this change the handler ran on the unjoined read
  goroutine, so neither was actually true.
- A synchronous `SendRequest` from inside an inbound request handler previously
  returned a `GenericError "Request timed out"` after the dispatcher's request
  timeout (30s default) and the station recovered. It now blocks until
  `Stop`/`StopCtx` — or until `ctx` expiry, with `SendRequestCtx` — because the
  only goroutine that could deliver that timeout error is the one parked in the
  handler. Documented on `SendRequest`/`SendRequestCtx`; the same was already
  true of a sync send from inside a `SendRequestAsync` callback.
- An inbound CALL still buffered in `incoming` when shutdown begins is dropped
  without a CALLERROR, so the peer times out. Deliberate: draining and replying
  at stop time would reintroduce a blocking send to a possibly-unreachable peer
  inside `Stop()`.
- Request-handler panic recovery moves from `ocppj`'s inline guard to the
  facade's own `RecoverPanicGoroutine` (same kind, action, requestID, panic sink
  and CALLERROR) — externally identical, but the facade guard is now what
  provides it, since `ocppj`'s wraps only the forwarding closure.

The channel stays cap 1, matching 1.6. Two honest consequences: cross-kind
buffering drops from two slots to one, and four producers now contend for that
one slot instead of three across two channels — so a slow request handler can
also delay outbound dispatch via a blocked `onRequestTimeout`, where before it
only stalled the read goroutine. Neither is a new hazard class (both were already
reachable via a slow response callback), and every send remains preemptible
against `stopC`. `responseEnvelope` is retired; `Errors()`/`errC` are untouched.

Guard: `ocpp2.0.1_test/inbound_ordering_test.go` — response-before-CALL and
error-before-CALL ordering, plus a structural test that handling a CALL *occupies*
the sole drainer so a later CALL_RESULT cannot be delivered concurrently (that
one rejects a response-biased two-channel drainer and a goroutine-per-CALL
request arm, which the ordering assertions alone cannot distinguish), two
round-trip no-regression tests, and the `StopCtx` unwind of a self-deadlocked
synchronous send. Plus
`ocpp2.0.1_test/panic_isolation_test.go::TestClientRequestHandlerPanicRecoveredFacade`
for the facade panic guard — including a loop-survival probe, without which a
guard that recovers correctly but kills the drain loop would pass — and
`ocpp2.0.1_test/lifecycle_shutdown_test.go::TestL2ShutdownRequestForwardingLeak`,
which is the only test that catches a non-preemptible request forwarding closure
and therefore a permanent read-goroutine leak. Green under `-race -count=3`.

## Server facade error reporting

The server facade error channels are monitoring streams. Their sends are
non-blocking, so an undrained full buffer drops errors instead of wedging the
canceled-request goroutine or a websocket read goroutine. The channels are
created in the facade constructors with a modest burst buffer and are never
closed.

| Symbol | Why keep it |
|--------|-------------|
| `ocpp1.6.centralSystem.error` | reports facade errors with a drop-on-full, non-blocking send |
| `ocpp2.0.1.csms.error` | keeps the 2.0.1 server facade behavior in parity with 1.6 |
| `ocpp1.6.newCentralSystem` / `ocpp2.0.1.newCSMS` | eagerly create `errC` with `errChanCapacity` before the endpoint is started |
| `ocpp1.6.centralSystem.Errors` / `ocpp2.0.1.csms.Errors` | returns the immutable, process-lifetime monitoring channel; consumers must drain it for life |
| `ocpp1.6.CentralSystem.Errors` / `ocpp2.0.1.CSMS.Errors` | documents drop-on-full behavior and that the buffer may contain errors from before the call |

**Guard:** `ocpp1.6_test/ec2_facade_error_test.go` and
`ocpp2.0.1_test/ec2_facade_error_test.go` cover the canceled-request-goroutine
and websocket-read paths, concurrent channel access, delivery, and pre-arm
buffering.

## Off-pump canceled-request handling (server facades)

The server facades run everything in canceled-request handling that touches
facade state — the callback-queue dequeue, the callback itself, the no-callback
`Errors()` report — on a dedicated goroutine; only the feature-name resolution
stays on the dispatcher pump.
The callback queue is guarded by a facade-wide mutex that `SendRequestAsync`
holds across the enqueue into the dispatcher, and that enqueue is a blocking
send into a bounded channel drained only by the dispatcher's message pump —
so dequeuing a canceled request's callback on the pump closes a lock cycle
that permanently wedges dispatch for every connected client and makes
`Stop`/`Shutdown` hang.

| Symbol | Why keep it |
|--------|-------------|
| `ocpp1.6.centralSystem.handleCanceledRequest` | runs everything that touches facade state — the callback-queue dequeue, the callback, the no-callback `Errors()` report — off the dispatcher pump; the feature name is resolved on the pump |
| `ocpp2.0.1.csms.handleCanceledRequest` | keeps the 2.0.1 server facade in parity with 1.6 |
| `ocpp1.6.unknownFeatureName` / `ocpp2.0.1.unknownFeatureName` | resolves the feature name before the goroutine starts, so a payload-less canceled request cannot panic in a defer argument |

**Guard:** `ocpp1.6_test/ecd1_pump_deadlock_test.go` and
`ocpp2.0.1_test/ecd1_pump_deadlock_test.go` saturate the dispatcher's request
channel behind a wedged write and then drive a cancel; a regression deadlocks
and is caught by the tests' outer watchdogs.

## Server dispatcher stop-drain cancellation (EC1)

The server dispatcher now mirrors the client's stop-drain behavior. A default
`FIFOQueueMap` is atomically detached at the pump's stop arm; pending state is
cleared before any cancellation is fired, and every well-formed detached
bundle receives one `ErrDispatcherStopped` terminal callback. The detached
per-client queues use their optional atomic `DrainAll` method when available,
with a serialized `Pop` fallback for custom queue implementations. Custom
`ServerQueueMap` implementations without the optional map-level `DrainAll`
retain the legacy `Init`-only behavior and receive no stop cancels.

The raw dispatcher callback runs on the message pump and must remain fast and
non-blocking. Completion ownership still guarantees at most one response or
cancel; a `SendRequest` that returns an error may additionally receive a
stop-cancel if `Stop` raced the enqueue. Facade callbacks are dispatched off
the pump by EC-D1, and facade `Errors()` remains best-effort rather than a
complete inventory of stop-drain cancellations. Stop-drain callbacks run
before the context-bounded shutdown phase, so a slow callback can extend
teardown beyond its context.

`CreateClient` holds the dispatcher's read lock across both its running check
and the queue creation. Splitting the two allowed a queue to be reinserted into
the map *after* the stop arm had already detached it, where the drain could not
see it and it survived into the next `Start` generation. Because `SendRequest`
pushes its bundle before its own running check, a caller that was told its send
had failed could still leave that bundle at the head of the surviving queue —
from where the next generation's pump dispatched it, putting a request on the
wire after its caller had been told it would not be sent. The window predates
the stop drain (the previous stop arm's `Init` had the identical non-atomic
reinsert) and reproduces on both the old and new stop arms in roughly a third of
iterations under contention; holding the read lock across both steps orders the
queue creation strictly before the drain, so the queue is either drained or
never created. Purging the queue map at `Start` was considered and rejected:
registering a prepared queue with `Add` before `Start` is an established way to
instrument a single client's queue, and a purge would silently discard it.

| Symbol | Why keep it |
|--------|-------------|
| `ocppj.FIFOQueueMap.DrainAll` | atomically detaches the client-to-queue map without widening `ServerQueueMap` |
| `ocppj.DefaultServerDispatcher.messagePump` stop arm | clears pending state before draining and fires guarded `ErrDispatcherStopped` cancels |
| `ocppj.DefaultServerDispatcher.CreateClient` | holds the read lock across the running check and the queue creation, so no queue can be reinserted after the stop drain detached the map |
| `ocppj.DefaultServerDispatcher.Stop` | preserves the write-lock-before-stop-signal barrier (pinned by the 1.6 facade storm); the unlock-before-pump-join barrier is pinned by the raw callback state-read test |
| `ocppj.ServerDispatcher.Stop` | documents stop cancellation, custom-map fallback, and the enqueue-race caveat |
| `ocppj.Server.Stop` / `Shutdown` | exposes the raw dispatcher stop-drain and its context-phase ordering |
| `ocpp1.6.centralSystem.Stop` / `Shutdown` + `ocpp1.6.CentralSystem` | documents scoped callback delivery, `Errors()` best-effort semantics, and off-pump facade callbacks |
| `ocpp2.0.1.csms.Stop` / `Shutdown` + `ocpp2.0.1.CSMS` | keeps the 2.0.1 facade contract in parity with 1.6 |

**Guard:** `ocppj/ec1_server_stop_cancel_test.go` covers pending and queued
multi-client drains, the off-pump completion photo finish (a staggered phase for
the genuine race plus two ordered iterations that pin the response-win and
stop-win branches deterministically, so branch coverage cannot flake), the
create-versus-drain ordering (a gated queue map parks `CreateClient` inside its
queue creation while `Stop` runs, then asserts the next generation dispatches
only its own request), the callback state-read unlock barrier, the
send-error/stop-cancel probe, the `ocppj.Server`
late-CALL_RESULT gate, panic recovery, the custom-map fallback, and the
optional concrete `DrainAll` surface. `ocpp1.6_test/ec1_server_stop_cancel_test.go`
and `ocpp2.0.1_test/ec1_server_stop_cancel_test.go` cover facade exactly-once
delivery and the 1.6 `SendRequestAsync` storm-vs-`Stop` watchdog. The panic
test intentionally does not assert `Errors()`; that routing assertion belongs
to EC3.

## Recovered handler panics on the facade error channels (EC3)

Recovered handler panics — on facade-routed paths and in the default
dispatchers — are reported on the facade `Errors()` channels as
`*ocppj.HandlerPanicError`, so they are visible by default: the `ocppj` log sink
is a no-op logger unless the consumer installs one, and `SetOnHandlerPanic` is
opt-in, so upstream turns a panicking handler into a silent `CALLERROR` loop.
Each facade constructor installs a routing callback on its endpoint; the facade
`SetOnHandlerPanic` stores the consumer's hook instead of delegating, so
registering one cannot remove the routing, and the constructor chains any hook
the caller registered on the endpoint beforehand. The report is emitted before
either hook runs, so a panicking hook cannot suppress it. Registering a hook
directly on the endpoint *after* facade construction replaces the wrapper and
disables the reporting.

| Symbol | Why keep it |
|--------|-------------|
| `ocppj.HandlerPanicError` | wraps a recovered panic for delivery on a facade error channel; consumers classify it with `errors.As` |
| `ocppj.Server.OnHandlerPanic` / `ocppj.Client.OnHandlerPanic` | lets an installed wrapper chain a previously registered hook instead of destroying it |
| `ocpp1.6.NewCentralSystem` / `ocpp2.0.1.NewCSMS` | install the panic routing wrapper on the server endpoint |
| `ocpp1.6.NewChargePoint` / `ocpp2.0.1.NewChargingStation` | install the same wrapper on the client endpoint, through the non-blocking helper |
| `ocpp1.6.chargePoint.errorNonBlocking` / `ocpp2.0.1.chargingStation.errorNonBlocking` | drop-on-full panic-only send, so the panic route cannot park the ws read goroutine, the `asyncCallbackHandler` goroutine, or the dispatcher pump on the client's blocking cap-1 error channel |
| the four facade `SetOnHandlerPanic` | store the hook rather than delegating it, so a consumer hook cannot clobber the routing |

Panics recovered inside a dispatcher — a raw `CanceledRequestHandler`, and the
`request.GetFeatureName()` resolution in the facades' `handleCanceledRequest`,
both reported as `ocppj.CancelHandlerKind` — are routed through the dispatcher's
own copy of the hook, which `SetOnHandlerPanic` propagates only to the default
dispatchers. A custom `ClientDispatcher`/`ServerDispatcher` never receives the
hook, so both of those reach neither the hook nor `Errors()`: panic reporting is
unsupported with a custom dispatcher. Everything the endpoints and facades
recover themselves is reported whichever dispatcher is in use. Server reports
use the facade's non-blocking cap-16 send and client reports are best-effort on
a cap-1 channel, and a single canceled request can yield both a
`*ocppj.HandlerPanicError` and a "no handler available" report when the
disconnect drain races the cancel goroutine, so `Errors()` stays a monitoring
stream — neither a complete panic inventory nor one entry per request.

**Guard:** `ocpp1.6_test/ec3_panic_errors_test.go` and
`ocpp2.0.1_test/ec3_panic_errors_test.go` cover default visibility, hook-plus-
channel delivery, both clobber directions, a panicking hook, an undrained storm,
the stop-drain hand-off, and the client route's non-blocking discipline.

**Release note:** Recovered handler panics — those the facades recover on their
own paths, plus those the default dispatchers recover internally — are now
reported on the facade `Errors()` channels as `*ocppj.HandlerPanicError`.
Consumers draining `Errors()` will start seeing these values; classify them
with `errors.As`. Registering a hook directly on an `ocppj.Server`/`ocppj.Client`
*after* handing it to a facade constructor disables the reporting — register it
on the facade instead. Panics recovered inside a custom
`ClientDispatcher`/`ServerDispatcher` are not reported; panic reporting is only
supported with the default dispatchers.

## Vendored OCA JSON schemas

`schemas/v201/`, `schemas/v16/base/` and `schemas/v16/security/` hold the Open Charge
Alliance's published OCPP 2.0.1 and OCPP 1.6 JSON schemas exactly as OCA distributes
them. No file under `schemas/` may ever be edited, reformatted, or otherwise modified —
any correction this project needs to a schema's declared constraints belongs in this
project's own Go code, never in the schema file itself.

| File | Why keep it |
|------|-------------|
| `schemas/NOTICE` | records the license these files are redistributed under and the provenance of each vendored set |
| `schemas/SHA256SUMS` | the per-file integrity manifest that proves the vendored tree still matches what was fetched |
| `scripts/fetch-schemas.sh` | re-fetches and verifies the schemas against a fresh OCA download, and rebuilds the vendored tree from nothing if that is ever needed |

**Guard:** `schemas/schemas_test.go` hashes every vendored file against
`schemas/SHA256SUMS` and fails the build the moment a byte changes.

## OCPP 2.0.1 generated messages

Four OCPP 2.0.1 messages — Heartbeat, BootNotification, SetChargingProfile and
TransactionEvent — plus every shared type they reach are now generated from the vendored OCA
schemas by `internal/codegen`, replacing their hand-written declarations. Generated shared
types live in `ocpp2.0.1/types/types_gen.go`; `ocpp2.0.1/types/types.go` keeps only the
declarations the generator does not own (the shared validator plumbing among them; `DateTime` stays in its own pre-existing file),
and `CustomData` moved to its own file. Regenerating is deterministic and byte-stable:

```
go run ./internal/codegen -manifest internal/codegen/config/v201.yaml
```

reproduces the committed tree byte-for-byte; a dirty diff after running it means the tree
was edited by hand and the edit belongs in the generator's configs instead.

The generated code follows the schema wherever the hand-written tree disagreed with it.
Every accept/reject and wire-shape difference this introduces is captured, classified and
test-enforced — the authoritative record is the exception manifests under
`ocpp2.0.1_test/testdata/parity/` (classes `FORK_BUG`, `OVERRIDE_CANDIDATE`,
`SCHEMA_FAITHFUL_CHANGE`, `STRUCT_VALIDATOR`; every row carries a citation), compared on
every test run against accept/reject and wire goldens recorded from the pre-swap tree at a
pinned commit. Semantic changes worth naming in prose:

- `ComponentVariable.Variable` is now `*Variable` with `omitempty` (optional), as the schema
  declares — the hand-written tree required it as a value, stricter than the schema.
- `ChargingProfile.ID` and `ChargingProfile.StackLevel` are now rejected when omitted — the
  schema marks both required; the hand-written tags (`gte=0`, no `required`) silently
  accepted the zero-value omission.
- Known limitation, recorded not fixed: a `required` token on a non-pointer numeric field
  (e.g. `ChargingSchedulePeriod.StartPeriod`) cannot distinguish an explicit, valid `0` from
  an absent field — validator.v9 sees the same zero value for both.
- Struct-level validators are carried forward only from a closed, named list
  (`allowedStructValidatorRows` in `ocpp2.0.1_test/validator_parity_test.go`); no other
  struct validator survives onto generated types unannounced.

| File | Why keep it |
|------|-------------|
| `ocpp2.0.1/types/types_gen.go` | every generated shared type; regenerated, never hand-edited |
| `internal/codegen/config/{v201,transform,overrides}.yaml` | the manifest, naming rules and audited per-field overrides that fully determine the generated output |
| `ocpp2.0.1_test/testdata/parity/` | recorded goldens + the exception manifests that authorize every intentional divergence |

**Guard:** the `ocpp2.0.1_test` harness bundle — wire parity against recorded goldens,
validator (accept/reject) parity with the closed exception classes above, round-trip and
new-representation coverage, allocation benchmarks, and completeness guards that rebuild the
generator's IR in-test so harness coverage cannot drift from the manifest.
`ocpp2.0.1/types/validator_tags_test.go` additionally proves every validate token the
generated code uses is registered on the shared validator.
