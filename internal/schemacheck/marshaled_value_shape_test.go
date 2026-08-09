package main

import (
	"strings"
	"testing"
)

// marshaledDateTimeValue is the shape at issue: a custom-marshaled struct held
// by value. Its declared type is a struct and its wire type is the string that
// struct marshals to — the exact pair a named string alias also produces, which
// is why the two are told apart by how the walker resolved them rather than by
// anything recoverable from the two type names afterwards.
func marshaledDateTimeValue(omitempty bool) GoField {
	return GoField{
		Name: "Timestamp", JSONName: "timestamp",
		DeclaredType: "DateTime", WireType: "string",
		CustomMarshaled: true, Omitempty: omitempty,
		Validate: []string{"omitempty"},
	}
}

// namedStringAliasValue is marshaledDateTimeValue's minimal pair: identical in
// every recorded field except how its wire type was arrived at. Its declared
// type really is a string underneath, so it has an empty value and omitempty
// omits it.
func namedStringAliasValue(omitempty bool) GoField {
	return GoField{
		Name: "Reason", JSONName: "reason",
		DeclaredType: "BootReason", WireType: "string",
		CustomMarshaled: false, Omitempty: omitempty,
		Validate: []string{"omitempty"},
	}
}

// TestOmitemptyReadsTheDeclaredKindNotTheWireType covers the rekeying. A struct
// that marshals to a JSON string is still a struct: encoding/json decides
// emptiness from the value's kind before any MarshalJSON is consulted, so it
// has no empty value and is never omitted. Reading the resolved wire type
// instead saw a string, decided a string can be empty, and concluded the field
// was omittable.
//
// The two fields below are identical in every recorded fact except that one
// resolved through the custom-marshaler registry, so nothing but that fact can
// separate them — which is the point: it is not derivable from the rest.
func TestOmitemptyReadsTheDeclaredKindNotTheWireType(t *testing.T) {
	marshaled := marshaledDateTimeValue(true)
	alias := namedStringAliasValue(true)

	if marshaled.WireType != alias.WireType {
		t.Fatalf("the pair no longer shares a wire type (%q vs %q), so it does not isolate the declared kind", marshaled.WireType, alias.WireType)
	}
	if marshaled.Pointer || marshaled.Slice || alias.Pointer || alias.Slice {
		t.Fatal("the pair must both be plain values for the declared kind to be the only discriminator")
	}

	if omitemptyCanOmit(marshaled) {
		t.Fatal("a custom-marshaled struct held by value was reported omittable; encoding/json never omits a struct, whatever it marshals to")
	}
	if !omitemptyCanOmit(alias) {
		t.Fatal("a named string alias held by value was reported non-omittable; its declared type is a string underneath, so its empty value exists")
	}
}

// TestRequiredMarshaledStructValueIsNotAnOptionalityLie is the consequence at
// the classifier level: tagging such a field omitempty takes nothing off the
// wire, so a required property held in one cannot go missing and reporting it
// as a contradiction is a finding about a payload that cannot occur.
//
// The row is still not IDENTICAL — the mapping for a date-time property is a
// pointer and this is a value — but that is a mapping change, not a
// contradiction, and it is what must be reported instead.
func TestRequiredMarshaledStructValueIsNotAnOptionalityLie(t *testing.T) {
	dateTime := "date-time"
	stringType := "string"
	input := ComparisonInput{
		Go: marshaledDateTimeValue(true),
		Schema: SchemaField{
			Pointer: "#/properties/timestamp", Type: &stringType,
			Required: true, Format: &dateTime,
		},
		GoPresent:     true,
		SchemaPresent: true,
	}

	result, err := classifyField(input)
	if err != nil {
		t.Fatalf("classification is not implemented: %v", err)
	}
	if result.Class == FORK_BUG {
		t.Fatalf("a required custom-marshaled struct value tagged omitempty was reported as an optionality lie (rule: %s); encoding/json never omits a struct", result.Rule)
	}
	if result.Class != SCHEMA_FAITHFUL_CHANGE {
		t.Fatalf("classified as %q (rule: %s), want %q — the date-time mapping is a pointer and this is a value", result.Class, result.Rule, SCHEMA_FAITHFUL_CHANGE)
	}
	if !strings.Contains(strings.ToLower(result.Rule), "date-time") {
		t.Fatalf("rule %q does not cite the mapping that actually differs", result.Rule)
	}
}

// TestRequiredNamedAliasValueWithOmitemptyStaysALie is the negative control on
// the same rekeying: the fix must not turn every value field non-omittable.
// A named string alias does have an empty value, so a required property held
// in one and tagged omitempty really can go missing.
func TestRequiredNamedAliasValueWithOmitemptyStaysALie(t *testing.T) {
	stringType := "string"
	input := ComparisonInput{
		Go: namedStringAliasValue(true),
		Schema: SchemaField{
			Pointer: "#/properties/reason", Type: &stringType, Required: true,
		},
		GoPresent:     true,
		SchemaPresent: true,
	}

	result, err := classifyField(input)
	if err != nil {
		t.Fatalf("classification is not implemented: %v", err)
	}
	if result.Class != FORK_BUG {
		t.Fatalf("a required named string alias tagged omitempty classified as %q (rule: %s), want %q — an empty string is omitted", result.Class, result.Rule, FORK_BUG)
	}
	if !strings.Contains(strings.ToLower(result.Rule), "optionality lie") {
		t.Fatalf("rule %q does not report the optionality lie", result.Rule)
	}
}

// TestWalkerRecordsCustomMarshalerResolution ties the classifier's input back
// to the walk that produces it: the fact has to be observed where the
// resolution happens, or every field reaching the classifier would carry the
// zero value and the pair above could never be told apart on real input.
func TestWalkerRecordsCustomMarshalerResolution(t *testing.T) {
	path := fixturePath(t, "go_ast", "registered_marshaler.go")
	resolver := newTypeResolver()
	resolver.RegisterImport(path, ImportBinding{Alias: "fixturetypes", Path: "example.com/fixturetypes"})
	registry := newWireTypeRegistry()
	registry.Register("example.com/fixturetypes.WireTime", WireType{Type: "string", Format: "date-time"})

	structs, err := walkGoFile(path, newResolution(resolver, registry))
	if err != nil {
		t.Fatalf("registered custom marshaler was not handled: %v", err)
	}
	payload := findStruct(structs, "RegisteredPayload")
	if payload == nil {
		t.Fatal("registered custom-marshaler fixture was not walked")
	}

	when := findGoField(payload.Fields, "When")
	if when == nil {
		t.Fatalf("When field was not found: %#v", payload.Fields)
	}
	if !when.CustomMarshaled {
		t.Fatalf("a field whose wire type came from the marshaler registry was not recorded as such: %#v", when)
	}

	// A field of an ordinary same-file struct type resolved from its own
	// declaration, so it must not be marked.
	nested := findGoField(payload.Fields, "Nested")
	if nested == nil {
		t.Fatalf("Nested field was not found: %#v", payload.Fields)
	}
	if nested.CustomMarshaled {
		t.Fatalf("a field resolved from its own declaration was marked as custom-marshaled: %#v", nested)
	}
}
