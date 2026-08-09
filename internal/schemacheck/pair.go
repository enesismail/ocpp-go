package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// loadSchemas parses every file in schemaFiles and returns the resulting
// documents keyed by their own base filename (e.g. "BootNotificationRequest.json"),
// the identity message pairing (buildMessageSide) looks them up by.
func loadSchemas(schemaFiles []string) (map[string]SchemaDocument, error) {
	docs := make(map[string]SchemaDocument, len(schemaFiles))
	for _, file := range schemaFiles {
		doc, err := walkSchema(file)
		if err != nil {
			return nil, fmt.Errorf("read schema %s: %w", file, err)
		}
		docs[filepath.Base(file)] = doc
	}
	return docs, nil
}

// indexSchemasByFeature reindexes a schema corpus — keyed, as loadSchemas
// returns it, by base filename — into two feature-name-keyed maps, one per
// message side, so message pairing never has to guess a schema file's name
// from a feature name. Each document's own Shape.FilenameShape (schema_walk.go's
// per-file detection) says how to strip its filename down to the feature name
// it names: "FooResponse.json" always names a response; "FooRequest.json"
// (the 2.0.1 convention) and the bare "Foo.json" (the 1.6 convention) both
// name a request, depending only on that file's own name — never a
// directory- or corpus-wide assumption, so a run spanning several schema
// sets with different conventions (a 2.0.1 run's single suffixed set, or a
// 1.6 run's two bare sets) needs no special-casing here.
//
// Two files claiming the same (featureName, side) is a hard error naming
// both — silently letting the second overwrite the first would hide a real
// corpus problem behind an ordinary-looking unpaired-message report.
func indexSchemasByFeature(byName map[string]SchemaDocument) (requests, responses map[string]SchemaDocument, err error) {
	requests = map[string]SchemaDocument{}
	responses = map[string]SchemaDocument{}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		doc := byName[name]
		var featureName, side string
		var target map[string]SchemaDocument
		switch doc.Shape.FilenameShape {
		case "response":
			featureName, target, side = strings.TrimSuffix(name, "Response.json"), responses, "response"
		case "request-suffixed":
			featureName, target, side = strings.TrimSuffix(name, "Request.json"), requests, "request"
		case "request-bare":
			featureName, target, side = strings.TrimSuffix(name, ".json"), requests, "request"
		default:
			return nil, nil, fmt.Errorf("schema %s: unrecognized filename shape %q", doc.File, doc.Shape.FilenameShape)
		}
		if existing, ok := target[featureName]; ok {
			return nil, nil, fmt.Errorf("two schema files claim the %s side of %q: %s and %s", side, featureName, existing.File, doc.File)
		}
		target[featureName] = doc
	}
	return requests, responses, nil
}

// toSchemaField converts one walked SchemaProperty into the report's
// SchemaField payload. present is false for a path the schema simply does
// not declare (a Go-only field): the returned SchemaField is then the zero
// value, matching a ComparisonInput with SchemaPresent=false.
func toSchemaField(prop SchemaProperty, present bool) SchemaField {
	if !present {
		return SchemaField{}
	}
	var typ *string
	if prop.Type != "" {
		t := prop.Type
		typ = &t
	}
	return SchemaField{
		Pointer:     prop.Pointer,
		Type:        typ,
		Required:    prop.Required,
		Format:      prop.Format,
		Enum:        prop.Enum,
		Constraints: prop.Constraints,
	}
}

