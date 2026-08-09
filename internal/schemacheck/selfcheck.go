package main

import (
	"fmt"
	"math"
	"path/filepath"
	"sort"
)

// dedupKey is the corpus-wide identity the dedup pass below uses: a named
// definition (the "definitions" entry a property was reached through), or,
// for a property declared directly at a schema file's own root, the file
// itself standing in for that root's own (anonymous, but file-unique)
// scope, paired with the property's own JSON name. Two properties sharing a
// dedupKey are, by construction, the same declaration inlined once per file
// that uses it — the fact the corpus-wide dedup pass exists to collapse.
type dedupKey struct {
	scope string
	name  string
}

func scopeFor(doc SchemaDocument, p SchemaProperty) string {
	if p.EnclosingDefinition != "" {
		return p.EnclosingDefinition
	}
	return doc.File
}

// dedupedProperty is the single representative SchemaProperty a dedupKey
// collapses to, plus the set of files it was actually observed in — needed
// both to report occurrence counts and to detect a structural conflict (the
// same key observed with two different shapes across files).
type dedupedProperty struct {
	key       dedupKey
	sample    SchemaProperty
	files     map[string]bool
	conflict  bool
	conflicts []string
}

// dedupCorpus performs the corpus-wide dedup this package's self-check and
// shared-type reporting both rely on, by (scope, property name), across
// every document in docs. It also performs structural-conflict detection: a
// second occurrence of the same key whose type/required/format/enum/
// constraints disagree with the first is flagged, and the conflicting file
// pair is recorded.
func dedupCorpus(docs []SchemaDocument) map[dedupKey]*dedupedProperty {
	out := map[dedupKey]*dedupedProperty{}
	for _, doc := range docs {
		for _, p := range doc.Properties {
			k := dedupKey{scope: scopeFor(doc, p), name: p.Name}
			existing, ok := out[k]
			if !ok {
				out[k] = &dedupedProperty{key: k, sample: p, files: map[string]bool{doc.File: true}}
				continue
			}
			existing.files[doc.File] = true
			if !sameShape(existing.sample, p) {
				existing.conflict = true
				existing.conflicts = append(existing.conflicts, doc.File)
			}
		}
	}
	return out
}

// sameShape reports whether two occurrences of what dedupCorpus considers
// the same declaration actually agree in every fact that matters to this
// comparison: type, required-ness, format, enum values and constraints. It
// deliberately ignores Pointer/Path (occurrence-specific, never expected to
// agree) and EnclosingDefinition (already folded into the dedup key itself).
func sameShape(a, b SchemaProperty) bool {
	if a.Type != b.Type || a.Required != b.Required {
		return false
	}
	if !equalStringPtr(a.Format, b.Format) {
		return false
	}
	if !equalStringSlice(a.Enum, b.Enum) {
		return false
	}
	return equalConstraints(a.Constraints, b.Constraints)
}

// equalConstraints deep-compares two Constraints values field by field,
// dereferencing every pointer: Constraints{} == Constraints{} in Go compares
// pointer *addresses* for its many *int/*float64/*bool/*string fields, which
// are freshly allocated on every schema walk and therefore never equal
// across two occurrences even when they hold the identical value — a
// structural-conflict false positive this comparison exists to avoid.
func equalConstraints(a, b Constraints) bool {
	return equalIntPtr(a.MaxLength, b.MaxLength) &&
		equalIntPtr(a.MinLength, b.MinLength) &&
		equalFloatPtr(a.Minimum, b.Minimum) &&
		equalFloatPtr(a.Maximum, b.Maximum) &&
		equalExclusiveBound(a.ExclusiveMinimum, b.ExclusiveMinimum) &&
		equalExclusiveBound(a.ExclusiveMaximum, b.ExclusiveMaximum) &&
		equalIntPtr(a.MinItems, b.MinItems) &&
		equalIntPtr(a.MaxItems, b.MaxItems) &&
		equalStringPtr(a.Pattern, b.Pattern) &&
		equalBoolPtr(a.AdditionalProperties, b.AdditionalProperties)
}

