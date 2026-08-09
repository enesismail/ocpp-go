package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func overrideFixture(t *testing.T, name string) string {
	t.Helper()
	return fixturePath("overrides", name)
}

// overrideIR is the shared fixture IR every governance test resolves its
// rows against. RecordFieldFixture is the workhorse target for the plain
// record-completeness and grammar checks (a single integer property keeps
// those fixtures independent of the worked BootNotificationResponse
// example); StringFixture and SliceFixture exist so the field-kind rule --
// a validator family may only load onto a field whose mapped Go kind it can
// actually constrain -- can be exercised against a string and a slice, not
// only a numeric property. Messages carries the one entry the
// report-rendering tests need: BootNotification, whose Block
// ("provisioning") and Name are what RenderOverridesReport is expected to
// derive an emitted file path from -- see
// TestOverridesReportIsDeterministicAcrossTwoRuns for exactly how.
func overrideIR() IR {
	return IR{
		Definitions: []Definition{
			{Name: "BootNotificationResponse", Kind: DefinitionRoot, Files: []string{"BootNotificationResponse.json"}, Properties: []Property{
				{Name: "interval", Type: "integer", Required: true},
				{Name: "idTokenInfo", Ref: "#/definitions/IdTokenInfo"},
			}},
			{Name: "IdTokenInfo", Kind: DefinitionObject, Files: []string{"BootNotificationResponse.json"}, Properties: []Property{
				{Name: "chargingPriority", Type: "integer"},
			}},
			{Name: "StringFixture", Kind: DefinitionObject, Files: []string{"string_fixture.json"}, Properties: []Property{
				{Name: "label", Type: "string"},
			}},
			{Name: "RecordFieldFixture", Kind: DefinitionObject, Files: []string{"record_field_fixture.json"}, Properties: []Property{
				{Name: "value", Type: "integer"},
			}},
			{Name: "SliceFixture", Kind: DefinitionObject, Files: []string{"slice_fixture.json"}, Properties: []Property{
				{Name: "items", Type: "array", Items: &Property{Type: "string"}},
			}},
		},
		Messages: []Message{
			{
				Name:         "BootNotification",
				Block:        "provisioning",
				Direction:    "CS->CSMS",
				Request:      "BootNotificationRequest.json",
				Response:     "BootNotificationResponse.json",
				RequestRoot:  "BootNotificationRequest",
				ResponseRoot: "BootNotificationResponse",
				Roots:        []string{"BootNotificationRequest", "BootNotificationResponse"},
				Reach:        []string{"IdTokenInfo"},
				Emit:         true,
			},
		},
	}
}

// overrideMappings mirrors overrideIR one entry per property: GoKind uses
// the same spelling the emitter's FieldMapping.GoType would ("int", "*int",
// "string", "[]string"), never a schema-type name, per OverrideMapping's
// own doc comment. FieldName mirrors the emitter's FieldMapping.FieldName the same
// way, so a report naming the generated field a row landed on has a real
// source to read it from instead of re-deriving it.
//
// The two entries TestOverrideRootAndSharedDefinitionLookupComposesTags
// composes against -- interval and chargingPriority -- carry the mapping
// the type-mapping rules genuinely produce for their own overrideIR
// property, because that test claims real composition through both
// addressable namespaces and would prove nothing against an invented base
// set. interval is required, so it is a value int whose base set is
// ["required"]. chargingPriority is an OPTIONAL integer with no minimum, so
// its range admits zero and the optionality rule maps it to *int with the
// base set ["omitempty"] -- the same result
// testdata/golden/optional_integer_zero.golden pins for the emitter, and
// never an empty base set, since an optional property always carries
// omitempty (testdata/golden/optional_string.golden states that in as many
// words).
//
// The other three entries are deliberate stand-ins rather than derived
// mappings: StringFixture, RecordFieldFixture and SliceFixture exist to
// give the record-completeness, grammar and field-kind checks a target
// whose BaseTags and GoKind each test picks for itself, so their entries
// here are the neutral starting point a test copies and overwrites for just
// its own target -- the same way the chargingPriority helper below does --
// without a missing map entry masking what that test is actually checking.
// Every test that composes against one of the three supplies the base set
// it means to compose against explicitly, at the call.
//
// Keeping RecordFieldFixture.value spelled "int" beside chargingPriority's
// "*int" is also what puts both of the integer spellings in front of the
// field-kind check: an implementation that recognises only the bare
// spelling as numeric fails in both directions -- it would reject the gte=0
// row
// TestOverrideRootAndSharedDefinitionLookupComposesTags requires to load
// onto *int, and accept the max=10 row the pointer-numeric wrong-kind case
// below requires it to reject.
func overrideMappings() OverrideMappings {
	return OverrideMappings{
		{Definition: "BootNotificationResponse", Property: "interval"}: {
			BaseTags: []string{"required"}, GoKind: "int", FieldName: "Interval",
		},
		{Definition: "IdTokenInfo", Property: "chargingPriority"}: {
			BaseTags: []string{"omitempty"}, GoKind: "*int", FieldName: "ChargingPriority",
		},
		{Definition: "StringFixture", Property: "label"}: {
			BaseTags: []string{"max=20"}, GoKind: "string", FieldName: "Label",
		},
		{Definition: "RecordFieldFixture", Property: "value"}: {
			BaseTags: []string{}, GoKind: "int", FieldName: "Value",
		},
		{Definition: "SliceFixture", Property: "items"}: {
			BaseTags: []string{}, GoKind: "[]string", FieldName: "Items",
		},
	}
}

// chargingPriorityFloorMapping starts from overrideMappings' defaults and
// gives IdTokenInfo.chargingPriority a real floor (gte=5) to tighten
// against. overrideMappings' own default for that target carries no floor
// at all, which the tighten-source check needs (a tighten row's from token
// must be genuinely present in the base tags), but the tighten-direction
// check's weakens/unchanged cases do not: both need gte=5 genuinely present
// in the base set so it is the row's own direction, not merely its
// presence, that a correct loader must judge.
//
// This entry is the mapping a schema declaring `minimum: 5` on that same
// optional property would produce, which is why both of its fields differ
// from the default above: the floor puts zero outside the property's range,
// so the optionality rule maps it to the value arm (int, not *int) with the
// floor appended to the optional base tag set.
func chargingPriorityFloorMapping() OverrideMappings {
	mappings := overrideMappings()
	mappings[OverrideTarget{Definition: "IdTokenInfo", Property: "chargingPriority"}] = OverrideMapping{
		BaseTags: []string{"omitempty", "gte=5"}, GoKind: "int", FieldName: "ChargingPriority",
	}
	return mappings
}

// sliceItemsDiveMapping gives SliceFixture.items a base tag set that
// already carries dive, so an add row proposing a second dive is a genuine
// duplicate-bare-family case rather than the first, legitimate one
// TestOverrideDiveAppliesToSliceFieldsPositiveControl exercises.
func sliceItemsDiveMapping() OverrideMappings {
	mappings := overrideMappings()
	mappings[OverrideTarget{Definition: "SliceFixture", Property: "items"}] = OverrideMapping{
		BaseTags: []string{"dive"}, GoKind: "[]string", FieldName: "Items",
	}
	return mappings
}

// overrideFailureFixture is one hard-fail case for a documented failure
// mode: the fixture file LoadOverrides must reject, and the substrings the
// rejection message must contain.
type overrideFailureFixture struct {
	name  string
	file  string
	wants []string
	// mappings, when set, replaces the second pass's default
	// overrideMappings() for this one fixture -- the tighten-direction
	// check's cases in particular each need a base tag set
	// overrideMappings()'s own defaults don't
	// supply (a floor or ceiling actually present to weaken or leave
	// unchanged). Left nil, a needsMappings case uses overrideMappings()
	// unmodified.
	mappings func() OverrideMappings
	// registeredValidators supplies the tag-grammar rule's "any validator
	// name the emitted tree actually registers" clause. Left nil,
	// LoadOverrides receives nil as every first-pass case always has; the
	// one tag-grammar case that needs an
	// explicitly empty (not merely absent) registry sets this to a non-nil
	// empty slice.
	registeredValidators []string
	// noMultilineLeak additionally asserts the failure message contains no
	// newline -- yaml.v3's own strict-decode error is multi-line raw text,
	// and the unknown-YAML-field check is the failure mode responsible for
	// re-wrapping it onto one line before it reaches a caller.
	noMultilineLeak bool
}

// overrideFailureCase is one documented hard-fail: its name, which
// validation pass and stage own it, and every fixture case that exercises
// it.
type overrideFailureCase struct {
	name          string
	pass          string
	stage         string
	needsMappings bool
	fixtures      []overrideFailureFixture
}

