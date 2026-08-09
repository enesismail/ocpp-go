package main

import (
	"bytes"
	"encoding/json"
	"go/format"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

func transformFixture(t *testing.T) TransformConfig {
	t.Helper()
	config, err := LoadTransformConfig(fixturePath("config", "transform.yaml"))
	requireImplemented(t, "initialism transform configuration", err)
	return config
}

// readGoldenSnippet reads a golden fixture that may begin with a leading
// "//"-comment block documenting a deliberate rendering decision, followed
// by exactly one blank line and then the content actually compared. The
// leading comment, when present, is stripped before comparison — golden
// files compare rendered output, never their own documentation.
func readGoldenSnippet(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v", path, err)
	}
	lines := strings.Split(string(data), "\n")
	index := 0
	for index < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[index]), "//") {
		index++
	}
	if index > 0 && index < len(lines) && strings.TrimSpace(lines[index]) == "" {
		lines = lines[index+1:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func float64Ptr(v float64) *float64 { return &v }
func intPtr(v int) *int             { return &v }

func TestEmitterNamingTransformUsesConfiguredLongestMatches(t *testing.T) {
	config := transformFixture(t)
	cases := []struct {
		name string
		kind DefinitionKind
		want string
	}{
		{name: "evseId", kind: DefinitionObject, want: "EVSEID"},
		{name: "vendorId", kind: DefinitionObject, want: "VendorID"},
		{name: "webSocketURL", kind: DefinitionObject, want: "WebSocketURL"},
		{name: "iso15693", kind: DefinitionObject, want: "ISO15693"},
		{name: "acChargingParameters", kind: DefinitionObject, want: "ACChargingParameters"},
		{name: "dcChargingParameters", kind: DefinitionObject, want: "DCChargingParameters"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := TransformIdentifier(tc.name, tc.kind, config)
			requireImplemented(t, "identifier transform", err)
			if got != tc.want {
				t.Fatalf("TransformIdentifier(%q) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// The naming transform's word-splitting step runs on "Id" exactly like it
// runs on "vendorId" or "evseId" — it does not special-case a definition's
// own leading word. The v201 table's "ID" entry
// is matched case-insensitively against every split word, definitions
// included, so "Id" is corrected to "ID" here too: IdTokenType (object)
// strips its trailing "Type" to "Id"+"Token", re-renders as "IDToken", not
// "IdToken"; IdTokenEnumType (enum) strips "Enum", keeps "Type", and
// re-renders as "IDTokenType", not "IdTokenType". The algorithm and the
// table are authoritative over today's hand-written "IdToken" spelling
// (types/types.go:89) — that spelling becomes one more schema-faithful
// rename once the generator replaces it, the same as every other
// no-suffix enum rename the naming transform already predicts.
func TestEmitterNamingTransformUsesDefinitionKindForSuffixes(t *testing.T) {
	config := transformFixture(t)
	object, err := TransformIdentifier("IdTokenType", DefinitionObject, config)
	requireImplemented(t, "object suffix transform", err)
	enum, err := TransformIdentifier("IdTokenEnumType", DefinitionEnum, config)
	requireImplemented(t, "enum suffix transform", err)
	if object != "IDToken" || enum != "IDTokenType" {
		t.Fatalf("kind-specific suffix transform = object %q, enum %q; want IDToken and IDTokenType (the initialism table applies to \"Id\" here exactly as it does in vendorId/evseId)", object, enum)
	}
}

// TestEmitterNamingTransformEdgeCasesFollowKindNotNamePattern covers four
// naming edge cases the two tests above never touch: an object name that
// already carries no Type suffix, an enum name with no Enum component to
// strip, a name that merely contains "type" without ending in the word
// "Type" at a case boundary, and a schema name that happens to look
// enum-shaped but is declared as an object. All four assert that the
// definition's Kind — not a pattern match on its own name — selects which
// arm of the object/enum naming rule runs.
func TestEmitterNamingTransformEdgeCasesFollowKindNotNamePattern(t *testing.T) {
	config := transformFixture(t)
	cases := []struct {
		name string
		kind DefinitionKind
		want string
		note string
	}{
		{name: "EVSE", kind: DefinitionObject, want: "EVSE", note: "object not ending in Type is idempotent"},
		{name: "PhaseType", kind: DefinitionEnum, want: "PhaseType", note: "enum name with no Enum component has nothing to strip"},
		// Datatype: same trailing-substring shape the original fixture name
		// pinned (a lowercase "type" run that is not a case-boundary-delimited
		// "Type" word, so the naming transform's suffix-strip rule does not
		// fire), renamed off the earlier identifier because that identifier
		// was itself an instance of vocabulary generated output is forbidden
		// from carrying — an identifier whose trailing letters spell the
		// word for a first-draft artifact, which would be a false positive
		// for any grep this task's own self-check runs over its diff, not a
		// naming-rule concern.
		{name: "Datatype", kind: DefinitionObject, want: "Datatype", note: `a trailing "type" substring is not the same as a trailing "Type" word`},
		{name: "FooEnumType", kind: DefinitionObject, want: "FooEnum", note: "kind, not the name's own suffix shape, selects the strip rule"},
	}
	for _, tc := range cases {
		t.Run(tc.note, func(t *testing.T) {
			got, err := TransformIdentifier(tc.name, tc.kind, config)
			requireImplemented(t, "naming edge case", err)
			if got != tc.want {
				t.Fatalf("TransformIdentifier(%q, %s) = %q, want %q (%s)", tc.name, tc.kind, got, tc.want, tc.note)
			}
		})
	}
}

// Both collision tests below share an error-content contract and
// assert against compound "<kind> <Name> (<file>)"-shaped phrases rather
// than bare identifiers. A bare want is unsafe here by construction: the
// naming transform's own collision case is always an X/XType pair (that is how the collision
// arises), so a bare "Foo" is always a textual substring of "FooType", and
// a bare "GizmoRequest" is always a substring of the constructor name
// "NewGizmoRequest" the naming rule derives from it — either bare want
// would be satisfied by an implementation that only ever printed the OTHER
// identifier, never proving the check actually named both sides of the
// collision. Combining each identifier with its own file or role produces
// a phrase that is not a substring of anything else in the fixture, so it
// can only be satisfied by an implementation that genuinely names that
// specific identifier.
func TestEmitterNamingCollisionHardFailsWithBothDefinitions(t *testing.T) {
	ir := IR{Definitions: []Definition{
		{Name: "FooType", Kind: DefinitionObject, Files: []string{"alpha_definition.json"}},
		{Name: "Foo", Kind: DefinitionObject, Files: []string{"beta_definition.json"}},
	}}
	placement := PlacementPlan{Home: map[string]string{"FooType": "types", "Foo": "types"}}
	err := CheckEmissionCollisions(ir, placement, TransformConfig{Initialisms: []string{"ID"}})
	requireExpectedHardFailureContains(t, "definition collision", err,
		"object FooType (alpha_definition.json)", "object Foo (beta_definition.json)", "types")
}

// TestEmitterCrossNamespaceCollisionNamesSymbolsAndPackage collides a
// constructor name with a definition's rendered name in the same package.
// The enum is named exactly "NewGizmoRequest" already — no Type/Enum suffix
// to strip. The fixture's files are unrelated strings ("root_source.json",
// "enum_source.json"), not "GizmoRequest.json"/"NewGizmoRequest.json" as an
// earlier version of this test had them: the constructor name is
// mechanically "New"+the root's own name, so any filename built the same
// way nests inside the other's, which is exactly the substring risk the
// compound-phrase contract described above closes. The two role-labeled
// phrases below only both appear if the implementation actually detected
// that the SAME rendered text "NewGizmoRequest" is claimed twice, once as
// a constructor (derived from the GizmoRequest root) and once as an enum's
// own name.
func TestEmitterCrossNamespaceCollisionNamesSymbolsAndPackage(t *testing.T) {
	ir := IR{
		Definitions: []Definition{
			{Name: "GizmoRequest", Kind: DefinitionRoot, Files: []string{"root_source.json"}},
			{Name: "NewGizmoRequest", Kind: DefinitionEnum, Values: []string{"On", "Off"}, Files: []string{"enum_source.json"}},
		},
		Messages: []Message{{Name: "Gizmo", RequestRoot: "GizmoRequest", Roots: []string{"GizmoRequest"}, Reach: []string{"NewGizmoRequest"}, Emit: true}},
	}
	placement := PlacementPlan{Home: map[string]string{"NewGizmoRequest": "message:Gizmo"}}
	err := CheckEmissionCollisions(ir, placement, TransformConfig{Initialisms: []string{"ID"}})
	requireExpectedHardFailureContains(t, "cross-namespace collision", err,
		"constructor NewGizmoRequest", "enum NewGizmoRequest", "root_source.json", "enum_source.json", "message:Gizmo")
}

// TestEmitterFeatureSymbolCollisionHardFails covers the namespace the
// per-package collision index names that no other fixture reaches: the
// Feature scaffolding a message file emits unconditionally (the
// <M>Feature struct and <M>FeatureName const every message file emits)
// shares one package-level index with every definition placed in that same
// package. DemoFeature is a plain object definition — no Type or Enum
// suffix to strip, so it renders as itself — and it is placed in message
// Demo's own package, where the generated DemoFeature struct already
// claims that identifier.
//
// The wants follow the compound-phrase contract above: "DemoFeature" bare
// would also be a substring of the DemoFeatureName const the same
// scaffolding emits, and of the constructor name NewDemoFeature the
// definition itself derives, so each want pairs the identifier with the
// role or file that only one side of the collision has. The definition's
// file is deliberately an unrelated string rather than DemoFeature.json,
// for the same nesting reason the cross-namespace fixture above records.
func TestEmitterFeatureSymbolCollisionHardFails(t *testing.T) {
	ir := IR{
		Definitions: []Definition{
			{Name: "DemoRequest", Kind: DefinitionRoot, Files: []string{"DemoRequest.json"}, Properties: []Property{
				{Name: "detail", Ref: "#/definitions/DemoFeature"},
			}},
			{Name: "DemoResponse", Kind: DefinitionRoot, Files: []string{"DemoResponse.json"}},
			{Name: "DemoFeature", Kind: DefinitionObject, Files: []string{"detail_source.json"}, Properties: []Property{
				{Name: "label", Type: "string"},
			}},
		},
		Messages: []Message{
			{Name: "Demo", Block: "demo", Direction: "CS->CSMS", Request: "DemoRequest.json", Response: "DemoResponse.json", RequestRoot: "DemoRequest", ResponseRoot: "DemoResponse", Roots: []string{"DemoRequest", "DemoResponse"}, Reach: []string{"DemoFeature"}, Emit: true},
		},
	}
	placement := PlacementPlan{Home: map[string]string{"DemoFeature": "message:Demo"}}
	err := CheckEmissionCollisions(ir, placement, TransformConfig{Initialisms: []string{"ID"}})
	requireExpectedHardFailureContains(t, "Feature symbol collision", err,
		"feature DemoFeature", "object DemoFeature (detail_source.json)", "message:Demo")
}

// TestEmitterFeatureNameConstCollisionHardFails covers the other symbol
// every message file emits unconditionally into a message package, the
// <M>FeatureName const,
// which the fixture above never reaches: there the definition collides with
// the <M>Feature struct, and a per-package index seeded with the struct
// alone answers that fixture correctly while still accepting this one. The
// output it would then emit declares DemoFeatureName twice in one package,
// once as the message's own const and once as a definition, which does not
// compile — so the const has to be in the index on its own account, not as
// a symbol assumed to travel with the struct.
//
// DemoFeatureNameType is a plain object, so the naming transform strips its
// trailing Type and it renders as exactly DemoFeatureName. The suffix is load-bearing
// rather than decorative: an index keyed on schema names instead of
// rendered ones sees DemoFeatureNameType and DemoFeatureName, two different
// strings, and misses the clash entirely.
//
// The wants spell the const's role out instead of pairing a bare identifier
// with a file, because DemoFeature is a proper substring of
// DemoFeatureName and the two roles therefore cannot be told apart by
// identifier text alone. "feature DemoFeature", the phrase the fixture
// above uses for the struct side, is contained in any message that names
// this const as "feature DemoFeatureName", so borrowing that phrasing here
// would let a check that only ever indexed the struct symbol, and matched
// it by prefix, satisfy this test without knowing the const exists — the
// same check the clean case below independently forbids. No report of a
// struct collision can produce "feature-name const DemoFeatureName". The
// definition side keeps the schema name paired with its own file, as every
// collision fixture here does, and that file is again an unrelated string
// rather than DemoFeatureNameType.json for the nesting reason recorded
// above.
func TestEmitterFeatureNameConstCollisionHardFails(t *testing.T) {
	ir := IR{
		Definitions: []Definition{
			{Name: "DemoRequest", Kind: DefinitionRoot, Files: []string{"DemoRequest.json"}, Properties: []Property{
				{Name: "detail", Ref: "#/definitions/DemoFeatureNameType"},
			}},
			{Name: "DemoResponse", Kind: DefinitionRoot, Files: []string{"DemoResponse.json"}},
			{Name: "DemoFeatureNameType", Kind: DefinitionObject, Files: []string{"name_source.json"}, Properties: []Property{
				{Name: "label", Type: "string"},
			}},
		},
		Messages: []Message{
			{Name: "Demo", Block: "demo", Direction: "CS->CSMS", Request: "DemoRequest.json", Response: "DemoResponse.json", RequestRoot: "DemoRequest", ResponseRoot: "DemoResponse", Roots: []string{"DemoRequest", "DemoResponse"}, Reach: []string{"DemoFeatureNameType"}, Emit: true},
		},
	}
	placement := PlacementPlan{Home: map[string]string{"DemoFeatureNameType": "message:Demo"}}
	err := CheckEmissionCollisions(ir, placement, TransformConfig{Initialisms: []string{"ID"}})
	requireExpectedHardFailureContains(t, "feature-name const collision", err,
		"feature-name const DemoFeatureName", "object DemoFeatureNameType (name_source.json)", "message:Demo")
}

// TestEmitterDuplicateFieldWithinOneStructHardFails is the other half of
// the per-struct field-name collision rule, and the half that only a hard failure can express. The
// clean case below relies on field names being scoped to their own struct,
// so two structs may reuse one name freely; that freedom is not an absence
// of a field index, it is a narrower one, and a DUPLICATE inside a single
// struct still hard-fails. Two properties of MeterType, evseId and evseID,
// both render EVSEID under the naming transform's word split plus the EVSE and ID
// initialisms — a real corpus shape, since the schema spells the same
// concept both ways across files.
//
// The wants name the rendered field, both schema property names, and the
// declaring definition with its file. Neither property name is a substring
// of the other (they differ in their last character) nor of the rendered
// EVSEID, so an implementation that reported only one side of the
// duplicate cannot satisfy the set.
func TestEmitterDuplicateFieldWithinOneStructHardFails(t *testing.T) {
	ir := IR{
		Definitions: []Definition{
			{Name: "MeterRequest", Kind: DefinitionRoot, Files: []string{"MeterRequest.json"}, Properties: []Property{
				{Name: "meter", Ref: "#/definitions/MeterType"},
			}},
			{Name: "MeterResponse", Kind: DefinitionRoot, Files: []string{"MeterResponse.json"}},
			{Name: "MeterType", Kind: DefinitionObject, Files: []string{"meter_source.json"}, Properties: []Property{
				{Name: "evseId", Type: "integer"},
				{Name: "evseID", Type: "integer"},
			}},
		},
		Messages: []Message{
			{Name: "Meter", Block: "metering", Direction: "CS->CSMS", Request: "MeterRequest.json", Response: "MeterResponse.json", RequestRoot: "MeterRequest", ResponseRoot: "MeterResponse", Roots: []string{"MeterRequest", "MeterResponse"}, Reach: []string{"MeterType"}, Emit: true},
		},
	}
	placement := PlacementPlan{Home: map[string]string{"MeterType": "message:Meter"}}
	err := CheckEmissionCollisions(ir, placement, TransformConfig{Initialisms: []string{"EVSE", "ID"}})
	requireExpectedHardFailureContains(t, "duplicate struct field", err,
		"field EVSEID", "evseId", "evseID", "object MeterType (meter_source.json)")
}

// TestEmitterNoCollisionsAcrossKindsPackagesFieldScopesAndFeatureNames is
// the clean case CheckEmissionCollisions never had, and it is what keeps
// the five hard-fail fixtures above from being satisfied by a check that
// simply rejects everything. IdTokenType (object) and IdTokenEnumType
// (enum) coexist by construction under the naming rule's kind-specific
// suffix stripping. As TestEmitterNamingTransformUsesDefinitionKindForSuffixes
// establishes, the initialism table now corrects "Id" the same way it
// corrects "vendorId", so these render IDToken and IDTokenType, not the
// earlier IdToken and IdTokenType — still two different names, still no
// collision, only the literal spelling changed.
//
// Three more clean shapes ride along, the first two of them near misses of
// a hard-fail fixture above. AlphaFeatureInfoType renders AlphaFeatureInfo
// into the very package whose Feature scaffolding declares AlphaFeature and
// AlphaFeatureName: a check that compared prefixes, or that reserved every
// identifier beginning with a Feature symbol, would reject a name the rule
// allows. AlphaFeatureInfo and IdToken are two structs in ONE package
// reusing the field names CustomData and Status, which the per-struct field
// index must permit even though those same names duplicated inside one
// struct hard-fail. OtherType repeats that reuse from a second package.
//
// The IR is a valid one, not just a bag of names: every Reach entry is
// backed by a $ref chain from its message's own roots, and the placement
// map is what the placement rule's reach count actually computes over it — each composite
// is reached by exactly one message and therefore lands in that message's
// package, while the reserved CustomDataType, $ref'd by three definitions
// and consequently in both reach sets, is placed nowhere at all (the
// reserved-type rule: there is nothing to place, because nothing is emitted for it).
func TestEmitterNoCollisionsAcrossKindsPackagesFieldScopesAndFeatureNames(t *testing.T) {
	ir := IR{
		Definitions: []Definition{
			{Name: "AlphaRequest", Kind: DefinitionRoot, Files: []string{"AlphaRequest.json"}, Properties: []Property{
				{Name: "idToken", Ref: "#/definitions/IdTokenType"},
				{Name: "info", Ref: "#/definitions/AlphaFeatureInfoType"},
			}},
			{Name: "AlphaResponse", Kind: DefinitionRoot, Files: []string{"AlphaResponse.json"}},
			{Name: "AlphaFeatureInfoType", Kind: DefinitionObject, Files: []string{"AlphaRequest.json"}, Properties: []Property{
				{Name: "customData", Ref: "#/definitions/CustomDataType"},
				{Name: "status", Type: "string"},
			}},
			{Name: "BetaRequest", Kind: DefinitionRoot, Files: []string{"BetaRequest.json"}, Properties: []Property{
				{Name: "other", Ref: "#/definitions/OtherType"},
			}},
			{Name: "BetaResponse", Kind: DefinitionRoot, Files: []string{"BetaResponse.json"}},
			{Name: "CustomDataType", Kind: DefinitionObject, Reserved: true},
			{Name: "IdTokenType", Kind: DefinitionObject, Files: []string{"IdToken1.json"}, Properties: []Property{
				{Name: "customData", Ref: "#/definitions/CustomDataType"},
				{Name: "status", Ref: "#/definitions/IdTokenEnumType", EnumDefinition: "IdTokenEnumType"},
			}},
			{Name: "IdTokenEnumType", Kind: DefinitionEnum, Values: []string{"ISO14443", "ISO15693"}, Files: []string{"IdToken2.json"}},
			{Name: "OtherType", Kind: DefinitionObject, Files: []string{"Other.json"}, Properties: []Property{
				{Name: "customData", Ref: "#/definitions/CustomDataType"},
				{Name: "status", Type: "string"},
			}},
		},
		Messages: []Message{
			{Name: "Alpha", Block: "alpha", Direction: "CS->CSMS", Request: "AlphaRequest.json", Response: "AlphaResponse.json", RequestRoot: "AlphaRequest", ResponseRoot: "AlphaResponse", Roots: []string{"AlphaRequest", "AlphaResponse"}, Reach: []string{"AlphaFeatureInfoType", "CustomDataType", "IdTokenEnumType", "IdTokenType"}, Emit: true},
			{Name: "Beta", Block: "beta", Direction: "CS->CSMS", Request: "BetaRequest.json", Response: "BetaResponse.json", RequestRoot: "BetaRequest", ResponseRoot: "BetaResponse", Roots: []string{"BetaRequest", "BetaResponse"}, Reach: []string{"CustomDataType", "OtherType"}, Emit: true},
		},
	}
	placement := PlacementPlan{Home: map[string]string{
		"AlphaFeatureInfoType": "message:Alpha",
		"IdTokenType":          "message:Alpha",
		"IdTokenEnumType":      "message:Alpha",
		"OtherType":            "message:Beta",
	}}
	err := CheckEmissionCollisions(ir, placement, TransformConfig{Initialisms: []string{"ID", "ISO"}})
	requireImplemented(t, "collision-free coexistence across kinds, packages, struct-scoped field names and Feature-adjacent names", err)
}

// TestEmitterDuplicateValidatorTagAcrossDistinctEnumsHardFails proves the
// validator-name index catches a collision the per-package definition-name
// check cannot see. This fixture used to rely on a different route: before
// the "Id" initialism correction propagated here, it relied on
// IdTokenEnumType and IDTokenEnumType stripping to two literally
// different Go names (IdTokenType vs IDTokenType) that nonetheless derived
// the same tag. That premise is gone — see
// TestEmitterNamingTransformUsesDefinitionKindForSuffixes — because "Id" is
// now corrected to "ID" wherever it appears, including here, so both
// enums strip to the exact same rendered name, IDTokenType.
//
// Paper proof that a different-name/same-tag pair is impossible under this
// algorithm, so that route is not pursued: a derived tag is
// lowerCamel(word1) followed by every later word exactly as the naming
// transform rendered it. Every word from the second one on renders with a leading uppercase
// letter (an initialism word is rendered all-uppercase; a title-cased word
// capitalizes its own first letter) — never lowercase — so the first
// uppercase letter in a tag string marks exactly where word1 ends. Two
// equal tag strings therefore split at the same offset, making their two
// word1 substrings equal case-insensitively; because both the
// initialism-table lookup and the title-case fallback are themselves
// case-insensitive, functions of a word's letters, two case-insensitively
// equal words always render identically. So word1 renders identically in
// both names, forcing the remaining words (already shown byte-equal from
// the tag match) to also be equal — the two full names are identical, not
// merely same-tag. A tag collision between two distinct Go names cannot
// happen; only a same-name, same-tag pair can.
//
// That leaves exactly the mechanism this fixture already exercises,
// honestly re-scoped rather than dropped: IdTokenEnumType and
// IDTokenEnumType now render the identical Go name IDTokenType (confirmed
// above), but they are placed in two different packages
// ("message:Alpha"/"message:Beta"), so the per-package definition-collision
// index never sees a conflict — two packages are free to
// each declare their own IDTokenType, exactly as two real Go packages may
// each declare a type called Config. Only the validator-tag index, scoped
// to the entire emitted set because the go-playground registry is
// process-global, catches that both would register
// idTokenType201. This is a stronger demonstration of why the tag index
// must be global than the original premise was: it is not rescuing a
// same-package name clash, it is catching a collision that is invisible to
// every per-package check by construction.
func TestEmitterDuplicateValidatorTagAcrossDistinctEnumsHardFails(t *testing.T) {
	ir := IR{Definitions: []Definition{
		{Name: "IdTokenEnumType", Kind: DefinitionEnum, Values: []string{"Central", "Local"}, Files: []string{"a.json"}},
		{Name: "IDTokenEnumType", Kind: DefinitionEnum, Values: []string{"ISO14443", "ISO15693"}, Files: []string{"b.json"}},
	}}
	placement := PlacementPlan{Home: map[string]string{
		"IdTokenEnumType": "message:Alpha",
		"IDTokenEnumType": "message:Beta",
	}}
	err := CheckEmissionCollisions(ir, placement, TransformConfig{Initialisms: []string{"ID", "ISO"}})
	requireExpectedHardFailureContains(t, "duplicate validator tag", err,
		"idTokenType201", "IdTokenEnumType", "IDTokenEnumType", "a.json", "b.json")
}

func TestEmitterPlacementUsesMessageReachCountAndSkipsRootsAndReserved(t *testing.T) {
	ir := IR{
		Definitions: []Definition{
			{Name: "SharedType", Kind: DefinitionObject},
			{Name: "SingleType", Kind: DefinitionObject},
			{Name: "BoundaryType", Kind: DefinitionObject},
			{Name: "CustomDataType", Kind: DefinitionObject, Reserved: true},
			{Name: "AlphaRequest", Kind: DefinitionRoot},
			{Name: "AlphaResponse", Kind: DefinitionRoot},
		},
		Messages: []Message{
			{Name: "Alpha", RequestRoot: "AlphaRequest", ResponseRoot: "AlphaResponse", Roots: []string{"AlphaRequest", "AlphaResponse"}, Reach: []string{"SharedType", "SingleType", "BoundaryType"}, Emit: true},
			{Name: "Beta", Reach: []string{"SharedType"}, Emit: true},
			{Name: "Unemitted", Reach: []string{"SharedType", "SingleType"}, Emit: false},
		},
	}
	plan, err := ComputePlacement(ir)
	requireImplemented(t, "computed placement", err)
	want := map[string]string{
		"SharedType":   "types",
		"SingleType":   "types",
		"BoundaryType": "message:Alpha",
	}
	if !reflect.DeepEqual(plan.Home, want) {
		t.Fatalf("placement = %#v, want %#v; roots and reserved definitions must be absent", plan.Home, want)
	}
}

// TestEmitterPlacementHardFailsOnUnreachableDefinition covers the
// reach-count-zero case: a $ref'd definition no declared message reaches
// is never silently dropped — it is a hard error naming the definition
// and the file that declared it, because a definition nobody references
// means the walk or the manifest is wrong.
func TestEmitterPlacementHardFailsOnUnreachableDefinition(t *testing.T) {
	ir := IR{
		Definitions: []Definition{
			{Name: "OrphanType", Kind: DefinitionObject, Files: []string{"orphan.json"}},
		},
		Messages: []Message{
			{Name: "Alpha", Reach: []string{}, Emit: true},
		},
	}
	_, err := ComputePlacement(ir)
	requireExpectedHardFailureContains(t, "unreachable definition", err, "OrphanType", "orphan.json")
}

type goldenMappingCase struct {
	name       string
	definition string
	property   string
	// context is the PackageContext RenderProperty renders with. Its zero
	// value (PackageContext{}) is documented on the PackageContext type
	// itself as the default block-package reading — the reading every case
	// below except datetime/custom_data relies on. A generic $ref composite
	// is never qualified by RenderProperty regardless of context (see
	// RenderProperty's doc comment: full placement-aware qualification is a
	// RenderMessage concern), so those cases render identically whichever
	// way context is set. datetime and custom_data are the two cases where
	// context actually drives the output — DateTime and CustomData are
	// always qualified unconditionally, unlike an ordinary composite — so
	// they set it explicitly instead of leaning on the zero value's
	// coincidentally-correct default.
	context PackageContext
}

func loadGoldenMapping(t *testing.T, tc goldenMappingCase) (IR, Definition, Property, string) {
	t.Helper()
	base := fixturePath("golden", tc.name)
	data, err := os.ReadFile(base + ".ir.json")
	if err != nil {
		t.Fatalf("read %s.ir.json: %v", tc.name, err)
	}
	var ir IR
	if err := json.Unmarshal(data, &ir); err != nil {
		t.Fatalf("decode %s.ir.json: %v", tc.name, err)
	}
	definition := findDefinition(ir.Definitions, tc.definition)
	if definition == nil {
		t.Fatalf("%s.ir.json has no definition %q", tc.name, tc.definition)
	}
	property := findProperty(definition.Properties, tc.property)
	if property == nil {
		t.Fatalf("%s.ir.json has no property %q on %q", tc.name, tc.property, tc.definition)
	}
	want := readGoldenSnippet(t, base+".golden")
	return ir, *definition, *property, want
}

func TestEmitterMappingGoldenRules(t *testing.T) {
	transform := transformFixture(t)
	typesPackage := PackageContext{PackageName: "provisioning", TypesPackage: "example.test/ocpp/ocpp2.0.1/types"}
	cases := []goldenMappingCase{
		{name: "required_string", definition: "MappingRequest", property: "name"},
		{name: "optional_string", definition: "MappingRequest", property: "label"},
		{name: "optional_integer_zero", definition: "MappingRequest", property: "count"},
		{name: "optional_integer_positive", definition: "MappingRequest", property: "limit"},
		{name: "optional_number_zero", definition: "MappingRequest", property: "ratio"},
		{name: "optional_number_positive", definition: "MappingRequest", property: "threshold"},
		{name: "optional_bool", definition: "MappingRequest", property: "enabled"},
		{name: "required_bool", definition: "MappingRequest", property: "active"},
		{name: "optional_composite", definition: "MappingRequest", property: "device"},
		{name: "required_composite", definition: "MappingRequest", property: "device"},
		{name: "array_cardinality", definition: "MappingRequest", property: "values"},
		{name: "composite_array", definition: "MappingRequest", property: "devices"},
		{name: "required_array", definition: "MappingRequest", property: "entries"},
		{name: "enum_ref", definition: "MappingRequest", property: "status"},
		{name: "datetime", definition: "MappingRequest", property: "currentTime", context: typesPackage},
		{name: "custom_data", definition: "MappingRequest", property: "customData", context: typesPackage},
		{name: "untyped", definition: "MappingRequest", property: "data"},
		{name: "constraints", definition: "MappingRequest", property: "code"},
		{name: "maximum_bound", definition: "MappingRequest", property: "stateOfCharge"},
		{name: "initialism_property", definition: "MappingRequest", property: "evseId"},
		{name: "enum_leading_initialism", definition: "MappingRequest", property: "evseStatus"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ir, definition, property, want := loadGoldenMapping(t, tc)
			got, err := RenderProperty(definition, property, MappingContext{Definitions: ir.Definitions, Package: tc.context, Transform: transform}, nil)
			requireImplemented(t, "golden property mapping", err)
			if strings.TrimSpace(got) != want {
				t.Fatalf("rendered %s = %q, want %q", tc.name, got, want)
			}
		})
	}
}

func TestEmitterMappingUsesPackageContextForSharedTypes(t *testing.T) {
	data, err := os.ReadFile(fixturePath("golden", "package_context.ir.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(data, &ir); err != nil {
		t.Fatal(err)
	}
	definition := *findDefinition(ir.Definitions, "SharedType")
	property := *findProperty(definition.Properties, "timestamp")
	block, err := RenderProperty(definition, property, MappingContext{Definitions: ir.Definitions, Package: PackageContext{PackageName: "provisioning", TypesPackage: "example/types"}}, nil)
	requireImplemented(t, "block-package qualification", err)
	typesSource, err := RenderProperty(definition, property, MappingContext{Definitions: ir.Definitions, Package: PackageContext{PackageName: "types", TypesPackage: "example/types", InTypes: true}}, nil)
	requireImplemented(t, "types-package qualification", err)
	if !strings.Contains(block, "*types.DateTime") || strings.Contains(typesSource, "types.DateTime") {
		t.Fatalf("package-contextual date-time qualification was wrong: block=%q types=%q", block, typesSource)
	}
}

func TestEmitterDefinitionAndEnumRenderingMatchesRuntimeAnatomy(t *testing.T) {
	data, err := os.ReadFile(fixturePath("golden", "enum_definition.ir.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(data, &ir); err != nil {
		t.Fatal(err)
	}
	definition := *findDefinition(ir.Definitions, "StatusEnumType")
	context := PackageContext{PackageName: "provisioning", TypesPackage: "example.test/ocpp/ocpp2.0.1/types"}
	got, err := RenderDefinition(definition, ir, context, transformFixture(t), nil)
	requireImplemented(t, "enum declaration rendering", err)
	want := readGoldenSnippet(t, fixturePath("golden", "enum_definition.golden"))
	if strings.TrimSpace(got) != want {
		t.Fatalf("enum rendering differed:\n%s\nwant:\n%s", got, want)
	}
	// RenderDefinition never self-registers a validator: registration is
	// assembled once per generated file by the caller (RenderMessage or
	// the shared types-file assembler), so a lone definition's render must
	// carry neither an init() nor a RegisterValidation call.
	if strings.Contains(got, "func init()") || strings.Contains(got, "RegisterValidation(") {
		t.Fatalf("RenderDefinition must not self-register a validator; registration is assembled once per file by the caller:\n%s", got)
	}
}

// TestEmitterEnumTagDerivationLeadingInitialismRun exercises the
// validator-tag derivation against a name whose first word is a
// multi-letter initialism — see enum_leading_initialism.golden's own
// comment for the worked derivation of evseStatusType201, and
// enum_definition_initialism.golden for the same leading-run rule applied
// to the switch variable name.
func TestEmitterEnumTagDerivationLeadingInitialismRun(t *testing.T) {
	data, err := os.ReadFile(fixturePath("golden", "enum_definition_initialism.ir.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(data, &ir); err != nil {
		t.Fatal(err)
	}
	definition := *findDefinition(ir.Definitions, "EVSEStatusEnumType")
	context := PackageContext{PackageName: "provisioning", TypesPackage: "example.test/ocpp/ocpp2.0.1/types"}
	got, err := RenderDefinition(definition, ir, context, transformFixture(t), nil)
	requireImplemented(t, "leading-initialism enum rendering", err)
	want := readGoldenSnippet(t, fixturePath("golden", "enum_definition_initialism.golden"))
	if strings.TrimSpace(got) != want {
		t.Fatalf("leading-initialism enum rendering differed:\n%s\nwant:\n%s", got, want)
	}
}

// TestEmitterEnumConstNamesDeriveFromNonIdentifierValues pins how a const
// name derives from a non-identifier-shaped enum VALUE.
// The naming transform's definition-name rules only ever describe deriving
// a Go NAME from a schema DEFINITION name — every worked example
// elsewhere is itself a bare identifier. The real TransactionEvent reach set's enum VALUES are
// not: MeasurandEnumType carries "Energy.Active.Import.Register" (dotted),
// PhaseEnumType carries "L1-N" (dashed), ReadingContextEnumType carries
// "Transaction.Begin" (a second dotted shape, already segmented into
// clean title-case words with no case-folding needed — the
// "preserving existing interior caps" clause is a no-op on this one,
// which is the point: the same rule handles a value that needs no repair
// and one that does identically).
//
// The derivation: split the raw value on runs of
// non-alphanumeric characters, title-case each segment (preserving
// existing interior caps — a segment that already reads as a clean word,
// like "Register" or "Begin", is not re-cased), apply the initialism
// table per segment exactly as the naming transform already does per word, and
// concatenate the segments directly after the enum's own Go type name.
// The const's VALUE — the string after "=" — is always the untouched raw
// schema string; only the NAME goes through this derivation. This fixture
// consolidates the three shapes worth pinning (dotted, dashed, and
// a dotted value whose segments already arrive correctly cased) into one
// enum per shape rather than fabricating a single synthetic enum with all
// three values, so each rendered const can be checked against the exact
// real-corpus name the resolution text itself gives:
// MeasurandTypeEnergyActiveImportRegister, PhaseTypeL1N,
// ReadingContextTypeTransactionBegin.
func TestEmitterEnumConstNamesDeriveFromNonIdentifierValues(t *testing.T) {
	data, err := os.ReadFile(fixturePath("golden", "enum_value_derivation.ir.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(data, &ir); err != nil {
		t.Fatal(err)
	}
	context := PackageContext{PackageName: "transactions", TypesPackage: "example.test/ocpp/ocpp2.0.1/types"}
	cases := []struct {
		definition string
		wantConst  string
	}{
		{"MeasurandEnumType", `MeasurandTypeEnergyActiveImportRegister MeasurandType = "Energy.Active.Import.Register"`},
		{"PhaseEnumType", `PhaseTypeL1N PhaseType = "L1-N"`},
		{"ReadingContextEnumType", `ReadingContextTypeTransactionBegin ReadingContextType = "Transaction.Begin"`},
	}
	for _, tc := range cases {
		t.Run(tc.definition, func(t *testing.T) {
			definition := *findDefinition(ir.Definitions, tc.definition)
			got, err := RenderDefinition(definition, ir, context, transformFixture(t), nil)
			requireImplemented(t, "non-identifier enum value rendering", err)
			if !strings.Contains(got, tc.wantConst) {
				t.Fatalf("rendered %s does not contain %q:\n%s", tc.definition, tc.wantConst, got)
			}
		})
	}
}

// TestEmitterCustomDataKeepsSchemaDeclarationPosition observes something
// no other test in this file does: custom_data.ir.json declares customData
// before name, and the reserved CustomData field is never pinned last — it
// renders wherever schema declaration order puts it, exactly like any
// other field. RenderProperty alone cannot show this, since it renders one
// field at a time; RenderDefinition, over the whole MappingRequest struct,
// can. This reads the struct body through structFieldLines
// (defined below, alongside its own no-domain-fields caller) rather than
// raw strings.Index scans — an Index scan over the whole rendered source
// can be fooled by an unrelated match anywhere in the file (a comment, an
// import, a different struct's own field), where structFieldLines is
// anchored to MappingRequest's own struct body and returns exactly its
// declared fields in order.
func TestEmitterCustomDataKeepsSchemaDeclarationPosition(t *testing.T) {
	data, err := os.ReadFile(fixturePath("golden", "custom_data.ir.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(data, &ir); err != nil {
		t.Fatal(err)
	}
	definition := *findDefinition(ir.Definitions, "MappingRequest")
	context := PackageContext{PackageName: "provisioning", TypesPackage: "example.test/ocpp/ocpp2.0.1/types"}
	got, err := RenderDefinition(definition, ir, context, transformFixture(t), nil)
	requireImplemented(t, "customData position rendering", err)
	fields := structFieldLines(t, got, "MappingRequest")
	if len(fields) != 2 {
		t.Fatalf("MappingRequest must declare exactly CustomData and Name and nothing else, got %v", fields)
	}
	if !strings.HasPrefix(fields[0], "CustomData ") {
		t.Fatalf("CustomData must stay in schema declaration position (first here, ahead of Name), never pinned last: %v", fields)
	}
	if !strings.HasPrefix(fields[1], "Name ") {
		t.Fatalf("Name must remain the second declared field: %v", fields)
	}
}

func TestEmitterConstructorAndFeatureScaffolding(t *testing.T) {
	ir := IR{Definitions: []Definition{
		{Name: "DemoRequest", Kind: DefinitionRoot, Properties: []Property{
			{Name: "second", Type: "string", Required: true},
			{Name: "first", Type: "integer", Required: true},
		}},
		{Name: "DemoResponse", Kind: DefinitionRoot},
	}}
	got, err := RenderMessage(ir, ManifestMessage{Name: "Demo", Block: "demo", Direction: "CS->CSMS", Request: "DemoRequest.json", Response: "DemoResponse.json", Emit: true}, PlacementPlan{}, EmitterOptions{})
	requireImplemented(t, "constructor and feature rendering", err)
	for _, want := range []string{"DemoFeatureName", "GetRequestType", "GetResponseType", "GetFeatureName", "NewDemoRequest", "func NewDemoRequest(second string, first int)"} {
		if !strings.Contains(got, want) {
			t.Errorf("message source does not contain %q:\n%s", want, got)
		}
	}
}

// TestEmitterSingleMessageCompositeRendersAheadOfRootsUnqualified pins
// this rule: a $ref'd definition reached by exactly one message (reach
// count 1, the placement rule's single-message case) places in that
// message's own file, not types/ —
// boot_notification.go's own anatomy renders such composites (ModemType,
// ChargingStationType, lines 57-69) ahead of the request/response structs
// that reference them (lines 72-84), and because the composite lives in
// the same file, the reference to it is unqualified — unlike a
// types/-placed composite's types.X spelling (TestEmitterPackageContextualOutputCompilesTogether).
// The constructor-generation rule also generalizes constructor generation
// to every generated composite, not only roots, so BadgeType gets its own NewBadge too.
//
// This exercises RenderMessage directly with a placement plan that puts
// BadgeType in the message's own file, rather than re-cutting the
// byte-exact whole-file goldens (widget_message.golden/widget_types_gen.golden
// already pin every other whole-file fact, and a
// focused RenderMessage assertion set is the cheaper, still-faithful
// alternative for this one case).
func TestEmitterSingleMessageCompositeRendersAheadOfRootsUnqualified(t *testing.T) {
	ir := IR{Definitions: []Definition{
		{Name: "BadgeRequest", Kind: DefinitionRoot, Properties: []Property{
			{Name: "badge", Ref: "#/definitions/BadgeType"},
		}},
		{Name: "BadgeResponse", Kind: DefinitionRoot},
		{Name: "BadgeType", Kind: DefinitionObject, Properties: []Property{
			{Name: "label", Type: "string", MaxLength: intPtr(40)},
		}},
	}}
	placement := PlacementPlan{Home: map[string]string{"BadgeType": "message:Badge"}}
	got, err := RenderMessage(ir, ManifestMessage{Name: "Badge", Block: "widgets", Direction: "CS->CSMS", Request: "BadgeRequest.json", Response: "BadgeResponse.json", Emit: true}, placement, EmitterOptions{})
	requireImplemented(t, "single-message composite rendering", err)

	compositeIndex := strings.Index(got, "type Badge struct")
	requestIndex := strings.Index(got, "type BadgeRequest struct")
	if compositeIndex < 0 || requestIndex < 0 {
		t.Fatalf("rendered message is missing the Badge composite or the BadgeRequest root:\n%s", got)
	}
	if compositeIndex > requestIndex {
		t.Fatalf("single-message composite Badge must render ahead of the root structs that reference it (boot_notification.go's own ModemType/ChargingStationType-before-BootNotificationRequest anatomy):\n%s", got)
	}
	if !strings.Contains(got, `Badge *Badge `+"`"+`json:"badge,omitempty" validate:"omitempty"`+"`") {
		t.Fatalf("BadgeRequest.badge must reference the single-message composite unqualified — it lives in this same file, no package prefix:\n%s", got)
	}
	if !strings.Contains(got, "func NewBadge() *Badge") {
		t.Fatalf("single-message composite Badge is missing its own constructor (constructors are emitted for every generated composite, not only roots):\n%s", got)
	}
}

// TestEmitterConstructorReturnKindFollowsHowACompositeIsReached pins the
// half of the constructor-generation rule every other constructor assertion in this file leaves free.
// Each of them — the roots above, and Badge, reached through an optional
// $ref — happens to want a *T, so an implementation that returned a
// pointer unconditionally would satisfy all of them while breaking the
// value-returning shape the hand-written tree already has: types.go:505's
// NewChargingSchedulePeriod returns ChargingSchedulePeriod, and
// types.go:393's NewConsumptionCost returns ConsumptionCost, because those
// types are only ever reached as slice elements.
//
// The rule keys on how a composite is REACHED, not on what kind of
// declaration it is: *T for a request/response root and for any composite
// reached through a pointer field, T for a composite reached only by value
// or as a slice element. One fixture carries all three reaches, so neither
// an always-pointer nor an always-value implementation can pass it.
// DeviceType is reached by a required $ref, which renders the value field
// "Device Device" (required_composite.golden's own shape); EntryType only
// as the element type of a required array, "Entries []Entry"
// (required_array.golden's shape); CardType through an optional $ref,
// "*Card". A value want is not a substring of its own pointer spelling —
// "func NewDevice(serial string) Device" cannot be satisfied by a rendered
// "func NewDevice(serial string) *Device" — so each want discriminates on
// the return kind alone. NewDevice also carries a required field, showing
// a value-returning constructor still takes its required parameters, and
// the root constructor's own parameter list shows the two composites
// spelled the same way there as in the fields they populate.
//
// This exercises RenderMessage with a placement plan for the same reason
// the fixture above does — a byte-exact golden would additionally pin the
// order of the constructor block's own members, which no rule in the task
// decides, and would fail on a rendering that is entirely correct.
func TestEmitterConstructorReturnKindFollowsHowACompositeIsReached(t *testing.T) {
	ir := IR{
		Definitions: []Definition{
			{Name: "ReaderRequest", Kind: DefinitionRoot, Properties: []Property{
				{Name: "device", Ref: "#/definitions/DeviceType", Required: true},
				{Name: "entries", Type: "array", Required: true, MinItems: intPtr(1), MaxItems: intPtr(1024), Items: &Property{Ref: "#/definitions/EntryType"}},
				{Name: "card", Ref: "#/definitions/CardType"},
			}},
			{Name: "ReaderResponse", Kind: DefinitionRoot},
			{Name: "CardType", Kind: DefinitionObject, Properties: []Property{
				{Name: "label", Type: "string"},
			}},
			{Name: "DeviceType", Kind: DefinitionObject, Properties: []Property{
				{Name: "serial", Type: "string", Required: true},
			}},
			{Name: "EntryType", Kind: DefinitionObject, Properties: []Property{
				{Name: "label", Type: "string"},
			}},
		},
		Messages: []Message{
			{Name: "Reader", Block: "widgets", Direction: "CS->CSMS", Request: "ReaderRequest.json", Response: "ReaderResponse.json", RequestRoot: "ReaderRequest", ResponseRoot: "ReaderResponse", Roots: []string{"ReaderRequest", "ReaderResponse"}, Reach: []string{"CardType", "DeviceType", "EntryType"}, Emit: true},
		},
	}
	placement := PlacementPlan{Home: map[string]string{
		"CardType":   "message:Reader",
		"DeviceType": "message:Reader",
		"EntryType":  "message:Reader",
	}}
	got, err := RenderMessage(ir, ManifestMessage{Name: "Reader", Block: "widgets", Direction: "CS->CSMS", Request: "ReaderRequest.json", Response: "ReaderResponse.json", Emit: true}, placement, EmitterOptions{})
	requireImplemented(t, "constructor return kinds", err)

	cases := []struct {
		want string
		why  string
	}{
		{"func NewDevice(serial string) Device", "Device is reached only by a required $ref (by value), so its constructor returns a value, not a pointer"},
		{"func NewEntry() Entry", "Entry is reached only as a slice element, so its constructor returns a value, not a pointer"},
		{"func NewCard() *Card", "Card is reached through an optional $ref (a pointer field), so its constructor returns a pointer"},
		{"func NewReaderRequest(device Device, entries []Entry) *ReaderRequest", "a request root always returns a pointer, and takes its required fields in schema declaration order, spelled as the fields they populate"},
	}
	for _, tc := range cases {
		if !strings.Contains(got, tc.want) {
			t.Errorf("rendered message does not contain %q: %s\n%s", tc.want, tc.why, got)
		}
	}
}

// TestEmitterRootsAreEmittedInTheirOwningMessageFile asserts the FILE-level
// consequence of root placement (the placement rule: a root is never a member of any reach
// set, so it never enters the placement computation at all), not the
// placement MAP itself — TestEmitterPlacementUsesMessageReachCountAndSkipsRootsAndReserved
// already pins that fact directly against ComputePlacement's return value,
// and re-asserting the same map shape here would only duplicate it
// under a different name. This version runs the whole EmitGo pipeline and
// reads the actual generated files: HeartbeatRequest and HeartbeatResponse
// must be declared in their own message file and must never appear in the
// shared types_gen.go, even though SharedType — reached by two emitted
// messages — does place there.
func TestEmitterRootsAreEmittedInTheirOwningMessageFile(t *testing.T) {
	manifest := Manifest{
		Version: "v201", GoModule: "example.test/roots", GoTree: "ocpp2.0.1", TypesPackage: "example.test/roots/ocpp2.0.1/types",
		Messages: []ManifestMessage{
			{Name: "Heartbeat", Block: "availability", Direction: "CS->CSMS", Request: "HeartbeatRequest.json", Response: "HeartbeatResponse.json", Emit: true},
			{Name: "Ping", Block: "availability", Direction: "CS->CSMS", Request: "PingRequest.json", Response: "PingResponse.json", Emit: true},
		},
	}
	ir := IR{
		Definitions: []Definition{
			{Name: "HeartbeatRequest", Kind: DefinitionRoot, Properties: []Property{{Name: "shared", Ref: "#/definitions/SharedType"}}},
			{Name: "HeartbeatResponse", Kind: DefinitionRoot},
			{Name: "PingRequest", Kind: DefinitionRoot, Properties: []Property{{Name: "shared", Ref: "#/definitions/SharedType"}}},
			{Name: "PingResponse", Kind: DefinitionRoot},
			{Name: "SharedType", Kind: DefinitionObject},
		},
		Messages: []Message{
			{Name: "Heartbeat", Block: "availability", Direction: "CS->CSMS", Request: "HeartbeatRequest.json", Response: "HeartbeatResponse.json", RequestRoot: "HeartbeatRequest", ResponseRoot: "HeartbeatResponse", Roots: []string{"HeartbeatRequest", "HeartbeatResponse"}, Reach: []string{"SharedType"}, Emit: true},
			{Name: "Ping", Block: "availability", Direction: "CS->CSMS", Request: "PingRequest.json", Response: "PingResponse.json", RequestRoot: "PingRequest", ResponseRoot: "PingResponse", Roots: []string{"PingRequest", "PingResponse"}, Reach: []string{"SharedType"}, Emit: true},
		},
	}
	dir := t.TempDir()
	err := EmitGo(ir, manifest, dir, EmitterOptions{Transform: TransformConfig{Initialisms: []string{"ID"}}})
	requireImplemented(t, "root placement file emission", err)

	heartbeatSource, err := os.ReadFile(filepath.Join(dir, "ocpp2.0.1", "availability", "heartbeat.go"))
	if err != nil {
		t.Fatalf("generated Heartbeat message file missing: %v", err)
	}
	for _, want := range []string{"type HeartbeatRequest struct", "type HeartbeatResponse struct"} {
		if !strings.Contains(string(heartbeatSource), want) {
			t.Errorf("heartbeat.go missing %q", want)
		}
	}

	typesSource, err := os.ReadFile(filepath.Join(dir, "ocpp2.0.1", "types", "types_gen.go"))
	if err != nil {
		t.Fatalf("generated shared types file missing: %v", err)
	}
	for _, unwanted := range []string{"type HeartbeatRequest struct", "type HeartbeatResponse struct"} {
		if strings.Contains(string(typesSource), unwanted) {
			t.Errorf("types_gen.go must never declare a message root, found %q", unwanted)
		}
	}
}

func TestEmitterOverrideComposesWithBaseTags(t *testing.T) {
	definition := Definition{Name: "BootNotificationResponse", Kind: DefinitionRoot, Properties: []Property{{Name: "interval", Type: "integer", Required: true}}}
	row := OverrideRow{Version: "v201", Definition: "BootNotificationResponse", Property: "interval", Rule: "add", Tag: "gte=0"}
	base, err := BaseTagSet(definition, definition.Properties[0])
	requireImplemented(t, "base tag lookup", err)
	got, err := ApplyTagOverride(base, row)
	requireImplemented(t, "tag override composition", err)
	if !reflect.DeepEqual(got, []string{"required", "gte=0"}) {
		t.Fatalf("override tags = %#v, want [required gte=0]", got)
	}
	field, err := RenderProperty(definition, definition.Properties[0], MappingContext{Package: PackageContext{PackageName: "provisioning"}}, []OverrideRow{row})
	requireImplemented(t, "override-rendered field", err)
	if !strings.Contains(field, `validate:"required,gte=0"`) {
		t.Fatalf("override was not composed into the rendered tag: %q", field)
	}
}

// TestEmitterTightenOverrideSubstitutesInPlace pins the composition rule
// for rule: tighten. An override row's fields are definition, property,
// tag, rule and from; From names the exact base token being replaced, and
// ApplyTagOverride must put the replacement in that token's own position,
// never append it as an extra token alongside the stale one.
func TestEmitterTightenOverrideSubstitutesInPlace(t *testing.T) {
	base := []string{"required", "gte=0"}
	row := OverrideRow{Version: "v201", Definition: "FixtureResponse", Property: "interval", Rule: "tighten", From: "gte=0", Tag: "gte=1"}
	got, err := ApplyTagOverride(base, row)
	requireImplemented(t, "tighten composition", err)
	want := []string{"required", "gte=1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tighten override = %#v, want %#v (gte=0 substituted in place, not appended)", got, want)
	}
}

// TestEmitterMapPropertyIsPlacementAgnostic pins MapProperty directly as
// the placement-agnostic seam it is designed to be: given only a
// definition, its property, the flat definitions list and the transform
// config — no PackageContext, no PlacementPlan — it produces the
// FieldMapping a governance check consults, independent of where the
// property's declaring struct eventually renders. See MapProperty's own
// doc comment for why PackageContext was dropped from its signature.
func TestEmitterMapPropertyIsPlacementAgnostic(t *testing.T) {
	definition := Definition{Name: "MappingRequest", Kind: DefinitionRoot, Properties: []Property{{Name: "name", Type: "string", Required: true}}}
	got, err := MapProperty(definition, definition.Properties[0], nil, TransformConfig{Initialisms: []string{"ID"}}, nil)
	requireImplemented(t, "direct MapProperty mapping", err)
	want := FieldMapping{FieldName: "Name", GoType: "string", JSONTag: `json:"name"`, ValidateTag: `validate:"required"`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MapProperty(name) = %#v, want %#v", got, want)
	}
}

// TestEmitterOverrideRejectsOptionalityTags pins the real structural bound
// on an override row, not an invented one. OverrideRow has no field that
// could even attempt to change a Go type, a field name or a placement —
// Rule is only ever "add" or "tighten" against a validate token — so the
// bound worth testing is the one a real row can actually attempt: naming
// "required" or "omitempty" as the tag itself. Those are optionality's
// tags, not validation-strictness tags, and an override may never touch
// either. The failure must name the forbidden class ("optionality"),
// which is not literal input text — an implementation that merely echoed
// the row's own tag back would not happen to say it.
func TestEmitterOverrideRejectsOptionalityTags(t *testing.T) {
	badRows := []OverrideRow{
		{Definition: "FixtureRequest", Property: "value", Rule: "add", Tag: "required"},
		{Definition: "FixtureRequest", Property: "value", Rule: "add", Tag: "omitempty"},
	}
	for _, row := range badRows {
		t.Run(row.Tag, func(t *testing.T) {
			_, err := ApplyTagOverride([]string{"omitempty"}, row)
			requireExpectedHardFailureContains(t, "optionality override rejection", err, "optionality", row.Tag)
		})
	}
}

// structFieldLines extracts the field-declaration lines inside typeName's
// struct body from source, so a check for "exactly these fields and no
// others" is anchored to the struct's own body instead of scanning the
// whole file for a loosely-matched substring an unrelated identifier,
// JSON tag or comment could also contain.
func structFieldLines(t *testing.T, source, typeName string) []string {
	t.Helper()
	marker := "type " + typeName + " struct {"
	start := strings.Index(source, marker)
	if start < 0 {
		t.Fatalf("source does not declare %s:\n%s", typeName, source)
	}
	body := source[start+len(marker):]
	end := strings.Index(body, "\n}")
	if end < 0 {
		t.Fatalf("%s struct body is not closed:\n%s", typeName, source)
	}
	var fields []string
	for _, line := range strings.Split(body[:end], "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			fields = append(fields, line)
		}
	}
	return fields
}

// TestEmitterNoDomainFieldsStillEmitsCustomDataAndZeroArgumentConstructor
// covers HeartbeatRequest and HeartbeatResponse, whose schemas declare no
// domain properties at all — only customData (verified against the
// vendored schema: not an empty object). The fixture carries customData on
// both structs, since the real 2.0.1 HeartbeatResponse does too; each
// struct is checked separately, because the reserved-field guarantee is
// per-composite (exactly one CustomData field on each), not file-wide.
//
// There is no separate byte-exact golden for this fixture: a third
// whole-file golden would duplicate the pinning power
// widget_message.golden/widget_types_gen.golden already provide, for a
// case that is otherwise fully covered by the anchored assertions below.
// The struct-body-anchored checks replace an earlier bare substring scan,
// which could both miss a real regression (an added field not spelled the
// way the scan expected) and flag unrelated content that coincidentally
// contained the same text.
func TestEmitterNoDomainFieldsStillEmitsCustomDataAndZeroArgumentConstructor(t *testing.T) {
	data, err := os.ReadFile(fixturePath("golden", "no_domain_fields.ir.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ir IR
	if err := json.Unmarshal(data, &ir); err != nil {
		t.Fatal(err)
	}
	got, err := RenderMessage(ir, ManifestMessage{Name: "Heartbeat", Block: "availability", Direction: "CS->CSMS", Request: "HeartbeatRequest.json", Response: "HeartbeatResponse.json", Emit: true}, PlacementPlan{}, EmitterOptions{})
	requireImplemented(t, "no-domain-fields message", err)
	if !strings.Contains(got, "func NewHeartbeatRequest() *HeartbeatRequest") || !strings.Contains(got, "func NewHeartbeatResponse() *HeartbeatResponse") {
		t.Fatalf("no-domain-fields message lost a zero-argument constructor on one of its structs:\n%s", got)
	}
	for _, typeName := range []string{"HeartbeatRequest", "HeartbeatResponse"} {
		fields := structFieldLines(t, got, typeName)
		if len(fields) != 1 || !strings.HasPrefix(fields[0], "CustomData ") {
			t.Fatalf("%s must declare exactly the reserved CustomData field and nothing else, got %v", typeName, fields)
		}
		if !strings.Contains(fields[0], "*types.CustomData") {
			t.Fatalf("%s.CustomData must reference the hand-written types.CustomData: %q", typeName, fields[0])
		}
	}
	if strings.Contains(got, "type CustomData ") || strings.Contains(got, "type CustomDataType ") {
		t.Fatalf("no-domain-fields message must never declare a generated CustomData/CustomDataType type:\n%s", got)
	}
}

// compileManifest and compileIR back every whole-tree EmitGo test in this
// file. Two messages, Compile and CompileOther, both reach SharedType and
// SharedStatusEnumType, giving each a reach count of 2 so both place in
// the shared types file — SharedType carries a date-time property, a
// customData $ref and an enum $ref, so the shared types file must spell
// *DateTime, *CustomData and Validate.RegisterValidation unqualified and
// the whole tree must still compile. CompileResponse's own status enum
// (CompileStatusEnumType) is reached by Compile alone, giving the
// validator-uniqueness scan real content: one registration lands in the
// shared types file, a second, different one lands in compile.go.
//
// Each message's reach set is backed by a $ref chain from its own roots,
// which is the only way a definition can enter one: CompileRequest.shared
// is what puts SharedType — and, through SharedType's own properties,
// SharedStatusEnumType — in Compile's reach set, exactly as
// CompileOtherRequest.shared does for CompileOther, while
// CompileRequest.customData and CompileResponse.status account for that
// set's other two entries directly.
//
// CompileOther is declared CSMS -> CS, deliberately the reverse
// of Compile's CS -> CSMS — every other fixture in this file only ever
// uses CS -> CSMS, which TestEmitterPackageContextualOutputCompilesTogether's
// banner assertion would not actually exercise against a hard-coded
// banner. CompileOtherResponse also carries its own direct "outcome"
// property, a second, independent reference to SharedStatusEnumType:
// SharedType's own nested "status" field already exercises
// this enum from inside the shared types file itself (unqualified, since
// it is declared there), but nothing previously exercised a message file
// referencing a types/-placed ENUM directly and qualified
// (types.SharedStatusType) the way it already does for the DateTime and
// CustomData support types.
func compileManifest() Manifest {
	return Manifest{
		Version: "v201", GoModule: "example.test/ocpp", GoTree: "ocpp2.0.1", TypesPackage: "example.test/ocpp/ocpp2.0.1/types",
		Messages: []ManifestMessage{
			{Name: "Compile", Block: "demo", Direction: "CS->CSMS", Request: "CompileRequest.json", Response: "CompileResponse.json", Emit: true},
			{Name: "CompileOther", Block: "demo", Direction: "CSMS->CS", Request: "CompileOtherRequest.json", Response: "CompileOtherResponse.json", Emit: true},
		},
	}
}

func compileIR() IR {
	return IR{
		Definitions: []Definition{
			{Name: "CompileRequest", Kind: DefinitionRoot, Properties: []Property{
				{Name: "timestamp", Type: "string", Format: "date-time", Required: true},
				{Name: "shared", Ref: "#/definitions/SharedType"},
				{Name: "customData", Ref: "#/definitions/CustomDataType"},
			}},
			{Name: "CompileResponse", Kind: DefinitionRoot, Properties: []Property{
				{Name: "status", Ref: "#/definitions/CompileStatusEnumType", EnumDefinition: "CompileStatusEnumType", Required: true},
			}},
			{Name: "CompileOtherRequest", Kind: DefinitionRoot, Properties: []Property{
				{Name: "shared", Ref: "#/definitions/SharedType"},
			}},
			{Name: "CompileOtherResponse", Kind: DefinitionRoot, Properties: []Property{
				{Name: "outcome", Ref: "#/definitions/SharedStatusEnumType", EnumDefinition: "SharedStatusEnumType"},
			}},
			{Name: "CustomDataType", Kind: DefinitionObject, Reserved: true},
			{Name: "SharedType", Kind: DefinitionObject, Properties: []Property{
				{Name: "value", Type: "string"},
				{Name: "occurredAt", Type: "string", Format: "date-time"},
				{Name: "customData", Ref: "#/definitions/CustomDataType"},
				{Name: "status", Ref: "#/definitions/SharedStatusEnumType", EnumDefinition: "SharedStatusEnumType"},
			}},
			{Name: "SharedStatusEnumType", Kind: DefinitionEnum, Values: []string{"Accepted", "Rejected"}},
			{Name: "CompileStatusEnumType", Kind: DefinitionEnum, Values: []string{"Accepted", "Rejected"}},
		},
		Messages: []Message{
			{Name: "Compile", Block: "demo", Direction: "CS->CSMS", Request: "CompileRequest.json", Response: "CompileResponse.json", RequestRoot: "CompileRequest", ResponseRoot: "CompileResponse", Roots: []string{"CompileRequest", "CompileResponse"}, Reach: []string{"CompileStatusEnumType", "CustomDataType", "SharedStatusEnumType", "SharedType"}, Emit: true},
			{Name: "CompileOther", Block: "demo", Direction: "CSMS->CS", Request: "CompileOtherRequest.json", Response: "CompileOtherResponse.json", RequestRoot: "CompileOtherRequest", ResponseRoot: "CompileOtherResponse", Roots: []string{"CompileOtherRequest", "CompileOtherResponse"}, Reach: []string{"CustomDataType", "SharedStatusEnumType", "SharedType"}, Emit: true},
		},
	}
}

// writeCompileSupport builds a hermetic scratch module for
// TestEmitterPackageContextualOutputCompilesTogether.
//
// A bare `require
// gopkg.in/go-playground/validator.v9 v9.31.0` with no go.sum can never
// build offline — default (readonly) mode refuses to proceed without a
// go.sum entry, and -mod=mod would try to fetch one over the network,
// which a sandboxed or offline CI runner does not have. Neither this
// scratch module (synthesized fresh in a t.TempDir() on every run) nor
// this repository vendors that go.sum. The fix replaces the real module
// with a local filesystem module: `replace
// gopkg.in/go-playground/validator.v9 => ./stubvalidator`, satisfied by a
// hand-written stub package declaring exactly the surface generated code
// imports — validator.FieldLevel (the isValidX parameter type), the Func
// signature RegisterValidation accepts, and New's Validate handle. A
// local-path replace needs no checksum: go reads the replacement straight
// off disk, so no go.sum entry is required for it at all.
//
// Verified hermetic by hand: a
// hand-written stand-in tree in this exact shape (go.mod + replace,
// stubvalidator/, ocpp2.0.1/types/support.go, a generated-shaped
// types_gen.go and block-package file) built and vetted clean under
// GOPROXY=off with no go.sum present, in both default (readonly) and
// -mod=mod modes — proving the harness this function feeds actually works
// offline, independent of EmitGo, which is still stubbed.
func writeCompileSupport(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "ocpp2.0.1", "types"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "stubvalidator"), 0o755); err != nil {
		t.Fatal(err)
	}
	goMod := "module example.test/ocpp\n\ngo 1.21\n\nrequire gopkg.in/go-playground/validator.v9 v9.31.0\n\nreplace gopkg.in/go-playground/validator.v9 => ./stubvalidator\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}
	stubGoMod := "module gopkg.in/go-playground/validator.v9\n\ngo 1.21\n"
	if err := os.WriteFile(filepath.Join(dir, "stubvalidator", "go.mod"), []byte(stubGoMod), 0o644); err != nil {
		t.Fatal(err)
	}
	stub := "// Package validator is a hermetic, offline stand-in for\n" +
		"// gopkg.in/go-playground/validator.v9, satisfying exactly the surface\n" +
		"// generated code uses: the FieldLevel type passed to isValidX functions,\n" +
		"// the Func signature RegisterValidation accepts, and New's Validate handle.\n" +
		"package validator\n\n" +
		"import \"reflect\"\n\n" +
		"// FieldLevel is the subset of the real interface generated isValidX\n" +
		"// functions call: fl.Field().String().\n" +
		"type FieldLevel interface {\n\tField() reflect.Value\n}\n\n" +
		"// Func is the signature RegisterValidation accepts.\n" +
		"type Func func(fl FieldLevel) bool\n\n" +
		"// Validate stands in for the real *validator.Validate handle.\n" +
		"type Validate struct{}\n\n" +
		"// New mirrors validator.New().\n" +
		"func New() *Validate { return &Validate{} }\n\n" +
		"// RegisterValidation mirrors (*validator.Validate).RegisterValidation.\n" +
		"func (v *Validate) RegisterValidation(tag string, fn Func) error { return nil }\n"
	if err := os.WriteFile(filepath.Join(dir, "stubvalidator", "validator.go"), []byte(stub), 0o644); err != nil {
		t.Fatal(err)
	}
	support := "package types\n\nimport \"gopkg.in/go-playground/validator.v9\"\n\ntype DateTime struct{}\ntype CustomData struct{}\nvar Validate = validator.New()\n"
	if err := os.WriteFile(filepath.Join(dir, "ocpp2.0.1", "types", "support.go"), []byte(support), 0o644); err != nil {
		t.Fatal(err)
	}
}

// importBlock extracts the text of source's import block — the
// parenthesized form or a single `import "..."` line — or "" if the file
// has none, so a check for "this import must/must not be present" is
// anchored to the import section instead of scanning the whole file,
// where an unrelated identifier or comment could coincidentally match.
func importBlock(t *testing.T, source string) string {
	t.Helper()
	const marker = "\nimport "
	start := strings.Index(source, marker)
	if start < 0 {
		return ""
	}
	rest := source[start+1:]
	if strings.HasPrefix(rest, "import (") {
		end := strings.Index(rest, ")\n")
		if end < 0 {
			t.Fatalf("unterminated import block:\n%s", source)
		}
		return rest[:end]
	}
	end := strings.Index(rest, "\n")
	if end < 0 {
		return rest
	}
	return rest[:end]
}

func TestEmitterPackageContextualOutputCompilesTogether(t *testing.T) {
	dir := t.TempDir()
	writeCompileSupport(t, dir)
	err := EmitGo(compileIR(), compileManifest(), dir, EmitterOptions{Transform: TransformConfig{Initialisms: []string{"ID"}}})
	requireImplemented(t, "package-contextual output", err)

	typesPath := filepath.Join(dir, "ocpp2.0.1", "types", "types_gen.go")
	if _, err := os.Stat(typesPath); err != nil {
		t.Fatalf("shared generated types file missing: %v", err)
	}

	// The block-package file asserts qualified types.X spellings: both come
	// from CompileRequest's own fields, so this needs no reach-set
	// reasoning to check.
	blockPath := filepath.Join(dir, "ocpp2.0.1", "demo", "compile.go")
	blockSource, err := os.ReadFile(blockPath)
	if err != nil {
		t.Fatalf("generated block-package message file missing: %v", err)
	}
	for _, want := range []string{"*types.DateTime", "*types.CustomData"} {
		if !strings.Contains(string(blockSource), want) {
			t.Errorf("compile.go missing expected package-qualified reference %q:\n%s", want, blockSource)
		}
	}

	// CompileOther is declared CSMS -> CS, the reverse of
	// Compile's CS -> CSMS: a banner that read "CS -> CSMS" here would
	// prove the emitter hard-codes the banner instead of reading it from
	// the manifest row it is currently rendering. CompileOtherResponse's
	// own "outcome" field is a direct qualified reference to
	// SharedStatusType, an ENUM placed in types/ (reach count 2)
	// — distinct from SharedType's own nested reference to that same
	// enum inside types_gen.go, which is unqualified because it lives in
	// that package already.
	otherPath := filepath.Join(dir, "ocpp2.0.1", "demo", "compile_other.go")
	otherSource, err := os.ReadFile(otherPath)
	if err != nil {
		t.Fatalf("generated CompileOther message file missing: %v", err)
	}
	if !strings.Contains(string(otherSource), "Compile Other (CSMS -> CS)") {
		t.Errorf("compile_other.go banner does not carry its own manifest direction CSMS -> CS:\n%s", otherSource)
	}
	if !strings.Contains(string(otherSource), "types.SharedStatusType") {
		t.Errorf("compile_other.go missing its qualified reference to the types/-placed enum SharedStatusType:\n%s", otherSource)
	}

	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	// GOPROXY=off turns any accidental network dependency into a loud
	// failure instead of a silent reliance on a warm module cache:
	// this fixture must build offline, and this is the
	// standing guarantee of that, independent of the one-time hand proof
	// recorded on writeCompileSupport.
	cmd.Env = append(os.Environ(), "GOPROXY=off")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generated scratch tree does not compile: %v\n%s", err, output)
	}

	typesSource, err := os.ReadFile(typesPath)
	if err != nil {
		t.Fatal(err)
	}
	typesText := string(typesSource)
	// Positive assertions alongside the negative ones below — a file that
	// simply had no content at all would otherwise satisfy "does not
	// contain reflect/self-import" vacuously.
	for _, want := range []string{"*DateTime", "*CustomData", "Validate.RegisterValidation("} {
		if !strings.Contains(typesText, want) {
			t.Errorf("types_gen.go missing expected unqualified reference %q:\n%s", want, typesText)
		}
	}
	if strings.Contains(typesText, "types.Validate") || strings.Contains(typesText, "types.DateTime") || strings.Contains(typesText, "types.CustomData") {
		t.Errorf("types_gen.go must reference its own package's symbols unqualified, never through a types. prefix:\n%s", typesText)
	}
	// The reflect/self-import negative is anchored to the import section,
	// not scanned across the whole file.
	typesImports := importBlock(t, typesText)
	if strings.Contains(typesImports, "reflect") || strings.Contains(typesImports, "example.test/ocpp/ocpp2.0.1/types") {
		t.Errorf("types_gen.go's own import block carries an unconditional import: %q", typesImports)
	}
}

// TestEmitterMessageFileIsNamedBySnakeCasedMessageName pins the file-path
// half of the placement rule for message roots, which is genuinely
// ambiguous read literally: "each root is emitted in the file named by its
// own schema stem" says nothing about a message with two roots — a
// request stem and a response stem — that must end up sharing one file.
// The real anatomy resolves the ambiguity by example:
// BootNotificationRequest.json and BootNotificationResponse.json both
// land in one boot_notification.go, named after the MESSAGE
// ("BootNotification"), snake-cased — not after either root's own file
// stem. This test adopts and pins that convention: one snake_case file
// per message, under <goTree>/<block>/.
func TestEmitterMessageFileIsNamedBySnakeCasedMessageName(t *testing.T) {
	dir := t.TempDir()
	err := EmitGo(compileIR(), compileManifest(), dir, EmitterOptions{Transform: TransformConfig{Initialisms: []string{"ID"}}})
	requireImplemented(t, "message file path emission", err)
	for _, want := range []string{
		filepath.Join(dir, "ocpp2.0.1", "demo", "compile.go"),
		filepath.Join(dir, "ocpp2.0.1", "demo", "compile_other.go"),
	} {
		if _, err := os.Stat(want); err != nil {
			t.Fatalf("expected message file missing at %s: %v", want, err)
		}
	}
}

func generatedFiles(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		files[rel] = data
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestEmitterOutputIsDeterministicAcrossRuns(t *testing.T) {
	one, two := t.TempDir(), t.TempDir()
	manifest := compileManifest()
	ir := compileIR()
	options := EmitterOptions{Transform: TransformConfig{Initialisms: []string{"ID"}}}
	requireImplemented(t, "first deterministic emit", EmitGo(ir, manifest, one, options))
	requireImplemented(t, "second deterministic emit", EmitGo(ir, manifest, two, options))
	first, second := generatedFiles(t, one), generatedFiles(t, two)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("two emits were not byte-identical: first=%v second=%v", sortedKeys(first), sortedKeys(second))
	}
}

func fileTimes(t *testing.T, dir string) map[string]time.Time {
	t.Helper()
	times := map[string]time.Time{}
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		times[rel] = info.ModTime()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return times
}

// rewindFileTimesToThePast sets every generated .go file's mtime to a fixed
// point well in the past. A subsequent no-op emit that is truly idempotent
// leaves those timestamps untouched; a real-time-based accidental rewrite
// sets them back to "now", which is trivially distinguishable from the
// deliberately-rewound past — no sleep, and no clock-resolution race,
// needed.
func rewindFileTimesToThePast(t *testing.T, dir string) {
	t.Helper()
	past := time.Now().Add(-1 * time.Hour).Truncate(time.Second)
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" {
			return nil
		}
		return os.Chtimes(path, past, past)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEmitterSecondRunDoesNotRewriteUnchangedFiles(t *testing.T) {
	dir := t.TempDir()
	manifest, ir := compileManifest(), compileIR()
	options := EmitterOptions{Transform: TransformConfig{Initialisms: []string{"ID"}}}
	requireImplemented(t, "initial idempotent emit", EmitGo(ir, manifest, dir, options))
	files := generatedFiles(t, dir)
	if len(files) == 0 {
		t.Fatal("initial emit wrote no Go files")
	}
	rewindFileTimesToThePast(t, dir)
	before := fileTimes(t, dir)
	requireImplemented(t, "unchanged-tree emit", EmitGo(ir, manifest, dir, options))
	after := fileTimes(t, dir)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("unchanged emit changed file times: before=%v after=%v", before, after)
	}
}

func TestEmitterGeneratedFilesCarryMarkerAndComputedImports(t *testing.T) {
	dir := t.TempDir()
	requireImplemented(t, "generated-file assembly", EmitGo(compileIR(), compileManifest(), dir, EmitterOptions{}))
	// An anchored date-literal pattern replaces an earlier bare
	// "timestamp:" substring scan, which could neither catch a
	// differently-worded leaked timestamp nor avoid a false positive from
	// unrelated text containing that word.
	datePattern := regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)
	files := generatedFiles(t, dir)
	for path, data := range files {
		if !strings.HasPrefix(string(data), "// Code generated ") || !strings.Contains(string(data), " DO NOT EDIT.\n") {
			t.Errorf("%s is missing the generated-file marker", path)
		}
		formatted, err := format.Source(data)
		if err != nil {
			t.Errorf("%s is not valid Go source: %v", path, err)
		} else if !bytes.Equal(formatted, data) {
			t.Errorf("%s is not gofmt-formatted", path)
		}
		if datePattern.Match(data) {
			t.Errorf("%s embeds a date-like literal, which deterministic generated output forbids", path)
		}
		if strings.Contains(string(data), "internal/codegen") && !strings.HasPrefix(string(data), "// Code generated by internal/codegen.") {
			t.Errorf("%s mentions the generator's own internal path outside the standard marker line", path)
		}
	}

	// The marker/gofmt/date checks above say nothing about
	// WHICH imports a file carries — a file that imported every package
	// unconditionally would still pass every one of them. The computed-
	// import rule needs its own, file-specific assertions: compile.go
	// emits Feature scaffolding (needs reflect), declares and registers
	// its own status enum (needs validator.v9), and references
	// *types.DateTime/*types.CustomData (needs the types import) — all
	// three groups. compile_other.go also emits Feature scaffolding
	// (every message file does, unconditionally) and references
	// *types.Shared/types.SharedStatusType (needs types), but
	// declares no enum of its own and must not import validator.v9 at
	// all. types_gen.go never emits Feature scaffolding and must never
	// import reflect, the computed-import rule's own worked example.
	compileImports := importBlock(t, string(files[filepath.Join("ocpp2.0.1", "demo", "compile.go")]))
	for _, want := range []string{`"reflect"`, `"gopkg.in/go-playground/validator.v9"`, `"example.test/ocpp/ocpp2.0.1/types"`} {
		if !strings.Contains(compileImports, want) {
			t.Errorf("compile.go's import block is missing %s (Feature scaffolding needs reflect; its own status enum needs validator.v9; its date-time/customData fields need types):\n%s", want, compileImports)
		}
	}

	compileOtherImports := importBlock(t, string(files[filepath.Join("ocpp2.0.1", "demo", "compile_other.go")]))
	if !strings.Contains(compileOtherImports, `"reflect"`) {
		t.Errorf("compile_other.go must still import reflect — every message file emits Feature scaffolding:\n%s", compileOtherImports)
	}
	if strings.Contains(compileOtherImports, `"gopkg.in/go-playground/validator.v9"`) {
		t.Errorf("compile_other.go declares no enum of its own and registers no validator; it must not import validator.v9:\n%s", compileOtherImports)
	}
	if !strings.Contains(compileOtherImports, `"example.test/ocpp/ocpp2.0.1/types"`) {
		t.Errorf("compile_other.go references *types.Shared and types.SharedStatusType and must import types:\n%s", compileOtherImports)
	}

	typesGenImports := importBlock(t, string(files[filepath.Join("ocpp2.0.1", "types", "types_gen.go")]))
	if strings.Contains(typesGenImports, `"reflect"`) {
		t.Errorf("types_gen.go emits no Feature scaffolding and must not import reflect:\n%s", typesGenImports)
	}
}

func TestEmitterValidatorRegistrationsAreUniqueAcrossGeneratedTree(t *testing.T) {
	dir := t.TempDir()
	requireImplemented(t, "validator registration assembly", EmitGo(compileIR(), compileManifest(), dir, EmitterOptions{}))
	seen := map[string]string{}
	for path, data := range generatedFiles(t, dir) {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			marker := "RegisterValidation(\""
			start := strings.Index(line, marker)
			if start < 0 {
				continue
			}
			tag := line[start+len(marker):]
			if quote := strings.IndexByte(tag, '"'); quote >= 0 {
				tag = tag[:quote]
			}
			if previous, exists := seen[tag]; exists {
				t.Fatalf("validator %q registered in both %s and %s", tag, previous, path)
			}
			seen[tag] = path
		}
	}
	// compileIR places one enum in the shared types file (SharedStatusEnumType)
	// and one in a message file (CompileStatusEnumType, reached only by
	// Compile), so this scan has real content instead of running over zero
	// or one registration.
	if len(seen) < 2 {
		t.Fatalf("expected at least two distinct validator registrations across the generated tree, found %d: %v", len(seen), seen)
	}
}

// wholeFileManifest and wholeFileIR back the two byte-exact whole-file
// goldens. WidgetCharge is the only emitted message; Gadget and
// Contraption are declared but never emitted (Emit: false), existing only
// to give WidgetLimitsType and WidgetOriginType a reach count of 2 each
// (placement counts over the full declared set, not the emitted subset)
// so both place in the shared types file — WidgetLimitsType is also
// referenced from WidgetChargeResponse, exercising a placed composite
// reference from the message file; WidgetOriginType is not referenced by
// WidgetCharge at all, existing purely to give widget_types_gen.golden two
// types so their sorted (not reach- or declaration-) order is actually
// pinned. Those reach counts are carried by real references, the only way
// a definition enters a reach set: GadgetRequest $refs both shared types
// and ContraptionRequest $refs WidgetOriginType. Every one of those
// references is an optional $ref, so both shared types are reached only
// through pointer fields and both constructors in
// widget_types_gen.golden return pointers — the constructor-generation
// rule's return-kind rule, whose
// value-returning arm is pinned by
// TestEmitterConstructorReturnKindFollowsHowACompositeIsReached on a
// fixture that reaches a composite by value and as a slice element.
// This exact fixture also pins the emitted FILE SET:
// Gadget and Contraption must produce no files of their own at all.
//
// The response enum is named WidgetChargeOutcomeEnumType, not
// WidgetChargeStatusEnumType: with the original "Status" name,
// alphabetical order, declaration order and reach/reference order all
// happened to agree (Reason sorts, is declared, and is referenced first
// either way), so the golden could not actually tell which rule the file
// assembler follows. "Outcome" sorts before "Reason" (O < R) while still
// being declared second and referenced second (WidgetChargeResponse.status
// still resolves it), forcing a real choice. The ordering rule already
// commits to alphabetical-by-Go-type-name for the shared types file; the spec is
// silent on a single message file's own multiple enums, so this task
// adopts the same rule there for one uniform ordering principle, and
// widget_message.golden pins it: WidgetChargeOutcomeType's type
// declaration, const group members and isValidX function all precede
// WidgetChargeReasonType's, even though Reason is declared and referenced
// first.
func wholeFileManifest() Manifest {
	return Manifest{
		Version: "v201", GoModule: "example.test/widgets", GoTree: "ocpp2.0.1", TypesPackage: "example.test/widgets/ocpp2.0.1/types",
		Messages: []ManifestMessage{
			{Name: "WidgetCharge", Block: "widgets", Direction: "CS->CSMS", Request: "WidgetChargeRequest.json", Response: "WidgetChargeResponse.json", Emit: true},
			{Name: "Gadget", Block: "widgets", Direction: "CS->CSMS", Request: "GadgetRequest.json", Response: "GadgetResponse.json", Emit: false},
			{Name: "Contraption", Block: "widgets", Direction: "CS->CSMS", Request: "ContraptionRequest.json", Response: "ContraptionResponse.json", Emit: false},
		},
	}
}

func wholeFileIR() IR {
	return IR{
		Definitions: []Definition{
			{Name: "WidgetChargeRequest", Kind: DefinitionRoot, Properties: []Property{
				{Name: "reason", Ref: "#/definitions/WidgetChargeReasonEnumType", EnumDefinition: "WidgetChargeReasonEnumType", Required: true},
				{Name: "retries", Type: "integer"},
				{Name: "priority", Type: "integer", Minimum: float64Ptr(1)},
				{Name: "customData", Ref: "#/definitions/CustomDataType"},
			}},
			{Name: "WidgetChargeResponse", Kind: DefinitionRoot, Properties: []Property{
				{Name: "status", Ref: "#/definitions/WidgetChargeOutcomeEnumType", EnumDefinition: "WidgetChargeOutcomeEnumType", Required: true},
				{Name: "limits", Ref: "#/definitions/WidgetLimitsType"},
				{Name: "customData", Ref: "#/definitions/CustomDataType"},
			}},
			{Name: "WidgetChargeReasonEnumType", Kind: DefinitionEnum, Values: []string{"Manual", "Scheduled"}},
			{Name: "WidgetChargeOutcomeEnumType", Kind: DefinitionEnum, Values: []string{"Accepted", "Rejected"}},
			{Name: "CustomDataType", Kind: DefinitionObject, Reserved: true},
			{Name: "WidgetLimitsType", Kind: DefinitionObject, Properties: []Property{
				{Name: "maxAmps", Type: "integer", Minimum: float64Ptr(1)},
				{Name: "notes", Type: "string", MaxLength: intPtr(200)},
			}},
			{Name: "WidgetOriginType", Kind: DefinitionObject, Properties: []Property{
				{Name: "country", Type: "string"},
				{Name: "city", Type: "string"},
			}},
			{Name: "GadgetRequest", Kind: DefinitionRoot, Properties: []Property{
				{Name: "limits", Ref: "#/definitions/WidgetLimitsType"},
				{Name: "origin", Ref: "#/definitions/WidgetOriginType"},
			}},
			{Name: "GadgetResponse", Kind: DefinitionRoot},
			{Name: "ContraptionRequest", Kind: DefinitionRoot, Properties: []Property{
				{Name: "origin", Ref: "#/definitions/WidgetOriginType"},
			}},
			{Name: "ContraptionResponse", Kind: DefinitionRoot},
		},
		Messages: []Message{
			{Name: "WidgetCharge", Block: "widgets", Direction: "CS->CSMS", Request: "WidgetChargeRequest.json", Response: "WidgetChargeResponse.json", RequestRoot: "WidgetChargeRequest", ResponseRoot: "WidgetChargeResponse", Roots: []string{"WidgetChargeRequest", "WidgetChargeResponse"}, Reach: []string{"CustomDataType", "WidgetChargeOutcomeEnumType", "WidgetChargeReasonEnumType", "WidgetLimitsType"}, Emit: true},
			{Name: "Gadget", Block: "widgets", Direction: "CS->CSMS", Request: "GadgetRequest.json", Response: "GadgetResponse.json", RequestRoot: "GadgetRequest", ResponseRoot: "GadgetResponse", Roots: []string{"GadgetRequest", "GadgetResponse"}, Reach: []string{"WidgetLimitsType", "WidgetOriginType"}, Emit: false},
			{Name: "Contraption", Block: "widgets", Direction: "CS->CSMS", Request: "ContraptionRequest.json", Response: "ContraptionResponse.json", RequestRoot: "ContraptionRequest", ResponseRoot: "ContraptionResponse", Roots: []string{"ContraptionRequest", "ContraptionResponse"}, Reach: []string{"WidgetOriginType"}, Emit: false},
		},
	}
}

func assertGoldenFileBytes(t *testing.T, gotPath, wantPath string) {
	t.Helper()
	got, err := os.ReadFile(gotPath)
	if err != nil {
		t.Fatalf("read generated file %s: %v", gotPath, err)
	}
	want, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read golden file %s: %v", wantPath, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s did not match %s byte for byte:\n--- got ---\n%s\n--- want ---\n%s", gotPath, wantPath, got, want)
	}
}

// TestEmitterWholeFileGoldensPinExactBytes compares two byte-exact
// whole-file goldens against real EmitGo output, hand-written by applying
// the mapping rules plus boot_notification.go's own anatomy. The
// explanation lives here, in this test's own comment, rather than pointing
// at "the goldens' own file headers" as an earlier version of this comment
// did — a byte-exact golden compared with assertGoldenFileBytes cannot
// carry a leading explanatory comment the way the small readGoldenSnippet
// fixtures elsewhere in this file do (stripping a header there is safe
// because those compare trimmed text; here the golden's bytes must equal
// EmitGo's literal output, which starts directly at the generated-file marker, so
// any leading comment would itself be a mismatch).
//
// widget_message.golden pins: two enums sharing one init() (in
// alphabetical-by-Go-type-name order — see wholeFileIR's own
// comment for why the enum was renamed to make this order actually
// observable), a placed composite reference (Limits *types.WidgetLimits),
// customData on both structs, both optionality arms (Retries *int with no
// minimum vs. Priority int with minimum 1), a non-empty
// EmitterOptions.Overrides row composed end to end (Priority
// gains an appended lte=10, mirroring the override-composition rule's own
// BootNotificationResponse interval worked example — validate:"omitempty,gte=1,lte=10", the base
// tag set with the override's token appended, never lte=10 alone),
// constructors, the Feature scaffolding (GetFeatureName exactly 3 times),
// the FeatureName const value, GetRequestType/GetResponseType's reflect
// bindings, the word-split banner ("Widget Charge" from message name
// "WidgetCharge"), the computed three-group import block, and the
// V-table's element order. widget_types_gen.golden pins the shared file's
// package clause, its Go-type-name sort order (WidgetLimits before
// WidgetOrigin, though WidgetOrigin is declared second in reach order),
// its own (empty, since neither type needs one) import block, and its
// NewWidgetLimits/NewWidgetOrigin constructors — both zero-argument
// (neither type has a required field) and both interleaved immediately
// after their own type, not gathered at the file's end: reading
// ocpp2.0.1/types/types.go shows every hand-written shared-type
// constructor (NewStatusInfo, NewConsumptionCost, NewChargingSchedule, …)
// following its own type the same way, which is the convention this
// shared-types-file golden matches. A message file's own constructors
// follow a DIFFERENT, already-established convention instead — grouped
// together after Feature scaffolding, matching boot_notification.go's own
// NewBootNotificationRequest/NewBootNotificationResponse pair at lines
// 120-127 — because a message file's V-table gives "constructors" one
// fixed slot after Feature scaffolding, a slot types_gen.go has no
// equivalent of (it never emits Feature scaffolding at all).
//
// Also pinned negatively by this same pair of
// goldens: a composite gains a CustomData field only when its OWN schema
// declares a customData $ref property (the CustomData rule's "every
// composite" reads as "every composite whose schema declares one", not as the emitter
// injecting the field independent of the schema) — WidgetChargeRequest and
// WidgetChargeResponse both declare customData and both carry the field;
// WidgetLimits and WidgetOrigin declare no such property and correctly
// carry none, in the very same generated tree.
func TestEmitterWholeFileGoldensPinExactBytes(t *testing.T) {
	dir := t.TempDir()
	options := EmitterOptions{
		Transform: transformFixture(t),
		Overrides: []OverrideRow{{Version: "v201", Definition: "WidgetChargeRequest", Property: "priority", Rule: "add", Tag: "lte=10"}},
	}
	err := EmitGo(wholeFileIR(), wholeFileManifest(), dir, options)
	requireImplemented(t, "whole-file golden emission", err)

	assertGoldenFileBytes(t,
		filepath.Join(dir, "ocpp2.0.1", "widgets", "widget_charge.go"),
		fixturePath("golden", "widget_message.golden"))
	assertGoldenFileBytes(t,
		filepath.Join(dir, "ocpp2.0.1", "types", "types_gen.go"),
		fixturePath("golden", "widget_types_gen.golden"))

	messageSource, err := os.ReadFile(filepath.Join(dir, "ocpp2.0.1", "widgets", "widget_charge.go"))
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(messageSource), "GetFeatureName"); count != 3 {
		t.Fatalf("GetFeatureName must appear exactly 3 times (Feature, Request, Response), found %d", count)
	}
}

// TestEmitterExactFileSetAndNoGeneratedCustomDataType pins two whole-tree
// facts the byte-exact goldens above cannot, because assertGoldenFileBytes
// only ever compares two individual files' content, never the tree's
// shape.
//
// The file-SET assertion: sortedKeys of every .go file EmitGo wrote
// must equal exactly the two files the byte-exact goldens above already
// check the CONTENT of — Gadget and Contraption are declared with Emit:
// false and must produce no gadget.go/contraption.go of their own at all,
// even though their reach sets are exactly what gives WidgetLimitsType and
// WidgetOriginType a reach count of 2 apiece and places both in the shared
// types file (the placement rule: placement is computed over the full
// declared set, not the emitted subset — this is that computation's
// file-system consequence made directly checkable).
//
// The tree-wide negative promised by the CustomData rule: no file
// anywhere in the emitted tree ever declares a generated CustomData or
// CustomDataType type, even though CustomData FIELDS appear on every
// composite whose own schema declares one (WidgetChargeRequest and
// WidgetChargeResponse both do here). TestEmitterNoDomainFieldsStillEmitsCustomDataAndZeroArgumentConstructor
// already checks this for one RenderMessage call's own output; this test
// gives the same negative real, tree-wide teeth over everything a whole
// EmitGo run produces.
func TestEmitterExactFileSetAndNoGeneratedCustomDataType(t *testing.T) {
	dir := t.TempDir()
	err := EmitGo(wholeFileIR(), wholeFileManifest(), dir, EmitterOptions{Transform: transformFixture(t)})
	requireImplemented(t, "whole-file set emission", err)

	files := generatedFiles(t, dir)
	want := []string{
		filepath.Join("ocpp2.0.1", "types", "types_gen.go"),
		filepath.Join("ocpp2.0.1", "widgets", "widget_charge.go"),
	}
	if got := sortedKeys(files); !reflect.DeepEqual(got, want) {
		t.Fatalf("generated file set = %v, want exactly %v (Emit:false messages Gadget/Contraption must produce no files of their own)", got, want)
	}

	for path, data := range files {
		if strings.Contains(string(data), "type CustomData ") || strings.Contains(string(data), "type CustomDataType ") {
			t.Fatalf("%s declares a generated CustomData/CustomDataType type; both names are reserved for the hand-written type, never a generated one, anywhere in the emitted tree", path)
		}
	}
}

func sortedKeys(values map[string][]byte) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
