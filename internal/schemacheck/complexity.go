package main

import (
	"fmt"
	"sort"
)

// messageComplexity computes one message's complexity score: the count of
// distinct transitively-referenced definitions
// (via $ref) plus the count of properties plus the count of enum-bearing
// properties, each deduped by the pair (schema file, JSON pointer) — never
// the pointer alone, since reqDoc and respDoc are separate documents and the
// same pointer text in each does not denote the same occurrence.
func messageComplexity(reqDoc, respDoc SchemaDocument) int {
	type key struct{ file, pointer string }
	definitions := map[key]bool{}
	properties := map[key]bool{}
	enumProps := map[key]bool{}

	for _, doc := range []SchemaDocument{reqDoc, respDoc} {
		for _, p := range doc.Properties {
			k := key{doc.File, p.Pointer}
			properties[k] = true
			if len(p.Enum) > 0 {
				enumProps[k] = true
			}
			if p.EnclosingDefinition != "" {
				definitions[key{doc.File, "#/definitions/" + p.EnclosingDefinition}] = true
			}
		}
	}
	return len(definitions) + len(properties) + len(enumProps)
}

// complexityDistributionOf computes the aggregate distribution (min, p25,
// median, p75, max) over scores using nearest-rank percentiles.
func complexityDistributionOf(scores []int) (ComplexityDistribution, error) {
	if len(scores) == 0 {
		return ComplexityDistribution{}, fmt.Errorf("compute complexity distribution: no messages")
	}
	sorted := append([]int(nil), scores...)
	sort.Ints(sorted)

	p25, err := nearestRankPercentile(scores, 25)
	if err != nil {
		return ComplexityDistribution{}, err
	}
	median, err := nearestRankPercentile(scores, 50)
	if err != nil {
		return ComplexityDistribution{}, err
	}
	p75, err := nearestRankPercentile(scores, 75)
	if err != nil {
		return ComplexityDistribution{}, err
	}
	return ComplexityDistribution{
		Min:    sorted[0],
		P25:    p25,
		Median: median,
		P75:    p75,
		Max:    sorted[len(sorted)-1],
	}, nil
}