// documentedOverrideFailures inventories the twelve governed hard failures
// the loader is required to raise. Each entry classifies one failure --
// which of the two validation passes owns it, and which inputs that pass
// judges it against -- and carries every fixture that failure needs.
// TestOverrideFailureModesAreExhaustivelyAssignedToValidationPasses reads
// the table for both jobs at once, so an entry carrying no fixture, or a
// fixture whose expected message no supplied input could ever produce,
// cannot hide behind a classification nothing exercises. The order is
// fixed: that test names the schema-reachability entry by its position.
var documentedOverrideFailures = [12]overrideFailureCase{
	{
		name: "missing or empty governance record field", pass: "first validation pass", stage: "YAML fields",
		fixtures: []overrideFailureFixture{
			{name: "missing rule", file: "govern_01.yaml", wants: []string{"RecordFieldFixture.value", "rule"}},
			{name: "missing rationale", file: "govern_02.yaml", wants: []string{"RecordFieldFixture.value", "rationale"}},
			{name: "missing source", file: "govern_03.yaml", wants: []string{"RecordFieldFixture.value", "source"}},
			{name: "missing author", file: "govern_04.yaml", wants: []string{"RecordFieldFixture.value", "author"}},
			{name: "missing date", file: "govern_05.yaml", wants: []string{"RecordFieldFixture.value", "date"}},
			{name: "empty rule", file: "govern_06.yaml", wants: []string{"RecordFieldFixture.value", "rule"}},
			{name: "empty rationale", file: "govern_07.yaml", wants: []string{"RecordFieldFixture.value", "rationale"}},
			{name: "empty source", file: "govern_08.yaml", wants: []string{"RecordFieldFixture.value", "source"}},
			{name: "empty author", file: "govern_09.yaml", wants: []string{"RecordFieldFixture.value", "author"}},
			{name: "empty date", file: "govern_10.yaml", wants: []string{"RecordFieldFixture.value", "date"}},
		},
	},
	{
		name: "unparseable governance date", pass: "first validation pass", stage: "YAML fields",
		fixtures: []overrideFailureFixture{
			{name: "unparseable date", file: "govern_11.yaml", wants: []string{"RecordFieldFixture.value", "date"}},
		},
	},
	{
		name: "unknown rule value", pass: "first validation pass", stage: "YAML fields",
		fixtures: []overrideFailureFixture{
			{name: "unknown rule", file: "govern_12.yaml", wants: []string{"RecordFieldFixture.value", "rule", "replace"}},
		},
	},
	{
		name: "tighten without its source token", pass: "first validation pass", stage: "YAML fields",
		fixtures: []overrideFailureFixture{
			{name: "tighten without from", file: "govern_13.yaml", wants: []string{"RecordFieldFixture.value", "from"}},
		},
	},
	{
		name: "source token absent from the base tags", pass: "second validation pass", stage: "base tags and mapped kind",
		needsMappings: true,
		fixtures: []overrideFailureFixture{
			{name: "absent source token", file: "govern_20.yaml", wants: []string{"IdTokenInfo.chargingPriority", "gte=5", "base"}},
		},
	},
	{
		name: "tighten that weakens a bound", pass: "second validation pass", stage: "base tags and mapped kind",
		needsMappings: true,
		fixtures: []overrideFailureFixture{
			{name: "floor moves down", file: "govern_21.yaml", wants: []string{"IdTokenInfo.chargingPriority", "gte=5", "tighten"}, mappings: chargingPriorityFloorMapping},
			{name: "ceiling moves up", file: "tighten_widens_ceiling.yaml", wants: []string{"StringFixture.label", "max=20", "tighten"}},
			{name: "bound left unchanged", file: "tighten_unchanged_bound.yaml", wants: []string{"IdTokenInfo.chargingPriority", "gte=5", "tighten"}, mappings: chargingPriorityFloorMapping},
		},
	},
	{
		name: "validator token for the wrong Go kind", pass: "second validation pass", stage: "base tags and mapped kind",
		needsMappings: true,
		fixtures: []overrideFailureFixture{
			{name: "length constraint against a numeric field", file: "wrong_kind.yaml", wants: []string{"BootNotificationResponse.interval", "max=10", "int"}},
			// The same rejection, one line down, against the pointer
			// spelling of that same numeric kind: chargingPriority is an
			// optional integer admitting zero, so it maps to *int (see
			// overrideMappings). This is the half of pointer-numeric
			// recognition a hard-fail can prove -- an implementation that
			// treats an unrecognised "*int" as "no kind constraint applies"
			// accepts this row and fails here. The other half is proved on
			// the loading side by
			// TestOverrideRootAndSharedDefinitionLookupComposesTags, whose
			// gte=0 row must load onto that same *int target: an
			// implementation that treats "*int" as non-numeric instead
			// rejects a legitimate row there. Neither test alone forces the
			// pointer spelling to be read as numeric; together they do.
			{name: "length constraint against a pointer-numeric field", file: "wrong_kind_pointer_numeric.yaml", wants: []string{"IdTokenInfo.chargingPriority", "max=10", "int"}},
			{name: "dive against a non-slice field", file: "govern_27.yaml", wants: []string{"RecordFieldFixture.value", "dive"}},
			{name: "gte against a non-numeric field", file: "govern_28.yaml", wants: []string{"StringFixture.label", "gte=5"}},
		},
	},
	{
		name: "tag grammar violation", pass: "first validation pass", stage: "YAML fields",
		fixtures: []overrideFailureFixture{
			{name: "unknown tag token", file: "govern_14.yaml", wants: []string{"RecordFieldFixture.value", "bogus=1"}},
			{name: "unknown from token", file: "govern_15.yaml", wants: []string{"RecordFieldFixture.value", "bogus=1"}},
			{name: "required excluded at the loader", file: "govern_31.yaml", wants: []string{"RecordFieldFixture.value", "required", "optionality"}},
			{name: "omitempty excluded at the loader", file: "govern_32.yaml", wants: []string{"RecordFieldFixture.value", "omitempty", "optionality"}},
			// A bare, unrecognized token with nothing registered must still
			// hard-fail: a loader that presumes any bare token outside the
			// fixed grammar is "a registered validator name" without
			// actually checking registeredValidators would accept this row
			// by accident.
			{name: "bare unrecognised token with nothing registered", file: "bare_token_unregistered.yaml", wants: []string{"RecordFieldFixture.value", "structonly"}, registeredValidators: []string{}},
		},
	},
	{
		name: "duplicate override key", pass: "first validation pass", stage: "YAML fields",
		fixtures: []overrideFailureFixture{
			{name: "duplicate key", file: "govern_16.yaml", wants: []string{"v201", "RecordFieldFixture.value", "gte=0"}},
		},
	},
	{
		name: "unreachable schema property or root", pass: "first validation pass", stage: "IR reachability",
		fixtures: []overrideFailureFixture{
			{name: "unreachable property", file: "unreachable_property.yaml", wants: []string{"BootNotificationResponse.missingProperty"}},
			{name: "unreachable root", file: "unreachable_root.yaml", wants: []string{"MissingMessageRoot.interval"}},
			{name: "near-miss property on the wrong definition", file: "govern_33.yaml", wants: []string{"BootNotificationResponse", "chargingPriority"}},
		},
	},
	{
		name: "unknown YAML field", pass: "first validation pass", stage: "YAML fields",
		fixtures: []overrideFailureFixture{
			// unknown_field.yaml carries its unrecognised field at the
			// document's own top level (a sibling of version/tagOverrides/
			// dedupAllowlist), not inside any row, so there is no row key
			// to attribute the failure to. That is exactly why this one
			// fixture wants the field name
			// alone while the three row-level fixtures below also demand
			// the row's own key -- a row-level unknown field does have a
			// row to name, and the failure should say so.
			{name: "unknown top-level field", file: "unknown_field.yaml", wants: []string{"unexpected"}, noMultilineLeak: true},
			{name: "row cannot introduce goType", file: "govern_17.yaml", wants: []string{"RecordFieldFixture.value", "goType"}, noMultilineLeak: true},
			{name: "row cannot introduce fieldName", file: "govern_18.yaml", wants: []string{"RecordFieldFixture.value", "fieldName"}, noMultilineLeak: true},
			{name: "row cannot introduce optional", file: "govern_19.yaml", wants: []string{"RecordFieldFixture.value", "optional"}, noMultilineLeak: true},
		},
	},
	{
		name: "duplicate validator family added", pass: "second validation pass", stage: "base tags and mapped kind",
		needsMappings: true,
		fixtures: []overrideFailureFixture{
			{name: "add duplicates an existing max family", file: "duplicate_family_add.yaml", wants: []string{"StringFixture.label", "max=10", "tighten"}},
			{name: "add duplicates an existing bare dive family", file: "duplicate_bare_family_add.yaml", wants: []string{"SliceFixture.items", "dive", "tighten"}, mappings: sliceItemsDiveMapping},
		},
	},
}

// TestOverrideFailureModesAreExhaustivelyAssignedToValidationPasses checks
// the inventory above -- every entry names a pass and a stage and carries at
// least one fixture, no name repeats, the table's size matches the number of
// distinct entries, and schema reachability sits in the first pass, where
// resolving a row against the IR alone is all it needs -- and then, for
// every fixture the inventory carries, calls LoadOverrides and requires the
// classified hard failure. Driving the loader from the inventory is what
// keeps the classification honest: a fixture whose expected message no input
// could ever supply is indistinguishable by inspection from one that
// genuinely works, and only running it tells the two apart. Every case
// therefore runs here for real, against overrideIR's and overrideMappings'
// own fields.
func TestOverrideFailureModesAreExhaustivelyAssignedToValidationPasses(t *testing.T) {
	seen := make(map[string]bool, len(documentedOverrideFailures))
	for _, failure := range documentedOverrideFailures {
		if failure.name == "" || failure.pass == "" || failure.stage == "" {
			t.Fatalf("failure inventory contains an incomplete assignment: %#v", failure)
		}
		if seen[failure.name] {
			t.Fatalf("failure inventory assigns %q more than once", failure.name)
		}
		seen[failure.name] = true
		if len(failure.fixtures) == 0 {
			t.Fatalf("failure inventory entry %q carries no fixture case to exercise the loader with", failure.name)
		}
	}
	if len(seen) != len(documentedOverrideFailures) {
		t.Fatalf("failure inventory has %d distinct entries, want all %d documented failure modes", len(seen), len(documentedOverrideFailures))
	}
	if documentedOverrideFailures[9].pass != "first validation pass" {
		t.Fatalf("schema reachability must remain in the first validation pass after IR resolution")
	}

	for _, failure := range documentedOverrideFailures {
		failure := failure
		t.Run(failure.name, func(t *testing.T) {
			for _, fixture := range failure.fixtures {
				fixture := fixture
				t.Run(fixture.name, func(t *testing.T) {
					var mappings OverrideMappings
					if failure.needsMappings {
						mappings = overrideMappings()
						if fixture.mappings != nil {
							mappings = fixture.mappings()
						}
					}
					_, err := LoadOverrides(overrideFixture(t, fixture.file), overrideIR(), mappings, fixture.registeredValidators)
					requireExpectedHardFailureContains(t, failure.name+": "+fixture.name, err, fixture.wants...)
					if fixture.noMultilineLeak && err != nil && strings.Contains(err.Error(), "\n") {
						t.Fatalf("%s: %s leaked yaml.v3 multi-line output: %q", failure.name, fixture.name, err.Error())
					}
				})
			}
		})
	}
}

// TestOverrideFirstPassAloneCanSucceed pins the order-free reading of the
// two-pass validation rule: mappings is exclusively second-pass input (the
// base-tags-and-mapped-kind checks -- source-token-absent, tighten-direction,
// field-kind and duplicate-validator-family -- all need the type-mapping
// rules' output for it), so a nil mappings must still let the first pass
// (the YAML-fields and schema-reachability checks) run to completion and
// SUCCEED on a fixture that is clean by every first-pass rule -- not fail by
// naming "the second pass" as deferred, and not fail at all. Without this,
// no override-governance test could run at all until the type-mapping rules
// that supply the second pass existed, even though nothing about the first
// pass actually depends on them.
func TestOverrideFirstPassAloneCanSucceed(t *testing.T) {
	config, err := LoadOverrides(overrideFixture(t, "lookup_both_namespaces.yaml"), overrideIR(), nil, nil)
	requireImplemented(t, "first-pass-only load", err)
	if len(config.TagOverrides) != 2 {
		t.Fatalf("first-pass-only load returned %d rows, want 2", len(config.TagOverrides))
	}
}

// TestOverrideConfigWithASecondDocumentIsRejected covers what a loader
// reading one YAML document never sees, the same way
// TestManifestWithASecondDocumentIsRejected covers it for the manifest.
// multi_document.yaml's first document is the same two-row config
// lookup_both_namespaces.yaml carries -- already asserted to load in
// TestOverrideFirstPassAloneCanSucceed above -- followed by a second
// document declaring an unrecognized top-level field and a row that would
// fail both grammar and date parsing. A loader that decodes only the first
// document accepts the file with every one of those unread, which is
// precisely the content the strict-field decode of the first document
// exists to reject.
func TestOverrideConfigWithASecondDocumentIsRejected(t *testing.T) {
	_, err := LoadOverrides(overrideFixture(t, "multi_document.yaml"), overrideIR(), nil, nil)
	requireExpectedHardFailureContains(t, "single-document override config check", err,
		"multi_document.yaml", "more than one YAML document")
}

// TestOverrideConfigUnknownVersionIsRejected requires that the document's
// own version name a schema generation this loader recognizes.
// unknown_version.yaml is otherwise a clean, single-row config -- every
// other check would let it load -- so an implementation that decoded the
// version field without ever checking its value would pass every other
// test in this package while silently loading rows nobody confirmed were
// checked against the right schema generation's ir.
func TestOverrideConfigUnknownVersionIsRejected(t *testing.T) {
	_, err := LoadOverrides(overrideFixture(t, "unknown_version.yaml"), overrideIR(), nil, nil)
	requireExpectedHardFailureContains(t, "unknown override config version", err, "v99", "v201")
}

// TestTransformUnknownYAMLFieldIsRejectedAndWrapped reads its fixture from
// testdata/config, not testdata/overrides: unknown_transform_field.yaml
// exercises LoadTransformConfig, the loader for transform.yaml, which has
// nothing to do with the tag-override table -- it belongs beside
// transform.yaml's own fixture (testdata/config/transform.yaml, see
// emitter_test.go's transformFixture), not inside the override-fixture
// helper that every test above this one uses.
func TestTransformUnknownYAMLFieldIsRejectedAndWrapped(t *testing.T) {
	_, err := LoadTransformConfig(fixturePath("config", "unknown_transform_field.yaml"))
	requireExpectedHardFailureContains(t, "unknown transform field", err, "unexpected")
	if err != nil && strings.Contains(err.Error(), "\n") {
		t.Fatalf("unknown transform field leaked yaml.v3 multi-line output: %q", err.Error())
	}
}

// TestOverrideDuplicateKeyIncludesTheTagComponent is the positive control
// for the duplicate-override-key check: two rows can share (version,
// definition, property) and still not collide, as long as their tag
// differs. A loader that dedups on (definition, property) alone -- ignoring
// tag, the row-lookup key's fourth component -- would wrongly reject this
// fixture; both rows must load.
func TestOverrideDuplicateKeyIncludesTheTagComponent(t *testing.T) {
	config, err := LoadOverrides(overrideFixture(t, "govern_23.yaml"), overrideIR(), nil, nil)
	requireImplemented(t, "dedup key tag component", err)
	if len(config.TagOverrides) != 2 {
		t.Fatalf("loaded %d rows for two same-(definition,property) rows with different tags, want both to load: %#v", len(config.TagOverrides), config.TagOverrides)
	}
}

