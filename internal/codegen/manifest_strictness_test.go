package main

import (
	"path/filepath"
	"testing"
)

// TestManifestWithASecondDocumentIsRejected covers what a loader reading one
// YAML document never sees. multi_document.yaml's first document is the same
// valid manifest happy_path.yaml carries — already asserted to load in
// TestManifestRoundTripsAllFields — followed by a second document declaring
// an unrecognized top-level field, an unknown block, an illegal direction and
// files that do not exist. A loader that decodes only the first document
// accepts the file with every one of those unread, which is precisely the
// content the strict-field decode of the first document exists to reject.
func TestManifestWithASecondDocumentIsRejected(t *testing.T) {
	_, err := LoadManifest(fixturePath("manifests", "multi_document.yaml"), "")
	requireExpectedHardFailureContains(t, "single-document manifest check", err,
		"multi_document.yaml", "more than one YAML document")
}

// TestCheckedInManifestCoversTheWholeSchemaCorpus pins the size of the
// declared corpus. Every other coverage check the loader runs is mutual —
// each row's files must exist, no file may be claimed twice, and no file in
// the schema directory may go unclaimed — so a manifest and a schema
// directory reduced together stay consistent with one another and pass all
// of them while a message pair has quietly left the set the generator reads.
// Only a count catches that, and the count belongs here rather than in the
// loader: the loader is version-agnostic and is exercised throughout this
// package against small fixture manifests, whereas 64 messages and 128 files
// are facts about this checked-in manifest and the schema corpus it names.
//
// Loading with no schema-directory override is deliberate: it is also how
// the manifest's own schemaDir is exercised, so a schemaDir that does not
// resolve from the manifest's own location fails here rather than the first
// time someone runs the command without the overriding flag.
//
// The two assertions below pin the schema directory's size as well as the
// manifest's, without reading the directory: 128 distinct claimed files that
// the loader has already checked all exist, in a directory the loader has
// already checked holds no unclaimed file, is a directory of exactly 128
// schema files.
func TestCheckedInManifestCoversTheWholeSchemaCorpus(t *testing.T) {
	const (
		wantRows  = 64
		wantFiles = 128
	)

	manifest, err := LoadManifest(filepath.Join("config", "v201.yaml"), "")
	requireImplemented(t, "load of the checked-in manifest with no schema-directory override", err)

	if len(manifest.Messages) != wantRows {
		t.Fatalf("the checked-in manifest declares %d messages, want %d: a dropped row takes a request/response pair out of everything built from this manifest", len(manifest.Messages), wantRows)
	}

	claimed := make(map[string]bool, wantFiles)
	for _, row := range manifest.Messages {
		claimed[row.Request] = true
		claimed[row.Response] = true
	}
	if len(claimed) != wantFiles {
		t.Fatalf("the checked-in manifest claims %d distinct schema files, want %d", len(claimed), wantFiles)
	}
}
