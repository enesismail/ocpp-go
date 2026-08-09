package main

import (
	"strings"
	"testing"
)

// pointerFamilyInput builds an optional pointer field of one type family
// against the schema property that family pairs with. omitempty is the only
// thing callers vary, so each family is exercised as a minimal pair.
func pointerFamilyInput(declaredType, wireType, schemaType string, slice, customMarshaled, omitempty bool, format *string, enum []string) ComparisonInput {
	schemaTypeCopy := schemaType
	return ComparisonInput{
		Go: GoField{
			Name: "Field", JSONName: "field",
			DeclaredType: declaredType, WireType: wireType,
			Pointer: true, Slice: slice, CustomMarshaled: customMarshaled,
			Omitempty: omitempty,
			// A named enum's value set, so the enum families reach their own
			// mapping rule rather than falling out as unexplained.
			EnumValues: enum,
		},
		Schema: SchemaField{
			Pointer: "#/properties/field", Type: &schemaTypeCopy,
			Required: false, Format: format, Enum: enum,
		},
		GoPresent:     true,
		SchemaPresent: true,
	}
}

// TestNullEmissionIsReachableForEveryPointerFamily is the reachability matrix.
// Whether a nil can reach the wire as null is decided by pointer-ness and the
// omitempty tag alone — it has nothing to do with which type family a row
// belongs to. Asked inside the family stages, the rule was only ever reachable
// for the families that happened to fall through to the end of the classifier;
// every family that answered earlier answered without ever asking.
//
// The left column is the bug: each of these was IDENTICAL, or a mapping change
// that silently outranked a contradiction. The right column is the control —
// with omitempty the pointer is exactly what the mapping asks for, so each row
// must land on its own family's answer instead.
func TestNullEmissionIsReachableForEveryPointerFamily(t *testing.T) {
	dateTime := "date-time"
	enum := []string{"Accepted", "Rejected"}

	cases := []struct {
		family string
		input  func(omitempty bool) ComparisonInput
		// withOmitemptyClass is what the row must become once omitempty makes
		// the pointer able to express absence — its own family's verdict.
		withOmitemptyClass string
	}{
		{
			family: "*types.DateTime",
			input: func(o bool) ComparisonInput {
				return pointerFamilyInput("*types.DateTime", "string", "string", false, true, o, &dateTime, nil)
			},
			withOmitemptyClass: IDENTICAL,
		},
		{
			family: "*string",
			input: func(o bool) ComparisonInput {
				return pointerFamilyInput("*string", "string", "string", false, false, o, nil, nil)
			},
			withOmitemptyClass: SCHEMA_FAITHFUL_CHANGE,
		},
		{
			family: "*NamedEnum",
			input: func(o bool) ComparisonInput {
				return pointerFamilyInput("*BootReason", "string", "string", false, false, o, nil, enum)
			},
			withOmitemptyClass: SCHEMA_FAITHFUL_CHANGE,
		},
		{
			family: "*[]T",
			input: func(o bool) ComparisonInput {
				return pointerFamilyInput("*[]string", "[]string", "array", true, false, o, nil, nil)
			},
			withOmitemptyClass: SCHEMA_FAITHFUL_CHANGE,
		},
		{
			family: "*int",
			input: func(o bool) ComparisonInput {
				return pointerFamilyInput("*int", "int", "integer", false, false, o, nil, nil)
			},
			withOmitemptyClass: IDENTICAL,
		},
		{
			family: "*bool",
			input: func(o bool) ComparisonInput {
				return pointerFamilyInput("*bool", "bool", "boolean", false, false, o, nil, nil)
			},
			withOmitemptyClass: IDENTICAL,
		},
		{
			family: "*Composite",
			input: func(o bool) ComparisonInput {
				return pointerFamilyInput("*types.StatusInfo", "types.StatusInfo", "object", false, false, o, nil, nil)
			},
			withOmitemptyClass: IDENTICAL,
		},
	}

	for _, test := range cases {
		t.Run(test.family+"/no omitempty reports the null", func(t *testing.T) {
			result, err := classifyField(test.input(false))
			if err != nil {
				t.Fatalf("classification is not implemented: %v", err)
			}
			if result.Class != FORK_BUG {
				t.Fatalf("%s with no omitempty classified as %q (rule: %s), want %q — a nil encodes as null whatever the pointer points at", test.family, result.Class, result.Rule, FORK_BUG)
			}
			if !strings.Contains(strings.ToLower(result.Rule), "optionality lie") {
				t.Fatalf("%s rule %q does not report the optionality lie", test.family, result.Rule)
			}
		})

		t.Run(test.family+"/omitempty hands the row to its own family", func(t *testing.T) {
			result, err := classifyField(test.input(true))
			if err != nil {
				t.Fatalf("classification is not implemented: %v", err)
			}
			if result.Class == FORK_BUG && strings.Contains(strings.ToLower(result.Rule), "optionality lie") {
				t.Fatalf("%s with omitempty still reported the optionality lie; the rule must turn on the tag, not on pointer-ness alone", test.family)
			}
			if result.Class != test.withOmitemptyClass {
				t.Fatalf("%s with omitempty classified as %q (rule: %s), want %q — its own family's verdict", test.family, result.Class, result.Rule, test.withOmitemptyClass)
			}
		})
	}
}

// TestNullEmissionExemptsUntypedProperties keeps the rule honest about what it
// claims: it fires because the schema's type names a set of instances that
// excludes null. A property with no type names no such set, so null is a legal
// instance of it and there is nothing to report.
func TestNullEmissionExemptsUntypedProperties(t *testing.T) {
	input := ComparisonInput{
		Go: GoField{
			Name: "Data", JSONName: "data",
			DeclaredType: "*interface{}", WireType: "interface{}",
			Pointer: true, Omitempty: false,
		},
		Schema:        SchemaField{Pointer: "#/properties/data", Type: nil, Required: false},
		GoPresent:     true,
		SchemaPresent: true,
	}
	result, err := classifyField(input)
	if err != nil {
		t.Fatalf("classification is not implemented: %v", err)
	}
	if result.Class == FORK_BUG {
		t.Fatalf("an untyped property was reported as rejecting null (rule: %s); an untyped property constrains nothing", result.Rule)
	}
}

// TestNullEmissionOutranksTheMappingStages pins the priority the fix depends
// on. A *[]T with no omitempty is two things at once: a pointer that can
// encode null, and a shape the array mapping would rewrite. The contradiction
// is reported, because a payload the schema rejects outranks a difference in
// how the schema would be modelled — the same order that puts the requiredness
// rule ahead of the constraint and mapping stages.
func TestNullEmissionOutranksTheMappingStages(t *testing.T) {
	input := pointerFamilyInput("*[]string", "[]string", "array", true, false, false, nil, nil)
	result, err := classifyField(input)
	if err != nil {
		t.Fatalf("classification is not implemented: %v", err)
	}
	if result.Class != FORK_BUG {
		t.Fatalf("a pointer-to-slice with no omitempty classified as %q (rule: %s), want %q", result.Class, result.Rule, FORK_BUG)
	}
	if strings.Contains(strings.ToLower(result.Rule), "array -> slice") {
		t.Fatalf("the mapping change was reported ahead of the contradiction (rule: %s)", result.Rule)
	}
}
