// Command recorder produces the GOLDEN corpus under
// ocpp2.0.1_test/testdata/parity/golden/ by reading the authored case
// definitions under ocpp2.0.1_test/testdata/parity/cases/, constructing
// master's hand-written structs from them, and recording what master's
// json.Marshal and types.Validate produce.
//
// This command is meant to be built and run from a `master` worktree,
// against the hand-written types the code-generation work this harness
// verifies eventually replaces: authored on the generated-tree branch,
// copied into a `master` worktree, run there, and its output copied back.
// Its own source imports nothing but the master packages it reads
// (provisioning, availability, transactions, smartcharging, types) plus
// encoding/json and os/exec, so it needs nothing else present on the
// worktree side.
//
// # Corpus layout: cases/ vs. golden/, and the -request/-response split
//
// ocpp2.0.1_test/testdata/parity/cases/<Message>/ holds the authored,
// immutable case inputs this command reads; ocpp2.0.1_test/testdata/parity/golden/
// (this command's -out) holds what it records from them. A case file's name
// is <case-name>-request.json or <case-name>-response.json — a suffix on
// the case name, not a request/ or response/ subdirectory — so that
// <Message>/ stays a single flat listing per message and a case's request
// and response fixtures (when a case has both) sort next to each other.
// <case-name> is independent per suffix: a "-request.json" case
// (minimal-required-only, all-optionals-populated, a realistic mid-case,
// and for TransactionEvent a hot-path shape) is not required to have a
// "-response.json" sibling of the same name, and the response-side cases
// added for wire coverage (accepted, rejected-with-status-info,
// with-id-token-info, current-time) exist only as "-response.json" files.
// This command writes exactly one output file per input file, at the same
// relative path under -out that the input has under -case-dir.
//
// # The violation- prefix
//
// A case file named violation-<description>-request.json or
// violation-<description>-response.json is a *violation* case: its payload
// is master-shaped (it decodes into the same struct any other case in its
// directory does) but deliberately fails master's validation — a wrong
// enum value, a missing required field, a value past a maxLength/minLength/
// minimum/maximum/minItems/maxItems bound, a JSON type mismatch, or (for
// BootNotificationResponse specifically) the negative-interval override
// case. Every other case file is a *wire* case: master accepts it, and this
// command's job for it is to record the marshaled bytes, not the rejection.
// The prefix is how this command tells the two apart without a second
// directory level: it reads every case file in a message's directory, and
// routes each one to the violation path or the wire path by checking this
// one prefix.
//
// A "JSON type mismatch" violation case (evseId or seqNo carrying a string,
// chargingStation carrying a string instead of an object) fails to decode
// into master's strongly-typed struct at all — encoding/json reports a
// json.UnmarshalTypeError rather than handing this command a value to run
// types.Validate.Struct against. That is itself the rejection: this command
// records accepted=false with tag "type" for a case whose JSON does not
// decode, exactly as it records accepted=false with the validator's own tag
// for a case that decodes but fails a validation rule. Both are "master
// rejects this payload"; only the layer that rejects it differs.
//
// # A known, deliberate corpus gap: minLength
//
// The violation corpus covers one case per constraint kind per message
// where that kind is expressible in the message's schema-derived struct
// tags: maxLength, minimum, maximum, minItems, maxItems, enum membership,
// missing required, wrong type, plus the interval:-1 override case on
// BootNotificationResponse. maximum's one case is TransactionEvent's
// violation-maximum-charging-priority-response.json, tripping
// IdTokenInfo.ChargingPriority's max=9 bound with a value of 10 — every
// other kind in this list already had a case before it was added. minLength
// is not included anywhere: none of the four covered messages' request or
// response structs carry a `min=` bound on a string field (every string
// length bound in this reach set is a maxLength). This is not an oversight
// discovered after the fact — the corpus is fixed before results are read,
// so "not expressible in this corpus" is recorded here as the deliberate
// answer for minLength, not noticed later and patched in.
//
// # A known, deliberate divergence carried in the fixtures: TransactionEvent.offline
//
// TransactionEvent's all-optionals-populated-request.json fixture sets
// "offline":false, matching the schema's boolean property. master's
// TransactionEventRequest.Offline is a non-pointer `bool` with `omitempty`
// (transaction_event.go), so master's own json.Marshal of the decoded
// struct silently drops the field again when false is its zero value — the
// fixture's "offline":false round-trips to no "offline" key at all on
// master's side, which is exactly what this command records as the golden
// payload. The generated side's Offline is a `*bool`: decoding the same
// "offline":false input produces a non-nil pointer to false, which
// generated code's own marshal keeps on the wire. Harness W's comparison
// (ocpp2.0.1_test/wire_parity_test.go) therefore decodes each case's
// *original authored input* — not this command's already-lossy recorded
// output — through the generated types independently, so this divergence
// actually surfaces as a canonicalized-JSON difference at .offline instead
// of disappearing into two independently-lossy recordings that happen to
// agree; the wire-parity exception manifest carries the authorized row for
// it. This command's own recorded golden/ payload is still, correctly,
// what master itself produces — recording anything else would not be
// "exactly as master produced them."
//
// # CI blind spot
//
// Everything under testdata/ is outside every package's build (Go's own
// convention: a directory named "testdata" is never imported by `go build`
// or `go vet`), and this command lives at
// ocpp2.0.1_test/testdata/parity/recorder/, so `go build ./...`,
// `go vet ./...` and `go test ./...` never compile it — a break here is
// invisible to the normal build and test commands. Compiling it is only ever exercised by
// explicitly building this path, e.g.
// `go build -o /dev/null ./ocpp2.0.1_test/testdata/parity/recorder` (the
// -o /dev/null form checks compilation without leaving a stray binary
// behind) — and that only succeeds at all from inside a `master` worktree,
// since this file imports symbols the generated tree's swap removed.
// Wiring an explicit compile-and-run step into CI belongs with the
// surrounding work that actually runs this command against a master
// worktree on a schedule, not this file; until it lands, a change here that
// breaks the build is only caught by running this command by hand.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/go-playground/validator.v9"

	"github.com/enesismail/ocpp-go/ocpp2.0.1/availability"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/provisioning"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/smartcharging"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/transactions"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/types"
)

