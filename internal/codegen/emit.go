package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// formatFieldLine renders one struct field as the single line every field
// golden pins: "FieldName GoType `json:"..." validate:"..."`", with the
// validate fragment omitted entirely when ValidateTag is empty (a required
// bool renders no validate tag at all).
func formatFieldLine(mapping FieldMapping) string {
	line := mapping.FieldName + " " + mapping.GoType + " `" + mapping.JSONTag
	if mapping.ValidateTag != "" {
		line += " " + mapping.ValidateTag
	}
	return line + "`"
}

// RenderProperty renders one struct field, composing MapProperty's
// placement-agnostic mapping with context.Package's package-contextual
// qualification and context.Transform's naming table. For a $ref to an
// ordinary composite, RenderProperty has no PlacementPlan and therefore
// always renders the bare, unqualified type name — the same way every
// golden fixture in testdata/golden/optional_composite.golden does; full
// package-contextual qualification for an arbitrary composite is a
// RenderMessage/EmitGo concern, which does carry a PlacementPlan. DateTime,
// CustomData and Validate are the exception: unlike an ordinary
// schema-derived composite, they are never subject to reach-count
// placement, because they are not generated at all — they are hand-written
// residents of the types package, never emitted by this generator — so
// they are qualified from context.Package.InTypes alone,
// unconditionally.
func RenderProperty(definition Definition, property Property, context MappingContext, overrides []OverrideRow) (string, error) {
	mapping, err := MapProperty(definition, property, context.Definitions, context.Transform, overrides)
	if err != nil {
		return "", err
	}
	if context.Package.InTypes {
		mapping.GoType = strings.ReplaceAll(mapping.GoType, "types.", "")
	}
	return formatFieldLine(mapping), nil
}

// qualifyBareTypeName inserts pkg. immediately before the bare type name in
// goType, preserving a leading "*" or "[]" prefix.
func qualifyBareTypeName(goType, pkg string) string {
	switch {
	case strings.HasPrefix(goType, "[]"):
		return "[]" + pkg + "." + goType[2:]
	case strings.HasPrefix(goType, "*"):
		return "*" + pkg + "." + goType[1:]
	default:
		return pkg + "." + goType
	}
}

// qualifiedFieldMapping is RenderMessage's and the shared types file
// assembler's own field renderer: unlike RenderProperty, it carries a
// PlacementPlan, so it can additionally qualify an ordinary composite or
// enum $ref whose target is placed in types/ — something RenderProperty
// deliberately never does (see its own doc comment). DateTime and
// CustomData are unaffected by this extra step: MapProperty already
// renders them in their block-package spelling, and the InTypes strip
// below handles them exactly the way RenderProperty does.
func qualifiedFieldMapping(definition Definition, property Property, definitions []Definition, transform TransformConfig, overrides []OverrideRow, placement PlacementPlan, context PackageContext) (FieldMapping, error) {
	mapping, err := MapProperty(definition, property, definitions, transform, overrides)
	if err != nil {
		return FieldMapping{}, err
	}
	if context.InTypes {
		mapping.GoType = strings.ReplaceAll(mapping.GoType, "types.", "")
		return mapping, nil
	}

	refName := ""
	switch {
	case property.Ref != "":
		refName = refTargetName(property.Ref)
	case property.Type == "array" && property.Items != nil && property.Items.Ref != "":
		refName = refTargetName(property.Items.Ref)
	}
	if refName != "" && placement.Home[refName] == "types" {
		mapping.GoType = qualifyBareTypeName(mapping.GoType, "types")
	}
	return mapping, nil
}

// structBody renders a struct's field list (raw, unformatted — the caller
// runs the result through format.Source, once, for the whole file it is
// assembling), preserving the properties' schema declaration order
// unconditionally: a customData field lands wherever the schema declares
// it, exactly like every other field, never pinned last.
func structBody(structName string, definition Definition, definitions []Definition, transform TransformConfig, overrides []OverrideRow, placement PlacementPlan, context PackageContext) (string, error) {
	var buf strings.Builder
	fmt.Fprintf(&buf, "type %s struct {\n", structName)
	for _, property := range definition.Properties {
		mapping, err := qualifiedFieldMapping(definition, property, definitions, transform, overrides, placement, context)
		if err != nil {
			return "", err
		}
		buf.WriteString(formatFieldLine(mapping) + "\n")
	}
	buf.WriteString("}\n")
	return buf.String(), nil
}

