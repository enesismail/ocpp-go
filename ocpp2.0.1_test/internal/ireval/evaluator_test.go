package ireval

import (
	"fmt"
	"testing"
)

func TestEvaluateSupportsEachKeywordOnPassAndFailPaths(t *testing.T) {
	// Pass/fail fixtures sit exactly at the boundary the keyword names,
	// rather than somewhere comfortably inside or outside it: a value one
	// past the limit is where an off-by-one in the evaluator would actually
	// show up (e.g. ">" written where ">=" belongs), and a value comfortably
	// clear of the limit would not catch that. minItems was already
	// boundary-exact; the rest are made so here.
	cases := []struct {
		keyword string
		pass    string
		fail    string
	}{
		{keyword: "required", pass: `{"value":"ok"}`, fail: `{}`},
		{keyword: "type", pass: `{"value":"ok"}`, fail: `{"value":7}`},
		{keyword: "maxLength", pass: `{"value":"abc"}`, fail: `{"value":"abcd"}`},
		{keyword: "minLength", pass: `{"value":"abc"}`, fail: `{"value":"ab"}`},
		{keyword: "minimum", pass: `{"value":2}`, fail: `{"value":1}`},
		{keyword: "maximum", pass: `{"value":5}`, fail: `{"value":6}`},
		{keyword: "minItems", pass: `{"value":[1,2]}`, fail: `{"value":[1]}`},
		{keyword: "maxItems", pass: `{"value":[1,2]}`, fail: `{"value":[1,2,3]}`},
		{keyword: "enum", pass: `{"value":"Ready"}`, fail: `{"value":"Rejected"}`},
		// The fail value contains "OK-42" as a substring but is not equal to
		// it end to end, so it only fails a properly anchored match: an
		// evaluator that dropped the pattern's own "^"/"$" anchors and fell
		// back to an unanchored substring search would wrongly accept it,
		// where "bad" alone would not have caught that mutant (it contains
		// no substring match either way).
		{keyword: "pattern", pass: `{"value":"OK-42"}`, fail: `{"value":"xOK-42y"}`},
	}

	for _, tc := range cases {
		t.Run(tc.keyword+" pass", func(t *testing.T) {
			verdict, err := Evaluate(keywordIR(tc.keyword), "Synthetic", Request, []byte(tc.pass))
			if err != nil {
				t.Fatalf("pass path for %s returned an error: %v", tc.keyword, err)
			}
			if verdict.Status != Valid {
				t.Fatalf("pass path for %s returned %#v, want valid", tc.keyword, verdict)
			}
		})
		t.Run(tc.keyword+" fail", func(t *testing.T) {
			verdict, err := Evaluate(keywordIR(tc.keyword), "Synthetic", Request, []byte(tc.fail))
			if err != nil {
				t.Fatalf("fail path for %s returned an error: %v", tc.keyword, err)
			}
			if verdict.Status != Invalid || verdict.Keyword != tc.keyword {
				t.Fatalf("fail path for %s returned %#v, want invalid/%s", tc.keyword, verdict, tc.keyword)
			}
		})
	}
}

// TestEvaluateMinimumZeroAllowsValueZero is the interval-override case's
// exact shape: BootNotificationResponse.Interval carries a schema minimum
// of 0 (mapped to the Go tag gte=0), and a value of exactly 0 is valid. A
// naive "value == 0 is falsy, treat as absent/reject" bug would only ever
// show up at this specific boundary, which is why it gets its own test
// instead of relying on the generic minimum case above (whose boundary is
// 2, not 0).
func TestEvaluateMinimumZeroAllowsValueZero(t *testing.T) {
	property := `{"name":"value","pointer":"#/properties/value","type":"integer","required":false,"minimum":0}`
	verdict, err := Evaluate(rootIR(property), "Synthetic", Request, []byte(`{"value":0}`))
	if err != nil {
		t.Fatalf("minimum:0 with value 0 returned an error: %v", err)
	}
	if verdict.Status != Valid {
		t.Fatalf("minimum:0 with value 0 = %#v, want valid", verdict)
	}
}

