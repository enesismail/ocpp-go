package main

import "testing"

// TestEqualConstraintsComparesPointedToValues is a regression test for a
// real bug this task's own pipeline hit: comparing two Constraints values
// with Go's == operator compares each *int/*float64/*bool/*string field's
// pointer *address*, not what it points to. Every schema walk allocates a
// fresh pointer for the same literal value, so two textually-identical
// declarations (the real case: additionalProperties:false, freshly
// unmarshaled once per schema file) never share an address — a bare ==
// comparison therefore reports every single one of them as a structural
// conflict, which is exactly what a first pass at this self-check did (65
// false-positive conflicts against the real corpus, M7 failing) before this
// function replaced it.
func TestEqualConstraintsComparesPointedToValues(t *testing.T) {
	one := 1
	anotherOne := 1 // deliberately a second allocation of the same value
	two := 2

	if !equalConstraints(Constraints{MaxLength: &one}, Constraints{MaxLength: &anotherOne}) {
		t.Fatal("two distinct pointers to the identical int value were reported unequal")
	}
	if equalConstraints(Constraints{MaxLength: &one}, Constraints{MaxLength: &two}) {
		t.Fatal("two pointers to genuinely different int values were reported equal")
	}
	if !equalConstraints(Constraints{}, Constraints{}) {
		t.Fatal("two zero-value Constraints were reported unequal")
	}
	if equalConstraints(Constraints{MaxLength: &one}, Constraints{}) {
		t.Fatal("a present bound and a nil bound were reported equal")
	}

	trueA, trueB := true, true
	if !equalConstraints(
		Constraints{AdditionalProperties: &trueA},
		Constraints{AdditionalProperties: &trueB},
	) {
		t.Fatal("two distinct pointers to the identical bool value (additionalProperties) were reported unequal")
	}

	numA, numB := 5.0, 5.0
	if !equalConstraints(
		Constraints{ExclusiveMinimum: &ExclusiveBound{Number: &numA}},
		Constraints{ExclusiveMinimum: &ExclusiveBound{Number: &numB}},
	) {
		t.Fatal("two distinct ExclusiveBound pointers wrapping the identical number were reported unequal")
	}
}

// TestDedupCorpusFlagsAGenuineStructuralConflict is dedupCorpus's own
// minimal-pair over the case the corpus-wide dedup exists to police: the
// *same named definition* ("WidgetType", inlined into two different schema
// files per the corpus's own convention) whose properties genuinely
// disagree between the two copies. Two occurrences that agree in every
// fact sameShape checks must not be flagged, even coming from different
// files; a root-level (unnamed-definition) property is deliberately not
// used here — scopeFor keys those by file, so two different files' own root
// properties are distinct declarations by design, never a "conflict".
func TestDedupCorpusFlagsAGenuineStructuralConflict(t *testing.T) {
	prop := func(required bool) SchemaProperty {
		return SchemaProperty{Name: "code", Type: "string", Required: required, EnclosingDefinition: "WidgetType"}
	}

	agreeing := []SchemaDocument{
		{File: "a.json", Properties: []SchemaProperty{prop(true)}},
		{File: "b.json", Properties: []SchemaProperty{prop(true)}},
	}
	deduped := dedupCorpus(agreeing)
	for k, dp := range deduped {
		if dp.conflict {
			t.Fatalf("two agreeing occurrences of %v were flagged as a structural conflict: %#v", k, dp)
		}
	}

	conflicting := []SchemaDocument{
		{File: "a.json", Properties: []SchemaProperty{prop(true)}},
		{File: "b.json", Properties: []SchemaProperty{prop(false)}},
	}
	deduped = dedupCorpus(conflicting)
	found := false
	for _, dp := range deduped {
		if dp.conflict {
			found = true
		}
	}
	if !found {
		t.Fatal("two occurrences of the same named definition's property, disagreeing on required-ness, were not flagged as a structural conflict")
	}
}

func TestCountUnionKeywordsFindsNestedOccurrences(t *testing.T) {
	doc := map[string]any{
		"properties": map[string]any{
			"a": map[string]any{"oneOf": []any{}},
			"b": map[string]any{
				"anyOf": []any{
					map[string]any{"allOf": []any{}},
				},
			},
		},
	}
	got := countUnionKeywords(doc)
	if got != 3 {
		t.Fatalf("countUnionKeywords = %d, want 3 (one oneOf, one anyOf, one nested allOf)", got)
	}
	if countUnionKeywords(map[string]any{"type": "string"}) != 0 {
		t.Fatal("a document with none of oneOf/anyOf/allOf reported a nonzero count")
	}
}
