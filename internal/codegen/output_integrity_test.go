package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestManifestTypesImportPathCompletesAModuleRelativePackage pins the one
// rule that turns the manifest's two independent facts — the module path
// the generated tree lives under, and the shared types package — into the
// single string a generated message file may legally write into its import
// block.
//
// The two spellings below are both real. The checked-in v201 manifest
// spells typesPackage the way it spells goTree, relative to the module root
// ("ocpp2.0.1/types"), which is not an import path any module can resolve;
// the emitter fixtures spell it out in full
// ("example.test/widgets/ocpp2.0.1/types"), which is. A rule that handled
// only the second would emit an unresolvable import for every real message
// file while every fixture stayed green, so both shapes are asserted here,
// and the fixture row carries the exact path widget_message.golden pins —
// the composition has to reproduce that byte for byte.
func TestManifestTypesImportPathCompletesAModuleRelativePackage(t *testing.T) {
	cases := []struct {
		name     string
		manifest Manifest
		want     string
	}{
		{
			name:     "module-relative package is completed with the module path",
			manifest: Manifest{GoModule: "github.com/enesismail/ocpp-go", GoTree: "ocpp2.0.1", TypesPackage: "ocpp2.0.1/types"},
			want:     "github.com/enesismail/ocpp-go/ocpp2.0.1/types",
		},
		{
			name:     "already module-qualified package is left exactly as written",
			manifest: Manifest{GoModule: "example.test/widgets", GoTree: "ocpp2.0.1", TypesPackage: "example.test/widgets/ocpp2.0.1/types"},
			want:     "example.test/widgets/ocpp2.0.1/types",
		},
		{
			name:     "the whole-file golden's own manifest still resolves to the path it pins",
			manifest: wholeFileManifest(),
			want:     "example.test/widgets/ocpp2.0.1/types",
		},
		{
			name:     "a package that is the module itself is not doubled",
			manifest: Manifest{GoModule: "example.test/types", TypesPackage: "example.test/types"},
			want:     "example.test/types",
		},
		{
			name:     "surrounding slashes never produce an empty path segment",
			manifest: Manifest{GoModule: "example.test/mod/", TypesPackage: "/ocpp2.0.1/types"},
			want:     "example.test/mod/ocpp2.0.1/types",
		},
		{
			name:     "a manifest with no module has nothing to complete the package with",
			manifest: Manifest{TypesPackage: "ocpp2.0.1/types"},
			want:     "ocpp2.0.1/types",
		},
		{
			// A near miss of the prefix test: "example.test/mod" is a prefix
			// of the string "example.test/modular/..." but not of its path,
			// so the module path must still be prepended.
			name:     "a module path that is only a text prefix of the package is not a path prefix",
			manifest: Manifest{GoModule: "example.test/mod", TypesPackage: "example.test/modular/types"},
			want:     "example.test/mod/example.test/modular/types",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.manifest.TypesImportPath(); got != tc.want {
				t.Fatalf("TypesImportPath() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestEmitterModuleRelativeTypesPackageEmitsABuildableImport is the same
// rule proved where it matters: through the emitter, into a real Go build.
//
// The fixture is deliberately the existing compile fixture with exactly one
// value changed — typesPackage written the way the checked-in v201 manifest
// writes it, relative to the module root instead of spelled out in full —
// so the only thing that can move the outcome is the import composition.
// The scratch module writes `module example.test/ocpp`, so the generated
// message file must name example.test/ocpp/ocpp2.0.1/types; a file that
// named the manifest's raw value would not compile, and `go build` is what
// says so.
func TestEmitterModuleRelativeTypesPackageEmitsABuildableImport(t *testing.T) {
	dir := t.TempDir()
	writeCompileSupport(t, dir)

	manifest := compileManifest()
	manifest.TypesPackage = "ocpp2.0.1/types"
	err := EmitGo(compileIR(), manifest, dir, EmitterOptions{Transform: TransformConfig{Initialisms: []string{"ID"}}})
	requireImplemented(t, "emission from a module-relative types package", err)

	source, err := os.ReadFile(filepath.Join(dir, "ocpp2.0.1", "demo", "compile.go"))
	if err != nil {
		t.Fatalf("generated block-package message file missing: %v", err)
	}
	imports := importBlock(t, string(source))
	if !strings.Contains(imports, `"example.test/ocpp/ocpp2.0.1/types"`) {
		t.Errorf("generated import block does not name the types package through the module path: %q", imports)
	}
	// The quoted form anchors the negative: the module-qualified path above
	// ends in the same characters, but never in the same characters
	// preceded by a quote.
	if strings.Contains(imports, `"ocpp2.0.1/types"`) {
		t.Errorf("generated import block carries the manifest's raw module-relative value, which no module can resolve: %q", imports)
	}

	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	// GOPROXY=off keeps the build offline: an unresolvable import must fail
	// as an unresolvable import, not as a network fetch attempt.
	cmd.Env = append(os.Environ(), "GOPROXY=off")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated scratch tree does not compile: %v\n%s", err, output)
	}
}

// TestMappingArrayOfEnumValidatesEachElement pins the tag an array of enum
// values carries. The fixture reproduces the corpus shape exactly:
// ChargingProfileCriterionType.chargingLimitSource is an optional array of
// one to four ChargingLimitSourceEnumType values.
//
// The base tag set already ends in dive, and every token after dive applies
// to the elements rather than to the slice, so the enum's registered
// validator name belongs there — derived from the ELEMENT's enum
// definition, the same derivation a scalar enum property uses. Without it
// the field's type is a named string type whose values nothing checks, and
// a payload carrying an unlisted source string validates cleanly.
//
// The second case is the control that keeps the first from being satisfied
// by appending a token to every diving slice: an array of composites still
// carries dive and nothing after it, because there is no enum to check.
func TestMappingArrayOfEnumValidatesEachElement(t *testing.T) {
	ir := IR{Definitions: []Definition{
		{Name: "ChargingProfileCriterionType", Kind: DefinitionObject, Files: []string{"GetChargingProfilesRequest.json"}, Properties: []Property{
			{Name: "chargingLimitSource", Type: "array", MinItems: intPtr(1), MaxItems: intPtr(4), Items: &Property{
				Ref:            "#/definitions/ChargingLimitSourceEnumType",
				EnumDefinition: "ChargingLimitSourceEnumType",
				Enum:           []string{"EMS", "Other", "SO", "CSO"},
			}},
			{Name: "chargingProfile", Type: "array", MaxItems: intPtr(4), Items: &Property{
				Ref: "#/definitions/ChargingProfileType",
			}},
		}},
		{Name: "ChargingLimitSourceEnumType", Kind: DefinitionEnum, Values: []string{"EMS", "Other", "SO", "CSO"}},
		{Name: "ChargingProfileType", Kind: DefinitionObject},
	}}
	definition := findDefinition(ir.Definitions, "ChargingProfileCriterionType")
	if definition == nil {
		t.Fatal("fixture is missing ChargingProfileCriterionType")
	}
	context := MappingContext{Definitions: ir.Definitions, Transform: TransformConfig{Initialisms: []string{"ID"}}}

	cases := []struct {
		property string
		want     string
	}{
		{
			property: "chargingLimitSource",
			want:     "ChargingLimitSource []ChargingLimitSourceType `json:\"chargingLimitSource,omitempty\" validate:\"omitempty,min=1,max=4,dive,chargingLimitSourceType201\"`",
		},
		{
			property: "chargingProfile",
			want:     "ChargingProfile []ChargingProfile `json:\"chargingProfile,omitempty\" validate:\"omitempty,max=4,dive\"`",
		},
	}
	for _, tc := range cases {
		t.Run(tc.property, func(t *testing.T) {
			property := findProperty(definition.Properties, tc.property)
			if property == nil {
				t.Fatalf("fixture is missing property %s", tc.property)
			}
			got, err := RenderProperty(*definition, *property, context, nil)
			requireImplemented(t, "array element tag derivation", err)
			if strings.TrimSpace(got) != tc.want {
				t.Fatalf("rendered field =\n%s\nwant\n%s", strings.TrimSpace(got), tc.want)
			}
		})
	}

	// Stated separately from the byte comparison above: the element check
	// is worthless in front of dive, where it would be applied to the slice
	// itself rather than to its values.
	property := findProperty(definition.Properties, "chargingLimitSource")
	rendered, err := RenderProperty(*definition, *property, context, nil)
	requireImplemented(t, "array element tag ordering", err)
	dive, enum := strings.Index(rendered, "dive"), strings.Index(rendered, "chargingLimitSourceType201")
	if dive < 0 || enum < 0 || dive > enum {
		t.Fatalf("the element validator must follow dive, not precede it: %s", strings.TrimSpace(rendered))
	}
}

// twoMessagesInOneBlockIR builds a corpus-shaped conflict: Alpha reaches a
// composite whose rendered Go name, BetaRequest, is also the name of Beta's
// own request root. Placement sends the composite to Alpha's file and the
// root is emitted in Beta's, which is fine as long as the two files are two
// packages — and fatal as soon as they are not. block is what decides that.
func twoMessagesInOneBlockIR(alphaBlock, betaBlock string) (IR, PlacementPlan) {
	ir := IR{
		Definitions: []Definition{
			{Name: "AlphaRequest", Kind: DefinitionRoot, Files: []string{"AlphaRequest.json"}, Properties: []Property{
				{Name: "detail", Ref: "#/definitions/BetaRequestType"},
			}},
			{Name: "AlphaResponse", Kind: DefinitionRoot, Files: []string{"AlphaResponse.json"}},
			{Name: "BetaRequestType", Kind: DefinitionObject, Files: []string{"alpha_detail.json"}, Properties: []Property{
				{Name: "label", Type: "string"},
			}},
			{Name: "BetaRequest", Kind: DefinitionRoot, Files: []string{"BetaRequest.json"}},
			{Name: "BetaResponse", Kind: DefinitionRoot, Files: []string{"BetaResponse.json"}},
		},
		Messages: []Message{
			{Name: "Alpha", Block: alphaBlock, Direction: "CS->CSMS", Request: "AlphaRequest.json", Response: "AlphaResponse.json", RequestRoot: "AlphaRequest", ResponseRoot: "AlphaResponse", Roots: []string{"AlphaRequest", "AlphaResponse"}, Reach: []string{"BetaRequestType"}, Emit: true},
			{Name: "Beta", Block: betaBlock, Direction: "CS->CSMS", Request: "BetaRequest.json", Response: "BetaResponse.json", RequestRoot: "BetaRequest", ResponseRoot: "BetaResponse", Roots: []string{"BetaRequest", "BetaResponse"}, Emit: true},
		},
	}
	return ir, PlacementPlan{Home: map[string]string{"BetaRequestType": "message:Alpha"}}
}

// TestEmitterCollisionBetweenTwoMessagesInOneBlockHardFails covers the
// scope the per-message index cannot see. Placement names a FILE, but Go
// enforces package-level identifiers across every file of a package at
// once, and the sixteen-block layout puts several messages in each package
// — availability alone holds Heartbeat and its neighbours. A composite
// placed in one message's file and a root, constructor or Feature symbol
// emitted into another's are package-mates, so they share one index.
//
// The wants pair each identifier with the role and file only one side has:
// BetaRequest is a substring of BetaRequestType, so identifier text alone
// cannot tell the two sides apart, and both emitting files are named
// because the package now holds declarations from more than one.
func TestEmitterCollisionBetweenTwoMessagesInOneBlockHardFails(t *testing.T) {
	ir, placement := twoMessagesInOneBlockIR("shared", "shared")
	err := CheckEmissionCollisions(ir, placement, TransformConfig{Initialisms: []string{"ID"}})
	requireExpectedHardFailureContains(t, "same-package collision across two messages", err,
		"in package shared",
		"object BetaRequestType (alpha_detail.json)",
		"root BetaRequest (BetaRequest.json)",
		"message:Alpha",
		"message:Beta")
}

// TestEmitterSameNameInTwoBlocksIsNotACollision is the control for the
// fixture above, and the reason its index cannot simply merge everything:
// the identical pair of declarations is collision-free the moment the two
// messages sit in different blocks, because two Go packages may each
// declare a BetaRequest exactly as two real packages may each declare a
// Config.
func TestEmitterSameNameInTwoBlocksIsNotACollision(t *testing.T) {
	ir, placement := twoMessagesInOneBlockIR("one", "two")
	err := CheckEmissionCollisions(ir, placement, TransformConfig{Initialisms: []string{"ID"}})
	requireImplemented(t, "collision-free coexistence of one name in two blocks", err)
}

// TestWriterFailedWriteLeavesTheExistingFileIntact pins the writer's
// replace-or-leave-alone contract. A generated file is only ever read back
// as a whole — the writer's sole notion of "unchanged" is a byte comparison
// against whatever the file currently holds — so a write that truncated the
// destination and then failed would leave behind a file that is neither the
// old output nor the new one, and that the next run would happily accept as
// simply "different".
//
// The injected failure is a destination directory that accepts no new
// files. Writing through a temporary file in that directory cannot start;
// writing in place would open the existing file (whose own mode still
// permits it), truncate it, and succeed — replacing content the run was
// never able to commit to.
func TestWriterFailedWriteLeavesTheExistingFileIntact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "types_gen.go")
	original := []byte("// Code generated by internal/codegen. DO NOT EDIT.\n\npackage types\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	// Registered after t.TempDir's own cleanup and therefore run before it:
	// a directory left read-only cannot be removed.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	// A process that ignores directory permissions (a root-owned container,
	// for one) cannot have this failure injected at all, and a test that
	// silently passed there would be asserting nothing.
	if probe, err := os.CreateTemp(dir, ".probe.*"); err == nil {
		name := probe.Name()
		probe.Close()
		os.Remove(name)
		t.Skip("this process can still create files in a read-only directory, so the write failure cannot be injected here")
	}

	err := writeIfChanged(path, []byte("// Code generated by internal/codegen. DO NOT EDIT.\n\npackage types\n\ntype Replacement struct{}\n"))
	if err == nil {
		t.Fatal("writeIfChanged reported success although the destination directory accepts no new file")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("write failure %v does not name the file it could not write", err)
	}

	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("the destination file is unreadable after a failed write: %v", readErr)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("a failed write changed the existing file:\ngot\n%s\nwant\n%s", got, original)
	}
}
