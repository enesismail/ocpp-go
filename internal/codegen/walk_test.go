package main

import (
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestSelfReferentialCycleIsRejected(t *testing.T) {
	_, err := WalkSchema(fixturePath("schemas", "cycle_self.json"))
	requireExpectedHardFailureContains(t, "self-referential cycle detection", err, "cycle_self.json", "NodeType")
}

func TestMutualCycleIsRejected(t *testing.T) {
	_, err := WalkSchema(fixturePath("schemas", "cycle_mutual.json"))
	requireExpectedHardFailureContains(t, "mutual cycle detection", err, "cycle_mutual.json", "LeftType", "RightType")
}

// TestCycleDetectionAPIReportsReferencePath pins the reference-path format
// DetectCycles reports: an arrow-joined sequence naming every definition on
// the cycle, starting and ending on the same name for a self-reference —
// "NodeType -> NodeType" here — not merely the definition's name once, so a
// message that could equally describe a hard failure unrelated to cycles is
// distinguishable from one that actually names the path walked.
func TestCycleDetectionAPIReportsReferencePath(t *testing.T) {
	err := DetectCycles([]Definition{{Name: "NodeType", Kind: DefinitionObject, Properties: []Property{{Name: "next", Ref: "#/definitions/NodeType"}}, Files: []string{"cycle_self.json"}}})
	requireExpectedHardFailure(t, "cycle detection API", "NodeType -> NodeType", err)
}

// chainLink names one $ref hop in the transitive_nesting fixture's chain:
// transitive_nesting -> ChargingProfileType -> ChargingScheduleType ->
// SalesTariffType -> SalesTariffEntryType -> ConsumptionCostType -> CostType,
// mirroring the real corpus's deepest observed reference chain
// (SetChargingProfileRequest resolves through the same five intermediate
// shared types before reaching CostType), so the reach set walks at least
// that deep, not only the first $ref target.
type chainLink struct {
	definition string
	property   string
	ref        string
}

var transitiveChain = []chainLink{
	{"ChargingProfileType", "schedule", "#/definitions/ChargingScheduleType"},
	{"ChargingScheduleType", "tariff", "#/definitions/SalesTariffType"},
	{"SalesTariffType", "entry", "#/definitions/SalesTariffEntryType"},
	{"SalesTariffEntryType", "cost", "#/definitions/ConsumptionCostType"},
	{"ConsumptionCostType", "amount", "#/definitions/CostType"},
}

func TestDirectIndirectionIsRejectedButTransitiveNestingIsAllowed(t *testing.T) {
	_, directErr := WalkSchema(fixturePath("schemas", "direct_indirection.json"))
	requireExpectedHardFailureContains(t, "direct reference indirection check", directErr, "direct_indirection.json", "#/definitions/FirstType")

	transitive, transitiveErr := WalkSchema(fixturePath("schemas", "transitive_nesting.json"))
	requireImplemented(t, "transitive reference walk", transitiveErr)
	if len(transitive.Definitions) != 6 {
		t.Fatalf("transitive fixture resolved %d definitions, want six", len(transitive.Definitions))
	}

	profile := findProperty(transitive.Root.Properties, "profile")
	if profile == nil || profile.Ref != "#/definitions/ChargingProfileType" {
		t.Fatalf("request root did not carry its $ref to the chain's first link: %#v", profile)
	}

	for _, link := range transitiveChain {
		definition := findDefinition(transitive.Definitions, link.definition)
		if definition == nil {
			t.Fatalf("transitive fixture is missing definition %s: %#v", link.definition, transitive.Definitions)
		}
		if definition.Kind != DefinitionObject {
			t.Fatalf("chain definition %s was not read as an object definition: %#v", link.definition, definition)
		}
		property := findProperty(definition.Properties, link.property)
		if property == nil || property.Ref != link.ref {
			t.Fatalf("chain link %s.%s lost its $ref: %#v", link.definition, link.property, property)
		}
	}

	leaf := findDefinition(transitive.Definitions, "CostType")
	if leaf == nil || len(leaf.Properties) != 1 || leaf.Properties[0].Name != "value" || leaf.Properties[0].Type != "number" {
		t.Fatalf("deepest chain definition lost its own property: %#v", leaf)
	}
}

func TestDeepTransitiveWalkAddsDeepestDefinitionToReachSet(t *testing.T) {
	manifest := Manifest{SchemaDir: fixturePath("schemas"), Messages: []ManifestMessage{{Name: "Nesting", Request: "transitive_nesting.json", Response: "transitive_nesting_response.json"}}}
	ir, err := WalkSchemas(manifest)
	requireImplemented(t, "deep transitive reach-set walk", err)
	if len(ir.Messages) != 1 {
		t.Fatalf("deep transitive walk returned %d messages, want one", len(ir.Messages))
	}
	message := ir.Messages[0]
	if !contains(message.Reach, "CostType") || contains(message.Reach, "transitive_nesting") || contains(message.Reach, "transitive_nesting_response") {
		t.Fatalf("reach set did not include the deepest referenced type while excluding both roots: %#v", message)
	}
	if !contains(message.Roots, "transitive_nesting") || !contains(message.Roots, "transitive_nesting_response") {
		t.Fatalf("request and response roots were not both indexed: %#v", message)
	}
}

// TestWalkSchemasCarriesPropertiesOnEveryDefinitionIncludingRoots is the
// IR-level counterpart of TestDirectIndirectionIsRejectedButTransitiveNestingIsAllowed:
// it inspects .Properties on IR.Definitions after the full multi-file walk
// (WalkSchemas), not just on a single document's own read (WalkSchema), and
// includes the request root itself so a walk that drops property carriage
// once a definition is promoted into the shared index fails here even if
// the single-document reader is correct.
func TestWalkSchemasCarriesPropertiesOnEveryDefinitionIncludingRoots(t *testing.T) {
	manifest := Manifest{SchemaDir: fixturePath("schemas"), Messages: []ManifestMessage{{Name: "Nesting", Request: "transitive_nesting.json", Response: "transitive_nesting_response.json"}}}
	ir, err := WalkSchemas(manifest)
	requireImplemented(t, "walk-level property carriage", err)

	root := findDefinition(ir.Definitions, "transitive_nesting")
	if root == nil || root.Kind != DefinitionRoot {
		t.Fatalf("request root was not indexed as a root-kind definition: %#v", root)
	}
	if len(root.Properties) != 1 || root.Properties[0].Name != "profile" || root.Properties[0].Ref != "#/definitions/ChargingProfileType" {
		t.Fatalf("request root did not carry its declared property with its $ref at the IR level: %#v", root)
	}

	for _, link := range transitiveChain {
		definition := findDefinition(ir.Definitions, link.definition)
		if definition == nil || definition.Kind != DefinitionObject || len(definition.Properties) == 0 {
			t.Fatalf("definition %s lost its kind or properties at the IR level: %#v", link.definition, definition)
		}
		property := findProperty(definition.Properties, link.property)
		if property == nil || property.Ref != link.ref {
			t.Fatalf("definition %s lost its $ref at the IR level: definition=%#v property=%#v", link.definition, definition, property)
		}
	}

	leaf := findDefinition(ir.Definitions, "CostType")
	if leaf == nil || len(leaf.Properties) != 1 || leaf.Properties[0].Name != "value" || leaf.Properties[0].Type != "number" {
		t.Fatalf("deepest chain definition lost its own property at the IR level: %#v", leaf)
	}
}

func TestDedupMergesSchemaFixtures(t *testing.T) {
	first, firstErr := ReadSchema(fixturePath("schemas", "dedup_a.json"))
	requireImplemented(t, "first dedup fixture read", firstErr)
	second, secondErr := ReadSchema(fixturePath("schemas", "dedup_b.json"))
	requireImplemented(t, "second dedup fixture read", secondErr)
	merged, err := Deduplicate(append(first.Definitions, second.Definitions...))
	requireImplemented(t, "fixture-backed identical definition deduplication", err)
	shared := findDefinition(merged, "SharedType")
	if shared == nil || shared.Occurrences != 2 || !reflect.DeepEqual(shared.Files, []string{"dedup_a.json", "dedup_b.json"}) {
		t.Fatalf("fixture definitions were not merged deterministically: %#v", merged)
	}
}

func TestDedupMergesIdenticalDefinitionsAndCountsFiles(t *testing.T) {
	definitions := []Definition{
		{Name: "SharedType", Kind: DefinitionObject, Files: []string{"second.json"}},
		{Name: "SharedType", Kind: DefinitionObject, Files: []string{"first.json"}},
	}
	merged, err := Deduplicate(definitions)
	requireImplemented(t, "identical definition deduplication", err)
	if len(merged) != 1 || merged[0].Occurrences != 2 || !reflect.DeepEqual(merged[0].Files, []string{"first.json", "second.json"}) {
		t.Fatalf("identical definitions were not merged deterministically: %#v", merged)
	}
}

// TestDedupConflictNamesBothFilesAndPointer reads both fixtures through
// ReadSchema (no orphan fixture whose content is never parsed) and asserts
// the differing JSON pointer as well as both filenames.
func TestDedupConflictNamesBothFilesAndPointer(t *testing.T) {
	first, firstErr := ReadSchema(fixturePath("schemas", "dedup_a.json"))
	requireImplemented(t, "dedup_a fixture read", firstErr)
	conflict, conflictErr := ReadSchema(fixturePath("schemas", "dedup_conflict.json"))
	requireImplemented(t, "dedup_conflict fixture read", conflictErr)

	_, err := Deduplicate(append(first.Definitions, conflict.Definitions...))
	requireExpectedHardFailureContains(t, "structural definition conflict", err,
		"dedup_a.json", "dedup_conflict.json", "#/definitions/SharedType/properties/id")
}

// TestDedupMergesIdenticalEnumDefinitions is the control for
// TestDedupEnumValueSetConflictNamesBothFilesAndValues below: the same
// *EnumType is inlined into file after file across this corpus, so an
// implementation that reported every same-named enum pair as a conflict
// would satisfy that test and still be unable to read anything. Identical
// enums merge exactly the way identical objects do, value list included.
func TestDedupMergesIdenticalEnumDefinitions(t *testing.T) {
	first, firstErr := ReadSchema(fixturePath("schemas", "dedup_enum_a.json"))
	requireImplemented(t, "dedup_enum_a fixture read", firstErr)
	second, secondErr := ReadSchema(fixturePath("schemas", "dedup_enum_b.json"))
	requireImplemented(t, "dedup_enum_b fixture read", secondErr)

	merged, err := Deduplicate(append(first.Definitions, second.Definitions...))
	requireImplemented(t, "identical enum definition deduplication", err)
	shared := findDefinition(merged, "StatusEnumType")
	if shared == nil || shared.Kind != DefinitionEnum || !reflect.DeepEqual(shared.Values, []string{"Accepted", "Rejected"}) {
		t.Fatalf("identical enum definitions did not merge into one enum carrying its value list: %#v", merged)
	}
	if shared.Occurrences != 2 || !reflect.DeepEqual(shared.Files, []string{"dedup_enum_a.json", "dedup_enum_b.json"}) {
		t.Fatalf("merged enum did not carry both source files: %#v", shared)
	}
}

// TestDedupEnumValueSetConflictNamesBothFilesAndValues covers the enum half
// of the dedup rule, which TestDedupConflictNamesBothFilesAndPointer leaves
// open: that fixture pair differs in an object property's type, so a
// comparison that walked property lists and never looked at Values would
// pass it while accepting two same-named enums declaring different
// alternatives — and the value list is precisely what a generated enum type
// is made of, so the merge would ship one of the two schemas' wire vocabulary
// under both names. The two lists here are the same length and differ in
// exactly one member, so counting values is not enough either.
func TestDedupEnumValueSetConflictNamesBothFilesAndValues(t *testing.T) {
	first, firstErr := ReadSchema(fixturePath("schemas", "dedup_enum_a.json"))
	requireImplemented(t, "dedup_enum_a fixture read", firstErr)
	conflict, conflictErr := ReadSchema(fixturePath("schemas", "dedup_enum_conflict.json"))
	requireImplemented(t, "dedup_enum_conflict fixture read", conflictErr)

	_, err := Deduplicate(append(first.Definitions, conflict.Definitions...))
	requireExpectedHardFailureContains(t, "enum value-set conflict", err,
		"dedup_enum_a.json", "dedup_enum_conflict.json", "#/definitions/StatusEnumType/enum", "Rejected", "Denied")
}

func TestRootDefinitionSharesIndexAndIsExcludedFromReachCount(t *testing.T) {
	manifest := Manifest{SchemaDir: fixturePath("schemas"), Messages: []ManifestMessage{{Name: "RootMessage", Request: "root_kind.json", Response: "transitive_nesting_response.json"}}}
	ir, err := WalkSchemas(manifest)
	requireImplemented(t, "root definition indexing", err)
	if len(ir.Messages) != 1 {
		t.Fatalf("root fixture returned %d messages, want one", len(ir.Messages))
	}
	root := findDefinition(ir.Definitions, "root_kind")
	if root == nil || root.Kind != DefinitionRoot || contains(ir.Messages[0].Reach, root.Name) {
		t.Fatalf("root was not indexed as a root outside reach counting: root=%#v messages=%#v", root, ir.Messages)
	}
	shared := findProperty(root.Properties, "shared")
	if shared == nil || shared.Ref != "#/definitions/SharedType" {
		t.Fatalf("root definition did not carry its own property with its $ref alongside $ref'd definitions in the same index: %#v", root)
	}
	if !contains(ir.Messages[0].Reach, "SharedType") {
		t.Fatalf("the root's own $ref target was not reachable through the message's reach set: %#v", ir.Messages[0])
	}
}

// TestReservedFlagSurvivesWalkSchemas is the WalkSchemas-level counterpart of
// TestCustomDataDefinitionIsFlaggedReserved: that test only pins Reserved at
// the single-document ReadSchema level, so a dedup or IR-assembly step that
// dropped the flag while merging custom_data.json's CustomDataType into the
// shared definitions index would pass every existing test and still ship
// broken. This test walks a manifest that includes custom_data.json and
// checks Reserved on the definition WalkSchemas hands back.
func TestReservedFlagSurvivesWalkSchemas(t *testing.T) {
	manifest := Manifest{SchemaDir: fixturePath("schemas"), Messages: []ManifestMessage{{Name: "CustomData", Request: "custom_data.json", Response: "alpha_response.json"}}}
	ir, err := WalkSchemas(manifest)
	requireImplemented(t, "reserved-flag walk", err)

	customData := findDefinition(ir.Definitions, "CustomDataType")
	if customData == nil || !customData.Reserved {
		t.Fatalf("CustomDataType lost its reserved flag through WalkSchemas: %#v", customData)
	}
	sibling := findDefinition(ir.Definitions, "VendorInfoType")
	if sibling == nil || sibling.Reserved {
		t.Fatalf("a non-reserved sibling definition was incorrectly flagged reserved through WalkSchemas: %#v", sibling)
	}
}

// TestMessageRootsAreSortedIndependentlyOfRequestResponseOrder uses a
// request/response pair whose response file stem sorts before the request
// file stem, so a Roots slice that merely echoes [request, response] order
// is distinguishable from one that is genuinely sorted by name. It also
// pins Block/Direction as carried IR fields.
func TestMessageRootsAreSortedIndependentlyOfRequestResponseOrder(t *testing.T) {
	manifest := Manifest{SchemaDir: fixturePath("schemas"), Messages: []ManifestMessage{{Name: "Ordering", Block: "diagnostics", Direction: "CS->CSMS", Request: "zulu_request.json", Response: "alpha_response.json"}}}
	ir, err := WalkSchemas(manifest)
	requireImplemented(t, "message roots population", err)
	if len(ir.Messages) != 1 {
		t.Fatalf("ordering fixture returned %d messages, want one", len(ir.Messages))
	}
	message := ir.Messages[0]
	if message.RequestRoot != "zulu_request" || message.ResponseRoot != "alpha_response" {
		t.Fatalf("request/response roots were not the file stems: %#v", message)
	}
	if !reflect.DeepEqual(message.Roots, []string{"alpha_response", "zulu_request"}) {
		t.Fatalf("message roots were not sorted by name independent of request/response order: %#v", message.Roots)
	}
	if message.Block != "diagnostics" || message.Direction != "CS->CSMS" {
		t.Fatalf("message did not carry block/direction from its manifest row: %#v", message)
	}
}

// TestIRDeterminismAcrossRepeatedWalks walks a manifest large enough for
// determinism regressions to actually show up: a 1-element or empty slice
// passes a sortedness check unconditionally, and reflect.DeepEqual alone has
// no chance of failing reliably on such small inputs even when map iteration
// order genuinely varies between the two walks. This test walks
// transitive_nesting.json (6 definitions, 6-element Reach) plus a second
// message (nesting_variant.json) that shares CostType structurally, so at
// least one Definition.Files has more than one element, and asserts explicit
// sortedness detectors — not reflect.DeepEqual alone — on every naturally
// map-backed IR slice. The manifest declares Variant before Nesting, the
// opposite of the two names' sorted order, so a walk that merely echoed
// manifest declaration order instead of genuinely sorting would still fail
// the sortedness checks below. This test repeats the walk twice in this
// process, not in two separate processes — the separate-process leg of the
// determinism claim (Step 8) is discharged by the full-corpus smoke run, not
// by this unit test.
func TestIRDeterminismAcrossRepeatedWalks(t *testing.T) {
	manifest := Manifest{
		SchemaDir: fixturePath("schemas"),
		Messages: []ManifestMessage{
			{Name: "Variant", Request: "nesting_variant.json", Response: "nesting_variant_response.json"},
			{Name: "Nesting", Request: "transitive_nesting.json", Response: "transitive_nesting_response.json"},
		},
	}
	first, firstErr := WalkSchemas(manifest)
	requireImplemented(t, "first deterministic IR walk", firstErr)
	second, secondErr := WalkSchemas(manifest)
	requireImplemented(t, "second deterministic IR walk", secondErr)

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("identical inputs produced different IR values across two separate walks (map-order nondeterminism): first=%#v second=%#v", first, second)
	}

	if !sort.SliceIsSorted(first.Definitions, func(i, j int) bool { return first.Definitions[i].Name < first.Definitions[j].Name }) {
		t.Fatalf("IR definitions are not sorted by name: %#v", first.Definitions)
	}
	if !sort.SliceIsSorted(first.Messages, func(i, j int) bool { return first.Messages[i].Name < first.Messages[j].Name }) {
		t.Fatalf("IR messages are not sorted by name: %#v", first.Messages)
	}
	for _, definition := range first.Definitions {
		if !sort.StringsAreSorted(definition.Files) {
			t.Fatalf("definition files are not sorted in memory: %#v", definition.Files)
		}
	}
	for _, message := range first.Messages {
		if !sort.StringsAreSorted(message.Reach) {
			t.Fatalf("message reach set is not sorted in memory: %#v", message.Reach)
		}
		if message.Name == "Nesting" && len(message.Reach) != 6 {
			t.Fatalf("nesting message reach set has %d entries, want six: %#v", len(message.Reach), message.Reach)
		}
	}

	shared := findDefinition(first.Definitions, "CostType")
	if shared == nil || shared.Occurrences != 2 {
		t.Fatalf("CostType was not deduplicated across transitive_nesting.json and nesting_variant.json: %#v", shared)
	}
	if len(shared.Files) != 2 || !sort.StringsAreSorted(shared.Files) {
		t.Fatalf("a definition shared across files must carry a sorted, multi-element Files list: %#v", shared.Files)
	}
}

