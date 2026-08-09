package main

import (
	"encoding/json"
	"testing"
)

// fixtureCompareOptions points at the small hand-built tree+schema pair
// under testdata/orchestration: one functional-block package (widgetblock) with
// one bidirectional-shaped message, and a sibling "types" package (named to
// match the real tree's own shared-types convention) supplying a
// cross-package enum, a cross-package composite carrying a struct-level
// validator, and a slice of that composite — enough to exercise discovery,
// direction detection, cross-file/cross-package stitching, struct-validator
// annotation, pairing/pruning, self-check computation and shared-type
// tracking together, end to end, without touching the real ocpp2.0.1 tree.
func fixtureCompareOptions() compareOptions {
	return compareOptions{
		tree:          "testdata/orchestration/tree",
		schemaDirs:    []string{"testdata/orchestration/schemas"},
		rawSchemaFlag: "testdata/orchestration/schemas",
	}
}

// fieldByPath finds side's field row at path, failing the test if it is
// absent — every assertion below is a spot check against a specific,
// hand-derived expectation, not a re-derivation of the whole report.
func fieldByPath(t *testing.T, side MessageSide, path string) Field {
	t.Helper()
	for _, f := range side.Fields {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("no field at path %q in %s (%d fields): %#v", path, side.GoType, len(side.Fields), side.Fields)
	return Field{}
}

func TestRunComparisonEndToEndOverFixtureTree(t *testing.T) {
	report, selfCheck, err := runComparison(fixtureCompareOptions())
	if err != nil {
		t.Fatalf("runComparison: %v", err)
	}

	if report.Coverage.Messages != 1 {
		t.Fatalf("coverage.messages = %d, want 1", report.Coverage.Messages)
	}
	if report.Coverage.SchemaFiles != 2 {
		t.Fatalf("coverage.schemaFiles = %d, want 2", report.Coverage.SchemaFiles)
	}
	if len(report.Coverage.UnpairedMessages) != 0 {
		t.Fatalf("unpairedMessages = %v, want none — the fixture's one message has both its schema files", report.Coverage.UnpairedMessages)
	}
	if len(report.Coverage.UnpairedSchemaFiles) != 0 {
		t.Fatalf("unpairedSchemaFiles = %v, want none", report.Coverage.UnpairedSchemaFiles)
	}
	if len(report.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(report.Messages))
	}

	m := report.Messages[0]
	if m.FeatureName != "Widget" {
		t.Fatalf("featureName = %q, want %q", m.FeatureName, "Widget")
	}
	if m.Profile != "widgetblock" {
		t.Fatalf("profile = %q, want %q", m.Profile, "widgetblock")
	}
	if m.Direction != "CS->CSMS" {
		t.Fatalf("direction = %q, want %q — the fixture declares OnWidget only on CSMSHandler", m.Direction, "CS->CSMS")
	}
	if m.Prototype {
		t.Fatal("fixture message classified as prototype:true; only the four real prototype message names should ever be")
	}
	if m.Complexity <= 0 {
		t.Fatalf("complexity = %d, want > 0", m.Complexity)
	}

	// Spot-check one row of every class the fixture was built to exercise.
	cases := []struct {
		side  MessageSide
		path  string
		class string
	}{
		{m.Request, "name", IDENTICAL},                // required string, matches exactly
		{m.Request, "status", SCHEMA_FAITHFUL_CHANGE}, // cross-package enum, values match the schema's
		{m.Request, "detail", STRUCT_VALIDATOR},       // cross-package composite carrying a struct-level rule
		{m.Request, "detail.code", IDENTICAL},         // detail's own field, reached only by stitching
		{m.Request, "detail.extra", FORK_BUG},         // schema-required, absent from the Go struct
		{m.Request, "secret", FORK_BUG},               // Go-only field, no schema counterpart
		{m.Request, "comment", ADDITIVE},              // schema-only optional field
		{m.Request, "tags", STRUCT_VALIDATOR},         // array of the same struct-validated composite
		{m.Request, "tags.code", IDENTICAL},           // array-element stitching reaches the same field
		{m.Response, "accepted", IDENTICAL},
		{m.Response, "level", OVERRIDE_CANDIDATE}, // Go's max=5 is stricter than the schema's maximum=10
		{m.Response, "reason", FORK_BUG},          // required in schema, absent from Go — root-level, no cascade needed
		{m.Response, "note", ADDITIVE},
	}
	for _, c := range cases {
		got := fieldByPath(t, c.side, c.path)
		if got.Class != c.class {
			t.Errorf("field %s.%s classified as %q, want %q (rule: %s)", c.side.GoType, c.path, got.Class, c.class, got.Rule)
		}
	}

	// The pruning fix (pair.go) must not have swallowed detail/tags' own
	// rows while correctly still reaching their genuine children — this
	// asserts the exact set for the request side rather than just spot
	// checks, so a regression that drops or duplicates a row is caught.
	wantRequestPaths := map[string]bool{
		"name": true, "status": true, "detail": true, "detail.code": true,
		"detail.notes": true, "detail.extra": true, "tags": true, "tags.code": true,
		"tags.notes": true, "tags.extra": true, "secret": true, "comment": true,
	}
	if len(m.Request.Fields) != len(wantRequestPaths) {
		t.Fatalf("request has %d fields, want %d: %v", len(m.Request.Fields), len(wantRequestPaths), fieldPaths(m.Request))
	}
	for _, f := range m.Request.Fields {
		if !wantRequestPaths[f.Path] {
			t.Errorf("unexpected request field path %q", f.Path)
		}
	}

	// The struct-level validator must not leak onto detail's own children:
	// only the field whose own type is the validated composite carries it.
	if code := fieldByPath(t, m.Request, "detail.code"); len(code.Go.StructValidators) != 0 {
		t.Fatalf("detail.code carries structValidators %v; the rule targets Detail itself, not its fields", code.Go.StructValidators)
	}

	// Shared-type tracking: types.Detail is reached twice (once as
	// "detail", once as "tags"' element type), and the report's GoType text
	// matches how the field itself spells it (declaredType), never a fully
	// module-qualified form.
	if len(report.SharedTypes) != 1 {
		t.Fatalf("sharedTypes = %#v, want exactly one entry (types.Detail)", report.SharedTypes)
	}
	st := report.SharedTypes[0]
	if st.GoType != "types.Detail" || st.SchemaDefinition != "DetailType" {
		t.Fatalf("sharedTypes[0] = %#v, want GoType=types.Detail SchemaDefinition=DetailType", st)
	}
	if st.Occurrences != 2 {
		t.Fatalf("types.Detail occurrences = %d, want 2 (the detail field and the tags element type)", st.Occurrences)
	}

	// Self-check: the fixture's schema corpus is tiny, so it cannot
	// reproduce the real v201 corpus's M1-M6 numbers (408, 252, ...) — this
	// is the self-check-failure path the task asks to be covered, and it
	// must report "fail" honestly rather than silently passing or panicking.
	if len(report.SelfCheck) != 7 {
		t.Fatalf("selfCheck has %d entries, want 7 (M1-M7)", len(report.SelfCheck))
	}
	sawFail := false
	for _, c := range report.SelfCheck {
		if c.Status != "pass" && c.Status != "fail" {
			t.Errorf("selfCheck %s has status %q, want pass or fail", c.ID, c.Status)
		}
		if c.Status == "fail" {
			sawFail = true
		}
	}
	if !sawFail {
		t.Fatal("no self-check entry reported fail against the fixture's tiny corpus; the fixture no longer exercises the failure path")
	}
	if selfCheck.composition.total == 0 {
		t.Fatal("self-check composition breakdown was not populated")
	}
}

