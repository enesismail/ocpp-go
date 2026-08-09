package main

import (
	"sort"
	"strings"
	"testing"
)

func stringPtr(s string) *string { return &s }

// TestSyntaxConstraintWithNoSchemaCounterpartIsStricterThanSchema covers the
// divergence a purely numeric constraint comparison cannot see: a string
// property the schema bounds only by length, carried by a Go field that also
// requires the value be a URL. Nothing about the two lengths differs, so a
// comparison that reads only bounds calls the row identical — and the fork
// goes on rejecting every schema-legal value that is not a URL, invisibly.
//
// Both real occurrences are covered, because the two tokens are different
// checks: "url" demands an absolute URL, "uri" accepts a rooted reference as
// well, and reading only one of them would leave the other silently dropped.
func TestSyntaxConstraintWithNoSchemaCounterpartIsStricterThanSchema(t *testing.T) {
	maxLength := 512
	cases := []struct {
		name  string
		token string
	}{
		{"absolute-URL constraint on a length-bounded string", "url"},
		{"URI constraint on a length-bounded string", "uri"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			input := ComparisonInput{
				Go: GoField{
					Name: "RemoteLocation", JSONName: "remoteLocation",
					DeclaredType: "string", WireType: "string",
					Validate: []string{"required", "max=512", test.token},
				},
				Schema: SchemaField{
					Pointer:     "#/definitions/LogParametersType/properties/remoteLocation",
					Type:        stringPtr("string"),
					Required:    true,
					Constraints: Constraints{MaxLength: &maxLength},
				},
				GoPresent:     true,
				SchemaPresent: true,
			}

			result, err := classifyField(input)
			if err != nil {
				t.Fatalf("classify: %v", err)
			}
			if result.Class != OVERRIDE_CANDIDATE {
				t.Fatalf("a %q constraint the schema does not impose classified as %q (rule: %s), want %q",
					test.token, result.Class, result.Rule, OVERRIDE_CANDIDATE)
			}
			if !strings.Contains(result.Rule, test.token) {
				t.Fatalf("rule %q does not name the token that produced the divergence", result.Rule)
			}
			if !strings.Contains(strings.ToLower(result.Rule), "stricter") {
				t.Fatalf("rule %q does not state the direction of the divergence", result.Rule)
			}
			if !strings.Contains(result.Rule, "no format") {
				t.Fatalf("rule %q does not state that the schema side declares nothing, which is the whole finding", result.Rule)
			}
		})
	}
}

// TestSyntaxConstraintMatchingSchemaFormatIsNotADivergence is the negative
// control, and the reason the rule cannot simply report every syntax token it
// sees: the same corpus carries fields where the schema does name the format
// the Go token enforces. Reporting those would bury the two real findings in
// noise and make the count meaningless.
func TestSyntaxConstraintMatchingSchemaFormatIsNotADivergence(t *testing.T) {
	input := ComparisonInput{
		Go: GoField{
			Name: "Location", JSONName: "location",
			DeclaredType: "string", WireType: "string",
			Validate: []string{"required", "uri"},
		},
		Schema: SchemaField{
			Pointer:  "#/properties/location",
			Type:     stringPtr("string"),
			Required: true,
			Format:   stringPtr("uri"),
		},
		GoPresent:     true,
		SchemaPresent: true,
	}

	result, err := classifyField(input)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if result.Class != IDENTICAL {
		t.Fatalf("a uri constraint the schema itself declares classified as %q (rule: %s), want %q",
			result.Class, result.Rule, IDENTICAL)
	}
}

// TestSyntaxConstraintIsNotReadFromElementLevelTokens guards the same
// boundary every other tag reader in this package guards: tokens after "dive"
// constrain a slice's elements, not the slice. Reading one as a constraint on
// the field would invent a divergence on an array property, which has no
// format keyword to compare against in the first place.
func TestSyntaxConstraintIsNotReadFromElementLevelTokens(t *testing.T) {
	input := ComparisonInput{
		Go: GoField{
			Name: "Locations", JSONName: "locations",
			DeclaredType: "[]string", WireType: "[]string",
			Slice: true, ElementType: "string",
			Validate: []string{"omitempty", "dive", "url"},
		},
		Schema: SchemaField{
			Pointer: "#/properties/locations",
			Type:    stringPtr("array"),
		},
		GoPresent:     true,
		SchemaPresent: true,
	}

	if result, matched := compareSemanticConstraints(input); matched {
		t.Fatalf("an element-level url token was read as a constraint on the array itself: %q", result.Rule)
	}
}

