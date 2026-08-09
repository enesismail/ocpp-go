package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMappingArrayElementScalarConstraintsAreEmittedAfterDive pins the tags an
// array whose ELEMENT schema carries scalar bounds must render.
//
// go-playground applies every token before dive to the slice itself and every
// token after it to the values, so an element bound rendered without dive says
// something entirely different from what the schema declares: max=512 in front
// of dive caps the number of elements, not the length of each one. The first
// case reproduces the only shape in the vendored corpus that has an element
// bound at all — an optional array of at least one string, each at most 512
// characters — and the field it produces is the one place where a missing
// element bound lets an over-long value validate cleanly.
//
// The remaining cases fix the derivation's edges: element bounds follow the
// same kind-sensitive mapping a scalar property's own bounds follow (length
// bounds render min/max, value bounds render gte/lte, min before max and gte
// before lte), an element enum's registered validator still renders last, and
// an array whose elements declare no bounds at all keeps exactly the tag it
// has today — dive is what separates slice tokens from element tokens, so an
// array with nothing to say about its elements must not emit it.
func TestMappingArrayElementScalarConstraintsAreEmittedAfterDive(t *testing.T) {
	ir := IR{Definitions: []Definition{
		{Name: "ElementBoundFixture", Kind: DefinitionObject, Files: []string{"ElementBoundFixture.json"}, Properties: []Property{
			// The corpus shape, copied field for field from
			// PublishFirmwareStatusNotificationRequest.location.
			{Name: "location", Type: "array", MinItems: intPtr(1), Items: &Property{
				Type: "string", MaxLength: intPtr(512),
			}},
			{Name: "codes", Type: "array", Required: true, MinItems: intPtr(1), MaxItems: intPtr(4), Items: &Property{
				Type: "string", MinLength: intPtr(2), MaxLength: intPtr(20),
			}},
			{Name: "percentages", Type: "array", Items: &Property{
				Type: "number", Minimum: float64Ptr(0), Maximum: float64Ptr(100),
			}},
			{Name: "sources", Type: "array", MaxItems: intPtr(4), Items: &Property{
				Ref:            "#/definitions/SourceEnumType",
				EnumDefinition: "SourceEnumType",
				MaxLength:      intPtr(36),
			}},
			// The negative control: no element bound, so no dive.
			{Name: "values", Type: "array", MinItems: intPtr(1), MaxItems: intPtr(3), Items: &Property{
				Type: "string",
			}},
			// The other negative control: a composite element still dives and
			// still renders nothing after it.
			{Name: "devices", Type: "array", MaxItems: intPtr(1024), Items: &Property{
				Ref: "#/definitions/DeviceType",
			}},
		}},
		{Name: "SourceEnumType", Kind: DefinitionEnum, Values: []string{"EMS", "Other"}},
		{Name: "DeviceType", Kind: DefinitionObject},
	}}
	definition := findDefinition(ir.Definitions, "ElementBoundFixture")
	if definition == nil {
		t.Fatal("fixture is missing ElementBoundFixture")
	}
	context := MappingContext{Definitions: ir.Definitions, Transform: TransformConfig{Initialisms: []string{"ID"}}}

	cases := []struct {
		property string
		want     string
	}{
		{
			property: "location",
			want:     "Location []string `json:\"location,omitempty\" validate:\"omitempty,min=1,dive,max=512\"`",
		},
		{
			property: "codes",
			want:     "Codes []string `json:\"codes\" validate:\"required,min=1,max=4,dive,min=2,max=20\"`",
		},
		{
			property: "percentages",
			want:     "Percentages []float64 `json:\"percentages,omitempty\" validate:\"omitempty,dive,gte=0,lte=100\"`",
		},
		{
			property: "sources",
			want:     "Sources []SourceType `json:\"sources,omitempty\" validate:\"omitempty,max=4,dive,max=36,sourceType201\"`",
		},
		{
			property: "values",
			want:     "Values []string `json:\"values,omitempty\" validate:\"omitempty,min=1,max=3\"`",
		},
		{
			property: "devices",
			want:     "Devices []Device `json:\"devices,omitempty\" validate:\"omitempty,max=1024,dive\"`",
		},
	}
	for _, tc := range cases {
		t.Run(tc.property, func(t *testing.T) {
			property := findProperty(definition.Properties, tc.property)
			if property == nil {
				t.Fatalf("fixture is missing property %s", tc.property)
			}
			got, err := RenderProperty(*definition, *property, context, nil)
			requireImplemented(t, "array element bound derivation", err)
			if strings.TrimSpace(got) != tc.want {
				t.Fatalf("rendered field =\n%s\nwant\n%s", strings.TrimSpace(got), tc.want)
			}
		})
	}

	// Stated separately from the byte comparisons above, because a rendering
	// that emitted the element bound in front of dive would still contain
	// every token the strings above name: an element bound is only an element
	// bound where it follows dive.
	t.Run("every element token follows dive", func(t *testing.T) {
		property := findProperty(definition.Properties, "location")
		rendered, err := RenderProperty(*definition, *property, context, nil)
		requireImplemented(t, "array element bound ordering", err)
		dive, bound := strings.Index(rendered, "dive"), strings.Index(rendered, "max=512")
		if dive < 0 {
			t.Fatalf("an array carrying an element bound must dive: %s", strings.TrimSpace(rendered))
		}
		if bound < 0 || dive > bound {
			t.Fatalf("the element bound must follow dive, not precede it: %s", strings.TrimSpace(rendered))
		}
	})
}

