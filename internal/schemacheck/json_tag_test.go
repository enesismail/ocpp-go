package main

import (
	"testing"
)

// TestJSONDashTagsAreSuppressedButLiteralDashSurvives covers encoding/json's
// suppression marker. A field tagged json:"-" is never encoded, so it has no
// wire presence and no schema property to pair against; emitting it under the
// literal name "-" would put a row in the report claiming the schema is
// missing a property named "-", for every such field in the tree.
//
// The one shape that looks the same and means the opposite is the tag "-,",
// encoding/json's own escape hatch for a field whose JSON name really is the
// single character "-". That field does cross the wire and must survive.
func TestJSONDashTagsAreSuppressedButLiteralDashSurvives(t *testing.T) {
	path := fixturePath(t, "go_ast", "json_skip.go")
	structs, err := walkGoFile(path, newResolution(newTypeResolver(), newWireTypeRegistry()))
	if err != nil {
		t.Fatalf("Go AST walker is not implemented for %s: %v", path, err)
	}
	payload := findStruct(structs, "SkipPayload")
	if payload == nil {
		t.Fatalf("SkipPayload was not discovered: %#v", structs)
	}

	if findGoField(payload.Fields, "Internal") != nil {
		t.Fatalf("a field tagged json:\"-\" was walked instead of suppressed: %#v", payload.Fields)
	}

	// The suppressed embedded field takes its promoted fields with it.
	for _, field := range payload.Fields {
		if field.JSONName == "promoted" {
			t.Fatalf("an embedded field tagged json:\"-\" still promoted its own fields: %#v", payload.Fields)
		}
	}

	// Exactly one field may carry the JSON name "-", and it must be the one
	// whose tag is "-," — the escape hatch, not the suppression marker.
	var dashNamed []GoField
	for _, field := range payload.Fields {
		if field.JSONName == "-" {
			dashNamed = append(dashNamed, field)
		}
	}
	if len(dashNamed) != 1 {
		t.Fatalf("%d fields carry the JSON name %q, want exactly 1 (the json:\"-,\" field): %#v", len(dashNamed), "-", payload.Fields)
	}
	if dashNamed[0].Name != "LiteralDash" {
		t.Fatalf("the field carrying JSON name %q is %q, want LiteralDash", "-", dashNamed[0].Name)
	}

	// Everything else the struct declares is untouched: a tagged field keeps
	// its name, and an untagged one keeps being encoded under its Go name.
	kept := findGoField(payload.Fields, "Kept")
	if kept == nil || kept.JSONName != "kept" {
		t.Fatalf("an ordinary tagged field did not survive suppression handling: %#v", payload.Fields)
	}
	untagged := findGoField(payload.Fields, "Untagged")
	if untagged == nil {
		t.Fatalf("an untagged field was suppressed: %#v", payload.Fields)
	}
	if untagged.JSONName != "" {
		t.Fatalf("an untagged field was given JSON name %q; encoding/json falls back to the Go field name, which the walker records as an empty tag name", untagged.JSONName)
	}

	if len(payload.Fields) != 3 {
		t.Fatalf("SkipPayload flattened to %d fields, want 3 (Kept, LiteralDash, Untagged): %#v", len(payload.Fields), payload.Fields)
	}
}
