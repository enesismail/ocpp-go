package main

import "testing"

// TestAcceptedTypeVocabularyReadsCleanly is the positive control every
// rejection test below is a minimal pair of: type_vocabulary.json declares
// each type this reader accepts, at each position it accepts it — the five
// property types, an array element, a root object, an object definition and a
// string enum definition — and every fixture in the tables below is a copy of
// it with exactly one type value changed. Without this control, a reader that
// rejected the whole keyword would pass every one of those tables.
func TestAcceptedTypeVocabularyReadsCleanly(t *testing.T) {
	document, err := ReadSchema(fixturePath("schemas", "type_vocabulary.json"))
	requireImplemented(t, "read of the accepted type vocabulary", err)

	if document.Root.Kind != DefinitionRoot {
		t.Fatalf("root object read as kind %q, want %q", document.Root.Kind, DefinitionRoot)
	}
	for name, want := range map[string]string{
		"value":   "string",
		"count":   "integer",
		"amount":  "number",
		"enabled": "boolean",
		"tags":    "array",
	} {
		property := findProperty(document.Root.Properties, name)
		if property == nil {
			t.Fatalf("property %s is missing from the root object", name)
		}
		if property.Type != want {
			t.Fatalf("property %s read as type %q, want %q", name, property.Type, want)
		}
	}

	tags := findProperty(document.Root.Properties, "tags")
	if tags.Items == nil {
		t.Fatalf("array property tags has no element schema: %#v", tags)
	}
	if tags.Items.Type != "string" {
		t.Fatalf("array element read as type %q, want %q", tags.Items.Type, "string")
	}

	status := findDefinition(document.Definitions, "StatusEnumType")
	if status == nil || status.Kind != DefinitionEnum {
		t.Fatalf("string enum definition was not read as an enum: %#v", status)
	}
	detail := findDefinition(document.Definitions, "DetailType")
	if detail == nil || detail.Kind != DefinitionObject {
		t.Fatalf("object definition was not read as an object: %#v", detail)
	}
	if note := findProperty(detail.Properties, "note"); note == nil || note.Type != "string" {
		t.Fatalf("property of an object definition was not read: %#v", note)
	}
}

// TestTypeOutsideItsPositionVocabularyIsRejected covers the type keyword's
// value, which decides how every later stage reads the value it describes. A
// reader that checks the keyword is a string but never what string it is
// accepts each fixture here and produces a description of something the
// schema does not declare — an object built from a root that says "array", a
// Go type built from an enum definition whose values were never strings, a
// property with no shape at all — and nothing in that output says so.
//
// Each row names the JSON pointer as well as the file and the value found:
// the pointer is what tells a wrong value apart from a wrong position, which
// is most of what this check catches (the corpus writes "object" and
// "string", just not everywhere).
func TestTypeOutsideItsPositionVocabularyIsRejected(t *testing.T) {
	tests := []struct {
		file string
		want []string
	}{
		// A root or definition that is not an object is still read property
		// by property, so the type it declares is the only thing saying it is
		// not one.
		{"type_root_array.json", []string{"type_root_array.json", "#/type", `"array"`}},
		{"type_object_definition_string.json", []string{"type_object_definition_string.json", "#/definitions/DetailType/type", `"string"`}},
		// "string" is what every enum definition in this corpus declares, and
		// enum values are read as JSON strings; "object" on the same
		// definition is a value list read as something it does not hold.
		{"type_enum_definition_object.json", []string{"type_enum_definition_object.json", "#/definitions/StatusEnumType/type", `"object"`}},
		// "object" is a type this reader accepts — on a root and on a named
		// definition, not on a property, where objects are reached by $ref and
		// an inline one has no members this reader would read.
		{"type_property_object.json", []string{"type_property_object.json", "#/properties/value/type", `"object"`}},
		{"type_items_object.json", []string{"type_items_object.json", "#/properties/tags/items/type", `"object"`}},
		// A value outside the vocabulary altogether: one that is no JSON
		// Schema type at all, and one that differs from an accepted value by a
		// single trailing space — the case a reader comparing anything looser
		// than the whole value lets through.
		{"type_property_unknown.json", []string{"type_property_unknown.json", "#/properties/value/type", `"text"`}},
		{"type_root_near_miss.json", []string{"type_root_near_miss.json", "#/type", `"object "`}},
	}
	for _, test := range tests {
		t.Run(test.file, func(t *testing.T) {
			_, err := ReadSchema(fixturePath("schemas", test.file))
			requireExpectedHardFailureContains(t, "type vocabulary check for "+test.file, err, test.want...)
		})
	}
}