// TestMappingRealCorpusElementBoundIsRenderedFromTheVendoredSchema is the same
// rule proved against the vendored corpus rather than a hand-built fixture: it
// walks the real schema directory, finds every array property whose element
// schema declares a bound, and asserts each one renders that bound after dive.
//
// The hand-built fixture above can only prove the mapping handles the shape;
// this proves the shape is actually reached from the schemas as they are
// vendored, through the walk that produces the definitions the emitter sees.
// It is written as a sweep rather than as an assertion about one named
// property so that a schema revision adding a second such array is covered the
// day it lands, and it names a floor of one so an empty sweep cannot pass.
func TestMappingRealCorpusElementBoundIsRenderedFromTheVendoredSchema(t *testing.T) {
	manifest, err := LoadManifest(filepath.Join("config", "v201.yaml"), "")
	requireImplemented(t, "loading the checked-in manifest", err)
	ir, err := WalkSchemas(manifest)
	requireImplemented(t, "walking the vendored schema corpus", err)
	transform, err := LoadTransformConfig(fixturePath("config", "transform.yaml"))
	requireImplemented(t, "loading the initialism table", err)

	checked := 0
	for _, definition := range ir.Definitions {
		for _, property := range definition.Properties {
			if property.Type != "array" || property.Items == nil {
				continue
			}
			if !hasScalarBound(*property.Items) {
				continue
			}
			checked++
			mapping, err := MapProperty(definition, property, ir.Definitions, transform, nil)
			requireImplemented(t, "mapping "+definition.Name+"."+property.Name, err)
			tag := mapping.ValidateTag
			dive := strings.Index(tag, "dive")
			if dive < 0 {
				t.Errorf("%s.%s declares element bounds but renders no dive: %s", definition.Name, property.Name, tag)
				continue
			}
			if property.Items.MaxLength != nil {
				bound := strings.Index(tag, "max=")
				if !strings.Contains(tag[dive:], "max=") || bound < 0 {
					t.Errorf("%s.%s drops its element maxLength: %s", definition.Name, property.Name, tag)
				}
			}
		}
	}
	if checked < 1 {
		t.Fatalf("the vendored corpus declared no array with element bounds, so this test asserted nothing")
	}
}

