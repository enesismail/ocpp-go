package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestSchemaWalkResolvesLocalReferences(t *testing.T) {
	path := fixturePath(t, "schemas", "draft06_response.json")
	document, err := walkSchema(path)
	if err != nil {
		t.Fatalf("schema walker is not implemented for %s: %v", path, err)
	}
	if len(document.Properties) != 5 {
		t.Fatalf("schema walk found %d properties, want 5 (status, previousStatus, modem, modem's iccid, timestamp): %#v", len(document.Properties), document.Properties)
	}

	modem := findSchemaProperty(document.Properties, "#/properties/modem")
	if modem == nil || modem.Type != "object" {
		t.Fatalf("object-typed property was not recorded as its own row: %#v", modem)
	}
	if modem.EnclosingDefinition != "" {
		t.Fatalf("root-level modem property incorrectly carries an enclosing definition: %#v", modem)
	}

	status := findSchemaProperty(document.Properties, "#/properties/status")
	if status == nil || status.Type != "string" {
		t.Fatalf("local $ref was not resolved to a string property: %#v", status)
	}
	// The pointer is the property's own OCCURRENCE — where "status" is
	// textually declared — never the $ref target's own pointer.
	if status.EnclosingDefinition != "" {
		t.Fatalf("root-level property incorrectly carries an enclosing definition: %#v", status)
	}

	previous := findSchemaProperty(document.Properties, "#/properties/previousStatus")
	if previous == nil || previous.Type != "string" {
		t.Fatalf("second $ref occurrence of the same definition was not resolved: %#v", previous)
	}
	if status.Pointer == previous.Pointer {
		t.Fatalf("two distinct $ref occurrences of the same definition collapsed onto one pointer: %s", status.Pointer)
	}

	// A property reached by recursing into a $ref'd object definition's own
	// "properties" is textually declared inside that definition, so its
	// pointer is rooted there and it carries the enclosing definition name —
	// the source for the (definition name, property name) dedup identity.
	iccid := findSchemaProperty(document.Properties, "#/definitions/ModemType/properties/iccid")
	if iccid == nil || iccid.Type != "string" {
		t.Fatalf("property nested inside a $ref'd object definition was not walked: %#v", iccid)
	}
	if iccid.EnclosingDefinition != "ModemType" {
		t.Fatalf("nested property's enclosing definition was not recorded: %#v", iccid)
	}

	timestamp := findSchemaProperty(document.Properties, "#/properties/timestamp")
	if timestamp == nil || timestamp.Format == nil || *timestamp.Format != "date-time" {
		t.Fatalf("date-time format was not captured: %#v", timestamp)
	}
}

func TestSchemaWalksDeepInlineProperties(t *testing.T) {
	tests := []struct {
		name         string
		valuePointer string
		valueType    string
		minimum      float64
	}{
		// MeterValues-style: array of object, each containing its own array
		// (not a flat array-of-array), matching the real 1.6 corpus shape.
		{name: "draft04_request.json", valuePointer: "#/properties/payload/properties/readings/items/properties/values/items/properties/value", valueType: "integer", minimum: 1},
		{name: "draft06_inline.json", valuePointer: "#/properties/nested/properties/items/items/properties/value", valueType: "integer", minimum: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			document, err := walkSchema(fixturePath(t, "schemas", test.name))
			if err != nil {
				t.Fatalf("inline schema walk is not implemented for %s: %v", test.name, err)
			}
			property := findSchemaProperty(document.Properties, test.valuePointer)
			if property == nil || property.Type != test.valueType {
				t.Fatalf("nested property=%#v, want pointer=%q type=%q", property, test.valuePointer, test.valueType)
			}
			if property.Constraints.Minimum == nil || *property.Constraints.Minimum != test.minimum {
				t.Fatalf("nested property constraints=%#v, want minimum=%v", property.Constraints, test.minimum)
			}
		})
	}
}

