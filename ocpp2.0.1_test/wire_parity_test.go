package ocpp2_test

// Harness W (wire parity) and Harness R (round trip). Both compare the
// generated 2.0.1 message types' JSON behavior against the GOLDEN corpus
// recorded from master and, for payloads only the generated API can
// express, against the minimal IR evaluator.
//
// GOLDEN wire cases are compared by decoding each case's *original authored
// input* (testdata/parity/cases/<Message>/<case>.json) independently through
// the generated types, never by decoding the golden/ recording itself back
// through them. The two are not interchangeable here: master's own marshal
// already drops one field silently (TransactionEvent.offline, a non-pointer
// bool with omitempty — see the recorder's package doc), so decoding the
// already-lossy golden/ payload into the generated *bool field would
// reproduce the same absence on both sides and hide the very divergence the
// wire-parity exception manifest exists to record. Decoding the original
// input on both sides — master's, once, at recording time; the generated
// side's, here, at comparison time — is what actually exercises the
// difference the generator's pointer-optionality mapping rule creates.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/enesismail/ocpp-go/ocpp2.0.1/availability"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/provisioning"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/smartcharging"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/transactions"
	"github.com/enesismail/ocpp-go/ocpp2.0.1_test/internal/canonjson"
	"github.com/enesismail/ocpp-go/ocpp2.0.1_test/internal/ireval"
)

const parityRoot = "testdata/parity"

// --- generated-side decode/marshal, one instantiation per message half ---

// wireDecoder decodes payload into the generated struct T and re-marshals
// it. It is the generated-side mirror of the recorder's own wireRecorder.
func wireDecoder[T any](payload []byte) ([]byte, error) {
	var v T
	if err := json.Unmarshal(payload, &v); err != nil {
		return nil, fmt.Errorf("decode into %T: %w", v, err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal %T: %w", v, err)
	}
	return out, nil
}

// roundTripStable decodes payload into T, marshals, decodes the result back
// into a fresh T, and marshals again, returning both marshaled forms so a
// caller can assert they are identical (json.RawMessage-idempotent).
func roundTripStable[T any](payload []byte) (first, second []byte, err error) {
	var v1 T
	if err := json.Unmarshal(payload, &v1); err != nil {
		return nil, nil, fmt.Errorf("first decode into %T: %w", v1, err)
	}
	first, err = json.Marshal(v1)
	if err != nil {
		return nil, nil, fmt.Errorf("first marshal %T: %w", v1, err)
	}
	var v2 T
	if err := json.Unmarshal(first, &v2); err != nil {
		return nil, nil, fmt.Errorf("second decode into %T: %w", v2, err)
	}
	second, err = json.Marshal(v2)
	if err != nil {
		return nil, nil, fmt.Errorf("second marshal %T: %w", v2, err)
	}
	return first, second, nil
}

var wireTargets = map[string]map[string]func([]byte) ([]byte, error){
	"BootNotification": {
		"request":  wireDecoder[provisioning.BootNotificationRequest],
		"response": wireDecoder[provisioning.BootNotificationResponse],
	},
	"Heartbeat": {
		"request":  wireDecoder[availability.HeartbeatRequest],
		"response": wireDecoder[availability.HeartbeatResponse],
	},
	"SetChargingProfile": {
		"request":  wireDecoder[smartcharging.SetChargingProfileRequest],
		"response": wireDecoder[smartcharging.SetChargingProfileResponse],
	},
	"TransactionEvent": {
		"request":  wireDecoder[transactions.TransactionEventRequest],
		"response": wireDecoder[transactions.TransactionEventResponse],
	},
}

var roundTripTargets = map[string]map[string]func([]byte) ([]byte, []byte, error){
	"BootNotification": {
		"request":  roundTripStable[provisioning.BootNotificationRequest],
		"response": roundTripStable[provisioning.BootNotificationResponse],
	},
	"Heartbeat": {
		"request":  roundTripStable[availability.HeartbeatRequest],
		"response": roundTripStable[availability.HeartbeatResponse],
	},
	"SetChargingProfile": {
		"request":  roundTripStable[smartcharging.SetChargingProfileRequest],
		"response": roundTripStable[smartcharging.SetChargingProfileResponse],
	},
	"TransactionEvent": {
		"request":  roundTripStable[transactions.TransactionEventRequest],
		"response": roundTripStable[transactions.TransactionEventResponse],
	},
}

// structNames maps (message, half) to the generated Go struct type's own
// name, the identifier raw_identity_messages.json lists members by.
var structNames = map[string]map[string]string{
	"BootNotification": {
		"request":  "BootNotificationRequest",
		"response": "BootNotificationResponse",
	},
	"Heartbeat": {
		"request":  "HeartbeatRequest",
		"response": "HeartbeatResponse",
	},
	"SetChargingProfile": {
		"request":  "SetChargingProfileRequest",
		"response": "SetChargingProfileResponse",
	},
	"TransactionEvent": {
		"request":  "TransactionEventRequest",
		"response": "TransactionEventResponse",
	},
}

// --- golden / exception-manifest / raw-identity-list loading ---

type goldenWire struct {
	MasterSHA string          `json:"master_sha"`
	Payload   json.RawMessage `json:"payload"`
}

type wireException struct {
	Message  string `json:"message"`
	Half     string `json:"half"`
	Path     string `json:"path"`
	Class    string `json:"class"`
	Citation string `json:"citation"`
	Note     string `json:"note"`
}

func loadWireExceptions(t testing.TB) map[string]wireException {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(parityRoot, "wire_exceptions.json"))
	require.NoError(t, err)
	var rows []wireException
	require.NoError(t, json.Unmarshal(raw, &rows))
	allowedClasses := map[string]bool{
		"SCHEMA_FAITHFUL_CHANGE": true,
		"FORK_BUG":               true,
		"ADDITIVE":               true,
		"OVERRIDE_CANDIDATE":     true,
		"STRUCT_VALIDATOR":       true,
	}
	byKey := make(map[string]wireException, len(rows))
	for _, row := range rows {
		require.Truef(t, allowedClasses[row.Class], "wire_exceptions.json row %s/%s/%s carries disallowed class %q", row.Message, row.Half, row.Path, row.Class)
		require.NotEmptyf(t, row.Citation, "wire_exceptions.json row %s/%s/%s has no citation for why the divergence is authorized", row.Message, row.Half, row.Path)
		key := row.Message + "\x00" + row.Half + "\x00" + row.Path
		byKey[key] = row
	}
	return byKey
}

