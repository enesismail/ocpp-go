package main

import "strings"

// reportField pairs one flattened Go field with the JSON-name-dotted path
// used to pair it against a schema property — a fact distinct from
// GoField.Path (walker-internal, built from Go identifiers): "iccid" tags a
// field named Iccid, so the two paths agree in spelling only by
// coincidence of casing convention, never by construction.
type reportField struct {
	field GoField
	path  string
}

// expandFields walks fields — one struct's own already-flattened field
// list, exactly as walkGoFile produced it for originFile — and returns the
// report-ready rows: every field the walker already flattened (each
// carrying its own JSON-name-dotted path), plus, spliced in with a
// re-rooted path, the fields of any reference walkGoFile itself never
// recurses into:
//
//   - a composite (non-slice) field whose type is declared in a *different*
//     file from originFile — same-package or cross-package. walkFieldType
//     only recurses into a same-file named struct; a field reached through a
//     package-qualified selector, or a bare name declared in a sibling file
//     of the same package, is left as an opaque terminal row (see
//     ast_walk.go's walkFieldType commentary) precisely because resolving it
//     needs another file's AST, which a single walkGoFile call does not
//     have.
//   - an array field's element type, whatever file it is declared in:
//     walkGoFile never recurses into an array's element type at all, so this
//     case applies even when the element is declared in originFile itself.
//
// Splicing follows the same convention the schema walker uses for array
// items (schema_walk.go's walkArrayItems): no index marker is added to the
// path, so "additionalInfo" (the array field) and "additionalInfo.type"
// (a property of its element type) sit side by side, matching how the
// schema side paths a $ref'd array item's own properties.
//
// visiting is the set of qualified type identities already on the current
// stitching chain, so a cross-file reference cycle terminates the same way
// walkGoFile's own same-file cycle guard does: the referencing field's own
// row is kept, with no further expansion.
func expandFields(idx *treeIndex, fields []GoField, originFile, pathPrefix string, visiting map[string]bool) ([]reportField, error) {
	type frame struct{ goPath, jsonPath string }
	var stack []frame
	var out []reportField

	for _, f := range fields {
		parentGo := parentGoPath(f.Path)
		for len(stack) > 0 && stack[len(stack)-1].goPath != parentGo {
			stack = stack[:len(stack)-1]
		}
		seg := f.JSONName
		if seg == "" {
			seg = f.Name
		}
		jsonPath := seg
		switch {
		case len(stack) > 0:
			jsonPath = stack[len(stack)-1].jsonPath + "." + seg
		case pathPrefix != "":
			jsonPath = pathPrefix + "." + seg
		}
		stack = append(stack, frame{goPath: f.Path, jsonPath: jsonPath})

		typeText := f.ElementType
		if !f.Slice {
			typeText = strings.TrimPrefix(f.DeclaredType, "*")
		}
		identity, resolvable := resolveTypeIdentity(idx.resolver, originFile, typeText)

		annotated := f
		if resolvable {
			if fn, ok := idx.validatorFns[identity]; ok {
				annotated.StructValidators = []string{fn}
			}
		}
		out = append(out, reportField{field: annotated, path: jsonPath})

		if !resolvable {
			continue
		}
		if !f.Slice && f.WireType != typeText {
			// Already resolved by the wire-type or scalar-alias registry
			// inside walkGoFile (e.g. a *types.DateTime or a named enum
			// alias) — not an opaque struct reference, nothing to expand.
			continue
		}
		if _, err := resolveWireType(identity, idx.resolution.Registry); err == nil {
			// An array element with a registered custom marshaler (e.g. a
			// slice of DateTime): a scalar on the wire, so there is nothing
			// further to splice in, matching the schema side's own
			// non-object array-item handling.
			continue
		}
		decl, found := idx.structs[identity]
		if !found {
			// Not a struct this tree declares: a foreign or unrecognized
			// type stays the opaque terminal row it has always been.
			continue
		}
		if !f.Slice && decl.file == originFile {
			// walkGoFile already recursed into this same-file struct; its
			// children are already later entries in fields.
			continue
		}
		if visiting[identity] {
			continue
		}
		idx.usedCrossFileStructs[identity] = true

		targetFields, err := idx.fieldsFor(decl)
		if err != nil {
			return nil, err
		}
		nextVisiting := make(map[string]bool, len(visiting)+1)
		for k := range visiting {
			nextVisiting[k] = true
		}
		nextVisiting[identity] = true

		children, err := expandFields(idx, targetFields, decl.file, jsonPath, nextVisiting)
		if err != nil {
			return nil, err
		}
		out = append(out, children...)
	}
	return out, nil
}

// parentGoPath returns path with its last dotted segment removed ("" for a
// top-level, undotted path) — the walker-internal Go-dotted path of the
// field that would be this one's parent in the struct it was flattened
// from, used to reconstruct that nesting while assigning JSON-name paths.
func parentGoPath(path string) string {
	idx := strings.LastIndex(path, ".")
	if idx < 0 {
		return ""
	}
	return path[:idx]
}

// resolveTypeIdentity computes the resolved, package-qualified identity a
// type-text expression (e.g. "types.IdToken", "ModemType", written exactly
// as walkGoFile records it in a field's DeclaredType/ElementType) would
// resolve to in file, using the same qualify()/selector rules the AST
// walker itself applies to a field's own declared type. ok is false for a
// builtin, a composite type expression with no single identity (slices,
// maps, the empty interface), or any selector/bare name the resolver
// cannot account for.
func resolveTypeIdentity(resolver *TypeResolver, file, typeText string) (identity string, ok bool) {
	if typeText == "" {
		return "", false
	}
	if strings.HasPrefix(typeText, "[]") || strings.HasPrefix(typeText, "map[") ||
		typeText == "interface{}" || typeText == "any" {
		return "", false
	}
	if strings.Contains(typeText, ".") {
		resolved, err := resolver.Resolve(file, typeText)
		if err != nil {
			return "", false
		}
		return resolved, true
	}
	if _, builtin := goBasicTypeNames[typeText]; builtin {
		return "", false
	}
	pkg, err := resolver.PackageOf(file)
	if err != nil {
		return "", false
	}
	return pkg + "." + typeText, true
}
