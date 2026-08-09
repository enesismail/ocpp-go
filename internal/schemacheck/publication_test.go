package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// assertNothingWritten fails unless dir holds no entries at all — the point
// of refusing to publish is that no artifact of a failed run survives, not
// merely that the two report files are absent.
func assertNothingWritten(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("read output directory %s: %v", dir, err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("a run that refused to publish still left %v in %s", names, dir)
	}
}

// TestRunRefusesToPublishWhenSelfChecksFail covers the self-check half of
// the publication check. The fixture's tree root is named for the corpus the
// recorded M1-M7 numbers were measured over, so a run over it is held to
// those numbers; its two-file schema corpus cannot possibly reproduce them.
// The run must therefore fail loudly and leave nothing behind, rather than
// publish a report with the failures demoted to an annotation.
func TestRunRefusesToPublishWhenSelfChecksFail(t *testing.T) {
	out := filepath.Join(t.TempDir(), "reports")
	var usage bytes.Buffer
	err := run([]string{
		"-tree", "testdata/rootvalidator/ocpp2.0.1",
		"-schemas", "testdata/rootvalidator/schemas",
		"-out", out,
	}, &usage)
	if err == nil {
		t.Fatal("a run whose self-checks failed exited successfully; a report that cannot reproduce its own corpus's measured numbers must not be published")
	}
	if !strings.Contains(err.Error(), "self-check") {
		t.Fatalf("error %q does not say a self-check failed", err.Error())
	}
	if !strings.Contains(err.Error(), "M1") {
		t.Fatalf("error %q does not name which check failed, so the failure cannot be adjudicated from it", err.Error())
	}
	assertNothingWritten(t, out)
}

// TestRunRefusesToPublishWhenCoverageIsIncomplete covers the coverage half,
// on a corpus whose version label is not the one the recorded self-check
// numbers describe — so incomplete coverage is the only thing that can hold
// it back, and a passing test cannot be the self-check half firing by
// accident.
//
// The probe is the real failure that motivated this: a schema set missing
// one message's request half, alongside another message that pairs
// completely. Every roll-up is then computed over a subset, and nothing in
// the published document would say so.
func TestRunRefusesToPublishWhenCoverageIsIncomplete(t *testing.T) {
	out := filepath.Join(t.TempDir(), "reports")
	var usage bytes.Buffer
	err := run([]string{
		"-tree", "testdata/coveragegap/tree",
		"-schemas", "testdata/coveragegap/schemas",
		"-out", out,
	}, &usage)
	if err == nil {
		t.Fatal("a run that compared only part of its inputs exited successfully; an incomplete comparison must not be published")
	}
	if !strings.Contains(err.Error(), "Beta") || !strings.Contains(err.Error(), "no schema pair") {
		t.Fatalf("error %q does not name the message left uncompared", err.Error())
	}
	if !strings.Contains(err.Error(), "BetaResponse.json") {
		t.Fatalf("error %q does not name the schema file no message consumed", err.Error())
	}
	if strings.Contains(err.Error(), "self-check") {
		t.Fatalf("error %q blames a self-check, but this corpus is not the one the recorded numbers describe — incomplete coverage must be what held it back", err.Error())
	}
	assertNothingWritten(t, out)
}

// TestPublicationFailuresDecisionTable pins the decision itself, including
// the two cases the end-to-end tests above cannot both show at once: that a
// complete, self-consistent run is publishable, and that self-check failures
// hold back only the corpus whose measured numbers they are.
func TestPublicationFailuresDecisionTable(t *testing.T) {
	passing := []SelfCheck{{ID: "M1", Claim: "c", Expected: 1, Actual: 1, Status: "pass"}}
	failing := []SelfCheck{{ID: "M1", Claim: "c", Expected: 408, Actual: 3, Status: "fail"}}

	cases := []struct {
		name         string
		report       Report
		wantWithheld bool
		wantMention  string
	}{
		{
			name:   "complete run over the measured corpus publishes",
			report: Report{Version: selfCheckCorpus, Coverage: Coverage{}, SelfCheck: passing},
		},
		{
			name:         "a failed self-check withholds the corpus it was measured over",
			report:       Report{Version: selfCheckCorpus, SelfCheck: failing},
			wantWithheld: true,
			wantMention:  "M1",
		},
		{
			name:   "a failed self-check does not withhold a corpus it does not describe",
			report: Report{Version: "vother", SelfCheck: failing},
		},
		{
			name:         "an unpaired message withholds any corpus",
			report:       Report{Version: "vother", Coverage: Coverage{UnpairedMessages: []string{"Heartbeat"}}, SelfCheck: passing},
			wantWithheld: true,
			wantMention:  "Heartbeat",
		},
		{
			name:         "an unconsumed schema file withholds any corpus",
			report:       Report{Version: "vother", Coverage: Coverage{UnpairedSchemaFiles: []string{"HeartbeatRequest.json"}}, SelfCheck: passing},
			wantWithheld: true,
			wantMention:  "HeartbeatRequest.json",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			report := c.report
			failures := publicationFailures(&report)
			if c.wantWithheld && len(failures) == 0 {
				t.Fatal("expected the report to be withheld, but no reason was reported")
			}
			if !c.wantWithheld && len(failures) != 0 {
				t.Fatalf("expected the report to be publishable, but it was withheld for %v", failures)
			}
			if c.wantMention != "" && !strings.Contains(strings.Join(failures, "\n"), c.wantMention) {
				t.Fatalf("the reasons %v do not name %q", failures, c.wantMention)
			}
		})
	}
}