// RenderDefinition renders one full definition: a struct for an object or
// root, or a type declaration, grouped const block and isValidX validator
// function for an enum.
//
// For an enum, RenderDefinition never emits that enum's init() function.
// Registration is assembled once per generated file, by whichever caller
// (RenderMessage for a message file, or the shared types-file assembler)
// collects every enum placed in that file and emits one combined init() at
// the end — exactly the shape boot_notification.go's own
// init() has today, registering two unrelated enums in one function
// (lines 129-132). If RenderDefinition emitted its own init() per enum, a
// file with two or more enums would either get two functions both named
// init() (a duplicate-declaration compile error) or would need the
// assembler to strip what RenderDefinition just added — strictly worse than
// never emitting it here. testdata/golden/enum_definition.golden and
// testdata/golden/enum_definition_initialism.golden both pin this: the
// registration line does not appear in either.
func RenderDefinition(definition Definition, ir IR, context PackageContext, transform TransformConfig, overrides []OverrideRow) (string, error) {
	switch definition.Kind {
	case DefinitionEnum:
		goName, err := TransformIdentifier(definition.Name, DefinitionEnum, transform)
		if err != nil {
			return "", err
		}
		return formatSource(buildEnumDeclaration(goName, definition.Values, transform))
	case DefinitionObject, DefinitionRoot:
		name := definition.Name
		if definition.Kind == DefinitionObject {
			transformed, err := TransformIdentifier(definition.Name, DefinitionObject, transform)
			if err != nil {
				return "", err
			}
			name = transformed
		}
		body, err := structBody(name, definition, ir.Definitions, transform, overrides, PlacementPlan{}, context)
		if err != nil {
			return "", err
		}
		return formatSource(body)
	default:
		return "", fmt.Errorf("render definition %s: unsupported kind %q", definition.Name, definition.Kind)
	}
}

// buildEnumTypeDecl renders one enum's bare type declaration.
func buildEnumTypeDecl(goName string) string {
	return fmt.Sprintf("type %s string\n\n", goName)
}

// buildEnumConstNames renders, in schema value order, the const name each
// raw value derives (the enum's own Go name followed by that value's
// derived words — see enumValueGoWords).
func buildEnumConstNames(goName string, values []string, transform TransformConfig) []string {
	names := make([]string, len(values))
	for i, value := range values {
		names[i] = goName + strings.Join(enumValueGoWords(value, transform), "")
	}
	return names
}

// buildEnumConstEntries renders one enum's const block lines (without the
// surrounding "const ( ... )").
func buildEnumConstEntries(goName string, values []string, transform TransformConfig) []string {
	names := buildEnumConstNames(goName, values, transform)
	lines := make([]string, len(values))
	for i, value := range values {
		lines[i] = fmt.Sprintf("%s %s = %q", names[i], goName, value)
	}
	return lines
}

// buildIsValidFunc renders one enum's isValidX validator function.
func buildIsValidFunc(goName string, values []string, transform TransformConfig) string {
	switchVar := escapeGoKeyword(strings.TrimSuffix(lowerCamel(goName), "Type"))
	constNames := buildEnumConstNames(goName, values, transform)
	var buf strings.Builder
	fmt.Fprintf(&buf, "func isValid%s(fl validator.FieldLevel) bool {\n", goName)
	fmt.Fprintf(&buf, "%s := %s(fl.Field().String())\n", switchVar, goName)
	fmt.Fprintf(&buf, "switch %s {\n", switchVar)
	fmt.Fprintf(&buf, "case %s:\n", strings.Join(constNames, ", "))
	buf.WriteString("return true\n")
	buf.WriteString("default:\n")
	buf.WriteString("return false\n")
	buf.WriteString("}\n")
	buf.WriteString("}\n")
	return buf.String()
}

