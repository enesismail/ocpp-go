package main

import (
	"reflect"
	"testing"
)

func TestSchemaReaderPreservesDeclarationOrder(t *testing.T) {
	document, err := ReadSchema(fixturePath("schemas", "declaration_order.json"))
	requireImplemented(t, "declaration-order schema reader", err)
	got := make([]string, 0, len(document.Root.Properties))
	for _, property := range document.Root.Properties {
		got = append(got, property.Name)
	}
	// Six properties in a deliberately non-alphabetical, non-map-friendly
	// order: a reader that silently iterates a map instead of the raw byte
	// stream has only a 1-in-720 chance of reproducing this order by luck.
	want := []string{"zeta", "alpha", "middle", "omega", "beta", "yankee"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("properties were reordered: got %v, want schema declaration order %v", got, want)
	}
}

func TestSchemaReaderCapturesEveryConstraint(t *testing.T) {
	document, err := ReadSchema(fixturePath("schemas", "all_constraints.json"))
	requireImplemented(t, "complete constraint capture", err)
	if len(document.Root.Properties) != 3 {
		t.Fatalf("constraint fixture returned %d properties, want three", len(document.Root.Properties))
	}
	text, number, list := document.Root.Properties[0], document.Root.Properties[1], document.Root.Properties[2]

	if text.Type != "string" || text.Format != "date-time" || text.Pointer != "#/properties/text" {
		t.Fatalf("string property type/format/pointer is incomplete: %#v", text)
	}
	if text.MinLength == nil || *text.MinLength != 1 {
		t.Fatalf("minLength was not captured as the exact fixture value: %#v", text)
	}
	if text.MaxLength == nil || *text.MaxLength != 32 {
		t.Fatalf("maxLength was not captured as the exact fixture value: %#v", text)
	}
	if text.Pattern != "^[a-z]+$" {
		t.Fatalf("pattern was not captured as the exact fixture value: %#v", text)
	}

	if number.Minimum == nil || *number.Minimum != 0 {
		t.Fatalf("minimum was not captured as the exact fixture value (zero vs. absent matters): %#v", number)
	}
	if number.Maximum == nil || *number.Maximum != 99 {
		t.Fatalf("maximum was not captured as the exact fixture value: %#v", number)
	}
	if number.ExclusiveMinimum == nil || number.ExclusiveMinimum.Number == nil || *number.ExclusiveMinimum.Number != 0 || number.ExclusiveMinimum.Boolean != nil {
		t.Fatalf("exclusiveMinimum did not capture the draft-06 numeric spelling with the exact fixture value: %#v", number)
	}
	if number.ExclusiveMaximum == nil || number.ExclusiveMaximum.Number == nil || *number.ExclusiveMaximum.Number != 99 || number.ExclusiveMaximum.Boolean != nil {
		t.Fatalf("exclusiveMaximum did not capture the draft-06 numeric spelling with the exact fixture value: %#v", number)
	}

	if list.MinItems == nil || *list.MinItems != 1 {
		t.Fatalf("minItems was not captured as the exact fixture value: %#v", list)
	}
	if list.MaxItems == nil || *list.MaxItems != 4 {
		t.Fatalf("maxItems was not captured as the exact fixture value: %#v", list)
	}
	if list.Items == nil || list.Items.Type != "string" {
		t.Fatalf("array element type is incomplete: %#v", list)
	}

	if !text.Required || !number.Required || list.Required {
		t.Fatalf("required-array membership was not captured per property: %#v", document.Root.Properties)
	}
	// additionalProperties is asserted separately, in
	// TestEnumReferenceResolvesValuesAndAllowsAdditionalProperties: an inline
	// anonymous object property (what "closed" used to be here) is a shape
	// the corpus never contains, so that assertion now lives on a shape the
	// corpus does — a $ref'd enum definition that carries the keyword.
}