// TestRunComparisonIsDeterministic covers determinism at the orchestration
// level: two independent runs over the same fixture inputs must marshal to
// byte-identical JSON.
func TestRunComparisonIsDeterministic(t *testing.T) {
	reportA, _, err := runComparison(fixtureCompareOptions())
	if err != nil {
		t.Fatalf("first runComparison: %v", err)
	}
	reportB, _, err := runComparison(fixtureCompareOptions())
	if err != nil {
		t.Fatalf("second runComparison: %v", err)
	}

	jsonA, err := marshalReportJSON(reportA)
	if err != nil {
		t.Fatalf("marshal first report: %v", err)
	}
	jsonB, err := marshalReportJSON(reportB)
	if err != nil {
		t.Fatalf("marshal second report: %v", err)
	}
	if string(jsonA) != string(jsonB) {
		t.Fatalf("two runs over identical inputs produced different JSON:\n--- A ---\n%s\n--- B ---\n%s", jsonA, jsonB)
	}

	// A field-order shuffle bug would still marshal identically if the
	// second run coincidentally iterated in the same order; parse and
	// spot-check message/field ordering is itself sorted, not merely equal
	// to itself.
	var decoded Report
	if err := json.Unmarshal(jsonA, &decoded); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	for i := 1; i < len(decoded.Messages); i++ {
		if decoded.Messages[i-1].FeatureName > decoded.Messages[i].FeatureName {
			t.Fatalf("messages are not sorted by featureName: %q before %q", decoded.Messages[i-1].FeatureName, decoded.Messages[i].FeatureName)
		}
	}
}

