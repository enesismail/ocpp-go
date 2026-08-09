package main

import (
	"fmt"
	"go/ast"
)

// structDecl names the file a tree-wide struct declaration was found in,
// alongside its own bare name — the two facts needed to fetch its fields
// (via a cached walkGoFile call) once a field elsewhere in the tree turns
// out to reference it.
type structDecl struct {
	file string
	name string
	// line is where the type is declared, so a rule attached to the struct as
	// a whole can cite a source location the way a field row does.
	line int
}

// treeIndex is the tree-wide state built once, before any message is
// walked: every file's package identity and import bindings (so
// package-qualified selectors anywhere in the tree resolve), every scalar
// alias and validator-tag value set declared anywhere (so a field in one
// file can carry a fact declared in another), every top-level struct
// declaration (so a cross-file/cross-package composite or array-element
// reference can be found and expanded — see expand.go), and every
// RegisterStructValidation target (so a STRUCT_VALIDATOR row can name the
// rule that fired).
type treeIndex struct {
	resolver         *TypeResolver
	resolution       *Resolution
	structs          map[string]structDecl
	validatorFns     map[string]string
	structFieldCache map[string][]StructInfo
	// usedCrossFileStructs records every qualified struct identity expand.go
	// actually stitched into some message's field list — the evidence
	// sharedTypes.go uses to confirm a shared-types-package struct is really
	// modelled by a Go field somewhere, rather than merely declared (the real
	// case this guards: types.CustomData exists in the tree but, as of this
	// run, is not referenced by any message field).
	usedCrossFileStructs map[string]bool
}

// buildTreeIndex performs the tree-wide registration pass every later step
// needs already done: parse every file once, register its package identity
// and import bindings, then seed the tree-wide scalar-alias and
// validator-tag registries, the tree-wide struct-declaration index, and the
// struct-validator function map. It then seeds the wire-type registry with
// this tool's known custom-marshaler types and validates, tree-wide, that
// no other type in the scanned files defines a MarshalJSON/UnmarshalJSON
// pair the registry does not already know about — a hard failure, never a
// silent guess.
func buildTreeIndex(goFiles []string, moduleRoot, modulePath string) (*treeIndex, error) {
	resolver := newTypeResolver()
	registry := newWireTypeRegistry()
	seedWireTypeRegistry(registry, modulePath)
	resolution := newResolution(resolver, registry)

	idx := &treeIndex{
		resolver:             resolver,
		resolution:           resolution,
		structs:              map[string]structDecl{},
		validatorFns:         map[string]string{},
		structFieldCache:     map[string][]StructInfo{},
		usedCrossFileStructs: map[string]bool{},
	}

	parsed := make(map[string]*astFileHandle, len(goFiles))
	for _, file := range goFiles {
		importPath, err := importPathForFile(file, moduleRoot, modulePath)
		if err != nil {
			return nil, err
		}
		resolver.RegisterFile(file, importPath)
		fset, astFile, err := parseASTFileWithPositions(file)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", file, err)
		}
		parsed[file] = &astFileHandle{file: astFile, importPath: importPath, structLines: structDeclLines(fset, astFile)}
		for _, binding := range importBindingsFromFile(astFile) {
			resolver.RegisterImport(file, binding)
		}
	}

	// funcValuesByPkg and tagCalls together replace a naive per-file
	// RegisterValidation resolution: a tag's isValidX function is only
	// guaranteed to live in the same *package* as the call that registers
	// it, never necessarily the same *file* (ocpp1.6/types splits its
	// RegisterValidation calls, centralized in types.go, from several of the
	// isValidX functions they name, declared in a sibling file,
	// security_extension.go). funcValuesByPkg is built from every file
	// before any tag is resolved, so resolution never depends on which
	// order goFiles happens to visit the call site and the declaration in.
	funcValuesByPkg := map[string]map[string][]string{}
	type validationTagCall struct {
		importPath, tag, fnName string
	}
	var tagCalls []validationTagCall

	for _, file := range goFiles {
		handle := parsed[file]
		for bareName, basic := range localScalarAliasIndex(handle.file) {
			resolution.RegisterScalarAlias(handle.importPath+"."+bareName, basic)
		}
		if funcValuesByPkg[handle.importPath] == nil {
			funcValuesByPkg[handle.importPath] = map[string][]string{}
		}
		for name, values := range discoverValidatorFuncValues(handle.file) {
			funcValuesByPkg[handle.importPath][name] = values
		}
		for tag, fnName := range discoverValidationTagFuncNames(handle.file) {
			tagCalls = append(tagCalls, validationTagCall{importPath: handle.importPath, tag: tag, fnName: fnName})
		}
		names, _ := localStructIndex(handle.file)
		for _, name := range names {
			idx.structs[handle.importPath+"."+name] = structDecl{file: file, name: name, line: handle.structLines[name]}
		}
		for target, fn := range structValidatorFunctions(handle.file) {
			idx.validatorFns[handle.importPath+"."+target] = fn
		}
	}
	for _, call := range tagCalls {
		if values, ok := funcValuesByPkg[call.importPath][call.fnName]; ok {
			resolution.RegisterEnum(call.tag, values)
		}
	}

	for _, file := range goFiles {
		if _, err := discoverCustomMarshalers(file, resolution); err != nil {
			return nil, err
		}
	}

	return idx, nil
}

