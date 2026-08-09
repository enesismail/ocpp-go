package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestManifestExplicitEmitFalseIsValid(t *testing.T) {
	manifest, err := LoadManifest(fixturePath("manifests", "explicit_emit_false.yaml"), "")
	requireImplemented(t, "explicit emit:false handling", err)
	if len(manifest.Messages) != 1 || manifest.Messages[0].Emit {
		t.Fatalf("present emit:false was treated as an absent required field: %#v", manifest)
	}
}

// TestManifestRoundTripsAllFields pins the happy path: every manifest-level
// field and every per-row field the loader is supposed to carry through
// unchanged, plus the resolved (not raw) schemaDir — resolved relative to
// the manifest file's own directory, not the process's working directory or
// a repository root.
func TestManifestRoundTripsAllFields(t *testing.T) {
	manifestPath := fixturePath("manifests", "happy_path.yaml")
	manifest, err := LoadManifest(manifestPath, "")
	requireImplemented(t, "manifest round trip", err)

	if manifest.Version != "v201" || manifest.GoModule != "github.com/enesismail/ocpp-go" || manifest.GoTree != "ocpp2.0.1" || manifest.TypesPackage != "ocpp2.0.1/types" {
		t.Fatalf("manifest-level fields were not round-tripped: %#v", manifest)
	}

	wantSchemaDir := filepath.Join(filepath.Dir(manifestPath), "schemas", "happy_path")
	if manifest.SchemaDir != wantSchemaDir {
		t.Fatalf("schemaDir was not resolved relative to the manifest file's own directory: got %q, want %q", manifest.SchemaDir, wantSchemaDir)
	}

	if len(manifest.Messages) != 1 {
		t.Fatalf("happy-path manifest returned %d rows, want one", len(manifest.Messages))
	}
	row := manifest.Messages[0]
	if row.Name != "FixtureMessage" || row.Block != "availability" || row.Direction != "CS->CSMS" || row.Request != "FixtureMessageRequest.json" || row.Response != "FixtureMessageResponse.json" || !row.Emit {
		t.Fatalf("manifest row fields were not round-tripped: %#v", row)
	}
}

// TestManifestSchemaDirOverrideReplacesManifestValue exercises the
// schemaDir override: a non-empty override replaces the manifest's own
// schemaDir entirely, rather than being joined with it or with the
// manifest's directory.
func TestManifestSchemaDirOverrideReplacesManifestValue(t *testing.T) {
	override := fixturePath("manifests", "schemas", "happy_path_override")
	manifest, err := LoadManifest(fixturePath("manifests", "happy_path.yaml"), override)
	requireImplemented(t, "manifest schemaDir override", err)
	if manifest.SchemaDir != override {
		t.Fatalf("schemaDir override was not applied verbatim: got %q, want %q", manifest.SchemaDir, override)
	}
}

func TestManifestHardFailures(t *testing.T) {
	tests := []struct {
		file string
		want []string
	}{
		// Every fixture in this table is two rows with the offending field on
		// row 2, and every want names the offending row as well as the
		// offending value. Both halves matter: a check that reports the right
		// complaint without saying which of 64 rows to look at leaves the
		// reader of the message to find it, and a fixture whose only row is
		// the bad one cannot tell a real row identifier apart from an
		// implementation that always says "row 1". Rows are identified by
		// their own name field, or by their 1-indexed position ("row N") when
		// the name is itself the field under test.
		//
		// unclaimed_file.yaml is the one exception, and the only single-row
		// fixture here: no row claims the file it reports, so there is no
		// offending row to name and the filename alone is the whole answer.
		//
		// Coverage boundary, stated honestly: only the name pair
		// (hardfail_a/b) and the emit pair (hardfail_k plus
		// TestManifestExplicitEmitFalseIsValid) actually distinguish an
		// omitted field from a present-but-invalid one. block/direction/
		// request/response have no legal empty value, so their omitted-key
		// and explicit-"" fixtures (c/d, e/f, g/h, i/j) exercise the same
		// decoded-zero-value validity check under two YAML spellings, not a
		// genuinely separate presence check the way name and emit do.
		{"hardfail_a.yaml", []string{"row 2", "name"}},
		{"hardfail_b.yaml", []string{"row 2", "name"}},
		{"hardfail_c.yaml", []string{"SecondRow", "block"}},
		{"hardfail_d.yaml", []string{"SecondRow", "block"}},
		{"hardfail_e.yaml", []string{"SecondRow", "direction"}},
		{"hardfail_f.yaml", []string{"SecondRow", "direction"}},
		{"hardfail_g.yaml", []string{"SecondRow", "request"}},
		{"hardfail_h.yaml", []string{"SecondRow", "request"}},
		{"hardfail_i.yaml", []string{"SecondRow", "response"}},
		{"hardfail_j.yaml", []string{"SecondRow", "response"}},
		{"hardfail_k.yaml", []string{"SecondRow", "emit"}},
		{"unknown_block.yaml", []string{"SecondRow", "not-a-block"}},
		{"missing_schema.yaml", []string{"SecondRow", "DoesNotExistRequest.json"}},
		{"illegal_direction.yaml", []string{"SecondRow", "server-to-client"}},
		{"unclaimed_file.yaml", []string{"Extra.json"}},
		{"double_claimed_file.yaml", []string{"SecondMessage", "FixtureMessageRequest.json"}},
	}
	for _, test := range tests {
		t.Run(test.file, func(t *testing.T) {
			_, err := LoadManifest(fixturePath("manifests", test.file), "")
			requireExpectedHardFailureContains(t, "manifest validation for "+test.file, err, test.want...)
		})
	}

	// duplicate_name.yaml: two rows share only their name field — each claims
	// its own distinct, existing request/response pair — so a loader that
	// merely checks for double-claimed files (and has no duplicate-name check
	// at all) cannot pass this fixture by accident.
	t.Run("duplicate_name.yaml", func(t *testing.T) {
		_, err := LoadManifest(fixturePath("manifests", "duplicate_name.yaml"), "")
		requireExpectedHardFailureContains(t, "manifest validation for duplicate_name.yaml", err, "FixtureMessage", "row 2")
	})
}

// TestManifestUnrecognizedTopLevelFieldIsWrappedOneLine and
// TestManifestUnrecognizedRowFieldIsWrappedOneLine are split out of the
// table above because they carry an extra assertion the other hard-fail
// fixtures don't: yaml.v3's own KnownFields error is multi-line raw text,
// and the loader must wrap it into a single line naming the offending field
// rather than surfacing that text verbatim.
func TestManifestUnrecognizedTopLevelFieldIsWrappedOneLine(t *testing.T) {
	_, err := LoadManifest(fixturePath("manifests", "unknown_field.yaml"), "")
	requireExpectedHardFailureContains(t, "manifest validation for unknown_field.yaml", err, "unexpected")
	if err != nil && strings.Contains(err.Error(), "\n") {
		t.Fatalf("unrecognized top-level field error leaked yaml.v3's raw multi-line text instead of a wrapped one-line message: %q", err.Error())
	}
}

func TestManifestUnrecognizedRowFieldIsWrappedOneLine(t *testing.T) {
	_, err := LoadManifest(fixturePath("manifests", "row_unknown_field.yaml"), "")
	requireExpectedHardFailureContains(t, "manifest validation for row_unknown_field.yaml", err, "unexpected")
	if err != nil && strings.Contains(err.Error(), "\n") {
		t.Fatalf("unrecognized row field error leaked yaml.v3's raw multi-line text instead of a wrapped one-line message: %q", err.Error())
	}
}