func fieldPaths(side MessageSide) []string {
	paths := make([]string, len(side.Fields))
	for i, f := range side.Fields {
		paths[i] = f.Path
	}
	return paths
}

func TestBuildSummaryComputesRollups(t *testing.T) {
	messages := []Message{
		{
			FeatureName: "A", Complexity: 10, Prototype: true,
			Request: MessageSide{Fields: []Field{
				{Path: "x", Class: IDENTICAL},
				{Path: "y", Class: SCHEMA_FAITHFUL_CHANGE, Breaking: true},
			}},
			Response: MessageSide{Fields: []Field{
				{Path: "z", Class: OVERRIDE_CANDIDATE},
			}},
		},
		{
			FeatureName: "B", Complexity: 20, Prototype: false,
			Request: MessageSide{Fields: []Field{
				{Path: "w", Class: FORK_BUG},
			}},
		},
	}
	summary, err := buildSummary(messages)
	if err != nil {
		t.Fatalf("buildSummary: %v", err)
	}
	if summary.ByClass.IDENTICAL != 1 || summary.ByClass.SCHEMA_FAITHFUL_CHANGE != 1 ||
		summary.ByClass.OVERRIDE_CANDIDATE != 1 || summary.ByClass.FORK_BUG != 1 {
		t.Fatalf("byClass = %#v, want one of each of the four classes exercised", summary.ByClass)
	}
	if summary.BreakingFields != 1 {
		t.Fatalf("breakingFields = %d, want 1", summary.BreakingFields)
	}
	if summary.MessagesWithBreakingFields != 1 {
		t.Fatalf("messagesWithBreakingFields = %d, want 1", summary.MessagesWithBreakingFields)
	}
	if summary.MessagesNeedingOverrides != 1 {
		t.Fatalf("messagesNeedingOverrides = %d, want 1", summary.MessagesNeedingOverrides)
	}
	if summary.OverrideDensity != 0.5 {
		t.Fatalf("overrideDensity = %v, want 0.5 (one of two messages)", summary.OverrideDensity)
	}
	if summary.PrototypeCount != 1 {
		t.Fatalf("prototypeCount = %d, want 1", summary.PrototypeCount)
	}
	if summary.ComplexityDistribution.Min != 10 || summary.ComplexityDistribution.Max != 20 {
		t.Fatalf("complexityDistribution = %#v, want min=10 max=20", summary.ComplexityDistribution)
	}
}

func TestDeriveVersionLabel(t *testing.T) {
	cases := []struct{ tree, want string }{
		{"ocpp2.0.1", "v201"},
		{"ocpp1.6", "v16"},
		{"path/to/ocpp2.0.1", "v201"},
	}
	for _, c := range cases {
		if got := deriveVersionLabel(c.tree); got != c.want {
			t.Errorf("deriveVersionLabel(%q) = %q, want %q", c.tree, got, c.want)
		}
	}
}