// hasScalarBound reports whether an array's element schema declares any bound
// the tag table maps to a validator token.
func hasScalarBound(item Property) bool {
	return item.MaxLength != nil || item.MinLength != nil || item.Minimum != nil || item.Maximum != nil
}

// enumConstantIR builds a package holding one enum and, optionally, an object
// definition whose rendered name is a second claim on one of that enum's
// constant names. Both land in the shared types package, so every identifier
// below is a package-level identifier of one Go package.
func enumConstantIR(values []string, extraObject string) (IR, PlacementPlan) {
	definitions := []Definition{
		{Name: "StateEnumType", Kind: DefinitionEnum, Files: []string{"StateEnumType.json"}, Values: values},
	}
	home := map[string]string{"StateEnumType": "types"}
	if extraObject != "" {
		definitions = append(definitions, Definition{Name: extraObject, Kind: DefinitionObject, Files: []string{"Extra.json"}})
		home[extraObject] = "types"
	}
	return IR{Definitions: definitions}, PlacementPlan{Home: home}
}

// TestEmitterEnumConstantCollisionWithinOneEnumHardFails covers the namespace
// the collision index could not previously see at all: the constants an enum
// block emits.
//
// An enum constant's name is derived from the raw schema value by discarding
// the value's own punctuation, so two values that differ only in which
// separator they use — "A-B" and "A.B" — derive one identifier. Nothing about
// the enum's type name or its validator tag is ambiguous, so every other index
// passes; emission succeeds, and the failure surfaces as a redeclaration error
// from the Go compiler against a file no one is allowed to hand-edit. Naming
// both raw values is what makes the failure actionable: the constant alone
// says which identifier was claimed twice, never which two schema values
// claimed it.
func TestEmitterEnumConstantCollisionWithinOneEnumHardFails(t *testing.T) {
	ir, placement := enumConstantIR([]string{"A-B", "A.B"}, "")
	err := CheckEmissionCollisions(ir, placement, TransformConfig{Initialisms: []string{"ID"}})
	requireExpectedHardFailureContains(t, "two enum values deriving one constant", err,
		"StateTypeAB",
		`"A-B"`,
		`"A.B"`,
		"StateEnumType",
		"StateEnumType.json")
}

// TestEmitterEnumConstantCollisionWithADeclarationHardFails covers the other
// half of the same namespace: a constant colliding with a declaration of a
// different kind in the same package. Go enforces one flat namespace over a
// package's constants, types and functions alike, so an object whose rendered
// name equals an emitted constant's is exactly as fatal as two constants
// sharing a name — and is invisible to an index that only compares constants
// against each other.
func TestEmitterEnumConstantCollisionWithADeclarationHardFails(t *testing.T) {
	ir, placement := enumConstantIR([]string{"Detail"}, "StateTypeDetailType")
	err := CheckEmissionCollisions(ir, placement, TransformConfig{Initialisms: []string{"ID"}})
	requireExpectedHardFailureContains(t, "an enum constant colliding with an object declaration", err,
		"StateTypeDetail",
		`"Detail"`,
		"StateEnumType",
		"StateTypeDetailType")
}

// TestEmitterDistinctEnumConstantsAreNotACollision is the negative control for
// both cases above, and the reason the index cannot simply reject any two
// constants sharing a prefix: values that derive genuinely different
// identifiers coexist, and so does an object whose rendered name merely
// resembles one.
func TestEmitterDistinctEnumConstantsAreNotACollision(t *testing.T) {
	ir, placement := enumConstantIR([]string{"A-B", "A-C", "Detail"}, "StateTypeDetailsType")
	err := CheckEmissionCollisions(ir, placement, TransformConfig{Initialisms: []string{"ID"}})
	requireImplemented(t, "distinct enum constants coexisting in one package", err)
}

