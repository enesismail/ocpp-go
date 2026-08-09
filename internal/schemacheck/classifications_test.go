package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// classifiableReport is a two-row report standing in for a real run: one row
// of a class a registry covers with a default, one of a class it insists be
// judged individually, plus a message-root row, which is addressed by an empty
// path and is the shape a naive keying would silently collapse.
// The three rule texts the fixture rows carry, spelled the way the comparator
// spells them: the fingerprint a judgement is pinned to.
const (
	floorRule      = "hand-written constraint gte=0 is stricter than schema minimum=no bound and looks deliberate"
	extraFieldRule = "Go field not in schema: the hand-written struct declares a field the schema does not authorize"
	structRule     = "cross-field struct validation rule isValidWidget has no schema counterpart and needs human adjudication"
)

// widgetValidatorBody is the normalized source a validator-body item pins,
// spelled the way the extractor produces it: gofmt output, no doc comment, no
// blank lines.
const widgetValidatorBody = `func isValidTrigger(v string) bool {
	switch v {
	case pkg.Alpha, pkg.Beta, pkg.Gamma:
		return true
	default:
		return false
	}
}`

// fixtureEvidence stands in for the tree facts a real run reads, carrying the
// one function and the one registration the evidence tests pin.
func fixtureEvidence() *treeEvidence {
	return &treeEvidence{
		functionBodies: map[string]string{
			"widget/widget.go:isValidTrigger": widgetValidatorBody,
		},
		bindings: map[string][]string{
			"widgetKind": {"isValidTrigger at widget/widget.go"},
		},
		bindingSites: map[string][]string{
			"widgetKind": {"isValidTrigger at widget/widget.go:12"},
		},
	}
}

func classifiableReport() *Report {
	return &Report{
		Version: "vfixture",
		Messages: []Message{
			{
				FeatureName: "Widget",
				Request: MessageSide{
					GoType: "WidgetRequest",
					Fields: []Field{
						{Path: "level", Class: OVERRIDE_CANDIDATE, Rule: floorRule},
						{Path: "secret", Class: FORK_BUG, Rule: extraFieldRule},
					},
				},
				Response: MessageSide{
					GoType: "WidgetResponse",
					Fields: []Field{
						{Path: "", Class: STRUCT_VALIDATOR, Rule: structRule},
					},
				},
			},
		},
	}
}

func fixtureRegistry() *classificationRegistry {
	return &classificationRegistry{
		Version: "vfixture",
		Classes: map[string]classDefault{
			OVERRIDE_CANDIDATE: {
				Classification: UPSTREAM_INHERITED,
				Severity:       CORRECTNESS,
				Note:           "a hand-written bound the schema does not impose",
				Covers:         []coveredRow{{Row: "Widget.request.level", Rule: floorRule}},
			},
		},
		RequireExplicit: []string{FORK_BUG},
		Fields: []fieldClassification{
			{Message: "Widget", Side: "request", Path: "secret", Class: FORK_BUG, Rule: extraFieldRule, Classification: SPEC_FIDELITY, Severity: BLOCKING_INTEROP, Note: "a property the schema does not authorize"},
			{Message: "Widget", Side: "response", Path: "", Class: STRUCT_VALIDATOR, Rule: structRule, Classification: UPSTREAM_INHERITED, Severity: COSMETIC, Note: "a cross-field rule the schema cannot state"},
		},
	}
}