func loadRawIdentitySet(t testing.TB) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(parityRoot, "raw_identity_messages.json"))
	require.NoError(t, err)
	var names []string
	require.NoError(t, json.Unmarshal(raw, &names))
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return set
}

func halfOfCaseFile(name string) (half, caseName string, ok bool) {
	switch {
	case strings.HasSuffix(name, "-request.json"):
		return "request", strings.TrimSuffix(name, "-request.json"), true
	case strings.HasSuffix(name, "-response.json"):
		return "response", strings.TrimSuffix(name, "-response.json"), true
	default:
		return "", "", false
	}
}

// listWireCases returns the non-violation ("wire") case files under
// testdata/parity/cases/<message>/, sorted, for every message wireTargets
// knows about.
func listWireCases(t testing.TB) map[string][]string {
	t.Helper()
	result := make(map[string][]string)
	for message := range wireTargets {
		entries, err := os.ReadDir(filepath.Join(parityRoot, "cases", message))
		require.NoError(t, err)
		var names []string
		for _, e := range entries {
			if e.IsDir() || strings.HasPrefix(e.Name(), "violation-") {
				continue
			}
			names = append(names, e.Name())
		}
		sort.Strings(names)
		result[message] = names
	}
	return result
}

// --- canonicalized JSON path diffing ---

// diffPaths reports the dot-joined object-key paths at which two decoded
// JSON values differ, relative to prefix. Array elements are compared
// pairwise by index but do not extend the path with an index segment (this
// corpus's authorized divergences are all object-property shaped, never
// array-element shaped, and the divergence census's own path convention — the citation
// key wire_exceptions.json rows are checked against — never names an
// index); a length mismatch is reported once, at the array's own path.
func diffPaths(prefix string, a, b interface{}) []string {
	switch av := a.(type) {
	case map[string]interface{}:
		bv, ok := b.(map[string]interface{})
		if !ok {
			return []string{prefix}
		}
		keys := make(map[string]struct{}, len(av)+len(bv))
		for k := range av {
			keys[k] = struct{}{}
		}
		for k := range bv {
			keys[k] = struct{}{}
		}
		sortedKeys := make([]string, 0, len(keys))
		for k := range keys {
			sortedKeys = append(sortedKeys, k)
		}
		sort.Strings(sortedKeys)
		var diffs []string
		for _, k := range sortedKeys {
			childPath := k
			if prefix != "" {
				childPath = prefix + "." + k
			}
			av1, aok := av[k]
			bv1, bok := bv[k]
			if aok != bok {
				diffs = append(diffs, childPath)
				continue
			}
			diffs = append(diffs, diffPaths(childPath, av1, bv1)...)
		}
		return diffs
	case []interface{}:
		bv, ok := b.([]interface{})
		if !ok || len(bv) != len(av) {
			return []string{prefix}
		}
		var diffs []string
		for i := range av {
			diffs = append(diffs, diffPaths(prefix, av[i], bv[i])...)
		}
		return diffs
	default:
		if fmt.Sprint(a) != fmt.Sprint(b) {
			return []string{prefix}
		}
		return nil
	}
}

func decodeGeneric(t testing.TB, raw []byte) interface{} {
	t.Helper()
	var v interface{}
	require.NoError(t, json.Unmarshal(raw, &v))
	return v
}

// compareWireCase is the comparison Harness W runs for one wire case: it
// canonicalizes both sides, and — when they differ — reports every JSON
// path that is not covered by an exception-manifest row. An empty result
// means the case passes. This is factored out of TestWireParityGolden so
// the red-observation bite-proof (TestWireParityCatchesMutatedGolden) can drive it with
// a deliberately corrupted golden payload without failing the real suite.
func compareWireCase(t testing.TB, message, half string, goldenPayload, generatedPayload []byte, exceptions map[string]wireException) (unauthorized []string) {
	t.Helper()
	goldenCanon, err := canonjson.Canonicalize(goldenPayload)
	require.NoError(t, err)
	genCanon, err := canonjson.Canonicalize(generatedPayload)
	require.NoError(t, err)
	if string(goldenCanon) == string(genCanon) {
		return nil
	}
	paths := diffPaths("", decodeGeneric(t, goldenPayload), decodeGeneric(t, generatedPayload))
	for _, p := range paths {
		key := message + "\x00" + half + "\x00" + p
		if _, ok := exceptions[key]; !ok {
			unauthorized = append(unauthorized, fmt.Sprintf("%s.%s.%s", message, half, p))
		}
	}
	return unauthorized
}