// TestEmitterVendoredCorpusEmitsNoCollidingConstant runs the extended index
// over every message the checked-in manifest declares, not only the four it
// emits today: a constant collision is a property of the schema corpus, so the
// day an emit flag flips on is the wrong day to discover one.
func TestEmitterVendoredCorpusEmitsNoCollidingConstant(t *testing.T) {
	manifest, err := LoadManifest(filepath.Join("config", "v201.yaml"), "")
	requireImplemented(t, "loading the checked-in manifest", err)
	ir, err := WalkSchemas(manifest)
	requireImplemented(t, "walking the vendored schema corpus", err)
	transform, err := LoadTransformConfig(fixturePath("config", "transform.yaml"))
	requireImplemented(t, "loading the initialism table", err)

	placement, err := ComputePlacement(ir)
	requireImplemented(t, "placing the vendored corpus", err)
	err = CheckEmissionCollisions(ir, placement, transform)
	requireImplemented(t, "the collision sweep over the vendored corpus", err)
}

// stagedEmissionIR builds two messages in one block. Beta's request root is
// present or absent depending on withBetaRoot: absent, Beta cannot render, and
// the run fails after Alpha has already rendered successfully.
func stagedEmissionIR(alphaLabelRequired bool, withBetaRoot bool) (IR, Manifest) {
	alphaRequest := Definition{Name: "AlphaRequest", Kind: DefinitionRoot, Files: []string{"AlphaRequest.json"}, Properties: []Property{
		{Name: "label", Type: "string", Required: alphaLabelRequired},
	}}
	definitions := []Definition{
		alphaRequest,
		{Name: "AlphaResponse", Kind: DefinitionRoot, Files: []string{"AlphaResponse.json"}},
		{Name: "BetaResponse", Kind: DefinitionRoot, Files: []string{"BetaResponse.json"}},
	}
	if withBetaRoot {
		definitions = append(definitions, Definition{Name: "BetaRequest", Kind: DefinitionRoot, Files: []string{"BetaRequest.json"}})
	}
	ir := IR{
		Definitions: definitions,
		Messages: []Message{
			{Name: "Alpha", Block: "demo", Direction: "CS->CSMS", Request: "AlphaRequest.json", Response: "AlphaResponse.json", RequestRoot: "AlphaRequest", ResponseRoot: "AlphaResponse", Roots: []string{"AlphaRequest", "AlphaResponse"}, Emit: true},
			{Name: "Beta", Block: "demo", Direction: "CS->CSMS", Request: "BetaRequest.json", Response: "BetaResponse.json", RequestRoot: "BetaRequest", ResponseRoot: "BetaResponse", Roots: []string{"BetaRequest", "BetaResponse"}, Emit: true},
		},
	}
	manifest := Manifest{
		Version: "v201", GoModule: "example.test/ocpp", GoTree: "ocpp2.0.1", TypesPackage: "example.test/ocpp/ocpp2.0.1/types",
		Messages: []ManifestMessage{
			{Name: "Alpha", Block: "demo", Direction: "CS->CSMS", Request: "AlphaRequest.json", Response: "AlphaResponse.json", Emit: true},
			{Name: "Beta", Block: "demo", Direction: "CS->CSMS", Request: "BetaRequest.json", Response: "BetaResponse.json", Emit: true},
		},
	}
	return ir, manifest
}

// treeSnapshot reads every file under root, keyed by its root-relative path.
// Unlike generatedFiles it filters nothing by extension: a run that leaves a
// stray temporary behind has damaged the tree just as surely as one that left
// a half-written .go file, and only an unfiltered read can say so.
func treeSnapshot(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[relative] = content
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return files
}

