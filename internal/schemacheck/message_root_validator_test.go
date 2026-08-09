package main

import "testing"

// rootValidatorCompareOptions points at testdata/rootvalidator: one message
// whose response struct carries a struct-level validation rule registered
// against the struct itself, and whose request holds a cross-package
// composite whose type carries one of the ordinary, field-anchored kind.
func rootValidatorCompareOptions() compareOptions {
	return compareOptions{
		tree:          "testdata/rootvalidator/ocpp2.0.1",
		schemaDirs:    []string{"testdata/rootvalidator/schemas"},
		rawSchemaFlag: "testdata/rootvalidator/schemas",
	}
}

// countStructValidatorRows totals the struct-validator rows across every
// message and both sides, independently of the summary the report computes,
// so the two can be compared rather than one echoing the other.
func countStructValidatorRows(report *Report) int {
	count := 0
	for _, m := range report.Messages {
		for _, side := range []MessageSide{m.Request, m.Response} {
			for _, f := range side.Fields {
				if f.Class == STRUCT_VALIDATOR {
					count++
				}
			}
		}
	}
	return count
}

func hasFieldAtPath(side MessageSide, path string) bool {
	for _, f := range side.Fields {
		if f.Path == path {
			return true
		}
	}
	return false
}

// TestStructValidatorOnMessageRootProducesItsOwnRow covers the one place a
// registered struct-level rule cannot reach a report through field pairing:
// a rule registered against a message's own request or response struct
// belongs to no field, so unless the message root gets a row of its own the
// rule is silently absent from a comparison that claims to omit nothing.
//
// The fixture carries both kinds at once, so the assertions below establish
// that the root row is genuinely additional — not the field-anchored row
// relabelled or moved.
func TestStructValidatorOnMessageRootProducesItsOwnRow(t *testing.T) {
	report, _, err := runComparison(rootValidatorCompareOptions())
	if err != nil {
		t.Fatalf("runComparison: %v", err)
	}
	if len(report.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(report.Messages))
	}
	m := report.Messages[0]

	root := fieldByPath(t, m.Response, rootMessagePath)
	if root.Class != STRUCT_VALIDATOR {
		t.Fatalf("the message-root row is classified %q, want %q (rule: %s)", root.Class, STRUCT_VALIDATOR, root.Rule)
	}
	if len(root.Go.StructValidators) != 1 || root.Go.StructValidators[0] != "validateGadgetResponse" {
		t.Fatalf("message-root row records structValidators %v, want exactly [validateGadgetResponse] — the rule registered against GadgetResponse itself", root.Go.StructValidators)
	}
	if root.Go.Name != "GadgetResponse" || root.Go.DeclaredType != "GadgetResponse" {
		t.Fatalf("message-root row names Go type %q/%q, want GadgetResponse on both — the row describes the response struct itself", root.Go.Name, root.Go.DeclaredType)
	}
	if root.Schema.Pointer != "#" {
		t.Fatalf("message-root row's schema pointer is %q, want %q (the schema document's own root)", root.Schema.Pointer, "#")
	}
	if root.Schema.Type == nil || *root.Schema.Type != "object" {
		t.Fatalf("message-root row's schema type = %v, want the root type the document declares (\"object\")", root.Schema.Type)
	}
	if root.Rule == "" || root.Detail == "" {
		t.Fatalf("message-root row must cite a rule and say why its path is empty; got rule %q detail %q", root.Rule, root.Detail)
	}

	// The row is additional: the response's own fields are untouched, and the
	// field-anchored rule on the request side still reports separately.
	if serial := fieldByPath(t, m.Response, "serial"); serial.Class == STRUCT_VALIDATOR {
		t.Fatal("the response's own serial field was classified STRUCT_VALIDATOR; the root rule must not leak onto the struct's fields")
	}
	part := fieldByPath(t, m.Request, "part")
	if part.Class != STRUCT_VALIDATOR {
		t.Fatalf("request field part is classified %q, want %q — the field-anchored rule must still report", part.Class, STRUCT_VALIDATOR)
	}
	if len(part.Go.StructValidators) != 1 || part.Go.StructValidators[0] != "isValidPart" {
		t.Fatalf("request field part records structValidators %v, want exactly [isValidPart]", part.Go.StructValidators)
	}

	// A message side whose struct carries no rule of its own gets no root
	// row at all — the row is produced by a discovered registration, never
	// emitted unconditionally for every message.
	if hasFieldAtPath(m.Request, rootMessagePath) {
		t.Fatalf("the request side has a message-root row, but no rule is registered against GadgetRequest: %v", fieldPaths(m.Request))
	}

	// Count pinned exactly, and cross-checked against the summary the report
	// publishes, so a row that appears but is not tallied (or the reverse) is
	// caught.
	const wantRows = 2
	if got := countStructValidatorRows(report); got != wantRows {
		t.Fatalf("counted %d struct-validator rows, want %d (one field-anchored on the request, one at the response's root)", got, wantRows)
	}
	if report.Summary.ByClass.STRUCT_VALIDATOR != wantRows {
		t.Fatalf("summary.byClass.STRUCT_VALIDATOR = %d, want %d", report.Summary.ByClass.STRUCT_VALIDATOR, wantRows)
	}
}
