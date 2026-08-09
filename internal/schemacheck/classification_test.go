package main

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// classificationFixture mirrors the on-disk fixture shape: raw, independently
// observed Go-side and schema-side facts plus the expected classification.
// There is deliberately no pre-computed "globalRule"/"structValidator answer"
// field here — classifyField must derive both Class and Rule itself by
// applying the mapping rules to ComparisonInput, or these tests would only
// be checking that a label was echoed back (a labeller, not a classifier).
type classificationFixture struct {
	ExpectedClass string          `json:"expectedClass"`
	Input         ComparisonInput `json:"input"`
}

func loadClassificationFixture(t *testing.T, file string) classificationFixture {
	t.Helper()
	path := fixturePath(t, "classification", file)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("classification fixture %s is unavailable: %v", path, err)
	}
	var fixture classificationFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("parse classification fixture %s: %v", path, err)
	}
	return fixture
}

func TestClassificationCoversEveryClass(t *testing.T) {
	cases := []struct {
		class string
		file  string
		// wantRuleContains is a case-insensitive substring the Rule text
		// must contain; every non-identical class needs one, since every
		// non-identical field row must name why it differs. UNEXPLAINED is
		// the deliberate exception in the other direction: by definition
		// nothing explains it, so its Rule must be empty. IDENTICAL is
		// exempt from both requirements — nothing differs, so nothing is
		// obliged to be cited, though the classifier is free to leave an
		// informational note; either is conformant.
		wantRuleEmpty    bool
		wantRuleContains string
	}{
		{class: IDENTICAL, file: "IDENTICAL.json"},
		{class: SCHEMA_FAITHFUL_CHANGE, file: "SCHEMA_FAITHFUL_CHANGE.json", wantRuleContains: "value + omitempty"},
		{class: FORK_BUG, file: "FORK_BUG.json", wantRuleContains: "wrong type"},
		{class: ADDITIVE, file: "ADDITIVE.json", wantRuleContains: "schema"},
		{class: OVERRIDE_CANDIDATE, file: "OVERRIDE_CANDIDATE.json", wantRuleContains: "strict"},
		{class: STRUCT_VALIDATOR, file: "STRUCT_VALIDATOR.json", wantRuleContains: "validat"},
		{class: UNEXPLAINED, file: "UNEXPLAINED.json", wantRuleEmpty: true},
	}
	for _, test := range cases {
		t.Run(test.class, func(t *testing.T) {
			fixture := loadClassificationFixture(t, test.file)
			if fixture.ExpectedClass != test.class {
				t.Fatalf("fixture class=%q, want %q", fixture.ExpectedClass, test.class)
			}
			result, err := classifyField(fixture.Input)
			if err != nil {
				t.Fatalf("classification %s is not implemented: %v", test.class, err)
			}
			if result.Class != test.class {
				t.Fatalf("classification returned %q, want %q", result.Class, test.class)
			}
			switch {
			case test.wantRuleEmpty:
				if result.Rule != "" {
					t.Fatalf("%s must cite no rule (nothing explains it — a row that cannot cite a reason is classed UNEXPLAINED by definition), got %q", test.class, result.Rule)
				}
			case test.class == IDENTICAL:
				// Only non-identical rows are obliged to cite a rule;
				// IDENTICAL may carry an empty Rule or an informational
				// one, so nothing further is asserted beyond the class
				// check above.
			default:
				if result.Rule == "" {
					t.Fatalf("%s did not cite a rule (every non-identical row must name why)", test.class)
				}
				if test.wantRuleContains != "" && !strings.Contains(strings.ToLower(result.Rule), strings.ToLower(test.wantRuleContains)) {
					t.Fatalf("%s rule %q does not cite the expected reason (want substring %q)", test.class, result.Rule, test.wantRuleContains)
				}
			}
		})
	}
}