// TestOverrideMaxAppliesToSliceCardinalityPositiveControl is the positive
// control for the field-kind rule's sub-rule that the failure-mode table
// only tests negatively: max=/min= apply to a string (length) or a slice
// (item cardinality), not only to a string. Without this, a field-kind
// implementation that hard-fails max= against every non-string field --
// overshooting the rule -- would
// pass every hard-fail case while silently rejecting every legitimate
// cardinality override.
func TestOverrideMaxAppliesToSliceCardinalityPositiveControl(t *testing.T) {
	config, err := LoadOverrides(overrideFixture(t, "govern_29.yaml"), overrideIR(), overrideMappings(), nil)
	requireImplemented(t, "max on a slice property (positive control)", err)
	if len(config.TagOverrides) != 1 {
		t.Fatalf("loaded %d override rows, want the single valid SliceFixture.items row", len(config.TagOverrides))
	}
}

// TestOverrideDiveAppliesToSliceFieldsPositiveControl is the positive
// control for the field-kind rule's dive sub-rule: dive must load onto a
// property that genuinely maps to a slice, or a field-kind implementation
// that rejects dive against every field -- overshooting the rule the same
// way an over-broad
// max=/min= implementation would -- would pass the negative case
// ("dive against a non-slice field" above) while making the validator
// useless for its one legitimate use.
func TestOverrideDiveAppliesToSliceFieldsPositiveControl(t *testing.T) {
	config, err := LoadOverrides(overrideFixture(t, "dive_on_slice_positive_control.yaml"), overrideIR(), overrideMappings(), nil)
	requireImplemented(t, "dive on a slice property (positive control)", err)
	if len(config.TagOverrides) != 1 {
		t.Fatalf("loaded %d override rows, want the single valid SliceFixture.items dive row", len(config.TagOverrides))
	}
}

// TestOverrideTagAcceptsARegisteredValidatorName is the tag-grammar rule's
// untested clause: the permitted grammar is not only the fixed token list (required,
// omitempty, max=, min=, gte=, lte=, dive) but "any validator name the
// emitted tree actually registers". fixtureRegisteredValidator is not in
// the fixed list and would fail grammar on its own; supplying it through
// registeredValidators must make the row load.
func TestOverrideTagAcceptsARegisteredValidatorName(t *testing.T) {
	config, err := LoadOverrides(overrideFixture(t, "govern_30.yaml"), overrideIR(), nil, []string{"fixtureRegisteredValidator"})
	requireImplemented(t, "registered validator tag", err)
	if len(config.TagOverrides) != 1 {
		t.Fatalf("loaded %d override rows, want the single registered-validator row", len(config.TagOverrides))
	}
}

// TestOverrideRowMustCarryPropertyAndTag closes a gap the failure inventory
// above does not name: a tagOverrides row's own key fields,
// definition/property/tag, are not among the governance-record fields the
// completeness check enumerates (rule, rationale, source, author, date), but
// a row with no property or no tag is
// exactly as incomplete as one missing its rationale. This is deliberately
// distinct from the dedup allowlist's shape: the dedup-allowlist governance
// rule leaves Property empty on every allowlist entry by design (see
// TestDedupAllowlistValidEntryLoads
// below, still expected to load), so this check must reject a bare
// tagOverrides row without also breaking that allowlist positive control --
// property/tag are required on a tagOverrides row precisely because they
// are not required, and never will be, on an allowlist row.
func TestOverrideRowMustCarryPropertyAndTag(t *testing.T) {
	for _, tc := range []struct {
		name  string
		file  string
		wants []string
	}{
		// property is itself absent, so only the definition half of the
		// row's key can be named -- there is no property to complete it.
		{name: "missing property", file: "tagoverride_row_missing_property.yaml", wants: []string{"RecordFieldFixture", "property"}},
		{name: "missing tag", file: "tagoverride_row_missing_tag.yaml", wants: []string{"RecordFieldFixture.value", "tag"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadOverrides(overrideFixture(t, tc.file), overrideIR(), nil, nil)
			requireExpectedHardFailureContains(t, "tagOverrides row missing a key field: "+tc.name, err, tc.wants...)
		})
	}
}

// TestOverrideMappingsMissingATargetIsRejected pins OverrideMappings' own
// contract (see its doc comment): supplying a non-nil map for the second
// pass promises a mapping for every row the loader checks against it, so a
// target absent from that non-nil map is a caller error, not license to
// treat the row as if it had no base tags and no Go kind. This starts from
// a fixture (govern_22.yaml, a tighten row moving RecordFieldFixture.value
// from gte=0 to gte=1) and, before deleting the one map entry it needs,
// first gives that entry the same genuinely-loadable state
// TestOverrideValidTightenRowLoadsAndComposes uses for this identical
// fixture (BaseTags []string{"gte=0"}, the tighten's own "from" actually
// present) rather than overrideMappings()' own default for this target
// (BaseTags []string{}, empty). Deleting the default entry instead would
// make a poor counterfactual: an empty base tag set already fails the
// tighten-source check's "tighten from an absent base tag" check on its
// own, so the row would fail whether or not the map entry existed, and a
// hard failure naming RecordFieldFixture.value could come from either
// check. Starting from a base tag set the fixture is known to load cleanly
// against means the deletion below is the only change, so any failure can
// only be explained by the missing map entry itself.
//
// The assertion below pins "missing mapping" alongside the row key for the
// same reason: RecordFieldFixture.value alone also appears in a
// tighten-source or field-kind hard-failure message, so a field-blind
// implementation that silently substitutes OverrideMapping's zero value for
// the missing entry -- instead of rejecting the caller's incomplete map
// outright -- could still produce a message naming the row and pass on that
// substring alone. Requiring "missing mapping" too means only the loader's
// own caller-error message, not a coincidentally-matching tighten-source or
// field-kind message, can satisfy this test.
func TestOverrideMappingsMissingATargetIsRejected(t *testing.T) {
	mappings := overrideMappings()
	mappings[OverrideTarget{Definition: "RecordFieldFixture", Property: "value"}] = OverrideMapping{
		BaseTags: []string{"gte=0"}, GoKind: "int", FieldName: "Value",
	}
	delete(mappings, OverrideTarget{Definition: "RecordFieldFixture", Property: "value"})
	_, err := LoadOverrides(overrideFixture(t, "govern_22.yaml"), overrideIR(), mappings, nil)
	requireExpectedHardFailureContains(t, "override mappings missing a target", err, "missing mapping", "RecordFieldFixture.value")
}

// The three dedupAllowlist fixtures below exercise the dedup-allowlist
// governance rule: the allowlist is "the same governance applied to a
// different hard-fail", reusing
// OverrideRow's record fields. An allowlist entry names a bare definition
// -- the enum-drift collision the allowlist exists to permit is between
// two same-named definitions, not a (definition, property) pair -- so
// Property is deliberately left empty on every entry here, and reachability
// for an allowlist row resolves the definition alone.

func TestDedupAllowlistEntryMissingFieldIsRejected(t *testing.T) {
	_, err := LoadOverrides(overrideFixture(t, "govern_24.yaml"), overrideIR(), nil, nil)
	requireExpectedHardFailureContains(t, "dedup allowlist missing field", err, "RecordFieldFixture", "rationale")
}

func TestDedupAllowlistUnreachableDefinitionIsRejected(t *testing.T) {
	_, err := LoadOverrides(overrideFixture(t, "govern_25.yaml"), overrideIR(), nil, nil)
	requireExpectedHardFailureContains(t, "dedup allowlist unreachable definition", err, "UnknownDriftDefinition")
}

func TestDedupAllowlistValidEntryLoads(t *testing.T) {
	config, err := LoadOverrides(overrideFixture(t, "govern_26.yaml"), overrideIR(), nil, nil)
	requireImplemented(t, "dedup allowlist valid entry", err)
	if len(config.DedupAllowlist) != 1 {
		t.Fatalf("loaded %d dedup allowlist entries, want 1", len(config.DedupAllowlist))
	}
}

// TestOverrideRowsAreSortedDeterministicallyNotFileOrder pins the
// deterministic-ordering rule for real -- loaded override rows must be
// sorted by their key, not left in file order: govern_34.yaml lists StringFixture
// before RecordFieldFixture, the reverse of sorted order, so a loader that
// merely preserves file order -- which would also pass every other test in
// this file, none of which check row order -- fails here.
func TestOverrideRowsAreSortedDeterministicallyNotFileOrder(t *testing.T) {
	config, err := LoadOverrides(overrideFixture(t, "govern_34.yaml"), overrideIR(), nil, nil)
	requireImplemented(t, "deterministic row ordering", err)
	if len(config.TagOverrides) != 2 {
		t.Fatalf("loaded %d override rows, want 2", len(config.TagOverrides))
	}
	if config.TagOverrides[0].Definition != "RecordFieldFixture" || config.TagOverrides[1].Definition != "StringFixture" {
		t.Fatalf("override rows were not applied in sorted key order (file lists StringFixture first): got %#v", config.TagOverrides)
	}
}

// TestOverrideRootAndSharedDefinitionLookupComposesTags is the dedicated
// lookup fixture the row-addressing rule calls for: BootNotificationResponse
// resolves as a message root (its own schema file's stem, not a
// #/definitions/ entry) and IdTokenInfo resolves as a $ref'd shared
// definition, through the same mechanism. Both rows must also carry the
// document's own version (the duplicate-key check's dedup key includes it,
// so the loader must actually populate it onto each
// row rather than leaving OverrideRow.Version at its YAML-excluded zero
// value), and both must compose correctly against the base tag set
// overrideMappings supplies for their own property -- proving composition,
// not just loading, works through both namespaces. Each row's tag is
// appended AFTER that base set, in the tag order the golden fixtures pin:
// the required root-addressed row composes to "required,gte=0", the
// optional shared-definition row to "omitempty,gte=0". Neither composed
// string is the appended token standing on its own, and neither base token
// may be dropped on the way through.
//
// The shared-definition row doubles as the loading half of pointer-numeric
// recognition (see the pointer-numeric wrong-kind case in the failure
// inventory above): chargingPriority maps to *int, so a field-kind check
// reading only the bare "int" spelling as numeric rejects this legitimate
// gte=0 row and fails here.
func TestOverrideRootAndSharedDefinitionLookupComposesTags(t *testing.T) {
	config, err := LoadOverrides(overrideFixture(t, "lookup_both_namespaces.yaml"), overrideIR(), overrideMappings(), nil)
	requireImplemented(t, "override lookup for message root and shared definition", err)
	if len(config.TagOverrides) != 2 {
		t.Fatalf("loaded %d override rows, want root and shared-definition rows", len(config.TagOverrides))
	}
	var intervalRow, priorityRow OverrideRow
	for _, row := range config.TagOverrides {
		switch {
		case row.Definition == "BootNotificationResponse" && row.Property == "interval":
			intervalRow = row
		case row.Definition == "IdTokenInfo" && row.Property == "chargingPriority":
			priorityRow = row
		}
	}
	if intervalRow.Definition == "" {
		t.Fatalf("loaded override rows do not include BootNotificationResponse.interval")
	}
	if priorityRow.Definition == "" {
		t.Fatalf("loaded override rows do not include IdTokenInfo.chargingPriority")
	}
	if intervalRow.Version != "v201" || priorityRow.Version != "v201" {
		t.Fatalf("loaded rows did not carry the document's own version: interval=%q priority=%q, want %q for both", intervalRow.Version, priorityRow.Version, "v201")
	}

	interval, err := ApplyTagOverride([]string{"required"}, intervalRow)
	requireImplemented(t, "interval override composition", err)
	if got, want := strings.Join(interval, ","), "required,gte=0"; got != want {
		t.Fatalf("BootNotificationResponse.interval tags = %q, want %q", got, want)
	}

	priority, err := ApplyTagOverride([]string{"omitempty"}, priorityRow)
	requireImplemented(t, "chargingPriority override composition", err)
	if got, want := strings.Join(priority, ","), "omitempty,gte=0"; got != want {
		t.Fatalf("IdTokenInfo.chargingPriority tags = %q, want %q", got, want)
	}
}

