package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// SchemaDocument is the reader-level representation of one JSON Schema file.
// The final walk promotes Root and Definitions into the shared IR index.
//
// ID and Title are the recognized metadata keywords ($id, title) preserved
// as read; $schema is not stored on the document because it is validated
// once (draft-06 is the only accepted dialect) and then has no further use.
// description is deliberately not stored anywhere: the reader's
// ignored-keyword allowlist covers it by name, alongside javaType and
// comment — recognized metadata is exactly
// $schema/$id/title, nothing more. ID's trailing segment (after the last
// ":") is checked against the schema file's stem — its basename with the
// .json extension removed — and a mismatch is a hard failure; this is also
// how a message root definition (see Definition, kind root) gets its name:
// the file stem, not anything derived from $id.
//
// AdditionalProperties is the additionalProperties keyword declared on the
// document's own top-level object, in the boolean form this corpus uses, and
// nil when the keyword is absent. Root carries the identical value: this
// field is the document-level view of the keyword, where it is read, and
// Root's copy is the one that outlives the read, because the root definition
// — not this document — is what the walk promotes into the shared
// definitions index (see Definition, kind root). A reader that recorded the
// keyword only here would drop it on the way into the index.
type SchemaDocument struct {
	File                 string
	ID                   string
	Title                string
	AdditionalProperties *bool
	Root                 Definition
	Definitions          []Definition
}

// rootRecognized, definitionRecognized and propertyRecognized are the
// allowlists for the three levels a keyword can appear at. Each set includes
// both the keywords this reader captures into the IR and the keywords it
// tolerates without storing (javaType, comment, description — and, at the
// property level, default and additionalItems: this corpus writes
// `additionalItems: false` on every array property as a fixed companion to
// a non-tuple `items` schema, where draft-06 gives it no effect, and writes
// `default` on several enum definitions and boolean/string/integer
// properties purely as documentation of the wire-level fallback value,
// neither of which changes the Go shape a property maps to). Any key outside
// its level's set is a hard failure naming the file and the stray keyword.
var rootRecognized = map[string]bool{
	"$schema": true, "$id": true, "title": true,
	"type": true, "properties": true, "required": true, "definitions": true,
	"additionalProperties": true,
	"javaType":             true, "comment": true, "description": true,
}

var definitionRecognized = map[string]bool{
	"type": true, "properties": true, "required": true, "enum": true,
	"additionalProperties": true, "$ref": true,
	"javaType": true, "comment": true, "description": true, "default": true,
}

var propertyRecognized = map[string]bool{
	"type": true, "format": true, "$ref": true,
	"maxLength": true, "minLength": true,
	"minimum": true, "maximum": true,
	"exclusiveMinimum": true, "exclusiveMaximum": true,
	"minItems": true, "maxItems": true, "pattern": true,
	"additionalProperties": true, "items": true,
	"javaType": true, "comment": true, "description": true,
	"default": true, "additionalItems": true,
}

// rootPointer is the JSON pointer of a schema file's own top-level object —
// the location a document-level keyword such as $id or title sits on, and
// the prefix every other pointer in the file extends.
const rootPointer = "#"

// draft06Dialect is the one $schema value this reader accepts, matched in
// full. A dialect is named by a URI, and a URI is not a substring: a value
// that merely mentions draft-06 somewhere inside it — a vendor URI carrying
// it in a path segment, say — names a dialect written by someone else, whose
// keywords may mean something other than what is read here. Matching on the
// whole value is what keeps "this document is draft-06" a fact rather than a
// coincidence of spelling.
const draft06Dialect = "http://json-schema.org/draft-06/schema#"