// TestEvaluateMinimumZeroRejectsValueBelowZero is
// TestEvaluateMinimumZeroAllowsValueZero's negative counterpart: minimum:0
// is a real, live lower bound, not a bound that a "0 is falsy, treat it as
// unset" implementation would skip checking altogether. Without this case,
// an evaluator that silently ignored minimum whenever it was 0 would still
// pass every other test in this file — minimum:0/value:0 looks identical
// whether the bound is enforced or skipped, and the only value that tells
// the two apart is one that violates the bound.
func TestEvaluateMinimumZeroRejectsValueBelowZero(t *testing.T) {
	property := `{"name":"value","pointer":"#/properties/value","type":"integer","required":false,"minimum":0}`
	verdict, err := Evaluate(rootIR(property), "Synthetic", Request, []byte(`{"value":-1}`))
	if err != nil {
		t.Fatalf("minimum:0 with value -1 returned an error: %v", err)
	}
	if verdict.Status != Invalid || verdict.Keyword != "minimum" {
		t.Fatalf("minimum:0 with value -1 = %#v, want invalid/minimum", verdict)
	}
}

// TestEvaluateFailVerdictReportsPropertyPath pins that a rejecting verdict
// names where it failed, not only which keyword: Path carries the
// property's IR pointer (the same pointer grammar
// internal/codegen/ir.go documents on Property.Pointer), so a caller can
// report a violation location, not just a bare keyword name.
func TestEvaluateFailVerdictReportsPropertyPath(t *testing.T) {
	verdict, err := Evaluate(keywordIR("maxLength"), "Synthetic", Request, []byte(`{"value":"abcd"}`))
	if err != nil {
		t.Fatalf("maxLength fail path returned an error: %v", err)
	}
	if verdict.Status != Invalid || verdict.Keyword != "maxLength" {
		t.Fatalf("maxLength fail verdict = %#v, want invalid/maxLength", verdict)
	}
	if verdict.Path != "#/properties/value" {
		t.Fatalf("maxLength fail verdict.Path = %q, want %q (the property's IR pointer)", verdict.Path, "#/properties/value")
	}
}

// TestEvaluateAbsentOptionalPropertyIsUnconstrained pins that an optional
// property's constraints only apply once the property is actually present.
// minLength never gets a chance to reject a payload that omits the
// property entirely; required is the only keyword that can reject an
// absence, and this property does not carry it.
func TestEvaluateAbsentOptionalPropertyIsUnconstrained(t *testing.T) {
	property := `{"name":"value","pointer":"#/properties/value","type":"string","required":false,"minLength":3}`
	verdict, err := Evaluate(rootIR(property), "Synthetic", Request, []byte(`{}`))
	if err != nil {
		t.Fatalf("absent optional property returned an error: %v", err)
	}
	if verdict.Status != Valid {
		t.Fatalf("absent optional property with minLength = %#v, want valid", verdict)
	}
}

// TestEvaluateFollowsRefToNestedDefinition exercises constraints reached
// only through a $ref — the golden dump's "ref" property shape
// (internal/codegen/testdata/golden/golden_message.json's "shared" property
// on root_kind). Following the reference is structural navigation to find
// the property's constraints, not itself an evaluated keyword, so it stays
// in scope even though the supported-keyword list never mentions "ref": the
// boundary this evaluator draws is on keywords, not on how a property's
// definition is reached.
func TestEvaluateFollowsRefToNestedDefinition(t *testing.T) {
	rootProperty := `{"name":"detail","pointer":"#/properties/detail","required":false,"ref":"#/definitions/Detail"}`
	detailDefinition := `{"name":"Detail","kind":"object","properties":[{"name":"label","pointer":"#/definitions/Detail/properties/label","type":"string","required":false,"maxLength":3}],"reserved":false,"occurrences":1,"files":["Detail.json"]}`
	ir := rootIR(rootProperty, detailDefinition)

	t.Run("pass", func(t *testing.T) {
		verdict, err := Evaluate(ir, "Synthetic", Request, []byte(`{"detail":{"label":"ok"}}`))
		if err != nil {
			t.Fatalf("ref-following pass path returned an error: %v", err)
		}
		if verdict.Status != Valid {
			t.Fatalf("ref-following pass path = %#v, want valid", verdict)
		}
	})
	t.Run("fail", func(t *testing.T) {
		verdict, err := Evaluate(ir, "Synthetic", Request, []byte(`{"detail":{"label":"toolong"}}`))
		if err != nil {
			t.Fatalf("ref-following fail path returned an error: %v", err)
		}
		if verdict.Status != Invalid || verdict.Keyword != "maxLength" {
			t.Fatalf("ref-following fail path = %#v, want invalid/maxLength", verdict)
		}
	})
}

