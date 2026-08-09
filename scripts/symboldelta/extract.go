package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// extractTree walks every .go file under root (recursing into
// subdirectories, one Go package per directory by convention, skipping
// "_test.go" files and any directory literally named "testdata"), parses
// each with go/parser only (no type-checking, no module resolution -- see
// model.go's SymbolTable doc), and folds every file's declarations into one
// SymbolTable. root may be a real module subtree or an ad hoc directory of
// generated files with no go.mod: both parse identically, since nothing
// here needs a resolved import graph.
func extractTree(root string) (*SymbolTable, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return extractFiles(root, files)
}

// extractFiles is extractTree's explicit-file-list twin: given exactly the
// files to read (each still reported relative to root), it applies the same
// parse-and-index pipeline. This is what a diff between two SPECIFIC,
// corresponding slices of two trees should use instead of extractTree's
// recursive walk: walking a whole "ocpp2.0.1/..." tree on the old side pulls
// in every one of the ~60 messages this task never regenerates, and every
// one of them would show up as an old-only symbol with no counterpart on the
// new side (which only ever contains the four regenerated messages plus
// types_gen.go) -- not a real rename or removal, just noise from comparing
// two trees of deliberately different scope. Passing the exact file list
// that corresponds to what the swap actually replaces (types.go's
// superseded declarations, and the four message files, on both sides)
// keeps the diff apples-to-apples.
func extractFiles(root string, files []string) (*SymbolTable, error) {
	table := newSymbolTable()
	fset := token.NewFileSet()

	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		relFile, relErr := filepath.Rel(root, path)
		if relErr != nil {
			relFile = path
		}
		if err := indexFile(table, f, f.Name.Name, relFile); err != nil {
			return nil, fmt.Errorf("index %s: %w", path, err)
		}
	}
	return table, nil
}

// indexFile folds one parsed file's top-level declarations into table.
// Structs, enums (named-string types accompanied by a const block of the
// same type) and aliases are recorded from *ast.GenDecl(TYPE); constructor-
// shaped funcs from *ast.FuncDecl; validator registration calls from a
// whole-file *ast.Inspect pass, since those calls live inside init(), not
// at file scope.
func indexFile(table *SymbolTable, f *ast.File, pkg, file string) error {
	// Pass 1: type declarations (structs, plain named types, aliases).
	// Plain named "type X string" declarations are provisionally recorded
	// as (empty) enums; pass 2 fills in their const values, and any that
	// never gets a const value stays an enum with zero Values -- which is
	// fine, since this codebase declares no bare string type that isn't an
	// enum.
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || !ts.Name.IsExported() {
				continue
			}
			name := ts.Name.Name
			if ts.Assign.IsValid() {
				table.Aliases[name] = AliasSymbol{Name: name, Package: pkg, File: file, Target: exprString(ts.Type)}
				continue
			}
			switch t := ts.Type.(type) {
			case *ast.StructType:
				table.Structs[name] = StructSymbol{Name: name, Package: pkg, File: file, Fields: structFields(t)}
			case *ast.Ident:
				if t.Name == "string" {
					table.Enums[name] = EnumSymbol{Name: name, Package: pkg, File: file, Underlying: "string"}
				}
			}
		}
	}

	// Pass 2: const blocks whose values belong to one of this file's enum
	// types. A ValueSpec with neither an explicit Type nor Values inherits
	// both from the immediately preceding spec in the same GenDecl -- the
	// same rule gofmt/go vet apply -- so lastType/lastEnum carry forward
	// across specs within one GenDecl.
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		var lastTypeName string
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if vs.Type != nil {
				lastTypeName = exprString(vs.Type)
			}
			enum, isEnum := table.Enums[lastTypeName]
			if !isEnum {
				continue
			}
			if len(vs.Values) == 0 {
				continue // iota-style follow-on with no literal of its own; not used in this codebase's enums
			}
			for i, name := range vs.Names {
				if !name.IsExported() && name.Name != "_" {
					continue
				}
				if i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				enum.ConstNames = append(enum.ConstNames, name.Name)
				enum.Values = append(enum.Values, value)
			}
			table.Enums[lastTypeName] = enum
		}
	}

	// Pass 3: top-level funcs.
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Recv != nil || !fd.Name.IsExported() {
			continue
		}
		fn := FuncSymbol{Name: fd.Name.Name, Package: pkg, File: file}
		if fd.Type.Params != nil {
			for _, p := range fd.Type.Params.List {
				n := len(p.Names)
				if n == 0 {
					n = 1
				}
				for i := 0; i < n; i++ {
					fn.Params = append(fn.Params, exprString(p.Type))
				}
			}
		}
		if fd.Type.Results != nil {
			for _, r := range fd.Type.Results.List {
				n := len(r.Names)
				if n == 0 {
					n = 1
				}
				for i := 0; i < n; i++ {
					fn.Results = append(fn.Results, exprString(r.Type))
				}
			}
		}
		if len(fn.Results) > 0 {
			ret := fn.Results[0]
			ret = strings.TrimPrefix(ret, "*")
			if ret != "error" {
				fn.ReturnsStructName = ret
			}
		}
		table.Funcs[fn.Name] = fn
	}

	// Pass 4: RegisterValidation / RegisterStructValidation calls, wherever
	// they occur in the file (always inside an init() body in this
	// codebase, so a plain top-level GenDecl/FuncDecl walk would miss
	// them).
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "RegisterValidation":
			if len(call.Args) != 2 {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			tag, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			fnIdent, ok := call.Args[1].(*ast.Ident)
			if !ok {
				return true
			}
			table.RegisteredValidations[tag] = fnIdent.Name
		case "RegisterStructValidation":
			if len(call.Args) != 2 {
				return true
			}
			fnIdent, ok := call.Args[0].(*ast.Ident)
			if !ok {
				return true
			}
			target := exprString(call.Args[1])
			target = strings.TrimSuffix(target, "{}")
			table.StructValidations[target] = fnIdent.Name
		}
		return true
	})

	return nil
}

