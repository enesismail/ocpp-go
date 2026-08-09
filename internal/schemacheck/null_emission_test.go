package main

import (
	"strings"
	"testing"
)

// TestNilPointerWithoutOmitemptyIsAnOptionalityLie covers the rule across the
// shapes it applies to, not just the one it was first written for. A nil
// pointer with nothing to omit it encodes as null, and none of the schema
// types this stage handles admits null: a nil *int is as invalid against
// "integer" as a nil *T is against "object". Scoping the rule to composites
// left every other pointer shape reporting a match it does not have.
func TestNilPointerWithoutOmitemptyIsAnOptionalityLie(t *testing.T) {
	cases := []struct {
		name       string
		file       string
		schemaKind string
	}{
		{name: "pointer to a numeric", file: "fork_bug_optional_pointer_numeric_without_omitempty.json", schemaKind: "integer"},
		{name: "pointer to a boolean", file: "fork_bug_optional_pointer_boolean_without_omitempty.json", schemaKind: "boolean"},
		// The shape the rule started on, kept here so the composite case and
		// the newly covered ones are read as one rule rather than two.
		{name: "pointer to a composite", file: "fork_bug_optional_composite_without_omitempty.json", schemaKind: "object"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			fixture := loadClassificationFixture(t, test.file)
			if !fixture.Input.Go.Pointer || fixture.Input.Go.Omitempty || fixture.Input.Schema.Required {
				t.Fatalf("fixture does not exercise an optional pointer with no omitempty: %#v", fixture.Input)
			}
			result, err := classifyField(fixture.Input)
			if err != nil {
				t.Fatalf("classification is not implemented: %v", err)
			}
			if result.Class != FORK_BUG {
				t.Fatalf("an optional %s held in a pointer with no omitempty classified as %q (rule: %s), want %q — a nil encodes as null, which the schema type does not admit", test.schemaKind, result.Class, result.Rule, FORK_BUG)
			}
			if !strings.Contains(strings.ToLower(result.Rule), "optionality lie") {
				t.Fatalf("rule %q does not report the optionality lie", result.Rule)
			}
			if !strings.Contains(strings.ToLower(result.Rule), "null") {
				t.Fatalf("rule %q does not name what actually reaches the wire", result.Rule)
			}
		})
	}
}

// TestOptionalPointerWithOmitemptyIsUnchanged is the rule's own negative
// control: the pointer is what the mapping asks for, and omitempty is what
// makes it able to express absence. Together they are the faithful shape and
// must keep reporting nothing — a rule that fired on pointer-ness alone would
// report every optional field in the tree.
func TestOptionalPointerWithOmitemptyIsUnchanged(t *testing.T) {
	for _, file := range []string{
		"optional_pointer_numeric_identical.json",
		"optional_composite_pointer_identical.json",
	} {
		t.Run(file, func(t *testing.T) {
			fixture := loadClassificationFixture(t, file)
			if !fixture.Input.Go.Pointer || !fixture.Input.Go.Omitempty {
				t.Fatalf("fixture does not exercise an optional pointer with omitempty: %#v", fixture.Input.Go)
			}
			result, err := classifyField(fixture.Input)
			if err != nil {
				t.Fatalf("classification is not implemented: %v", err)
			}
			if result.Class != IDENTICAL {
				t.Fatalf("the faithful shape classified as %q (rule: %s), want %q", result.Class, result.Rule, IDENTICAL)
			}
		})
	}
}

// TestOptionalValueWithoutOmitemptyIsStillNotReported guards the boundary the
// widening had to respect. A value field with no omitempty always encodes its
// value, and emitting a property the schema permits but does not require is
// never invalid — nothing reaches the wire that the schema rejects, so there
// is nothing to report. The rule turns on the field being nilable, which is
// what makes null reachable, never on the omitempty tag alone.
func TestOptionalValueWithoutOmitemptyIsStillNotReported(t *testing.T) {
	for _, file := range []string{
		"optional_integer_without_omitempty.json",
		"wiretype_object_identical.json",
	} {
		t.Run(file, func(t *testing.T) {
			fixture := loadClassificationFixture(t, file)
			input := fixture.Input
			input.Schema.Required = false
			input.Go.Omitempty = false
			input.Go.Pointer = false

			result, err := classifyField(input)
			if err != nil {
				t.Fatalf("classification is not implemented: %v", err)
			}
			if result.Class == FORK_BUG {
				t.Fatalf("a value field that always encodes was reported as a contradiction (rule: %s); it can put nothing on the wire the schema rejects", result.Rule)
			}
		})
	}
}