// TestSchemaWalkCapturesExclusiveBoundsPerDialect covers both forms
// ExclusiveBound can carry: draft-04 models exclusiveMinimum/exclusiveMaximum
// as a boolean modifier paired with minimum/maximum ("the minimum applies,
// exclusively"), while draft-06+ redefines them as a standalone number that
// replaces minimum/maximum outright. Exactly one of ExclusiveBound.Bool /
// .Number is populated, matching whichever form the source document used.
func TestSchemaWalkCapturesExclusiveBoundsPerDialect(t *testing.T) {
	t.Run("draft-04 boolean form", func(t *testing.T) {
		document, err := walkSchema(fixturePath(t, "schemas", "draft04_request.json"))
		if err != nil {
			t.Fatalf("schema walk is not implemented: %v", err)
		}
		pointer := "#/properties/payload/properties/readings/items/properties/values/items/properties/value"
		property := findSchemaProperty(document.Properties, pointer)
		if property == nil {
			t.Fatalf("draft-04 nested value property was not walked: %#v", document.Properties)
		}
		bound := property.Constraints.ExclusiveMinimum
		if bound == nil || bound.Bool == nil || !*bound.Bool {
			t.Fatalf("draft-04 boolean exclusiveMinimum form was not captured: %#v", bound)
		}
		if bound.Number != nil {
			t.Fatalf("draft-04 boolean form incorrectly carries a numeric bound too: %#v", bound)
		}
		if property.Constraints.Minimum == nil || *property.Constraints.Minimum != 1 {
			t.Fatalf("draft-04 boolean exclusiveMinimum must pair with its own minimum value: %#v", property.Constraints.Minimum)
		}
	})

	t.Run("draft-06 numeric form", func(t *testing.T) {
		document, err := walkSchema(fixturePath(t, "schemas", "draft06_inline.json"))
		if err != nil {
			t.Fatalf("schema walk is not implemented: %v", err)
		}
		property := findSchemaProperty(document.Properties, "#/properties/nested/properties/count")
		if property == nil {
			t.Fatalf("draft-06 count property was not walked: %#v", document.Properties)
		}
		min := property.Constraints.ExclusiveMinimum
		if min == nil || min.Number == nil || *min.Number != 5 {
			t.Fatalf("draft-06 standalone numeric exclusiveMinimum form was not captured: %#v", min)
		}
		if min.Bool != nil {
			t.Fatalf("draft-06 numeric form incorrectly carries a boolean bound too: %#v", min)
		}
		// exclusiveMaximum is the same standalone-number dialect form as
		// exclusiveMinimum above, on the same fixture property — the pair
		// this fixture's "count" property was extended with specifically so
		// exclusiveMaximum, previously never exercised anywhere in this
		// suite, gets its own coverage rather than only its Minimum sibling.
		max := property.Constraints.ExclusiveMaximum
		if max == nil || max.Number == nil || *max.Number != 100 {
			t.Fatalf("draft-06 standalone numeric exclusiveMaximum form was not captured: %#v", max)
		}
		if max.Bool != nil {
			t.Fatalf("draft-06 numeric exclusiveMaximum form incorrectly carries a boolean bound too: %#v", max)
		}
	})
}