// TestEmitterRenderFailureLeavesTheTreeUntouched pins the whole run's
// all-or-nothing contract, which is a level above the per-file atomicity the
// writer already provides.
//
// A generated tree is only meaningful as one generation: every file in it was
// produced by the same rules from the same inputs, and that is precisely the
// property anyone reading a regeneration diff, and a drift check comparing
// the tree against a rerun, both rely on. A run that renders and writes file by
// file breaks that the moment any later file fails to render — the tree then
// holds some files from the new generation and some from the old, in a state no
// input produces and no rerun reproduces, and the failure the operator sees
// names only the file that could not be rendered.
//
// The fixture emits a valid tree first, then reruns with two changes at once:
// Alpha's own output differs (its one field flips from optional to required, so
// its rendered tags change), and Beta's request root is missing, so Beta cannot
// render. Alpha is rendered before Beta, so a run that writes as it goes
// commits Alpha's new content and then fails.
func TestEmitterRenderFailureLeavesTheTreeUntouched(t *testing.T) {
	dir := t.TempDir()
	options := EmitterOptions{Transform: TransformConfig{Initialisms: []string{"ID"}}}

	goodIR, manifest := stagedEmissionIR(false, true)
	err := EmitGo(goodIR, manifest, dir, options)
	requireImplemented(t, "the first, complete emission", err)
	before := treeSnapshot(t, dir)
	if len(before) == 0 {
		t.Fatal("the first emission wrote no file, so there is nothing for the failing run to damage")
	}

	brokenIR, _ := stagedEmissionIR(true, false)
	err = EmitGo(brokenIR, manifest, dir, options)
	if err == nil {
		t.Fatal("emission succeeded although Beta's request root is absent from the IR")
	}
	if !strings.Contains(err.Error(), "BetaRequest") {
		t.Errorf("the emission failure does not name the missing root: %v", err)
	}

	after := treeSnapshot(t, dir)
	for _, path := range sortedKeys(after) {
		original, ok := before[path]
		if !ok {
			t.Errorf("the failed run left a new file behind: %s", path)
			continue
		}
		if !bytes.Equal(original, after[path]) {
			t.Errorf("the failed run replaced %s with content from a generation it never completed:\ngot\n%s\nwant\n%s", path, after[path], original)
		}
	}
	for _, path := range sortedKeys(before) {
		if _, ok := after[path]; !ok {
			t.Errorf("the failed run removed %s", path)
		}
	}
}

// TestEmitterRenderFailureWritesNothingIntoAnEmptyTree is the same contract
// stated where there is no previous generation to compare against: a first run
// that cannot render every file must leave no file at all, rather than a
// partial tree that looks to the next run — whose only notion of "already
// current" is a byte comparison per file — like a tree someone deliberately
// generated.
func TestEmitterRenderFailureWritesNothingIntoAnEmptyTree(t *testing.T) {
	dir := t.TempDir()
	brokenIR, manifest := stagedEmissionIR(false, false)
	err := EmitGo(brokenIR, manifest, dir, EmitterOptions{Transform: TransformConfig{Initialisms: []string{"ID"}}})
	if err == nil {
		t.Fatal("emission succeeded although Beta's request root is absent from the IR")
	}
	written := treeSnapshot(t, dir)
	if len(written) != 0 {
		t.Fatalf("a run that could not render every file still wrote %v", sortedKeys(written))
	}
}

// destinationPathIR builds one message per (name, block) pair, each with the
// two roots a message file needs and nothing else, so the only thing the
// resulting emission can differ in is where its files land.
func destinationPathIR(rows [][2]string) (IR, Manifest) {
	var ir IR
	var manifest Manifest
	manifest.Version = "v201"
	manifest.GoModule = "example.test/ocpp"
	manifest.GoTree = "ocpp2.0.1"
	manifest.TypesPackage = "example.test/ocpp/ocpp2.0.1/types"
	for _, row := range rows {
		name, block := row[0], row[1]
		request, response := name+"Request", name+"Response"
		ir.Definitions = append(ir.Definitions,
			Definition{Name: request, Kind: DefinitionRoot, Files: []string{request + ".json"}},
			Definition{Name: response, Kind: DefinitionRoot, Files: []string{response + ".json"}},
		)
		ir.Messages = append(ir.Messages, Message{
			Name: name, Block: block, Direction: "CS->CSMS",
			Request: request + ".json", Response: response + ".json",
			RequestRoot: request, ResponseRoot: response,
			Roots: []string{request, response}, Emit: true,
		})
		manifest.Messages = append(manifest.Messages, ManifestMessage{
			Name: name, Block: block, Direction: "CS->CSMS",
			Request: request + ".json", Response: response + ".json", Emit: true,
		})
	}
	return ir, manifest
}

