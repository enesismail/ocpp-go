package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strconv"
	"strings"
)

// parseFileSet parses path and returns both its AST and the FileSet used to
// parse it — every other function in this file that needs to report a line
// number re-derives it from this same FileSet rather than re-parsing.
func parseFileSet(path string) (*token.FileSet, *ast.File, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, nil, fmt.Errorf("parse Go file %s: %w", path, err)
	}
	return fset, file, nil
}

// parseASTFile parses path and returns its AST alone, for callers that have
// no need of line-number reporting.
func parseASTFile(path string) (*ast.File, error) {
	_, file, err := parseFileSet(path)
	return file, err
}

// parseASTFileWithPositions is parseASTFile for the callers that need to
// report where a declaration sits, not only what it declares.
func parseASTFileWithPositions(path string) (*token.FileSet, *ast.File, error) {
	return parseFileSet(path)
}

// structDeclLines reports the source line each top-level "type X struct" is
// declared on, keyed by type name. It exists for the one row that has no field
// of its own — a rule registered against a message's whole struct — so that row
// can cite a declaration site like every other row does.
func structDeclLines(fset *token.FileSet, file *ast.File) map[string]int {
	lines := map[string]int{}
	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.TYPE {
			continue
		}
		for _, spec := range genDecl.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			if _, ok := typeSpec.Type.(*ast.StructType); !ok {
				continue
			}
			lines[typeSpec.Name.Name] = fset.Position(typeSpec.Pos()).Line
		}
	}
	return lines
}

// localStructIndex collects every top-level "type X struct {...}"
// declaration in file, in declaration order.
func localStructIndex(file *ast.File) (names []string, decls map[string]*ast.StructType) {
	decls = make(map[string]*ast.StructType)
	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.TYPE {
			continue
		}
		for _, spec := range genDecl.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			structType, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				continue
			}
			names = append(names, typeSpec.Name.Name)
			decls[typeSpec.Name.Name] = structType
		}
	}
	return names, decls
}

// goBasicTypeNames maps every Go predeclared basic type name a named type
// declaration can be defined in terms of onto the spelling the rest of this
// package reasons about. The two aliases Go itself defines — byte for uint8
// and rune for int32 — are folded onto their canonical names here, so a
// field declared through them is not mistaken for an unrecognized named
// type further down. Non-basic underlying types (structs, slices, maps,
// interfaces, pointers, channels, funcs) are deliberately absent: only a
// scalar alias has a single scalar wire kind to inherit.
var goBasicTypeNames = map[string]string{
	"string":  "string",
	"bool":    "bool",
	"int":     "int",
	"int8":    "int8",
	"int16":   "int16",
	"int32":   "int32",
	"int64":   "int64",
	"uint":    "uint",
	"uint8":   "uint8",
	"uint16":  "uint16",
	"uint32":  "uint32",
	"uint64":  "uint64",
	"uintptr": "uintptr",
	"float32": "float32",
	"float64": "float64",
	"byte":    "uint8",
	"rune":    "int32",
}

// localScalarAliasIndex collects every top-level "type X <scalar>"
// declaration in file — the idiom the message tree models protocol enums
// with (type BootReason string, and a const block of BootReason values) —
// mapping each such name to the Go basic type it ultimately resolves to.
//
// A declaration written in terms of another same-file named type
// (type Chained Reason) is followed through, so only the scalar at the end
// of the chain is recorded. A name whose declaration is anything but a bare
// identifier — a struct, slice, map, interface, pointer, or a selector
// naming a type from another package — is absent from the result: it has no
// single scalar kind to inherit, and the walker keeps treating it the way it
// always has (a same-file struct is recursed into; anything else stays an
// opaque terminal reference).
func localScalarAliasIndex(file *ast.File) map[string]string {
	direct := map[string]string{}
	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.TYPE {
			continue
		}
		for _, spec := range genDecl.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			ident, ok := typeSpec.Type.(*ast.Ident)
			if !ok {
				continue
			}
			direct[typeSpec.Name.Name] = ident.Name
		}
	}

	resolved := make(map[string]string, len(direct))
	for name := range direct {
		if basic, ok := resolveScalarAliasChain(name, direct); ok {
			resolved[name] = basic
		}
	}
	return resolved
}

// resolveScalarAliasChain follows name through direct (the file's own
// name -> named-underlying-type edges) until it reaches a Go predeclared
// basic type, returning that type's canonical name. The hop count is bounded
// by the number of declarations, so a declaration cycle — which the Go
// compiler would reject anyway, but which this tool must not hang on —
// terminates with ok=false rather than looping.
func resolveScalarAliasChain(name string, direct map[string]string) (string, bool) {
	current := name
	for hops := 0; hops <= len(direct); hops++ {
		next, ok := direct[current]
		if !ok {
			return "", false
		}
		if basic, ok := goBasicTypeNames[next]; ok {
			return basic, true
		}
		current = next
	}
	return "", false
}

// goFileWalker carries per-file context shared by every field-flattening
// call: the file's own path (for error messages and resolver lookups), the
// FileSet backing its AST (for line numbers), the file's own top-level
// struct declarations (for same-file recursion), the file's own scalar
// alias declarations (so a named enum type carries its underlying scalar as
// its wire type), the validator-tag -> accepted-value-set registry (so a
// field carrying a registered tag carries that tag's value set) and the
// type-resolution context (for package-qualified selectors).
type goFileWalker struct {
	path    string
	fset    *token.FileSet
	structs map[string]*ast.StructType
	scalars map[string]string
	enums   map[string][]string
	res     *Resolution
}

