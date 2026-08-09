package main

import "testing"

// TestMessageComplexityDedupsByFileAndPointer covers the complexity metric's
// own dedup identity (schema file, JSON pointer) — distinct from every other
// dedup identity this package uses: the same pointer text in the request and
// the response document must count twice (they are different files), while
// the same pointer reached twice *within* one document (a shared definition
// referenced from two places) must count once.
func TestMessageComplexityDedupsByFileAndPointer(t *testing.T) {
	req := SchemaDocument{
		File: "FooRequest.json",
		Properties: []SchemaProperty{
			{Pointer: "#/properties/a", Path: "a"},
			// Same pointer reached twice within this one document (a shared
			// definition referenced from two places) - counts once.
			{Pointer: "#/definitions/D/properties/x", Path: "a.x", EnclosingDefinition: "D"},
			{Pointer: "#/definitions/D/properties/x", Path: "b.x", EnclosingDefinition: "D"},
			{Pointer: "#/definitions/E/properties/y", Path: "a.y", EnclosingDefinition: "E", Enum: []string{"ON", "OFF"}},
		},
	}
	resp := SchemaDocument{
		File: "FooResponse.json",
		Properties: []SchemaProperty{
			// Same pointer text as a request-side property, but a different
			// file: must count as its own, separate occurrence.
			{Pointer: "#/properties/a", Path: "a"},
		},
	}

	got := messageComplexity(req, resp)
	// definitions: {FooRequest.json,#/definitions/D} + {FooRequest.json,#/definitions/E} = 2
	// properties: 3 distinct (file,pointer) pairs on the request side (the
	// twice-reached #/definitions/D/properties/x counts once) + 1 on the
	// response side (the same pointer text as the request's "a", but a
	// different file) = 4
	// enumProps: 1 ({FooRequest.json,#/definitions/E/properties/y})
	want := 2 + 4 + 1
	if got != want {
		t.Fatalf("messageComplexity = %d, want %d", got, want)
	}
}

func TestComplexityDistributionOfRejectsEmptyInput(t *testing.T) {
	if _, err := complexityDistributionOf(nil); err == nil {
		t.Fatal("complexityDistributionOf accepted an empty score list")
	}
}

func TestComplexityDistributionOfMatchesHandComputedQuartiles(t *testing.T) {
	scores := []int{5, 1, 9, 3, 7}
	got, err := complexityDistributionOf(scores)
	if err != nil {
		t.Fatalf("complexityDistributionOf: %v", err)
	}
	if got.Min != 1 || got.Max != 9 || got.Median != 5 {
		t.Fatalf("distribution = %#v, want min=1 max=9 median=5", got)
	}
}

// TestDefinitionReachCountsDistinguishesReachesByParentPath mirrors the real
// shared-definition-reaches fixture used elsewhere in this package: the
// identity of one "reach" into a definition is (file, the referencing
// property's own path), not the shared pointer all its properties carry.
func TestDefinitionReachCountsDistinguishesReachesByParentPath(t *testing.T) {
	docs := []SchemaDocument{
		{
			File: "a.json",
			Properties: []SchemaProperty{
				{Pointer: "#/definitions/CustomDataType/properties/vendorId", Path: "customData.vendorId", EnclosingDefinition: "CustomDataType"},
				{Pointer: "#/definitions/CustomDataType/properties/vendorId", Path: "station.customData.vendorId", EnclosingDefinition: "CustomDataType"},
				{Pointer: "#/definitions/CustomDataType/properties/vendorId", Path: "station.modem.customData.vendorId", EnclosingDefinition: "CustomDataType"},
			},
		},
	}
	counts := definitionReachCounts(docs)
	if counts["CustomDataType"] != 3 {
		t.Fatalf("CustomDataType reach count = %d, want 3 (one per distinct parent path)", counts["CustomDataType"])
	}
}