// TestEvaluateResolvesEnumValuesThroughRef exercises the shape the emitted IR
// uses for an enum-valued property: the property itself carries only "ref" and
// "enumDefinition", and the permitted values live on the referenced definition
// (kind "enum", with a "values" list), exactly as the dump
// internal/codegen/testdata/golden/enum_ref.ir.json records. The inline "enum"
// row in the table above stays, because a property carrying its own value list
// is also a real shape in this contract: internal/codegen/ir.go declares
// Property.Enum, and internal/codegen/schema_test.go's
// TestEnumReferenceResolvesValuesAndAllowsAdditionalProperties pins that the
// reader resolves a referenced enum's values onto the referencing property's
// Enum field. Only the case below, though, catches an evaluator that reads a
// property's own value list and stops there: the property here has no list of
// its own, so such an implementation finds nothing to check and accepts a
// misspelled member.
func TestEvaluateResolvesEnumValuesThroughRef(t *testing.T) {
	rootProperty := `{"name":"status","pointer":"#/properties/status","required":false,"ref":"#/definitions/StatusEnumType","enumDefinition":"StatusEnumType"}`
	enumDefinition := `{"name":"StatusEnumType","kind":"enum","values":["Accepted","Rejected"],"reserved":false,"occurrences":1,"files":["StatusEnumType.json"]}`
	ir := rootIR(rootProperty, enumDefinition)

	t.Run("pass", func(t *testing.T) {
		verdict, err := Evaluate(ir, "Synthetic", Request, []byte(`{"status":"Accepted"}`))
		if err != nil {
			t.Fatalf("ref'd-enum pass path returned an error: %v", err)
		}
		if verdict.Status != Valid {
			t.Fatalf("ref'd-enum pass path = %#v, want valid", verdict)
		}
	})
	t.Run("fail", func(t *testing.T) {
		// "Rejcted" is a misspelling of the member "Rejected", the exact
		// mistake an evaluator that never reaches the referenced value list
		// would wave through.
		verdict, err := Evaluate(ir, "Synthetic", Request, []byte(`{"status":"Rejcted"}`))
		if err != nil {
			t.Fatalf("ref'd-enum fail path returned an error: %v", err)
		}
		if verdict.Status != Invalid || verdict.Keyword != "enum" {
			t.Fatalf("ref'd-enum fail path = %#v, want invalid/enum", verdict)
		}
	})
}

// TestEvaluateAppliesArrayItemConstraints pins that evaluation descends into
// array elements. The minItems and maxItems rows in the table above vary only
// cardinality, and their element subschema carries no constraint at all, so
// nothing there tells an evaluator that stops at the array's length apart from
// one that also checks what the array holds. internal/codegen/ir.go models an
// element as Items *Property — an element carries the same keyword set as any
// other property — and this exercises that with maxLength on the element. The
// violating element is the second one, so an evaluator that checks only the
// first element fails this too.
func TestEvaluateAppliesArrayItemConstraints(t *testing.T) {
	property := `{"name":"value","pointer":"#/properties/value","type":"array","required":false,"items":{"name":"item","pointer":"#/properties/value/items","type":"string","required":false,"maxLength":3}}`
	ir := rootIR(property)

	t.Run("pass", func(t *testing.T) {
		verdict, err := Evaluate(ir, "Synthetic", Request, []byte(`{"value":["abc","xyz"]}`))
		if err != nil {
			t.Fatalf("array-item pass path returned an error: %v", err)
		}
		if verdict.Status != Valid {
			t.Fatalf("array-item pass path = %#v, want valid", verdict)
		}
	})
	t.Run("fail", func(t *testing.T) {
		verdict, err := Evaluate(ir, "Synthetic", Request, []byte(`{"value":["abc","abcd"]}`))
		if err != nil {
			t.Fatalf("array-item fail path returned an error: %v", err)
		}
		if verdict.Status != Invalid || verdict.Keyword != "maxLength" {
			t.Fatalf("array-item fail path = %#v, want invalid/maxLength (the constraint on the element, not on the array)", verdict)
		}
	})
}

// TestEvaluateReportsUnsupportedKeyword uses exclusiveMinimum and
// exclusiveMaximum — draft-06's own two keywords this evaluator explicitly
// defers, per the package doc's stated supported set — rather than
// additionalProperties. additionalProperties:false is present on nearly
// every real definition in the corpus, so using it here would make this
// fixture fire on almost anything the evaluator is asked to check; see
// TestEvaluateSupportedKeywordDecidesBeforeAdditionalPropertiesPreempts
// below for why that distinction is load-bearing, not cosmetic. The IR
// here is built through keywordIR, the same complete-dump-shape helper the
// pass/fail table above uses, rather than a hand-rolled, differently-shaped
// document.
func TestEvaluateReportsUnsupportedKeyword(t *testing.T) {
	for _, keyword := range []string{"exclusiveMinimum", "exclusiveMaximum"} {
		t.Run(keyword, func(t *testing.T) {
			verdict, err := Evaluate(keywordIR(keyword), "Synthetic", Request, []byte(`{"value":5}`))
			if err != nil {
				t.Fatalf("unsupported keyword must be a verdict, not an evaluator error: %v", err)
			}
			if verdict.Status != Unsupported || verdict.Keyword != keyword {
				t.Fatalf("unsupported keyword verdict = %#v, want unsupported/%s", verdict, keyword)
			}
		})
	}
}

