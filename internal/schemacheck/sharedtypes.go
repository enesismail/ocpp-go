package main

import (
	"sort"
	"strings"
)

// parentJSONPath returns path with its last dotted segment removed ("" for
// a top-level path) — used to identify one "reach" into a shared
// definition: every property a single $ref resolves to shares the same
// parent path (the referencing property's own path).
func parentJSONPath(path string) string {
	idx := strings.LastIndex(path, ".")
	if idx < 0 {
		return ""
	}
	return path[:idx]
}

// definitionReachCounts computes, for every named schema definition, how
// many distinct places across the corpus reach it via $ref: the identity of
// one reach is (schema file, the referencing property's own path), so three
// references to the same definition within one file — the real
// CustomDataType shape — count as three, and the same definition reached
// from two different schema files counts as two more.
func definitionReachCounts(docs []SchemaDocument) map[string]int {
	type reach struct{ file, parent string }
	seen := map[string]map[reach]bool{}
	for _, doc := range docs {
		for _, p := range doc.Properties {
			if p.EnclosingDefinition == "" {
				continue
			}
			if seen[p.EnclosingDefinition] == nil {
				seen[p.EnclosingDefinition] = map[reach]bool{}
			}
			seen[p.EnclosingDefinition][reach{doc.File, parentJSONPath(p.Path)}] = true
		}
	}
	counts := make(map[string]int, len(seen))
	for def, set := range seen {
		counts[def] = len(set)
	}
	return counts
}

// buildSharedTypes pairs every named schema definition genuinely modelled by
// a Go type in the tree's shared types package to that Go type, reporting
// how many places reference it and any structural conflict the corpus-wide
// dedup pass found for it. A definition is included only when idx confirms
// some message field actually stitched in a struct under that name — a
// definition the schema corpus reaches but the Go tree never models (the
// real case: CustomDataType, declared as ocpp2.0.1/types.CustomData but not
// yet used by any field) is not a "shared type" in the sense this report
// means, so it is left out rather than reported as one.
func buildSharedTypes(reachCounts map[string]int, conflicts []string, typesPackage string, idx *treeIndex) []SharedType {
	conflictsByScope := map[string][]string{}
	for _, c := range conflicts {
		scope, _, ok := strings.Cut(c, "/")
		if !ok {
			continue
		}
		conflictsByScope[scope] = append(conflictsByScope[scope], c)
	}

	var out []SharedType
	for def, occurrences := range reachCounts {
		candidates := []string{strings.TrimSuffix(def, "Type"), def}
		var goName string
		for _, candidate := range candidates {
			identity := typesPackage + "." + candidate
			if _, declared := idx.structs[identity]; declared && idx.usedCrossFileStructs[identity] {
				goName = candidate
				break
			}
		}
		if goName == "" {
			continue
		}
		out = append(out, SharedType{
			GoType:              "types." + goName,
			SchemaDefinition:    def,
			Occurrences:         occurrences,
			StructuralConflicts: append([]string(nil), conflictsByScope[def]...),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GoType < out[j].GoType })
	return out
}