// rootTypes, objectDefinitionTypes, enumDefinitionTypes and propertyTypes are
// the values the type keyword may take at each of the four positions it can
// appear at. A value outside its position's set is a hard failure naming the
// file, the JSON pointer and the value found — never a value carried into the
// IR unread, because type is the keyword every later stage reads a value's
// shape from, and a wrong one there is not visible in the output it produces.
//
// Each set is exactly what that position can be read as:
//
//   - A schema file's root object and every named object definition describe
//     an object, and are read as one: their properties and required arrays
//     only mean anything on an object. A root or definition declaring itself
//     an array — or a scalar, or a near-miss spelling of "object" — would
//     still be read property by property and would still produce a Go type,
//     describing something the schema does not.
//   - An enum definition's values are read as JSON strings, so "string" is
//     the only type it can carry without its value list being read as
//     something other than what it holds.
//   - A property, and an array's element schema, either carries one of the
//     scalar types or is an array of them. Objects are always reached by a
//     $ref to a named definition and never written inline, so "object" is not
//     accepted here: this reader recognizes no properties keyword at property
//     level, so an inline object would be read as a value with no members at
//     all and mapped to a Go type nothing in the schema describes.
//
// Whether the keyword may be absent differs by position, and follows from
// what its absence would leave the reader doing:
//
//   - On a root object and on every named definition the keyword is
//     REQUIRED, and an absent one is a hard failure naming the file, the JSON
//     pointer and the type that position expects. draft-06 places no
//     constraint on a schema that declares no type, so a root or definition
//     without one describes a value that need not be an object at all — yet
//     this reader would still read its properties and required array and
//     still promote it into the index as an object-shaped entry, producing a
//     Go type for a shape the schema never claimed. Requiring the keyword
//     costs nothing against the vendored corpus, where every root, every
//     object definition and every enum definition declares one explicitly.
//   - On a property, and on an array's element schema, the keyword is
//     OPTIONAL, because its absence there is a shape the corpus does write
//     and this reader does describe: either a $ref, whose target carries the
//     type, or the explicit untyped property. Absence is handled where the
//     keyword is read, and only a present value reaches the set above.
var (
	rootTypes             = []string{"object"}
	objectDefinitionTypes = []string{"object"}
	enumDefinitionTypes   = []string{"string"}
	propertyTypes         = []string{"array", "boolean", "integer", "number", "string"}
)

// rawObject is a JSON object decoded with its member order preserved, the
// only property encoding/json's map-based decoding cannot recover on its
// own (see decodeOrderedObject).
type rawObject struct {
	keys   []string
	values map[string]json.RawMessage
}

// definitionInfo is the first-pass reading of one named definition: enough
// to resolve a $ref against it (its kind, an enum's value list, its own
// additionalProperties) before any property list is built. Property
// resolution is a second pass over the same file because a property that
// $refs an enum needs the enum's values already known, and the corpus does
// not guarantee a definition is declared before the definitions that use
// it.
type definitionInfo struct {
	kind                 DefinitionKind
	values               []string
	additionalProperties *bool
}