// TestOverrideValidTightenRowLoadsAndComposes is the positive control the
// tighten-source and tighten-direction checks were both missing: a tighten
// row whose from is genuinely present in the base tags and whose
// replacement genuinely moves the bound the strict direction (gte=0 to
// gte=1) must load, and must compose by in-place substitution -- the
// type-mapping rules' own convention -- not by appending a second token
// alongside the one it replaces.
func TestOverrideValidTightenRowLoadsAndComposes(t *testing.T) {
	mappings := overrideMappings()
	mappings[OverrideTarget{Definition: "RecordFieldFixture", Property: "value"}] = OverrideMapping{BaseTags: []string{"gte=0"}, GoKind: "int", FieldName: "Value"}
	config, err := LoadOverrides(overrideFixture(t, "govern_22.yaml"), overrideIR(), mappings, nil)
	requireImplemented(t, "valid tighten row", err)
	if len(config.TagOverrides) != 1 {
		t.Fatalf("loaded %d override rows, want the single valid tighten row", len(config.TagOverrides))
	}
	composed, err := ApplyTagOverride([]string{"gte=0"}, config.TagOverrides[0])
	requireImplemented(t, "tighten composition", err)
	if got, want := strings.Join(composed, ","), "gte=1"; got != want {
		t.Fatalf("RecordFieldFixture.value tags = %q, want %q (in-place substitution, not an appended second token)", got, want)
	}
}

// TestOverrideValidTightenNarrowsCeilingPositiveControl is the
// tighten-direction check's other missing positive control: lte/max may
// only move DOWN, so a tighten that genuinely narrows a ceiling (max=20 to
// max=10) must load and compose -- proving the rule constrains direction,
// not merely "any value change", the way
// TestOverrideValidTightenRowLoadsAndComposes already proves it for a
// floor moving up.
func TestOverrideValidTightenNarrowsCeilingPositiveControl(t *testing.T) {
	config, err := LoadOverrides(overrideFixture(t, "tighten_narrows_ceiling.yaml"), overrideIR(), overrideMappings(), nil)
	requireImplemented(t, "valid ceiling-narrowing tighten row", err)
	if len(config.TagOverrides) != 1 {
		t.Fatalf("loaded %d override rows, want the single valid tighten row", len(config.TagOverrides))
	}
	composed, err := ApplyTagOverride([]string{"max=20"}, config.TagOverrides[0])
	requireImplemented(t, "ceiling tighten composition", err)
	if got, want := strings.Join(composed, ","), "max=10"; got != want {
		t.Fatalf("StringFixture.label tags = %q, want %q (in-place substitution, not an appended second token)", got, want)
	}
}

func TestOverrideConfigContainsExactlyThreeFiles(t *testing.T) {
	configDir := filepath.Join("config")
	entries, err := os.ReadDir(configDir)
	if os.IsNotExist(err) {
		t.Fatalf("generator config directory %s is missing; want exactly v201.yaml, overrides.yaml, and transform.yaml", configDir)
	}
	if err != nil {
		t.Fatalf("read generator config directory %s: %v", configDir, err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatalf("generator config directory %s contains a subdirectory %q; want exactly the three config files and no nested directory", configDir, entry.Name())
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	want := []string{"overrides.yaml", "transform.yaml", "v201.yaml"}
	if len(names) != len(want) {
		t.Fatalf("generator config contains %d files %v, want exactly %v", len(names), names, want)
	}
	for index := range want {
		if names[index] != want[index] {
			t.Fatalf("generator config files = %v, want exactly %v", names, want)
		}
	}
}

// TestCheckedInTransformConfigListsTheMandatedInitialisms checks the second
// of the three config files by its CONTENT, the way the test below checks
// overrides.yaml: the directory listing above proves transform.yaml exists,
// which an empty file with the right name also would. The naming transform
// reads exactly one input from it -- the initialism table -- and that table
// is fixed data, not a matter of local taste: it is the corpus-grounded set
// the identifier rules name (ID, URL, URI, EVSE, EV, SOC, OCSP, ISO, CSMS,
// AC, DC), and shipping it short by one entry silently regresses every
// identifier that entry covers (evseId renders EvseId instead of EVSEID) in
// a way no other test in this package would notice.
//
// Order is asserted, not just membership, because the algorithm reading this
// list resolves overlapping entries by longest match -- EVSE before EV is
// the case that decides -- so the file's own order is part of the data and
// a re-sorted list is a changed input, not a cosmetic edit.
//
// The parse goes through LoadTransformConfig rather than an ad-hoc decode
// here, so this test reads the file exactly as the generator does: a
// transform.yaml the real loader rejects can never pass by being readable
// through some second, more forgiving path.
func TestCheckedInTransformConfigListsTheMandatedInitialisms(t *testing.T) {
	path := filepath.Join("config", "transform.yaml")
	config, err := LoadTransformConfig(path)
	requireImplemented(t, "checked-in transform configuration", err)

	want := []string{"ID", "URL", "URI", "EVSE", "EV", "SOC", "OCSP", "ISO", "CSMS", "AC", "DC"}
	if len(config.Initialisms) != len(want) {
		t.Fatalf("checked-in %s lists %d initialisms %v, want exactly %v", path, len(config.Initialisms), config.Initialisms, want)
	}
	for index := range want {
		if config.Initialisms[index] != want[index] {
			t.Fatalf("checked-in %s initialisms = %v, want exactly %v in that order", path, config.Initialisms, want)
		}
	}
}

func TestCheckedInOverrideConfigHasTheGovernedIntervalRowAndOwnerHeader(t *testing.T) {
	path := filepath.Join("config", "overrides.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read checked-in override config %s: %v; want the owner header and the single interval row", path, err)
	}
	text := string(data)
	for _, want := range []string{"Owner:", "Every row must carry", "tagOverrides:", "definition: BootNotificationResponse", "property: interval", "tag: gte=0", "dedupAllowlist: []"} {
		if !strings.Contains(text, want) {
			t.Fatalf("checked-in override config is missing %q", want)
		}
	}
	// The override config's canonical shape (and every fixture in this
	// package) indents a tagOverrides list item by two spaces, not four --
	// anchoring on the newline plus that exact indentation is what lets
	// this assertion accept a real, canonically-shaped file instead of
	// rejecting it.
	if strings.Count(text, "\n  - definition:") != 1 {
		t.Fatalf("checked-in override config must carry exactly one tag-override row")
	}
}

// TestOverridesReportIsDeterministicAcrossTwoRuns loads the fixture config
// once -- LoadOverrides is the only path a path resolves through -- and
// renders that same loaded config twice, byte-comparing the result.
// RenderOverridesReport takes no path of its own (see its doc comment): the
// two-runs comparison is meaningful precisely because rendering is pure,
// with no file it could read differently between calls.
//
// "provisioning/boot_notification.go" and "Interval" are both genuinely
// derivable from this test's own inputs, not hardcoded expectations no
// implementation could ever satisfy: overrideIR's Messages entry names
// BootNotification's Block ("provisioning") and its own Name
// ("BootNotification"), which -- snake-cased by the same convention
// emitter_test.go's TestEmitterMessageFileIsNamedBySnakeCasedMessageName
// pins for the real emitter ("BootNotification" becomes
// "boot_notification.go") -- gives the file path; overrideMappings' own
// FieldName entry for BootNotificationResponse.interval gives "Interval"
// directly, the same way the emitter's FieldMapping.FieldName would. A correct
// RenderOverridesReport derives the path through Message.Block plus that
// one shared snake-case rule, not a naming mechanism of its own: there must
// be exactly one place that decides a message's file name, and the real
// emitter already owns it.
//
// lookup_both_namespaces.yaml carries two rows, not one -- the same fixture
// TestOverrideRootAndSharedDefinitionLookupComposesTags resolves against a
// message root (BootNotificationResponse.interval) and a $ref'd shared
// definition (IdTokenInfo.chargingPriority) -- so the assertions below check
// both rows are present in the rendered report, and both by the same
// mapping-derived field name (overrideMappings' FieldName entries, "Interval"
// and "ChargingPriority"). Checking only the first row would let a renderer
// that silently drops every row past the first satisfy this test.
//
// The report's job is to show, per row, what the override actually did and
// who is answerable for it, so BOTH rows are held to the same completeness
// bar -- the full per-row governance record, not the row's key alone --
// rather than the first row's record standing in for the second's:
//
//   - the rule the row applied ("add" for both rows here).
//   - the COMPOSED tag, not the row's own raw one -- "required,gte=0" for
//     the root-addressed row and "omitempty,gte=0" for the shared-definition
//     row, exactly the two strings
//     TestOverrideRootAndSharedDefinitionLookupComposesTags pins through
//     ApplyTagOverride. A report that echoes the raw "gte=0" it read from
//     the file tells a reader nothing about the tag the field ends up
//     carrying, and would leave "was the base set preserved" answerable
//     only by re-running the emitter.
//   - where it landed: the derived file path and the mapped field name.
//     Both rows land in the SAME file, which is derived rather than a
//     coincidence to be worked around: the placement rule places a $ref'd
//     definition by counting the DECLARED messages whose reach set contains it (>1
//     goes to types/, exactly 1 to that message's own file), and overrideIR
//     declares exactly one message, BootNotification, whose Reach set is
//     the only one naming IdTokenInfo. So the shared-definition row's field
//     is generated into provisioning/boot_notification.go too, and there is
//     no second, distinct path string available to assert it by.
//   - the row's governance record, by VALUE and not merely by label. The
//     author, date and cited source below are the fixture's own, and each
//     row additionally contributes a distinctive phrase from its own
//     rationale, so a renderer that prints the record's headings with the
//     values dropped -- or prints one row's record twice -- fails here.
//     This is what makes the report answer "why does this field carry this
//     tag" without opening the config file.
//
// The values BOTH rows carry -- the rule, the derived file path, the author,
// the date, and the leading clause of the two cited sources -- are therefore
// asserted by COUNT and not by presence: each has to appear at least once
// per row, so a renderer that prints one row's record and silently drops the
// other's fails on the count even though the string itself is somewhere in
// the output. Presence alone would let the first row's record answer for the
// second. The count is a floor, never an equality, so a report that also
// names one of these values in a heading is not failed for doing so.
//
// What a count cannot catch -- one row's record printed twice -- the
// row-distinctive strings do: each row's own field name, composed tag,
// rationale phrase and source clause belongs to exactly one row, and the
// source clauses are anchored on their shared "Part 2, " prefix so that a
// report rendering the row key as two space-separated columns
// ("IdTokenInfo chargingPriority") cannot supply one by accident.
//
// Every want is either a VALUE the fixture itself carries or a path derived
// from the IR the way the emitter derives it; none of them pins a label, a
// column heading or any other layout the spec does not mandate, so the
// report's shape stays the renderer's to choose.
func TestOverridesReportIsDeterministicAcrossTwoRuns(t *testing.T) {
	config, err := LoadOverrides(overrideFixture(t, "lookup_both_namespaces.yaml"), overrideIR(), overrideMappings(), nil)
	requireImplemented(t, "load overrides fixture for the report", err)

	first, err := RenderOverridesReport(config, overrideIR(), overrideMappings())
	requireImplemented(t, "first overrides report", err)
	second, err := RenderOverridesReport(config, overrideIR(), overrideMappings())
	requireImplemented(t, "second overrides report", err)
	if !bytes.Equal(first, second) {
		t.Fatalf("overrides report changed between identical runs:\nfirst %q\nsecond %q", first, second)
	}
	// The strings only one row can supply: its key, the field it landed on,
	// its composed tag, and the two record fields whose values differ
	// between the rows (a phrase from its rationale and the tail of its
	// cited source).
	for _, want := range []string{
		// The root-addressed row.
		"BootNotificationResponse.interval", "Interval",
		"required,gte=0", "negative heartbeat interval",
		"Part 2, BootNotification response",
		// The shared-definition row, checked to the same depth.
		"IdTokenInfo.chargingPriority", "ChargingPriority",
		"omitempty,gte=0", "negative charging priority",
		"Part 2, IdTokenInfo chargingPriority",
	} {
		if !bytes.Contains(first, []byte(want)) {
			t.Fatalf("overrides report does not name %q: %q", want, first)
		}
	}
	// The record fields whose values the two rows share, once per row.
	for _, shared := range []struct {
		want string
		why  string
	}{
		{"add", "the rule each row applied"},
		{"provisioning/boot_notification.go", "the generated file each row landed in -- the placement rule puts IdTokenInfo in the file of the single declared message reaching it, the same file the root row's field is emitted into"},
		{"Generator maintainer", "each row's author"},
		{"2026-08-08", "each row's date"},
		{"OCPP 2.0.1 Part 2", "the leading clause of each row's cited source"},
	} {
		if got := bytes.Count(first, []byte(shared.want)); got < 2 {
			t.Fatalf("overrides report names %q %d time(s), want at least once per row (%s): %q", shared.want, got, shared.why, first)
		}
	}
}

// TestOverridesReportReflectsDifferentRendererInputs is the negative control
// TestOverridesReportIsDeterministicAcrossTwoRuns can't be on its own: that
// test pins one fixed Message/mapping pair (provisioning/BootNotification,
// FieldName "Interval"), and a renderer that simply returns those same
// bytes for any input -- ignoring the ir and mappings arguments entirely --
// would satisfy it just as well as a correct one. This test swaps in a
// wholly different Message (diagnostics/GetLog, unrelated to
// BootNotification) and a different mapping's FieldName (LogType), built by
// hand rather than through LoadOverrides since RenderOverridesReport takes
// an already-loaded config, and requires the rendered bytes to name that
// Message's own derived file path and field -- a renderer hardcoded to the
// determinism test's fixture fails here because neither string it must
// produce ever appears in that fixture's inputs.
func TestOverridesReportReflectsDifferentRendererInputs(t *testing.T) {
	ir := IR{
		Definitions: []Definition{
			{Name: "GetLogRequest", Kind: DefinitionRoot, Files: []string{"GetLogRequest.json"}, Properties: []Property{
				{Name: "requestId", Type: "integer", Required: true},
			}},
			{Name: "GetLogResponse", Kind: DefinitionRoot, Files: []string{"GetLogResponse.json"}, Properties: []Property{
				{Name: "logType", Type: "string"},
			}},
		},
		Messages: []Message{
			{
				Name:         "GetLog",
				Block:        "diagnostics",
				Direction:    "CSMS->CS",
				Request:      "GetLogRequest.json",
				Response:     "GetLogResponse.json",
				RequestRoot:  "GetLogRequest",
				ResponseRoot: "GetLogResponse",
				Roots:        []string{"GetLogRequest", "GetLogResponse"},
				Emit:         true,
			},
		},
	}
	mappings := OverrideMappings{
		{Definition: "GetLogResponse", Property: "logType"}: {
			BaseTags: []string{}, GoKind: "string", FieldName: "LogType",
		},
	}
	config := OverrideConfig{
		Version: "v16",
		TagOverrides: []OverrideRow{
			{
				Version:    "v16",
				Definition: "GetLogResponse",
				Property:   "logType",
				Rule:       "add",
				Tag:        "max=50",
				Rationale:  "The diagnostics log type is bounded to the values GetLogEnumType defines, so the generated field also carries a defensive length ceiling.",
				Source:     "GetLogResponse.logType",
				Author:     "Generator maintainer",
				Date:       "2026-08-08",
			},
		},
	}

	report, err := RenderOverridesReport(config, ir, mappings)
	requireImplemented(t, "overrides report for a different Message/mapping pair", err)
	for _, want := range []string{"diagnostics/get_log.go", "LogType", "GetLogResponse.logType", "max=50"} {
		if !bytes.Contains(report, []byte(want)) {
			t.Fatalf("overrides report does not reflect its own inputs (%q missing): %q", want, report)
		}
	}
}

// TestOverridesReportCommandDispatchesThroughRenderer exercises
// -overrides-report the way TestDumpIRCommandDispatchesThroughMarshalIR
// exercises -dump-ir, through both of writeOverridesReport's seams: stub
// loadOverrides to succeed and renderOverridesReport to report a sentinel,
// and require run to surface exactly that sentinel -- only a command that
// reached the renderer through a successful load can produce it. With both
// real seams restored, LoadOverrides is itself still unimplemented, and the
// honest load-first precedence (see writeOverridesReport's own doc comment)
// means the command fails at the load stage before RenderOverridesReport is
// ever reached for real; the error chain is checked against LoadOverrides'
// own "load overrides" prefix for that reason, not RenderOverridesReport's.
//
// The middle case pins the precedence itself rather than inferring it from
// today's unimplemented loader: with the load seam failing and the renderer
// seam recording whether it ran, the command must surface the load failure
// AND leave the renderer untouched. That argument survives implementation,
// because it exercises the seams and not the state of what sits behind
// them: a command that rendered first and reported the load error second
// would still fail here the day LoadOverrides starts succeeding, and so
// would one that rendered a zero-value config after a failed load --
// which, for a report whose whole purpose is to show what the checked-in
// table did, would print an empty listing that reads exactly like a table
// with no rows in it.
func TestOverridesReportCommandDispatchesThroughRenderer(t *testing.T) {
	realLoadOverrides, realRenderer := loadOverrides, renderOverridesReport
	t.Cleanup(func() {
		loadOverrides = realLoadOverrides
		renderOverridesReport = realRenderer
	})

	reached := errors.New("overrides report renderer reached")
	loadOverrides = func(string, IR, OverrideMappings, []string) (OverrideConfig, error) { return OverrideConfig{}, nil }
	renderOverridesReport = func(OverrideConfig, IR, OverrideMappings) ([]byte, error) { return nil, reached }
	if err := run([]string{"-overrides-report", filepath.Join(t.TempDir(), "report.txt")}, io.Discard); !errors.Is(err, reached) {
		t.Fatalf("-overrides-report did not dispatch through its renderer seam: %+v", err)
	}

	loadRejected := errors.New("checked-in override table rejected")
	rendererRan := false
	loadOverrides = func(string, IR, OverrideMappings, []string) (OverrideConfig, error) {
		return OverrideConfig{}, loadRejected
	}
	renderOverridesReport = func(OverrideConfig, IR, OverrideMappings) ([]byte, error) {
		rendererRan = true
		return nil, nil
	}
	if err := run([]string{"-overrides-report", filepath.Join(t.TempDir(), "report.txt")}, io.Discard); !errors.Is(err, loadRejected) {
		t.Fatalf("-overrides-report did not surface its load failure: %+v", err)
	}
	if rendererRan {
		t.Fatalf("-overrides-report rendered a report from a config that failed to load")
	}

	loadOverrides = realLoadOverrides
	renderOverridesReport = realRenderer

	err := run([]string{"-overrides-report", filepath.Join(t.TempDir(), "report.txt")}, io.Discard)
	if err != nil && !strings.Contains(err.Error(), "load overrides") {
		t.Fatalf("-overrides-report did not dispatch through LoadOverrides: %+v", err)
	}
	requireImplemented(t, "overrides report command", err)
}

// chdir switches the process's working directory to dir for the duration of
// t, restoring the directory t started in once t finishes. The stdlib grew
// testing.T.Chdir for exactly this, but this module builds against go1.21
// (see go.mod) and vet rejects testing.T.Chdir below that version, so the
// two cwd-dependent tests below manage the switch by hand instead.
func chdir(t *testing.T, dir string) {
	t.Helper()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory before chdir: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir to %s: %v", dir, err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previous); err != nil {
			t.Fatalf("restore working directory to %s: %v", previous, err)
		}
	})
}

