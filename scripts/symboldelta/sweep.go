package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"
)

// Hit is one AST node in the target tree that references something a
// DeltaRow renamed, removed or otherwise changed.
type Hit struct {
	File    string     `json:"file"`
	Line    int        `json:"line"`
	Class   DeltaClass `json:"class"`
	OldName string     `json:"oldName,omitempty"`
	NewName string     `json:"newName,omitempty"`
	Owner   string     `json:"owner,omitempty"`
	Detail  string     `json:"detail"`
}

// SweepResult is the worklist: every hit, plus the distinct file set (the
// union of every file touched by at least one hit) and the rows that
// produced zero hits (worth knowing -- either the rename is not yet used anywhere live,
// or the sweep's own matching missed it, which the plain-text grep
// cross-check run alongside this tool is there to catch).
type SweepResult struct {
	Hits      []Hit    `json:"hits"`
	Files     []string `json:"files"`
	UnhitRows []string `json:"unhitRows"`
}

// sweepTree loads every package matching patterns (via golang.org/x/tools/
// go/packages, full type-checking: NeedSyntax|NeedTypes|NeedTypesInfo, so
// identifiers resolve through pkg.TypesInfo's Uses/Defs/Selections maps of
// real type-checked references, not by lexical text matching) rooted at
// dir, and finds every AST use-site of every delta row,
// filtered to types whose resolved package path starts with typesPkgPrefix
// (the safety net against a same-named 1.6 symbol -- 1.6 and 2.0.1 share
// several package basenames, and this is the "resolve every hit by its
// import block" rule implemented through the type-checker instead of
// textually).
func sweepTree(dir string, patterns []string, typesPkgPrefix string, rows []DeltaRow) (*SweepResult, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedDeps | packages.NeedImports,
		Dir: dir,
		// Tests is required, not optional: several of this sweep's real
		// targets (ocpp2.0.1_test/, and any package whose _test.go files
		// carry the only use-sites) are directories containing ONLY
		// "_test.go" files. Without Tests:true, go/packages loads the
		// production (non-test) variant only, which for such a directory
		// has zero GoFiles -- the sweep would silently visit no files
		// there at all rather than erroring, which is worse than a build
		// failure: it looks like a clean run with nothing to report.
		Tests: true,
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, fmt.Errorf("load %v: %w", patterns, err)
	}
	var loadErrs []string
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		for _, e := range p.Errors {
			loadErrs = append(loadErrs, fmt.Sprintf("%s: %s", p.PkgPath, e))
		}
	})
	if len(loadErrs) > 0 {
		return nil, fmt.Errorf("package load errors:\n%s", strings.Join(loadErrs, "\n"))
	}

	idx := newDeltaIndex(rows)
	var hits []Hit
	fileSet := map[string]bool{}

	seen := map[*packages.Package]bool{}
	var visit func(p *packages.Package)
	visit = func(p *packages.Package) {
		if seen[p] {
			return
		}
		seen[p] = true
		for _, f := range p.Syntax {
			filename := posFile(p.Fset, f.Pos())
			fileHits := sweepFile(p.Fset, f, p.TypesInfo, typesPkgPrefix, idx)
			for _, h := range fileHits {
				h.File = filename
				hits = append(hits, h)
				fileSet[filename] = true
			}
		}
		for _, imp := range p.Imports {
			visit(imp)
		}
	}
	for _, p := range pkgs {
		visit(p)
	}

	hits = dedupeHits(hits)

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].File != hits[j].File {
			return hits[i].File < hits[j].File
		}
		if hits[i].Line != hits[j].Line {
			return hits[i].Line < hits[j].Line
		}
		return hits[i].Detail < hits[j].Detail
	})

	files := make([]string, 0, len(fileSet))
	for f := range fileSet {
		files = append(files, f)
	}
	sort.Strings(files)

	hitRowKeys := map[string]bool{}
	for _, h := range hits {
		hitRowKeys[rowKey(h.Class, h.OldName, h.Owner)] = true
	}
	var unhit []string
	for _, r := range rows {
		if !rowNeedsSweep(r.Class) {
			continue
		}
		if r.Class == ClassChangedConstructorSig && r.OldName == "" {
			continue // "constructor added": no old-side function name exists to find a call site for
		}
		key := rowKey(r.Class, r.OldName, r.OwnerOld)
		if !hitRowKeys[key] {
			unhit = append(unhit, string(r.Class)+" "+r.OwnerOld+"."+r.OldName)
		}
	}
	sort.Strings(unhit)

	return &SweepResult{Hits: hits, Files: files, UnhitRows: unhit}, nil
}

// dedupeHits collapses hits produced by loading the same source file more
// than once, which Tests:true causes deliberately (a package with test
// files is loaded as multiple overlapping variants -- production,
// in-package test, external test -- and this sweep walks every one of
// them): the same file/line/class/old/new combination reported by two
// variants is one real use-site, not two.
func dedupeHits(hits []Hit) []Hit {
	seen := map[string]bool{}
	var out []Hit
	for _, h := range hits {
		key := strings.Join([]string{h.File, fmt.Sprint(h.Line), string(h.Class), h.OldName, h.NewName, h.Owner, h.Detail}, "|")
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, h)
	}
	return out
}