func equalExclusiveBound(a, b *ExclusiveBound) bool {
	if a == nil || b == nil {
		return a == b
	}
	return equalBoolPtr(a.Bool, b.Bool) && equalFloatPtr(a.Number, b.Number)
}

func equalStringPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func equalIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func equalFloatPtr(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func equalBoolPtr(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// selfCheckResult carries the seven M1-M7 report rows plus the diagnostic
// detail (conflicts, untyped property paths, the full M2 composition
// breakdown) a manual adjudication of a mismatch, or the Markdown report's
// method section, would need beyond the single expected/actual pair each
// SelfCheck carries.
type selfCheckResult struct {
	checks              []SelfCheck
	structuralConflicts []string
	untypedPaths        []string
	composition         compositionBreakdown
}

// compositionBreakdown is M2's full eight-way split of the deduped optional
// properties, reported in the Markdown method section alongside the
// single-number M2 self-check row (deduped optional object properties).
type compositionBreakdown struct {
	object, plainString, stringEnum int
	integer, number, boolean        int
	untyped, array                  int
	total                           int
}

// runSelfChecks computes M1-M7 straight from the loaded schema corpus,
// independent of any Go-tree fact.
func runSelfChecks(docs []SchemaDocument) selfCheckResult {
	deduped := dedupCorpus(docs)

	var optionalKeys []dedupKey
	for k, dp := range deduped {
		if !dp.sample.Required {
			optionalKeys = append(optionalKeys, k)
		}
	}
	m1 := len(optionalKeys)

	var (
		objectCount, plainStringCount, stringEnumCount int
		integerCount, numberCount, booleanCount        int
		untypedCount, arrayCount                       int
		customDataObjectCount                          int
		dateTimeStringCount                            int
	)
	for _, k := range optionalKeys {
		dp := deduped[k]
		p := dp.sample
		switch p.Type {
		case "object":
			objectCount++
			if p.Name == "customData" {
				customDataObjectCount++
			}
		case "string":
			isDateTime := p.Format != nil && *p.Format == "date-time"
			if len(p.Enum) > 0 {
				stringEnumCount++
			} else {
				plainStringCount++
			}
			if isDateTime {
				dateTimeStringCount++
			}
		case "integer":
			integerCount++
		case "number":
			numberCount++
		case "boolean":
			booleanCount++
		case "array":
			arrayCount++
		case "":
			untypedCount++
		}
	}

	m2Total := objectCount + plainStringCount + stringEnumCount + integerCount + numberCount + booleanCount + untypedCount + arrayCount

	m3Percent := 0
	if objectCount > 0 {
		m3Percent = int(math.Round(float64(customDataObjectCount) / float64(objectCount) * 100))
	}

	stringPool := plainStringCount + stringEnumCount
	m4 := stringPool - dateTimeStringCount

	// M5: every literal "minimum" keyword occurrence in the corpus, deduped
	// by (file, pointer) - a declaration reused via $ref within one file
	// carries the keyword once in the text, not once per reach.
	type fp struct{ file, pointer string }
	minimumOccurrences := map[fp]float64{}
	for _, doc := range docs {
		for _, p := range doc.Properties {
			if p.Constraints.Minimum != nil {
				minimumOccurrences[fp{doc.File, p.Pointer}] = *p.Constraints.Minimum
			}
		}
	}
	m5Count := len(minimumOccurrences)
	m5AllZero := true
	for _, v := range minimumOccurrences {
		if v > 0 {
			m5AllZero = false
		}
	}

	// M6: properties untyped by design, deduped by (file, pointer).
	untypedSeen := map[fp]bool{}
	var untypedPaths []string
	for _, doc := range docs {
		for _, p := range doc.Properties {
			if p.Type == "" {
				k := fp{doc.File, p.Pointer}
				if !untypedSeen[k] {
					untypedSeen[k] = true
					untypedPaths = append(untypedPaths, filepath.Base(doc.File)+"#"+p.Path)
				}
			}
		}
	}
	sort.Strings(untypedPaths)
	m6 := len(untypedSeen)

	// M7: corpus uniformity - no oneOf/anyOf/allOf, no exclusiveMinimum/Maximum
	// anywhere, and zero structural conflicts in the dedup pass above. A
	// multi-level $ref chain would already have hard-failed schema loading
	// (resolveLocalRef, schema_walk.go) before self-checks ever run, so it
	// contributes nothing further to count here.
	unionKeywordCount := 0
	for _, doc := range docs {
		raw, err := loadJSONDocument(doc.File)
		if err == nil {
			unionKeywordCount += countUnionKeywords(raw)
		}
	}
	exclusiveCount := 0
	for _, doc := range docs {
		for _, p := range doc.Properties {
			if p.Constraints.ExclusiveMinimum != nil || p.Constraints.ExclusiveMaximum != nil {
				exclusiveCount++
			}
		}
	}
	var conflicts []string
	for k, dp := range deduped {
		if dp.conflict {
			conflicts = append(conflicts, fmt.Sprintf("%s/%s: conflicting shapes across %v", k.scope, k.name, append([]string{}, dp.conflicts...)))
		}
	}
	sort.Strings(conflicts)
	m7 := unionKeywordCount + exclusiveCount + len(conflicts)

	checks := []SelfCheck{
		{ID: "M1", Claim: "deduped optional properties in the corpus", Expected: 408, Actual: m1, Status: status(408, m1)},
		{ID: "M2", Claim: "deduped optional object properties (the largest slice of M1's composition)", Expected: 252, Actual: objectCount, Status: status(252, objectCount)},
		{ID: "M3", Claim: "percent of deduped optional object properties that are customData", Expected: 72, Actual: m3Percent, Status: status(72, m3Percent)},
		{ID: "M4", Claim: "deduped string/enum fields narrowed from pointer to value+omitempty", Expected: 58, Actual: m4, Status: status(58, m4)},
		{ID: "M5", Claim: "literal minimum occurrences in the corpus, none greater than zero", Expected: 8, Actual: minimumCheckActual(m5Count, m5AllZero), Status: minimumStatus(m5Count, m5AllZero)},
		{ID: "M6", Claim: "properties untyped by design", Expected: 2, Actual: m6, Status: status(2, m6)},
		{ID: "M7", Claim: "corpus-uniformity violations (union keywords, exclusive bounds, structural conflicts)", Expected: 0, Actual: m7, Status: status(0, m7)},
	}

	composition := compositionBreakdown{
		object: objectCount, plainString: plainStringCount, stringEnum: stringEnumCount,
		integer: integerCount, number: numberCount, boolean: booleanCount,
		untyped: untypedCount, array: arrayCount, total: m2Total,
	}

	return selfCheckResult{checks: checks, structuralConflicts: conflicts, untypedPaths: untypedPaths, composition: composition}
}

func status(expected, actual int) string {
	if expected == actual {
		return "pass"
	}
	return "fail"
}

// minimumCheckActual folds M5's two-part claim (a count, and a
// none-greater-than-zero assertion) into the single actual/expected pair
// SelfCheck can carry: the count itself, unless some minimum was found
// greater than zero, in which case a value that can never equal the
// expected 8 (forcing a visible "fail") is reported instead.
func minimumCheckActual(count int, allZero bool) int {
	if !allZero {
		return -1
	}
	return count
}

func minimumStatus(count int, allZero bool) string {
	if !allZero {
		return "fail"
	}
	return status(8, count)
}

// countUnionKeywords recursively counts every occurrence of oneOf, anyOf or
// allOf anywhere in a parsed JSON document — JSON Schema's union-type
// vocabulary, which M7 claims is absent from this corpus.
func countUnionKeywords(node any) int {
	count := 0
	switch v := node.(type) {
	case map[string]any:
		for _, keyword := range []string{"oneOf", "anyOf", "allOf"} {
			if _, ok := v[keyword]; ok {
				count++
			}
		}
		for _, child := range v {
			count += countUnionKeywords(child)
		}
	case []any:
		for _, child := range v {
			count += countUnionKeywords(child)
		}
	}
	return count
}