// TestOverridesReportResolvesConfigAgainstTheModuleRootNotTheWorkingDirectory
// proves -overrides-report reads the real checked-in config -- and produces
// the identical report -- regardless of which directory inside the module
// it is invoked from. Before this test could pass, the command joined a
// bare "config/..." onto the process's own working directory, which only
// ever happened to resolve correctly when that directory was
// internal/codegen itself (exactly where `go test` leaves it); the same
// invocation from the repository root, or from any other package
// directory, either failed to find the file at all or, from a directory
// that happened to contain its own "config" subdirectory, could have
// silently resolved a different one.
func TestOverridesReportResolvesConfigAgainstTheModuleRootNotTheWorkingDirectory(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	repoRoot := filepath.Clean(filepath.Join(cwd, "..", ".."))
	if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err != nil {
		t.Fatalf("could not locate the repository root above %s: %v", cwd, err)
	}

	cases := []struct {
		name string
		dir  string
	}{
		{"the repository root", repoRoot},
		{"a sibling package directory", filepath.Join(repoRoot, "ocpp2.0.1")},
		{"the codegen package directory itself", cwd},
	}

	var reports [][]byte
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chdir(t, tc.dir)
			reportPath := filepath.Join(t.TempDir(), "report.txt")
			if err := run([]string{"-overrides-report", reportPath}, io.Discard); err != nil {
				t.Fatalf("-overrides-report failed when invoked from %s: %+v", tc.dir, err)
			}
			data, err := os.ReadFile(reportPath)
			if err != nil {
				t.Fatalf("read report written from %s: %v", tc.dir, err)
			}
			reports = append(reports, data)
		})
	}

	for i := 1; i < len(reports); i++ {
		if !bytes.Equal(reports[0], reports[i]) {
			t.Fatalf("-overrides-report produced different bytes depending on the working directory it ran from (case 0 vs case %d)", i)
		}
	}
}

// TestOverridesReportFailsCleanlyOutsideAModuleCheckout requires that
// resolving the checked-in config against a working directory with no
// go.mod anywhere above it fails with a message naming the reason, rather
// than the bare file-not-found error a cwd-relative join would have
// produced against some directory that happens not to exist under an
// unrelated working directory.
func TestOverridesReportFailsCleanlyOutsideAModuleCheckout(t *testing.T) {
	chdir(t, t.TempDir())
	err := run([]string{"-overrides-report", filepath.Join(t.TempDir(), "report.txt")}, io.Discard)
	requireExpectedHardFailureContains(t, "-overrides-report outside a module checkout", err, "go.mod")
}

