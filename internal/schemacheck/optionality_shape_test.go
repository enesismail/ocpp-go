package main

import (
	"strings"
	"testing"
)

// TestCompositeShapes covers the $ref-to-object properties, which the
// declared-shape stage previously said nothing about at all: their wire type
// agrees with the schema's "object" whatever their pointer-ness, so every one
// of them reached the end of the classifier and was reported as matching.
//
// A required composite is always present, so the mapping models it as a value
// and a pointer expresses a state the schema does not have. An optional one is
// the reverse, and more sharply: omitempty has no effect on a struct, so a
// composite value always encodes and absence cannot be expressed in any form.
func TestCompositeShapes(t *testing.T) {
	cases := []struct {
		name              string
		file              string
		wantClass         string
		wantBreaking      bool
		wantRuleFragments []string
	}{
		{
			name:              "required composite modelled as a pointer",
			file:              "schema_faithful_required_composite_pointer.json",
			wantClass:         SCHEMA_FAITHFUL_CHANGE,
			wantBreaking:      true,
			wantRuleFragments: []string{"required composite", "value"},
		},
		{
			name:              "optional composite modelled as a value",
			file:              "schema_faithful_optional_composite_value.json",
			wantClass:         SCHEMA_FAITHFUL_CHANGE,
			wantBreaking:      true,
			wantRuleFragments: []string{"optional composite", "pointer", "omitempty does not apply to structs"},
		},
		{
			// The shape the mapping asks for: nothing to report.
			name:      "optional composite modelled as a pointer with omitempty",
			file:      "optional_composite_pointer_identical.json",
			wantClass: IDENTICAL,
		},
		{
			// The pointer is right but nothing omits it, so a nil encodes as
			// null — which a property typed "object" does not admit. The field
			// can therefore put a value on the wire the schema rejects.
			name:              "optional composite pointer with no omitempty",
			file:              "fork_bug_optional_composite_without_omitempty.json",
			wantClass:         FORK_BUG,
			wantRuleFragments: []string{"optionality lie", "null"},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			fixture := loadClassificationFixture(t, test.file)
			if fixture.ExpectedClass != test.wantClass {
				t.Fatalf("fixture %s declares expectedClass=%q, want %q", test.file, fixture.ExpectedClass, test.wantClass)
			}
			result, err := classifyField(fixture.Input)
			if err != nil {
				t.Fatalf("classification is not implemented: %v", err)
			}
			if result.Class != test.wantClass {
				t.Fatalf("%s classified as %q (rule: %s), want %q", test.name, result.Class, result.Rule, test.wantClass)
			}
			if result.Breaking != test.wantBreaking {
				t.Fatalf("%s breaking=%t, want %t", test.name, result.Breaking, test.wantBreaking)
			}
			for _, fragment := range test.wantRuleFragments {
				if !strings.Contains(strings.ToLower(result.Rule), strings.ToLower(fragment)) {
					t.Fatalf("rule %q does not name %q", result.Rule, fragment)
				}
			}
		})
	}
}

// TestRequiredCompositeValueIsUnchanged is the negative control for the
// required-composite rule, over the fixture that already pins the matching
// shape: a required composite held in a value is what the mapping asks for and
// must keep reporting nothing.
func TestRequiredCompositeValueIsUnchanged(t *testing.T) {
	fixture := loadClassificationFixture(t, "wiretype_object_identical.json")
	if !fixture.Input.Schema.Required || fixture.Input.Go.Pointer {
		t.Fatalf("fixture no longer exercises a required composite held in a value: %#v", fixture.Input)
	}
	result, err := classifyField(fixture.Input)
	if err != nil {
		t.Fatalf("classification is not implemented: %v", err)
	}
	if result.Class != IDENTICAL {
		t.Fatalf("a required composite held in a value classified as %q (rule: %s), want %q", result.Class, result.Rule, IDENTICAL)
	}
}

// TestZeroExcludingFloorIsDecidedBeforeGoShape covers both directions of the
// exemption a numeric floor above zero creates. The exemption is a statement
// about the schema — the property never admits zero, so omitempty cannot drop
// a legal value — and it therefore has to be settled from the schema before
// the Go field is looked at.
//
// Settled the other way round, from the Go side first, a pointer field is
// waved through before the exemption is ever consulted, and the one shape the
// exemption actually makes wrong goes unreported: under it the faithful shape
// is the value type, so a pointer is the divergence.
func TestZeroExcludingFloorIsDecidedBeforeGoShape(t *testing.T) {
	t.Run("pointer where the narrowed shape is a value", func(t *testing.T) {
		for _, file := range []string{
			"schema_faithful_pointer_where_floor_excludes_zero.json",
			// The same fact spelled as draft-06's standalone exclusive bound
			// rather than an inclusive floor.
			"schema_faithful_pointer_where_exclusive_floor_excludes_zero.json",
		} {
			fixture := loadClassificationFixture(t, file)
			if !fixture.Input.Go.Pointer || fixture.Input.Schema.Required {
				t.Fatalf("%s does not exercise an optional pointer: %#v", file, fixture.Input)
			}
			result, err := classifyField(fixture.Input)
			if err != nil {
				t.Fatalf("classification is not implemented: %v", err)
			}
			if result.Class != SCHEMA_FAITHFUL_CHANGE {
				t.Fatalf("%s classified as %q (rule: %s), want %q — a floor above zero makes omitempty unambiguous, so the value type is the faithful shape and the pointer is the divergence", file, result.Class, result.Rule, SCHEMA_FAITHFUL_CHANGE)
			}
			if !result.Breaking {
				t.Fatalf("%s: dropping the pointer changes the declared type, so this row must be breaking", file)
			}
			if !strings.Contains(strings.ToLower(result.Rule), "floor excludes zero") {
				t.Fatalf("%s rule %q does not cite the exemption that makes the value type faithful", file, result.Rule)
			}
		}
	})

	t.Run("value where the pointer shape is required", func(t *testing.T) {
		// No floor, so zero is admissible and the pointer stands; the value
		// here is the divergence. Holding the two directions in one test is
		// what stops an implementation from satisfying either alone by
		// reporting every optional numeric, or none of them.
		fixture := loadClassificationFixture(t, "schema_faithful_optional_integer.json")
		if fixture.Input.Go.Pointer || fixture.Input.Schema.Constraints.Minimum != nil {
			t.Fatalf("fixture does not exercise a value with no zero-excluding floor: %#v", fixture.Input)
		}
		result, err := classifyField(fixture.Input)
		if err != nil {
			t.Fatalf("classification is not implemented: %v", err)
		}
		if result.Class != SCHEMA_FAITHFUL_CHANGE || !result.Breaking {
			t.Fatalf("optional integer held in a value classified as %q (breaking=%t), want %q and breaking", result.Class, result.Breaking, SCHEMA_FAITHFUL_CHANGE)
		}
	})

	t.Run("value under the exemption reports nothing", func(t *testing.T) {
		// The shape the exemption asks for. If the exemption were applied to
		// the wrong side, this is what would start being reported.
		fixture := loadClassificationFixture(t, "optional_integer_with_positive_minimum.json")
		result, err := classifyField(fixture.Input)
		if err != nil {
			t.Fatalf("classification is not implemented: %v", err)
		}
		if result.Class != IDENTICAL {
			t.Fatalf("value + omitempty under a zero-excluding floor classified as %q (rule: %s), want %q", result.Class, result.Rule, IDENTICAL)
		}
	})
}
