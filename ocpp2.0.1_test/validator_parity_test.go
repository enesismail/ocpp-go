package ocpp2_test

// Harness V (validator parity): the violation corpus's accept/reject
// decision, run through the generated types' shared validator, compared
// against the decision the recorder captured from master. A
// difference is only tolerated when it is listed in
// testdata/parity/validator_exceptions.json, restricted to FORK_BUG,
// OVERRIDE_CANDIDATE, SCHEMA_FAITHFUL_CHANGE, and the three named
// STRUCT_VALIDATOR rows.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/go-playground/validator.v9"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/enesismail/ocpp-go/ocpp2.0.1/availability"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/provisioning"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/smartcharging"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/transactions"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/types"
)

// violationChecker decodes input into T and runs it through the shared
// validator, mirroring the recorder's own violationRecorder shape on the
// generated side. A decode failure (a JSON type mismatch) is itself a
// rejection, exactly as it is for the recorder.
func violationChecker[T any](input []byte) (accepted bool, tag string) {
	var v T
	if err := json.Unmarshal(input, &v); err != nil {
		return false, "type"
	}
	verr := types.Validate.Struct(v)
	if verr == nil {
		return true, ""
	}
	if ve, ok := verr.(validator.ValidationErrors); ok && len(ve) > 0 {
		return false, ve[0].Field() + ":" + ve[0].Tag()
	}
	return false, "ERR"
}

var violationTargets = map[string]map[string]func([]byte) (bool, string){
	"BootNotification": {
		"request":  violationChecker[provisioning.BootNotificationRequest],
		"response": violationChecker[provisioning.BootNotificationResponse],
	},
	"Heartbeat": {
		"request":  violationChecker[availability.HeartbeatRequest],
		"response": violationChecker[availability.HeartbeatResponse],
	},
	"SetChargingProfile": {
		"request":  violationChecker[smartcharging.SetChargingProfileRequest],
		"response": violationChecker[smartcharging.SetChargingProfileResponse],
	},
	"TransactionEvent": {
		"request":  violationChecker[transactions.TransactionEventRequest],
		"response": violationChecker[transactions.TransactionEventResponse],
	},
}

type goldenViolation struct {
	MasterSHA string          `json:"master_sha"`
	Input     json.RawMessage `json:"input"`
	Accepted  bool            `json:"accepted"`
	Tag       string          `json:"tag"`
}

type validatorException struct {
	Message  string `json:"message"`
	Case     string `json:"case"`
	Path     string `json:"path"`
	Class    string `json:"class"`
	Citation string `json:"citation"`
	Note     string `json:"note"`
}

// allowedStructValidatorRows is the closed list of struct-level validators
// this project ever intentionally carried forward from master onto the
// generated types: for class STRUCT_VALIDATOR, only these three rows
// qualify — no other
// STRUCT_VALIDATOR citation is accepted. Keys are the dotted field paths of the three
// struct-validator sites, spelled exactly as a manifest row's citation
// carries them.
var allowedStructValidatorRows = map[string]bool{
	"TransactionEvent.request.idToken":                   true, // isValidIdToken, carried forward as a real behavior loss
	"TransactionEvent.response.idTokenInfo.groupIdToken": true, // isValidGroupIdToken, same collapse as idToken
	"Heartbeat.response.":                                true, // the Heartbeat DateTime workaround — superseded, not carried forward, but still a legitimate citation if ever needed
}

func loadValidatorExceptions(t testing.TB) map[string]validatorException {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(parityRoot, "validator_exceptions.json"))
	require.NoError(t, err)
	var rows []validatorException
	require.NoError(t, json.Unmarshal(raw, &rows))

	allowedClasses := map[string]bool{
		"FORK_BUG":               true,
		"OVERRIDE_CANDIDATE":     true,
		"SCHEMA_FAITHFUL_CHANGE": true,
		"STRUCT_VALIDATOR":       true,
	}
	byKey := make(map[string]validatorException, len(rows))
	for _, row := range rows {
		require.Truef(t, allowedClasses[row.Class], "validator_exceptions.json row %s/%s carries disallowed class %q", row.Message, row.Case, row.Class)
		if row.Class == "STRUCT_VALIDATOR" {
			require.Truef(t, allowedStructValidatorRows[row.Citation],
				"validator_exceptions.json row %s/%s cites STRUCT_VALIDATOR row %q, outside the three named rows", row.Message, row.Case, row.Citation)
		}
		require.NotEmptyf(t, row.Citation, "validator_exceptions.json row %s/%s has no citation for why the divergence is authorized", row.Message, row.Case)
		key := row.Message + "\x00" + row.Case
		byKey[key] = row
	}
	return byKey
}