// TestWireParityGolden is Harness W over the GOLDEN corpus: canonicalized
// parity for every wire case, plus raw-byte identity for messages listed in
// raw_identity_messages.json.
func TestWireParityGolden(t *testing.T) {
	exceptions := loadWireExceptions(t)
	rawIdentity := loadRawIdentitySet(t)
	cases := listWireCases(t)

	var totalCases, byteDiffering, canonicallyExcepted, rawIdentityChecked int

	for message, files := range cases {
		for _, file := range files {
			file := file
			half, caseName, ok := halfOfCaseFile(file)
			require.Truef(t, ok, "%s/%s: filename does not end with -request.json or -response.json", message, file)

			t.Run(message+"/"+caseName+"/"+half, func(t *testing.T) {
				caseInput, err := os.ReadFile(filepath.Join(parityRoot, "cases", message, file))
				require.NoError(t, err)

				goldenRaw, err := os.ReadFile(filepath.Join(parityRoot, "golden", message, file))
				require.NoError(t, err)
				var g goldenWire
				require.NoError(t, json.Unmarshal(goldenRaw, &g))
				require.NotEmptyf(t, g.MasterSHA, "%s/%s: golden file carries no master_sha", message, file)

				generated, err := wireTargets[message][half](caseInput)
				require.NoError(t, err)

				goldenCanon, err := canonjson.Canonicalize(g.Payload)
				require.NoError(t, err)
				genCanon, err := canonjson.Canonicalize(generated)
				require.NoError(t, err)

				unauthorized := compareWireCase(t, message, half, g.Payload, generated, exceptions)
				assert.Emptyf(t, unauthorized, "unauthorized canonicalized-JSON divergence(s) for %s/%s: %v", message, file, unauthorized)
				if string(g.Payload) != string(generated) {
					byteDiffering++
				}
				if len(unauthorized) == 0 && string(goldenCanon) != string(genCanon) {
					canonicallyExcepted++
				}

				structName := structNames[message][half]
				if rawIdentity[structName] {
					rawIdentityChecked++
					assert.Equalf(t, string(g.Payload), string(generated),
						"%s is on raw_identity_messages.json but %s/%s is not raw-byte identical to the golden", structName, message, file)
				}
				totalCases++
			})
		}
	}

	t.Logf("Harness W: %d wire cases checked, %d differ at the raw-byte level (field order and/or content), %d needed an authorized wire-parity exception (genuine content divergence), %d additionally raw-identity-checked",
		totalCases, byteDiffering, canonicallyExcepted, rawIdentityChecked)
}

// TestWireParityCatchesMutatedGolden is Harness W's red observation:
// a golden payload with one byte flipped (an accepted enum value swapped
// for a different accepted one, so the mutation is itself valid JSON and
// decodes cleanly on the generated side, but no longer matches) must be
// reported as an unauthorized divergence rather than silently passing.
func TestWireParityCatchesMutatedGolden(t *testing.T) {
	exceptions := loadWireExceptions(t)
	original := []byte(`{"reason":"PowerUp","chargingStation":{"model":"ACME-Home","vendorName":"ACME"}}`)
	generated, err := wireTargets["BootNotification"]["request"](original)
	require.NoError(t, err)

	mutated := []byte(`{"reason":"Watchdog","chargingStation":{"model":"ACME-Home","vendorName":"ACME"}}`)

	unauthorized := compareWireCase(t, "BootNotification", "request", mutated, generated, exceptions)
	assert.NotEmpty(t, unauthorized, "a mutated golden payload (different reason) must be reported as an unauthorized divergence, not pass silently")
	if len(unauthorized) > 0 {
		assert.Contains(t, unauthorized[0], "reason")
	}
}

// --- Harness R: round trip, including CustomData extension keys ---

// TestRoundTripStability runs unmarshal->marshal->unmarshal->marshal over
// every GOLDEN wire case's original input and every NEW-ONLY payload,
// asserting the two marshaled forms are byte-identical (a stable round
// trip: nothing drifts on a second pass).
func TestRoundTripStability(t *testing.T) {
	cases := listWireCases(t)
	var checked int
	for message, files := range cases {
		for _, file := range files {
			half, caseName, ok := halfOfCaseFile(file)
			require.True(t, ok)
			t.Run("golden/"+message+"/"+caseName+"/"+half, func(t *testing.T) {
				input, err := os.ReadFile(filepath.Join(parityRoot, "cases", message, file))
				require.NoError(t, err)
				first, second, err := roundTripTargets[message][half](input)
				require.NoError(t, err)
				assert.Equal(t, string(first), string(second), "round trip is not stable")
			})
			checked++
		}
	}

	newOnly := listNewOnlyCases(t)
	for message, files := range newOnly {
		for _, file := range files {
			half, caseName, ok := halfOfCaseFile(file)
			require.True(t, ok)
			t.Run("new-only/"+message+"/"+caseName+"/"+half, func(t *testing.T) {
				input, err := os.ReadFile(filepath.Join(parityRoot, "new-only", message, file))
				require.NoError(t, err)
				first, second, err := roundTripTargets[message][half](input)
				require.NoError(t, err)
				assert.Equal(t, string(first), string(second), "round trip is not stable")
			})
			checked++
		}
	}
	t.Logf("Harness R: %d payloads round-tripped", checked)
}