// buildEnumDeclaration renders one enum's complete self-contained block:
// type declaration, its own const block and its own isValidX function —
// the shape a single RenderDefinition call, or one entry in the shared
// types file's per-name interleaving, needs. RenderMessage's own grouped
// layout (all enum type decls, then one combined const block, then all
// isValidX funcs) is assembled from the three builders above directly,
// not from this function.
func buildEnumDeclaration(goName string, values []string, transform TransformConfig) string {
	var buf strings.Builder
	buf.WriteString(buildEnumTypeDecl(goName))
	buf.WriteString("const (\n")
	for _, line := range buildEnumConstEntries(goName, values, transform) {
		buf.WriteString(line + "\n")
	}
	buf.WriteString(")\n\n")
	buf.WriteString(buildIsValidFunc(goName, values, transform))
	return buf.String()
}

// determineReturnsPointer implements the constructor return-kind rule for a
// generated composite: *T when reached through at least one pointer field
// (an optional, non-array $ref) anywhere in the full declared definition
// set; T when reached only by value or as a slice element. Message roots
// never call this — they always return *T — since a root is never itself a
// $ref target and this function would trivially answer false for one.
func determineReturnsPointer(name string, definitions []Definition) bool {
	for _, definition := range definitions {
		for _, property := range definition.Properties {
			if property.Type == "array" || property.Ref == "" {
				continue
			}
			if refTargetName(property.Ref) == name && !property.Required {
				return true
			}
		}
	}
	return false
}

// renderConstructor renders one New<T> constructor: a parameter, in schema
// declaration order, for every required field (spelled as the field it
// populates), and a struct literal setting exactly those fields.
func renderConstructor(structName string, definition Definition, definitions []Definition, transform TransformConfig, overrides []OverrideRow, placement PlacementPlan, context PackageContext, returnsPointer bool) (string, error) {
	var params []string
	var args []string
	for _, property := range definition.Properties {
		if !property.Required {
			continue
		}
		mapping, err := qualifiedFieldMapping(definition, property, definitions, transform, overrides, placement, context)
		if err != nil {
			return "", err
		}
		paramName := escapeGoKeyword(lowerCamel(mapping.FieldName))
		params = append(params, paramName+" "+mapping.GoType)
		args = append(args, mapping.FieldName+": "+paramName)
	}

	returnType := structName
	literal := structName + "{" + strings.Join(args, ", ") + "}"
	if returnsPointer {
		returnType = "*" + structName
		literal = "&" + literal
	}
	return fmt.Sprintf("func New%s(%s) %s {\nreturn %s\n}\n", structName, strings.Join(params, ", "), returnType, literal), nil
}

// localPlacedDefinitions returns, sorted by rendered Go name, every
// definition placed in pkg — used both by RenderMessage (pkg
// "message:<name>") and the shared types file assembler (pkg "types").
func localPlacedDefinitions(ir IR, placement PlacementPlan, transform TransformConfig, pkg string, kind DefinitionKind) ([]Definition, map[string]string, error) {
	var defs []Definition
	goNames := map[string]string{}
	for name, home := range placement.Home {
		if home != pkg {
			continue
		}
		definition := findDefinitionByName(ir.Definitions, name)
		if definition == nil {
			return nil, nil, fmt.Errorf("render: placed definition %s not found in the IR", name)
		}
		if definition.Kind != kind {
			continue
		}
		goName, err := TransformIdentifier(definition.Name, kind, transform)
		if err != nil {
			return nil, nil, err
		}
		defs = append(defs, *definition)
		goNames[definition.Name] = goName
	}
	sort.Slice(defs, func(i, j int) bool { return goNames[defs[i].Name] < goNames[defs[j].Name] })
	return defs, goNames, nil
}

// formatBannerDirection renders the manifest's compact direction value
// ("CS->CSMS") in the spaced form the banner comment uses ("CS -> CSMS"),
// matching boot_notification.go's own anatomy.
func formatBannerDirection(direction string) string {
	return strings.ReplaceAll(direction, "->", " -> ")
}