// TestStructValidatorFixtureIsolatesTheValidatorFact keeps the
// STRUCT_VALIDATOR fixture a minimal pair against the IDENTICAL one: the two
// inputs agree on every observed fact except the discovered struct-validation
// rule, so that rule is the only thing that can move the row out of
// IDENTICAL. A fixture that also carried, say, an optionality difference
// would fail a classifier that correctly reported that difference, and would
// let a classifier that stops at the validator fact hide a real divergence
// behind it — neither outcome says anything about the class under test.
func TestStructValidatorFixtureIsolatesTheValidatorFact(t *testing.T) {
	identical := loadClassificationFixture(t, "IDENTICAL.json")
	validator := loadClassificationFixture(t, "STRUCT_VALIDATOR.json")

	if len(validator.Input.Go.StructValidators) == 0 {
		t.Fatal("STRUCT_VALIDATOR fixture records no struct-validation rule, so it cannot exercise the class at all")
	}
	if len(identical.Input.Go.StructValidators) != 0 {
		t.Fatalf("IDENTICAL fixture records a struct-validation rule (%v), so the two fixtures no longer differ in exactly that one fact", identical.Input.Go.StructValidators)
	}
	withoutValidator := validator.Input
	withoutValidator.Go.StructValidators = nil
	if !reflect.DeepEqual(withoutValidator, identical.Input) {
		t.Fatalf("STRUCT_VALIDATOR fixture differs from IDENTICAL beyond the struct-validation rule, so the rule is not the sole discriminator:\nstruct-validator fixture with the rule removed = %#v\nidentical fixture                            = %#v", withoutValidator, identical.Input)
	}

	result, err := classifyField(validator.Input)
	if err != nil {
		t.Fatalf("classification is not implemented: %v", err)
	}
	if result.Class != STRUCT_VALIDATOR {
		t.Fatalf("a row whose only difference from an IDENTICAL row is a discovered struct-validation rule was classified as %q, want %q", result.Class, STRUCT_VALIDATOR)
	}
}

// TestSchemaFaithfulChangeClassificationTiesToBreakingPredicate ties
// ClassificationResult.Breaking, as produced by the classifier on a real
// fixture, to the same value an independent breakingPredicate call produces
// for the equivalent before/after signatures — the two must never diverge.
func TestSchemaFaithfulChangeClassificationTiesToBreakingPredicate(t *testing.T) {
	fixture := loadClassificationFixture(t, "SCHEMA_FAITHFUL_CHANGE.json")
	result, err := classifyField(fixture.Input)
	if err != nil {
		t.Fatalf("classification is not implemented: %v", err)
	}
	if result.Class != SCHEMA_FAITHFUL_CHANGE {
		t.Fatalf("fixture classified as %q, want %q", result.Class, SCHEMA_FAITHFUL_CHANGE)
	}

	// The fixture's Go side is *string (the pre-narrowing convention); the
	// schema-faithful mapping for an optional string/enum field produces a
	// plain string (value + omitempty) instead. That declaredType change is
	// what makes this fixture breaking.
	before := FieldSignature{Present: true, Name: fixture.Input.Go.Name, DeclaredType: fixture.Input.Go.DeclaredType}
	after := FieldSignature{Present: true, Name: fixture.Input.Go.Name, DeclaredType: "string"}
	independent, err := breakingPredicate(SCHEMA_FAITHFUL_CHANGE, before, after)
	if err != nil {
		t.Fatalf("breaking predicate is not implemented: %v", err)
	}
	if !independent {
		t.Fatal("independent breaking-predicate computation over the fixture's declaredType change did not report breaking")
	}
	if result.Breaking != independent {
		t.Fatalf("classifier's Breaking=%t disagrees with the independent breaking-predicate result=%t", result.Breaking, independent)
	}
}