// walkGoFile walks path's declared structs and their fields, flattening
// embedded fields the way encoding/json does and recursing into named
// struct-typed fields with cycle detection. See stubs.go's original doc
// comment on this function for the wire-type resolution contract package-
// qualified fields follow.
func walkGoFile(path string, res *Resolution) ([]StructInfo, error) {
	fset, file, err := parseFileSet(path)
	if err != nil {
		return nil, err
	}
	names, decls := localStructIndex(file)
	w := &goFileWalker{
		path:    path,
		fset:    fset,
		structs: decls,
		scalars: localScalarAliasIndex(file),
		// A validator tag registered in this same file is the definitive
		// account of what that tag accepts here, so it takes precedence over
		// anything an orchestrator pre-registered for the tree as a whole;
		// the pre-registered set is what supplies tags registered in some
		// other file (the shared types package registers many of them).
		enums: mergeEnumSets(res.enumRegistry(), extractEnumsFromFile(file)),
		res:   res,
	}

	structs := make([]StructInfo, 0, len(names))
	for _, name := range names {
		fields, err := w.flattenStructFields(decls[name], map[string]bool{name: true})
		if err != nil {
			return nil, err
		}
		structs = append(structs, StructInfo{Name: name, Fields: fields})
	}
	return structs, nil
}

// flattenStructFields walks a struct's own top-level field list, in
// declaration order, with no path prefix — used both for a struct's own
// top-level StructInfo.Fields and, with a prefix, when recursing into a
// named nested field (flattenNestedFields). It is a shadowing-scope root:
// see flattenFields.
func (w *goFileWalker) flattenStructFields(structType *ast.StructType, visiting map[string]bool) ([]GoField, error) {
	return w.flattenFields(structType, "", visiting)
}

// flattenNestedFields is flattenStructFields for a struct reached by
// recursing into a named field, threading that field's own dotted path
// prefix through every field it produces. It is its own shadowing-scope
// root (see flattenFields) because a named (non-embedded) struct field
// marshals to a nested JSON object — a namespace of its own that never
// competes for JSON names with whatever field reached it.
func (w *goFileWalker) flattenNestedFields(structType *ast.StructType, pathPrefix string, visiting map[string]bool) ([]GoField, error) {
	return w.flattenFields(structType, pathPrefix, visiting)
}

// flattenFields gathers structType's own fields and every field promoted
// into its namespace through anonymous embedding, then resolves any
// same-JSON-name collision among them the way encoding/json itself would:
// the shallower field wins, and a collision between two fields at the same
// depth drops every field carrying that name (resolveShadowing). This is
// the one shadowing-scope boundary in the walk — gatherFields recurses
// through embedding without applying that rule itself, precisely so a name
// shared between, say, this struct's own field and a field promoted from an
// embed two hops down can be judged once, here, with the whole scope in
// view, rather than piecemeal partway through the recursion.
func (w *goFileWalker) flattenFields(structType *ast.StructType, pathPrefix string, visiting map[string]bool) ([]GoField, error) {
	raw, err := w.gatherFields(structType, pathPrefix, visiting, 0)
	if err != nil {
		return nil, err
	}
	return resolveShadowing(raw), nil
}

// fieldAtDepth pairs a field gathered by gatherFields with its embedding
// depth: 0 for a field declared directly in the scope-root struct, 1 for a
// field promoted from a directly (anonymously) embedded struct, 2 for a
// field promoted through two embedding hops, and so on — the fact
// resolveShadowing needs to reproduce encoding/json's own depth-precedence
// rule. children carries a named (non-embedded) struct field's own,
// already-fully-resolved nested fields (dotted-path leaves under this
// field): they travel with their parent bare row rather than joining the
// shadow-candidate pool themselves, since they live in a nested JSON
// object's own namespace and must never be judged against the flat scope's
// names. children is nil for every field that is not itself a named nested
// struct.
type fieldAtDepth struct {
	field    GoField
	depth    int
	children []GoField
}

// gatherFields walks structType's own field list the way flattenFields
// always has, except it does not resolve JSON-name shadowing itself: an
// anonymous (embedded) field's own fields are gathered one depth deeper and
// returned unresolved, so that a name they share with a shallower field
// elsewhere in this same scope can be resolved once, by the caller that
// owns the whole scope (flattenFields), rather than being decided
// prematurely down here with only a partial view of the scope.
func (w *goFileWalker) gatherFields(structType *ast.StructType, pathPrefix string, visiting map[string]bool, depth int) ([]fieldAtDepth, error) {
	if structType.Fields == nil {
		return nil, nil
	}
	var out []fieldAtDepth
	for _, field := range structType.Fields.List {
		tag := fieldTag(field)
		if tag.skip {
			// A json:"-" field never crosses the wire, so encoding/json
			// leaves it out of the payload entirely — and, for an anonymous
			// embedded field, leaves out everything it would otherwise have
			// promoted. A field the wire never carries has no schema
			// property to pair against and must not be reported as one the
			// schema is missing, so it is dropped here rather than emitted
			// under the literal name "-".
			continue
		}
		if len(field.Names) == 0 {
			// An embedded field carrying a JSON name is not flattened at
			// all: encoding/json treats it as an ordinary named field and
			// marshals it as a nested object under that name. Only an
			// untagged embed promotes its own fields.
			if tag.name != "" {
				typeName := embeddedTypeName(field.Type)
				childPath := typeName
				if pathPrefix != "" {
					childPath = pathPrefix + "." + typeName
				}
				named, err := w.walkFieldType(field.Type, typeName, childPath, tag, visiting, false, depth)
				if err != nil {
					return nil, err
				}
				out = append(out, named...)
				continue
			}
			// Anonymous/embedded field: its own fields are promoted
			// straight into the parent's field list, with no name prefix,
			// matching encoding/json's flattening — one embedding hop
			// deeper than the fields declared directly in this struct.
			promoted, err := w.walkFieldType(field.Type, "", pathPrefix, tag, visiting, true, depth)
			if err != nil {
				return nil, err
			}
			out = append(out, promoted...)
			continue
		}
		for _, ident := range field.Names {
			childPath := ident.Name
			if pathPrefix != "" {
				childPath = pathPrefix + "." + ident.Name
			}
			produced, err := w.walkFieldType(field.Type, ident.Name, childPath, tag, visiting, false, depth)
			if err != nil {
				return nil, err
			}
			out = append(out, produced...)
		}
	}
	return out, nil
}

