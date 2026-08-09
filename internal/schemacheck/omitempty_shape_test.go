package main

import (
	"strings"
	"testing"
)

// TestOmitemptyCanOmitFollowsEncodingJSONEmptyValues pins the one place the
// semantics live. encoding/json omits a field only when its value is the empty
// one for its kind, and it recognizes an empty value for a specific list of
// kinds — notably not for a struct, which has none and so always marshals.
//
// Every rule that reasons about omission reads this, so getting it wrong once
// is getting it wrong everywhere; the table below is deliberately exhaustive
// over the shapes the walker can produce rather than over the cases any one
// rule happens to hit.
func TestOmitemptyCanOmitFollowsEncodingJSONEmptyValues(t *testing.T) {
	cases := []struct {
		name  string
		field GoField
		want  bool
	}{
		{name: "pointer", field: GoField{DeclaredType: "*int", WireType: "int", Pointer: true}, want: true},
		{name: "pointer to struct", field: GoField{DeclaredType: "*types.StatusInfo", WireType: "types.StatusInfo", Pointer: true}, want: true},
		{name: "slice", field: GoField{DeclaredType: "[]string", WireType: "[]string", Slice: true}, want: true},
		{name: "map", field: GoField{DeclaredType: "map[string]string", WireType: "map[string]string"}, want: true},
		{name: "empty interface", field: GoField{DeclaredType: "interface{}", WireType: "interface{}"}, want: true},
		{name: "string", field: GoField{DeclaredType: "string", WireType: "string"}, want: true},
		{name: "named string alias", field: GoField{DeclaredType: "BootReason", WireType: "string"}, want: true},
		{name: "integer", field: GoField{DeclaredType: "int", WireType: "int"}, want: true},
		{name: "float", field: GoField{DeclaredType: "float64", WireType: "float64"}, want: true},
		{name: "boolean", field: GoField{DeclaredType: "bool", WireType: "bool"}, want: true},
		// The case the whole helper exists for: a struct held by value.
		{name: "struct value", field: GoField{DeclaredType: "types.StatusInfo", WireType: "types.StatusInfo"}, want: false},
		{name: "same-file struct value", field: GoField{DeclaredType: "ModemType", WireType: "ModemType"}, want: false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := omitemptyCanOmit(test.field); got != test.want {
				t.Fatalf("omitemptyCanOmit(%s) = %t, want %t", test.field.DeclaredType, got, test.want)
			}
		})
	}
}

// TestOmitemptyOnAStructValueIsNotAnOptionalityLie covers the rule that read
// the tag without asking whether it could fire. A struct held by value is
// never omitted, so tagging it omitempty is misleading but takes nothing off
// the wire: the required property is always encoded, as {} if nothing was set.
// Reporting it as a contradiction of the schema is a finding about a payload
// that cannot occur.
func TestOmitemptyOnAStructValueIsNotAnOptionalityLie(t *testing.T) {
	fixture := loadClassificationFixture(t, "required_composite_value_with_omitempty.json")
	if !fixture.Input.Schema.Required || !fixture.Input.Go.Omitempty || fixture.Input.Go.Pointer {
		t.Fatalf("fixture no longer exercises a required struct value tagged omitempty: %#v", fixture.Input)
	}
	result, err := classifyField(fixture.Input)
	if err != nil {
		t.Fatalf("classification is not implemented: %v", err)
	}
	if result.Class == FORK_BUG {
		t.Fatalf("a required struct value tagged omitempty was reported as an optionality lie, but encoding/json never omits a struct (rule: %s)", result.Rule)
	}
	if result.Class != IDENTICAL {
		t.Fatalf("required struct value tagged omitempty classified as %q (rule: %s), want %q", result.Class, result.Rule, IDENTICAL)
	}
}

// TestNilableNonPointerShapeStillReportsTheOptionalityLie is the negative
// control: the fix must turn on what encoding/json can omit, not on a coarse
// "is it a pointer" test. An empty interface is neither a pointer nor a slice,
// yet a nil one is omitted, so a required property held in one and tagged
// omitempty really can go missing.
func TestNilableNonPointerShapeStillReportsTheOptionalityLie(t *testing.T) {
	fixture := loadClassificationFixture(t, "fork_bug_required_interface_with_omitempty.json")
	if fixture.Input.Go.Pointer || fixture.Input.Go.Slice {
		t.Fatalf("fixture is a pointer or slice, so it does not exercise the non-pointer nilable case: %#v", fixture.Input.Go)
	}
	result, err := classifyField(fixture.Input)
	if err != nil {
		t.Fatalf("classification is not implemented: %v", err)
	}
	if result.Class != FORK_BUG {
		t.Fatalf("a required property held in an empty interface tagged omitempty classified as %q (rule: %s), want %q — a nil interface is omitted", result.Class, result.Rule, FORK_BUG)
	}
	if !strings.Contains(strings.ToLower(result.Rule), "optionality lie") {
		t.Fatalf("rule %q does not report the optionality lie", result.Rule)
	}
}

// TestPointerToSliceIsASchemaFaithfulChange covers the array shape, which the
// declared-shape stage skipped outright: every slice returned before any rule
// could look at it, so a pointer wrapped around one was reported as matching.
//
// A slice already expresses absence — it is nilable, and omitempty omits it —
// so the mapping for an array property is the bare slice, required or not. The
// pointer adds a second, redundant spelling of absence and changes the
// declared type, so it is breaking.
func TestPointerToSliceIsASchemaFaithfulChange(t *testing.T) {
	for _, file := range []string{
		"schema_faithful_pointer_to_slice_required.json",
		"schema_faithful_pointer_to_slice_optional.json",
	} {
		t.Run(file, func(t *testing.T) {
			fixture := loadClassificationFixture(t, file)
			if !fixture.Input.Go.Slice || !fixture.Input.Go.Pointer {
				t.Fatalf("fixture does not exercise a pointer to a slice: %#v", fixture.Input.Go)
			}
			result, err := classifyField(fixture.Input)
			if err != nil {
				t.Fatalf("classification is not implemented: %v", err)
			}
			if result.Class != SCHEMA_FAITHFUL_CHANGE {
				t.Fatalf("a pointer to a slice classified as %q (rule: %s), want %q", result.Class, result.Rule, SCHEMA_FAITHFUL_CHANGE)
			}
			if !result.Breaking {
				t.Fatal("dropping the pointer changes the declared type, so this row must be breaking")
			}
			if !strings.Contains(strings.ToLower(result.Rule), "slice") {
				t.Fatalf("rule %q does not cite the array mapping", result.Rule)
			}
		})
	}
}

// TestBareSliceIsUnchanged is the array rule's negative control, over the
// fixture that already pins the matching shape: the mapping asks for a bare
// slice, so a bare slice must keep reporting nothing.
func TestBareSliceIsUnchanged(t *testing.T) {
	fixture := loadClassificationFixture(t, "wiretype_array_identical.json")
	if !fixture.Input.Go.Slice || fixture.Input.Go.Pointer {
		t.Fatalf("fixture no longer exercises a bare slice: %#v", fixture.Input.Go)
	}
	result, err := classifyField(fixture.Input)
	if err != nil {
		t.Fatalf("classification is not implemented: %v", err)
	}
	if result.Class != IDENTICAL {
		t.Fatalf("a bare slice classified as %q (rule: %s), want %q", result.Class, result.Rule, IDENTICAL)
	}
}