// TestOverrideTagIsExactlyOneWellFormedValidatorToken holds the tag grammar
// to one token with a value of the shape its family takes, which is the
// whole of what stands between the override table and generated code.
//
// The validator splits a struct tag on "," into tokens and on "|" into
// alternatives, and reads everything after a token's first "=" as that
// token's parameter, so a row whose tag was accepted as one opaque
// "family=<any non-blank text>" string could write a second validator into a
// field's tag that its governance record never mentions -- `gte=0,dive`
// loads a dive onto an int, and the validator panics on it the first time
// that message is validated, with nothing failing at build time to warn
// anybody. The parameter's own shape matters for the same reason: the
// validator parses a bound with a base-0 whole-number parse for every
// integer field and panics when the text does not fit it, so `gte=abc`,
// `gte=1e5`, `lte=Inf` and a trailing space are each a panic waiting for a
// payload, and `max=010` is a silently different bound (8, read as octal).
// "0x2C" is the validator's own escape for a comma inside a parameter, so it
// is a comma smuggled past a naive comma check.
//
// Each case names the token it rejects, so a loader that hard-failed
// everything (the other way to pass a table of rejections) is caught by the
// positive controls the rest of this file already carries: the checked-in
// gte=0 row, dive on a slice, max= on a slice, and a bare registered
// validator name all still have to load.
func TestOverrideTagIsExactlyOneWellFormedValidatorToken(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		file                 string
		wants                []string
		registeredValidators []string
	}{
		{
			name:  "a second token smuggled behind a comma",
			file:  "tag_grammar_token_list.yaml",
			wants: []string{"RecordFieldFixture.value", "gte=0,dive", "single validator token"},
		},
		{
			name:  "an alternative smuggled behind a pipe",
			file:  "tag_grammar_or_alternative.yaml",
			wants: []string{"RecordFieldFixture.value", "gte=0|lte=5", "single validator token"},
		},
		{
			name:  "a trailing space inside the token",
			file:  "tag_grammar_trailing_space.yaml",
			wants: []string{"RecordFieldFixture.value", "gte=0 ", "whitespace"},
		},
		{
			name:  "a second equals sign",
			file:  "tag_grammar_second_equals.yaml",
			wants: []string{"RecordFieldFixture.value", "gte=0=1", `more than one "="`},
		},
		{
			name:  "an empty value",
			file:  "tag_grammar_missing_value.yaml",
			wants: []string{"RecordFieldFixture.value", "gte=", "decimal number"},
		},
		{
			name:  "a bound family carrying no value at all",
			file:  "tag_grammar_bare_family.yaml",
			wants: []string{"RecordFieldFixture.value", "gte", "carries no value"},
		},
		{
			name:  "a non-numeric bound value",
			file:  "tag_grammar_non_numeric_value.yaml",
			wants: []string{"RecordFieldFixture.value", "gte=abc", "decimal number"},
		},
		{
			name:  "a bound value in exponent notation",
			file:  "tag_grammar_exponent_value.yaml",
			wants: []string{"RecordFieldFixture.value", "gte=1e5", "decimal number"},
		},
		{
			name:  "a non-finite bound value",
			file:  "tag_grammar_infinite_value.yaml",
			wants: []string{"RecordFieldFixture.value", "lte=Inf", "decimal number"},
		},
		{
			name:  "a bound value written in non-ASCII digits",
			file:  "tag_grammar_unicode_digit_value.yaml",
			wants: []string{"RecordFieldFixture.value", "decimal number"},
		},
		{
			name:  "a comma escaped as the validator's own hex text",
			file:  "tag_grammar_comma_escape_value.yaml",
			wants: []string{"RecordFieldFixture.value", "max=0x2C", "whole number"},
		},
		{
			name:  "a cardinality value with a leading zero",
			file:  "tag_grammar_leading_zero_value.yaml",
			wants: []string{"RecordFieldFixture.value", "max=010", "whole number"},
		},
		{
			name:  "a fractional cardinality value",
			file:  "tag_grammar_fractional_cardinality.yaml",
			wants: []string{"RecordFieldFixture.value", "max=1.5", "whole number"},
		},
		{
			name:  "a negative cardinality value",
			file:  "tag_grammar_negative_cardinality.yaml",
			wants: []string{"RecordFieldFixture.value", "min=-1", "whole number"},
		},
		{
			name:  "a value on a family that takes none",
			file:  "tag_grammar_dive_with_value.yaml",
			wants: []string{"RecordFieldFixture.value", "dive=1", "dive takes none"},
		},
		{
			// A registered validator name is a bare token. Recognising one by
			// its family -- the text before "=" -- rather than by the whole
			// token would let any registered name carry an arbitrary tail into
			// the generated tag.
			name:                 "a parameter appended to a registered validator name",
			file:                 "tag_grammar_registered_with_parameter.yaml",
			wants:                []string{"RecordFieldFixture.value", "fixtureRegisteredValidator=0", "not a recognized validator token"},
			registeredValidators: []string{"fixtureRegisteredValidator"},
		},
		{
			// from is held to the same grammar as tag: it is written into no
			// generated tag itself, but a from naming two tokens describes a
			// base token that cannot exist, and the row would be judged
			// against a fiction.
			name:  "a second token smuggled into from",
			file:  "tag_grammar_from_token_list.yaml",
			wants: []string{"RecordFieldFixture.value", "from", "gte=0,dive", "single validator token"},
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadOverrides(overrideFixture(t, tc.file), overrideIR(), nil, tc.registeredValidators)
			requireExpectedHardFailureContains(t, "tag grammar: "+tc.name, err, tc.wants...)
		})
	}
}

// TestOverrideBoundValueMustFitTheMappedGoKind covers what the family-versus-
// kind rule leaves open once a bound family is on a numeric field at all: the
// validator reads a bound parameter with the parse its field's kind calls for
// -- a whole-number parse for an integer field, an unsigned one for an
// unsigned field, a float parse only for a float field -- and panics when the
// text does not fit. So gte=0.5 on an int, and gte=-1 on a uint, are both
// grammatical and both land on a numeric field, and both take down the first
// validation of the message carrying them.
//
// The same fixture is loaded twice against two different mapped kinds, and
// must be rejected against one and accepted against the other. That is what
// distinguishes a kind-driven check from one that simply refuses negative
// bounds: gte=-1 is a legitimate floor on a signed field.
func TestOverrideBoundValueMustFitTheMappedGoKind(t *testing.T) {
	t.Run("a fractional bound on a whole-number field", func(t *testing.T) {
		_, err := LoadOverrides(overrideFixture(t, "bound_fraction_on_integer_field.yaml"), overrideIR(), overrideMappings(), nil)
		requireExpectedHardFailureContains(t, "fractional bound on an int field", err,
			"BootNotificationResponse.interval", "gte=0.5", "int", "whole number")
	})

	t.Run("a negative bound on an unsigned field", func(t *testing.T) {
		mappings := overrideMappings()
		mappings[OverrideTarget{Definition: "RecordFieldFixture", Property: "value"}] = OverrideMapping{
			BaseTags: []string{}, GoKind: "uint", FieldName: "Value",
		}
		_, err := LoadOverrides(overrideFixture(t, "bound_negative_on_unsigned_field.yaml"), overrideIR(), mappings, nil)
		requireExpectedHardFailureContains(t, "negative bound on a uint field", err,
			"RecordFieldFixture.value", "gte=-1", "uint", "negative")
	})

	t.Run("the same negative bound on a signed field loads", func(t *testing.T) {
		config, err := LoadOverrides(overrideFixture(t, "bound_negative_on_unsigned_field.yaml"), overrideIR(), overrideMappings(), nil)
		requireImplemented(t, "negative bound on an int field (positive control)", err)
		if len(config.TagOverrides) != 1 {
			t.Fatalf("loaded %d override rows, want the single negative-floor row", len(config.TagOverrides))
		}
	})
}

// TestOverrideTightenReplacesWithinOneValidatorFamily pins the half of the
// monotonicity rule that direction alone cannot state: which constraint the
// replacement is a tightening OF.
//
// A tighten substitutes its tag for the base token from names, so the two
// have to constrain the same thing. Judging direction against from's family
// while leaving the replacement's own family unchecked reads "lte=100
// replaced by gte=50" as a tightening -- 50 really is below 100 -- and the
// composed tag then carries a floor where the schema's ceiling used to be:
// every value the ceiling excluded validates, which is the exact opposite of
// what a row recorded as a tightening claims to do. Both families apply to a
// numeric field, so the kind rule does not catch it either.
//
// The loosening and tightening cases below run against the same rule so that
// the family check cannot be satisfied by a loader that has simply stopped
// checking direction, and the bare-family case covers the one shape a value
// comparison says nothing about: replacing a token with itself.
func TestOverrideTightenReplacesWithinOneValidatorFamily(t *testing.T) {
	t.Run("a replacement from another family is rejected", func(t *testing.T) {
		mappings := overrideMappings()
		mappings[OverrideTarget{Definition: "RecordFieldFixture", Property: "value"}] = OverrideMapping{
			BaseTags: []string{"omitempty", "lte=100"}, GoKind: "int", FieldName: "Value",
		}
		_, err := LoadOverrides(overrideFixture(t, "tighten_crosses_validator_families.yaml"), overrideIR(), mappings, nil)
		requireExpectedHardFailureContains(t, "cross-family tighten", err,
			"RecordFieldFixture.value", "lte=100", "gte=50", "families")
	})

	t.Run("a bare token replaced by itself is rejected", func(t *testing.T) {
		_, err := LoadOverrides(overrideFixture(t, "tighten_same_bare_token.yaml"), overrideIR(), sliceItemsDiveMapping(), nil)
		requireExpectedHardFailureContains(t, "no-op tighten of a bare token", err,
			"SliceFixture.items", "dive", "unchanged")
	})

	t.Run("a same-family replacement that loosens is rejected", func(t *testing.T) {
		_, err := LoadOverrides(overrideFixture(t, "tighten_widens_ceiling.yaml"), overrideIR(), overrideMappings(), nil)
		requireExpectedHardFailureContains(t, "same-family loosening tighten", err,
			"StringFixture.label", "max=20", "max=30", "ceiling")
	})

	t.Run("a same-family replacement that tightens loads", func(t *testing.T) {
		config, err := LoadOverrides(overrideFixture(t, "tighten_narrows_ceiling.yaml"), overrideIR(), overrideMappings(), nil)
		requireImplemented(t, "same-family tightening row (positive control)", err)
		if len(config.TagOverrides) != 1 {
			t.Fatalf("loaded %d override rows, want the single tightening row", len(config.TagOverrides))
		}
	})
}

// reportTagValues returns the value of every tag line in a rendered report,
// in the order the report lists them. It anchors on the line's own "tag:"
// prefix at any indentation rather than on a fixed layout, and reads the
// whole value rather than searching for a substring, because a composed tag
// contains every shorter composition of itself: "required,gte=0" is a
// substring of "required,gte=0,lte=86400", so a containment check could not
// tell the tag a field really carries from a hypothetical one.
func reportTagValues(report []byte) []string {
	var values []string
	for _, line := range strings.Split(string(report), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "tag:") {
			values = append(values, strings.TrimSpace(strings.TrimPrefix(trimmed, "tag:")))
		}
	}
	return values
}

// TestOverridesReportTagIsTheTagTheEmitterEmits closes the one gap between
// the report and the generated tree that a reader of the report cannot
// detect: whether the tag the report attributes to a field is the tag that
// field actually carries.
//
// Two rows on one property is where the two can part company. The emitter
// composes both onto the base tag set in the order the loaded row slice
// lists them, so the field carries required,gte=0,lte=86400; a report that
// renders each row against the base tag set on its own instead reports
// required,gte=0 and required,lte=86400 -- two tags nothing in the tree
// carries, and never the one it does. Since the whole purpose of the report
// is to answer "why does this field carry this tag" without reading the
// generated file, a divergence there is invisible to exactly the reader it
// was written for.
//
// The comparison is made against the real mapping, not a hand-written one:
// the base tag set comes from the same MapProperty the emitter renders
// through, and the expected string is taken from that emitter's own rendered
// validate tag. The literal below is pinned as well, so both sides producing
// the same wrong answer (an emitter applying no row at all would agree with a
// report rendering none) still fails.
func TestOverridesReportTagIsTheTagTheEmitterEmits(t *testing.T) {
	ir := overrideIR()
	transform, err := LoadTransformConfig(fixturePath("config", "transform.yaml"))
	requireImplemented(t, "loading the initialism table", err)
	mappings, registeredValidators, err := buildOverrideMappings(ir, transform)
	requireImplemented(t, "building the mapping the emitter itself produces", err)

	config, err := LoadOverrides(overrideFixture(t, "report_multi_row_target.yaml"), ir, mappings, registeredValidators)
	requireImplemented(t, "loading the two-row override fixture", err)
	if len(config.TagOverrides) != 2 {
		t.Fatalf("loaded %d override rows, want both rows targeting BootNotificationResponse.interval", len(config.TagOverrides))
	}

	definition := findDefinition(ir.Definitions, "BootNotificationResponse")
	if definition == nil {
		t.Fatalf("fixture IR declares no BootNotificationResponse definition")
	}
	property := findProperty(definition.Properties, "interval")
	if property == nil {
		t.Fatalf("fixture IR declares no interval property on BootNotificationResponse")
	}
	emitted, err := MapProperty(*definition, *property, ir.Definitions, transform, config.TagOverrides)
	requireImplemented(t, "mapping the overridden property through the emitter", err)
	emittedTag := strings.Join(splitValidateTag(emitted.ValidateTag), ",")
	if want := "required,gte=0,lte=86400"; emittedTag != want {
		t.Fatalf("the emitter composed %q from the two loaded rows, want %q", emittedTag, want)
	}

	report, err := RenderOverridesReport(config, ir, mappings)
	requireImplemented(t, "rendering the report for a property carrying two rows", err)
	reported := reportTagValues(report)
	if len(reported) != len(config.TagOverrides) {
		t.Fatalf("report lists %d tags for %d loaded rows: %q", len(reported), len(config.TagOverrides), report)
	}
	for index, tag := range reported {
		if tag != emittedTag {
			t.Fatalf("report row %d claims tag %q, but the emitter writes %q onto that field: %q", index, tag, emittedTag, report)
		}
	}
}