func rowNeedsSweep(c DeltaClass) bool {
	switch c {
	case ClassAddedType, ClassAddedField, ClassAmbiguousRename:
		return false // additions have no old use-site to find; an ambiguous row was never paired, so there is nothing indexed to hit
	default:
		return true
	}
}

func rowKey(class DeltaClass, oldName, owner string) string {
	return string(class) + "|" + owner + "|" + oldName
}

func posFile(fset *token.FileSet, pos token.Pos) string {
	return fset.Position(pos).Filename
}

// deltaIndex is computeDelta's output, reshaped for O(1) lookup during the
// sweep.
type deltaIndex struct {
	renamedType     map[string]DeltaRow            // old type name -> row
	removedType     map[string]DeltaRow            // old type name -> row
	fieldByOwner    map[string]map[string]DeltaRow // owner old name -> old field name -> row (renamed/removed/pointer-changed)
	constructor     map[string]DeltaRow            // old func name -> row
	validatorTag    map[string]DeltaRow            // old tag token -> row
	structValidator map[string]DeltaRow            // old func name -> row
}

func newDeltaIndex(rows []DeltaRow) deltaIndex {
	idx := deltaIndex{
		renamedType:     map[string]DeltaRow{},
		removedType:     map[string]DeltaRow{},
		fieldByOwner:    map[string]map[string]DeltaRow{},
		constructor:     map[string]DeltaRow{},
		validatorTag:    map[string]DeltaRow{},
		structValidator: map[string]DeltaRow{},
	}
	for _, r := range rows {
		switch r.Class {
		case ClassRenamedType:
			idx.renamedType[r.OldName] = r
		case ClassRemovedType:
			idx.removedType[r.OldName] = r
		case ClassRenamedField, ClassRemovedField, ClassChangedPointerness:
			if idx.fieldByOwner[r.OwnerOld] == nil {
				idx.fieldByOwner[r.OwnerOld] = map[string]DeltaRow{}
			}
			idx.fieldByOwner[r.OwnerOld][r.OldName] = r
		case ClassChangedConstructorSig:
			if r.OldName != "" {
				idx.constructor[r.OldName] = r
			}
		case ClassRenamedValidatorTag:
			idx.validatorTag[r.OldName] = r
		case ClassRemovedStructValidator:
			idx.structValidator[r.OldName] = r
		}
	}
	return idx
}

