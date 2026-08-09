package main

import (
	"strings"
	"testing"
)

// reportWithStructValidatorRows builds a two-row report by hand: one
// struct-validator row anchored on a field, one at a message root. Building
// it here rather than running the comparison keeps the assertions about the
// *renderer* — the section must exist and must carry every row — independent
// of which rows any particular tree happens to produce.
func reportWithStructValidatorRows() *Report {
	object := "object"
	return &Report{
		Version:   "v201",
		GoTree:    "ocpp2.0.1",
		SchemaDir: "schemas/v201",
		Coverage:  Coverage{Messages: 2, SchemaFiles: 4, UnpairedMessages: []string{}, UnpairedSchemaFiles: []string{}},
		Summary: Summary{
			ByClass: ClassCounts{IDENTICAL: 1, STRUCT_VALIDATOR: 2},
		},
		Messages: []Message{
			{
				FeatureName: "Authorize", Profile: "authorization", GoPackage: "ocpp2.0.1/authorization", GoFile: "authorize.go",
				Request: MessageSide{GoType: "AuthorizeRequest", SchemaFile: "AuthorizeRequest.json", Fields: []Field{
					{
						Path:   "idToken",
						Go:     GoField{Name: "IdToken", StructValidators: []string{"isValidIdToken"}},
						Schema: SchemaField{Pointer: "#/properties/idToken"},
						Class:  STRUCT_VALIDATOR,
						Rule:   "cross-field struct validation rule isValidIdToken has no schema counterpart and needs human adjudication",
					},
				}},
				Response: MessageSide{GoType: "AuthorizeResponse", SchemaFile: "AuthorizeResponse.json"},
			},
			{
				FeatureName: "Heartbeat", Profile: "availability", GoPackage: "ocpp2.0.1/availability", GoFile: "heartbeat.go",
				Request: MessageSide{GoType: "HeartbeatRequest", SchemaFile: "HeartbeatRequest.json"},
				Response: MessageSide{GoType: "HeartbeatResponse", SchemaFile: "HeartbeatResponse.json", Fields: []Field{
					{
						Path:   rootMessagePath,
						Go:     GoField{Name: "HeartbeatResponse", DeclaredType: "HeartbeatResponse", WireType: "object", StructValidators: []string{"validateHeartbeatResponse"}},
						Schema: SchemaField{Pointer: "#", Type: &object, Required: true},
						Class:  STRUCT_VALIDATOR,
						Rule:   "cross-field struct validation rule validateHeartbeatResponse has no schema counterpart and needs human adjudication",
						Detail: "registered against HeartbeatResponse itself rather than against one of its fields, so this row's path is the message root",
					},
					{
						Path:   "currentTime",
						Go:     GoField{Name: "CurrentTime"},
						Schema: SchemaField{Pointer: "#/properties/currentTime"},
						Class:  IDENTICAL,
					},
				}},
			},
		},
	}
}

// TestMarkdownReportListsEveryStructValidatorRow covers the class the report
// exists to hand to a human: struct-level rules have no mechanical
// resolution, so a count alone is useless — each row must be listed with
// enough pointers to find both the rule and the schema location it has no
// counterpart at.
func TestMarkdownReportListsEveryStructValidatorRow(t *testing.T) {
	md := renderMarkdownReport(reportWithStructValidatorRows(), selfCheckResult{})

	if !strings.Contains(md, "## Struct-level validation rules") {
		t.Fatal("the report has no struct-level validation rules section, so its STRUCT_VALIDATOR count cannot be adjudicated from the document")
	}
	section := sectionOf(t, md, "## Struct-level validation rules")

	for _, want := range []string{
		"isValidIdToken", "#/properties/idToken", "IdToken", "ocpp2.0.1/authorization/authorize.go",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("the field-anchored struct-validator row is missing %q:\n%s", want, section)
		}
	}
	for _, want := range []string{
		"validateHeartbeatResponse", "HeartbeatResponse", "ocpp2.0.1/availability/heartbeat.go", "(message root)",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("the message-root struct-validator row is missing %q:\n%s", want, section)
		}
	}
	if strings.Contains(section, "currentTime") {
		t.Errorf("the section lists a row that is not a struct-validator row:\n%s", section)
	}
}

// TestMarkdownReportRendersStructValidatorSectionWhenEmpty guards the
// wrongful-rejection direction: a tree with no struct-level rules at all
// must still say so, rather than dropping the section and leaving a reader
// unable to tell "none" from "not looked for".
func TestMarkdownReportRendersStructValidatorSectionWhenEmpty(t *testing.T) {
	report := reportWithStructValidatorRows()
	report.Messages = nil
	md := renderMarkdownReport(report, selfCheckResult{})
	section := sectionOf(t, md, "## Struct-level validation rules")
	if !strings.Contains(section, "None.") {
		t.Fatalf("a report with no struct-level rules does not say so:\n%s", section)
	}
}

// TestMarkdownReportStatesItsLimitations covers the one wrong reading the
// class histogram invites: that nothing unexplained means nothing wrong. The
// document has to state what it did not look at, or a zero is read as a
// clean bill of health for questions the instrument never asked.
func TestMarkdownReportStatesItsLimitations(t *testing.T) {
	md := renderMarkdownReport(reportWithStructValidatorRows(), selfCheckResult{})

	if !strings.Contains(md, "## Limitations") {
		t.Fatal("the report states no limitations, so UNEXPLAINED = 0 reads as semantic completeness")
	}
	section := sectionOf(t, md, "## Limitations")

	// Each of these names a whole question the comparison does not ask. A
	// reader who takes UNEXPLAINED = 0 for agreement is wrong about every one
	// of them, so each has to be stated, not merely implied.
	for _, want := range []struct{ subject, marker string }{
		{"the UNEXPLAINED caveat", "does **not** mean"},
		{"validator function bodies", "Validator function bodies"},
		{"cross-field constraints", "Cross-field constraints"},
		{"custom marshaler behaviour", "Custom marshaler behaviour"},
		{"runtime conformance", "Runtime conformance"},
		{"uncompared constraint keywords", "Constraint keywords outside the compared set"},
	} {
		if !strings.Contains(section, want.marker) {
			t.Errorf("the limitations section does not state %s (looking for %q):\n%s", want.subject, want.marker, section)
		}
	}
}

// sectionOf returns the text of the named Markdown section, up to the next
// heading at the same level or the end of the document.
func sectionOf(t *testing.T, md, heading string) string {
	t.Helper()
	start := strings.Index(md, heading)
	if start < 0 {
		t.Fatalf("no %q section in the rendered report", heading)
	}
	rest := md[start+len(heading):]
	if end := strings.Index(rest, "\n## "); end >= 0 {
		return rest[:end]
	}
	return rest
}