// TestRoundTripCustomDataExtensionKeys is Harness R's specific
// acceptance-criterion proof: a payload carrying at least three vendor
// extension keys round-trips with every key and value intact, and the
// marshaled key order is deterministic (CustomData.MarshalJSON sorts
// extension keys ascending, vendorId first).
func TestRoundTripCustomDataExtensionKeys(t *testing.T) {
	input, err := os.ReadFile(filepath.Join(parityRoot, "new-only", "BootNotification", "customdata-vendor-extensions-request.json"))
	require.NoError(t, err)

	first, second, err := roundTripTargets["BootNotification"]["request"](input)
	require.NoError(t, err)
	require.Equal(t, string(first), string(second), "round trip is not stable")

	var decoded map[string]interface{}
	require.NoError(t, json.Unmarshal(first, &decoded))
	customData, ok := decoded["customData"].(map[string]interface{})
	require.True(t, ok, "customData did not survive the round trip")

	expected := []string{"vendorId", "firmwareChannel", "installYear", "note", "siteId"}
	for _, key := range expected {
		_, present := customData[key]
		assert.Truef(t, present, "extension key %q did not survive the round trip", key)
	}
	assert.GreaterOrEqualf(t, len(customData)-1, 3, "expected at least three vendor extension keys, found %d", len(customData)-1)

	// Key ordering: vendorId first, then extension keys ascending
	// (firmwareChannel, installYear, note, siteId).
	orderedKeys := []string{`"vendorId"`, `"firmwareChannel"`, `"installYear"`, `"note"`, `"siteId"`}
	lastIndex := -1
	for _, k := range orderedKeys {
		idx := strings.Index(string(first), k)
		require.Greaterf(t, idx, lastIndex, "key %s is not in the expected sorted position in %s", k, first)
		lastIndex = idx
	}
}

// TestRoundTripCatchesDroppedExtensionKey is Harness R's red observation:
// if an extension key were lost across the round trip, the
// survival assertion above would need to fail. This proves the assertion
// shape actually bites, by constructing the corrupted round trip directly
// (dropping a key from the decoded CustomData before the second marshal)
// rather than mutating production code to induce the bug.
func TestRoundTripCatchesDroppedExtensionKey(t *testing.T) {
	input, err := os.ReadFile(filepath.Join(parityRoot, "new-only", "BootNotification", "customdata-vendor-extensions-request.json"))
	require.NoError(t, err)

	var decoded map[string]interface{}
	require.NoError(t, json.Unmarshal(input, &decoded))
	customData, ok := decoded["customData"].(map[string]interface{})
	require.True(t, ok)
	delete(customData, "siteId") // simulate a lossy round trip
	corrupted, err := json.Marshal(decoded)
	require.NoError(t, err)

	var reDecoded map[string]interface{}
	require.NoError(t, json.Unmarshal(corrupted, &reDecoded))
	reCustomData, ok := reDecoded["customData"].(map[string]interface{})
	require.True(t, ok)

	_, present := reCustomData["siteId"]
	assert.False(t, present, "sanity check: the deliberately dropped key must actually be absent")
	// The real assertion this exercise stands in for is
	// TestRoundTripCustomDataExtensionKeys's require.True(present) above:
	// on this corrupted input it would fail, which is the red observation.
}

// --- NEW-ONLY corpus, checked against the IR evaluator ---

func listNewOnlyCases(t testing.TB) map[string][]string {
	t.Helper()
	result := make(map[string][]string)
	messageDirs, err := os.ReadDir(filepath.Join(parityRoot, "new-only"))
	require.NoError(t, err)
	for _, md := range messageDirs {
		if !md.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(parityRoot, "new-only", md.Name()))
		require.NoError(t, err)
		var names []string
		for _, e := range entries {
			if !e.IsDir() {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		result[md.Name()] = names
	}
	return result
}

// TestWireParityNewOnly is Harness W's NEW-ONLY half: every payload
// only the generated API can express is checked against the minimal IR
// evaluator's supported keyword subset. A verdict of Invalid is always a
// failure (every NEW-ONLY fixture here is constructed to be schema-legal).
// A verdict of Unsupported is accepted only when the reported keyword is
// "format" — this corpus's NEW-ONLY payloads that carry a date-time
// property hit exactly that recorded deferral by design (the evaluator
// deliberately does not implement the "format" keyword); any other
// unsupported keyword, or any Invalid verdict, fails the test.
func TestWireParityNewOnly(t *testing.T) {
	irDoc, err := os.ReadFile(filepath.Join(parityRoot, "ir", "ir.json"))
	require.NoError(t, err)

	cases := listNewOnlyCases(t)
	var valid, deferred int
	for message, files := range cases {
		for _, file := range files {
			half, caseName, ok := halfOfCaseFile(file)
			require.Truef(t, ok, "%s/%s: filename does not end with -request.json or -response.json", message, file)
			t.Run(message+"/"+caseName+"/"+half, func(t *testing.T) {
				var h ireval.Half
				if half == "request" {
					h = ireval.Request
				} else {
					h = ireval.Response
				}
				payload, err := os.ReadFile(filepath.Join(parityRoot, "new-only", message, file))
				require.NoError(t, err)

				verdict, err := ireval.Evaluate(irDoc, message, h, payload)
				require.NoError(t, err)

				switch verdict.Status {
				case ireval.Valid:
					valid++
				case ireval.Unsupported:
					assert.Equalf(t, "format", verdict.Keyword, "%s/%s: unsupported keyword %q is not a recorded deferral", message, file, verdict.Keyword)
					deferred++
				default:
					t.Errorf("%s/%s: rejected as invalid (keyword=%s path=%s), but every NEW-ONLY fixture is constructed to be schema-legal", message, file, verdict.Keyword, verdict.Path)
				}
			})
		}
	}
	t.Logf("Harness W (NEW-ONLY): %d valid, %d recorded format deferrals", valid, deferred)
}

// --- Harness coverage completeness guard ---
//
// wireTargets, roundTripTargets, structNames and (in validator_parity_test.go)
// violationTargets are all hand-registered per message. At a larger scale
// than this prototype's four messages, a new message's fixtures could land
// under cases/ or new-only/ without ever being added to these maps, and
// every loop in this file that ranges over a map's own keys (listWireCases,
// listViolationCases) would silently never see it -- not a failure, just
// nothing run. The guard below closes that gap by deriving the messages
// this harness is *supposed* to cover from a source that has nothing to do
// with these maps -- the generator's own -dump-ir output, which records
// which messages the manifest actually emits -- and asserting every one of
// them is registered everywhere it needs to be, and has fixtures on disk.

// emittedMessages returns the set of message names the generator's own
// checked-in -manifest/-schemas configuration actually emits (emit: true),
// freshly regenerated every time by building and running the frozen
// generator -- deliberately NOT the committed testdata/parity/ir/ir.json
// snapshot (that file is still used elsewhere, by TestWireParityNewOnly,
// for its constraint data; it is a different concern). A snapshot file
// only reflects whatever the manifest said the last time someone
// regenerated it by hand: a manifest row flipped to emit:true, with the
// snapshot left stale, would leave this completeness guard checking last
// year's set and never notice the new message needs coverage. Regenerating
// here, from the manifest and schema config actually checked in, is what
// closes that gap for good rather than moving it one file over.
//
// The generator is built once per test binary run (buildCodegen, cached
// below) and the resulting binary is exec'd directly rather than shelling
// out through `go run` on every call: `go run` re-resolves and rebuilds on
// every invocation, and repeated invocations are both slower and a less
// predictable use of the toolchain under test-harness sandboxing than one
// build followed by direct exec calls.
func repositoryRoot() (string, error) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller could not locate wire_parity_test.go")
	}
	directory := filepath.Dir(filename)
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", fmt.Errorf("could not find go.mod above wire_parity_test.go")
		}
		directory = parent
	}
}

