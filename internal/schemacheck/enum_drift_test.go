package main

import (
	"reflect"
	"strings"
	"testing"
)

// TestWalkedFieldsCarryTheirValidatorsAcceptedValues covers the half of enum
// handling that lives in the walk: a validate tag naming a registered
// validator says nothing on its own about which values that validator
// accepts, so the accepted set discovered from the validator's own switch has
// to reach the field carrying the tag. Without it, no amount of comparison
// downstream can detect a drifted value set, because one side is always
// empty.
func TestWalkedFieldsCarryTheirValidatorsAcceptedValues(t *testing.T) {
	path := fixturePath(t, "go_ast", "enum_fields.go")
	structs, err := walkGoFile(path, newResolution(newTypeResolver(), newWireTypeRegistry()))
	if err != nil {
		t.Fatalf("Go AST walker is not implemented for %s: %v", path, err)
	}
	payload := findStruct(structs, "EnumPayload")
	if payload == nil {
		t.Fatalf("EnumPayload was not discovered: %#v", structs)
	}

	state := findGoField(payload.Fields, "State")
	if state == nil {
		t.Fatalf("State field was not walked: %#v", payload.Fields)
	}
	want := []string{"Idle", "Uploaded"}
	if !reflect.DeepEqual(state.EnumValues, want) {
		t.Fatalf("field carrying validate tag %v has enumValues %#v, want %#v — the tag alone says nothing about which values it accepts", state.Validate, state.EnumValues, want)
	}

	// Negative control: a field whose tags name no registered validator has
	// no extractable value set, and must not be given one.
	plain := findGoField(payload.Fields, "Plain")
	if plain == nil {
		t.Fatalf("Plain field was not walked: %#v", payload.Fields)
	}
	if len(plain.EnumValues) != 0 {
		t.Fatalf("field with no registered validator tag was given enumValues %#v", plain.EnumValues)
	}

	// A tag placed after "dive" constrains the slice's elements, not the
	// slice. Attaching it to the slice would pit a set of element values
	// against a schema array property, which lists none, and report drift
	// that does not exist.
	states := findGoField(payload.Fields, "States")
	if states == nil {
		t.Fatalf("States field was not walked: %#v", payload.Fields)
	}
	if len(states.EnumValues) != 0 {
		t.Fatalf("slice field was given the element-level (post-dive) tag's values %#v as its own value set", states.EnumValues)
	}
}

// TestResolutionSuppliesValidatorTagsRegisteredElsewhere covers the other
// source of a field's value set: the real tree registers many validator tags
// in its shared types package, not in the message file that uses them, so a
// per-file walk has to be able to take those in from the tree-wide registry.
func TestResolutionSuppliesValidatorTagsRegisteredElsewhere(t *testing.T) {
	path := fixturePath(t, "go_ast", "enum_fields.go")
	resolution := newResolution(newTypeResolver(), newWireTypeRegistry())
	resolution.RegisterEnum("max", []string{"unreachable"}) // a bound token is never a tag name
	resolution.RegisterEnum("elsewhere", []string{"A", "B"})

	structs, err := walkGoFile(path, resolution)
	if err != nil {
		t.Fatalf("Go AST walker is not implemented for %s: %v", path, err)
	}
	payload := findStruct(structs, "EnumPayload")
	if payload == nil {
		t.Fatalf("EnumPayload was not discovered: %#v", structs)
	}
	plain := findGoField(payload.Fields, "Plain")
	if plain == nil {
		t.Fatalf("Plain field was not walked: %#v", payload.Fields)
	}
	// "max=20" is a bound, not a tag name; a lookup that split on "=" and
	// matched the left-hand side would wrongly hand this field a value set.
	if len(plain.EnumValues) != 0 {
		t.Fatalf("a bound token was matched as a validator tag name: %#v", plain.EnumValues)
	}
}

// TestEnumValueSetDrift covers the comparison half: two sides that both
// enumerate values must be compared as sets, in both directions, and any
// difference reported as a contradiction of the schema. A value the schema
// lists and the tree rejects makes the fork refuse a legal message; a value
// the tree accepts and the schema does not lets an illegal one through.
func TestEnumValueSetDrift(t *testing.T) {
	cases := []struct {
		name              string
		file              string
		wantClass         string
		wantRuleFragments []string
	}{
		{
			// Declaration order differs between the two sides here, so a
			// comparison done as ordered lists rather than sets would report
			// drift that does not exist.
			name:      "matching value sets in different order",
			file:      "enum_set_matches.json",
			wantClass: IDENTICAL,
		},
		{
			name:              "schema lists a value the tree rejects",
			file:              "fork_bug_enum_drift.json",
			wantClass:         FORK_BUG,
			wantRuleFragments: []string{"enum value-set drift", "AcceptedCanceled"},
		},
		{
			name:              "tree accepts a value the schema does not list",
			file:              "fork_bug_enum_drift_go_only_value.json",
			wantClass:         FORK_BUG,
			wantRuleFragments: []string{"enum value-set drift", "Retired"},
		},
		{
			// Nothing was extractable from the Go side, so there is no set to
			// compare. Silence is not agreement, but it is not evidence of
			// drift either, and reporting one would be a fabricated finding.
			name:      "no extractable value set on the Go side",
			file:      "enum_no_extractable_values.json",
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
			for _, fragment := range test.wantRuleFragments {
				if !strings.Contains(result.Rule, fragment) {
					t.Fatalf("rule %q does not name %q, so the drift cannot be grouped or acted on", result.Rule, fragment)
				}
			}
		})
	}
}

// TestEnumValueSetDriftIsolatesTheValueSet keeps the drift fixture a minimal
// pair against the matching one: the two agree on every observed fact except
// the schema's own value list, so nothing else can account for the different
// outcome.
func TestEnumValueSetDriftIsolatesTheValueSet(t *testing.T) {
	matching := loadClassificationFixture(t, "enum_set_matches.json")
	drifted := loadClassificationFixture(t, "fork_bug_enum_drift.json")

	realigned := drifted.Input
	realigned.Schema.Enum = matching.Input.Schema.Enum
	if !reflect.DeepEqual(realigned, matching.Input) {
		t.Fatalf("drift fixture differs from the matching one beyond the schema's value list:\ndrifted with the schema list realigned = %#v\nmatching                              = %#v", realigned, matching.Input)
	}

	result, err := classifyField(drifted.Input)
	if err != nil {
		t.Fatalf("classification is not implemented: %v", err)
	}
	if result.Class != FORK_BUG {
		t.Fatalf("a row whose only difference from a matching one is the schema's value list classified as %q, want %q", result.Class, FORK_BUG)
	}
}