// TestForkBugSubKinds covers the FORK_BUG sub-kinds beyond the plain
// wrong-type case already in FORK_BUG.json. The report's field-row shape
// has no dedicated sub-kind field — grouping "by sub-kind" for the
// FORK-BUG roll-up is done by reading the Rule text itself — so each case
// here requires a distinct, distinguishing token in Rule rather than merely
// a non-empty one.
func TestForkBugSubKinds(t *testing.T) {
	cases := []struct {
		name             string
		file             string
		wantRuleContains string
	}{
		{name: "looser than schema constraint", file: "fork_bug_looser_constraint.json", wantRuleContains: "looser"},
		{name: "schema-required field missing from Go", file: "fork_bug_required_missing.json", wantRuleContains: "required"},
		// A Go field the schema does not authorize is its own explicit
		// output row, never silently dropped.
		{name: "Go-only field absent from schema", file: "fork_bug_go_only_field.json", wantRuleContains: "not in schema"},
		// validator.v9's min/max tokens are type-sensitive: on a numeric Go
		// field they bound the value (like gte/lte), not its length. This
		// fixture's Go side is a plain int with validate:"min=-9,max=9" —
		// the real IdTokenInfo.ChargingPriority shape — paired against a
		// schema maximum one below the Go tag's max, so the divergence can
		// only be caught by comparing against the schema's numeric
		// maximum, never minLength/maxLength (which a numeric schema
		// property never carries).
		{name: "numeric constraint looser than schema", file: "fork_bug_numeric_min_max.json", wantRuleContains: "looser"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			fixture := loadClassificationFixture(t, test.file)
			result, err := classifyField(fixture.Input)
			if err != nil {
				t.Fatalf("classification is not implemented: %v", err)
			}
			if result.Class != FORK_BUG {
				t.Fatalf("%s classified as %q, want %q", test.name, result.Class, FORK_BUG)
			}
			if result.Rule == "" {
				t.Fatalf("%s did not cite a rule", test.name)
			}
			if !strings.Contains(strings.ToLower(result.Rule), test.wantRuleContains) {
				t.Fatalf("%s rule %q does not carry a sub-kind-distinguishing token (want substring %q), so the FORK-BUG roll-up grouped by sub-kind would have no distinguishable key", test.name, result.Rule, test.wantRuleContains)
			}
		})
	}
}

// TestLooserConstraintIsNotOverrideCandidate guards the direction of the
// stricter/looser split explicitly: a looser-than-schema constraint must
// not be misclassified as the (stricter-only) OVERRIDE_CANDIDATE class.
func TestLooserConstraintIsNotOverrideCandidate(t *testing.T) {
	fixture := loadClassificationFixture(t, "fork_bug_looser_constraint.json")
	result, err := classifyField(fixture.Input)
	if err != nil {
		t.Fatalf("classification is not implemented: %v", err)
	}
	if result.Class == OVERRIDE_CANDIDATE {
		t.Fatal("a looser-than-schema constraint was classified as OVERRIDE_CANDIDATE instead of FORK_BUG")
	}
}

// TestRegisteredMarshalerDateTimeComparesOnWireType is the negative control
// for a classifier that cheats by comparing declaredType instead of
// wireType: this field's declaredType (a pointer to a registered-marshaler
// type, mirroring *types.DateTime) legitimately diverges from its own
// wireType (the plain string the resolver produces once the custom
// marshaler is accounted for), and a schema-faithful string+date-time
// property is exactly what that wireType should compare equal to. Schema
// conformance is defined in terms of wireType, never declaredType — a
// classifier that compared declaredType here would see "*types.DateTime"
// against a schema "string" and wrongly call this UNEXPLAINED.
func TestRegisteredMarshalerDateTimeComparesOnWireType(t *testing.T) {
	fixture := loadClassificationFixture(t, "IDENTICAL_registered_marshaler_datetime.json")
	if fixture.ExpectedClass != IDENTICAL {
		t.Fatalf("fixture class=%q, want %q", fixture.ExpectedClass, IDENTICAL)
	}
	if fixture.Input.Go.DeclaredType == fixture.Input.Go.WireType {
		t.Fatalf("fixture does not exercise a declaredType/wireType divergence: both are %q", fixture.Input.Go.DeclaredType)
	}
	result, err := classifyField(fixture.Input)
	if err != nil {
		t.Fatalf("classification is not implemented: %v", err)
	}
	if result.Class != IDENTICAL {
		t.Fatalf("registered-marshaler date-time field classified as %q, want %q — classification must compare wireType, never declaredType", result.Class, IDENTICAL)
	}
}

func TestSchemaFaithfulTypeChangeIsBreaking(t *testing.T) {
	breaking, err := breakingPredicate(SCHEMA_FAITHFUL_CHANGE,
		FieldSignature{Present: true, Name: "Value", DeclaredType: "string"},
		FieldSignature{Present: true, Name: "Value", DeclaredType: "*string"})
	if err != nil {
		t.Fatalf("breaking predicate is not implemented: %v", err)
	}
	if !breaking {
		t.Fatal("declared type change was classified as non-breaking")
	}
}