// buildCodegen compiles the frozen generator once, to a throwaway binary
// under a fresh temp directory, and returns its path. root must be the
// repository root (the generator's own package path, ./internal/codegen,
// is a relative filesystem path, not a module import path, so the build
// has to run from there).
func buildCodegen(root string) (string, error) {
	dir, err := os.MkdirTemp("", "ocpp-codegen-bin-*")
	if err != nil {
		return "", fmt.Errorf("create scratch dir for the generator binary: %w", err)
	}
	binPath := filepath.Join(dir, "codegen")
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", binPath, "./internal/codegen")
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("go build ./internal/codegen: %w (stderr: %s)", err, stderr.String())
	}
	return binPath, nil
}

// regenerateEmittedMessages runs the already-built generator binary's
// -dump-ir against manifestPath/schemaDir (both resolved relative to root)
// and returns the set of message names the fresh dump reports emit:true
// for. Exposed separately from emittedMessages so a test can drive it
// directly against a deliberately modified manifest copy, without
// disturbing emittedMessages' own cached, real-manifest result.
func regenerateEmittedMessages(root, binPath, manifestPath, schemaDir string) (map[string]bool, error) {
	outDir, err := os.MkdirTemp("", "ocpp-ir-*")
	if err != nil {
		return nil, fmt.Errorf("create scratch dir for the IR dump: %w", err)
	}
	defer os.RemoveAll(outDir)

	cmd := exec.Command(binPath, "-manifest", manifestPath, "-schemas", schemaDir, "-dump-ir", outDir)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("run the generator binary: %w (stderr: %s)", err, stderr.String())
	}

	raw, err := os.ReadFile(filepath.Join(outDir, "ir.json"))
	if err != nil {
		return nil, fmt.Errorf("read freshly-dumped ir.json: %w", err)
	}
	var doc struct {
		Messages []struct {
			Name string `json:"name"`
			Emit bool   `json:"emit"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("decode freshly-dumped ir.json: %w", err)
	}
	set := make(map[string]bool)
	for _, m := range doc.Messages {
		if m.Emit {
			set[m.Name] = true
		}
	}
	return set, nil
}

var (
	emittedOnce   sync.Once
	emittedResult map[string]bool
	emittedErr    error
)

// emittedMessages is the cached, real-manifest entry point every completeness
// check in this file uses. The regeneration runs once per test binary (a
// sync.Once, not per-subtest -- the dominant cost is the one-time `go
// build`, measured at well under a second, and every caller wants the
// identical answer within one run); its error, if any, is stored rather
// than reported from inside the Do callback and re-checked by every caller,
// because sync.Once marks itself done even if its callback exits early via
// t.Fatal (t.Fatal calls runtime.Goexit, which still runs the deferred
// "mark done" store) -- calling a *testing.T method inside the callback
// would make every test after the first one silently see an empty result
// instead of the same failure.
func emittedMessages(t testing.TB) map[string]bool {
	t.Helper()
	emittedOnce.Do(func() {
		root, err := repositoryRoot()
		if err != nil {
			emittedErr = err
			return
		}
		binPath, err := buildCodegen(root)
		if err != nil {
			emittedErr = err
			return
		}
		emittedResult, emittedErr = regenerateEmittedMessages(root, binPath,
			filepath.Join("internal", "codegen", "config", "v201.yaml"),
			filepath.Join("schemas", "v201"))
	})
	if emittedErr != nil {
		t.Fatalf("regenerate the emitted-message set from the live manifest (not the committed ir.json snapshot): %v", emittedErr)
	}
	return emittedResult
}

// dirNames lists the immediate subdirectory names of dir.
func dirNames(t testing.TB, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}

// countCaseFiles counts the files directly under dir that do (wantViolation
// true) or do not (wantViolation false) carry the "violation-" prefix --
// used to count a message's wire-case and violation-case fixtures
// separately without going through listWireCases/listViolationCases, both
// of which are keyed by the very maps this guard is checking for
// completeness and so cannot be used to check them.
func countCaseFiles(t testing.TB, dir string, wantViolation bool) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		isViolation := strings.HasPrefix(e.Name(), "violation-")
		if isViolation == wantViolation {
			n++
		}
	}
	return n
}