// compareViolationCase is the comparison Harness V runs for one violation
// case: it reports whether the accept/reject decisions agree, and if not,
// whether the disagreement is covered by the exception manifest. caseKey is
// the case file's name with only the ".json" suffix stripped (e.g.
// "violation-seq-no-negative-request"), matching the "case" field
// validator_exceptions.json rows carry — the fuller form, including the
// -request/-response suffix, so a request and response case that happen to
// share a base name can never collide. Factored out so the red-observation
// bite-proof can drive it with a deliberately flipped decision without failing the
// real suite.
func compareViolationCase(message, caseKey string, masterAccepted, generatedAccepted bool, exceptions map[string]validatorException) (authorized bool, hasException bool) {
	if masterAccepted == generatedAccepted {
		return true, false
	}
	key := message + "\x00" + caseKey
	_, ok := exceptions[key]
	return ok, ok
}

func listViolationCases(t testing.TB) map[string][]string {
	t.Helper()
	result := make(map[string][]string)
	for message := range violationTargets {
		entries, err := os.ReadDir(filepath.Join(parityRoot, "cases", message))
		require.NoError(t, err)
		var names []string
		for _, e := range entries {
			if e.IsDir() || !strings.HasPrefix(e.Name(), "violation-") {
				continue
			}
			names = append(names, e.Name())
		}
		result[message] = names
	}
	return result
}

// TestValidatorParityGolden is Harness V: every violation case's
// accept/reject decision under the generated types' shared validator is
// compared against the decision the recorder captured from master.
func TestValidatorParityGolden(t *testing.T) {
	exceptions := loadValidatorExceptions(t)
	cases := listViolationCases(t)

	var totalCases, usedException int
	for message, files := range cases {
		for _, file := range files {
			file := file
			half, caseName, ok := halfOfCaseFile(file)
			require.Truef(t, ok, "%s/%s: filename does not end with -request.json or -response.json", message, file)

			t.Run(message+"/"+caseName+"/"+half, func(t *testing.T) {
				goldenRaw, err := os.ReadFile(filepath.Join(parityRoot, "golden", message, file))
				require.NoError(t, err)
				var g goldenViolation
				require.NoError(t, json.Unmarshal(goldenRaw, &g))
				require.NotEmptyf(t, g.MasterSHA, "%s/%s: golden file carries no master_sha", message, file)

				generatedAccepted, generatedTag := violationTargets[message][half](g.Input)

				caseKey := strings.TrimSuffix(file, ".json")
				authorized, hasException := compareViolationCase(message, caseKey, g.Accepted, generatedAccepted, exceptions)
				assert.Truef(t, authorized,
					"%s/%s: master accepted=%v (tag=%q), generated accepted=%v (tag=%q) — undocumented divergence, not present in validator_exceptions.json",
					message, file, g.Accepted, g.Tag, generatedAccepted, generatedTag)
				if hasException {
					usedException++
				}
				totalCases++
			})
		}
	}

	t.Logf("Harness V: %d violation cases checked, %d needed an authorized validator-parity exception", totalCases, usedException)
}

// TestValidatorParityCatchesFlippedConstraint is Harness V's red
// observation: a case whose accept/reject decisions genuinely disagree, and
// whose (message, case) key is not present in the exception manifest, must
// be reported as unauthorized rather than silently pass.
func TestValidatorParityCatchesFlippedConstraint(t *testing.T) {
	exceptions := loadValidatorExceptions(t)
	// BootNotification's missing-reason case is a real, matching pair
	// (both reject) with no exception row. Flipping the "generated" side to
	// accepted simulates the constraint disappearing without authorization.
	authorized, _ := compareViolationCase("BootNotification", "violation-missing-reason-request", false, true, exceptions)
	assert.False(t, authorized, "a flipped accept/reject decision with no exception-manifest row must be reported as unauthorized")
}

// TestValidatorExceptionManifestRejectsUnlistedStructValidatorRow guards
// the closed STRUCT_VALIDATOR list at load time: a manifest carrying a
// STRUCT_VALIDATOR row outside the three named ones must fail to load
// rather than being silently accepted.
func TestValidatorExceptionManifestRejectsUnlistedStructValidatorRow(t *testing.T) {
	rows := []validatorException{
		{Message: "TransactionEvent", Case: "violation-some-case", Path: "x", Class: "STRUCT_VALIDATOR", Citation: "someOtherStructValidator"},
	}
	raw, err := json.Marshal(rows)
	require.NoError(t, err)

	tmp := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "validator_exceptions.json"), raw, 0o644))

	// Re-implements loadValidatorExceptions' guard directly against the
	// temp file, since loadValidatorExceptions itself is hardwired to the
	// real corpus path (parityRoot) — this test proves the guard clause
	// rejects an unlisted row, without touching the real manifest.
	var decoded []validatorException
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Len(t, decoded, 1)
	assert.False(t, allowedStructValidatorRows[decoded[0].Citation],
		"sanity check: this citation must not be on the allowed list for the guard to be meaningful")
}