func judgedRow(t *testing.T, report *Report, side, path string) Field {
	t.Helper()
	fields := report.Messages[0].Request.Fields
	if side == "response" {
		fields = report.Messages[0].Response.Fields
	}
	for _, f := range fields {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("no %s row at path %q", side, path)
	return Field{}
}

// TestClassificationsAnnotateEveryRowTheRegistryAccountsFor covers the merge
// itself: a per-row judgement lands on its own row, a class-wide default lands
// on every row of that class, and the message-root row — whose path is empty —
// is addressed like any other rather than swallowed.
func TestClassificationsAnnotateEveryRowTheRegistryAccountsFor(t *testing.T) {
	report := classifiableReport()
	if err := applyClassifications(report, fixtureRegistry(), fixtureEvidence()); err != nil {
		t.Fatalf("applying a registry that describes the run: %v", err)
	}

	explicit := judgedRow(t, report, "request", "secret")
	if explicit.Classification != SPEC_FIDELITY || explicit.Severity != BLOCKING_INTEROP {
		t.Fatalf("per-row judgement was not applied: %+v", explicit)
	}
	if explicit.ClassificationNote == "" {
		t.Fatal("per-row judgement lost its note, which is the only place its reasoning is written down")
	}

	defaulted := judgedRow(t, report, "request", "level")
	if defaulted.Classification != UPSTREAM_INHERITED || defaulted.Severity != CORRECTNESS {
		t.Fatalf("class-wide default was not applied: %+v", defaulted)
	}

	root := judgedRow(t, report, "response", "")
	if root.Classification != UPSTREAM_INHERITED || root.Severity != COSMETIC {
		t.Fatalf("the message-root row was not annotated: %+v", root)
	}
}

// TestClassificationsRejectEntryWithNoMatchingRow is the drift guard in the
// direction that matters most: the tree changed, a row the registry judged no
// longer exists, and the judgement now describes nothing. Ignoring it would
// let a registry rot silently while the report kept looking complete.
func TestClassificationsRejectEntryWithNoMatchingRow(t *testing.T) {
	registry := fixtureRegistry()
	registry.Fields = append(registry.Fields, fieldClassification{
		Message: "Widget", Side: "request", Path: "gone", Class: FORK_BUG, Rule: extraFieldRule,
		Classification: UPSTREAM_INHERITED, Severity: COSMETIC,
	})

	err := applyClassifications(classifiableReport(), registry, fixtureEvidence())
	if err == nil {
		t.Fatal("a judgement addressing a row this run does not have was accepted")
	}
	if !strings.Contains(err.Error(), "gone") {
		t.Fatalf("error %q does not name the entry that matches nothing", err)
	}
}

// TestClassificationsRejectDefaultCoveringARowItDoesNotList is the same guard
// in the other direction: a new row appeared in a class that carries a
// default, and would silently inherit a judgement written before it existed.
func TestClassificationsRejectDefaultCoveringARowItDoesNotList(t *testing.T) {
	report := classifiableReport()
	report.Messages[0].Request.Fields = append(report.Messages[0].Request.Fields,
		Field{Path: "newlyBounded", Class: OVERRIDE_CANDIDATE, Rule: floorRule})

	err := applyClassifications(report, fixtureRegistry(), fixtureEvidence())
	if err == nil {
		t.Fatal("a class default silently covered a row it was never reviewed against")
	}
	if !strings.Contains(err.Error(), "Widget.request.newlyBounded") {
		t.Fatalf("error %q does not name the row that appeared", err)
	}
}

// TestClassificationsRejectACountPreservingSwap is the reason a class default
// records which rows it covers rather than how many. A tree edit that removes
// one row of a defaulted class and adds another of the same class leaves every
// count in the registry correct, and a count-based guard sees nothing — while
// the new row silently inherits a judgement written about the row that left.
// Naming the rows turns one silent swap into two named failures.
func TestClassificationsRejectACountPreservingSwap(t *testing.T) {
	report := classifiableReport()
	// Same class, same number of rows, different row: "level" left and
	// "ceiling" arrived.
	report.Messages[0].Request.Fields[0] = Field{Path: "ceiling", Class: OVERRIDE_CANDIDATE, Rule: floorRule}

	err := applyClassifications(report, fixtureRegistry(), fixtureEvidence())
	if err == nil {
		t.Fatal("a swap that preserved the row count was accepted, so the new row took a judgement made about the row it replaced")
	}
	if !strings.Contains(err.Error(), "Widget.request.ceiling") {
		t.Fatalf("error %q does not name the row that arrived", err)
	}
	if !strings.Contains(err.Error(), "Widget.request.level") {
		t.Fatalf("error %q does not name the row that left", err)
	}
}

// TestClassificationsRejectARowWhoseClassMoved covers the third way coverage
// can go stale without any row appearing or disappearing: the same field is
// still there, but the comparison now says something different about it, so
// the default it is listed under was written about a different finding.
func TestClassificationsRejectARowWhoseClassMoved(t *testing.T) {
	report := classifiableReport()
	report.Messages[0].Request.Fields[0].Class = SCHEMA_FAITHFUL_CHANGE

	err := applyClassifications(report, fixtureRegistry(), fixtureEvidence())
	if err == nil {
		t.Fatal("a row that changed comparison class kept the judgement written for its old one")
	}
	if !strings.Contains(err.Error(), "Widget.request.level") || !strings.Contains(err.Error(), SCHEMA_FAITHFUL_CHANGE) {
		t.Fatalf("error %q does not say which row moved, or to what", err)
	}
}

// TestClassificationsAcceptAnUnchangedRun is the negative control the three
// drift guards need: a registry that still describes its run must merge
// silently. Without it, a guard that rejected everything would pass every
// test above.
func TestClassificationsAcceptAnUnchangedRun(t *testing.T) {
	if err := applyClassifications(classifiableReport(), fixtureRegistry(), fixtureEvidence()); err != nil {
		t.Fatalf("a registry that still describes its run was rejected: %v", err)
	}
}

// TestClassificationsRejectMalformedCoverage covers the coverage list's own
// well-formedness, all of it about keeping the file mechanically regenerable
// and unambiguous: a key nothing can address, a row claimed twice, a row
// claimed by two different defaults, a row that both carries a per-row
// judgement and is claimed by a default (the per-row one wins, so the claim
// would be a line nothing checks), and a list out of sorted order.
func TestClassificationsRejectMalformedCoverage(t *testing.T) {
	cases := []struct {
		name     string
		covers   []string
		wantTerm string
	}{
		{"key naming no side", []string{"Widget"}, "not a row key"},
		{"key naming neither side", []string{"Widget.sideways.level"}, "sideways"},
		{"the same row listed twice", []string{"Widget.request.level", "Widget.request.level"}, "listed twice"},
		{"a row that also carries a per-row judgement", []string{"Widget.request.level", "Widget.request.secret"}, "per-row judgement"},
		{"out of sorted order", []string{"Widget.request.level", "Widget.request.alpha"}, "sorted order"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			registry := fixtureRegistry()
			fallback := registry.Classes[OVERRIDE_CANDIDATE]
			fallback.Covers = nil
			for _, row := range test.covers {
				fallback.Covers = append(fallback.Covers, coveredRow{Row: row, Rule: floorRule})
			}
			registry.Classes[OVERRIDE_CANDIDATE] = fallback

			err := applyClassifications(classifiableReport(), registry, fixtureEvidence())
			if err == nil {
				t.Fatal("the coverage list was accepted")
			}
			if !strings.Contains(err.Error(), test.wantTerm) {
				t.Fatalf("error %q does not name %q", err, test.wantTerm)
			}
		})
	}
}

// TestClassificationsRejectARowClaimedByTwoDefaults needs two defaults to
// exist at once, so it stands apart from the single-list cases above.
func TestClassificationsRejectARowClaimedByTwoDefaults(t *testing.T) {
	registry := fixtureRegistry()
	registry.Classes[SCHEMA_FAITHFUL_CHANGE] = classDefault{
		Classification: UPSTREAM_INHERITED,
		Severity:       COSMETIC,
		Covers:         []coveredRow{{Row: "Widget.request.level", Rule: floorRule}},
	}

	err := applyClassifications(classifiableReport(), registry, fixtureEvidence())
	if err == nil {
		t.Fatal("a row claimed by two class defaults was accepted")
	}
	if !strings.Contains(err.Error(), "Widget.request.level") || !strings.Contains(err.Error(), "undecided") {
		t.Fatalf("error %q does not report the contested row", err)
	}
}

// TestClassificationsRejectUnjudgedRowOfARequireExplicitClass covers the
// classes whose rows are the findings themselves. Letting one through
// unannotated would drop a defect out of every count the report reports.
func TestClassificationsRejectUnjudgedRowOfARequireExplicitClass(t *testing.T) {
	registry := fixtureRegistry()
	registry.Fields = registry.Fields[1:] // drop the FORK_BUG judgement

	err := applyClassifications(classifiableReport(), registry, fixtureEvidence())
	if err == nil {
		t.Fatal("a FORK_BUG row with no judgement was accepted even though the registry requires one")
	}
	if !strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), FORK_BUG) {
		t.Fatalf("error %q does not name the row left unjudged", err)
	}
}

