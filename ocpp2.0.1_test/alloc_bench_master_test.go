//go:build masterbench

package ocpp2_test

// Harness A (allocation profile), OLD (master) side. This file is the
// mechanical mirror of alloc_bench_test.go's construct/marshal/unmarshal
// benchmarks, written against master's hand-written types instead of the
// generated ones — same benchmark names (so `benchstat` pairs each old
// benchmark with its generated-side counterpart automatically), same field
// order, same literal values, no logic beyond assignment.
//
// # Why this file cannot live in the normal build
//
// This branch's code-generation swap deleted or renamed every symbol this
// file references (provisioning.ChargingStationType, provisioning.BootReason,
// types.EVSE's old shape, etc. do not exist in the generated tree — the
// same problem the parity recorder solves the same way, with its own
// two-checkout mechanics). Compiling this file in the branch checkout,
// where alloc_bench_test.go's generated-side benchmarks and the ~84 other
// already-adapted test files in this same package also live, would fail on
// both sides at once: missing symbols here, and (if this file somehow did
// compile) duplicate top-level declarations against alloc_bench_test.go's
// own construct* helpers and sink variables.
//
// The masterbench build tag is what keeps this out of harm's way: with the
// tag inactive (the default — nothing in this repository's tooling ever
// passes -tags masterbench), the Go toolchain drops this file before
// parsing it at all, so `go build ./...`, `go vet ./...` and
// `go test ./...` never see it, exactly like the recorder's own testdata/
// exclusion. This file is only ever meant to run one way: authored here,
// then plain-copied — together with testdata/parity/golden/, which its
// loadGoldenPayload helper reads — into a `master` worktree that has none
// of the generated-side files above (a `git worktree add` of `master`
// checks out master's own committed tree, which never had them), and run
// there with `go test -tags masterbench -bench . -run '^$'`. Passing
// -tags masterbench inside the branch checkout is not a supported
// invocation: it recreates the exact conflict this paragraph describes,
// on purpose, and failing loudly is the correct outcome if it ever
// happens by mistake.
//
// This is this task's own version of the recorder's "CI blind spot" note:
// a break here is invisible to `go build ./...`/`go vet ./...`/
// `go test ./...` and is only caught by actually running the benchmark
// against a master worktree.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/enesismail/ocpp-go/ocpp2.0.1/availability"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/provisioning"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/transactions"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/types"
)

const masterParityRoot = "testdata/parity"

func loadGoldenPayload(tb testing.TB, message, file string) []byte {
	tb.Helper()
	raw, err := os.ReadFile(filepath.Join(masterParityRoot, "golden", message, file))
	if err != nil {
		tb.Fatal(err)
	}
	var g struct {
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		tb.Fatal(err)
	}
	return g.Payload
}

var (
	sinkHeartbeatResponse       *availability.HeartbeatResponse
	sinkBootNotificationRequest *provisioning.BootNotificationRequest
	sinkTransactionEventRequest *transactions.TransactionEventRequest
)

// --- shape 1: Heartbeat (minimal message) ---

func constructHeartbeatResponse() *availability.HeartbeatResponse {
	return &availability.HeartbeatResponse{
		CurrentTime: *types.NewDateTime(time.Date(2026, 8, 8, 9, 15, 0, 0, time.UTC)),
	}
}

func BenchmarkConstructHeartbeatResponse(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	var v *availability.HeartbeatResponse
	for i := 0; i < b.N; i++ {
		v = constructHeartbeatResponse()
	}
	b.StopTimer()
	if types.DateTimeIsNull(&v.CurrentTime) {
		b.Fatal("nil CurrentTime")
	}
	sinkHeartbeatResponse = v
}