// TestSchemaReaderPreservesRecognizedMetadata asserts the recognized
// metadata keywords ($id, title) are actually reachable on the returned
// document, not merely tolerated without a parse failure. The fixture also
// carries description, javaType and comment at both the document and
// property level — the reader's ignored-keyword allowlist covers all three
// by name rather than storing them, so their presence here is proof the reader
// tolerates them, not something this test asserts a captured value for.
func TestSchemaReaderPreservesRecognizedMetadata(t *testing.T) {
	document, err := ReadSchema(fixturePath("schemas", "metadata_keywords.json"))
	requireImplemented(t, "recognized schema metadata handling", err)
	if document.ID != "urn:OCPP:Cp:2:2020:3:metadata_keywords" {
		t.Fatalf("schema $id was not preserved: %#v", document)
	}
	if document.Title != "metadata_keywords" {
		t.Fatalf("schema title was not preserved: %#v", document)
	}
}

func TestEnumReferenceResolvesValuesAndAllowsAdditionalProperties(t *testing.T) {
	document, err := ReadSchema(fixturePath("schemas", "enum_ref.json"))
	requireImplemented(t, "enum reference resolution", err)
	definition := findDefinition(document.Definitions, "StatusEnumType")
	if definition == nil || definition.Kind != DefinitionEnum || !reflect.DeepEqual(definition.Values, []string{"Accepted", "Rejected"}) {
		t.Fatalf("enum definition kind or values were not preserved: %#v", document.Definitions)
	}
	status := findProperty(document.Root.Properties, "status")
	if status == nil || !reflect.DeepEqual(status.Enum, []string{"Accepted", "Rejected"}) {
		t.Fatalf("enum property did not carry the resolved value list: %#v", status)
	}
	if status.Ref != "#/definitions/StatusEnumType" {
		t.Fatalf("enum property did not carry its $ref: %#v", status)
	}
	// StatusEnumType's own schema carries additionalProperties:false — the
	// corpus's real "additionalProperties on a string-typed enum" shape
	// (unlike an inline anonymous object, which the corpus never contains).
	// A property resolving through a $ref never carries its own sibling
	// keywords in this corpus, so the value the target definition's schema
	// declares is what a per-property constraint capture surfaces on the
	// referencing property itself.
	if status.AdditionalProperties == nil || *status.AdditionalProperties {
		t.Fatalf("enum property did not capture the additionalProperties=false carried by its $ref'd enum definition: %#v", status)
	}
	if status.EnumDefinition != "StatusEnumType" {
		t.Fatalf("enum property did not name the enum definition it resolved through: %#v", status)
	}
}

func TestRequiredArrayIsInheritedTwoLevelsDown(t *testing.T) {
	document, err := ReadSchema(fixturePath("schemas", "required_inheritance.json"))
	requireImplemented(t, "requiredness inheritance", err)
	outer := findDefinition(document.Definitions, "OuterType")
	middle := findDefinition(document.Definitions, "MiddleType")
	if outer == nil || middle == nil {
		t.Fatalf("requiredness fixture omitted a named definition: outer=%#v middle=%#v", outer, middle)
	}

	rootOuter := findProperty(document.Root.Properties, "outer")
	if rootOuter == nil || rootOuter.Ref != "#/definitions/OuterType" {
		t.Fatalf("root property did not carry its $ref to OuterType: %#v", rootOuter)
	}

	outerMiddle := findProperty(outer.Properties, "middle")
	middleLeaf := findProperty(middle.Properties, "leaf")
	if outerMiddle == nil || middleLeaf == nil || !outerMiddle.Required || !middleLeaf.Required {
		t.Fatalf("requiredness did not reach both named levels: outer=%#v middle=%#v", outer, middle)
	}
	if outerMiddle.Ref != "#/definitions/MiddleType" {
		t.Fatalf("outer.middle did not carry its $ref to MiddleType: %#v", outerMiddle)
	}

	// Negative control: both OuterType and MiddleType declare their own
	// property named "note", but only OuterType's required array lists it —
	// a reader that treats "does this name appear in any required array in
	// the file" as sufficient, instead of scoping the check to the
	// enclosing definition, would wrongly mark MiddleType.note required too.
	outerNote := findProperty(outer.Properties, "note")
	if outerNote == nil || !outerNote.Required {
		t.Fatalf("OuterType's own required property was not captured: %#v", outer)
	}
	middleNote := findProperty(middle.Properties, "note")
	if middleNote == nil {
		t.Fatalf("requiredness fixture is missing its negative-control property: %#v", middle)
	}
	if middleNote.Required {
		t.Fatalf("a property was marked required by an unrelated definition's required array: %#v", middleNote)
	}
}