// RenderMessage renders one message file's complete body — banner, any
// enums and single-message composites placed locally, the request and
// response structs, the Feature scaffolding, every constructor and, when
// this file declares at least one enum, the combined validator
// registration init(). It never renders the package clause or import
// block (those need the manifest's GoModule/TypesPackage, which this
// function does not take — EmitGo assembles them around this body) and
// never the generated-file marker.
func RenderMessage(ir IR, message ManifestMessage, placement PlacementPlan, options EmitterOptions) (string, error) {
	transform := options.Transform
	overrides := options.Overrides

	requestRootName := message.Name + "Request"
	responseRootName := message.Name + "Response"
	requestRoot := findDefinitionByName(ir.Definitions, requestRootName)
	responseRoot := findDefinitionByName(ir.Definitions, responseRootName)
	if requestRoot == nil {
		return "", fmt.Errorf("render message %s: request root %s not found", message.Name, requestRootName)
	}
	if responseRoot == nil {
		return "", fmt.Errorf("render message %s: response root %s not found", message.Name, responseRootName)
	}

	pkg := "message:" + message.Name
	localEnums, enumGoNames, err := localPlacedDefinitions(ir, placement, transform, pkg, DefinitionEnum)
	if err != nil {
		return "", fmt.Errorf("render message %s: %w", message.Name, err)
	}
	localObjects, objectGoNames, err := localPlacedDefinitions(ir, placement, transform, pkg, DefinitionObject)
	if err != nil {
		return "", fmt.Errorf("render message %s: %w", message.Name, err)
	}

	context := PackageContext{}
	var buf strings.Builder

	fmt.Fprintf(&buf, "// -------------------- %s (%s) --------------------\n\n", splitToPhrase(message.Name), formatBannerDirection(message.Direction))
	fmt.Fprintf(&buf, "const %sFeatureName = %q\n\n", message.Name, message.Name)

	for _, def := range localEnums {
		buf.WriteString(buildEnumTypeDecl(enumGoNames[def.Name]))
	}
	if len(localEnums) > 0 {
		buf.WriteString("const (\n")
		for _, def := range localEnums {
			for _, line := range buildEnumConstEntries(enumGoNames[def.Name], def.Values, transform) {
				buf.WriteString(line + "\n")
			}
		}
		buf.WriteString(")\n\n")
	}
	for _, def := range localEnums {
		buf.WriteString(buildIsValidFunc(enumGoNames[def.Name], def.Values, transform))
		buf.WriteString("\n")
	}

	for _, def := range localObjects {
		body, err := structBody(objectGoNames[def.Name], def, ir.Definitions, transform, overrides, placement, context)
		if err != nil {
			return "", fmt.Errorf("render message %s: %w", message.Name, err)
		}
		buf.WriteString(body)
		buf.WriteString("\n")
	}

	requestBody, err := structBody(requestRootName, *requestRoot, ir.Definitions, transform, overrides, placement, context)
	if err != nil {
		return "", fmt.Errorf("render message %s: %w", message.Name, err)
	}
	buf.WriteString(requestBody)
	buf.WriteString("\n")
	responseBody, err := structBody(responseRootName, *responseRoot, ir.Definitions, transform, overrides, placement, context)
	if err != nil {
		return "", fmt.Errorf("render message %s: %w", message.Name, err)
	}
	buf.WriteString(responseBody)
	buf.WriteString("\n")

	fmt.Fprintf(&buf, "type %sFeature struct{}\n\n", message.Name)
	fmt.Fprintf(&buf, "func (f %sFeature) GetFeatureName() string {\nreturn %sFeatureName\n}\n\n", message.Name, message.Name)
	fmt.Fprintf(&buf, "func (f %sFeature) GetRequestType() reflect.Type {\nreturn reflect.TypeOf(%s{})\n}\n\n", message.Name, requestRootName)
	fmt.Fprintf(&buf, "func (f %sFeature) GetResponseType() reflect.Type {\nreturn reflect.TypeOf(%s{})\n}\n\n", message.Name, responseRootName)
	fmt.Fprintf(&buf, "func (r %s) GetFeatureName() string {\nreturn %sFeatureName\n}\n\n", requestRootName, message.Name)
	fmt.Fprintf(&buf, "func (c %s) GetFeatureName() string {\nreturn %sFeatureName\n}\n\n", responseRootName, message.Name)

	for _, def := range localObjects {
		returnsPointer := determineReturnsPointer(def.Name, ir.Definitions)
		ctor, err := renderConstructor(objectGoNames[def.Name], def, ir.Definitions, transform, overrides, placement, context, returnsPointer)
		if err != nil {
			return "", fmt.Errorf("render message %s: %w", message.Name, err)
		}
		buf.WriteString(ctor)
		buf.WriteString("\n")
	}
	requestCtor, err := renderConstructor(requestRootName, *requestRoot, ir.Definitions, transform, overrides, placement, context, true)
	if err != nil {
		return "", fmt.Errorf("render message %s: %w", message.Name, err)
	}
	buf.WriteString(requestCtor)
	buf.WriteString("\n")
	responseCtor, err := renderConstructor(responseRootName, *responseRoot, ir.Definitions, transform, overrides, placement, context, true)
	if err != nil {
		return "", fmt.Errorf("render message %s: %w", message.Name, err)
	}
	buf.WriteString(responseCtor)
	buf.WriteString("\n")

	if len(localEnums) > 0 {
		buf.WriteString("func init() {\n")
		for _, def := range localEnums {
			tag, err := validatorTagName(def.Name, transform)
			if err != nil {
				return "", fmt.Errorf("render message %s: %w", message.Name, err)
			}
			fmt.Fprintf(&buf, "_ = types.Validate.RegisterValidation(%q, isValid%s)\n", tag, enumGoNames[def.Name])
		}
		buf.WriteString("}\n")
	}

	return formatSource(buf.String())
}

