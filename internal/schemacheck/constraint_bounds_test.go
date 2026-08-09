package main

import (
	"strings"
	"testing"
)

// TestOneSidedConstraintBoundsAreDivergences covers the two cases where only
// one side declares a bound. Both are real divergences and neither can be
// caught by comparing two numbers, because one of the two numbers does not
// exist:
//
//   - the schema bounds a property the hand-written code does not, so the
//     hand-written code admits values the schema rejects — looser than the
//     schema, and the reason a required element-count floor the tree never
//     adopted is otherwise invisible;
//   - the hand-written code bounds a property the schema does not, so it
//     rejects values the schema admits — stricter than the schema, which is
//     what the override-candidate class exists to record.
//
// Skipping either one drops it from the report silently, which is
// indistinguishable in the output from the two sides agreeing.
func TestOneSidedConstraintBoundsAreDivergences(t *testing.T) {
	cases := []struct {
		name              string
		file              string
		wantClass         string
		wantRuleFragments []string
	}{
		{
			// The real shape: a schema array property with an element-count
			// floor and ceiling, against a Go slice that adopted only the
			// ceiling.
			name:              "schema declares a bound the Go field does not",
			file:              "constraint_schema_only_bound.json",
			wantClass:         FORK_BUG,
			wantRuleFragments: []string{"looser", "minItems"},
		},
		{
			// The real shape: a Go integer field with a non-negativity floor
			// against a schema integer property that sets no minimum.
			name:              "Go field declares a bound the schema does not",
			file:              "constraint_go_only_bound.json",
			wantClass:         OVERRIDE_CANDIDATE,
			wantRuleFragments: []string{"stricter", "gte=0", "no bound"},
		},
		{
			// Both sides bound the field, and they agree — the case that must
			// stay quiet, or every bounded field in the tree becomes a row.
			name:      "both sides declare matching bounds",
			file:      "constraint_both_sides_match.json",
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
				if !strings.Contains(strings.ToLower(result.Rule), strings.ToLower(fragment)) {
					t.Fatalf("rule %q does not name %q, so the divergence cannot be grouped or acted on", result.Rule, fragment)
				}
			}
		})
	}
}

// TestSchemaOnlyBoundNamesTheKeywordForTheGoKind guards the direction of the
// schema-side lookup: which schema keyword a bound token corresponds to
// depends on the Go field's own kind, and a one-sided report has to name the
// keyword that actually applies. Reading a slice's floor out of minLength —
// which an array property never carries — would find nothing and report
// agreement where a real divergence exists.
func TestSchemaOnlyBoundNamesTheKeywordForTheGoKind(t *testing.T) {
	fixture := loadClassificationFixture(t, "constraint_schema_only_bound.json")
	if !fixture.Input.Go.Slice {
		t.Fatalf("fixture does not exercise a slice field: %#v", fixture.Input.Go)
	}
	if fixture.Input.Schema.Constraints.MinLength != nil {
		t.Fatal("fixture carries minLength, which would let a kind-blind implementation pass for the wrong reason")
	}
	if fixture.Input.Schema.Constraints.MinItems == nil {
		t.Fatal("fixture must carry the schema element-count floor for there to be a one-sided bound at all")
	}

	result, matched := compareConstraints(fixture.Input)
	if !matched {
		t.Fatal("compareConstraints reported no divergence for a field the schema bounds and the Go field does not")
	}
	if result.Class != FORK_BUG {
		t.Fatalf("a bound only the schema declares classified as %q, want %q", result.Class, FORK_BUG)
	}
}

// TestElementLevelBoundsDoNotBoundTheSliceItself covers the tokens after
// "dive", which validator.v9 hands to a slice's elements. Reading one of them
// as a bound on the slice would both invent a Go-side element-count bound
// that does not exist and compare a string-length ceiling against an
// element-count one.
func TestElementLevelBoundsDoNotBoundTheSliceItself(t *testing.T) {
	maxItems := 4
	arrayType := "array"
	input := ComparisonInput{
		Go: GoField{
			Name: "Data", JSONName: "data",
			DeclaredType: "[]string", WireType: "[]string",
			Slice: true, ElementType: "string", Omitempty: true,
			// The ceiling here bounds each element's length, not the number
			// of elements; the slice itself carries no bound at all.
			Validate: []string{"omitempty", "dive", "max=512"},
		},
		Schema: SchemaField{
			Pointer: "#/properties/data", Type: &arrayType,
			Constraints: Constraints{MaxItems: &maxItems},
		},
		GoPresent:     true,
		SchemaPresent: true,
	}

	result, matched := compareConstraints(input)
	if !matched {
		t.Fatalf("the schema's element-count ceiling was not reported as a bound the Go field lacks; the element-level bound was read as the slice's own")
	}
	if result.Class != FORK_BUG {
		t.Fatalf("classified as %q (rule: %s), want %q — the element-level ceiling of 512 must not be read as an element-count ceiling stricter than the schema's 4", result.Class, result.Rule, FORK_BUG)
	}
	if strings.Contains(result.Rule, "512") {
		t.Fatalf("rule %q cites the element-level bound as if it bounded the slice", result.Rule)
	}
}

// TestExclusiveBoundTokensCountAsDeclaredBounds guards against a new
// false-positive class the one-sided reporting could otherwise open: a field
// bounded with an exclusive token really is bounded, and reporting that it
// "declares no bound" would be untrue. An exclusive bound at the schema's own
// value is strictly tighter than the schema's inclusive one, never equal to
// it.
func TestExclusiveBoundTokensCountAsDeclaredBounds(t *testing.T) {
	minimum := 0.0
	integerType := "integer"
	input := ComparisonInput{
		Go: GoField{
			Name: "EvseID", JSONName: "evseId",
			DeclaredType: "int", WireType: "int",
			Validate: []string{"gt=0"},
		},
		Schema: SchemaField{
			Pointer: "#/properties/evseId", Type: &integerType,
			Constraints: Constraints{Minimum: &minimum},
		},
		GoPresent:     true,
		SchemaPresent: true,
	}

	result, matched := compareConstraints(input)
	if !matched {
		t.Fatal("an exclusive Go bound against an inclusive schema bound of the same value was reported as agreement")
	}
	if result.Class != OVERRIDE_CANDIDATE {
		t.Fatalf("classified as %q (rule: %s), want %q — gt=0 admits strictly fewer values than minimum=0", result.Class, result.Rule, OVERRIDE_CANDIDATE)
	}
	if strings.Contains(strings.ToLower(result.Rule), "no bound") {
		t.Fatalf("rule %q claims the Go field declares no bound, but it declares gt=0", result.Rule)
	}
}
