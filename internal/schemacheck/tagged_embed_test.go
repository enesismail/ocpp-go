package main

import (
	"testing"
)

// TestTaggedEmbeddedFieldIsNotFlattened covers encoding/json's rule that an
// embedded field carrying a JSON name is not an embed as far as the wire is
// concerned: it marshals as an ordinary named field, nesting its own fields
// under that name. A flattener that promoted every anonymous field regardless
// of its tag would put those fields in the wrong place — at the enclosing
// object's top level, under names the wire never carries there.
func TestTaggedEmbeddedFieldIsNotFlattened(t *testing.T) {
	path := fixturePath(t, "go_ast", "tagged_embed.go")
	structs, err := walkGoFile(path, newResolution(newTypeResolver(), newWireTypeRegistry()))
	if err != nil {
		t.Fatalf("Go AST walker is not implemented for %s: %v", path, err)
	}
	payload := findStruct(structs, "TaggedEmbedPayload")
	if payload == nil {
		t.Fatalf("TaggedEmbedPayload was not discovered: %#v", structs)
	}

	// The embed itself is a field, under the name its tag gives it.
	inner := findGoField(payload.Fields, "Inner")
	if inner == nil {
		t.Fatalf("the tagged embed did not become a field of its own: %#v", payload.Fields)
	}
	if inner.JSONName != "inner" {
		t.Fatalf("the tagged embed's JSON name = %q, want %q", inner.JSONName, "inner")
	}

	// Its own field is nested under it, not promoted alongside Note.
	if findGoFieldByPath(payload.Fields, "Inner.Code") == nil {
		t.Fatalf("the tagged embed's own field was not nested under it: %#v", payload.Fields)
	}
	for _, field := range payload.Fields {
		if field.JSONName == "code" && field.Path == "Code" {
			t.Fatalf("the tagged embed's field was promoted into the enclosing object's namespace: %#v", payload.Fields)
		}
	}

	note := findGoField(payload.Fields, "Note")
	if note == nil || note.JSONName != "note" {
		t.Fatalf("an ordinary named field beside the tagged embed was disturbed: %#v", payload.Fields)
	}
}

// TestTaggedFieldDominatesUntaggedAtEqualDepth covers the other half of the
// rule. Two fields promoted at the same depth marshal under the same name —
// one because its tag says so, the other by falling back to its Go field name.
// encoding/json does not treat that as ambiguous: a tag is a statement of
// intent, so the tagged field wins and the untagged one is dropped.
//
// The existing same-depth fixture, where BOTH claimants are tagged, stays
// ambiguous and keeps dropping every field carrying the name — the two
// together are what pin the rule rather than just its outcome in one case.
func TestTaggedFieldDominatesUntaggedAtEqualDepth(t *testing.T) {
	path := fixturePath(t, "go_ast", "tagged_embed.go")
	structs, err := walkGoFile(path, newResolution(newTypeResolver(), newWireTypeRegistry()))
	if err != nil {
		t.Fatalf("Go AST walker is not implemented for %s: %v", path, err)
	}
	payload := findStruct(structs, "TaggedDominancePayload")
	if payload == nil {
		t.Fatalf("TaggedDominancePayload was not discovered: %#v", structs)
	}

	if len(payload.Fields) != 1 {
		t.Fatalf("flattened to %d fields, want exactly 1 (the tagged claimant): %#v", len(payload.Fields), payload.Fields)
	}
	survivor := payload.Fields[0]
	if survivor.JSONName != "Note" {
		t.Fatalf("the surviving field carries JSON name %q, want %q — the untagged field won, or both were dropped as ambiguous", survivor.JSONName, "Note")
	}
}