func TestArrayCapturesCompositeElementType(t *testing.T) {
	document, err := ReadSchema(fixturePath("schemas", "array_composite.json"))
	requireImplemented(t, "composite array element capture", err)
	entries := findProperty(document.Root.Properties, "entries")
	if entries == nil || entries.Items == nil || entries.Items.Ref != "#/definitions/EntryType" || entries.MinItems == nil || entries.MaxItems == nil {
		t.Fatalf("composite array information is incomplete: %#v", entries)
	}
}

func TestUntypedPropertyIsExplicit(t *testing.T) {
	document, err := ReadSchema(fixturePath("schemas", "untyped.json"))
	requireImplemented(t, "untyped property capture", err)
	data := findProperty(document.Root.Properties, "data")
	if data == nil || !data.Untyped {
		t.Fatalf("untyped property was not represented explicitly: %#v", data)
	}
	// Negative control: a normally-typed sibling must not also read as untyped.
	reference := findProperty(document.Root.Properties, "reference")
	if reference == nil || reference.Untyped {
		t.Fatalf("a typed property was incorrectly flagged untyped: %#v", reference)
	}
}

// TestCustomDataDefinitionIsFlaggedReserved covers the corpus's most common
// shape (a bare {"$ref": "#/definitions/CustomDataType"} customData
// property) and the reserved flag it drives, with a sibling object
// definition as a negative control.
func TestCustomDataDefinitionIsFlaggedReserved(t *testing.T) {
	document, err := ReadSchema(fixturePath("schemas", "custom_data.json"))
	requireImplemented(t, "custom data reservation", err)

	customData := findDefinition(document.Definitions, "CustomDataType")
	if customData == nil || !customData.Reserved {
		t.Fatalf("CustomDataType was not flagged reserved: %#v", customData)
	}
	sibling := findDefinition(document.Definitions, "VendorInfoType")
	if sibling == nil || sibling.Reserved {
		t.Fatalf("a non-reserved sibling definition was incorrectly flagged reserved: %#v", sibling)
	}

	customDataProperty := findProperty(document.Root.Properties, "customData")
	if customDataProperty == nil || customDataProperty.Ref != "#/definitions/CustomDataType" {
		t.Fatalf("customData property did not carry its $ref: %#v", customDataProperty)
	}
}

func TestUnknownSchemaKeywordIsRejected(t *testing.T) {
	_, err := ReadSchema(fixturePath("schemas", "unknown_keyword.json"))
	requireExpectedHardFailureContains(t, "unknown property keyword check", err, "#/properties/value/futureKeyword")
}

// TestUnknownRootKeywordIsRejected and TestUnknownDefinitionKeywordIsRejected
// cover the same allowlist check at the two other levels a stray keyword can
// appear: the document root and a named definition, neither of which is a
// property.
func TestUnknownRootKeywordIsRejected(t *testing.T) {
	_, err := ReadSchema(fixturePath("schemas", "unknown_keyword_root.json"))
	requireExpectedHardFailureContains(t, "unknown root keyword check", err, "unknown_keyword_root.json", "futureRootKeyword")
}

func TestUnknownDefinitionKeywordIsRejected(t *testing.T) {
	_, err := ReadSchema(fixturePath("schemas", "unknown_keyword_definition.json"))
	requireExpectedHardFailureContains(t, "unknown definition keyword check", err, "unknown_keyword_definition.json", "StrayType", "futureDefinitionKeyword")
}

func TestNonLocalReferenceIsRejected(t *testing.T) {
	_, err := ReadSchema(fixturePath("schemas", "nonlocal_ref.json"))
	requireExpectedHardFailureContains(t, "non-local reference check", err,
		"nonlocal_ref.json", "#/properties/value", "Other.json#/definitions/ValueType")
}