// resolveShadowing applies encoding/json's own depth-precedence rule to one
// flattening scope's raw, unresolved field list: for every JSON name that
// occurs more than once, only the field at that name's shallowest depth
// survives; if more than one field shares that shallowest depth, the name
// is ambiguous and every field carrying it is dropped — exactly what
// encoding/json does with an ambiguous embedded selector, rather than
// guessing which one the caller meant. A surviving field's own children
// (see fieldAtDepth) are carried along with it; a dropped field's children
// are dropped too, since a struct field absent from JSON output cannot
// contribute a nested object either. Fields that survive keep their
// original relative (declaration) order.
func resolveShadowing(raw []fieldAtDepth) []GoField {
	minDepth := make(map[string]int, len(raw))
	for _, item := range raw {
		key := shadowKey(item.field)
		if d, ok := minDepth[key]; !ok || item.depth < d {
			minDepth[key] = item.depth
		}
	}
	count := make(map[string]int, len(raw))
	tagged := make(map[string]int, len(raw))
	for _, item := range raw {
		key := shadowKey(item.field)
		if item.depth != minDepth[key] {
			continue
		}
		count[key]++
		if item.field.JSONName != "" {
			tagged[key]++
		}
	}
	out := make([]GoField, 0, len(raw))
	for _, item := range raw {
		key := shadowKey(item.field)
		if item.depth != minDepth[key] || !survivesShadowing(item.field, count[key], tagged[key]) {
			continue
		}
		out = append(out, item.field)
		out = append(out, item.children...)
	}
	return out
}

// survivesShadowing decides, for one field at its name's shallowest depth,
// whether it is the one encoding/json would marshal. atDepth and taggedAtDepth
// are how many fields share that name at that depth, and how many of those
// carry an explicit JSON tag name.
//
// A tag is a statement of intent, so exactly one tagged field at the shallowest
// depth wins outright however many untagged ones sit beside it. Failing that,
// a lone field wins by being alone. Anything else is a name encoding/json
// considers ambiguous — several tagged claimants, or several untagged ones —
// and it drops every field carrying it rather than picking one.
func survivesShadowing(field GoField, atDepth, taggedAtDepth int) bool {
	if taggedAtDepth == 1 {
		return field.JSONName != ""
	}
	return taggedAtDepth == 0 && atDepth == 1
}

// embeddedTypeName is the Go field name an embedded field is known by: the
// embedded type's own name, with any pointer indirection and package
// qualification stripped, exactly as Go itself derives it.
func embeddedTypeName(expr ast.Expr) string {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return t.Sel.Name
	default:
		return exprString(expr)
	}
}

// shadowKey is the identity two fields are considered to collide under for
// depth-precedence purposes: the JSON tag name they marshal under, falling
// back to the Go field name itself for an untagged field — encoding/json's
// own default.
func shadowKey(field GoField) string {
	if field.JSONName != "" {
		return field.JSONName
	}
	return field.Name
}

// walkFieldType classifies one field's type expression and returns the
// depth-tagged GoField row(s) it produces: a single leaf for a builtin or
// registered wire type, a single "bare" row (with its recursively
// flattened, already-resolved children attached) for a same-file named
// struct type, or — for an anonymous embedded field — just the promoted
// children, one depth deeper, with no bare row of their own.
//
// declaredType always keeps the literal, pointer-qualified type exactly as
// written; wireType is that same field after wire-type resolution, which
// for anything but a registered custom marshaler means the pointer
// indirection stripped away — a plain "*string" field with no marshaler
// carries wireType "string", since that pointer never changes what
// actually crosses the wire.
// walkFieldType records where each row it produces was declared before
// returning it — the file and line of the field's own type expression. That is
// evidence the comparison itself cannot reconstruct afterwards: a row reached
// through a shared composite is declared in the package that declares the
// composite, not in the message file the row is reported under, so a reader
// told only the message file would be sent to the wrong place. Rows produced
// by a deeper walk already carry their own, more precise, position, and are
// left alone.
func (w *goFileWalker) walkFieldType(expr ast.Expr, name, path string, tag fieldTagInfo, visiting map[string]bool, anonymous bool, depth int) ([]fieldAtDepth, error) {
	produced, err := w.walkFieldTypeAt(expr, name, path, tag, visiting, anonymous, depth)
	if err != nil {
		return nil, err
	}
	position := w.fset.Position(expr.Pos())
	for i := range produced {
		if produced[i].field.File == "" {
			produced[i].field.File = position.Filename
			produced[i].field.Line = position.Line
		}
	}
	return produced, nil
}

