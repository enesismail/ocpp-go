package main

import (
	"strings"
	"testing"
)

// TestDateTimeValueFieldIsNotIdentical covers the pointer half of the
// date-time rule. Its two existing fixtures both agree on pointer-ness and so
// only ever exercised whether a custom marshaler ran at all; this one runs one
// and still diverges, because the mapping's output is a pointer to the
// date-time type and the field is a value.
//
// The rule carries no required/optional split: the narrowing that moves an
// optional field from a pointer to value + omitempty is confined to strings
// and enums, and a date-time property is explicitly held out of it, so the
// pointer stands either way.
func TestDateTimeValueFieldIsNotIdentical(t *testing.T) {
	fixture := loadClassificationFixture(t, "schema_faithful_datetime_value.json")
	if fixture.ExpectedClass != SCHEMA_FAITHFUL_CHANGE {
		t.Fatalf("fixture declares expectedClass=%q, want %q", fixture.ExpectedClass, SCHEMA_FAITHFUL_CHANGE)
	}
	if fixture.Input.Go.Pointer {
		t.Fatal("fixture's Go side is a pointer, so it cannot exercise the value-modelled case")
	}
	if strings.TrimPrefix(fixture.Input.Go.DeclaredType, "*") == fixture.Input.Go.WireType {
		t.Fatal("fixture's declaredType and wireType agree, so no custom marshaler ran and this is the plain-string case the existing fixtures already cover")
	}

	result, err := classifyField(fixture.Input)
	if err != nil {
		t.Fatalf("classification is not implemented: %v", err)
	}
	if result.Class == IDENTICAL {
		t.Fatal("a value-typed date-time field was classified as IDENTICAL, so every such field would be missing from the epoch-break count")
	}
	if result.Class != SCHEMA_FAITHFUL_CHANGE {
		t.Fatalf("value-typed date-time field classified as %q (rule: %s), want %q", result.Class, result.Rule, SCHEMA_FAITHFUL_CHANGE)
	}
	if !strings.Contains(strings.ToLower(result.Rule), "date-time") {
		t.Fatalf("rule %q does not cite the date-time mapping rule", result.Rule)
	}
	if !result.Breaking {
		t.Fatal("swapping a value for a pointer changes the declared type, so this row must be breaking")
	}
}

// TestOptionalZeroValueShapes covers the two shapes whose Go zero value is an
// ordinary value the schema admits. A number's 0 and a boolean's false are not
// "empty" in any sense the schema has, so value + omitempty cannot express
// them: encoding/json drops the field and a reader cannot tell the value from
// its absence. The mapping models both as pointers.
func TestOptionalZeroValueShapes(t *testing.T) {
	cases := []struct {
		name              string
		file              string
		wantClass         string
		wantBreaking      bool
		wantRuleFragments []string
	}{
		{
			name:              "optional integer modelled as value + omitempty",
			file:              "schema_faithful_optional_integer.json",
			wantClass:         SCHEMA_FAITHFUL_CHANGE,
			wantBreaking:      true,
			wantRuleFragments: []string{"optional integer", "pointer", "0"},
		},
		{
			name:              "optional boolean modelled as value + omitempty",
			file:              "schema_faithful_optional_boolean.json",
			wantClass:         SCHEMA_FAITHFUL_CHANGE,
			wantBreaking:      true,
			wantRuleFragments: []string{"optional boolean", "pointer", "false"},
		},
		{
			// No omitempty, so the field always encodes and its zero value is
			// perfectly expressible. Nothing diverges, and a rule keyed off
			// the type name alone would wrongly report that it did.
			name:      "optional integer that always encodes",
			file:      "optional_integer_without_omitempty.json",
			wantClass: IDENTICAL,
		},
		{
			// The documented exception: a floor above zero means the schema
			// never admits the zero value, so omitting it loses nothing.
			name:      "optional integer whose schema floor excludes zero",
			file:      "optional_integer_with_positive_minimum.json",
			wantClass: IDENTICAL,
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

// TestRequiredZeroValueShapeIsNotReported guards the optionality key: a
// required property is not this rule's business at all. Where it is also
// modelled omittable the row is already reported as a contradiction of the
// schema, which must keep outranking a mapping change.
func TestRequiredZeroValueShapeIsNotReported(t *testing.T) {
	fixture := loadClassificationFixture(t, "schema_faithful_optional_integer.json")
	input := fixture.Input
	input.Schema.Required = true

	result, err := classifyField(input)
	if err != nil {
		t.Fatalf("classification is not implemented: %v", err)
	}
	if result.Class != FORK_BUG {
		t.Fatalf("a required property modelled omittable classified as %q (rule: %s), want %q", result.Class, result.Rule, FORK_BUG)
	}
	if !strings.Contains(strings.ToLower(result.Rule), "optionality lie") {
		t.Fatalf("rule %q reports the mapping change instead of the contradiction", result.Rule)
	}
}

// TestUntypedPropertyShape covers the schema's own untyped properties — the
// two that carry an arbitrary JSON value by design. The only Go shape that
// holds one is the empty interface, which is already nilable, so an optional
// untyped property needs no pointer on top.
func TestUntypedPropertyShape(t *testing.T) {
	t.Run("empty interface matches", func(t *testing.T) {
		fixture := loadClassificationFixture(t, "untyped_empty_interface_identical.json")
		if fixture.Input.Schema.Type != nil {
			t.Fatalf("fixture's schema side carries a type (%q), so it does not exercise an untyped property", *fixture.Input.Schema.Type)
		}
		result, err := classifyField(fixture.Input)
		if err != nil {
			t.Fatalf("classification is not implemented: %v", err)
		}
		if result.Class != IDENTICAL {
			t.Fatalf("an untyped property held in an empty interface classified as %q (rule: %s), want %q", result.Class, result.Rule, IDENTICAL)
		}
		// Decided by a rule rather than reached by falling off the end of the
		// classifier, which is what let every other untyped shape through.
		if result.Rule == "" {
			t.Fatal("the untyped shape was not decided by any rule; it reached IDENTICAL by fallthrough")
		}
	})

	t.Run("narrower Go type is a mapping change", func(t *testing.T) {
		fixture := loadClassificationFixture(t, "schema_faithful_untyped_narrowed.json")
		result, err := classifyField(fixture.Input)
		if err != nil {
			t.Fatalf("classification is not implemented: %v", err)
		}
		if result.Class != SCHEMA_FAITHFUL_CHANGE {
			t.Fatalf("an untyped property held in a narrower Go type classified as %q (rule: %s), want %q", result.Class, result.Rule, SCHEMA_FAITHFUL_CHANGE)
		}
		if !result.Breaking {
			t.Fatal("widening the field to an empty interface changes its declared type, so this row must be breaking")
		}
	})
}