// TestOverrideAddMayNotDuplicateAFamilyAnEarlierRowAdded holds the
// one-row-per-validator-family rule across the rows of one property, not just
// against the tag set the schema produced.
//
// A property carrying two add rows of one family composes to a field holding
// both tokens -- gte=0,gte=5 -- so the constraint that field really enforces
// is stated in neither row's record, and the report now shows that
// composition to a reader who cannot tell which of the two rows to believe.
// Checking each row against the base tag set alone cannot see it: neither row
// duplicates anything the schema produced, only what the other row added.
//
// The control below is the property carrying two rows of DIFFERENT families,
// which stays legal: a floor and a ceiling are two constraints, each with its
// own record, and composing both is what a table of corrections is for. A
// check that simply refused a property its second row would pass the
// rejection above and break that.
func TestOverrideAddMayNotDuplicateAFamilyAnEarlierRowAdded(t *testing.T) {
	t.Run("a second row of the same family is rejected", func(t *testing.T) {
		_, err := LoadOverrides(overrideFixture(t, "duplicate_family_across_rows.yaml"), overrideIR(), overrideMappings(), nil)
		requireExpectedHardFailureContains(t, "two add rows of one validator family", err,
			"BootNotificationResponse.interval", "gte=5", "gte=0", "one row per validator family")
	})

	t.Run("two rows of different families still load", func(t *testing.T) {
		config, err := LoadOverrides(overrideFixture(t, "report_multi_row_target.yaml"), overrideIR(), overrideMappings(), nil)
		requireImplemented(t, "a floor and a ceiling on one property (positive control)", err)
		if len(config.TagOverrides) != 2 {
			t.Fatalf("loaded %d override rows, want both the floor and the ceiling row", len(config.TagOverrides))
		}
	})
}

// TestOverrideTightenMayNotNameATokenAnotherRowAdded pins what from is
// allowed to name, which the rule above raises: a tighten's from names the
// exact BASE tag it replaces, and that is what the source-token check reads
// it against, so a from naming a token an earlier row introduced is rejected
// even though that token really is in the composed tag by the time this row
// applies.
//
// The rule the loader enforces and the composition the emitter would perform
// deliberately part company here: substituting one token for another needs
// nothing but the token's presence, so the emitter would compose this pair
// happily, and the loader is the only thing standing between the pair and
// generated code. It is worth standing there. from exists to fail loudly when
// the base mapping stops producing the token a row was written against, and a
// from pointing at another row instead makes that tripwire monitor this
// table's own output rather than the schema's. It would also make the two
// rows one constraint spread over two records, ordered by their own tags,
// when the same result -- a floor of five -- is one add row with one record.
func TestOverrideTightenMayNotNameATokenAnotherRowAdded(t *testing.T) {
	_, err := LoadOverrides(overrideFixture(t, "tighten_from_row_added_token.yaml"), overrideIR(), overrideMappings(), nil)
	requireExpectedHardFailureContains(t, "tighten naming a token another row added", err,
		"BootNotificationResponse.interval", "gte=0", "base tag set")
}

// sliceCardinalityAndElementMapping gives SliceFixture.items the base tag set
// a list that bounds both itself and its entries produces: minItems 1 and
// maxItems 5 render in front of dive as the item count, and the entries' own
// maxLength 36 renders after it as each entry's length. Two max= tokens, one
// on each side of dive, are two different constraints -- which is the shape a
// family-blind ownership rule cannot tell apart.
func sliceCardinalityAndElementMapping() OverrideMappings {
	mappings := overrideMappings()
	mappings[OverrideTarget{Definition: "SliceFixture", Property: "items"}] = OverrideMapping{
		BaseTags: []string{"required", "min=1", "max=5", "dive", "max=36"}, GoKind: "[]string", FieldName: "Items",
	}
	return mappings
}

// sliceRepeatedTokenMapping is the same shape with one token spelled
// identically on both sides of dive: a list of at least three entries, each
// at least three characters. Nothing in a row can say which of the two a
// from: min=3 means.
func sliceRepeatedTokenMapping() OverrideMappings {
	mappings := overrideMappings()
	mappings[OverrideTarget{Definition: "SliceFixture", Property: "items"}] = OverrideMapping{
		BaseTags: []string{"required", "min=3", "dive", "min=3"}, GoKind: "[]string", FieldName: "Items",
	}
	return mappings
}

// valueFloorMapping gives RecordFieldFixture.value a floor to tighten, the
// mapping a schema declaring minimum 0 on that integer property produces.
func valueFloorMapping() OverrideMappings {
	mappings := overrideMappings()
	mappings[OverrideTarget{Definition: "RecordFieldFixture", Property: "value"}] = OverrideMapping{
		BaseTags: []string{"gte=0"}, GoKind: "int", FieldName: "Value",
	}
	return mappings
}

// TestOverrideBoundValueMustBeWithinTheValidatorsRange covers the half of a
// bound's well-formedness that its spelling does not settle: a value of
// exactly the right shape still panics the validator if it is outside the
// range the parse reads it into. Bounds are read with a whole-number parse
// for a string's length, a slice's item count and an integer field, an
// unsigned parse for an unsigned field, and a float parse for a float one, so
// the ceiling is the parse's, not the field's: one past the whole-number
// limit is a panic on the first validation of that message, and a decimal too
// large to represent is a panic before that.
//
// The limit itself must load, or the check is a range check that has moved
// the range.
func TestOverrideBoundValueMustBeWithinTheValidatorsRange(t *testing.T) {
	t.Run("the whole-number limit itself loads", func(t *testing.T) {
		config, err := LoadOverrides(overrideFixture(t, "bound_int64_max.yaml"), overrideIR(), overrideMappings(), nil)
		requireImplemented(t, "a bound at the whole-number limit (positive control)", err)
		if len(config.TagOverrides) != 1 {
			t.Fatalf("loaded %d override rows, want the single boundary row", len(config.TagOverrides))
		}
	})

	for _, tc := range []struct {
		name     string
		file     string
		mappings OverrideMappings
		wants    []string
	}{
		{
			name:     "one past the whole-number limit",
			file:     "bound_int64_overflow.yaml",
			mappings: overrideMappings(),
			wants:    []string{"BootNotificationResponse.interval", "9223372036854775808", "9223372036854775807"},
		},
		{
			name:     "one past the negative whole-number limit",
			file:     "bound_int64_negative_overflow.yaml",
			mappings: overrideMappings(),
			wants:    []string{"BootNotificationResponse.interval", "-9223372036854775809", "-9223372036854775808"},
		},
		{
			// Rejected by the grammar, before any Go kind is consulted: no
			// numeric field of any kind can hold it.
			name:  "a decimal too large to represent",
			file:  "bound_decimal_overflow.yaml",
			wants: []string{"BootNotificationResponse.interval", "outside the range"},
		},
		{
			// A length or item count is always read with the whole-number
			// parse, whichever field kind carries it, so its range is fixed
			// without a mapping too.
			name:  "an item count past the whole-number limit",
			file:  "cardinality_overflow.yaml",
			wants: []string{"StringFixture.label", "99999999999999999999", "outside the range"},
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadOverrides(overrideFixture(t, tc.file), overrideIR(), tc.mappings, nil)
			requireExpectedHardFailureContains(t, "out-of-range bound: "+tc.name, err, tc.wants...)
		})
	}
}

// TestOverrideFamilyOwnershipIsScopedByDive pins the namespace a validator
// family belongs to. dive splits a slice's tag in two: in front of it a
// min=/max= bounds the list itself, after it the same spelling bounds each
// value in the list. They are different constraints, so one row replacing
// each is two rows of one family that do not collide -- and a rule that keyed
// ownership by family alone would reject the second of them, blocking a pair
// the mapping itself produces the base tags for.
//
// The other two cases are the ones the row format cannot express. An added
// token joins the end of the tag, which is the element half once the base set
// dives, while a min=/max= row on a slice property names the list's own item
// count -- so on a diving list the row means one thing and would do another,
// and there is no field in the row to say which was intended. Naming a from
// token that appears on both sides of dive is the same gap from the other
// direction. Both are refused rather than resolved by whichever side the
// composition rule happens to reach.
func TestOverrideFamilyOwnershipIsScopedByDive(t *testing.T) {
	t.Run("one row per dive scope loads and composes in place", func(t *testing.T) {
		mappings := sliceCardinalityAndElementMapping()
		config, err := LoadOverrides(overrideFixture(t, "tighten_both_dive_scopes.yaml"), overrideIR(), mappings, nil)
		requireImplemented(t, "a cardinality tighten and an element tighten on one property", err)
		if len(config.TagOverrides) != 2 {
			t.Fatalf("loaded %d override rows, want both the cardinality and the element row", len(config.TagOverrides))
		}
		tokens := mappings[OverrideTarget{Definition: "SliceFixture", Property: "items"}].BaseTags
		for _, row := range config.TagOverrides {
			tokens, err = ApplyTagOverride(tokens, row)
			requireImplemented(t, "composing "+row.Tag, err)
		}
		if got, want := strings.Join(tokens, ","), "required,min=1,max=3,dive,max=20"; got != want {
			t.Fatalf("composed tag = %q, want %q (each row replacing the bound on its own side of dive)", got, want)
		}
	})

	t.Run("an item-count add over a diving list is refused", func(t *testing.T) {
		_, err := LoadOverrides(overrideFixture(t, "add_cardinality_over_element_base.yaml"), overrideIR(), sliceCardinalityAndElementMapping(), nil)
		requireExpectedHardFailureContains(t, "an item-count add that would land after dive", err,
			"SliceFixture.items", "min=1", "dive", "cannot say which")
	})

	t.Run("a from token appearing on both sides of dive is refused", func(t *testing.T) {
		_, err := LoadOverrides(overrideFixture(t, "tighten_ambiguous_dive_token.yaml"), overrideIR(), sliceRepeatedTokenMapping(), nil)
		requireExpectedHardFailureContains(t, "a from token on both sides of dive", err,
			"SliceFixture.items", "min=3", "both sides of dive")
	})
}

// TestOverrideTightenTakesOwnershipOfTheFamilyItReplaces closes the gap
// between what the loader accepts and what the emitter can compose. Two
// tighten rows naming one base token both satisfy the source check --
// the token really is in the base tag set, and it is there for both of them --
// but composition substitutes it away on the first row, so the second finds
// nothing to replace and generation fails on a table governance had already
// accepted. Making tighten take ownership of the family it replaced is what
// keeps the two verdicts the same.
//
// The control is a tighten and an add of a different family on one property,
// which stays legal: taking ownership of one family must not close the
// property to every other row.
func TestOverrideTightenTakesOwnershipOfTheFamilyItReplaces(t *testing.T) {
	t.Run("a second tighten of one base token is rejected", func(t *testing.T) {
		_, err := LoadOverrides(overrideFixture(t, "tighten_twice_same_base_token.yaml"), overrideIR(), valueFloorMapping(), nil)
		requireExpectedHardFailureContains(t, "two tighten rows naming one base token", err,
			"RecordFieldFixture.value", "gte=0", "gte=2", "one row per validator family")
	})

	t.Run("a tighten and an add of another family still load", func(t *testing.T) {
		mappings := valueFloorMapping()
		config, err := LoadOverrides(overrideFixture(t, "tighten_and_other_family_add.yaml"), overrideIR(), mappings, nil)
		requireImplemented(t, "a tighten and an unrelated add on one property (positive control)", err)
		if len(config.TagOverrides) != 2 {
			t.Fatalf("loaded %d override rows, want both the tighten and the add", len(config.TagOverrides))
		}
		tokens := mappings[OverrideTarget{Definition: "RecordFieldFixture", Property: "value"}].BaseTags
		for _, row := range config.TagOverrides {
			tokens, err = ApplyTagOverride(tokens, row)
			requireImplemented(t, "composing "+row.Tag, err)
		}
		if got, want := strings.Join(tokens, ","), "gte=1,lte=100"; got != want {
			t.Fatalf("composed tag = %q, want %q", got, want)
		}
	})
}