// structFields renders a struct type's field list into FieldSymbols, one
// per declared name (a grouped declaration like "Foo, Bar string" yields
// two FieldSymbols sharing one tag/type, matching how encoding/json and
// validator.v9 treat them). An embedded field (no explicit name) is
// recorded once, keyed by its type's own base name, with Embedded set.
func structFields(t *ast.StructType) []FieldSymbol {
	var fields []FieldSymbol
	if t.Fields == nil {
		return fields
	}
	for _, f := range t.Fields.List {
		var rawTag string
		if f.Tag != nil {
			if unquoted, err := strconv.Unquote(f.Tag.Value); err == nil {
				rawTag = unquoted
			}
		}
		jsonTag, jsonOpts := splitTag(reflect.StructTag(rawTag).Get("json"))
		validateTag := reflect.StructTag(rawTag).Get("validate")
		typeStr := exprString(f.Type)

		if len(f.Names) == 0 {
			base := typeStr
			base = strings.TrimPrefix(base, "*")
			if idx := strings.LastIndex(base, "."); idx >= 0 {
				base = base[idx+1:]
			}
			fields = append(fields, FieldSymbol{
				GoName: base, GoType: typeStr, JSONTag: jsonTag, JSONOptions: jsonOpts,
				ValidateTag: validateTag, Embedded: true,
			})
			continue
		}
		for _, name := range f.Names {
			fields = append(fields, FieldSymbol{
				GoName: name.Name, GoType: typeStr, JSONTag: jsonTag, JSONOptions: jsonOpts,
				ValidateTag: validateTag,
			})
		}
	}
	return fields
}

func splitTag(v string) (key, opts string) {
	parts := strings.SplitN(v, ",", 2)
	if len(parts) == 0 {
		return "", ""
	}
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], parts[1]
}

// exprString renders the small set of type-expression shapes this codebase
// actually uses (qualified identifiers, pointers, slices, maps) without
// needing go/types or a resolved import graph. Anything else falls back to
// go/printer, which never fails but may render less tidily; that fallback
// is not expected to trigger on this corpus.
func exprString(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return exprString(t.X) + "." + t.Sel.Name
	case *ast.StarExpr:
		return "*" + exprString(t.X)
	case *ast.ArrayType:
		return "[]" + exprString(t.Elt)
	case *ast.MapType:
		return "map[" + exprString(t.Key) + "]" + exprString(t.Value)
	case *ast.CompositeLit:
		return exprString(t.Type) + "{}"
	case *ast.InterfaceType:
		return "interface{}"
	default:
		return printFallback(e)
	}
}

func printFallback(e ast.Expr) string {
	var buf strings.Builder
	fset := token.NewFileSet()
	if err := printer.Fprint(&buf, fset, e); err != nil {
		return fmt.Sprintf("%T", e)
	}
	return buf.String()
}