// TestEvaluateSupportedKeywordDecidesBeforeAdditionalPropertiesPreempts pins
// the scoping rule documented on the package: an unsupported keyword (here,
// additionalProperties:false, carried by the definition itself rather than
// by the property) is reported only when it would actually decide the
// payload's verdict. This payload violates maxLength and adds no property
// the definition does not list, so additionalProperties is never decisive
// — the maxLength verdict must win. Without this rule the evaluator would
// report Unsupported here, because additionalProperties:false appears on
// essentially every real definition in the corpus; that would make the
// evaluator vacuous on real input, never reaching a single supported
// keyword.
func TestEvaluateSupportedKeywordDecidesBeforeAdditionalPropertiesPreempts(t *testing.T) {
	property := `{"name":"value","pointer":"#/properties/value","type":"string","required":false,"maxLength":3}`
	ir := buildIR(property, `"additionalProperties":false,`)

	verdict, err := Evaluate(ir, "Synthetic", Request, []byte(`{"value":"abcd"}`))
	if err != nil {
		t.Fatalf("supported-keyword-decides case returned an error: %v", err)
	}
	if verdict.Status != Invalid || verdict.Keyword != "maxLength" {
		t.Fatalf("verdict = %#v, want invalid/maxLength (additionalProperties must not preempt a decisive supported keyword)", verdict)
	}
}

// TestEvaluateUnlistedPropertyOnClosedObjectIsDeferred is the other half of
// TestEvaluateSupportedKeywordDecidesBeforeAdditionalPropertiesPreempts: the
// same closed definition, but a payload that satisfies every supported keyword
// and adds a property the definition does not list. Nothing supported can
// decide this payload, and additionalProperties:false is the one keyword that
// can — so here it is decisive, and the verdict is Unsupported naming it (a
// recorded deferral to the full schema validator), not Valid. Without this
// case an evaluator that ignored additionalProperties altogether would report
// Valid for a payload the schema rejects and still pass every other test in
// this file, since the precedence case above only ever asks it to stay out of
// the way.
func TestEvaluateUnlistedPropertyOnClosedObjectIsDeferred(t *testing.T) {
	property := `{"name":"value","pointer":"#/properties/value","type":"string","required":false,"maxLength":3}`
	ir := buildIR(property, `"additionalProperties":false,`)

	verdict, err := Evaluate(ir, "Synthetic", Request, []byte(`{"value":"abc","extra":1}`))
	if err != nil {
		t.Fatalf("unlisted-property case returned an error: %v", err)
	}
	if verdict.Status != Unsupported || verdict.Keyword != "additionalProperties" {
		t.Fatalf("verdict = %#v, want unsupported/additionalProperties (an unlisted property on a closed object is deferred, not accepted)", verdict)
	}
}

// TestEvaluateResponseHalfChecksResponseRoot pins that Evaluate can check a
// message's response payload, not only its request. Today's evaluator (and
// every test above) only ever names the request root, which leaves the
// response half untestable even though this evaluator is the only thing that
// checks a response payload against its schema at all; this mirrors the
// corpus's TransactionEventResponse.idTokenInfo shape — a
// constraint that exists only on the response side — with a single
// maxLength property on a synthetic response root.
func TestEvaluateResponseHalfChecksResponseRoot(t *testing.T) {
	property := `{"name":"value","pointer":"#/properties/value","type":"string","required":false,"maxLength":3}`
	ir := responseIR(property)

	t.Run("pass", func(t *testing.T) {
		verdict, err := Evaluate(ir, "Synthetic", Response, []byte(`{"value":"abc"}`))
		if err != nil {
			t.Fatalf("response-half pass path returned an error: %v", err)
		}
		if verdict.Status != Valid {
			t.Fatalf("response-half pass path = %#v, want valid", verdict)
		}
	})
	t.Run("fail", func(t *testing.T) {
		verdict, err := Evaluate(ir, "Synthetic", Response, []byte(`{"value":"abcd"}`))
		if err != nil {
			t.Fatalf("response-half fail path returned an error: %v", err)
		}
		if verdict.Status != Invalid || verdict.Keyword != "maxLength" {
			t.Fatalf("response-half fail path = %#v, want invalid/maxLength", verdict)
		}
	})
}

