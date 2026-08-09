package main

import (
	"strings"
	"testing"
)

// optionalPointerWithTokens builds an optional pointer field with no json
// omitempty — the shape the null-emission rule is about — varying only the
// validate tag, so the token is the sole discriminator.
func optionalPointerWithTokens(validate []string) ComparisonInput {
	objectType := "object"
	return ComparisonInput{
		Go: GoField{
			Name: "StatusInfo", JSONName: "statusInfo",
			DeclaredType: "*types.StatusInfo", WireType: "types.StatusInfo",
			Pointer: true, Omitempty: false, Validate: validate,
		},
		Schema: SchemaField{
			Pointer: "#/properties/statusInfo", Type: &objectType, Required: false,
		},
		GoPresent:     true,
		SchemaPresent: true,
	}
}

// TestRequiredTokenExemptsTheNullEmissionRule covers the exemption the two
// optionality rules must share. The rule reports that a nil can reach the wire
// as null; a bare required token means validation rejects that nil before
// anything is encoded, so the payload the rule is about can never occur, and
// reporting it would inflate the count with a defect that cannot happen.
//
// The pair below differs in nothing but the token.
func TestRequiredTokenExemptsTheNullEmissionRule(t *testing.T) {
	t.Run("with a required token the rule stays silent", func(t *testing.T) {
		result, err := classifyField(optionalPointerWithTokens([]string{"required"}))
		if err != nil {
			t.Fatalf("classification is not implemented: %v", err)
		}
		if result.Class == FORK_BUG {
			t.Fatalf("a pointer whose validation rejects nil was reported as able to ship a null (rule: %s)", result.Rule)
		}
	})

	t.Run("without it the rule still fires", func(t *testing.T) {
		result, err := classifyField(optionalPointerWithTokens([]string{"omitempty"}))
		if err != nil {
			t.Fatalf("classification is not implemented: %v", err)
		}
		if result.Class != FORK_BUG {
			t.Fatalf("an optional pointer with no omitempty and no required validation classified as %q (rule: %s), want %q", result.Class, result.Rule, FORK_BUG)
		}
		if !strings.Contains(strings.ToLower(result.Rule), "optionality lie") {
			t.Fatalf("rule %q does not report the optionality lie", result.Rule)
		}
	})

	t.Run("an element-level required token does not exempt the field", func(t *testing.T) {
		// Everything after "dive" constrains a slice's elements, so a required
		// token there says the elements must be present — never that the field
		// itself will hold a non-nil value.
		result, err := classifyField(optionalPointerWithTokens([]string{"omitempty", "dive", "required"}))
		if err != nil {
			t.Fatalf("classification is not implemented: %v", err)
		}
		if result.Class != FORK_BUG {
			t.Fatalf("an element-level required token was read as a guarantee about the field itself: classified as %q (rule: %s), want %q", result.Class, result.Rule, FORK_BUG)
		}
	})
}

// TestBothOptionalityRulesShareTheExemption pins the two sides as one rule.
// They are the same question asked in opposite directions — can this field put
// a payload on the wire the schema rejects — so a validator that rules out the
// offending value has to answer both. Reading the token on one side only is
// how the two drifted apart in the first place.
func TestBothOptionalityRulesShareTheExemption(t *testing.T) {
	integerType := "integer"

	// The mirror shape: schema-required, held in a pointer, tagged omitempty.
	// Its required token is what makes it honest, and always has.
	requiredSide := ComparisonInput{
		Go: GoField{
			Name: "RequestID", JSONName: "requestId",
			DeclaredType: "*int", WireType: "int",
			Pointer: true, Omitempty: true, Validate: []string{"required"},
		},
		Schema:        SchemaField{Pointer: "#/properties/requestId", Type: &integerType, Required: true},
		GoPresent:     true,
		SchemaPresent: true,
	}
	if _, matched := compareRequiredness(requiredSide); matched {
		t.Fatal("the required-side rule fired despite a required token; it has read that token since it was written")
	}

	optionalSide := optionalPointerWithTokens([]string{"required"})
	if _, matched := compareNullEmission(optionalSide); matched {
		t.Fatal("the optional-side rule fired despite a required token; the two sides disagree about the same fact")
	}

	// And with the token gone, both fire again — so the exemption is what is
	// being tested, not two rules that happen never to fire.
	requiredSide.Go.Validate = []string{"omitempty"}
	if _, matched := compareRequiredness(requiredSide); !matched {
		t.Fatal("the required-side rule stopped firing without a required token")
	}
	if _, matched := compareNullEmission(optionalPointerWithTokens(nil)); !matched {
		t.Fatal("the optional-side rule stopped firing without a required token")
	}
}

// TestSchemaOptionalFieldMandatedByValidationIsNotReported pins the shape the
// exemption creates: a property the schema says may be absent, held in a field
// validation insists is present. It is a strange shape, and it is deliberately
// silent.
//
// It is not a contradiction: always sending an optional property is valid
// against the schema, the same reasoning that keeps a value field which always
// encodes unreported. It is stricter than the schema, but the stricter/looser
// machinery is defined over a fixed mapping of bound keywords — maxLength,
// minLength, minimum, maximum, minItems, maxItems — and presence is not among
// them; presence divergence has exactly one named outcome, the optionality lie
// in the other direction. So there is no row for it to occupy, and inventing
// one would be a mapping-table extension rather than a reading of it.
func TestSchemaOptionalFieldMandatedByValidationIsNotReported(t *testing.T) {
	result, err := classifyField(optionalPointerWithTokens([]string{"required"}))
	if err != nil {
		t.Fatalf("classification is not implemented: %v", err)
	}
	if result.Class == FORK_BUG {
		t.Fatalf("classified as a contradiction (rule: %s); always sending an optional property is valid against the schema", result.Rule)
	}
	if result.Class == OVERRIDE_CANDIDATE {
		t.Fatalf("classified as %q (rule: %s); the stricter-than-schema class is defined over bound keywords, and presence is not one of them", result.Class, result.Rule)
	}
	if result.Class != IDENTICAL {
		t.Fatalf("classified as %q (rule: %s), want %q — the pointer is the shape the mapping asks for, and nothing else about the field diverges", result.Class, result.Rule, IDENTICAL)
	}
}