func TestSchemaFaithfulTagOnlyChangeIsNotBreaking(t *testing.T) {
	breaking, err := breakingPredicate(SCHEMA_FAITHFUL_CHANGE,
		FieldSignature{Present: true, Name: "Value", DeclaredType: "string", ValidateTag: "max=10"},
		FieldSignature{Present: true, Name: "Value", DeclaredType: "string", ValidateTag: "max=20"})
	if err != nil {
		t.Fatalf("breaking predicate is not implemented: %v", err)
	}
	if breaking {
		t.Fatal("validation-tag-only change was classified as breaking")
	}
}

func TestBreakingPredicateNameChangeIsBreaking(t *testing.T) {
	breaking, err := breakingPredicate(SCHEMA_FAITHFUL_CHANGE,
		FieldSignature{Present: true, Name: "Value", DeclaredType: "string"},
		FieldSignature{Present: true, Name: "Val", DeclaredType: "string"})
	if err != nil {
		t.Fatalf("breaking predicate is not implemented: %v", err)
	}
	if !breaking {
		t.Fatal("Go field name change was classified as non-breaking")
	}
}

func TestBreakingPredicatePresenceChangeIsBreaking(t *testing.T) {
	breaking, err := breakingPredicate(SCHEMA_FAITHFUL_CHANGE,
		FieldSignature{Present: true, Name: "Value", DeclaredType: "string"},
		FieldSignature{Present: false, Name: "Value", DeclaredType: "string"})
	if err != nil {
		t.Fatalf("breaking predicate is not implemented: %v", err)
	}
	if !breaking {
		t.Fatal("field presence change (added/removed) was classified as non-breaking")
	}
}

func TestBreakingPredicateConstructorSignatureChangeIsBreaking(t *testing.T) {
	breaking, err := breakingPredicate(SCHEMA_FAITHFUL_CHANGE,
		FieldSignature{Present: true, Name: "Value", DeclaredType: "string", ConstructorSignature: "NewFoo(value string)"},
		FieldSignature{Present: true, Name: "Value", DeclaredType: "string", ConstructorSignature: "NewFoo(value string, extra int)"})
	if err != nil {
		t.Fatalf("breaking predicate is not implemented: %v", err)
	}
	if !breaking {
		t.Fatal("constructor signature change was classified as non-breaking")
	}
}

// TestBreakingPredicateEnumIdentityChangeIsBreaking isolates the enum
// trigger. The enum type/const name a field uses is its own signature fact,
// separate from the declared type text: it names the identifiers a caller
// writes at the call site, and it can change while the field's declared type
// stays exactly as it was — here a plain string field whose accepted value
// set comes from a named enum/const set resolved through its validation tag.
// Everything else in the pair is held identical (name, declared type,
// presence, validation tag, constructor signature), so enum identity is the
// only thing that moved and the predicate has to react to that fact on its
// own. A predicate that only compared declared types would report this
// caller-visible rename as non-breaking.
func TestBreakingPredicateEnumIdentityChangeIsBreaking(t *testing.T) {
	before := FieldSignature{
		Present:              true,
		Name:                 "Status",
		DeclaredType:         "string",
		ValidateTag:          "required,chargePointStatus",
		EnumType:             "ChargePointStatus",
		ConstructorSignature: "NewStatusNotification(status string)",
	}
	after := before
	after.EnumType = "ChargePointStatusEnumType"

	breaking, err := breakingPredicate(SCHEMA_FAITHFUL_CHANGE, before, after)
	if err != nil {
		t.Fatalf("breaking predicate is not implemented: %v", err)
	}
	if !breaking {
		t.Fatalf("enum type/const name change was classified as non-breaking; the pair differs only in enum identity (%q -> %q), everything else is identical: %#v", before.EnumType, after.EnumType, before)
	}
}

// TestBreakingPredicateNonSchemaFaithfulClassIsNeverBreaking is the class
// guard: breaking is meaningful only on SCHEMA_FAITHFUL_CHANGE rows — every
// other class's breaking is false, even when the underlying signature change
// (here, a name change) would have been breaking on a SCHEMA_FAITHFUL_CHANGE
// row.
func TestBreakingPredicateNonSchemaFaithfulClassIsNeverBreaking(t *testing.T) {
	breaking, err := breakingPredicate(FORK_BUG,
		FieldSignature{Present: true, Name: "Value", DeclaredType: "string"},
		FieldSignature{Present: true, Name: "Val", DeclaredType: "string"})
	if err != nil {
		t.Fatalf("breaking predicate is not implemented: %v", err)
	}
	if breaking {
		t.Fatal("a non-SCHEMA_FAITHFUL_CHANGE class reported breaking=true; breaking must be false for every class but SCHEMA_FAITHFUL_CHANGE")
	}
}