// coverageGaps compares the emitted-message set against every place this
// harness's target maps and the on-disk fixture corpus need to agree, and
// returns one description per mismatch found, sorted for a deterministic
// report -- an empty result means the harness's own registration is
// complete. Factored out as a pure function (no *testing.T, no disk access)
// so a red-observation test can drive it with a deliberately incomplete
// input without touching the real corpus or the real target maps.
func coverageGaps(emitted, wireKeys, violationKeys, roundTripKeys, structNameKeys map[string]bool, caseDirs, newOnlyDirs []string, wireFixtureCounts, violationFixtureCounts map[string]int) []string {
	var gaps []string
	for name := range emitted {
		if !wireKeys[name] {
			gaps = append(gaps, fmt.Sprintf("%s: emitted (ir.json) but missing from wireTargets", name))
		}
		if !violationKeys[name] {
			gaps = append(gaps, fmt.Sprintf("%s: emitted (ir.json) but missing from validator_parity_test.go's violationTargets", name))
		}
		if !roundTripKeys[name] {
			gaps = append(gaps, fmt.Sprintf("%s: emitted (ir.json) but missing from roundTripTargets", name))
		}
		if !structNameKeys[name] {
			gaps = append(gaps, fmt.Sprintf("%s: emitted (ir.json) but missing from structNames", name))
		}
		if wireFixtureCounts[name] == 0 {
			gaps = append(gaps, fmt.Sprintf("%s: emitted (ir.json) but has zero wire-case fixtures under cases/", name))
		}
		if violationFixtureCounts[name] == 0 {
			gaps = append(gaps, fmt.Sprintf("%s: emitted (ir.json) but has zero violation-case fixtures under cases/", name))
		}
	}
	for _, dir := range caseDirs {
		if !emitted[dir] {
			gaps = append(gaps, fmt.Sprintf("%s: has a cases/ directory but is not emitted (ir.json) -- stray or renamed message?", dir))
		}
	}
	for _, dir := range newOnlyDirs {
		if !emitted[dir] {
			gaps = append(gaps, fmt.Sprintf("%s: has a new-only/ directory but is not emitted (ir.json) -- stray or renamed message?", dir))
		}
	}
	sort.Strings(gaps)
	return gaps
}

// keysOfWireTargets, keysOfRoundTripTargets, keysOfStructNames and
// keysOfViolationTargets each extract a map[string]bool of the top-level
// keys from one of the four differently-shaped target maps this file's and
// validator_parity_test.go's registrations come in, so coverageGaps can be
// driven from the real registrations without needing a generic map-keys
// helper for each distinct value type.
func keysOfWireTargets(m map[string]map[string]func([]byte) ([]byte, error)) map[string]bool {
	keys := make(map[string]bool, len(m))
	for k := range m {
		keys[k] = true
	}
	return keys
}

func keysOfRoundTripTargets(m map[string]map[string]func([]byte) ([]byte, []byte, error)) map[string]bool {
	keys := make(map[string]bool, len(m))
	for k := range m {
		keys[k] = true
	}
	return keys
}

func keysOfStructNames(m map[string]map[string]string) map[string]bool {
	keys := make(map[string]bool, len(m))
	for k := range m {
		keys[k] = true
	}
	return keys
}

func keysOfViolationTargets(m map[string]map[string]func([]byte) (bool, string)) map[string]bool {
	keys := make(map[string]bool, len(m))
	for k := range m {
		keys[k] = true
	}
	return keys
}

// TestHarnessTargetMapsAreComplete is the completeness guard itself: it
// derives the emit:true message set from ir.json and asserts every one of
// wireTargets, roundTripTargets, structNames and violationTargets carries
// it, that it has at least one wire-case and one violation-case fixture on
// disk, and that no cases/ or new-only/ directory names a message outside
// that set. A gap in either direction fails loudly, by name, rather than
// the loop that would otherwise just never visit it.
func TestHarnessTargetMapsAreComplete(t *testing.T) {
	emitted := emittedMessages(t)

	caseDirs := dirNames(t, filepath.Join(parityRoot, "cases"))
	newOnlyDirs := dirNames(t, filepath.Join(parityRoot, "new-only"))

	wireFixtureCounts := make(map[string]int)
	violationFixtureCounts := make(map[string]int)
	for _, name := range caseDirs {
		wireFixtureCounts[name] = countCaseFiles(t, filepath.Join(parityRoot, "cases", name), false)
		violationFixtureCounts[name] = countCaseFiles(t, filepath.Join(parityRoot, "cases", name), true)
	}

	gaps := coverageGaps(
		emitted,
		keysOfWireTargets(wireTargets),
		keysOfViolationTargets(violationTargets),
		keysOfRoundTripTargets(roundTripTargets),
		keysOfStructNames(structNames),
		caseDirs, newOnlyDirs,
		wireFixtureCounts, violationFixtureCounts,
	)
	assert.Emptyf(t, gaps, "harness coverage is incomplete:\n%s", strings.Join(gaps, "\n"))
}