// TestSchemaWalkRecordsIgnoredVendorKeywords covers deliberately ignoring
// javaType, comment and description — recording that they were ignored,
// rather than silently dropping them or folding them into a constraint —
// both at the document root and on an individual property. It asserts the
// exact per-occurrence "<pointer>:<keyword>" strings rather than a Contains
// scan over keyword names, because the fixture's "description" keyword
// occurs at BOTH the document root and on timestamp, and only exact,
// distinct entries prove the two occurrences were recorded separately
// rather than collapsed into one document-wide flag.
func TestSchemaWalkRecordsIgnoredVendorKeywords(t *testing.T) {
	document, err := walkSchema(fixturePath(t, "schemas", "draft06_response.json"))
	if err != nil {
		t.Fatalf("schema walk is not implemented: %v", err)
	}
	wantIgnored := []string{
		"#/properties/timestamp:javaType",
		"#/properties/timestamp:comment",
		"#:description",
		"#/properties/timestamp:description",
	}
	for _, want := range wantIgnored {
		found := false
		for _, ignored := range document.IgnoredKeywords {
			if ignored == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("ignored keyword occurrence %q was not recorded: %#v", want, document.IgnoredKeywords)
		}
	}

	timestamp := findSchemaProperty(document.Properties, "#/properties/timestamp")
	if timestamp == nil {
		t.Fatal("timestamp property was not walked")
	}
	// Strengthened beyond MaxLength/MinLength alone (a near-vacuous check,
	// since nothing in this fixture would ever populate those two fields
	// regardless of how javaType/comment/description were handled): none of
	// the three ignored keywords carries any constraint meaning, so the
	// entire Constraints value must stay the zero value.
	if timestamp.Constraints != (Constraints{}) {
		t.Fatalf("ignored vendor keywords were folded into a constraint instead of being recorded as ignored: %#v", timestamp.Constraints)
	}
}