// ReadSchema parses one schema document and is the entry point for local,
// draft-aware schema reading.
//
// Every $ref must name a definition the same file declares: this corpus has
// no external or remote references, so a target that is missing from the
// file's own definitions block cannot be resolved from anywhere else. An
// unresolvable target is a hard failure naming the file, the JSON pointer of
// the property that carries the $ref, and the missing target — never a
// skipped property, which would drop a field from the generated type with
// nothing in the output to say so.
func ReadSchema(path string) (SchemaDocument, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SchemaDocument{}, fmt.Errorf("read schema %s: %v", path, err)
	}

	root, err := decodeOrderedDocument(data)
	if err != nil {
		return SchemaDocument{}, fmt.Errorf("read schema %s: %v", path, err)
	}

	for _, key := range root.keys {
		if !rootRecognized[key] {
			return SchemaDocument{}, fmt.Errorf("read schema %s: unrecognized schema keyword %q at the document root", path, key)
		}
	}

	schemaKeyword, hasSchemaKeyword, err := stringValue(root.values, "$schema", path, rootPointer)
	if err != nil {
		return SchemaDocument{}, err
	}
	if !hasSchemaKeyword {
		return SchemaDocument{}, fmt.Errorf("read schema %s: no dialect is declared at %s/$schema (only %s is supported)", path, rootPointer, draft06Dialect)
	}
	if schemaKeyword != draft06Dialect {
		return SchemaDocument{}, fmt.Errorf("read schema %s: unsupported schema dialect %q at %s/$schema (only %s is supported)", path, schemaKeyword, rootPointer, draft06Dialect)
	}

	const idPrefix = "urn:OCPP:Cp:2:2020:3:"
	id, _, err := stringValue(root.values, "$id", path, rootPointer)
	if err != nil {
		return SchemaDocument{}, err
	}
	if !strings.HasPrefix(id, idPrefix) {
		return SchemaDocument{}, fmt.Errorf("read schema %s: schema $id %q is not a well-formed %s identifier", path, id, idPrefix)
	}
	stem := strings.TrimSuffix(filepath.Base(path), ".json")
	trailing := strings.TrimPrefix(id, idPrefix)
	if trailing != stem {
		return SchemaDocument{}, fmt.Errorf("read schema %s: schema $id trailing segment %q does not match the file stem %q", path, trailing, stem)
	}
	title, _, err := stringValue(root.values, "title", path, rootPointer)
	if err != nil {
		return SchemaDocument{}, err
	}

	rootType, hasRootType, err := stringValue(root.values, "type", path, rootPointer)
	if err != nil {
		return SchemaDocument{}, err
	}
	if err := requireType(path, rootPointer, rootType, hasRootType, rootTypes); err != nil {
		return SchemaDocument{}, err
	}

	rootAdditionalProperties, err := boolValue(root.values, "additionalProperties")
	if err != nil {
		return SchemaDocument{}, fmt.Errorf("read schema %s: additionalProperties at the document root: %v", path, err)
	}

	definitionsRaw, err := rawObjectField(root, "definitions")
	if err != nil {
		return SchemaDocument{}, fmt.Errorf("read schema %s: malformed definitions object: %v", path, err)
	}

	// First pass: classify every named definition (object vs. enum, its own
	// additionalProperties, an enum's resolved values) without resolving any
	// property's $ref yet, so the second pass can look any of them up
	// regardless of declaration order within the file.
	infos := make(map[string]definitionInfo, len(definitionsRaw.keys))
	entries := make(map[string]rawObject, len(definitionsRaw.keys))
	for _, name := range definitionsRaw.keys {
		entry, err := decodeOrderedObject(definitionsRaw.values[name])
		if err != nil {
			return SchemaDocument{}, fmt.Errorf("read schema %s: malformed definition %s: %v", path, name, err)
		}

		if len(entry.keys) == 1 && entry.keys[0] == "$ref" {
			ref, _, err := stringValue(entry.values, "$ref", path, fmt.Sprintf("#/definitions/%s", name))
			if err != nil {
				return SchemaDocument{}, err
			}
			return SchemaDocument{}, fmt.Errorf("read schema %s: definition %s at #/definitions/%s is a direct reference indirection to %s, which is not permitted", path, name, name, ref)
		}
		for _, key := range entry.keys {
			if !definitionRecognized[key] {
				return SchemaDocument{}, fmt.Errorf("read schema %s: unrecognized schema keyword %q on definition %s", path, key, name)
			}
		}

		entries[name] = entry
		info := definitionInfo{kind: DefinitionObject}
		if _, isEnum := entry.values["enum"]; isEnum {
			values, err := parseEnumValues(path, name, entry)
			if err != nil {
				return SchemaDocument{}, err
			}
			info.kind = DefinitionEnum
			info.values = values
		}

		// The definition's own kind decides which type it must declare, so
		// this check waits until the enum keyword above has classified it.
		definitionPointer := fmt.Sprintf("#/definitions/%s", name)
		definitionType, hasDefinitionType, err := stringValue(entry.values, "type", path, definitionPointer)
		if err != nil {
			return SchemaDocument{}, err
		}
		accepted := objectDefinitionTypes
		if info.kind == DefinitionEnum {
			accepted = enumDefinitionTypes
		}
		if err := requireType(path, definitionPointer, definitionType, hasDefinitionType, accepted); err != nil {
			return SchemaDocument{}, err
		}

		additionalProperties, err := boolValue(entry.values, "additionalProperties")
		if err != nil {
			return SchemaDocument{}, fmt.Errorf("read schema %s: additionalProperties on definition %s: %v", path, name, err)
		}
		info.additionalProperties = additionalProperties
		infos[name] = info
	}

	// Second pass: build each object definition's property list, now that
	// every definition in the file is resolvable by name.
	definitions := make([]Definition, 0, len(definitionsRaw.keys))
	for _, name := range definitionsRaw.keys {
		info := infos[name]
		definition := Definition{
			Name:                 name,
			Kind:                 info.kind,
			AdditionalProperties: info.additionalProperties,
			Reserved:             name == "CustomDataType",
			Occurrences:          1,
			Files:                []string{filepath.Base(path)},
		}
		if info.kind == DefinitionEnum {
			definition.Values = info.values
		} else {
			properties, err := parseProperties(path, entries[name], fmt.Sprintf("#/definitions/%s", name), infos)
			if err != nil {
				return SchemaDocument{}, err
			}
			definition.Properties = properties
		}
		definitions = append(definitions, definition)
	}

	rootProperties, err := parseProperties(path, root, rootPointer, infos)
	if err != nil {
		return SchemaDocument{}, err
	}

	rootDefinition := Definition{
		Name:                 stem,
		Kind:                 DefinitionRoot,
		Properties:           rootProperties,
		AdditionalProperties: rootAdditionalProperties,
		Occurrences:          1,
		Files:                []string{filepath.Base(path)},
	}

	return SchemaDocument{
		File:                 filepath.Base(path),
		ID:                   id,
		Title:                title,
		AdditionalProperties: rootAdditionalProperties,
		Root:                 rootDefinition,
		Definitions:          definitions,
	}, nil
}