func (w *goFileWalker) walkFieldTypeAt(expr ast.Expr, name, path string, tag fieldTagInfo, visiting map[string]bool, anonymous bool, depth int) ([]fieldAtDepth, error) {
	pointer := false
	inner := expr
	if star, ok := inner.(*ast.StarExpr); ok {
		pointer = true
		inner = star.X
	}
	declared := exprString(expr)
	enumValues := w.enumValuesFor(tag)

	switch t := inner.(type) {
	case *ast.ArrayType:
		field := GoField{
			Name: name, Path: path, DeclaredType: declared, WireType: exprString(t),
			Pointer: pointer, Slice: true, ElementType: exprString(t.Elt),
			JSONName: tag.name, Omitempty: tag.omitempty, Validate: tag.validate,
			EnumValues: enumValues,
		}
		return []fieldAtDepth{{field: field, depth: depth}}, nil

	case *ast.Ident:
		// A named type, or a builtin. A bare name refers to this file's own
		// package, so if that package's identity is known the registries can
		// be consulted for it under the same qualified key a selector from
		// another package would produce — the sibling-file case, which a
		// single file's AST cannot answer on its own (the real shapes: a
		// DateTime declared in datetime.go and used in types.go, and enum
		// aliases declared in one message file and used in another).
		if qualified, err := w.qualify(t.Name); err == nil {
			if wireType, err := resolveWireType(qualified, w.res.Registry); err == nil {
				// A registered custom marshaler is never walked as a struct:
				// what it encodes to is what crosses the wire, whatever its
				// declaration looks like.
				field := GoField{
					Name: name, Path: path, DeclaredType: declared, WireType: wireType.Type,
					Pointer: pointer, JSONName: tag.name, Omitempty: tag.omitempty, Validate: tag.validate,
					EnumValues: enumValues, CustomMarshaled: true,
				}
				return []fieldAtDepth{{field: field, depth: depth}}, nil
			}
			if basic, ok := w.res.scalarAliasFor(qualified); ok {
				field := GoField{
					Name: name, Path: path, DeclaredType: declared, WireType: basic,
					Pointer: pointer, JSONName: tag.name, Omitempty: tag.omitempty, Validate: tag.validate,
					EnumValues: enumValues,
				}
				return []fieldAtDepth{{field: field, depth: depth}}, nil
			}
		}
		if structType, ok := w.structs[t.Name]; ok {
			return w.walkNamedStruct(t.Name, structType, name, path, pointer, tag, visiting, depth)
		}
		// A same-file named type declared as a scalar alias — the idiom the
		// message tree writes protocol enums in (type BootReason string) —
		// puts its underlying scalar on the wire, not an object. declaredType
		// keeps the named type exactly as written; only wireType folds down
		// to the scalar, which is what schema-conformance comparison reads.
		// A named type neither this file nor any registry accounts for stays
		// opaque rather than being guessed at.
		wireType := t.Name
		if basic, ok := w.scalars[t.Name]; ok {
			wireType = basic
		}
		field := GoField{
			Name: name, Path: path, DeclaredType: declared, WireType: wireType,
			Pointer: pointer, JSONName: tag.name, Omitempty: tag.omitempty, Validate: tag.validate,
			EnumValues: enumValues,
		}
		return []fieldAtDepth{{field: field, depth: depth}}, nil

	case *ast.SelectorExpr:
		pkgIdent, ok := t.X.(*ast.Ident)
		if !ok {
			return nil, fmt.Errorf("%s: field %q has an unsupported qualified type expression", w.posString(expr.Pos()), fieldLabel(name, anonymous))
		}
		selector := pkgIdent.Name + "." + t.Sel.Name
		qualified, err := w.res.Resolver.Resolve(w.path, selector)
		if err != nil {
			return nil, fmt.Errorf("%s: field %q uses unresolved selector %q: %w", w.posString(expr.Pos()), fieldLabel(name, anonymous), selector, err)
		}
		if wireType, err := resolveWireType(qualified, w.res.Registry); err == nil {
			field := GoField{
				Name: name, Path: path, DeclaredType: declared, WireType: wireType.Type,
				Pointer: pointer, JSONName: tag.name, Omitempty: tag.omitempty, Validate: tag.validate,
				EnumValues: enumValues, CustomMarshaled: true,
			}
			return []fieldAtDepth{{field: field, depth: depth}}, nil
		}
		// A named type from another package that is declared as a scalar —
		// the tree's enum idiom again, only across a package boundary — puts
		// that scalar on the wire. The registry is how a walk of one file
		// learns what another package declares; declaredType still keeps the
		// literal, package-qualified type exactly as written.
		if basic, ok := w.res.scalarAliasFor(qualified); ok {
			field := GoField{
				Name: name, Path: path, DeclaredType: declared, WireType: basic,
				Pointer: pointer, JSONName: tag.name, Omitempty: tag.omitempty, Validate: tag.validate,
				EnumValues: enumValues,
			}
			return []fieldAtDepth{{field: field, depth: depth}}, nil
		}
		// Neither a registered wire type nor a registered scalar alias: treat
		// it as an opaque terminal reference. walkGoFile only ever has one
		// file's AST loaded, so it cannot recurse into a struct declared in
		// another package — an
		// orchestrator walking multiple files is responsible for stitching
		// a further walk of that file in, if it needs to. Recorded as a
		// terminal field rather than guessed at or dropped.
		//
		// For an anonymous (embedded) field this is a real limitation, not
		// just a stopping point: encoding/json would promote this struct's
		// own fields straight into the parent's JSON namespace, exactly
		// like the same-file case above, but doing that here would need a
		// second file's AST this function does not have. Rather than guess
		// at what those fields are — or silently drop the field, which
		// would leave it out of the report entirely — it is recorded as its
		// own terminal row, and WireType carries the fully resolved,
		// package-qualified type identity (qualified) rather than the
		// literal declared text, since that identity is the one thing a
		// later cross-package promotion pass would need to find and stitch
		// this field's real, promoted children in.
		wireType := strings.TrimPrefix(declared, "*")
		if anonymous {
			wireType = qualified
		}
		field := GoField{
			Name: name, Path: path, DeclaredType: declared, WireType: wireType,
			Pointer: pointer, JSONName: tag.name, Omitempty: tag.omitempty, Validate: tag.validate,
			EnumValues: enumValues,
		}
		return []fieldAtDepth{{field: field, depth: depth}}, nil

	default:
		field := GoField{
			Name: name, Path: path, DeclaredType: declared, WireType: strings.TrimPrefix(declared, "*"),
			Pointer: pointer, JSONName: tag.name, Omitempty: tag.omitempty, Validate: tag.validate,
			EnumValues: enumValues,
		}
		return []fieldAtDepth{{field: field, depth: depth}}, nil
	}
}