// TestGoldenIRDump pins the -dump-ir serialized shape against a committed
// golden file. It fails red today at the WalkSchemas call, before the
// marshal/compare ever runs — the golden file exists so that once the walk
// is implemented, the exact serialized contract other code consuming the IR
// builds against is pinned, not merely "some deterministic JSON". The
// comparison calls MarshalIR, the same function -dump-ir itself calls,
// rather than marshaling the IR in-test: a test-local marshal call could
// stay green even if -dump-ir's own rendering diverged from it, which would
// let the golden file quietly stop describing what the tool actually writes.
func TestGoldenIRDump(t *testing.T) {
	manifest := Manifest{SchemaDir: fixturePath("schemas"), Messages: []ManifestMessage{{Name: "GoldenMessage", Block: "diagnostics", Direction: "CS->CSMS", Request: "root_kind.json", Response: "transitive_nesting_response.json", Emit: true}}}
	ir, err := WalkSchemas(manifest)
	requireImplemented(t, "golden IR walk", err)

	got, err := MarshalIR(ir)
	requireImplemented(t, "golden IR marshal", err)

	want, err := os.ReadFile(fixturePath("golden", "golden_message.json"))
	if err != nil {
		t.Fatalf("golden fixture unreadable: %v", err)
	}
	if strings.TrimRight(string(want), "\n") != string(got) {
		t.Fatalf("IR dump does not match the committed golden file:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
