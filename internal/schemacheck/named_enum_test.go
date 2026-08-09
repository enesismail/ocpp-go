package main

import (
	"strings"
	"testing"
)

// namedEnumInput builds the shape this rule is about: a schema property typed
// string with an enumerated value list, against a Go field whose declared type
// is a named scalar alias rather than a bare string. goEnum and schemaEnum are
// the two accepted-value sets, which is the only thing the callers below vary.
func namedEnumInput(goEnum, schemaEnum []string) ComparisonInput {
	stringType := "string"
	return ComparisonInput{
		Go: GoField{
			Name: "Reason", JSONName: "reason",
			DeclaredType: "BootReason", WireType: "string",
			Validate:   []string{"required", "bootReason"},
			EnumValues: goEnum,
		},
		Schema: SchemaField{
			Pointer: "#/properties/reason", Type: &stringType,
			Required: true, Enum: schemaEnum,
		},
		GoPresent:     true,
		SchemaPresent: true,
	}
}

// TestNamedEnumTypeIsASchemaFaithfulChange covers the mapping rule for the
// idiom the tree writes schema enums in: a named scalar alias whose accepted
// values were recovered from the validator enforcing them. The schema-faithful
// mapping writes that property as its own named enum type, derived from the
// schema definition's name, so adopting it swaps the hand-written type for a
// different one — a declared-type change, and breaking on that account, the
// same way the date-time rule's swap to *types.DateTime is.
func TestNamedEnumTypeIsASchemaFaithfulChange(t *testing.T) {
	values := []string{"PowerUp", "LocalReset", "RemoteReset"}
	result, err := classifyField(namedEnumInput(values, values))
	if err != nil {
		t.Fatalf("classification is not implemented: %v", err)
	}
	if result.Class != SCHEMA_FAITHFUL_CHANGE {
		t.Fatalf("a named scalar alias backing a schema enum classified as %q (rule: %s), want %q", result.Class, result.Rule, SCHEMA_FAITHFUL_CHANGE)
	}
	if !strings.Contains(strings.ToLower(result.Rule), "named enum type") {
		t.Fatalf("rule %q does not name the mapping that produces the change", result.Rule)
	}
	if !result.Breaking {
		t.Fatal("adopting the generated enum type in place of the hand-written one changes the field's declared type, so this row must be breaking")
	}
}

// TestNamedAliasWithNoExtractableValuesStaysUnexplained is the guard on the
// distinguishing fact. The rule above turns on a value set actually having
// been recovered from the tree; with an empty one, nothing established that
// the named type is an enum at all, and the row must keep falling through to
// the class reserved for shapes nothing accounts for. The classifier's own
// fixture for that class is exactly this shape, so a rule that fired on the
// named type alone would overturn it.
func TestNamedAliasWithNoExtractableValuesStaysUnexplained(t *testing.T) {
	input := namedEnumInput(nil, []string{"PowerUp", "LocalReset", "RemoteReset"})
	result, err := classifyField(input)
	if err != nil {
		t.Fatalf("classification is not implemented: %v", err)
	}
	if result.Class != UNEXPLAINED {
		t.Fatalf("a named alias with no recovered value set classified as %q (rule: %s), want %q", result.Class, result.Rule, UNEXPLAINED)
	}

	// The same holds against the pinned fixture for that class, whose Go side
	// is the same shape: a named declared type over a string wire type, with
	// no value set recovered.
	fixture := loadClassificationFixture(t, "UNEXPLAINED.json")
	if len(fixture.Input.Go.EnumValues) != 0 {
		t.Fatalf("the unexplained fixture now carries a value set (%v), so it no longer isolates the empty-set case", fixture.Input.Go.EnumValues)
	}
	fixtureResult, err := classifyField(fixture.Input)
	if err != nil {
		t.Fatalf("classification is not implemented: %v", err)
	}
	if fixtureResult.Class != UNEXPLAINED {
		t.Fatalf("the pinned unexplained fixture now classifies as %q, so the named-enum rule fired on the named type alone", fixtureResult.Class)
	}
}

// TestNamedEnumTypeRuleRunsAfterValueSetComparison pins the ordering. A named
// alias whose recovered values disagree with the schema's is a contradiction
// of the schema first and a mapping change second: reporting it as a mapping
// change would bury a real interop defect — the tree refusing a value the
// protocol permits — inside a class that means "nothing is wrong, generation
// would just write this differently".
func TestNamedEnumTypeRuleRunsAfterValueSetComparison(t *testing.T) {
	input := namedEnumInput(
		[]string{"PowerUp", "LocalReset"},
		[]string{"PowerUp", "LocalReset", "RemoteReset"},
	)
	result, err := classifyField(input)
	if err != nil {
		t.Fatalf("classification is not implemented: %v", err)
	}
	if result.Class != FORK_BUG {
		t.Fatalf("a named alias whose values drift from the schema's classified as %q (rule: %s), want %q", result.Class, result.Rule, FORK_BUG)
	}
	if !strings.Contains(result.Rule, "enum value-set drift") {
		t.Fatalf("rule %q reports the mapping change instead of the value-set contradiction", result.Rule)
	}
	if result.Breaking {
		t.Fatal("breaking is meaningful only on a schema-faithful mapping change; a contradiction of the schema must never carry it")
	}
}