// walkNamedStruct handles a field whose type is a struct declared in this
// same file: named (anonymous == false) fields get a bare row for the
// field itself, at this depth, with their own recursively flattened and
// already-shadow-resolved children attached (flattenNestedFields starts a
// fresh scope for them — see flattenFields); a self-referential type
// already on the current recursion path is kept as that bare terminal row
// with no further recursion, so the walk always terminates. An anonymous
// (embedded) field never gets a bare row — the encoding/json flattening
// rule — only its own children, gathered one depth deeper and left
// unresolved for the enclosing scope's own flattenFields call to judge
// alongside everything else in that scope.
func (w *goFileWalker) walkNamedStruct(typeName string, structType *ast.StructType, name, path string, pointer bool, tag fieldTagInfo, visiting map[string]bool, depth int) ([]fieldAtDepth, error) {
	declared := typeName
	if pointer {
		declared = "*" + typeName
	}

	anonymous := name == ""
	var bare *GoField
	if !anonymous {
		bare = &GoField{
			Name: name, Path: path, DeclaredType: declared, WireType: typeName,
			Pointer: pointer, JSONName: tag.name, Omitempty: tag.omitempty, Validate: tag.validate,
			EnumValues: w.enumValuesFor(tag),
		}
	}

	if visiting[typeName] {
		// Cycle: keep the bare row as a terminal field (or, for an
		// anonymous embed of a self-referential type — not exercised by
		// any fixture — produce nothing) rather than recursing again.
		if bare != nil {
			return []fieldAtDepth{{field: *bare, depth: depth}}, nil
		}
		return nil, nil
	}

	nextVisiting := make(map[string]bool, len(visiting)+1)
	for k := range visiting {
		nextVisiting[k] = true
	}
	nextVisiting[typeName] = true

	if anonymous {
		children, err := w.gatherFields(structType, "", nextVisiting, depth+1)
		if err != nil {
			return nil, err
		}
		return children, nil
	}

	children, err := w.flattenNestedFields(structType, path, nextVisiting)
	if err != nil {
		return nil, err
	}
	return []fieldAtDepth{{field: *bare, depth: depth, children: children}}, nil
}

// qualify turns a bare type name — which, being unqualified, names a type in
// this file's own package — into the resolved, package-qualified identity the
// registries are keyed by. It reports an error rather than guessing when the
// file's package identity was never registered, and every caller treats that
// as "the registries have nothing to say here" and falls back to reading the
// file's own declarations. That is deliberate: a walk driven without package
// identities is still a valid walk of one file, it simply cannot see past it,
// and inventing an identity would silently mis-key every lookup.
func (w *goFileWalker) qualify(typeName string) (string, error) {
	importPath, err := w.res.Resolver.PackageOf(w.path)
	if err != nil {
		return "", err
	}
	return importPath + "." + typeName, nil
}

func fieldLabel(name string, anonymous bool) string {
	if anonymous {
		return "(embedded)"
	}
	return name
}

func (w *goFileWalker) posString(pos token.Pos) string {
	position := w.fset.Position(pos)
	return fmt.Sprintf("%s:%d", position.Filename, position.Line)
}

// fieldTagInfo is the parsed shape of a struct field's `json`/`validate`
// tags. skip records encoding/json's suppression marker — a json tag of
// exactly "-" — which is a different fact from a field that simply carries
// no tag, and a different fact again from the tag "-," whose name genuinely
// is the single character "-".
type fieldTagInfo struct {
	name      string
	omitempty bool
	skip      bool
	validate  []string
}

func fieldTag(field *ast.Field) fieldTagInfo {
	if field.Tag == nil {
		return fieldTagInfo{}
	}
	raw, err := strconv.Unquote(field.Tag.Value)
	if err != nil {
		raw = strings.Trim(field.Tag.Value, "`")
	}
	tag := reflect.StructTag(raw)

	var info fieldTagInfo
	if jsonTag, ok := tag.Lookup("json"); ok {
		// encoding/json reads a json tag whose whole value is "-" as "never
		// encode this field". The one exception it makes for itself is the
		// tag "-,", which is how a field whose JSON name really is "-" gets
		// written; that one still encodes, under the literal name "-", so it
		// falls through to the ordinary name/option parsing below.
		if jsonTag == "-" {
			info.skip = true
		} else {
			parts := strings.Split(jsonTag, ",")
			info.name = parts[0]
			for _, opt := range parts[1:] {
				if opt == "omitempty" {
					info.omitempty = true
				}
			}
		}
	}
	if validate, ok := tag.Lookup("validate"); ok && validate != "" {
		info.validate = strings.Split(validate, ",")
	}
	return info
}

// fieldLevelValidateTokens returns the leading run of a validate tag's
// tokens that constrain the field itself. validator.v9's "dive" token hands
// everything after it to the elements of a slice or map, so those trailing
// tokens describe the element type, not the field — reading, say, the
// element bound in "omitempty,dive,max=512" as if it bounded the slice's own
// length would compare a string-length ceiling against an element-count one.
func fieldLevelValidateTokens(validate []string) []string {
	for i, token := range validate {
		if token == "dive" {
			return validate[:i]
		}
	}
	return validate
}

