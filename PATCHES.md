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
| `ocppj/dispatcher.go:240` | request-timeout path builds `newRequestTimeoutError(bundle.Call.UniqueId)` instead of a bare `GenericError` | actually emits the tag on timeout |

**Guard:** `ocppj/request_timeout_test.go` asserts the *property* (a timeout matches the
sentinel; a plain `GenericError` CALLERROR and an untagged `Error` do not), so it survives
refactors but fails the moment the sentinel is dropped or `Error.Is` is re-flattened. It
runs under the race gate in CI (`.github/workflows/gate.yaml`, added in `0df5cca`).

> Line numbers are current as of the entries above; if the API moves, update this table
> and the guard test together. The property test is the real backstop — the line numbers
> are only a navigation aid.