// TestSyntaxConstraintOnlyAppliesToStringProperties keeps the rule inside what
// these tokens actually constrain. A schema property of any other type has no
// syntax for a string token to be stricter than.
func TestSyntaxConstraintOnlyAppliesToStringProperties(t *testing.T) {
	input := ComparisonInput{
		Go: GoField{
			Name: "Count", JSONName: "count",
			DeclaredType: "int", WireType: "int",
			Validate: []string{"numeric"},
		},
		Schema: SchemaField{
			Pointer: "#/properties/count",
			Type:    stringPtr("integer"),
		},
		GoPresent:     true,
		SchemaPresent: true,
	}

	if result, matched := compareSemanticConstraints(input); matched {
		t.Fatalf("a syntax token was read against a non-string property: %q", result.Rule)
	}
}

// TestEverySyntaxTokenIsCompared sweeps the whole table rather than the two
// tokens this corpus happens to use. A token present in the table but never
// exercised is a token a future tree could carry with the comparison silently
// dropping it — the exact defect the two real findings came from — so every
// entry is driven end to end: with no schema format it must report a
// divergence, and with a format it names it must not.
func TestEverySyntaxTokenIsCompared(t *testing.T) {
	tokens := make([]string, 0, len(syntaxTokenFormats))
	for token := range syntaxTokenFormats {
		tokens = append(tokens, token)
	}
	sort.Strings(tokens)

	for _, token := range tokens {
		counterparts := syntaxTokenFormats[token]
		input := ComparisonInput{
			Go: GoField{
				Name: "Value", JSONName: "value",
				DeclaredType: "string", WireType: "string",
				Validate: []string{token},
			},
			Schema: SchemaField{
				Pointer: "#/properties/value",
				Type:    stringPtr("string"),
			},
			GoPresent:     true,
			SchemaPresent: true,
		}

		result, matched := compareSemanticConstraints(input)
		if !matched {
			t.Errorf("token %q is in the table but a field carrying it against a format-less schema property reported no divergence", token)
			continue
		}
		if result.Class != OVERRIDE_CANDIDATE {
			t.Errorf("token %q against a format-less schema property classified as %q, want %q", token, result.Class, OVERRIDE_CANDIDATE)
		}

		for _, format := range counterparts {
			withFormat := input
			withFormat.Schema.Format = stringPtr(format)
			if result, matched := compareSemanticConstraints(withFormat); matched {
				t.Errorf("token %q against schema format %q was reported as a divergence: %q", token, format, result.Rule)
			}
		}
	}
}

// TestSyntaxTokenTableExcludesNonSyntaxTokens is the decorrelation guard. The
// table is consulted for every field-level token, so admitting a token that
// bounds a length, states presence, or restricts a value set would turn a row
// the numeric or value-set comparison already settles — or one where the two
// sides agree — into a spurious divergence, and would do it across the whole
// corpus at once.
func TestSyntaxTokenTableExcludesNonSyntaxTokens(t *testing.T) {
	for _, token := range []string{
		"required", "omitempty", "dive", "len", "min", "max", "gte", "gt", "lte", "lt",
		"eq", "ne", "oneof", "unique", "contains", "excludes", "startswith", "endswith",
		"eqfield", "isdefault", "required_with", "required_without",
	} {
		if _, present := syntaxTokenFormats[token]; present {
			t.Errorf("token %q constrains something other than a string's syntax but is in the syntax table", token)
		}
	}
	for _, token := range []string{"url", "uri", "email", "uuid", "hostname", "ip"} {
		if _, present := syntaxTokenFormats[token]; !present {
			t.Errorf("token %q constrains a string's syntax but is missing from the syntax table, so a field carrying it would be compared as if it carried nothing", token)
		}
	}
}