// WalkSchema reads and recursively resolves one schema file into a document,
// adding cycle detection on top of ReadSchema's own checks. A cycle can only
// ever run through $refs local to this one file — the corpus has no
// cross-file references — so checking the file's own root plus its
// definitions together is sufficient; WalkSchemas relies on exactly this
// property to skip a second, corpus-wide cycle pass.
func WalkSchema(path string) (SchemaDocument, error) {
	document, err := ReadSchema(path)
	if err != nil {
		return SchemaDocument{}, err
	}
	all := append([]Definition{document.Root}, document.Definitions...)
	if err := DetectCycles(all); err != nil {
		return SchemaDocument{}, fmt.Errorf("walk schema %s: %w", path, err)
	}
	return document, nil
}

// decodeOrderedDocument parses data as a whole JSON document: one object,
// and nothing after it but whitespace. Anything following the root object's
// closing brace is a hard failure rather than content read up to that brace
// and then dropped — a second document appended to a schema file, or a
// fragment an edit left behind, would otherwise be read as a well-formed
// schema whose extra content nothing in the output mentions.
func decodeOrderedDocument(data []byte) (rawObject, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	object, err := decodeOrderedObjectFrom(decoder)
	if err != nil {
		return rawObject{}, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return rawObject{}, fmt.Errorf("malformed content after the root object: %v", err)
		}
		return rawObject{}, fmt.Errorf("unexpected content after the root object")
	}
	return object, nil
}

// decodeOrderedObject parses data as a JSON object while recovering the
// declaration order encoding/json's map-based decoding loses. It walks the
// byte stream token by token rather than unmarshaling into a map. data is
// one value taken from an enclosing document — a nested object already
// delimited by the decoder that produced it — so, unlike a whole document,
// it has no trailing content to check for.
func decodeOrderedObject(data []byte) (rawObject, error) {
	return decodeOrderedObjectFrom(json.NewDecoder(bytes.NewReader(data)))
}

// decodeOrderedObjectFrom reads exactly one JSON object from decoder,
// leaving whatever follows it unread for the caller to judge.
func decodeOrderedObjectFrom(decoder *json.Decoder) (rawObject, error) {
	token, err := decoder.Token()
	if err != nil {
		return rawObject{}, err
	}
	delim, ok := token.(json.Delim)
	if !ok || delim != '{' {
		return rawObject{}, fmt.Errorf("expected a JSON object, got %v", token)
	}

	object := rawObject{values: make(map[string]json.RawMessage)}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return rawObject{}, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return rawObject{}, fmt.Errorf("expected a string object key, got %v", keyToken)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return rawObject{}, err
		}
		object.keys = append(object.keys, key)
		object.values[key] = value
	}
	if _, err := decoder.Token(); err != nil { // the closing '}'
		return rawObject{}, err
	}
	return object, nil
}