// TestPerFileSchemaShapeDetection covers dialect and shape detection as five
// separately observed per-file facts. The fixture set is deliberately
// decorrelated: definitions presence and $ref presence vary independently of
// each other (a file may define something nothing references), both vary
// independently of the dialect (draft-04 appears with and without them), the
// identifier spelling varies independently of the dialect (draft-06 appears
// with both "$id" and the legacy "id"), and the request-filename convention
// varies within one dialect (draft-06 files carry bare and suffixed request
// names in different sets). A detector that computed one fact from another, or
// from a combination of others — "draft-04, therefore no definitions"; "has
// definitions, therefore has references"; "draft-06, therefore $id"; "draft-06,
// therefore suffixed request names"; "has definitions unless it has neither a
// $ref nor a Response filename" — cannot satisfy all eight rows.
//
// Qux.json is a synthetic decorrelation fixture, not a corpus shape: the
// published sets do pair "id" with draft-04 and "$id" with draft-06 in every
// file, so no real file declares draft-06 and then uses "id". The combination
// is nevertheless well-formed — "id" is simply not draft-06's identifier
// keyword — and detecting a fact per file means recording what a file
// actually does, odd combinations included. Without this row, an
// implementation that derived the spelling from the declared draft, never
// inspecting which key the file carries, would satisfy every other row.
//
// Every fact is asserted on its own so a failure names the fact that was
// wrong rather than printing a whole struct and leaving the reader to
// diff it.
func TestPerFileSchemaShapeDetection(t *testing.T) {
	tests := []struct {
		name              string
		wantSchema        string
		wantIdentifier    string
		wantDefinitions   bool
		wantReferences    bool
		wantFilenameShape string
	}{
		// Base 1.6 shape: draft-04, "id", fully inline, bare request name.
		{name: "Foo.json", wantSchema: "http://json-schema.org/draft-04/schema#", wantIdentifier: "id", wantFilenameShape: "request-bare"},
		// 2.0.1 shape: draft-06, "$id", suffixed request name.
		{name: "FooRequest.json", wantSchema: "http://json-schema.org/draft-06/schema#", wantIdentifier: "$id", wantFilenameShape: "request-suffixed"},
		// A definitions block with local references into it.
		{name: "FooResponse.json", wantSchema: "http://json-schema.org/draft-06/schema#", wantIdentifier: "$id", wantDefinitions: true, wantReferences: true, wantFilenameShape: "response"},
		// The 1.6 security shape: the 2.0.1 dialect with the 1.6 bare request
		// filename, and an identifier that still says "req" inside a file
		// whose name does not. Together with FooRequest.json this is a pair
		// differing in exactly one detected fact — the filename convention —
		// so neither convention nor dialect can be read off the other.
		{name: "Bar.json", wantSchema: "http://json-schema.org/draft-06/schema#", wantIdentifier: "$id", wantFilenameShape: "request-bare"},
		// Definitions present with nothing referencing them. Paired with
		// FooResponse.json this differs in exactly one detected fact — $ref
		// presence — which is what forces the two to be observed separately.
		{name: "BarResponse.json", wantSchema: "http://json-schema.org/draft-06/schema#", wantIdentifier: "$id", wantDefinitions: true, wantFilenameShape: "response"},
		// A bare request filename carrying definitions nothing references.
		// Against Bar.json this differs in exactly one detected fact —
		// definitions presence — which is what forces definitions presence to
		// be observed on its own rather than reconstructed from the dialect,
		// the identifier spelling, the filename convention, $ref presence, or
		// any combination of them.
		{name: "Corge.json", wantSchema: "http://json-schema.org/draft-06/schema#", wantIdentifier: "$id", wantDefinitions: true, wantFilenameShape: "request-bare"},
		// draft-04 carrying definitions and a local $ref. Both keywords are
		// part of draft-04, so this is a legal file; shape is read from the
		// file rather than assumed from the dialect the file declares.
		{name: "Baz.json", wantSchema: "http://json-schema.org/draft-04/schema#", wantIdentifier: "id", wantDefinitions: true, wantReferences: true, wantFilenameShape: "request-bare"},
		// The synthetic row: draft-06 declared in $schema, the legacy "id" key
		// used for the identifier. Against Bar.json it differs in exactly one
		// detected fact — the spelling — and against Foo.json in exactly one
		// other — the dialect — so neither can be read off the other.
		{name: "Qux.json", wantSchema: "http://json-schema.org/draft-06/schema#", wantIdentifier: "id", wantFilenameShape: "request-bare"},
	}

	// The decorrelation the table rests on is asserted rather than left to a
	// comment, so a later edit that collapses the fixture set back into a
	// fully correlated matrix fails here instead of silently weakening every
	// row below.
	sawDefinitionsWithoutReferences := false
	for _, test := range tests {
		if test.wantDefinitions && !test.wantReferences {
			sawDefinitionsWithoutReferences = true
		}
	}
	if !sawDefinitionsWithoutReferences {
		t.Fatal("no fixture carries definitions without a $ref: definitions presence and $ref presence would stay correlated, and a detector deriving either from the other would pass every row")
	}
	// A fact is observed in isolation only when two fixtures differ in that
	// fact and in nothing else; with no such pair, an implementation deriving
	// the fact from one of the others still satisfies every row. All five
	// detected facts are required to have such a pair, with no exemptions:
	// rows that individually make each single-fact derivation implausible are
	// not a substitute, because they leave the compound derivations standing.
	// Without Corge.json, for instance, a detector computing hasDefinitions as
	// "has a $ref, or is named *Response.json" satisfies every other row in
	// the table without ever inspecting the definitions keyword.
	isolated := map[string]bool{}
	for i := range tests {
		for j := i + 1; j < len(tests); j++ {
			differences, differing := 0, ""
			for _, fact := range []struct {
				name    string
				differs bool
			}{
				{name: "$schema", differs: tests[i].wantSchema != tests[j].wantSchema},
				{name: "identifier spelling", differs: tests[i].wantIdentifier != tests[j].wantIdentifier},
				{name: "definitions presence", differs: tests[i].wantDefinitions != tests[j].wantDefinitions},
				{name: "$ref presence", differs: tests[i].wantReferences != tests[j].wantReferences},
				{name: "request-filename convention", differs: tests[i].wantFilenameShape != tests[j].wantFilenameShape},
			} {
				if fact.differs {
					differences++
					differing = fact.name
				}
			}
			if differences == 1 {
				isolated[differing] = true
			}
		}
	}
	for _, fact := range []string{"$schema", "identifier spelling", "definitions presence", "$ref presence", "request-filename convention"} {
		if !isolated[fact] {
			t.Fatalf("no two fixtures differ in %s and in nothing else, so that fact is never observed in isolation and an implementation deriving it from another fact, or from a combination of them, would pass every row", fact)
		}
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := fixturePath(t, "schemas", test.name)
			document := readJSONFixture(t, path)
			shape, err := detectSchemaShape(document, test.name)
			if err != nil {
				t.Fatalf("shape detection is not implemented for %s: %v", test.name, err)
			}
			if shape.Schema != test.wantSchema {
				t.Errorf("$schema=%q, want %q", shape.Schema, test.wantSchema)
			}
			if shape.IdentifierKeyword != test.wantIdentifier {
				t.Errorf("identifier keyword=%q, want %q — the spelling is read from the key this file actually carries, never inferred from the draft its $schema declares", shape.IdentifierKeyword, test.wantIdentifier)
			}
			if shape.HasDefinitions != test.wantDefinitions {
				t.Errorf("hasDefinitions=%t, want %t — definitions presence is read from this file, never inferred from its dialect or from whether it uses $ref", shape.HasDefinitions, test.wantDefinitions)
			}
			if shape.HasReferences != test.wantReferences {
				t.Errorf("hasReferences=%t, want %t — $ref presence is read from this file, never inferred from its dialect or from whether it carries a definitions block", shape.HasReferences, test.wantReferences)
			}
			if shape.FilenameShape != test.wantFilenameShape {
				t.Errorf("filenameShape=%q, want %q — the request-filename convention is a property of the set this file belongs to, never of the dialect it declares or of the identifier inside it", shape.FilenameShape, test.wantFilenameShape)
			}
		})
	}
}

