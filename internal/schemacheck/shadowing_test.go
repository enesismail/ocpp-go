package main

import (
	"testing"
)

// TestEmbeddedFieldShadowingFollowsEncodingJSONDepthPrecedence covers the
// AST walker's field-flattening depth-precedence rule: encoding/json
// resolves a JSON-name collision between a struct's own field and a field
// promoted from an embedded struct by keeping only the shallower one, and
// drops every field sharing a name that collides at the same depth. A
// flattener that just concatenated a struct's own fields with whatever an
// embedded struct promotes — with no shadowing check at all — would emit
// both of ShadowingPayload's "code" fields and both of AmbiguousPayload's
// "note" fields, neither of which matches what encoding/json would actually
// put on the wire.
func TestEmbeddedFieldShadowingFollowsEncodingJSONDepthPrecedence(t *testing.T) {
	path := fixturePath(t, "go_ast", "shadowing.go")
	resolver := newTypeResolver()
	registry := newWireTypeRegistry()
	structs, err := walkGoFile(path, newResolution(resolver, registry))
	if err != nil {
		t.Fatalf("Go AST walker is not implemented for %s: %v", path, err)
	}

	t.Run("shallower field wins over a same-name promoted field", func(t *testing.T) {
		payload := findStruct(structs, "ShadowingPayload")
		if payload == nil {
			t.Fatal("ShadowingPayload was not discovered")
		}
		var codeFields []GoField
		for _, field := range payload.Fields {
			if field.JSONName == "code" {
				codeFields = append(codeFields, field)
			}
		}
		if len(codeFields) != 1 {
			t.Fatalf("json name %q appeared %d times in the flattened field set, want exactly 1 (the shallower field only): %#v", "code", len(codeFields), codeFields)
		}
		// The survivor must be ShadowingPayload's own depth-0 field
		// (validate:"required"), never ShadowedEmbed's depth-1 promoted one
		// (validate:"len=4") — checking the validate tag, not just the
		// count, proves *which* field survived rather than merely that one
		// did.
		got := codeFields[0]
		if len(got.Validate) != 1 || got.Validate[0] != "required" {
			t.Fatalf("surviving %q field carries validate=%v, want [\"required\"] (ShadowingPayload's own field, not the promoted, deeper ShadowedEmbed.Code)", "code", got.Validate)
		}
	})

	t.Run("same-depth collision drops every field carrying the name", func(t *testing.T) {
		payload := findStruct(structs, "AmbiguousPayload")
		if payload == nil {
			t.Fatal("AmbiguousPayload was not discovered")
		}
		for _, field := range payload.Fields {
			if field.JSONName == "note" {
				t.Fatalf("json name %q survived a same-depth collision between EmbedA and EmbedB (both promoted at depth 1); encoding/json drops every field carrying an ambiguous name: %#v", "note", payload.Fields)
			}
		}
		if len(payload.Fields) != 0 {
			t.Fatalf("AmbiguousPayload flattened to %d fields, want 0 (EmbedA.Note and EmbedB.Note are the struct's only fields, and both collide): %#v", len(payload.Fields), payload.Fields)
		}
	})
}