// renderTypesFileBody renders the shared types file's body: every
// definition placed in "types", ordered by Go type name, an object
// interleaved immediately with its own constructor, an enum rendered as
// its own self-contained type/const/isValidX block; and, when at least one
// placed enum exists, one combined init() at the end registering all of
// them, matching the same one-init()-per-file assembly RenderMessage uses.
func renderTypesFileBody(ir IR, placement PlacementPlan, transform TransformConfig, overrides []OverrideRow) (string, error) {
	objects, objectGoNames, err := localPlacedDefinitions(ir, placement, transform, "types", DefinitionObject)
	if err != nil {
		return "", fmt.Errorf("render shared types file: %w", err)
	}
	enums, enumGoNames, err := localPlacedDefinitions(ir, placement, transform, "types", DefinitionEnum)
	if err != nil {
		return "", fmt.Errorf("render shared types file: %w", err)
	}

	type placedItem struct {
		definition Definition
		goName     string
		kind       DefinitionKind
	}
	var items []placedItem
	for _, def := range objects {
		items = append(items, placedItem{definition: def, goName: objectGoNames[def.Name], kind: DefinitionObject})
	}
	for _, def := range enums {
		items = append(items, placedItem{definition: def, goName: enumGoNames[def.Name], kind: DefinitionEnum})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].goName < items[j].goName })

	context := PackageContext{InTypes: true}
	var buf strings.Builder
	var registrations []placedItem

	for _, item := range items {
		switch item.kind {
		case DefinitionObject:
			body, err := structBody(item.goName, item.definition, ir.Definitions, transform, overrides, placement, context)
			if err != nil {
				return "", fmt.Errorf("render shared types file: %w", err)
			}
			buf.WriteString(body)
			buf.WriteString("\n")
			returnsPointer := determineReturnsPointer(item.definition.Name, ir.Definitions)
			ctor, err := renderConstructor(item.goName, item.definition, ir.Definitions, transform, overrides, placement, context, returnsPointer)
			if err != nil {
				return "", fmt.Errorf("render shared types file: %w", err)
			}
			buf.WriteString(ctor)
			buf.WriteString("\n")
		case DefinitionEnum:
			buf.WriteString(buildEnumDeclaration(item.goName, item.definition.Values, transform))
			buf.WriteString("\n")
			registrations = append(registrations, item)
		}
	}

	if len(registrations) > 0 {
		buf.WriteString("func init() {\n")
		for _, item := range registrations {
			tag, err := validatorTagName(item.definition.Name, transform)
			if err != nil {
				return "", fmt.Errorf("render shared types file: %w", err)
			}
			fmt.Fprintf(&buf, "_ = Validate.RegisterValidation(%q, isValid%s)\n", tag, item.goName)
		}
		buf.WriteString("}\n")
	}

	return buf.String(), nil
}