// TestRequestFilenameConventionIsPerSet asserts that the request-filename
// convention (bare "Foo.json" vs suffixed "FooRequest.json") is a fact
// about a whole schema directory, computed over the set of filenames it
// contains — never sniffed from one response file's $id.
func TestRequestFilenameConventionIsPerSet(t *testing.T) {
	tests := []struct {
		name      string
		filenames []string
		want      string
	}{
		{name: "1.6-style bare request names", filenames: []string{"Foo.json", "FooResponse.json", "Bar.json", "BarResponse.json"}, want: "request-bare"},
		{name: "2.0.1-style suffixed request names", filenames: []string{"FooRequest.json", "FooResponse.json", "BarRequest.json", "BarResponse.json"}, want: "request-suffixed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := requestFilenameConvention(test.filenames)
			if err != nil {
				t.Fatalf("request-filename convention detection is not implemented: %v", err)
			}
			if got != test.want {
				t.Fatalf("request-filename convention=%q, want %q", got, test.want)
			}
		})
	}
}

// TestRequestFilenameConventionRejectsEmptyAndAmbiguousSets covers the two
// documented error returns: an empty set has no filenames to derive a
// convention from, and a set mixing bare and suffixed request filenames has
// no single answer — both are hard errors rather than a guessed default.
func TestRequestFilenameConventionRejectsEmptyAndAmbiguousSets(t *testing.T) {
	t.Run("empty set", func(t *testing.T) {
		_, err := requestFilenameConvention(nil)
		if err == nil {
			t.Fatal("empty filename set did not produce an error")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "empty") {
			t.Fatalf("empty filename set error does not name the reason (empty input): %v", err)
		}
	})
	t.Run("mixed convention set", func(t *testing.T) {
		filenames := []string{"Foo.json", "FooResponse.json", "BarRequest.json", "BarResponse.json"}
		_, err := requestFilenameConvention(filenames)
		if err == nil {
			t.Fatal("a set mixing bare and suffixed request filenames did not produce an error")
		}
		msg := strings.ToLower(err.Error())
		if !strings.Contains(msg, "mixed") && !strings.Contains(msg, "ambiguous") {
			t.Fatalf("mixed/ambiguous filename set error does not name the reason: %v", err)
		}
	})
}