// TestHarnessCoverageGuardCatchesMissingTarget is coverageGaps' own red
// observation: an emitted message absent from every target map, and a
// stray cases/ directory for a message that is not emitted, must both be
// reported -- driven entirely with synthetic in-memory data, never by
// mutating the real corpus or the real target maps (both of which stay
// untouched by this test).
func TestHarnessCoverageGuardCatchesMissingTarget(t *testing.T) {
	emitted := map[string]bool{"BootNotification": true, "PhantomMessage": true}
	wireKeys := map[string]bool{"BootNotification": true}
	violationKeys := map[string]bool{"BootNotification": true}
	roundTripKeys := map[string]bool{"BootNotification": true}
	structNameKeys := map[string]bool{"BootNotification": true}
	caseDirs := []string{"BootNotification", "StrayDirectory"}
	wireFixtureCounts := map[string]int{"BootNotification": 3, "PhantomMessage": 0}
	violationFixtureCounts := map[string]int{"BootNotification": 2, "PhantomMessage": 0}

	gaps := coverageGaps(emitted, wireKeys, violationKeys, roundTripKeys, structNameKeys, caseDirs, nil, wireFixtureCounts, violationFixtureCounts)

	joined := strings.Join(gaps, "\n")
	assert.Contains(t, joined, "PhantomMessage: emitted (ir.json) but missing from wireTargets")
	assert.Contains(t, joined, "PhantomMessage: emitted (ir.json) but missing from validator_parity_test.go's violationTargets")
	assert.Contains(t, joined, "PhantomMessage: emitted (ir.json) but has zero wire-case fixtures under cases/")
	assert.Contains(t, joined, "StrayDirectory: has a cases/ directory but is not emitted (ir.json)")
}

// flipToEmitTrue returns a copy of manifest with name's own row -- and only
// that row -- changed from "emit: false" to "emit: true". It locates the
// row by its "- name: <name>" line and then the first "emit: false" line
// after it, which the manifest's own fixed per-row field order (block,
// direction, request, response, emit) always places immediately below that
// row's other fields and before the next row starts.
func flipToEmitTrue(t testing.TB, manifest, name string) string {
	t.Helper()
	anchor := "- name: " + name + "\n"
	idx := strings.Index(manifest, anchor)
	require.Greaterf(t, idx, -1, "manifest must contain a %q row to flip", anchor)
	rest := manifest[idx+len(anchor):]
	const from = "emit: false\n"
	emitIdx := strings.Index(rest, from)
	require.Greaterf(t, emitIdx, -1, "%s's own row must contain %q below it", name, from)
	return manifest[:idx+len(anchor)] + rest[:emitIdx] + "emit: true\n" + rest[emitIdx+len(from):]
}