// wireRecorder decodes a case's input bytes into T (master's hand-written
// struct), re-marshals it, and returns the marshaled bytes — the "struct's
// raw json.Marshal bytes, exactly as master produced them" the golden
// format calls for. It also confirms the result is well-formed JSON, using
// only stdlib encoding/json (this command never imports canonjson:
// canonicalization is reserved for comparison time, and canonjson does not
// exist on the master worktree this command actually runs in).
func wireRecorder[T any](input []byte) ([]byte, error) {
	var v T
	if err := json.Unmarshal(input, &v); err != nil {
		return nil, fmt.Errorf("decode into %T: %w", v, err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal %T: %w", v, err)
	}
	if !json.Valid(out) {
		return nil, fmt.Errorf("marshal %T produced invalid JSON", v)
	}
	return out, nil
}

// violationRecorder decodes a case's input bytes into T and runs master's
// shared validator against the decoded value, reporting whether master
// accepts the payload and, when it does not, the tag that first rejected
// it. A decode failure is itself a rejection (see the "JSON type mismatch"
// section in the package doc) and reports tag "type" rather than treating
// the case as an internal error.
func violationRecorder[T any](input []byte) (accepted bool, tag string, err error) {
	var v T
	if decodeErr := json.Unmarshal(input, &v); decodeErr != nil {
		return false, "type", nil
	}
	verr := types.Validate.Struct(v)
	if verr == nil {
		return true, "", nil
	}
	if validationErrors, ok := verr.(validator.ValidationErrors); ok && len(validationErrors) > 0 {
		return false, validationErrors[0].Tag(), nil
	}
	return false, "", fmt.Errorf("validate %T: %w", v, verr)
}

// messageTarget pairs a message half's wire and violation recorders, both
// closed over the same master struct type.
type messageTarget struct {
	wire      func([]byte) ([]byte, error)
	violation func([]byte) (bool, string, error)
}

// targets maps a case directory name (the message) and a filename suffix
// (the half) to the master struct type that decodes it. Every one of the
// four prototype messages' request and response halves is listed; a case
// file under any other directory, or with neither suffix, is a corpus
// layout error this command refuses to guess about (see run's dispatch).
var targets = map[string]map[string]messageTarget{
	"BootNotification": {
		"request":  {wire: wireRecorder[provisioning.BootNotificationRequest], violation: violationRecorder[provisioning.BootNotificationRequest]},
		"response": {wire: wireRecorder[provisioning.BootNotificationResponse], violation: violationRecorder[provisioning.BootNotificationResponse]},
	},
	"Heartbeat": {
		"request":  {wire: wireRecorder[availability.HeartbeatRequest], violation: violationRecorder[availability.HeartbeatRequest]},
		"response": {wire: wireRecorder[availability.HeartbeatResponse], violation: violationRecorder[availability.HeartbeatResponse]},
	},
	"SetChargingProfile": {
		"request":  {wire: wireRecorder[smartcharging.SetChargingProfileRequest], violation: violationRecorder[smartcharging.SetChargingProfileRequest]},
		"response": {wire: wireRecorder[smartcharging.SetChargingProfileResponse], violation: violationRecorder[smartcharging.SetChargingProfileResponse]},
	},
	"TransactionEvent": {
		"request":  {wire: wireRecorder[transactions.TransactionEventRequest], violation: violationRecorder[transactions.TransactionEventRequest]},
		"response": {wire: wireRecorder[transactions.TransactionEventResponse], violation: violationRecorder[transactions.TransactionEventResponse]},
	},
}

// wireGolden and violationGolden are the two golden/ file bodies this
// command writes. Field declaration order is the JSON key order
// (encoding/json emits struct fields in declaration order): master_sha
// first, then payload, or master_sha, input, accepted, tag.
type wireGolden struct {
	MasterSHA string          `json:"master_sha"`
	Payload   json.RawMessage `json:"payload"`
}

type violationGolden struct {
	MasterSHA string          `json:"master_sha"`
	Input     json.RawMessage `json:"input"`
	Accepted  bool            `json:"accepted"`
	Tag       string          `json:"tag"`
}

func main() {
	caseDir := flag.String("case-dir", "", "directory of authored case definitions to read (e.g. ocpp2.0.1_test/testdata/parity/cases)")
	out := flag.String("out", "", "directory the recorded GOLDEN corpus is written to (e.g. ocpp2.0.1_test/testdata/parity/golden)")
	flag.Parse()

	if err := run(*caseDir, *out); err != nil {
		fmt.Fprintln(os.Stderr, "recorder:", err)
		os.Exit(1)
	}
}

func run(caseDir, out string) error {
	if caseDir == "" {
		return fmt.Errorf("-case-dir is required")
	}
	if out == "" {
		return fmt.Errorf("-out is required")
	}

	sha, err := masterSHA()
	if err != nil {
		return fmt.Errorf("determine master_sha: %w", err)
	}

	messageDirs, err := os.ReadDir(caseDir)
	if err != nil {
		return fmt.Errorf("read case directory %s: %w", caseDir, err)
	}

	var recorded int
	var failures []string

	for _, messageDir := range messageDirs {
		if !messageDir.IsDir() {
			failures = append(failures, fmt.Sprintf("%s: unexpected non-directory entry at the top level of -case-dir", messageDir.Name()))
			continue
		}
		message := messageDir.Name()
		halves, ok := targets[message]
		if !ok {
			failures = append(failures, fmt.Sprintf("%s: no recorder registered for this message directory", message))
			continue
		}

		caseFiles, err := os.ReadDir(filepath.Join(caseDir, message))
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: read directory: %v", message, err))
			continue
		}

		for _, caseFile := range caseFiles {
			if caseFile.IsDir() || !strings.HasSuffix(caseFile.Name(), ".json") {
				failures = append(failures, fmt.Sprintf("%s/%s: not a .json file", message, caseFile.Name()))
				continue
			}

			half, ok := halfOf(caseFile.Name())
			if !ok {
				failures = append(failures, fmt.Sprintf("%s/%s: filename does not end with -request.json or -response.json", message, caseFile.Name()))
				continue
			}
			target, ok := halves[half]
			if !ok {
				failures = append(failures, fmt.Sprintf("%s/%s: no recorder registered for half %q", message, caseFile.Name(), half))
				continue
			}

			relPath := filepath.Join(message, caseFile.Name())
			inPath := filepath.Join(caseDir, relPath)
			input, err := os.ReadFile(inPath)
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s: read: %v", relPath, err))
				continue
			}
			if !json.Valid(input) {
				failures = append(failures, fmt.Sprintf("%s: authored case input is not well-formed JSON", relPath))
				continue
			}

			isViolation := strings.HasPrefix(caseFile.Name(), "violation-")
			var body []byte
			if isViolation {
				accepted, tag, err := target.violation(input)
				if err != nil {
					failures = append(failures, fmt.Sprintf("%s: %v", relPath, err))
					continue
				}
				body, err = json.Marshal(violationGolden{MasterSHA: sha, Input: json.RawMessage(input), Accepted: accepted, Tag: tag})
				if err != nil {
					failures = append(failures, fmt.Sprintf("%s: render golden: %v", relPath, err))
					continue
				}
			} else {
				payload, err := target.wire(input)
				if err != nil {
					failures = append(failures, fmt.Sprintf("%s: %v", relPath, err))
					continue
				}
				body, err = json.Marshal(wireGolden{MasterSHA: sha, Payload: json.RawMessage(payload)})
				if err != nil {
					failures = append(failures, fmt.Sprintf("%s: render golden: %v", relPath, err))
					continue
				}
			}

			outPath := filepath.Join(out, relPath)
			if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
				failures = append(failures, fmt.Sprintf("%s: create output directory: %v", relPath, err))
				continue
			}
			if err := os.WriteFile(outPath, append(body, '\n'), 0o644); err != nil {
				failures = append(failures, fmt.Sprintf("%s: write output: %v", relPath, err))
				continue
			}
			recorded++
		}
	}

	sort.Strings(failures)
	if len(failures) > 0 {
		for _, f := range failures {
			fmt.Fprintln(os.Stderr, "recorder:", f)
		}
		return fmt.Errorf("%d case(s) failed to record (%d recorded successfully)", len(failures), recorded)
	}

	fmt.Fprintf(os.Stdout, "recorder: recorded %d case(s) from %s into %s (master_sha=%s)\n", recorded, caseDir, out, sha)
	return nil
}

// halfOf reports the message half a case filename names, by its required
// suffix, and the suffix itself is stripped from nothing here — callers
// that need the case name on its own strip it separately. Any filename not
// ending in one of the two recognized suffixes reports ok=false.
func halfOf(filename string) (half string, ok bool) {
	switch {
	case strings.HasSuffix(filename, "-request.json"):
		return "request", true
	case strings.HasSuffix(filename, "-response.json"):
		return "response", true
	default:
		return "", false
	}
}

// masterSHA reports the commit checked out in the current working
// directory, via `git rev-parse HEAD`. Run from inside a master worktree,
// this is the master commit the golden corpus is recorded from; run
// anywhere else, it is whatever that directory has checked out, faithfully
// recorded rather than assumed.
func masterSHA() (string, error) {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}