// assembleFile wraps a rendered body with the generated-file marker, the
// package clause and a computed import block, then formats the whole file
// once. Imports are computed from what the body actually references:
// forcedImports names the packages a file needs unconditionally (a message
// file always needs "reflect", since every message file emits Feature
// scaffolding; the shared types file needs nothing unconditionally);
// validator.v9 is added only when the body declares a validator; the types
// package is added only when the body carries a qualified types.
// reference and typesImportPath is non-empty (the shared types file is
// never given one, since it can never import itself).
func assembleFile(packageName string, body string, forcedImports []string, typesImportPath string) ([]byte, error) {
	var groups [][]string
	if len(forcedImports) > 0 {
		groups = append(groups, forcedImports)
	}
	if strings.Contains(body, "validator.FieldLevel") {
		groups = append(groups, []string{"gopkg.in/go-playground/validator.v9"})
	}
	if typesImportPath != "" && strings.Contains(body, "types.") {
		groups = append(groups, []string{typesImportPath})
	}

	var buf strings.Builder
	buf.WriteString("// Code generated by internal/codegen. DO NOT EDIT.\n\n")
	fmt.Fprintf(&buf, "package %s\n\n", packageName)
	if len(groups) > 0 {
		buf.WriteString("import (\n")
		for i, group := range groups {
			if i > 0 {
				buf.WriteString("\n")
			}
			for _, imp := range group {
				fmt.Fprintf(&buf, "\t%q\n", imp)
			}
		}
		buf.WriteString(")\n\n")
	}
	buf.WriteString(body)

	formatted, err := formatSource(buf.String())
	if err != nil {
		return nil, err
	}
	return []byte(formatted), nil
}

// generatedFile is one complete file the emitter has finished rendering: its
// destination path, the exact bytes to put there, and a description of what
// produced it, used to name both sides of a destination collision.
type generatedFile struct {
	path    string
	content []byte
	source  string
}

// checkDestinationPaths reports the first pair of rendered files that would be
// written to one place.
//
// This is the file-namespace sibling of the package-level identifier index.
// The two namespaces are genuinely independent: a message's file name is its
// message name reduced to lowercase words joined by underscores, and that
// reduction is not injective, so two messages can differ as identifiers —
// deriving different root types, a different Feature struct and a different
// FeatureName const, passing every identifier index — while reducing to one
// file name. NotifyEVChargingNeeds and NotifyEvChargingNeeds are exactly that
// pair. Without this check the run exits zero, writes the file twice, and keeps
// whichever message the manifest happens to list second; the tree looks
// complete and one message is simply missing from it.
//
// The shared types file is indexed with the message files rather than written
// past them. Its path is fixed — the types directory under the generated tree,
// holding types_gen.go — but nothing stops a manifest row from naming that
// place: a row's block is validated against the directories that exist under
// the generated tree, and the types directory is one of them. A message named
// TypesGen in block types reduces to that exact path, and since the shared file
// is rendered first it is the one that would disappear — the file every other
// generated file imports.
//
// Paths are compared case-insensitively, deliberately. Two paths differing only
// in case are two files on Linux and one file on macOS and Windows, whose
// filesystems are case-insensitive by default. A case-sensitive index would
// pass on a case-sensitive CI runner while the same inputs silently dropped a
// file on a developer's machine, which is the one outcome this check exists to
// prevent. Folding can only ever make the check stricter, and stricter is never
// wrong here: no rule in this generator deliberately produces two files whose
// paths differ only in case, so there is no legitimate output for the fold to
// reject. The failure names both original spellings, never the folded key,
// which appears in neither input.
func checkDestinationPaths(files []generatedFile) error {
	seen := make(map[string]generatedFile, len(files))
	for _, file := range files {
		key := strings.ToLower(filepath.Clean(file.path))
		prior, ok := seen[key]
		if !ok {
			seen[key] = file
			continue
		}
		if prior.path == file.path {
			return fmt.Errorf("%s and %s both emit %s", prior.source, file.source, file.path)
		}
		return fmt.Errorf("%s emits %s and %s emits %s, which are one file on a case-insensitive filesystem",
			prior.source, prior.path, file.source, file.path)
	}
	return nil
}