// TestClassificationsRejectARegistryForAnotherCorpus stops the failure mode a
// version label exists to prevent: message and field names repeat across
// protocol versions, so a registry pointed at the wrong run would match some
// rows, miss others, and produce a report that looks annotated.
func TestClassificationsRejectARegistryForAnotherCorpus(t *testing.T) {
	registry := fixtureRegistry()
	registry.Version = "v999"

	err := applyClassifications(classifiableReport(), registry, fixtureEvidence())
	if err == nil {
		t.Fatal("a registry written for another corpus was merged onto this one")
	}
	if !strings.Contains(err.Error(), "v999") || !strings.Contains(err.Error(), "vfixture") {
		t.Fatalf("error %q does not name both corpora", err)
	}
}

// TestClassificationsRejectVocabularyNothingDefines keeps the report's own
// words closed. A severity the method section does not define cannot be
// counted, and a class-name typo would create a default that covers nothing
// while reading as though it covered a whole group.
func TestClassificationsRejectVocabularyNothingDefines(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*classificationRegistry)
		wantTerm string
	}{
		{
			name:     "unknown severity",
			mutate:   func(r *classificationRegistry) { r.Fields[0].Severity = "SEVERE" },
			wantTerm: "SEVERE",
		},
		{
			name:     "unknown classification",
			mutate:   func(r *classificationRegistry) { r.Fields[0].Classification = "MAYBE" },
			wantTerm: "MAYBE",
		},
		{
			name: "class default naming no comparison class",
			mutate: func(r *classificationRegistry) {
				r.Classes["OVERRIDE_CANDIDATES"] = classDefault{
					Classification: UPSTREAM_INHERITED, Severity: COSMETIC,
					Covers: []coveredRow{{Row: "Widget.request.level", Rule: floorRule}},
				}
			},
			wantTerm: "OVERRIDE_CANDIDATES",
		},
		{
			name:     "side that addresses neither half of a message",
			mutate:   func(r *classificationRegistry) { r.Fields[0].Side = "req" },
			wantTerm: "req",
		},
		{
			name: "class default with no coverage list at all",
			mutate: func(r *classificationRegistry) {
				fallback := r.Classes[OVERRIDE_CANDIDATE]
				fallback.Covers = nil
				r.Classes[OVERRIDE_CANDIDATE] = fallback
			},
			wantTerm: "lists no covers",
		},
		{
			name: "class both defaulted and required to be explicit",
			mutate: func(r *classificationRegistry) {
				r.RequireExplicit = append(r.RequireExplicit, OVERRIDE_CANDIDATE)
			},
			wantTerm: OVERRIDE_CANDIDATE,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			registry := fixtureRegistry()
			test.mutate(registry)
			err := applyClassifications(classifiableReport(), registry, fixtureEvidence())
			if err == nil {
				t.Fatal("the registry was accepted")
			}
			if !strings.Contains(err.Error(), test.wantTerm) {
				t.Fatalf("error %q does not name %q", err, test.wantTerm)
			}
		})
	}
}

// TestFailedClassificationLeavesTheReportUnannotated covers the all-or-nothing
// property. A partly-annotated report is indistinguishable from a fully
// annotated one whose remaining rows were judged to be nothing, so a rejected
// registry must leave no trace of itself behind.
func TestFailedClassificationLeavesTheReportUnannotated(t *testing.T) {
	registry := fixtureRegistry()
	registry.Fields = append(registry.Fields, fieldClassification{
		Message: "Widget", Side: "request", Path: "gone", Class: FORK_BUG, Rule: extraFieldRule,
		Classification: UPSTREAM_INHERITED, Severity: COSMETIC,
	})

	report := classifiableReport()
	if err := applyClassifications(report, registry, fixtureEvidence()); err == nil {
		t.Fatal("the stale registry was accepted")
	}
	for _, side := range []string{"request", "response"} {
		fields := report.Messages[0].Request.Fields
		if side == "response" {
			fields = report.Messages[0].Response.Fields
		}
		for _, f := range fields {
			if f.Classification != "" || f.Severity != "" || f.ClassificationNote != "" {
				t.Fatalf("%s row %q kept an annotation from a registry that was rejected: %+v", side, f.Path, f)
			}
		}
	}
}

// TestClassificationRegistryRejectsAMisspelledKey covers the decoding step. A
// hand-written file's most likely defect is a typo, and a decoder that skipped
// unknown keys would drop the judgement while the file still read as complete
// to its author.
func TestClassificationRegistryRejectsAMisspelledKey(t *testing.T) {
	_, err := loadClassificationRegistry(fixturePath(t, "classifications", "vtree_misspelled_key.yaml"))
	if err == nil {
		t.Fatal("a registry with a misspelled key loaded cleanly")
	}
	if !strings.Contains(err.Error(), "severty") {
		t.Fatalf("error %q does not name the key it could not place", err)
	}
}