// emitIntoTempDir runs a full emission into a fresh directory and reports the
// error together with everything the run left on disk, so every destination
// test can assert the hard failure and the all-or-nothing contract at once.
func emitIntoTempDir(t *testing.T, ir IR, manifest Manifest) (error, map[string][]byte) {
	t.Helper()
	dir := t.TempDir()
	err := EmitGo(ir, manifest, dir, EmitterOptions{Transform: TransformConfig{Initialisms: []string{"ID", "EV"}}})
	return err, treeSnapshot(t, dir)
}

// TestEmitterTwoMessagesTargetingOneFileHardFails covers the file namespace,
// which no identifier index can see.
//
// A message's file name is its message name reduced to lowercase words joined
// by underscores, and that reduction is not injective: NotifyEVChargingNeeds
// and NotifyEvChargingNeeds differ as identifiers — they derive different root
// types, a different Feature struct and a different FeatureName const, so every
// package-level index passes them — and reduce to one file name. Emitting both
// into one block writes one file twice and keeps whichever message the manifest
// happens to list second, silently, with no diagnostic anywhere: the run exits
// zero, the tree looks complete, and one message is simply absent from it.
//
// The two names are the corpus's own shape: NotifyEVChargingNeeds is a real
// message in the vendored manifest, and the initialism table is what makes its
// EV survive as one word.
func TestEmitterTwoMessagesTargetingOneFileHardFails(t *testing.T) {
	ir, manifest := destinationPathIR([][2]string{
		{"NotifyEVChargingNeeds", "smartcharging"},
		{"NotifyEvChargingNeeds", "smartcharging"},
	})
	err, written := emitIntoTempDir(t, ir, manifest)
	requireExpectedHardFailureContains(t, "two messages emitting one file", err,
		"NotifyEVChargingNeeds",
		"NotifyEvChargingNeeds",
		filepath.Join("ocpp2.0.1", "smartcharging", "notify_ev_charging_needs.go"))
	if len(written) != 0 {
		t.Errorf("a run that hard-failed on a destination collision still wrote %v", sortedKeys(written))
	}
}

// TestEmitterMessageCollidingWithTheSharedTypesFileHardFails proves the shared
// types file is indexed alongside the message files rather than written past
// them.
//
// The shared file's path is fixed — the types directory under the generated
// tree, holding types_gen.go — and nothing stops a manifest row from naming
// that same place: the block is validated against the directories that exist
// under the generated tree, and the types directory is one of them, so a row
// with block "types" is accepted. A message named TypesGen in it reduces to
// types_gen.go, which is the shared file exactly. The shared file is written
// first, so without a shared index it is the one that silently disappears — and
// it is the file every other generated file imports.
func TestEmitterMessageCollidingWithTheSharedTypesFileHardFails(t *testing.T) {
	ir, manifest := destinationPathIR([][2]string{{"TypesGen", "types"}})
	err, written := emitIntoTempDir(t, ir, manifest)
	requireExpectedHardFailureContains(t, "a message claiming the shared types file's path", err,
		"TypesGen",
		"types",
		filepath.Join("ocpp2.0.1", "types", "types_gen.go"))
	if len(written) != 0 {
		t.Errorf("a run that hard-failed on a destination collision still wrote %v", sortedKeys(written))
	}
}

