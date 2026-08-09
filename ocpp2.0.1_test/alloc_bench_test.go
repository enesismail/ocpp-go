package ocpp2_test

// Harness A (allocation profile), generated side. Three shapes — a minimal
// message (Heartbeat), a mid message (BootNotification), and the hot-path
// shape a charging station's meter reporting actually produces on the wire
// (a TransactionEvent carrying several MeterValues, each with several
// SampledValues) — each benchmarked for construction, marshal and
// unmarshal, in ocppj/bench_test.go's own style (b.ReportAllocs,
// b.ResetTimer after setup).
//
// The old side lives in alloc_bench_master_test.go, gated behind the
// masterbench build tag: it references types this branch's swap removed,
// so it can only ever compile against a `master` worktree (see that file's
// own doc comment for why, and the two-checkout mechanics it uses). Both
// files' construct* helpers are meant to be mechanical mirrors of each
// other — same field order, same literal values taken from the same golden
// fixture, no logic beyond assignment — so the comparison is of the two
// APIs' shapes, not of two differently-chosen workloads.
//
// No threshold is asserted here, and nothing here is tuned to change a
// number. These are measurements, not tests with a pass/fail bar; the
// red-observation bite-proof for this harness (a smoke assertion that the
// numbers are real) lives in TestAllocBenchmarksProduceRealMeasurements
// below.

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

// sinkHeartbeatResponse, sinkBootNotificationRequest and
// sinkTransactionEventRequest are package-level "sinks": each construct
// benchmark below assigns its final loop result to one of these instead of
// merely discarding it. Without a sink, a trivial single-allocation
// constructor gets inlined into the benchmark loop and Go's escape
// analysis can then prove the result never leaves the loop body, stack-
// allocating it and reporting a false zero allocs/op — a well-known
// benchmarking pitfall, not a property of the constructors themselves.
// Assigning to a package-level variable forces the value to escape, so the
// reported allocation count reflects what a real caller (who does keep the
// result) would pay.
var (
	sinkHeartbeatResponse       *availability.HeartbeatResponse
	sinkBootNotificationRequest *provisioning.BootNotificationRequest
	sinkTransactionEventRequest *transactions.TransactionEventRequest
)

func loadGoldenPayload(tb testing.TB, message, file string) []byte {
	tb.Helper()
	raw, err := os.ReadFile(filepath.Join(parityRoot, "golden", message, file))
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

// --- shape 1: Heartbeat (minimal message) ---

func constructHeartbeatResponse() *availability.HeartbeatResponse {
	return &availability.HeartbeatResponse{
		CurrentTime: types.NewDateTime(time.Date(2026, 8, 8, 9, 15, 0, 0, time.UTC)),
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
	if v.CurrentTime == nil {
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
		ChargingStation: provisioning.ChargingStation{
			SerialNumber:    "CS-042",
			Model:           "ACME-Station",
			VendorName:      "ACME",
			FirmwareVersion: "2.4.0",
			Modem: &provisioning.Modem{
				Iccid: "89010000000000000002",
			},
		},
		Reason: provisioning.BootReasonTypeScheduledReset,
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
		EventType:     transactions.TransactionEventTypeUpdated,
		Timestamp:     types.NewDateTime(time.Date(2026, 8, 8, 8, 45, 0, 0, time.UTC)),
		TriggerReason: transactions.TriggerReasonTypeMeterValuePeriodic,
		SeqNo:         2,
		TransactionInfo: transactions.Transaction{
			TransactionID: "tx-hot-01",
			ChargingState: transactions.ChargingStateTypeCharging,
		},
		EVSE: &types.EVSE{ID: 1},
		MeterValue: []types.MeterValue{
			{
				Timestamp: types.NewDateTime(time.Date(2026, 8, 8, 8, 44, 0, 0, time.UTC)),
				SampledValue: []types.SampledValue{
					{Value: 8.75, Measurand: types.MeasurandTypeEnergyActiveImportRegister},
					{Value: 230.1, Measurand: types.MeasurandTypeVoltage},
				},
			},
			{
				Timestamp: types.NewDateTime(time.Date(2026, 8, 8, 8, 45, 0, 0, time.UTC)),
				SampledValue: []types.SampledValue{
					{Value: 8.8, Measurand: types.MeasurandTypeEnergyActiveImportRegister},
					{Value: 230, Measurand: types.MeasurandTypeVoltage},
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

// TestAllocBenchmarksProduceRealMeasurements is Harness A's smoke check:
// there is no threshold to assert and no wrong answer to construct for a
// benchmark, only a check that a real one was produced. It runs each
// benchmark function directly through
// testing.Benchmark and asserts every one completed at least one iteration
// and reports finite, non-zero allocations — proving the benchmarks
// execute and measure something real, not that any number is good or bad.
func TestAllocBenchmarksProduceRealMeasurements(t *testing.T) {
	benchmarks := map[string]func(*testing.B){
		"ConstructHeartbeatResponse":              BenchmarkConstructHeartbeatResponse,
		"UnmarshalHeartbeatResponse":              BenchmarkUnmarshalHeartbeatResponse,
		"MarshalHeartbeatResponse":                BenchmarkMarshalHeartbeatResponse,
		"ConstructBootNotificationRequest":        BenchmarkConstructBootNotificationRequest,
		"UnmarshalBootNotificationRequest":        BenchmarkUnmarshalBootNotificationRequest,
		"MarshalBootNotificationRequest":          BenchmarkMarshalBootNotificationRequest,
		"ConstructTransactionEventRequestHotPath": BenchmarkConstructTransactionEventRequestHotPath,
		"UnmarshalTransactionEventRequestHotPath": BenchmarkUnmarshalTransactionEventRequestHotPath,
		"MarshalTransactionEventRequestHotPath":   BenchmarkMarshalTransactionEventRequestHotPath,
	}
	for name, fn := range benchmarks {
		name, fn := name, fn
		t.Run(name, func(t *testing.T) {
			result := testing.Benchmark(fn)
			if result.N == 0 {
				t.Fatalf("%s: benchmark completed zero iterations", name)
			}
			if result.AllocsPerOp() == 0 {
				t.Fatalf("%s: reported zero allocs/op — expected a real, non-zero allocation count", name)
			}
			if result.AllocedBytesPerOp() == 0 {
				t.Fatalf("%s: reported zero B/op — expected a real, non-zero byte count", name)
			}
			t.Logf("%s: N=%d allocs/op=%d B/op=%d ns/op=%d", name, result.N, result.AllocsPerOp(), result.AllocedBytesPerOp(), result.NsPerOp())
		})
	}
}