// TestDanglingLocalReferenceIsRejected covers the other half of the
// reference rule TestNonLocalReferenceIsRejected starts on: this $ref is
// well-formed and local, and still names a definition the file does not
// declare. Nothing in this corpus can resolve it — every reference is local,
// so the file's own definitions block is the only place the target could
// live — and the failure mode a reader falls into here is silent: skipping
// the property drops a field from the generated type, and leaving the $ref
// unresolved hands the emitter a name with nothing behind it. The fixture
// also carries a sibling property whose $ref does resolve, so a reader that
// simply failed on encountering any $ref cannot pass this.
func TestDanglingLocalReferenceIsRejected(t *testing.T) {
	_, err := ReadSchema(fixturePath("schemas", "dangling_ref.json"))
	requireExpectedHardFailureContains(t, "dangling local reference check", err,
		"dangling_ref.json", "#/properties/value", "#/definitions/MissingType")

	// The walk resolves references through this same read, so it cannot be
	// more permissive than the read is: a walk that swallowed the
	// unresolvable target on its way into the IR would leave the check above
	// with no effect on anything the tool actually produces.
	_, walkErr := WalkSchema(fixturePath("schemas", "dangling_ref.json"))
	requireExpectedHardFailureContains(t, "dangling local reference check through the walk", walkErr,
		"dangling_ref.json", "#/properties/value", "#/definitions/MissingType")
}

// TestAdditionalPropertiesIsCapturedOnRootsAndNamedDefinitions covers the
// keyword everywhere it lands on an object:
// TestEnumReferenceResolvesValuesAndAllowsAdditionalProperties already pins
// it on a property resolving through a $ref'd enum, which leaves the two
// object-level positions — a named definition, and the schema file's own
// top-level object — untested, and the corpus writes it in both. A reader
// that captured it only per property would silently lose the object-level
// declarations, and a mapping stage cannot tell an object the schema closed
// from one it said nothing about once the keyword is gone.
func TestAdditionalPropertiesIsCapturedOnRootsAndNamedDefinitions(t *testing.T) {
	document, err := ReadSchema(fixturePath("schemas", "additional_properties.json"))
	requireImplemented(t, "object-level additionalProperties capture", err)

	if document.AdditionalProperties == nil || *document.AdditionalProperties {
		t.Fatalf("the document root's additionalProperties:false was not captured: %#v", document)
	}
	if document.Root.AdditionalProperties == nil || *document.Root.AdditionalProperties {
		t.Fatalf("the root definition did not carry the same additionalProperties the document does — the root definition, not the document, is what survives into the shared definitions index: %#v", document.Root)
	}

	closed := findDefinition(document.Definitions, "ClosedType")
	if closed == nil || closed.AdditionalProperties == nil || *closed.AdditionalProperties {
		t.Fatalf("a named object definition's additionalProperties:false was not captured: %#v", closed)
	}

	// Negative control at the definition level: a sibling that declares no
	// additionalProperties must come back nil rather than defaulting to
	// false, which would read as "closed" on a definition whose schema never
	// said so.
	open := findDefinition(document.Definitions, "OpenType")
	if open == nil {
		t.Fatalf("additionalProperties fixture is missing its negative-control definition: %#v", document.Definitions)
	}
	if open.AdditionalProperties != nil {
		t.Fatalf("a definition declaring no additionalProperties was given a value: %#v", open)
	}

	// Negative control at the document level, on a file whose root declares
	// no additionalProperties at all: both views of the keyword stay nil.
	absent, absentErr := ReadSchema(fixturePath("schemas", "dedup_a.json"))
	requireImplemented(t, "additionalProperties absence", absentErr)
	if absent.AdditionalProperties != nil || absent.Root.AdditionalProperties != nil {
		t.Fatalf("a root declaring no additionalProperties was given a value: document=%#v root=%#v", absent.AdditionalProperties, absent.Root.AdditionalProperties)
	}
}