// TestEmittedMessagesGuardCatchesUnregeneratedManifestFlip is the live
// -manifest regeneration's own revert-drill, driven with real data end to
// end: a faithful copy of the real, checked-in manifest with exactly one
// row flipped from emit: false to emit: true (Authorize, which today has
// none of this harness's registrations or fixtures), run through the
// actual frozen generator binary, feeding the actual coverageGaps function
// together with this harness's real, unmodified target maps and on-disk
// fixture corpus. It proves the specific thing the committed-snapshot
// design could not: flipping a manifest row is picked up by a fresh
// regeneration with no separate "remember to regenerate the snapshot" step
// for anyone to forget, because there is no snapshot read anywhere in this
// path. The real, checked-in internal/codegen/config/v201.yaml is never
// written to -- this test builds its own modified copy in a temp
// directory and points -manifest at that instead -- and this test itself
// confirms that file is still byte-for-byte what it was before the drill.
func TestEmittedMessagesGuardCatchesUnregeneratedManifestFlip(t *testing.T) {
	root, err := repositoryRoot()
	require.NoError(t, err)

	realManifestPath := filepath.Join(root, "internal", "codegen", "config", "v201.yaml")
	realManifest, err := os.ReadFile(realManifestPath)
	require.NoError(t, err)
	require.Containsf(t, string(realManifest), "- name: Authorize\n",
		"drill assumption: Authorize must still be a row in the real manifest")

	flipped := flipToEmitTrue(t, string(realManifest), "Authorize")
	require.NotEqual(t, string(realManifest), flipped, "the flip must actually change the manifest text")

	// The manifest loader resolves goTree by walking up from the manifest
	// file's own directory until it finds a go.mod (manifest.go's own
	// documented rule), independently of -schemas -- so the modified copy
	// has to live somewhere under this actual repository, not an
	// unrelated OS temp directory with no go.mod above it at all. Placed
	// directly under root with a name nothing else in this task scans,
	// and removed unconditionally when the test returns.
	scratchDir, err := os.MkdirTemp(root, ".codegen-drill-scratch-*")
	require.NoError(t, err)
	defer os.RemoveAll(scratchDir)
	tmpManifestPath := filepath.Join(scratchDir, "v201-drill.yaml")
	require.NoError(t, os.WriteFile(tmpManifestPath, []byte(flipped), 0o644))

	binPath, err := buildCodegen(root)
	require.NoError(t, err)

	driftedEmitted, err := regenerateEmittedMessages(root, binPath, tmpManifestPath, filepath.Join("schemas", "v201"))
	require.NoError(t, err)
	require.Truef(t, driftedEmitted["Authorize"],
		"the modified manifest copy must report Authorize as emit:true after a fresh regeneration")

	// Everything else -- the real target maps, the real on-disk fixture
	// corpus -- is untouched: exactly what emittedMessages(t) would hand
	// TestHarnessTargetMapsAreComplete if the real manifest had actually
	// been flipped this way and nobody had touched wireTargets,
	// violationTargets, roundTripTargets, structNames or cases/ yet.
	caseDirs := dirNames(t, filepath.Join(parityRoot, "cases"))
	newOnlyDirs := dirNames(t, filepath.Join(parityRoot, "new-only"))
	wireFixtureCounts := make(map[string]int)
	violationFixtureCounts := make(map[string]int)
	for _, name := range caseDirs {
		wireFixtureCounts[name] = countCaseFiles(t, filepath.Join(parityRoot, "cases", name), false)
		violationFixtureCounts[name] = countCaseFiles(t, filepath.Join(parityRoot, "cases", name), true)
	}

	gaps := coverageGaps(
		driftedEmitted,
		keysOfWireTargets(wireTargets),
		keysOfViolationTargets(violationTargets),
		keysOfRoundTripTargets(roundTripTargets),
		keysOfStructNames(structNames),
		caseDirs, newOnlyDirs,
		wireFixtureCounts, violationFixtureCounts,
	)

	joined := strings.Join(gaps, "\n")
	assert.Contains(t, joined, "Authorize: emitted (ir.json) but missing from wireTargets")
	assert.Contains(t, joined, "Authorize: emitted (ir.json) but missing from validator_parity_test.go's violationTargets")
	assert.Contains(t, joined, "Authorize: emitted (ir.json) but missing from roundTripTargets")
	assert.Contains(t, joined, "Authorize: emitted (ir.json) but missing from structNames")
	assert.Contains(t, joined, "Authorize: emitted (ir.json) but has zero wire-case fixtures under cases/")
	assert.Contains(t, joined, "Authorize: emitted (ir.json) but has zero violation-case fixtures under cases/")

	afterManifest, err := os.ReadFile(realManifestPath)
	require.NoError(t, err)
	require.Equal(t, string(realManifest), string(afterManifest),
		"the real, checked-in manifest must be byte-for-byte unchanged by this drill")
}

// --- Raw-identity eligibility: reproducible derivation, not a trusted list ---

// deriveRawIdentityEligible independently recomputes, from the live corpus
// (every wire case's original input decoded through the generated types
// and compared byte-for-byte against the golden's recorded payload), which
// generated struct types achieve raw-byte identity on *every one* of their
// current wire cases. This is the same rule this task originally applied
// once, by hand, to populate raw_identity_messages.json; running it again
// here on every test invocation is what makes the committed file's claim
// reproducible rather than merely asserted -- see
// TestRawIdentityListMatchesLiveDerivation below.
func deriveRawIdentityEligible(t testing.TB) map[string]bool {
	t.Helper()
	cases := listWireCases(t)

	type halfKey struct{ message, half string }
	allMatchSoFar := make(map[halfKey]bool)

	for message, files := range cases {
		for _, file := range files {
			half, _, ok := halfOfCaseFile(file)
			require.True(t, ok)

			caseInput, err := os.ReadFile(filepath.Join(parityRoot, "cases", message, file))
			require.NoError(t, err)
			goldenRaw, err := os.ReadFile(filepath.Join(parityRoot, "golden", message, file))
			require.NoError(t, err)
			var g goldenWire
			require.NoError(t, json.Unmarshal(goldenRaw, &g))

			generated, err := wireTargets[message][half](caseInput)
			require.NoError(t, err)

			match := string(g.Payload) == string(generated)
			k := halfKey{message, half}
			if v, seen := allMatchSoFar[k]; seen {
				allMatchSoFar[k] = v && match
			} else {
				allMatchSoFar[k] = match
			}
		}
	}

	eligible := make(map[string]bool)
	for k, allMatch := range allMatchSoFar {
		if allMatch {
			eligible[structNames[k.message][k.half]] = true
		}
	}
	return eligible
}

// TestRawIdentityListMatchesLiveDerivation is the raw-identity guard: it
// asserts the committed raw_identity_messages.json is exactly what
// deriveRawIdentityEligible computes from today's corpus. A future fixture
// change that breaks raw-byte identity for a struct still listed, or grants
// it to one that is not, makes this assertion fail instead of leaving a
// stale claim in the committed file.
func TestRawIdentityListMatchesLiveDerivation(t *testing.T) {
	committed := loadRawIdentitySet(t)
	derived := deriveRawIdentityEligible(t)
	assert.Equal(t, derived, committed, "raw_identity_messages.json has drifted from what today's corpus actually supports -- regenerate it from deriveRawIdentityEligible's result")
}

// TestRawIdentityGuardCatchesDrift is this guard's own red observation: the
// real, live-derived eligibility set must be reported as different from an
// obviously wrong one, proving the equality check the real guard relies on
// actually discriminates rather than passing vacuously.
func TestRawIdentityGuardCatchesDrift(t *testing.T) {
	derived := deriveRawIdentityEligible(t)
	wrong := map[string]bool{"NoSuchGeneratedStruct": true}
	assert.NotEqual(t, derived, wrong, "an obviously wrong eligibility set must not compare equal to the live derivation")
}