// astFileHandle pairs one file's parsed AST with its own resolved package
// identity, so the second registration pass below does not need to
// re-parse or re-resolve either.
type astFileHandle struct {
	file       *ast.File
	importPath string
	// structLines is the source line of each top-level struct the file
	// declares, carried alongside the AST because the position information is
	// available only while the file is being parsed.
	structLines map[string]int
}

// seedWireTypeRegistry registers this tool's known custom-marshaler types:
// ocpp2.0.1/types.DateTime and ocpp1.6/types.DateTime both wrap time.Time
// and marshal to a JSON string (the date-time narrowing rule depends on
// this), and ocpp2.0.1/types.CustomData marshals its VendorId/Extensions
// pair to a plain JSON object. Both trees are seeded unconditionally: the
// registry is a fact about this tool's known custom marshalers, independent
// of which single tree a given run's -tree flag names, so a 1.6 run and a
// 2.0.1 run share one seed list.
func seedWireTypeRegistry(registry *WireTypeRegistry, modulePath string) {
	registry.Register(modulePath+"/ocpp2.0.1/types.DateTime", WireType{Type: "string", Format: "date-time"})
	registry.Register(modulePath+"/ocpp1.6/types.DateTime", WireType{Type: "string", Format: "date-time"})
	registry.Register(modulePath+"/ocpp2.0.1/types.CustomData", WireType{Type: "object"})
}

// structsForFile returns file's own StructInfo list, computed by walkGoFile
// on first use and cached thereafter — the same file is reached repeatedly
// as different messages stitch in the shared types package.
func (idx *treeIndex) structsForFile(file string) ([]StructInfo, error) {
	if cached, ok := idx.structFieldCache[file]; ok {
		return cached, nil
	}
	structs, err := walkGoFile(file, idx.resolution)
	if err != nil {
		return nil, err
	}
	idx.structFieldCache[file] = structs
	return structs, nil
}

// fieldsFor returns the already-flattened field list walkGoFile produced
// for decl's own struct.
func (idx *treeIndex) fieldsFor(decl structDecl) ([]GoField, error) {
	structs, err := idx.structsForFile(decl.file)
	if err != nil {
		return nil, err
	}
	for _, s := range structs {
		if s.Name == decl.name {
			return s.Fields, nil
		}
	}
	return nil, fmt.Errorf("internal error: struct %s not found in %s despite being indexed there", decl.name, decl.file)
}