// enumValuesFor resolves a field's own validate tag tokens against the
// discovered validator-tag registry (tag token -> the set of values that
// tag's validator accepts) and returns the first match, so an enum-typed
// field carries the value set the tree actually enforces rather than only
// the name of the tag that enforces it. Tokens carrying a "=" are bounds,
// never tag names, and are skipped; element-level tokens (everything after
// "dive") describe the element type and are not this field's value set. A
// field whose tags name no registered validator has no extractable value
// set and gets nil, which is the signal that its values cannot be compared.
func (w *goFileWalker) enumValuesFor(tag fieldTagInfo) []string {
	for _, token := range fieldLevelValidateTokens(tag.validate) {
		if strings.Contains(token, "=") {
			continue
		}
		if values, ok := w.enums[token]; ok && len(values) > 0 {
			return append([]string(nil), values...)
		}
	}
	return nil
}

// mergeEnumSets overlays one validator-tag registry onto another, with
// overlay winning any tag both declare.
func mergeEnumSets(base, overlay map[string][]string) map[string][]string {
	merged := make(map[string][]string, len(base)+len(overlay))
	for tag, values := range base {
		merged[tag] = values
	}
	for tag, values := range overlay {
		merged[tag] = values
	}
	return merged
}

// exprString renders a type expression back to source form (e.g.
// "*fixturetypes.WireTime", "[]string"), matching the literal, pointer- and
// package-qualification-preserving declaredType the report format demands.
func exprString(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + exprString(t.X)
	case *ast.SelectorExpr:
		return exprString(t.X) + "." + t.Sel.Name
	case *ast.ArrayType:
		return "[]" + exprString(t.Elt)
	case *ast.MapType:
		return "map[" + exprString(t.Key) + "]" + exprString(t.Value)
	case *ast.InterfaceType:
		return "interface{}"
	default:
		return fmt.Sprintf("%T", expr)
	}
}

// resolveWireType looks up identity in registry. identity is expected to
// already be the resolved, package-qualified type identity (import path +
// "." + declared type name) — any pointer/slice decoration stripped and any
// package-qualified selector resolved via TypeResolver.Resolve/PackageOf
// before this is ever called, not raw declaration syntax. An identity
// registry does not know about is a hard error naming that identity, never
// a silently accepted skip.
func resolveWireType(identity string, registry *WireTypeRegistry) (WireType, error) {
	wireType, ok := registry.Types[identity]
	if !ok {
		return WireType{}, fmt.Errorf("unregistered wire type %q", identity)
	}
	return wireType, nil
}

// receiverTypeName strips a pointer receiver down to its bare type name.
func receiverTypeName(expr ast.Expr) string {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

// fileConstStrings scans file's top-level const/var declarations for
// name = "string literal" pairs, the lookup table method-set and enum
// discovery both use to resolve an identifier back to the constant string
// value it names.
func fileConstStrings(file *ast.File) map[string]string {
	consts := map[string]string{}
	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || (genDecl.Tok != token.CONST && genDecl.Tok != token.VAR) {
			continue
		}
		for _, spec := range genDecl.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range valueSpec.Names {
				if i >= len(valueSpec.Values) {
					continue
				}
				lit, ok := valueSpec.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				if s, err := strconv.Unquote(lit.Value); err == nil {
					consts[name.Name] = s
				}
			}
		}
	}
	return consts
}

// methodSetAccumulator collects the recognized methods and resolved
// identity facts found for one receiver type while scanning a file.
type methodSetAccumulator struct {
	methodSet                              map[string]bool
	methods                                []string
	featureName, requestType, responseType string
}

// discoverMethodSets scans path for types implementing any of
// GetFeatureName/GetRequestType/GetResponseType, resolving GetFeatureName's
// returned identifier to its constant string value and GetRequestType's/
// GetResponseType's reflect.TypeOf(X{}) argument to the type name X —
// never derived from the receiver type's own name or the file name, since
// real Feature types disagree with both. A reflect.TypeOf argument may name
// a type from another package (a real 1.6 shape: cross-block and
// cross-version imports); that selector is resolved through this file's own
// import declarations, exactly like any other package-qualified type
// elsewhere in the walker, rather than left to resolve silently to "".
func discoverMethodSets(path string) ([]MethodSetInfo, error) {
	fset, file, err := parseFileSet(path)
	if err != nil {
		return nil, err
	}
	consts := fileConstStrings(file)
	resolver := newTypeResolver()
	for _, binding := range importBindingsFromFile(file) {
		resolver.RegisterImport(path, binding)
	}

	byType := map[string]*methodSetAccumulator{}
	var order []string
	const (
		methodFeatureName  = "GetFeatureName"
		methodRequestType  = "GetRequestType"
		methodResponseType = "GetResponseType"
	)

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 {
			continue
		}
		if fn.Name.Name != methodFeatureName && fn.Name.Name != methodRequestType && fn.Name.Name != methodResponseType {
			continue
		}
		recvType := receiverTypeName(fn.Recv.List[0].Type)
		if recvType == "" {
			continue
		}
		acc, ok := byType[recvType]
		if !ok {
			acc = &methodSetAccumulator{methodSet: map[string]bool{}}
			byType[recvType] = acc
			order = append(order, recvType)
		}
		if !acc.methodSet[fn.Name.Name] {
			acc.methodSet[fn.Name.Name] = true
			acc.methods = append(acc.methods, fn.Name.Name)
		}
		switch fn.Name.Name {
		case methodFeatureName:
			acc.featureName = resolveReturnedString(fn, consts)
		case methodRequestType:
			requestType, err := resolveReturnedTypeOfArgument(fn, fset, path, resolver)
			if err != nil {
				return nil, err
			}
			acc.requestType = requestType
		case methodResponseType:
			responseType, err := resolveReturnedTypeOfArgument(fn, fset, path, resolver)
			if err != nil {
				return nil, err
			}
			acc.responseType = responseType
		}
	}

	result := make([]MethodSetInfo, 0, len(order))
	for _, typeName := range order {
		acc := byType[typeName]
		result = append(result, MethodSetInfo{
			TypeName:     typeName,
			Methods:      acc.methods,
			FeatureName:  acc.featureName,
			RequestType:  acc.requestType,
			ResponseType: acc.responseType,
		})
	}
	return result, nil
}