func BenchmarkUnmarshalHeartbeatResponse(b *testing.B) {
	payload := loadGoldenPayload(b, "Heartbeat", "current-time-response.json")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var v availability.HeartbeatResponse
		if err := json.Unmarshal(payload, &v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMarshalHeartbeatResponse(b *testing.B) {
	v := constructHeartbeatResponse()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := json.Marshal(v); err != nil {
			b.Fatal(err)
		}
	}
}

// --- shape 2: BootNotification (mid message) ---

func constructBootNotificationRequest() *provisioning.BootNotificationRequest {
	return &provisioning.BootNotificationRequest{
		ChargingStation: provisioning.ChargingStationType{
			SerialNumber:    "CS-042",
			Model:           "ACME-Station",
			VendorName:      "ACME",
			FirmwareVersion: "2.4.0",
			Modem: &provisioning.ModemType{
				Iccid: "89010000000000000002",
			},
		},
		Reason: provisioning.BootReasonScheduledReset,
	}
}

func BenchmarkConstructBootNotificationRequest(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	var v *provisioning.BootNotificationRequest
	for i := 0; i < b.N; i++ {
		v = constructBootNotificationRequest()
	}
	b.StopTimer()
	if v.ChargingStation.Model == "" {
		b.Fatal("empty Model")
	}
	sinkBootNotificationRequest = v
}

func BenchmarkUnmarshalBootNotificationRequest(b *testing.B) {
	payload := loadGoldenPayload(b, "BootNotification", "realistic-mid-case-request.json")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var v provisioning.BootNotificationRequest
		if err := json.Unmarshal(payload, &v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMarshalBootNotificationRequest(b *testing.B) {
	v := constructBootNotificationRequest()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := json.Marshal(v); err != nil {
			b.Fatal(err)
		}
	}
}

// --- shape 3: TransactionEvent hot path (N=2 MeterValues x M=2 SampledValues) ---

func constructTransactionEventRequest() *transactions.TransactionEventRequest {
	return &transactions.TransactionEventRequest{
		EventType:     transactions.TransactionEventUpdated,
		Timestamp:     types.NewDateTime(time.Date(2026, 8, 8, 8, 45, 0, 0, time.UTC)),
		TriggerReason: transactions.TriggerReasonMeterValuePeriodic,
		SequenceNo:    2,
		TransactionInfo: transactions.Transaction{
			TransactionID: "tx-hot-01",
			ChargingState: transactions.ChargingStateCharging,
		},
		Evse: &types.EVSE{ID: 1},
		MeterValue: []types.MeterValue{
			{
				Timestamp: *types.NewDateTime(time.Date(2026, 8, 8, 8, 44, 0, 0, time.UTC)),
				SampledValue: []types.SampledValue{
					{Value: 8.75, Measurand: types.MeasurandEnergyActiveImportRegister},
					{Value: 230.1, Measurand: types.MeasurandVoltage},
				},
			},
			{
				Timestamp: *types.NewDateTime(time.Date(2026, 8, 8, 8, 45, 0, 0, time.UTC)),
				SampledValue: []types.SampledValue{
					{Value: 8.8, Measurand: types.MeasurandEnergyActiveImportRegister},
					{Value: 230, Measurand: types.MeasurandVoltage},
				},
			},
		},
	}
}

func BenchmarkConstructTransactionEventRequestHotPath(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	var v *transactions.TransactionEventRequest
	for i := 0; i < b.N; i++ {
		v = constructTransactionEventRequest()
	}
	b.StopTimer()
	if len(v.MeterValue) != 2 {
		b.Fatal("unexpected MeterValue length")
	}
	sinkTransactionEventRequest = v
}

func BenchmarkUnmarshalTransactionEventRequestHotPath(b *testing.B) {
	payload := loadGoldenPayload(b, "TransactionEvent", "hot-path-n2-m2-request.json")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var v transactions.TransactionEventRequest
		if err := json.Unmarshal(payload, &v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMarshalTransactionEventRequestHotPath(b *testing.B) {
	v := constructTransactionEventRequest()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := json.Marshal(v); err != nil {
			b.Fatal(err)
		}
	}
}