// fixtureEnumValidators is the registry the enum-token cases run against: two
// names of the shape validatorTagName derives from an enum definition, which
// is the only shape this generator ever registers.
var fixtureEnumValidators = []string{"chargingLimitSourceType201", "registrationStatusType201"}

// enumTypedLabelMapping gives StringFixture.label the mapping an enum-typed
// property produces: the enum's own generated named type, and the enum's own
// token already appended to the base tag set by the mapping rules.
func enumTypedLabelMapping() OverrideMappings {
	mappings := overrideMappings()
	mappings[OverrideTarget{Definition: "StringFixture", Property: "label"}] = OverrideMapping{
		BaseTags: []string{"required", "registrationStatusType201"}, GoKind: "RegistrationStatusType", FieldName: "Label",
	}
	return mappings
}

// stringSliceElementMapping and intSliceElementMapping are a minimal pair: two
// lists whose elements differ only in kind, both diving so that an added token
// lands on the elements. What a registered token may load onto is decided by
// the element, not by the slice, and only these two together prove it -- the
// first alone is satisfied by an implementation that waves every slice
// through.
func stringSliceElementMapping() OverrideMappings {
	mappings := overrideMappings()
	mappings[OverrideTarget{Definition: "SliceFixture", Property: "items"}] = OverrideMapping{
		BaseTags: []string{"required", "dive", "max=36"}, GoKind: "[]string", FieldName: "Items",
	}
	return mappings
}

func intSliceElementMapping() OverrideMappings {
	mappings := overrideMappings()
	mappings[OverrideTarget{Definition: "SliceFixture", Property: "items"}] = OverrideMapping{
		BaseTags: []string{"required", "dive", "gte=0"}, GoKind: "[]int", FieldName: "Items",
	}
	return mappings
}

// TestOverrideRegisteredTokenOnlyLoadsOntoAFieldItCanRead covers the branch of
// the tag grammar that the kind rules never reached: a registered validator
// name passes the grammar on the strength of being registered, and nothing
// then asked whether the field it names can be read by it.
//
// The generated check is a string switch -- it converts fl.Field().String()
// to the enum's type and matches it against that enum's constants -- and
// reflect's String is the one getter that does not panic on a mismatched
// kind: it returns "<int Value>" for an int. So the token on a numeric field
// produces no crash and no build failure. It silently matches nothing, and
// every legitimate value that field ever carries is rejected, which is a
// quieter failure than the panic the bound families produce and no easier to
// find.
//
// The legal targets follow from that: the string-kinded fields. In this
// generator's mapping those are the "string" spelling itself and a generated
// enum's own named type (declared as `type X string`), the latter recognised
// by the enum token the mapping already appended to its base tag set rather
// than by its Go name, which is indistinguishable from any other named type.
// A slice of either is reached through dive, where the element is what gets
// read.
func TestOverrideRegisteredTokenOnlyLoadsOntoAFieldItCanRead(t *testing.T) {
	t.Run("a numeric field is refused", func(t *testing.T) {
		_, err := LoadOverrides(overrideFixture(t, "enum_token_on_numeric_field.yaml"), overrideIR(), overrideMappings(), fixtureEnumValidators)
		requireExpectedHardFailureContains(t, "a registered token on an int field", err,
			"BootNotificationResponse.interval", "registrationStatusType201", "int", "string")
	})

	t.Run("a string field loads", func(t *testing.T) {
		config, err := LoadOverrides(overrideFixture(t, "enum_token_on_string_field.yaml"), overrideIR(), overrideMappings(), fixtureEnumValidators)
		requireImplemented(t, "a registered token on a string field (positive control)", err)
		if len(config.TagOverrides) != 1 {
			t.Fatalf("loaded %d override rows, want the single enumerated-value row", len(config.TagOverrides))
		}
	})

	t.Run("the elements of a string list load", func(t *testing.T) {
		config, err := LoadOverrides(overrideFixture(t, "enum_token_on_string_slice_elements.yaml"), overrideIR(), stringSliceElementMapping(), fixtureEnumValidators)
		requireImplemented(t, "a registered token on string elements (positive control)", err)
		if len(config.TagOverrides) != 1 {
			t.Fatalf("loaded %d override rows, want the single enumerated-value row", len(config.TagOverrides))
		}
	})

	t.Run("the elements of a numeric list are refused", func(t *testing.T) {
		_, err := LoadOverrides(overrideFixture(t, "enum_token_on_int_slice_elements.yaml"), overrideIR(), intSliceElementMapping(), fixtureEnumValidators)
		requireExpectedHardFailureContains(t, "a registered token on int elements", err,
			"SliceFixture.items", "registrationStatusType201", "int", "string")
	})
}

// TestOverrideRegisteredTokensShareOneValueSetPerField pins the ruling on a
// second enum token: refused, through the same family-ownership machinery the
// bound families use, with every registered token sharing one family.
//
// The derivation is that a registered name constrains WHICH VALUES the field
// may hold, and a field has one such set -- the mapping derives exactly one
// enum token for an enum-typed property, from that property's own $ref. Two
// tokens compose to a tag demanding both, so the value must sit in the
// intersection of two enums' values, which for two distinct enums is empty:
// the field could never validate. Giving each token its own family (the
// literal reading of "the bare token itself" as a family) is what would let
// that through, so they share one.
//
// The same reasoning refuses the tighten spelling of the question. A tighten
// must be shown to be strictly stricter, and nothing the mapping carries says
// one enum's values are a subset of another's -- the field's own declared type
// is what fixes them -- so swapping one enum's token for another's cannot be
// established as a tightening rather than a change of meaning.
func TestOverrideRegisteredTokensShareOneValueSetPerField(t *testing.T) {
	t.Run("a second enum token added by another row is refused", func(t *testing.T) {
		_, err := LoadOverrides(overrideFixture(t, "enum_token_second_on_one_field.yaml"), overrideIR(), overrideMappings(), fixtureEnumValidators)
		requireExpectedHardFailureContains(t, "two enum tokens added to one field", err,
			"StringFixture.label", "registrationStatusType201", "chargingLimitSourceType201", "satisfy both")
	})

	t.Run("a second enum token over the field's own is refused", func(t *testing.T) {
		_, err := LoadOverrides(overrideFixture(t, "enum_token_cross_enum_add.yaml"), overrideIR(), enumTypedLabelMapping(), fixtureEnumValidators)
		requireExpectedHardFailureContains(t, "an enum token added over the field's own", err,
			"StringFixture.label", "chargingLimitSourceType201", "registrationStatusType201", "satisfy both")
	})

	// "permitted values" is asserted rather than the generic "stricter" the
	// direction rules also use: with every registered token in a family of its
	// own, this row reads as an ordinary cross-family replacement and is
	// refused by that rule instead, for a reason that says nothing about
	// enums. Only the value-set message can satisfy this.
	t.Run("tightening one enum into another is refused", func(t *testing.T) {
		_, err := LoadOverrides(overrideFixture(t, "enum_token_cross_enum_tighten.yaml"), overrideIR(), enumTypedLabelMapping(), fixtureEnumValidators)
		requireExpectedHardFailureContains(t, "a tighten across two enums", err,
			"StringFixture.label", "registrationStatusType201", "chargingLimitSourceType201", "permitted values")
	})
}

// requiredStringSliceMapping is the mapping a required array of plain strings
// produces: no bounds of its own and nothing for its elements to say, so the
// base set does not dive. Any dive on such a property can only come from a
// row, which is what makes it the case where reading scope off the base set
// and reading it off the composed set give different answers.
func requiredStringSliceMapping() OverrideMappings {
	mappings := overrideMappings()
	mappings[OverrideTarget{Definition: "SliceFixture", Property: "items"}] = OverrideMapping{
		BaseTags: []string{"required"}, GoKind: "[]string", FieldName: "Items",
	}
	return mappings
}

// TestOverrideScopeIsReadFromTheComposedTagSet pins the input every
// scope-sensitive rule reads: the tag set a row actually composes onto, base
// plus every earlier row, rather than the base set on its own.
//
// dive is a token a row may add, and adding it moves the end of the tag into
// the element half. So a second row's token can land somewhere the base set
// gives no sign of: with a base of required and rows adding dive and then
// min=2, the field is emitted as required,dive,min=2, where min=2 is each
// entry's length. Judged against the base alone the same row reads as the
// list's item count and passes -- and a two-entry minimum that a
// single-entry list satisfies is exactly the silent disagreement between what
// governance recorded and what the field enforces that this stage exists to
// prevent.
//
// A row adding dive on its own stays legal, which is not a choice made here:
// dive is in the permitted tag grammar, the field-kind rule says which
// properties it may load onto, and a row adding it to a slice is already
// pinned as loading. What changes is what the NEXT row on that property is
// judged against -- once anything has dived, an item-count row on that
// property meets the standing refusal, because the row format cannot say
// which side of dive it means.
func TestOverrideScopeIsReadFromTheComposedTagSet(t *testing.T) {
	t.Run("a row adding dive still loads on its own", func(t *testing.T) {
		config, err := LoadOverrides(overrideFixture(t, "dive_on_slice_positive_control.yaml"), overrideIR(), requiredStringSliceMapping(), nil)
		requireImplemented(t, "a row adding dive to a list (positive control)", err)
		if len(config.TagOverrides) != 1 {
			t.Fatalf("loaded %d override rows, want the single dive row", len(config.TagOverrides))
		}
	})

	t.Run("an item count added after a row's own dive is refused", func(t *testing.T) {
		_, err := LoadOverrides(overrideFixture(t, "add_dive_then_cardinality.yaml"), overrideIR(), requiredStringSliceMapping(), nil)
		requireExpectedHardFailureContains(t, "an item count added after a row added dive", err,
			"SliceFixture.items", "min=2", "dive", "cannot say which")
	})

	t.Run("a base-dive target's two-scope tighten pair still loads", func(t *testing.T) {
		mappings := sliceCardinalityAndElementMapping()
		config, err := LoadOverrides(overrideFixture(t, "tighten_both_dive_scopes.yaml"), overrideIR(), mappings, nil)
		requireImplemented(t, "the cardinality and element tighten pair (control)", err)
		if len(config.TagOverrides) != 2 {
			t.Fatalf("loaded %d override rows, want both scope-specific rows", len(config.TagOverrides))
		}
	})
}

// TestTransformConfigWithASecondDocumentIsRejected covers what a loader
// reading one YAML document never sees, the same way
// TestOverrideConfigWithASecondDocumentIsRejected covers it for the override
// table and TestManifestWithASecondDocumentIsRejected for the manifest.
// multi_document_transform.yaml's first document is the real initialism
// table; the second declares a different table and a field the strict decode
// of the first would have rejected. A loader that decodes only the first
// document accepts the file with both of those unread, and the naming
// transform then runs against a table the file does not unambiguously
// declare -- which renames identifiers across the whole generated tree.
func TestTransformConfigWithASecondDocumentIsRejected(t *testing.T) {
	_, err := LoadTransformConfig(fixturePath("config", "multi_document_transform.yaml"))
	requireExpectedHardFailureContains(t, "single-document initialism table check", err,
		"multi_document_transform.yaml", "more than one YAML document")
	if err != nil && strings.Contains(err.Error(), "\n") {
		t.Fatalf("second-document rejection leaked yaml.v3 multi-line output: %q", err.Error())
	}
}
