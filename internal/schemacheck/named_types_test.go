package main

import (
	"strings"
	"testing"
)

// TestNamedScalarAliasFieldsCarryTheirUnderlyingWireType covers the walk over
// the idiom the message tree writes protocol enums in: a named type declared
// as a scalar (type BootReason string) used as a field type. What such a
// field puts on the wire is that scalar, so wireType — the value schema
// conformance is decided on — has to follow the declaration through to it,
// while declaredType keeps the named type exactly as written. A walker that
// stopped at the name would leave wireType holding an unrecognized type name,
// which the schema-vocabulary normalizer can only read as "object".
func TestNamedScalarAliasFieldsCarryTheirUnderlyingWireType(t *testing.T) {
	path := fixturePath(t, "go_ast", "scalar_alias.go")
	structs, err := walkGoFile(path, newResolution(newTypeResolver(), newWireTypeRegistry()))
	if err != nil {
		t.Fatalf("Go AST walker is not implemented for %s: %v", path, err)
	}
	payload := findStruct(structs, "AliasPayload")
	if payload == nil {
		t.Fatalf("AliasPayload was not discovered: %#v", structs)
	}

	cases := []struct {
		field            string
		wantDeclaredType string
		wantWireType     string
		wantSchemaKind   string
	}{
		{field: "Reason", wantDeclaredType: "BootReason", wantWireType: "string", wantSchemaKind: "string"},
		{field: "Interval", wantDeclaredType: "Interval", wantWireType: "int", wantSchemaKind: "integer"},
		{field: "Enabled", wantDeclaredType: "Enabled", wantWireType: "bool", wantSchemaKind: "boolean"},
		// Declared through another named alias rather than a builtin: the
		// chain has to be followed, not hopped once.
		{field: "Chained", wantDeclaredType: "Chained", wantWireType: "string", wantSchemaKind: "string"},
		// Declared through byte, which Go itself defines as uint8.
		{field: "Counter", wantDeclaredType: "Counter", wantWireType: "uint8", wantSchemaKind: "integer"},
		// Negative control: a named STRUCT type keeps being an object, so a
		// fix that folded every named type onto a scalar would fail here.
		{field: "Nested", wantDeclaredType: "Opaque", wantWireType: "Opaque", wantSchemaKind: "object"},
	}
	for _, test := range cases {
		t.Run(test.field, func(t *testing.T) {
			field := findGoField(payload.Fields, test.field)
			if field == nil {
				t.Fatalf("field %s was not walked: %#v", test.field, payload.Fields)
			}
			if field.DeclaredType != test.wantDeclaredType {
				t.Fatalf("declaredType = %q, want %q — the literal type as written must survive wire-type resolution", field.DeclaredType, test.wantDeclaredType)
			}
			if field.WireType != test.wantWireType {
				t.Fatalf("wireType = %q, want %q", field.WireType, test.wantWireType)
			}
			if got := normalizeWireType(field.WireType, field.Slice); got != test.wantSchemaKind {
				t.Fatalf("wireType %q normalizes to schema kind %q, want %q", field.WireType, got, test.wantSchemaKind)
			}
		})
	}

	// The struct-typed control must still be recursed into, so folding named
	// types onto scalars cannot have cost the walk its nested leaves.
	if findGoFieldByPath(payload.Fields, "Nested.Code") == nil {
		t.Fatalf("named struct field was no longer recursed into: %#v", payload.Fields)
	}
}

// TestNamedScalarAliasClassifiesAgainstItsScalarType is the classification
// half of the pair: the same walked field, paired against a schema property
// of its own scalar type and against ones of genuinely different types.
//
// The matching case must not be reported as a wrong type — that is the
// false-positive this guards, and with 64 enum-typed fields in the tree it
// would dominate the wrong-type count outright. It does not become IDENTICAL:
// its declared type is a named type where the mapping's plain-string output
// is a bare string, and the classifier's own fixtures already pin that shape
// as one nothing accounts for, so it lands in the class reserved for exactly
// that and is triaged there.
//
// The mismatching cases must stay wrong types. The object case is the sharp
// one: before the scalar fold, an enum field's wire type was itself read as
// an object, so a schema object property agreed with it and the genuine
// mismatch went unreported.
func TestNamedScalarAliasClassifiesAgainstItsScalarType(t *testing.T) {
	path := fixturePath(t, "go_ast", "scalar_alias.go")
	structs, err := walkGoFile(path, newResolution(newTypeResolver(), newWireTypeRegistry()))
	if err != nil {
		t.Fatalf("Go AST walker is not implemented for %s: %v", path, err)
	}
	payload := findStruct(structs, "AliasPayload")
	if payload == nil {
		t.Fatalf("AliasPayload was not discovered: %#v", structs)
	}
	reason := findGoField(payload.Fields, "Reason")
	if reason == nil {
		t.Fatalf("Reason field was not walked: %#v", payload.Fields)
	}

	cases := []struct {
		schemaType   string
		wantWrongTyp bool
	}{
		{schemaType: "string", wantWrongTyp: false},
		{schemaType: "integer", wantWrongTyp: true},
		{schemaType: "object", wantWrongTyp: true},
	}
	for _, test := range cases {
		t.Run("schema type "+test.schemaType, func(t *testing.T) {
			schemaType := test.schemaType
			result, err := classifyField(ComparisonInput{
				Go:            *reason,
				Schema:        SchemaField{Pointer: "#/properties/reason", Type: &schemaType, Required: true},
				GoPresent:     true,
				SchemaPresent: true,
			})
			if err != nil {
				t.Fatalf("classification is not implemented: %v", err)
			}
			wrongType := result.Class == FORK_BUG && strings.Contains(strings.ToLower(result.Rule), "wrong type")
			if wrongType != test.wantWrongTyp {
				t.Fatalf("a %q-typed named scalar alias against schema type %q classified as %q (rule: %s); wrong-type reported=%t, want %t",
					reason.WireType, test.schemaType, result.Class, result.Rule, wrongType, test.wantWrongTyp)
			}
		})
	}
}