// renderTree renders every file a run produces, without writing anything.
// Placement and the identifier collision sweep run once, up front, over the
// whole IR — every message file's rendering shares the one PlacementPlan and
// relies on CheckEmissionCollisions having already hard-failed on any naming
// conflict. The shared types file is always produced, even when it places
// nothing. Once every file exists, checkDestinationPaths closes the one
// namespace the identifier indexes cannot see: where the files actually land.
func renderTree(ir IR, manifest Manifest, outDir string, options EmitterOptions) ([]generatedFile, error) {
	placement, err := ComputePlacement(ir)
	if err != nil {
		return nil, err
	}
	if err := CheckEmissionCollisions(ir, placement, options.Transform); err != nil {
		return nil, err
	}

	typesBody, err := renderTypesFileBody(ir, placement, options.Transform, options.Overrides)
	if err != nil {
		return nil, err
	}
	typesFile, err := assembleFile("types", typesBody, nil, "")
	if err != nil {
		return nil, fmt.Errorf("assemble shared types file: %w", err)
	}
	files := []generatedFile{{
		path:    filepath.Join(outDir, manifest.GoTree, "types", "types_gen.go"),
		content: typesFile,
		source:  "the shared types file",
	}}

	for _, message := range manifest.Messages {
		if !message.Emit {
			continue
		}
		body, err := RenderMessage(ir, message, placement, options)
		if err != nil {
			return nil, fmt.Errorf("render message %s: %w", message.Name, err)
		}
		file, err := assembleFile(message.Block, body, []string{"reflect"}, manifest.TypesImportPath())
		if err != nil {
			return nil, fmt.Errorf("assemble message %s: %w", message.Name, err)
		}
		files = append(files, generatedFile{
			path:    filepath.Join(outDir, manifest.GoTree, message.Block, snakeCase(message.Name)+".go"),
			content: file,
			source:  "message " + message.Name,
		})
	}
	if err := checkDestinationPaths(files); err != nil {
		return nil, err
	}
	return files, nil
}

// EmitGo renders a complete scratch tree — the shared types file and one file
// per emit:true message — and then writes it.
//
// The two phases are separate on purpose. A generated tree is only meaningful
// as one generation: every file in it came from the same rules and the same
// inputs, which is exactly what anyone reading a regeneration diff and a
// drift check comparing the tree against a rerun both depend on. Rendering and
// writing file by file gives that up the moment any later file fails to
// render — the tree is then part new generation and part old, a state no input
// produces and no rerun reproduces, and the error the operator sees names only
// the file that could not be rendered. Rendering everything first means a
// failure anywhere leaves the tree exactly as it was.
//
// Each file is then passed through the idempotent writer, which replaces a
// file whole or not at all and skips one whose bytes already match, so a
// no-op re-run touches nothing. The narrow residue the split does not cover is
// an I/O failure part-way through the write phase — a full disk, a revoked
// permission — which no amount of pre-rendering can prevent; every file
// written up to that point is still a complete, correctly rendered file of the
// new generation, and rerunning finishes the job.
func EmitGo(ir IR, manifest Manifest, outDir string, options EmitterOptions) error {
	files, err := renderTree(ir, manifest, outDir, options)
	if err != nil {
		return fmt.Errorf("emit go: %w", err)
	}
	for _, file := range files {
		if err := writeIfChanged(file.path, file.content); err != nil {
			return fmt.Errorf("emit go: %w", err)
		}
	}
	return nil
}