// TestAdditionalPropertiesSurvivesWalkSchemas is the WalkSchemas-level
// counterpart of TestAdditionalPropertiesIsCapturedOnRootsAndNamedDefinitions,
// in the shape TestReservedFlagSurvivesWalkSchemas uses for the reserved flag:
// that test pins the keyword at the single-document ReadSchema level only, so
// a dedup or IR-assembly step that dropped it while promoting these
// definitions into the shared index would pass every existing test and still
// ship a mapping stage that cannot tell a closed object from one whose schema
// said nothing. The root's value is checked on the definitions-index copy
// because that is where SchemaDocument's own godoc says it survives — the
// root definition, not the document, is what the walk promotes.
func TestAdditionalPropertiesSurvivesWalkSchemas(t *testing.T) {
	manifest := Manifest{SchemaDir: fixturePath("schemas"), Messages: []ManifestMessage{{Name: "AdditionalProperties", Request: "additional_properties.json", Response: "alpha_response.json"}}}
	ir, err := WalkSchemas(manifest)
	requireImplemented(t, "additionalProperties walk", err)

	closed := findDefinition(ir.Definitions, "ClosedType")
	if closed == nil || closed.AdditionalProperties == nil || *closed.AdditionalProperties {
		t.Fatalf("a named object definition lost its additionalProperties:false through WalkSchemas: %#v", closed)
	}

	root := findDefinition(ir.Definitions, "additional_properties")
	if root == nil || root.Kind != DefinitionRoot {
		t.Fatalf("the document root was not indexed as a root-kind definition: %#v", root)
	}
	if root.AdditionalProperties == nil || *root.AdditionalProperties {
		t.Fatalf("the document root's additionalProperties:false did not reach its definitions-index copy: %#v", root)
	}

	// Negative control, as at the reader level: a sibling declaring no
	// additionalProperties must still arrive nil rather than defaulting to
	// false, which would report an object the schema left open as closed.
	open := findDefinition(ir.Definitions, "OpenType")
	if open == nil {
		t.Fatalf("additionalProperties fixture is missing its negative-control definition at the IR level: %#v", ir.Definitions)
	}
	if open.AdditionalProperties != nil {
		t.Fatalf("a definition declaring no additionalProperties was given a value through WalkSchemas: %#v", open)
	}
}

// TestNonDraft06SchemaDialectIsRejected asserts the reader accepts only the
// draft-06 dialect: a draft-04 document is a hard failure naming the file
// and the dialect found, checked at the whole document before any property
// is read (this is also what makes Bound.Boolean unreachable on a
// successful read).
func TestNonDraft06SchemaDialectIsRejected(t *testing.T) {
	_, err := ReadSchema(fixturePath("schemas", "draft04.json"))
	requireExpectedHardFailureContains(t, "schema dialect check", err, "draft04.json", "draft-04")
}

// TestMalformedSchemaIDIsRejected covers a $id that is not shaped like the
// corpus's urn:OCPP:... form at all.
func TestMalformedSchemaIDIsRejected(t *testing.T) {
	_, err := ReadSchema(fixturePath("schemas", "malformed_id.json"))
	requireExpectedHardFailureContains(t, "schema $id format check", err, "malformed_id.json", "not-a-valid-schema-id")
}

// TestSchemaIDStemMismatchIsRejected is distinct from
// TestMalformedSchemaIDIsRejected: this $id is well-formed but its trailing
// segment does not match the file's own stem, which every other fixture in
// this directory (and the real corpus) satisfies by construction.
func TestSchemaIDStemMismatchIsRejected(t *testing.T) {
	_, err := ReadSchema(fixturePath("schemas", "mismatched_id.json"))
	requireExpectedHardFailureContains(t, "schema $id stem check", err, "mismatched_id.json", "WrongStem")
}

// TestEnumDefinitionWithNoValuesIsRejected asserts that an enum definition
// whose values are absent or empty is a hard failure, not an empty enum.
func TestEnumDefinitionWithNoValuesIsRejected(t *testing.T) {
	_, err := ReadSchema(fixturePath("schemas", "empty_enum.json"))
	requireExpectedHardFailureContains(t, "empty enum values check", err, "empty_enum.json", "EmptyStatusEnumType")
}