// TestWireTypeVocabularyNormalization covers every non-string JSON Schema
// type against its Go wireType counterpart, in both directions: a fixture
// whose Go wireType and schema type name the same shape must not be
// FORK_BUG, and a minimal-pair fixture whose schema type was changed to a
// genuinely different shape must be. Every pair holds everything but the
// schema type fixed, isolating wireType normalization as the only thing
// that can move the result — a classifier that still compared Go's own
// type spelling against the schema's as raw text would fail every
// "identical" case here (only a Go field literally spelled "string" would
// ever agree with a schema "string"), while a classifier that always
// mapped to "object" regardless of Go kind would fail the integer/number/
// boolean/array "identical" cases specifically.
func TestWireTypeVocabularyNormalization(t *testing.T) {
	cases := []struct {
		schemaKind string
		identical  string
		mismatch   string
	}{
		{schemaKind: "integer", identical: "wiretype_integer_identical.json", mismatch: "wiretype_integer_mismatch.json"},
		{schemaKind: "number", identical: "wiretype_number_identical.json", mismatch: "wiretype_number_mismatch.json"},
		{schemaKind: "boolean", identical: "wiretype_boolean_identical.json", mismatch: "wiretype_boolean_mismatch.json"},
		{schemaKind: "array", identical: "wiretype_array_identical.json", mismatch: "wiretype_array_mismatch.json"},
		{schemaKind: "object", identical: "wiretype_object_identical.json", mismatch: "wiretype_object_mismatch.json"},
	}
	for _, test := range cases {
		t.Run(test.schemaKind+"/matching wireType is not FORK_BUG", func(t *testing.T) {
			fixture := loadClassificationFixture(t, test.identical)
			if fixture.ExpectedClass != IDENTICAL {
				t.Fatalf("fixture %s declares expectedClass=%q, want %q", test.identical, fixture.ExpectedClass, IDENTICAL)
			}
			result, err := classifyField(fixture.Input)
			if err != nil {
				t.Fatalf("classification is not implemented: %v", err)
			}
			if result.Class == FORK_BUG {
				t.Fatalf("Go wireType %q was not recognized as schema type %q: got %q instead of %q (rule: %s)",
					fixture.Input.Go.WireType, test.schemaKind, result.Class, IDENTICAL, result.Rule)
			}
			if result.Class != IDENTICAL {
				t.Fatalf("Go wireType %q against schema type %q classified as %q, want %q", fixture.Input.Go.WireType, test.schemaKind, result.Class, IDENTICAL)
			}
		})
		t.Run(test.schemaKind+"/genuine mismatch is FORK_BUG", func(t *testing.T) {
			fixture := loadClassificationFixture(t, test.mismatch)
			if fixture.ExpectedClass != FORK_BUG {
				t.Fatalf("fixture %s declares expectedClass=%q, want %q", test.mismatch, fixture.ExpectedClass, FORK_BUG)
			}
			result, err := classifyField(fixture.Input)
			if err != nil {
				t.Fatalf("classification is not implemented: %v", err)
			}
			if result.Class != FORK_BUG {
				t.Fatalf("Go wireType %q against a genuinely different schema type classified as %q, want %q — a normalizer that collapses every non-string type onto one bucket would wrongly pass this", fixture.Input.Go.WireType, result.Class, FORK_BUG)
			}
			if !strings.Contains(strings.ToLower(result.Rule), "wrong type") {
				t.Fatalf("FORK_BUG rule %q does not cite the wrong-type reason", result.Rule)
			}
		})
	}
}