// rawObjectField reads a nested object field of parent, defaulting to an
// empty object when the field is absent.
func rawObjectField(parent rawObject, key string) (rawObject, error) {
	raw, ok := parent.values[key]
	if !ok {
		return rawObject{values: map[string]json.RawMessage{}}, nil
	}
	return decodeOrderedObject(raw)
}

// stringValue reads a string-valued keyword, telling an absent key apart
// from a present one whose value is not a JSON string. The second case is an
// error, never a reported absence: reading `"type": ["string", "null"]` as
// "no type declared" would turn a property the schema does describe into the
// corpus's untyped shape and drop every constraint it carries, and the same
// swallow on format or pattern loses a constraint outright — in both cases
// with nothing in the output to say so. location is the JSON pointer of the
// object the key sits on, so the failure names the exact place in the file.
func stringValue(values map[string]json.RawMessage, key, path, location string) (string, bool, error) {
	raw, ok := values[key]
	if !ok {
		return "", false, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false, fmt.Errorf("read schema %s: %s/%s is not a JSON string: %v", path, location, key, err)
	}
	return s, true, nil
}

func boolValue(values map[string]json.RawMessage, key string) (*bool, error) {
	raw, ok := values[key]
	if !ok {
		return nil, nil
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

func intValue(values map[string]json.RawMessage, key string) (*int, error) {
	raw, ok := values[key]
	if !ok {
		return nil, nil
	}
	var v int
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func floatValue(values map[string]json.RawMessage, key string) (*float64, error) {
	raw, ok := values[key]
	if !ok {
		return nil, nil
	}
	var v float64
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// requireType validates the type keyword at a position that must declare one
// — a schema file's root object, and every named definition. present is
// stringValue's report of whether the keyword was there at all, so an absent
// keyword and one whose value is not a JSON string stay distinguishable: the
// second has already failed by the time this is called.
//
// An absent keyword is a hard failure, not a value defaulted to the position's
// only accepted type. Absence is not a weaker spelling of the expected value:
// draft-06 constrains nothing about a schema that declares no type, so the
// document says this value may be anything, while this reader would go on to
// read its properties and required array and index it as an object regardless
// — emitting a Go type for a shape the schema never declared, with nothing in
// the output to say the declaration was missing. The failure names the file,
// the JSON pointer and the type expected there, because a document rejected
// for a keyword it does not contain gives its reader no other way to find the
// place to add one.
func requireType(path, objectPointer, value string, present bool, accepted []string) error {
	if !present {
		return fmt.Errorf("read schema %s: no type is declared at %s/type (expected: %s)",
			path, objectPointer, quotedList(accepted))
	}
	return checkType(path, objectPointer, value, accepted)
}

// checkType validates one type keyword's value against the set of values its
// position accepts, naming the file, the enclosing object's JSON pointer, the
// value found and what that position does accept — the last of these because
// a value rejected as "not accepted" leaves the reader of a schema file no
// way to tell a typo from a shape this reader genuinely does not describe.
func checkType(path, objectPointer, value string, accepted []string) error {
	for _, candidate := range accepted {
		if value == candidate {
			return nil
		}
	}
	return fmt.Errorf("read schema %s: %s/type is %q, which is not a type accepted there (accepted: %s)",
		path, objectPointer, value, quotedList(accepted))
}

// quotedList renders a position's accepted type values for a failure message,
// quoted so a value whose only defect is surrounding whitespace is visible as
// one.
func quotedList(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, fmt.Sprintf("%q", value))
	}
	return strings.Join(quoted, ", ")
}

func parseEnumValues(path, name string, entry rawObject) ([]string, error) {
	raw, ok := entry.values["enum"]
	if ok {
		var values []string
		if err := json.Unmarshal(raw, &values); err != nil {
			return nil, fmt.Errorf("read schema %s: enum definition %s has malformed enum values: %v", path, name, err)
		}
		if len(values) > 0 {
			return values, nil
		}
	}
	return nil, fmt.Errorf("read schema %s: enum definition %s has no enum values", path, name)
}

// requiredSet reads the required array declared directly on obj, scoped to
// obj alone: a nested definition's own required array never marks a
// property on a different definition, even one reached by a property of the
// same name.
func requiredSet(obj rawObject) (map[string]bool, error) {
	result := map[string]bool{}
	raw, ok := obj.values["required"]
	if !ok {
		return result, nil
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil {
		return nil, err
	}
	for _, name := range names {
		result[name] = true
	}
	return result, nil
}

// parseProperties builds the property list declared directly on obj (a root
// object or a named definition), in schema declaration order. objectPointer
// is the JSON pointer of obj itself — "#" for a root object,
// "#/definitions/<Name>" for a named definition — and every pointer this
// function reports, its own and each property's, extends it, so a failure
// names the place in the file the same way whichever object it came from.
func parseProperties(path string, obj rawObject, objectPointer string, infos map[string]definitionInfo) ([]Property, error) {
	required, err := requiredSet(obj)
	if err != nil {
		return nil, fmt.Errorf("read schema %s: malformed required array at %s/required: %v", path, objectPointer, err)
	}
	propertiesRaw, ok := obj.values["properties"]
	if !ok {
		return nil, nil
	}
	properties, err := decodeOrderedObject(propertiesRaw)
	if err != nil {
		return nil, fmt.Errorf("read schema %s: malformed properties object: %v", path, err)
	}

	result := make([]Property, 0, len(properties.keys))
	for _, name := range properties.keys {
		valueObject, err := decodeOrderedObject(properties.values[name])
		if err != nil {
			return nil, fmt.Errorf("read schema %s: malformed property %s: %v", path, name, err)
		}
		pointer := objectPointer + "/properties/" + name
		property, err := parseProperty(path, name, pointer, valueObject, required[name], infos)
		if err != nil {
			return nil, err
		}
		result = append(result, property)
	}
	return result, nil
}

// parseProperty reads one property (or array item) value object. name is
// empty when obj describes an array's items rather than a named property.
func parseProperty(path, name, pointer string, obj rawObject, required bool, infos map[string]definitionInfo) (Property, error) {
	for _, key := range obj.keys {
		if !propertyRecognized[key] {
			return Property{}, fmt.Errorf("read schema %s: unrecognized schema keyword at %s/%s", path, pointer, key)
		}
	}

	property := Property{Name: name, Pointer: pointer, Required: required}

	if refRaw, ok := obj.values["$ref"]; ok {
		var ref string
		if err := json.Unmarshal(refRaw, &ref); err != nil {
			return Property{}, fmt.Errorf("read schema %s: %s has a malformed $ref: %v", path, pointer, err)
		}
		target, err := resolveLocalRef(path, pointer, ref, infos)
		if err != nil {
			return Property{}, err
		}
		property.Ref = ref
		info := infos[target]
		if info.kind == DefinitionEnum {
			property.Enum = info.values
			property.EnumDefinition = target
		}
		property.AdditionalProperties = info.additionalProperties
		// A property resolving through a $ref never carries its own sibling
		// constraint keywords in this corpus (verified over the vendored
		// v201 corpus, not merely assumed): what its target's own schema
		// declares is what a per-property read surfaces here.
		return property, nil
	}

	typ, hasType, err := stringValue(obj.values, "type", path, pointer)
	if err != nil {
		return Property{}, err
	}
	if !hasType {
		// Neither a $ref nor a type keyword: this is the corpus's explicit
		// untyped shape (DataTransferRequest/Response.data), not an
		// object that merely carries no other constraints — a property that
		// declares a type but no further constraints (e.g. a bare {"type":
		// "string"}) falls through below instead, with every constraint
		// field left at its zero value.
		property.Untyped = true
		return property, nil
	}
	if err := checkType(path, pointer, typ, propertyTypes); err != nil {
		return Property{}, err
	}
	property.Type = typ
	format, hasFormat, err := stringValue(obj.values, "format", path, pointer)
	if err != nil {
		return Property{}, err
	}
	if hasFormat {
		property.Format = format
	}

	maxLength, err := intValue(obj.values, "maxLength")
	if err != nil {
		return Property{}, fmt.Errorf("read schema %s: %s maxLength: %v", path, pointer, err)
	}
	property.MaxLength = maxLength
	minLength, err := intValue(obj.values, "minLength")
	if err != nil {
		return Property{}, fmt.Errorf("read schema %s: %s minLength: %v", path, pointer, err)
	}
	property.MinLength = minLength
	minItems, err := intValue(obj.values, "minItems")
	if err != nil {
		return Property{}, fmt.Errorf("read schema %s: %s minItems: %v", path, pointer, err)
	}
	property.MinItems = minItems
	maxItems, err := intValue(obj.values, "maxItems")
	if err != nil {
		return Property{}, fmt.Errorf("read schema %s: %s maxItems: %v", path, pointer, err)
	}
	property.MaxItems = maxItems

	minimum, err := floatValue(obj.values, "minimum")
	if err != nil {
		return Property{}, fmt.Errorf("read schema %s: %s minimum: %v", path, pointer, err)
	}
	property.Minimum = minimum
	maximum, err := floatValue(obj.values, "maximum")
	if err != nil {
		return Property{}, fmt.Errorf("read schema %s: %s maximum: %v", path, pointer, err)
	}
	property.Maximum = maximum

	exclusiveMinimum, err := floatValue(obj.values, "exclusiveMinimum")
	if err != nil {
		return Property{}, fmt.Errorf("read schema %s: %s exclusiveMinimum: %v", path, pointer, err)
	}
	if exclusiveMinimum != nil {
		property.ExclusiveMinimum = &Bound{Number: exclusiveMinimum}
	}
	exclusiveMaximum, err := floatValue(obj.values, "exclusiveMaximum")
	if err != nil {
		return Property{}, fmt.Errorf("read schema %s: %s exclusiveMaximum: %v", path, pointer, err)
	}
	if exclusiveMaximum != nil {
		property.ExclusiveMaximum = &Bound{Number: exclusiveMaximum}
	}

	pattern, hasPattern, err := stringValue(obj.values, "pattern", path, pointer)
	if err != nil {
		return Property{}, err
	}
	if hasPattern {
		property.Pattern = pattern
	}

	additionalProperties, err := boolValue(obj.values, "additionalProperties")
	if err != nil {
		return Property{}, fmt.Errorf("read schema %s: %s additionalProperties: %v", path, pointer, err)
	}
	property.AdditionalProperties = additionalProperties

	if itemsRaw, ok := obj.values["items"]; ok {
		itemsObject, err := decodeOrderedObject(itemsRaw)
		if err != nil {
			return Property{}, fmt.Errorf("read schema %s: %s/items: %v", path, pointer, err)
		}
		items, err := parseProperty(path, "", pointer+"/items", itemsObject, false, infos)
		if err != nil {
			return Property{}, err
		}
		property.Items = &items
	}

	return property, nil
}

// resolveLocalRef validates that ref is a local reference into infos (the
// file's own definitions) and returns the target definition's name.
func resolveLocalRef(path, pointer, ref string, infos map[string]definitionInfo) (string, error) {
	const prefix = "#/definitions/"
	if !strings.HasPrefix(ref, prefix) {
		return "", fmt.Errorf("read schema %s: %s has a non-local $ref %s", path, pointer, ref)
	}
	name := strings.TrimPrefix(ref, prefix)
	if name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("read schema %s: %s has a non-local $ref %s", path, pointer, ref)
	}
	if _, ok := infos[name]; !ok {
		return "", fmt.Errorf("read schema %s: %s references undefined local target %s", path, pointer, ref)
	}
	return name, nil
}