// resolveReturnedString resolves a single-statement-return function's
// result to a string: a direct string literal, or an identifier resolved
// against consts.
func resolveReturnedString(fn *ast.FuncDecl, consts map[string]string) string {
	if fn.Body == nil {
		return ""
	}
	for _, stmt := range fn.Body.List {
		ret, ok := stmt.(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 1 {
			continue
		}
		switch v := ret.Results[0].(type) {
		case *ast.BasicLit:
			if v.Kind == token.STRING {
				if s, err := strconv.Unquote(v.Value); err == nil {
					return s
				}
			}
		case *ast.Ident:
			if s, ok := consts[v.Name]; ok {
				return s
			}
		}
	}
	return ""
}

// resolveReturnedTypeOfArgument resolves "return reflect.TypeOf(X{})" to the
// type X names: the bare type name for a same-package composite literal
// (*ast.Ident), or the fully resolved, package-qualified identity (import
// path + "." + type name) for a package-qualified one (*ast.SelectorExpr),
// resolved through resolver's binding for path — the same TypeResolver
// mechanism every other package-qualified type in the walker goes through,
// rather than a second, silent code path that drops the package entirely. A
// selector whose import binding resolver does not know about is a hard
// error naming the file, line and selector — the same convention
// walkFieldType's own SelectorExpr handling uses — never a silent empty
// result.
func resolveReturnedTypeOfArgument(fn *ast.FuncDecl, fset *token.FileSet, path string, resolver *TypeResolver) (string, error) {
	if fn.Body == nil {
		return "", nil
	}
	for _, stmt := range fn.Body.List {
		ret, ok := stmt.(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 1 {
			continue
		}
		call, ok := ret.Results[0].(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			continue
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "TypeOf" {
			continue
		}
		composite, ok := call.Args[0].(*ast.CompositeLit)
		if !ok {
			continue
		}
		switch typeExpr := composite.Type.(type) {
		case *ast.Ident:
			return typeExpr.Name, nil
		case *ast.SelectorExpr:
			pkgIdent, ok := typeExpr.X.(*ast.Ident)
			if !ok {
				position := fset.Position(typeExpr.Pos())
				return "", fmt.Errorf("%s:%d: reflect.TypeOf argument has an unsupported qualified type expression", position.Filename, position.Line)
			}
			selector := pkgIdent.Name + "." + typeExpr.Sel.Name
			qualified, err := resolver.Resolve(path, selector)
			if err != nil {
				position := fset.Position(typeExpr.Pos())
				return "", fmt.Errorf("%s:%d: reflect.TypeOf argument uses unresolved selector %q: %w", position.Filename, position.Line, selector, err)
			}
			return qualified, nil
		}
	}
	return "", nil
}

// importBindingsFromFile extracts file's own import declarations as
// ImportBinding pairs: an explicit "alias \"path\"" import keeps its
// written alias; an unaliased import's binding is Go's own default — the
// last slash-separated segment of the import path — matching how that
// import is actually referred to elsewhere in the file.
func importBindingsFromFile(file *ast.File) []ImportBinding {
	var bindings []ImportBinding
	for _, imp := range file.Imports {
		importPath, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		alias := importPath
		if idx := strings.LastIndex(importPath, "/"); idx >= 0 {
			alias = importPath[idx+1:]
		}
		if imp.Name != nil {
			alias = imp.Name.Name
		}
		bindings = append(bindings, ImportBinding{Alias: alias, Path: importPath})
	}
	return bindings
}

// marshalerMethods accumulates whether a receiver type's MarshalJSON and
// UnmarshalJSON declarations were both found, and the position of the
// MarshalJSON one — the position the hard-error/report line always names.
type marshalerMethods struct {
	marshal, unmarshal bool
	marshalPos         token.Pos
}

// discoverCustomMarshalers scans path for types defining a MarshalJSON/
// UnmarshalJSON pair. Every type it finds must already be present in
// res.Registry, keyed by its resolved, package-qualified identity — see
// stubs.go's original doc comment on this function for the full contract.
func discoverCustomMarshalers(path string, res *Resolution) ([]MarshalerInfo, error) {
	fset, file, err := parseFileSet(path)
	if err != nil {
		return nil, err
	}

	byType := map[string]*marshalerMethods{}
	var order []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 {
			continue
		}
		if fn.Name.Name != "MarshalJSON" && fn.Name.Name != "UnmarshalJSON" {
			continue
		}
		recvType := receiverTypeName(fn.Recv.List[0].Type)
		if recvType == "" {
			continue
		}
		methods, ok := byType[recvType]
		if !ok {
			methods = &marshalerMethods{}
			byType[recvType] = methods
			order = append(order, recvType)
		}
		if fn.Name.Name == "MarshalJSON" {
			methods.marshal = true
			methods.marshalPos = fn.Pos()
		} else {
			methods.unmarshal = true
		}
	}

	var qualifiedPackage string
	for _, typeName := range order {
		methods := byType[typeName]
		if !methods.marshal || !methods.unmarshal {
			continue
		}
		if qualifiedPackage == "" {
			qualifiedPackage, err = res.Resolver.PackageOf(path)
			if err != nil {
				return nil, err
			}
		}
		break
	}

	var infos []MarshalerInfo
	for _, typeName := range order {
		methods := byType[typeName]
		if !methods.marshal || !methods.unmarshal {
			continue
		}
		position := fset.Position(methods.marshalPos)
		qualified := qualifiedPackage + "." + typeName
		if _, ok := res.Registry.Types[qualified]; !ok {
			return nil, fmt.Errorf("unregistered custom marshaler: type %s (%s:%d) defines a MarshalJSON/UnmarshalJSON pair but is not registered in the wire-type registry under %q — register its wire representation there", typeName, position.Filename, position.Line, qualified)
		}
		infos = append(infos, MarshalerInfo{Type: typeName, File: position.Filename, Line: position.Line})
	}
	return infos, nil
}

// discoverStructValidators scans path for *.RegisterStructValidation(fn,
// Type{}, ...) calls and returns the target type names they were
// registered against — matched by the method name, not by the receiver
// identifier, so it is unaffected by what a given tree happens to call its
// registry variable.
func discoverStructValidators(path string) ([]string, error) {
	_, file, err := parseFileSet(path)
	if err != nil {
		return nil, err
	}
	var targets []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "RegisterStructValidation" || len(call.Args) < 2 {
			return true
		}
		for _, arg := range call.Args[1:] {
			composite, ok := arg.(*ast.CompositeLit)
			if !ok {
				continue
			}
			if ident, ok := composite.Type.(*ast.Ident); ok {
				targets = append(targets, ident.Name)
			}
		}
		return true
	})
	return targets, nil
}