// TestDateTimeSchemaPropertyWithPlainStringFieldIsNotIdentical covers the
// date-time narrowing rule's other direction from
// TestRegisteredMarshalerDateTimeComparesOnWireType: a schema property typed
// string with format:date-time is not satisfied by a plain Go string field
// that never went through the DateTime wire-type resolver at all (its
// declaredType and wireType agree, unlike the registered-marshaler fixture,
// where they legitimately diverge). The schema-faithful mapping for this
// shape is *types.DateTime, so the pairing must classify as
// SCHEMA_FAITHFUL_CHANGE — and, since that changes the field's declared type
// outright rather than just its pointer-ness, as breaking.
func TestDateTimeSchemaPropertyWithPlainStringFieldIsNotIdentical(t *testing.T) {
	fixture := loadClassificationFixture(t, "schema_faithful_datetime_narrowing.json")
	if fixture.ExpectedClass != SCHEMA_FAITHFUL_CHANGE {
		t.Fatalf("fixture declares expectedClass=%q, want %q", fixture.ExpectedClass, SCHEMA_FAITHFUL_CHANGE)
	}
	if fixture.Input.Go.DeclaredType != fixture.Input.Go.WireType {
		t.Fatalf("fixture does not exercise the no-custom-marshaler shape: declaredType=%q wireType=%q must agree", fixture.Input.Go.DeclaredType, fixture.Input.Go.WireType)
	}
	result, err := classifyField(fixture.Input)
	if err != nil {
		t.Fatalf("classification is not implemented: %v", err)
	}
	if result.Class == IDENTICAL {
		t.Fatal("a plain (unmarshaled) string field paired against a string+format:date-time schema property was classified as IDENTICAL — the census would undercount the date-time narrowing class")
	}
	if result.Class != SCHEMA_FAITHFUL_CHANGE {
		t.Fatalf("plain string vs string+format:date-time classified as %q, want %q", result.Class, SCHEMA_FAITHFUL_CHANGE)
	}
	if !strings.Contains(strings.ToLower(result.Rule), "date-time") {
		t.Fatalf("rule %q does not cite the date-time mapping rule", result.Rule)
	}
	if !result.Breaking {
		t.Fatal("adopting *types.DateTime in place of a plain string changes the declared type outright, so this row must be breaking")
	}
}

// TestNumericMinMaxTokensCompareAgainstSchemaBounds covers validator.v9's
// type-sensitive min/max: a numeric Go field's min/max tokens are numeric
// bounds (like gte/lte), never a length. The divergence direction is
// TestForkBugSubKinds's "numeric constraint looser than schema" case; this
// test covers the companion direction — a numeric field whose validate
// min/max exactly match the schema's minimum/maximum — asserting both the
// end-to-end classification (IDENTICAL, not FORK_BUG) and, more precisely,
// that compareConstraints itself considered the numeric bounds and found
// them equal (matched=false) rather than never having compared them at all,
// which would also produce matched=false for the wrong reason (a numeric
// field's minLength/maxLength are always nil, so a constraint check that
// still consulted those would silently no-op regardless of what the
// validate tag said).
func TestNumericMinMaxTokensCompareAgainstSchemaBounds(t *testing.T) {
	fixture := loadClassificationFixture(t, "numeric_min_max_matches_schema.json")
	if fixture.ExpectedClass != IDENTICAL {
		t.Fatalf("fixture declares expectedClass=%q, want %q", fixture.ExpectedClass, IDENTICAL)
	}
	if !isNumericWireType(fixture.Input.Go.WireType) {
		t.Fatalf("fixture does not exercise a numeric Go field: wireType=%q", fixture.Input.Go.WireType)
	}
	if fixture.Input.Schema.Constraints.MinLength != nil || fixture.Input.Schema.Constraints.MaxLength != nil {
		t.Fatal("fixture carries minLength/maxLength, which would let a type-blind implementation pass this test for the wrong reason")
	}
	if fixture.Input.Schema.Constraints.Minimum == nil || fixture.Input.Schema.Constraints.Maximum == nil {
		t.Fatal("fixture must carry schema minimum/maximum for the numeric comparison to have anything to check against")
	}

	_, matched := compareConstraints(fixture.Input)
	if matched {
		t.Fatal("compareConstraints reported a divergence for a numeric field whose validate min/max exactly match the schema's minimum/maximum")
	}

	result, err := classifyField(fixture.Input)
	if err != nil {
		t.Fatalf("classification is not implemented: %v", err)
	}
	if result.Class != IDENTICAL {
		t.Fatalf("numeric field with matching min/max classified as %q, want %q", result.Class, IDENTICAL)
	}
}