// buildMessageSide walks the Go struct named typeName, declared in
// messageFile, expands it against the whole tree (composite/array-element
// stitching — see expand.go), pairs the result against schemaDoc's own
// walked properties by their shared JSON-name-dotted path, classifies every
// paired row, and returns the finished MessageSide alongside the sorted set
// of paths this side paired (feeding sharedType occurrence tracking in the
// caller).
func buildMessageSide(idx *treeIndex, typeName, messageFile string, schemaDoc SchemaDocument) (MessageSide, []pairedPath, error) {
	rootIdentity, ok := resolveTypeIdentity(idx.resolver, messageFile, typeName)
	if !ok {
		return MessageSide{}, nil, fmt.Errorf("resolve root type identity for %s in %s", typeName, messageFile)
	}
	rootFields, err := idx.fieldsFor(structDecl{file: messageFile, name: typeName})
	if err != nil {
		return MessageSide{}, nil, err
	}
	expanded, err := expandFields(idx, rootFields, messageFile, "", map[string]bool{rootIdentity: true})
	if err != nil {
		return MessageSide{}, nil, err
	}

	goByPath := make(map[string]GoField, len(expanded))
	for _, rf := range expanded {
		goByPath[rf.path] = rf.field
	}
	schemaByPath := make(map[string]SchemaProperty, len(schemaDoc.Properties))
	for _, p := range schemaDoc.Properties {
		schemaByPath[p.Path] = p
	}

	pathSet := make(map[string]bool, len(goByPath)+len(schemaByPath))
	for p := range goByPath {
		pathSet[p] = true
	}
	for p := range schemaByPath {
		pathSet[p] = true
	}
	paths := make([]string, 0, len(pathSet))
	for p := range pathSet {
		paths = append(paths, p)
	}
	// Ascending depth (shallowest first, ties broken lexically) so the
	// pairedComposite membership test below always sees a path's parent
	// decided before the path itself.
	sort.Slice(paths, func(i, j int) bool {
		di, dj := strings.Count(paths[i], "."), strings.Count(paths[j], ".")
		if di != dj {
			return di < dj
		}
		return paths[i] < paths[j]
	})

	// A schema property (or Go field) reached only under a composite that
	// itself has no counterpart on the other side is not walked further:
	// once "customData" is established as entirely absent from the Go tree
	// (its own row: ADDITIVE), enumerating a separate row for every one of
	// its own required sub-properties (e.g. customData.vendorId) would
	// report the same single divergence — an object the Go tree does not
	// model — once per property inside it, rather than once. A path's own
	// row is still emitted regardless of whether that path itself pairs;
	// only its *descendants* are pruned, and only when the path's immediate
	// parent failed to pair on both sides (a top-level path's parent is the
	// message root, which always "pairs" by definition).
	pairedComposite := map[string]bool{"": true}
	included := make(map[string]bool, len(paths))
	for _, p := range paths {
		parent := parentJSONPath(p)
		if !pairedComposite[parent] {
			continue
		}
		included[p] = true
		_, goOK := goByPath[p]
		_, schemaOK := schemaByPath[p]
		if goOK && schemaOK {
			pairedComposite[p] = true
		}
	}

	orderedPaths := make([]string, 0, len(included))
	for _, p := range paths {
		if included[p] {
			orderedPaths = append(orderedPaths, p)
		}
	}
	sort.Strings(orderedPaths)

	fields := make([]Field, 0, len(orderedPaths)+1)
	rootRow, hasRootRule, err := rootStructValidatorRow(idx, rootIdentity, typeName, schemaDoc)
	if err != nil {
		return MessageSide{}, nil, err
	}
	if hasRootRule {
		fields = append(fields, rootRow)
	}
	var paired []pairedPath
	for _, p := range orderedPaths {
		goField, goOK := goByPath[p]
		schemaProp, schemaOK := schemaByPath[p]
		schemaField := toSchemaField(schemaProp, schemaOK)

		input := ComparisonInput{
			Go:            goField,
			Schema:        schemaField,
			GoPresent:     goOK,
			SchemaPresent: schemaOK,
		}
		result, err := classifyField(input)
		if err != nil {
			return MessageSide{}, nil, fmt.Errorf("classify %s in %s: %w", p, messageFile, err)
		}
		fields = append(fields, Field{
			Path:     p,
			Go:       goField,
			Schema:   schemaField,
			Class:    result.Class,
			Rule:     result.Rule,
			Detail:   result.Detail,
			Breaking: result.Breaking,
		})
		if goOK && schemaOK {
			paired = append(paired, pairedPath{path: p, goField: goField, schemaProp: schemaProp})
		}
	}

	return MessageSide{
		GoType:     typeName,
		SchemaFile: filepath.Base(schemaDoc.File),
		Fields:     fields,
	}, paired, nil
}

// rootMessagePath is the path a row carries when it describes a message's
// request/response struct as a whole rather than one of its fields: the
// empty path, the same spelling the pairing pass already uses for the
// message root as every top-level path's parent. It is the only row a
// message side can carry at that path.
const rootMessagePath = ""

// rootStructValidatorRow builds the row for a struct-level validation rule
// registered against a message's own request or response struct.
//
// Such a rule is discovered by exactly the same pass that finds a rule
// registered against a shared composite (treeIndex.validatorFns, keyed by
// resolved type identity), but it can never surface through field pairing:
// pairing annotates the *field whose type* carries a rule, and a message's
// own root struct is nobody's field. Without a row of its own the rule would
// be silently absent from a comparison that claims to omit nothing. It is
// therefore emitted here, at the message root, and classified by the same
// classifier every other row goes through, so its class and rule text are
// produced the same way rather than asserted.
//
// The schema side of the row is the schema document's own root: pointer "#",
// the root "type" the document declares, and required, a message payload
// always being present.
func rootStructValidatorRow(idx *treeIndex, rootIdentity, typeName string, doc SchemaDocument) (Field, bool, error) {
	fn, ok := idx.validatorFns[rootIdentity]
	if !ok {
		return Field{}, false, nil
	}
	goField := GoField{
		Name:         typeName,
		DeclaredType: typeName,
		// A Go struct puts a JSON object on the wire; this row describes the
		// struct itself, so that is its wire type.
		WireType:         "object",
		StructValidators: []string{fn},
	}
	// The row describes a rule attached to the struct as a whole, so the
	// location it cites is the struct's own declaration — the same evidence
	// every field row carries, reported for the one row that has no field.
	// The declaration is cited rather than the RegisterStructValidation call
	// that attaches the rule: every other row in the report points at where
	// the thing it describes is declared, and a reader who lands on the struct
	// sees both the fields the rule reads and, in the same file, the rule.
	if decl, ok := idx.structs[rootIdentity]; ok {
		goField.File = decl.file
		goField.Line = decl.line
	}
	schemaField := SchemaField{Pointer: "#", Required: true}
	if doc.RootType != "" {
		rootType := doc.RootType
		schemaField.Type = &rootType
	}
	result, err := classifyField(ComparisonInput{
		Go:            goField,
		Schema:        schemaField,
		GoPresent:     true,
		SchemaPresent: true,
	})
	if err != nil {
		return Field{}, false, fmt.Errorf("classify the %s struct-level rule: %w", typeName, err)
	}
	return Field{
		Path:     rootMessagePath,
		Go:       goField,
		Schema:   schemaField,
		Class:    result.Class,
		Rule:     result.Rule,
		Detail:   fmt.Sprintf("registered against %s itself rather than against one of its fields, so this row's path is the message root", typeName),
		Breaking: result.Breaking,
	}, true, nil
}

// pairedPath is one path both sides declared, carried out of buildMessageSide
// so the caller can derive shared-type occurrence evidence without
// re-walking either side.
type pairedPath struct {
	path       string
	goField    GoField
	schemaProp SchemaProperty
}