// extractEnums scans path for Validate.RegisterValidation("<tag>", isValidX)
// calls, keyed by the tag token rather than the function name, and resolves
// each such isValidX function's accepted set by reading the identifiers in
// its switch statement's non-default case list and resolving those
// identifiers back to their declared constant string values.
func extractEnums(path string) (map[string][]string, error) {
	_, file, err := parseFileSet(path)
	if err != nil {
		return nil, err
	}
	return extractEnumsFromFile(file), nil
}

// extractEnumsFromFile is extractEnums over an already-parsed AST, for
// callers (the field walker) that have the file open already and would
// otherwise re-read and re-parse it purely to learn the same thing.
func extractEnumsFromFile(file *ast.File) map[string][]string {
	consts := fileConstStrings(file)
	funcs := map[string]*ast.FuncDecl{}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil {
			funcs[fn.Name.Name] = fn
		}
	}

	enums := map[string][]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "RegisterValidation" || len(call.Args) != 2 {
			return true
		}
		tagLit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || tagLit.Kind != token.STRING {
			return true
		}
		tag, err := strconv.Unquote(tagLit.Value)
		if err != nil {
			return true
		}
		fnIdent, ok := call.Args[1].(*ast.Ident)
		if !ok {
			return true
		}
		fnDecl, ok := funcs[fnIdent.Name]
		if !ok {
			return true
		}
		enums[tag] = extractSwitchCaseValues(fnDecl, consts)
		return true
	})
	return enums
}

// discoverValidatorFuncValues scans file for every top-level, receiverless
// function whose body's first switch statement enumerates a non-empty set of
// accepted values (extractSwitchCaseValues), keyed by the function's own bare
// name — independent of whether file also contains the RegisterValidation
// call that names it. A tree-wide pass collecting this map from every file of
// one package (buildTreeIndex) is what lets a RegisterValidation call in one
// file resolve a validator function declared in a *different* file of the
// same package — the real 1.6 shape: ocpp1.6/types centralizes many
// RegisterValidation calls in types.go against isValidX functions declared in
// a sibling file, security_extension.go, which extractEnumsFromFile's own
// single-file funcs lookup cannot see.
func discoverValidatorFuncValues(file *ast.File) map[string][]string {
	consts := fileConstStrings(file)
	out := map[string][]string{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil {
			continue
		}
		if values := extractSwitchCaseValues(fn, consts); len(values) > 0 {
			out[fn.Name.Name] = values
		}
	}
	return out
}

// discoverValidationTagFuncNames scans file for every
// Validate.RegisterValidation("<tag>", isValidX) call, returning the bare
// function name each tag names — deliberately not yet resolved to a value
// set, since isValidX may be declared in a different file of the same
// package than the call itself, which no single file's AST can answer on its
// own (see discoverValidatorFuncValues). The unqualified identifier is
// guaranteed to name something in the calling file's own package: Go has no
// syntax for an unqualified identifier to refer to another package's
// declaration.
func discoverValidationTagFuncNames(file *ast.File) map[string]string {
	out := map[string]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "RegisterValidation" || len(call.Args) != 2 {
			return true
		}
		tagLit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || tagLit.Kind != token.STRING {
			return true
		}
		tag, err := strconv.Unquote(tagLit.Value)
		if err != nil {
			return true
		}
		fnIdent, ok := call.Args[1].(*ast.Ident)
		if !ok {
			return true
		}
		out[tag] = fnIdent.Name
		return true
	})
	return out
}

// extractSwitchCaseValues collects, in source order, the values of every
// non-default case clause in fn's (first) switch statement, resolving each
// case identifier against consts and passing string literals through
// directly.
func extractSwitchCaseValues(fn *ast.FuncDecl, consts map[string]string) []string {
	var values []string
	if fn.Body == nil {
		return values
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		for _, stmt := range sw.Body.List {
			clause, ok := stmt.(*ast.CaseClause)
			if !ok || clause.List == nil {
				continue // a default clause has a nil List
			}
			for _, expr := range clause.List {
				switch v := expr.(type) {
				case *ast.Ident:
					if s, ok := consts[v.Name]; ok {
						values = append(values, s)
					}
				case *ast.BasicLit:
					if v.Kind == token.STRING {
						if s, err := strconv.Unquote(v.Value); err == nil {
							values = append(values, s)
						}
					}
				}
			}
		}
		return false
	})
	return values
}