// sweepFile is one *ast.File's contribution: an ast.Inspect walk covering
// every DeltaClass this file's syntax can carry, plus a
// separate raw struct-tag string scan (that part explicitly does not go
// through the type-checker -- struct tag contents are string literals
// either way, so AST and grep see the same bytes there).
func sweepFile(fset *token.FileSet, f *ast.File, info *types.Info, typesPkgPrefix string, idx deltaIndex) []Hit {
	var hits []Hit
	line := func(pos token.Pos) int { return fset.Position(pos).Line }

	underNamedType := func(t types.Type) (*types.Named, bool) {
		for {
			switch u := t.(type) {
			case *types.Named:
				return u, true
			case *types.Pointer:
				t = u.Elem()
			default:
				return nil, false
			}
		}
	}
	inScope := func(named *types.Named) bool {
		pkg := named.Obj().Pkg()
		return pkg != nil && strings.HasPrefix(pkg.Path(), typesPkgPrefix)
	}

	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {

		case *ast.Ident:
			obj := info.Uses[node]
			if obj == nil {
				obj = info.Defs[node]
			}
			tn, ok := obj.(*types.TypeName)
			if !ok || tn.Pkg() == nil || !strings.HasPrefix(tn.Pkg().Path(), typesPkgPrefix) {
				break
			}
			if row, ok := idx.renamedType[tn.Name()]; ok {
				hits = append(hits, Hit{Line: line(node.Pos()), Class: ClassRenamedType, OldName: row.OldName, NewName: row.NewName, Detail: "identifier reference"})
			}
			if row, ok := idx.removedType[tn.Name()]; ok {
				hits = append(hits, Hit{Line: line(node.Pos()), Class: ClassRemovedType, OldName: row.OldName, Detail: "identifier reference to a type with no generated counterpart"})
			}

		case *ast.SelectorExpr:
			sel, ok := info.Selections[node]
			if !ok || sel == nil {
				break
			}
			named, isNamed := underNamedType(sel.Recv())
			if !isNamed || !inScope(named) {
				break
			}
			owner := named.Obj().Name()
			fields, hasOwner := idx.fieldByOwner[owner]
			if !hasOwner {
				break
			}
			if row, ok := fields[node.Sel.Name]; ok {
				hits = append(hits, Hit{Line: line(node.Pos()), Class: row.Class, Owner: owner, OldName: row.OldName, NewName: row.NewName, Detail: "selector " + owner + "." + node.Sel.Name})
			}

		case *ast.CompositeLit:
			t := info.TypeOf(node)
			named, isNamed := underNamedType(t)
			if !isNamed || !inScope(named) {
				break
			}
			owner := named.Obj().Name()
			fields, hasOwner := idx.fieldByOwner[owner]
			if !hasOwner {
				break
			}
			for _, elt := range node.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok {
					continue
				}
				if row, ok := fields[key.Name]; ok {
					hits = append(hits, Hit{Line: line(kv.Pos()), Class: row.Class, Owner: owner, OldName: row.OldName, NewName: row.NewName, Detail: "keyed composite literal " + owner + "{" + key.Name + ": ...}"})
				}
			}

		case *ast.CallExpr:
			// RegisterStructValidation(fn, Type{}) is checked first and
			// separately from the general constructor-call resolution
			// below: the CALLEE here is validator.v9's own method (Fun
			// resolves to a *types.Func whose Pkg() is the validator
			// library, never ocpp2.0.1), so the ordinary "resolve Fun,
			// filter by typesPkgPrefix" path can never see it -- what
			// needs resolving is call.Args[0], the registered function
			// itself.
			if sel, ok := node.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "RegisterStructValidation" && len(node.Args) > 0 {
				if argIdent, ok := node.Args[0].(*ast.Ident); ok {
					if argFn, ok := info.Uses[argIdent].(*types.Func); ok && argFn.Pkg() != nil && strings.HasPrefix(argFn.Pkg().Path(), typesPkgPrefix) {
						if row, ok := idx.structValidator[argFn.Name()]; ok {
							hits = append(hits, Hit{Line: line(node.Pos()), Class: ClassRemovedStructValidator, Owner: row.OwnerOld, OldName: row.OldName, Detail: "RegisterStructValidation(" + argFn.Name() + ", ...)"})
						}
					}
				}
				break
			}

			var fn *types.Func
			switch callee := node.Fun.(type) {
			case *ast.Ident:
				if f, ok := info.Uses[callee].(*types.Func); ok {
					fn = f
				}
			case *ast.SelectorExpr:
				if f, ok := info.Uses[callee.Sel].(*types.Func); ok {
					fn = f
				}
				if sel, ok := info.Selections[callee]; ok && sel != nil {
					if f, ok := sel.Obj().(*types.Func); ok {
						fn = f
					}
				}
			}
			if fn == nil || fn.Pkg() == nil || !strings.HasPrefix(fn.Pkg().Path(), typesPkgPrefix) {
				break
			}
			if row, ok := idx.constructor[fn.Name()]; ok {
				hits = append(hits, Hit{Line: line(node.Pos()), Class: ClassChangedConstructorSig, Owner: row.OwnerOld, OldName: row.OldName, NewName: row.NewName, Detail: "call to " + fn.Name()})
			}

		case *ast.TypeSpec:
			if !node.Assign.IsValid() {
				break
			}
			t := info.TypeOf(node.Type)
			named, isNamed := underNamedType(t)
			if !isNamed || !inScope(named) {
				break
			}
			if row, ok := idx.renamedType[named.Obj().Name()]; ok {
				hits = append(hits, Hit{Line: line(node.Pos()), Class: ClassRenamedType, OldName: row.OldName, NewName: row.NewName, Detail: "type alias " + node.Name.Name + " = " + named.Obj().Name()})
			}
		}
		return true
	})

	hits = append(hits, sweepStructTags(fset, f, idx.validatorTag)...)
	return hits
}

// sweepStructTags is the plain-string-literal half of the sweep: every
// struct field's `validate:"..."` tag content, tokenised on commas, checked
// against every renamed validator-tag token. This deliberately does not use
// TypesInfo -- tag contents are opaque strings to the type-checker, so a
// renamed validator token inside one is exactly the class of breakage a
// purely compiler-driven (identifier-resolution-only) sweep cannot find.
func sweepStructTags(fset *token.FileSet, f *ast.File, tagRows map[string]DeltaRow) []Hit {
	var hits []Hit
	ast.Inspect(f, func(n ast.Node) bool {
		st, ok := n.(*ast.StructType)
		if !ok || st.Fields == nil {
			return true
		}
		for _, field := range st.Fields.List {
			if field.Tag == nil {
				continue
			}
			raw, err := strconv.Unquote(field.Tag.Value)
			if err != nil {
				continue
			}
			validate := reflect.StructTag(raw).Get("validate")
			if validate == "" {
				continue
			}
			for _, tok := range strings.Split(validate, ",") {
				tok = strings.SplitN(tok, "=", 2)[0]
				if row, ok := tagRows[tok]; ok {
					hits = append(hits, Hit{
						Line: fset.Position(field.Tag.Pos()).Line, Class: ClassRenamedValidatorTag,
						Owner: row.OwnerOld, OldName: row.OldName, NewName: row.NewName, Detail: "validate tag token " + tok,
					})
				}
			}
		}
		return true
	})
	return hits
}
