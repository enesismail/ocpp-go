package main

import (
	"sort"
	"testing"
)

// TestSharedDefinitionReachesAreDistinctOccurrences covers the fact a shared
// definition's properties are declared once but reached once per reference.
// The fixture mirrors the real shape: one definition referenced from the
// document root, from a nested object, and from an object nested inside that
// one — three reaches, and therefore three occurrences of the property it
// declares.
//
// A walk that identified an occurrence only by where the property is
// textually declared would report the same thing three times, with no way to
// tell the reaches apart and no way to pair any of them against the Go field
// that actually holds it, since pairing is scoped to a path.
func TestSharedDefinitionReachesAreDistinctOccurrences(t *testing.T) {
	path := fixturePath(t, "schemas", "shared_definition_reaches.json")
	document, err := walkSchema(path)
	if err != nil {
		t.Fatalf("schema walker is not implemented for %s: %v", path, err)
	}

	var vendorIDs []SchemaProperty
	for _, prop := range document.Properties {
		if prop.Name == "vendorId" {
			vendorIDs = append(vendorIDs, prop)
		}
	}
	if len(vendorIDs) != 3 {
		t.Fatalf("the shared definition's property was walked %d times, want 3 (once per reference): %#v", len(vendorIDs), vendorIDs)
	}

	got := make([]string, 0, len(vendorIDs))
	for _, prop := range vendorIDs {
		got = append(got, prop.Path)
	}
	sort.Strings(got)
	want := []string{
		"customData.vendorId",
		"station.customData.vendorId",
		"station.modem.customData.vendorId",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("occurrence paths = %v, want %v — three references to one definition must be three distinguishable occurrences", got, want)
		}
	}

	// The companion fact, asserted so the split between the two identities is
	// deliberate rather than accidental: all three share a pointer, because a
	// pointer says where a property is *declared*, and a shared definition is
	// declared once however often it is referenced.
	for _, prop := range vendorIDs[1:] {
		if prop.Pointer != vendorIDs[0].Pointer {
			t.Fatalf("occurrences of one shared definition no longer share a declaration pointer (%q vs %q); the per-declaration identity a per-file count is taken over would change meaning", prop.Pointer, vendorIDs[0].Pointer)
		}
	}
	if vendorIDs[0].Pointer != "#/definitions/CustomDataType/properties/vendorId" {
		t.Fatalf("declaration pointer = %q, want it rooted at the definition the property is declared in", vendorIDs[0].Pointer)
	}
}

// TestOccurrencePathsCoverEveryReachedProperty checks the paths of the whole
// document, not just the repeated one: a path is built by threading each
// reference through, so an object reached through another object carries both
// hops.
func TestOccurrencePathsCoverEveryReachedProperty(t *testing.T) {
	path := fixturePath(t, "schemas", "shared_definition_reaches.json")
	document, err := walkSchema(path)
	if err != nil {
		t.Fatalf("schema walker is not implemented for %s: %v", path, err)
	}

	got := make([]string, 0, len(document.Properties))
	for _, prop := range document.Properties {
		got = append(got, prop.Path)
	}
	sort.Strings(got)
	want := []string{
		"customData",
		"customData.vendorId",
		"station",
		"station.customData",
		"station.customData.vendorId",
		"station.modem",
		"station.modem.customData",
		"station.modem.customData.vendorId",
		"station.modem.iccid",
	}
	if len(got) != len(want) {
		t.Fatalf("walked %d occurrences, want %d:\ngot  %v\nwant %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("occurrence paths = %v, want %v", got, want)
		}
	}
}