// TestAbsentTypeAtAnObjectPositionIsRejected covers the type keyword's
// presence at the two positions this reader always reads as an object: a
// schema file's root, and a named definition. The table above covers a type
// that is present and wrong; a missing one is the same defect arriving
// silently. draft-06 constrains nothing about a schema that declares no type,
// so each fixture here describes a value that need not be an object (or, for
// the enum row, a value list that need not hold strings) — while a reader that
// only checks a type it happens to find reads the properties and required
// array anyway and indexes the result as object-shaped, emitting a Go type for
// a shape the schema never declared.
//
// Each fixture is type_vocabulary.json with exactly one type keyword deleted,
// so nothing but its absence can be what the read fails on, and the row
// asserts the expected type as well as the file and the pointer: a document
// rejected over a keyword it does not contain gives its reader nothing else to
// go on about what belongs there.
func TestAbsentTypeAtAnObjectPositionIsRejected(t *testing.T) {
	tests := []struct {
		file string
		want []string
	}{
		{"type_root_absent.json", []string{"type_root_absent.json", "#/type", `"object"`}},
		{"type_object_definition_absent.json", []string{"type_object_definition_absent.json", "#/definitions/DetailType/type", `"object"`}},
		// An enum definition is the one object position whose expected type is
		// not "object": its values are read as JSON strings, so "string" is
		// what the failure has to name.
		{"type_enum_definition_absent.json", []string{"type_enum_definition_absent.json", "#/definitions/StatusEnumType/type", `"string"`}},
	}
	for _, test := range tests {
		t.Run(test.file, func(t *testing.T) {
			_, err := ReadSchema(fixturePath("schemas", test.file))
			requireExpectedHardFailureContains(t, "type presence check for "+test.file, err, test.want...)
		})
	}
}

// TestNonCanonicalDraft06DialectURIIsRejected covers the $schema keyword's
// value as a whole. A dialect is named by a URI, so a value that merely
// contains "draft-06" somewhere inside it — here, a vendor URI carrying it as
// a path segment — names a dialect written by someone else, whose keywords
// need not mean what this reader reads them as. dialect_near_miss.json is
// otherwise identical to the fixture the control above reads cleanly, so the
// dialect is the only thing it can fail on, and the failure names the URI
// found rather than only the file: a document rejected for its dialect is
// usually one nobody expected to have a dialect problem.
func TestNonCanonicalDraft06DialectURIIsRejected(t *testing.T) {
	_, err := ReadSchema(fixturePath("schemas", "dialect_near_miss.json"))
	requireExpectedHardFailureContains(t, "schema dialect check", err,
		"dialect_near_miss.json", "https://schemas.example.invalid/json-schema/draft-06/schema#")
}

// TestMalformedRequiredNamesItsEnclosingObject covers where a malformed
// required array is reported, not whether it is. Every other hard failure in
// this reader names a JSON pointer, and the required array is read once per
// object: a message naming only the file leaves the reader of a document with
// dozens of definitions to find which one it came from. The two rows are the
// two pointer grammars an object can have — a schema file's root, and a named
// definition.
func TestMalformedRequiredNamesItsEnclosingObject(t *testing.T) {
	tests := []struct {
		file string
		want []string
	}{
		{"malformed_required_root.json", []string{"malformed_required_root.json", "#/required"}},
		{"malformed_required_definition.json", []string{"malformed_required_definition.json", "#/definitions/DetailType/required"}},
	}
	for _, test := range tests {
		t.Run(test.file, func(t *testing.T) {
			_, err := ReadSchema(fixturePath("schemas", test.file))
			requireExpectedHardFailureContains(t, "malformed required array check for "+test.file, err, test.want...)
		})
	}
}
