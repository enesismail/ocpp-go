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