// rootIR builds a complete IR document — matching the shape
// internal/codegen emits, per its golden dump
// (internal/codegen/testdata/golden/golden_message.json) — around a single
// root definition named SyntheticRequest carrying properties, plus any
// extraDefinitions (used by the $ref-following test to add the definition
// a property refers to).
func rootIR(properties string, extraDefinitions ...string) []byte {
	return buildIR(properties, "", extraDefinitions...)
}

// buildIR is rootIR generalized with a rootExtraFields raw-JSON fragment
// (trailing comma included) spliced into the root definition itself, used
// by the additionalProperties-precedence test to mark the root definition
// closed without adding another helper family.
func buildIR(properties, rootExtraFields string, extraDefinitions ...string) []byte {
	definitions := `{"name":"SyntheticRequest","kind":"root","properties":[` + properties + `],` + rootExtraFields + `"reserved":false,"occurrences":1,"files":["SyntheticRequest.json"]}`
	for _, extra := range extraDefinitions {
		definitions += "," + extra
	}
	return []byte(fmt.Sprintf(`{"definitions":[%s],"messages":[{"name":"Synthetic","block":"test","direction":"CS->CSMS","request":"SyntheticRequest.json","response":"SyntheticResponse.json","requestRoot":"SyntheticRequest","responseRoot":"SyntheticResponse","roots":["SyntheticRequest","SyntheticResponse"],"reach":[],"emit":true}]}`, definitions))
}

func keywordIR(keyword string) []byte {
	return rootIR(keywordProperty(keyword))
}

// responseIR builds a complete IR document like rootIR, but places
// properties on the message's response root (SyntheticResponse) instead of
// its request root (SyntheticRequest, present but property-less), so a test
// can exercise Evaluate against the half it is actually asking for rather
// than the request side every other helper in this file builds.
func responseIR(properties string, extraDefinitions ...string) []byte {
	definitions := `{"name":"SyntheticRequest","kind":"root","properties":[],"reserved":false,"occurrences":1,"files":["SyntheticRequest.json"]},` +
		`{"name":"SyntheticResponse","kind":"root","properties":[` + properties + `],"reserved":false,"occurrences":1,"files":["SyntheticResponse.json"]}`
	for _, extra := range extraDefinitions {
		definitions += "," + extra
	}
	return []byte(fmt.Sprintf(`{"definitions":[%s],"messages":[{"name":"Synthetic","block":"test","direction":"CS->CSMS","request":"SyntheticRequest.json","response":"SyntheticResponse.json","requestRoot":"SyntheticRequest","responseRoot":"SyntheticResponse","roots":["SyntheticRequest","SyntheticResponse"],"reach":[],"emit":true}]}`, definitions))
}

func keywordProperty(keyword string) string {
	switch keyword {
	case "required":
		return `{"name":"value","pointer":"#/properties/value","type":"string","required":true}`
	case "maxLength":
		return `{"name":"value","pointer":"#/properties/value","type":"string","required":false,"maxLength":3}`
	case "minLength":
		return `{"name":"value","pointer":"#/properties/value","type":"string","required":false,"minLength":3}`
	case "minimum":
		return `{"name":"value","pointer":"#/properties/value","type":"integer","required":false,"minimum":2}`
	case "maximum":
		return `{"name":"value","pointer":"#/properties/value","type":"integer","required":false,"maximum":5}`
	case "minItems":
		return `{"name":"value","pointer":"#/properties/value","type":"array","required":false,"minItems":2,"items":{"name":"item","pointer":"#/properties/value/items","type":"integer","required":false}}`
	case "maxItems":
		return `{"name":"value","pointer":"#/properties/value","type":"array","required":false,"maxItems":2,"items":{"name":"item","pointer":"#/properties/value/items","type":"integer","required":false}}`
	case "enum":
		return `{"name":"value","pointer":"#/properties/value","type":"string","required":false,"enum":["Ready","Paused"]}`
	case "pattern":
		return `{"name":"value","pointer":"#/properties/value","type":"string","required":false,"pattern":"^OK-[0-9]+$"}`
	case "exclusiveMinimum":
		return `{"name":"value","pointer":"#/properties/value","type":"integer","required":false,"exclusiveMinimum":{"number":0}}`
	case "exclusiveMaximum":
		return `{"name":"value","pointer":"#/properties/value","type":"integer","required":false,"exclusiveMaximum":{"number":10}}`
	default:
		return `{"name":"value","pointer":"#/properties/value","type":"string","required":false}`
	}
}