// TestRunMergesClassificationsIntoTheReport covers the whole path the audit is
// refreshed through: the tool reads the tree, the schemas and the registry,
// and writes one document carrying both the computed comparison and the
// judgements — with no editing step in between, which is what makes the
// document reproducible.
func TestRunMergesClassificationsIntoTheReport(t *testing.T) {
	out := t.TempDir()
	var usage bytes.Buffer
	err := run([]string{
		"-tree", "testdata/orchestration/tree",
		"-schemas", "testdata/orchestration/schemas",
		"-out", out,
		"-format", "json",
		"-classifications", "testdata/classifications/vtree.yaml",
	}, &usage)
	if err != nil {
		t.Fatalf("run: %v (usage output: %s)", err, usage.String())
	}

	data, err := os.ReadFile(filepath.Join(out, "divergence-census-vtree.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report Report
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}

	judged := 0
	for _, m := range report.Messages {
		for _, fields := range [][]Field{m.Request.Fields, m.Response.Fields} {
			for _, f := range fields {
				if f.Class == FORK_BUG && f.Severity == "" {
					t.Fatalf("%s row %q is a FORK_BUG the registry judged, but the written report carries no severity", m.FeatureName, f.Path)
				}
				if f.Classification != "" {
					judged++
				}
			}
		}
	}
	total := 0
	for _, m := range report.Messages {
		total += len(m.Request.Fields) + len(m.Response.Fields)
	}
	if judged != total {
		t.Fatalf("the written report carries %d judged row(s) out of %d; every row must be judged or the run should have failed", judged, total)
	}
}

// TestRunWithoutClassificationsWritesNoJudgementKeys is the other half of the
// contract, and the reason a corpus with no registry is unaffected: the three
// keys are absent from the document entirely rather than present and empty,
// so a report nobody judged cannot be read as one judged to be clean.
func TestRunWithoutClassificationsWritesNoJudgementKeys(t *testing.T) {
	out := t.TempDir()
	var usage bytes.Buffer
	if err := run([]string{
		"-tree", "testdata/orchestration/tree",
		"-schemas", "testdata/orchestration/schemas",
		"-out", out,
		"-format", "json",
	}, &usage); err != nil {
		t.Fatalf("run: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(out, "divergence-census-vtree.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"classification"`, `"severity"`, `"classificationNote"`} {
		if bytes.Contains(data, []byte(key)) {
			t.Fatalf("a run naming no registry wrote %s into the report", key)
		}
	}
}

// TestRunWithClassificationsIsByteIdenticalAcrossInvocations extends the
// determinism guarantee to cover the merge. The audit document is refreshed by
// re-running this command, and a refresh whose bytes moved for no reason
// cannot be checked against the document it replaces.
func TestRunWithClassificationsIsByteIdenticalAcrossInvocations(t *testing.T) {
	outA, outB := t.TempDir(), t.TempDir()
	var usage bytes.Buffer
	for _, out := range []string{outA, outB} {
		if err := run([]string{
			"-tree", "testdata/orchestration/tree",
			"-schemas", "testdata/orchestration/schemas",
			"-out", out,
			"-format", "json",
			"-classifications", "testdata/classifications/vtree.yaml",
		}, &usage); err != nil {
			t.Fatalf("run into %s: %v", out, err)
		}
	}
	dataA, err := os.ReadFile(filepath.Join(outA, "divergence-census-vtree.json"))
	if err != nil {
		t.Fatal(err)
	}
	dataB, err := os.ReadFile(filepath.Join(outB, "divergence-census-vtree.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dataA, dataB) {
		t.Fatal("two runs over identical inputs, including the same registry, produced different bytes")
	}
}

// TestRunFailsOnARegistryThatDoesNotDescribeTheRun covers the CLI's own
// handling of the drift guard: the failure has to stop the run, because a
// report written anyway would carry the stale judgements into the document
// that replaces the reviewed one.
func TestRunFailsOnARegistryThatDoesNotDescribeTheRun(t *testing.T) {
	out := t.TempDir()
	var usage bytes.Buffer
	err := run([]string{
		"-tree", "testdata/orchestration/tree",
		"-schemas", "testdata/orchestration/schemas",
		"-out", out,
		"-format", "json",
		"-classifications", "testdata/classifications/vtree_misspelled_key.yaml",
	}, &usage)
	if err == nil {
		t.Fatal("a registry the tool could not decode did not stop the run")
	}
	assertFileAbsent(t, filepath.Join(out, "divergence-census-vtree.json"))
}

// TestClassificationsRejectAnExplicitJudgementWhoseClassMoved is the per-row
// half of the guard the coverage lists give class defaults. A judgement is
// written about a specific difference; if the comparison later says something
// else about the same field — the sharpest case being a row that stops being
// identical and starts contradicting the schema — the old wording would still
// be printed beside the new class, and the report would carry a severity
// nobody assigned to what the row now says.
func TestClassificationsRejectAnExplicitJudgementWhoseClassMoved(t *testing.T) {
	report := classifiableReport()
	// The row the registry judged as a FORK_BUG now compares clean.
	report.Messages[0].Request.Fields[1].Class = IDENTICAL

	err := applyClassifications(report, fixtureRegistry(), fixtureEvidence())
	if err == nil {
		t.Fatal("a per-row judgement was applied to a row whose comparison class had changed under it")
	}
	if !strings.Contains(err.Error(), "Widget.request.secret") {
		t.Fatalf("error %q does not name the row", err)
	}
	if !strings.Contains(err.Error(), FORK_BUG) || !strings.Contains(err.Error(), IDENTICAL) {
		t.Fatalf("error %q does not name both the class the judgement was written against and the one the run produced", err)
	}
}

// TestClassificationsRejectAnUnjudgedRowOfAnyClass covers the completeness
// rule. Restricting row-by-row judgement to one or two classes leaves the
// others able to gain rows in silence — and a row nothing accounts for never
// reaches the severity counts the report is read for, so a new defect can
// arrive without moving a single number.
func TestClassificationsRejectAnUnjudgedRowOfAnyClass(t *testing.T) {
	for _, class := range []string{ADDITIVE, UNEXPLAINED, STRUCT_VALIDATOR, IDENTICAL, SCHEMA_FAITHFUL_CHANGE} {
		t.Run(class, func(t *testing.T) {
			report := classifiableReport()
			report.Messages[0].Response.Fields = append(report.Messages[0].Response.Fields,
				Field{Path: "arrived", Class: class})

			err := applyClassifications(report, fixtureRegistry(), fixtureEvidence())
			if err == nil {
				t.Fatalf("a %s row nothing in the registry accounts for was left unjudged and the run was allowed to publish", class)
			}
			if !strings.Contains(err.Error(), "Widget.response.arrived") {
				t.Fatalf("error %q does not name the unjudged row", err)
			}
		})
	}
}

// TestClassificationsRejectAMismatchedNonDivergencePairing keeps the two axes
// consistent. A row called a divergence but rated NONE would be counted as a
// finding by one axis and as nothing by the other, and the report's totals
// would stop reconciling.
func TestClassificationsRejectAMismatchedNonDivergencePairing(t *testing.T) {
	cases := []struct {
		name           string
		classification string
		severity       string
	}{
		{"a divergence rated as no severity", UPSTREAM_INHERITED, NONE},
		{"a non-divergence carrying a severity", NOT_A_DIVERGENCE, COSMETIC},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			registry := fixtureRegistry()
			registry.Fields[0].Classification = test.classification
			registry.Fields[0].Severity = test.severity

			err := applyClassifications(classifiableReport(), registry, fixtureEvidence())
			if err == nil {
				t.Fatal("the mismatched pairing was accepted")
			}
			if !strings.Contains(err.Error(), NOT_A_DIVERGENCE) || !strings.Contains(err.Error(), NONE) {
				t.Fatalf("error %q does not explain which two values travel together", err)
			}
		})
	}
}

// TestClassificationsRejectAJudgementWhoseDefectChanged is the reason a
// judgement pins the comparator's rule and not only its class. A class says
// what kind of answer the comparison reached; it does not say what the answer
// was about. A field whose length bound was too loose and which later becomes
// an untyped field the schema types is a contradiction of the schema both
// times — same class, same address — and the judgement written about the bound,
// naming that bound, would be printed beside a row that no longer mentions it.
func TestClassificationsRejectAJudgementWhoseDefectChanged(t *testing.T) {
	const wrongType = `wrong type: hand-written wire type "interface{}" (schema vocabulary "object") does not match schema type "string"`

	t.Run("per-row judgement", func(t *testing.T) {
		report := classifiableReport()
		report.Messages[0].Request.Fields[1].Rule = wrongType // still FORK_BUG

		err := applyClassifications(report, fixtureRegistry(), fixtureEvidence())
		if err == nil {
			t.Fatal("a judgement written about one defect was applied to a row now reporting another")
		}
		if !strings.Contains(err.Error(), "Widget.request.secret") {
			t.Fatalf("error %q does not name the row", err)
		}
		// The message quotes both rules; the found one carries its own quote
		// characters, so only its quote-free head is matched here.
		if !strings.Contains(err.Error(), extraFieldRule) || !strings.Contains(err.Error(), "wrong type: hand-written wire type") {
			t.Fatalf("error %q does not quote both the defect judged and the defect found", err)
		}
	})

	t.Run("class-default coverage", func(t *testing.T) {
		const syntax = "hand-written constraint url restricts the value's syntax where the schema declares no format, so it is stricter than the schema and looks deliberate"
		report := classifiableReport()
		// The override-candidate class really does hold two different
		// findings, so a row crossing between them keeps its class.
		report.Messages[0].Request.Fields[0].Rule = syntax

		err := applyClassifications(report, fixtureRegistry(), fixtureEvidence())
		if err == nil {
			t.Fatal("a row crossed from one finding to another inside the same class and kept the default written for the first")
		}
		if !strings.Contains(err.Error(), "Widget.request.level") {
			t.Fatalf("error %q does not name the row", err)
		}
		if !strings.Contains(err.Error(), floorRule) || !strings.Contains(err.Error(), syntax) {
			t.Fatalf("error %q does not quote both the defect covered and the defect found", err)
		}
	})
}

// TestReportedInvocationRuns guards the one thing a reproduction command has
// to do. -out is required, so a command printed without it exits with a usage
// message; a reader who pastes it gets nothing and has no way to tell whether
// the report was reproducible at all.
func TestReportedInvocationRuns(t *testing.T) {
	report, _, err := runComparison(compareOptions{
		tree:          "testdata/orchestration/tree",
		schemaDirs:    []string{"testdata/orchestration/schemas"},
		rawSchemaFlag: "testdata/orchestration/schemas",
	})
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(report.Invocation)
	for _, flag := range []string{"-tree", "-schemas", "-out"} {
		if !containsString(fields, flag) {
			t.Fatalf("the reported invocation %q omits %s, so running it as printed fails", report.Invocation, flag)
		}
	}

	// Run it: everything after "go run ./internal/schemacheck" is this
	// command's own argument list, and it has to be accepted.
	args := fields[3:]
	out := t.TempDir()
	for i, arg := range args {
		if arg == "-out" && i+1 < len(args) {
			args[i+1] = out
		}
	}
	var usage bytes.Buffer
	if err := run(args, &usage); err != nil {
		t.Fatalf("the reported invocation does not run: %v (usage output: %s)", err, usage.String())
	}
	assertFileExistsNonEmpty(t, filepath.Join(out, "divergence-census-vtree.json"))
}

// unaccountedReport is a run whose one row the comparator reached no account
// for: class UNEXPLAINED, rule empty. Judging it is a hand decision, and the
// class and rule pins say nothing that could ever stop matching.
func unaccountedReport() *Report {
	return &Report{
		Version: "vfixture",
		Messages: []Message{{
			FeatureName: "Widget",
			Request: MessageSide{Fields: []Field{{
				Path:  "trigger",
				Class: UNEXPLAINED,
				Go:    GoField{Validate: []string{"required", "widgetKind"}},
			}}},
		}},
	}
}

func unaccountedRegistry() *classificationRegistry {
	return &classificationRegistry{
		Version: "vfixture",
		Fields: []fieldClassification{{
			Message: "Widget", Side: "request", Path: "trigger",
			Class: UNEXPLAINED, Rule: "",
			Evidence: []evidenceItem{
				{Kind: evidenceGoValidate, Expected: []string{"required", "widgetKind"}},
				{Kind: evidenceValidatorBinding, Location: "widgetKind",
					Expected: []string{"isValidTrigger at widget/widget.go"}},
				{Kind: evidenceValidatorBody, Location: "widget/widget.go:isValidTrigger",
					ExpectedText: widgetValidatorBody},
			},
			Classification: NOT_A_DIVERGENCE, Severity: NONE,
			Note: "checked by hand: the accepted set matches the schema's enum",
		}},
	}
}

// TestUnaccountedJudgementRequiresEvidence closes the vacuity the class and
// rule pins cannot: on a row the comparator reached no account for, both pins
// are constants, so a judgement carrying only them survives any change at all
// to the facts it was actually made from.
func TestUnaccountedJudgementRequiresEvidence(t *testing.T) {
	registry := unaccountedRegistry()
	registry.Fields[0].Evidence = nil

	err := applyClassifications(unaccountedReport(), registry, fixtureEvidence())
	if err == nil {
		t.Fatal("a judgement on a row with no comparator account, and no evidence of its own, was accepted")
	}
	if !strings.Contains(err.Error(), UNEXPLAINED) || !strings.Contains(err.Error(), "pinned to nothing") {
		t.Fatalf("error %q does not say why the judgement is unverifiable", err)
	}
}

// TestUnaccountedJudgementFailsWhenItsEvidenceMoves is the reviewer's scenario
// in miniature: the validator's decision surface loses a value, the row stays
// UNEXPLAINED with an empty rule, and the judgement that the implementation
// accepts exactly what the schema allows is now false. The failure has to print
// both lists, or the reader cannot tell what to re-adjudicate.
func TestUnaccountedJudgementFailsWhenItsEvidenceMoves(t *testing.T) {
	t.Run("a value leaves the decision surface", func(t *testing.T) {
		tree := fixtureEvidence()
		tree.functionBodies["widget/widget.go:isValidTrigger"] = strings.Replace(widgetValidatorBody, "pkg.Alpha, pkg.Beta, pkg.Gamma", "pkg.Alpha, pkg.Gamma", 1)

		err := applyClassifications(unaccountedReport(), unaccountedRegistry(), tree)
		if err == nil {
			t.Fatal("a value disappeared from the validator's switch and the hand-made judgement about it was kept")
		}
		for _, want := range []string{"pkg.Beta", "recorded", "found", "widget/widget.go:isValidTrigger"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q does not contain %q, so it does not say what moved", err, want)
			}
		}
	})

	// The values a validator names are only half of what it decides. Pinning
	// them alone leaves the accepting branch free to start rejecting, with
	// every recorded value still present and the pin still matching — which is
	// why the whole function is pinned rather than its case clauses.
	t.Run("the matching branch starts rejecting", func(t *testing.T) {
		tree := fixtureEvidence()
		flipped := strings.Replace(widgetValidatorBody, "case pkg.Alpha, pkg.Beta, pkg.Gamma:\n\t\treturn true", "case pkg.Alpha, pkg.Beta, pkg.Gamma:\n\t\treturn false", 1)
		if flipped == widgetValidatorBody {
			t.Fatal("the fixture body was not altered, so this test would pass without testing anything")
		}
		tree.functionBodies["widget/widget.go:isValidTrigger"] = flipped

		err := applyClassifications(unaccountedReport(), unaccountedRegistry(), tree)
		if err == nil {
			t.Fatal("the validator now rejects every value it used to accept, and the judgement that it matches the schema was kept")
		}
		if !strings.Contains(err.Error(), "recorded") || !strings.Contains(err.Error(), "found") {
			t.Fatalf("error %q does not print both versions, so a reader cannot see what to re-adjudicate", err)
		}
		if !strings.Contains(err.Error(), "return false") || !strings.Contains(err.Error(), "return true") {
			t.Fatalf("error %q does not show the branch that changed", err)
		}
	})
}

// TestValidatorBodyNormalizationIgnoresNonBehaviouralChurn is the other half of
// the pin's contract. A fingerprint that fired on reindentation or a comment
// edit would be retired within a release, so the text is normalized: gofmt
// output, doc comment dropped, blank lines dropped.
func TestValidatorBodyNormalizationIgnoresNonBehaviouralChurn(t *testing.T) {
	const behaviour = `package widget

func isValidTrigger(v string) bool {
	switch v {
	case pkg.Alpha:
		return true
	}
	return false
}
`
	const sameBehaviourDifferentText = `package widget

// A doc comment that says nothing about behaviour.
func isValidTrigger(v string) bool {

	switch v {

	// an interior comment
	case pkg.Alpha:
		return true

	}

	return false
}
`
	bodyOf := func(src string) string {
		dir := t.TempDir()
		path := filepath.Join(dir, "widget.go")
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		fset, file, err := parseASTFileWithPositions(path)
		if err != nil {
			t.Fatal(err)
		}
		return functionBodies(fset, file)["isValidTrigger"]
	}

	if a, b := bodyOf(behaviour), bodyOf(sameBehaviourDifferentText); a != b {
		t.Fatalf("two spellings of the same function normalized differently:\n%s\n---\n%s", a, b)
	}
	if got := bodyOf(behaviour); !strings.Contains(got, "case pkg.Alpha:") || !strings.Contains(got, "return true") {
		t.Fatalf("normalization dropped the statements it exists to pin: %s", got)
	}
}

// TestUnaccountedJudgementFailsWhenAPremiseIsFixed covers the other direction,
// which matters just as much: the tag a judgement calls unregistered gets
// registered. The defect is gone — and the judgement recording it, with its
// severity, has to be read again rather than left describing a tree that no
// longer behaves that way.
func TestUnaccountedJudgementFailsWhenAPremiseIsFixed(t *testing.T) {
	registry := unaccountedRegistry()
	registry.Fields[0].Evidence = []evidenceItem{
		{Kind: evidenceGoValidate, Expected: []string{"required", "widgetKind"}},
		{Kind: evidenceValidatorBinding, Location: "widgetKind", Expected: nil},
	}

	err := applyClassifications(unaccountedReport(), registry, fixtureEvidence())
	if err == nil {
		t.Fatal("the tag the judgement calls unregistered is registered, and the judgement was kept")
	}
	if !strings.Contains(err.Error(), "widget/widget.go:12") {
		t.Fatalf("error %q does not name where the registration now is", err)
	}
}

// TestEvidenceOnAnAccountedRowStaysGreen is the negative control. The
// requirement is about rows the comparator could not account for; a run whose
// evidence still holds, and rows that need none, must merge silently.
func TestEvidenceOnAnAccountedRowStaysGreen(t *testing.T) {
	if err := applyClassifications(unaccountedReport(), unaccountedRegistry(), fixtureEvidence()); err != nil {
		t.Fatalf("a judgement whose evidence still holds was rejected: %v", err)
	}
	if err := applyClassifications(classifiableReport(), fixtureRegistry(), fixtureEvidence()); err != nil {
		t.Fatalf("rows the comparator accounts for were made to carry evidence they do not need: %v", err)
	}
}

// TestEvidenceRejectsMalformedBlocks keeps the block itself checkable: a kind
// nothing knows how to re-read, or a kind that needs a location and has none,
// would both read as a pinned fact while pinning nothing.
func TestEvidenceRejectsMalformedBlocks(t *testing.T) {
	cases := []struct {
		name     string
		item     evidenceItem
		wantTerm string
	}{
		{"unknown kind", evidenceItem{Kind: "vibes", Expected: []string{"x"}}, "vibes"},
		{"body evidence with no location", evidenceItem{Kind: evidenceValidatorBody, ExpectedText: "func f() {}"}, "no location"},
		{"binding evidence with no location", evidenceItem{Kind: evidenceValidatorBinding}, "no location"},
		{"body evidence with no source", evidenceItem{Kind: evidenceValidatorBody, Location: "widget/widget.go:isValidTrigger"}, "no expectedText"},
		{"body evidence recording a list", evidenceItem{Kind: evidenceValidatorBody, Location: "widget/widget.go:isValidTrigger", ExpectedText: "func f() {}", Expected: []string{"x"}}, "pins a function's source"},
		{"set evidence recording a text", evidenceItem{Kind: evidenceGoValidate, ExpectedText: "func f() {}"}, "pins a set"},
		{"tag evidence with no location", evidenceItem{Kind: evidenceValidatorBinding}, "no location"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			registry := unaccountedRegistry()
			registry.Fields[0].Evidence = []evidenceItem{test.item}
			err := applyClassifications(unaccountedReport(), registry, fixtureEvidence())
			if err == nil {
				t.Fatal("the malformed evidence block was accepted")
			}
			if !strings.Contains(err.Error(), test.wantTerm) {
				t.Fatalf("error %q does not name %q", err, test.wantTerm)
			}
		})
	}
}

// TestFunctionBodiesReadUnresolvableSelectorsVerbatim covers the extraction the
// whole mechanism rests on. The expressions this corpus needs fingerprinted are
// package-qualified constants — exactly the ones that cannot be resolved — so
// they have to be captured as written, together with the statements that decide
// what happens to them.
func TestFunctionBodiesReadUnresolvableSelectorsVerbatim(t *testing.T) {
	const src = `package widget

func isValidTrigger(v string) bool {
	switch v {
	case other.Gamma, core.Alpha, core.Beta:
		return true
	default:
		return false
	}
}

func unrelated(v int) bool { return v > 0 }
`
	dir := t.TempDir()
	path := filepath.Join(dir, "widget.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	fset, file, err := parseASTFileWithPositions(path)
	if err != nil {
		t.Fatal(err)
	}
	bodies := functionBodies(fset, file)
	got := bodies["isValidTrigger"]
	for _, want := range []string{"other.Gamma, core.Alpha, core.Beta", "return true", "return false"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the extracted body %q does not contain %q", got, want)
		}
	}
	if _, present := bodies["unrelated"]; !present {
		t.Fatal("a function with no switch statement was skipped; every function has to be reachable, since evidence names one by name")
	}
}

// TestFunctionBodiesDistinguishBehaviourFromValues is the extraction-side half
// of the pin, and the one the earlier design failed. Two versions of a
// validator that name exactly the same values, and differ only in what the
// matching branch returns, must extract to different text — otherwise a
// validator can be turned inside out with every recorded value still present.
func TestFunctionBodiesDistinguishBehaviourFromValues(t *testing.T) {
	body := func(returns string) string {
		src := `package widget

func isValidTrigger(v string) bool {
	switch v {
	case core.Alpha, core.Beta:
		return ` + returns + `
	default:
		return false
	}
}
`
		dir := t.TempDir()
		path := filepath.Join(dir, "widget.go")
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		fset, file, err := parseASTFileWithPositions(path)
		if err != nil {
			t.Fatal(err)
		}
		return functionBodies(fset, file)["isValidTrigger"]
	}

	accepts, rejects := body("true"), body("false")
	if accepts == "" {
		t.Fatal("no body was extracted, so this test would compare two empty strings")
	}
	if accepts == rejects {
		t.Fatal("a validator that accepts its listed values and one that rejects them extracted identically, so the evidence pins the values and not the decision")
	}
}

// TestUnaccountedJudgementFailsWhenTheTagIsRebound is the middle link of the
// chain. Rebinding a tag to a different function changes neither the field that
// names the tag nor the source of the function that used to be bound, so a
// judgement pinning only the two ends survives a validator being replaced
// outright — which is how a reject-everything function reaches production with
// the judgement still reading "matches the schema's enum exactly".
func TestUnaccountedJudgementFailsWhenTheTagIsRebound(t *testing.T) {
	t.Run("rebound to another function", func(t *testing.T) {
		tree := fixtureEvidence()
		tree.bindings["widgetKind"] = []string{"rejectEverything at widget/widget.go"}
		tree.bindingSites["widgetKind"] = []string{"rejectEverything at widget/widget.go:12"}

		err := applyClassifications(unaccountedReport(), unaccountedRegistry(), tree)
		if err == nil {
			t.Fatal("the tag was rebound to a different function and the judgement about the old one was kept")
		}
		for _, want := range []string{"isValidTrigger at widget/widget.go", "rejectEverything at widget/widget.go", "recorded", "found"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q does not contain %q, so it does not show which binding moved", err, want)
			}
		}
	})

	t.Run("a second registration added", func(t *testing.T) {
		tree := fixtureEvidence()
		tree.bindings["widgetKind"] = append(tree.bindings["widgetKind"], "shadowingValidator at widget/other.go")

		err := applyClassifications(unaccountedReport(), unaccountedRegistry(), tree)
		if err == nil {
			t.Fatal("a second registration of the same tag was accepted; the library lets the later one replace the earlier silently")
		}
		if !strings.Contains(err.Error(), "shadowingValidator") {
			t.Fatalf("error %q does not name the registration that arrived", err)
		}
	})

	t.Run("the registration removed", func(t *testing.T) {
		tree := fixtureEvidence()
		delete(tree.bindings, "widgetKind")

		err := applyClassifications(unaccountedReport(), unaccountedRegistry(), tree)
		if err == nil {
			t.Fatal("the tag stopped being registered at all and the judgement was kept")
		}
	})
}

// TestEvidenceChainMustBeComplete is the structural half: the requirement is
// about the shape of a judgement, not about any one row. Every gap below is a
// link this gate has already found something slipping through.
func TestEvidenceChainMustBeComplete(t *testing.T) {
	cases := []struct {
		name     string
		evidence []evidenceItem
		wantTerm string
	}{
		{
			name: "a body pinned with no binding reaching it",
			evidence: []evidenceItem{
				{Kind: evidenceGoValidate, Expected: []string{"required", "widgetKind"}},
				{Kind: evidenceValidatorBody, Location: "widget/widget.go:isValidTrigger", ExpectedText: widgetValidatorBody},
			},
			wantTerm: "rebound",
		},
		{
			name: "a binding pinned with the bound function's body unpinned",
			evidence: []evidenceItem{
				{Kind: evidenceGoValidate, Expected: []string{"required", "widgetKind"}},
				{Kind: evidenceValidatorBinding, Location: "widgetKind", Expected: []string{"isValidTrigger at widget/widget.go"}},
			},
			wantTerm: "what it decides is unpinned",
		},
		{
			name: "a validator pinned without the field's own tag",
			evidence: []evidenceItem{
				{Kind: evidenceValidatorBinding, Location: "widgetKind", Expected: []string{"isValidTrigger at widget/widget.go"}},
				{Kind: evidenceValidatorBody, Location: "widget/widget.go:isValidTrigger", ExpectedText: widgetValidatorBody},
			},
			wantTerm: "could name a different tag",
		},
		{
			name: "a binding entry that names no function",
			evidence: []evidenceItem{
				{Kind: evidenceGoValidate, Expected: []string{"required", "widgetKind"}},
				{Kind: evidenceValidatorBinding, Location: "widgetKind", Expected: []string{"widget/widget.go"}},
			},
			wantTerm: "does not name a function",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			registry := unaccountedRegistry()
			registry.Fields[0].Evidence = test.evidence
			err := applyClassifications(unaccountedReport(), registry, fixtureEvidence())
			if err == nil {
				t.Fatal("an incomplete evidence chain was accepted")
			}
			if !strings.Contains(err.Error(), test.wantTerm) {
				t.Fatalf("error %q does not explain the missing link (%q)", err, test.wantTerm)
			}
		})
	}

	// A binding pinned as empty asserts the tag reaches nothing, so it demands
	// no body — the shape the two unregistered-tag judgements in the real
	// corpus have, and it must stay legal.
	t.Run("an empty binding needs no body", func(t *testing.T) {
		registry := unaccountedRegistry()
		registry.Fields[0].Evidence = []evidenceItem{
			{Kind: evidenceGoValidate, Expected: []string{"required", "widgetKind"}},
			{Kind: evidenceValidatorBinding, Location: "unboundTag"},
		}
		if err := applyClassifications(unaccountedReport(), registry, fixtureEvidence()); err != nil {
			t.Fatalf("a judgement asserting a tag is bound to nothing was rejected: %v", err)
		}
	})
}

// TestValidationBindingsReadTagAndFunctionTogether covers the extraction. The
// tag alone is not the fact: which function it reaches is what a body pin hangs
// off, and a registration that binds the same tag twice has to yield two
// entries, since the library lets the later call replace the earlier in silence.
func TestValidationBindingsReadTagAndFunctionTogether(t *testing.T) {
	const src = `package widget

func init() {
	_ = types.Validate.RegisterValidation("widgetKind", isValidWidgetKind)
	_ = types.Validate.RegisterValidation("widgetKind", shadowingValidator)
	_ = types.Validate.RegisterValidation("otherKind", isValidOther)
}
`
	dir := t.TempDir()
	path := filepath.Join(dir, "widget.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	fset, file, err := parseASTFileWithPositions(path)
	if err != nil {
		t.Fatal(err)
	}
	bindings := validationBindings(fset, file)
	if len(bindings["widgetKind"]) != 2 {
		t.Fatalf("widgetKind yielded %d binding(s); both registrations have to be reported, or a shadowing one is invisible", len(bindings["widgetKind"]))
	}
	names := []string{bindings["widgetKind"][0].function, bindings["widgetKind"][1].function}
	sort.Strings(names)
	if !equalStrings(names, []string{"isValidWidgetKind", "shadowingValidator"}) {
		t.Fatalf("bound functions read as %v", names)
	}
	if len(bindings["otherKind"]) != 1 || bindings["otherKind"][0].function != "isValidOther" {
		t.Fatalf("otherKind read as %v", bindings["otherKind"])
	}
}

// TestCollectedBindingsNameTheBoundFunction covers the assembled evidence, not
// just the per-file extraction. Recording only where a tag is registered, and
// not what it is registered to, is the whole defect this link exists to close:
// a rebind leaves the sites where they were and changes only the function.
func TestCollectedBindingsNameTheBoundFunction(t *testing.T) {
	dir := t.TempDir()
	const src = `package widget

func isValidWidgetKind(fl int) bool { return true }

func init() {
	_ = types.Validate.RegisterValidation("widgetKind", isValidWidgetKind)
}
`
	if err := os.WriteFile(filepath.Join(dir, "widget.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	tree, err := collectTreeEvidence(dir)
	if err != nil {
		t.Fatal(err)
	}
	bindings := tree.bindings["widgetKind"]
	if len(bindings) != 1 {
		t.Fatalf("widgetKind collected %d binding(s): %v", len(bindings), bindings)
	}
	if !strings.HasPrefix(bindings[0], "isValidWidgetKind at ") {
		t.Fatalf("the collected binding %q does not name the function the tag is bound to, so a rebind would leave it unchanged", bindings[0])
	}
	if !strings.Contains(tree.bindingSites["widgetKind"][0], ":") {
		t.Fatalf("the reported registration site %q carries no line, so a failure cannot send a reader to it", tree.bindingSites["widgetKind"][0])
	}
}