func TestEnumPropertyAllowsAdditionalPropertiesKeyword(t *testing.T) {
	path := fixturePath(t, "schemas", "Foo.json")
	document, err := walkSchema(path)
	if err != nil {
		t.Fatalf("enum-property walking is not implemented: %v", err)
	}
	property := findSchemaProperty(document.Properties, "#/properties/status")
	if property == nil || property.Type != "string" {
		t.Fatalf("enum property was not walked: %#v", property)
	}
	if strings.Join(property.Enum, ",") != "Accepted,Rejected" {
		t.Fatalf("enum values=%v, want [Accepted Rejected]", property.Enum)
	}
	// additionalProperties has exactly one home: Constraints.AdditionalProperties
	// (contract.go's shared constraint shape) — SchemaProperty does not carry
	// a second, duplicate field for it.
	if property.Constraints.AdditionalProperties == nil || *property.Constraints.AdditionalProperties {
		t.Fatalf("additionalProperties inside enum property was not tolerated: %#v", property.Constraints.AdditionalProperties)
	}
}

func TestUnknownSchemaDraftIsRejected(t *testing.T) {
	path := fixturePath(t, "schemas", "unknown_draft.json")
	document := readJSONFixture(t, path)
	_, err := detectSchemaShape(document, "unknown_draft.json")
	if err == nil {
		t.Fatal("unknown schema draft was not rejected")
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "unknown schema draft") {
		t.Fatalf("unknown schema draft error missing the required phrase: %v", err)
	}
	if !strings.Contains(err.Error(), "unknown_draft.json") {
		t.Fatalf("unknown schema draft error does not name the offending file: %v", err)
	}
	if !strings.Contains(err.Error(), "draft-07") {
		t.Fatalf("unknown schema draft error does not name the offending draft: %v", err)
	}
}

func TestExternalReferenceIsRejected(t *testing.T) {
	document := readJSONFixture(t, fixturePath(t, "schemas", "external_ref.json"))
	reference := "other.json#/definitions/Value"
	_, err := resolveLocalRef(document, reference)
	if err == nil {
		t.Fatal("non-local $ref was not rejected")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "local") {
		t.Fatalf("non-local $ref error missing the required phrase: %v", err)
	}
	if !strings.Contains(err.Error(), reference) {
		t.Fatalf("non-local $ref error does not name the offending reference: %v", err)
	}
}

func TestMultiLevelReferenceIsRejected(t *testing.T) {
	document := readJSONFixture(t, fixturePath(t, "schemas", "multi_level_ref.json"))
	reference := "#/definitions/First"
	_, err := resolveLocalRef(document, reference)
	if err == nil {
		t.Fatal("multi-level $ref chain was not rejected")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "multi") {
		t.Fatalf("multi-level $ref error missing the required phrase: %v", err)
	}
	if !strings.Contains(err.Error(), reference) {
		t.Fatalf("multi-level $ref error does not name the offending reference: %v", err)
	}
}

// TestResolveLocalRefSucceedsOnSingleLevelReference covers the success path:
// no fixture in this suite previously exercised resolveLocalRef returning a
// resolved definition rather than an error.
func TestResolveLocalRefSucceedsOnSingleLevelReference(t *testing.T) {
	document := readJSONFixture(t, fixturePath(t, "schemas", "draft06_response.json"))
	resolved, err := resolveLocalRef(document, "#/definitions/Status")
	if err != nil {
		t.Fatalf("local $ref resolution is not implemented: %v", err)
	}
	if resolved["type"] != "string" {
		t.Fatalf("resolved local $ref has the wrong shape: %#v", resolved)
	}
	enum, ok := resolved["enum"].([]any)
	if !ok || len(enum) != 2 {
		t.Fatalf("resolved local $ref lost its enum values: %#v", resolved)
	}
}

func readJSONFixture(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("parse fixture %s: %v", path, err)
	}
	return document
}

func findSchemaProperty(properties []SchemaProperty, pointer string) *SchemaProperty {
	for i := range properties {
		if properties[i].Pointer == pointer {
			return &properties[i]
		}
	}
	return nil
}
