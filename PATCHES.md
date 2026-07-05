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
