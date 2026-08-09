package main

import (
	"reflect"
	"strings"
	"testing"
)

// TestSchemaRequiredPropertyModelledOmittable covers the optionality lie: a
// property the schema requires, modelled by a Go field the encoder is free to
// leave out. encoding/json drops an omitempty field whenever it holds its
// zero value, so such a field can produce a payload missing a property the
// schema mandates — a contradiction of the schema that no presence check
// catches, because the field is there; only its guarantee of reaching the
// wire is missing.
func TestSchemaRequiredPropertyModelledOmittable(t *testing.T) {
	fixture := loadClassificationFixture(t, "fork_bug_required_but_omittable.json")
	if fixture.ExpectedClass != FORK_BUG {
		t.Fatalf("fixture declares expectedClass=%q, want %q", fixture.ExpectedClass, FORK_BUG)
	}
	if !fixture.Input.Schema.Required {
		t.Fatal("fixture's schema side does not mark the property required, so there is no requiredness to contradict")
	}
	if !fixture.Input.Go.Omitempty {
		t.Fatal("fixture's Go side is not omittable, so it cannot exercise the lie")
	}

	result, err := classifyField(fixture.Input)
	if err != nil {
		t.Fatalf("classification is not implemented: %v", err)
	}
	if result.Class != FORK_BUG {
		t.Fatalf("a schema-required property modelled omittable classified as %q (rule: %s), want %q", result.Class, result.Rule, FORK_BUG)
	}
	if !strings.Contains(strings.ToLower(result.Rule), "optionality lie") {
		t.Fatalf("rule %q does not carry a sub-kind-distinguishing token, so the roll-up grouped by sub-kind would have no distinguishable key", result.Rule)
	}
}

// TestRequiredValidationMakesAnOmittableFieldHonest is the companion
// direction and a minimal pair against the fixture above: the two agree on
// every observed fact except the validate tag's tokens. A bare "required"
// token rejects the zero value before the struct is ever encoded, so the
// omitempty can never fire on a value that passed validation, and the pairing
// contradicts nothing.
func TestRequiredValidationMakesAnOmittableFieldHonest(t *testing.T) {
	lying := loadClassificationFixture(t, "fork_bug_required_but_omittable.json")
	honest := loadClassificationFixture(t, "required_field_with_required_validation.json")
	if honest.ExpectedClass != IDENTICAL {
		t.Fatalf("fixture declares expectedClass=%q, want %q", honest.ExpectedClass, IDENTICAL)
	}

	realigned := honest.Input
	realigned.Go.Validate = lying.Input.Go.Validate
	if !reflect.DeepEqual(realigned, lying.Input) {
		t.Fatalf("the two fixtures differ beyond the validate tag, so the required token is not the sole discriminator:\nhonest with the tag realigned = %#v\nlying                         = %#v", realigned, lying.Input)
	}

	result, err := classifyField(honest.Input)
	if err != nil {
		t.Fatalf("classification is not implemented: %v", err)
	}
	if result.Class != IDENTICAL {
		t.Fatalf("an omittable field whose validation rejects the zero value classified as %q (rule: %s), want %q", result.Class, result.Rule, IDENTICAL)
	}
}

// TestSchemaOptionalPropertyThatAlwaysEncodesIsNotReported guards the
// direction that must stay quiet. Emitting a property the schema permits but
// does not require always produces a valid payload, so a Go field that never
// omits an optional property contradicts nothing and must not be reported as
// though it did.
func TestSchemaOptionalPropertyThatAlwaysEncodesIsNotReported(t *testing.T) {
	fixture := loadClassificationFixture(t, "fork_bug_required_but_omittable.json")
	input := fixture.Input
	input.Schema.Required = false
	input.Go.Omitempty = false
	input.Go.Pointer = false

	result, err := classifyField(input)
	if err != nil {
		t.Fatalf("classification is not implemented: %v", err)
	}
	if result.Class == FORK_BUG {
		t.Fatalf("a schema-optional property modelled as always encoded was reported as a contradiction (rule: %s)", result.Rule)
	}
}

// TestOptionalityCheckReadsFieldLevelValidationOnly keeps the "required"
// exemption honest: validator.v9 hands every token after "dive" to a slice's
// elements, so a required token down there says the elements must be present,
// never that the field itself will reach the wire.
func TestOptionalityCheckReadsFieldLevelValidationOnly(t *testing.T) {
	arrayType := "array"
	input := ComparisonInput{
		Go: GoField{
			Name: "Data", JSONName: "data",
			DeclaredType: "[]string", WireType: "[]string",
			Slice: true, ElementType: "string", Omitempty: true,
			Validate: []string{"omitempty", "dive", "required"},
		},
		Schema:        SchemaField{Pointer: "#/properties/data", Type: &arrayType, Required: true},
		GoPresent:     true,
		SchemaPresent: true,
	}

	result, err := classifyField(input)
	if err != nil {
		t.Fatalf("classification is not implemented: %v", err)
	}
	if result.Class != FORK_BUG {
		t.Fatalf("an element-level required token was read as a guarantee about the field itself: classified as %q (rule: %s), want %q", result.Class, result.Rule, FORK_BUG)
	}
	if !strings.Contains(strings.ToLower(result.Rule), "optionality lie") {
		t.Fatalf("rule %q does not report the optionality lie", result.Rule)
	}
}
