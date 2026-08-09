package main

import "testing"

// TestTrailingContentAfterTheRootObjectIsRejected covers the end of a schema
// file, which a reader that stops at the root object's closing brace never
// looks at. The fixture is a complete, valid schema followed by a second
// JSON object; a reader that reads only as far as the first object accepts
// it and produces a document that describes part of the file, with nothing in
// the output saying the rest was left behind.
//
// trailing_content_clean.json is the same document without the appended
// object and must read cleanly, so the failure below is the trailing content
// rather than anything else the fixture carries.
func TestTrailingContentAfterTheRootObjectIsRejected(t *testing.T) {
	_, err := ReadSchema(fixturePath("schemas", "trailing_content.json"))
	requireExpectedHardFailureContains(t, "trailing content check", err,
		"trailing_content.json", "after the root object")

	_, cleanErr := ReadSchema(fixturePath("schemas", "trailing_content_clean.json"))
	requireImplemented(t, "read of the same document without trailing content", cleanErr)
}

// TestPresentKeywordOfTheWrongJSONShapeIsRejected covers the difference
// between a keyword the schema does not declare and one it declares with a
// value of the wrong JSON shape. Both look alike to a reader that decodes a
// keyword and treats a failed decode as an absence, and each row here is a
// case where that reading loses information silently rather than loudly:
//
//   - type: a union such as ["string", "null"] read as "no type declared"
//     turns the property into the corpus's untyped shape, which is the
//     spelling reserved for a property that genuinely constrains nothing,
//     and drops the constraints declared alongside it;
//   - format and pattern: read as absent, the constraint simply disappears
//     from the generated type;
//   - title: read as absent, the document's own name is lost.
//
// Every fixture except malformed_title.json carries a sibling property whose
// same keyword is well-formed, so a reader that failed on the keyword's mere
// presence cannot pass. Each expectation names the JSON pointer as well as
// the file, because a message naming the file alone leaves the reader of one
// of the corpus's larger documents to find the property themselves.
func TestPresentKeywordOfTheWrongJSONShapeIsRejected(t *testing.T) {
	tests := []struct {
		file string
		want []string
	}{
		{"union_type.json", []string{"union_type.json", "#/properties/value/type"}},
		{"malformed_format.json", []string{"malformed_format.json", "#/properties/value/format"}},
		{"malformed_pattern.json", []string{"malformed_pattern.json", "#/properties/value/pattern"}},
		{"malformed_title.json", []string{"malformed_title.json", "#/title"}},
	}
	for _, test := range tests {
		t.Run(test.file, func(t *testing.T) {
			_, err := ReadSchema(fixturePath("schemas", test.file))
			requireExpectedHardFailureContains(t, "wrong-shaped keyword check for "+test.file, err, test.want...)
		})
	}
}