// TestEmitterDestinationPathsDifferingOnlyInCaseHardFail pins the deliberate
// decision to compare destination paths case-insensitively.
//
// Two paths differing only in case are two files on Linux and one file on
// macOS and Windows, whose filesystems are case-insensitive by default. A
// case-sensitive index would therefore pass on a case-sensitive CI runner while
// the same inputs silently dropped a file on a developer's machine — the worst
// available outcome, because the check exists precisely to guarantee that every
// file the emitter renders reaches disk. Folding can only ever make the check
// stricter, and stricter is never wrong here: no rule in this generator
// deliberately produces two files whose paths differ only in case, so there is
// no legitimate output for the fold to reject.
//
// The failure names both original spellings rather than the folded key, since
// the folded key appears in neither input and would send the reader looking for
// a path that does not exist.
func TestEmitterDestinationPathsDifferingOnlyInCaseHardFail(t *testing.T) {
	// The file component of a message's path is always all-lowercase, so a
	// case-only difference can live in the directory component alone — which
	// means the two messages must still reduce to one file name. The pair below
	// does both at once: two distinct identifiers reducing to one file name, in
	// two blocks differing only in case. They are two Go packages, so every
	// identifier index passes them.
	ir, manifest := destinationPathIR([][2]string{
		{"NotifyEVChargingNeeds", "demo"},
		{"NotifyEvChargingNeeds", "Demo"},
	})
	err, written := emitIntoTempDir(t, ir, manifest)
	requireExpectedHardFailureContains(t, "two destination paths differing only in case", err,
		filepath.Join("ocpp2.0.1", "demo", "notify_ev_charging_needs.go"),
		filepath.Join("ocpp2.0.1", "Demo", "notify_ev_charging_needs.go"),
		"case-insensitive")
	if len(written) != 0 {
		t.Errorf("a run that hard-failed on a destination collision still wrote %v", sortedKeys(written))
	}
}

// TestEmitterDistinctDestinationPathsAreNotACollision is the negative control
// for all three cases above, and the reason the index is keyed on the whole
// path rather than on the file name alone: the same message name in two
// different blocks is two directories and two perfectly legitimate files, which
// is how the sixteen-block layout works, and two different names in one block
// are two files as well.
func TestEmitterDistinctDestinationPathsAreNotACollision(t *testing.T) {
	ir, manifest := destinationPathIR([][2]string{
		// The pair that collides in one block, here split across two: one file
		// name, two directories, two entirely legitimate files.
		{"NotifyEVChargingNeeds", "smartcharging"},
		{"NotifyEvChargingNeeds", "availability"},
		// And two different names in one block, the ordinary case.
		{"Reset", "provisioning"},
	})
	err, written := emitIntoTempDir(t, ir, manifest)
	requireImplemented(t, "distinct destination paths coexisting in one run", err)
	if len(written) != 4 {
		t.Fatalf("expected the shared types file plus three message files, got %v", sortedKeys(written))
	}
}

// TestEmitterVendoredCorpusEmitsNoCollidingDestinationPath runs the
// destination index over every message the checked-in manifest declares, not
// only the four it emits today: two names reducing to one file is a property of
// the manifest, so the day an emit flag flips on is the wrong day to discover
// one.
func TestEmitterVendoredCorpusEmitsNoCollidingDestinationPath(t *testing.T) {
	manifest, err := LoadManifest(filepath.Join("config", "v201.yaml"), "")
	requireImplemented(t, "loading the checked-in manifest", err)
	ir, err := WalkSchemas(manifest)
	requireImplemented(t, "walking the vendored schema corpus", err)
	transform, err := LoadTransformConfig(fixturePath("config", "transform.yaml"))
	requireImplemented(t, "loading the initialism table", err)

	for i := range manifest.Messages {
		manifest.Messages[i].Emit = true
	}
	files, err := renderTree(ir, manifest, t.TempDir(), EmitterOptions{Transform: transform})
	requireImplemented(t, "rendering every declared message", err)
	// One file per declared message, plus the shared types file: a rendering
	// that silently dropped one would otherwise satisfy the sweep above.
	if want := len(manifest.Messages) + 1; len(files) != want {
		t.Fatalf("rendered %d files for %d declared messages, want %d", len(files), len(manifest.Messages), want)
	}
}
