package canonjson

import (
	"bytes"
	"testing"
)

func TestCanonicalizeSortsNestedObjectKeys(t *testing.T) {
	input := []byte(`{"z":{"b":2,"a":1},"a":{"d":{"y":true,"x":false},"c":3}}`)
	want := []byte(`{"a":{"c":3,"d":{"x":false,"y":true}},"z":{"a":1,"b":2}}`)

	got, err := Canonicalize(input)
	if err != nil {
		t.Fatalf("Canonicalize returned an error for valid nested JSON: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Canonicalize nested keys = %s, want %s", got, want)
	}
}

func TestCanonicalizePreservesArrayOrder(t *testing.T) {
	input := []byte(`{"items":[{"b":2,"a":1},3,{"d":4,"c":5}]}`)
	want := []byte(`{"items":[{"a":1,"b":2},3,{"c":5,"d":4}]}`)

	got, err := Canonicalize(input)
	if err != nil {
		t.Fatalf("Canonicalize returned an error for valid array JSON: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Canonicalize array result = %s, want %s", got, want)
	}
}

func TestCanonicalizeLeavesNumberAndStringBytesUntouched(t *testing.T) {
	input := []byte(`{"text":"\u0061","number":1.00,"negative":-0.0e+0}`)
	want := []byte(`{"negative":-0.0e+0,"number":1.00,"text":"\u0061"}`)

	got, err := Canonicalize(input)
	if err != nil {
		t.Fatalf("Canonicalize returned an error for valid scalar JSON: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Canonicalize scalar result = %s, want %s", got, want)
	}
}

// TestCanonicalizeRejectsInvalidJSON is table-driven and, deliberately,
// includes one well-formed row alongside the malformed ones. Without a
// valid-input row, "invalid input must return an error" would be satisfied by
// an implementation that returns an error unconditionally, for every input,
// valid or not — so the malformed rows alone assert nothing about whether
// malformed input is actually told apart from well-formed input. The valid
// row is the control that proves the error is input-specific: this test fails
// until Canonicalize is implemented, and passes only once the two are
// distinguished.
func TestCanonicalizeRejectsInvalidJSON(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "valid control", input: `{"a":1,"b":[2,3]}`, wantErr: false},
		{name: "trailing garbage", input: `{"a":1}{"b":2}`, wantErr: true},
		{name: "truncated", input: `{"missing":`, wantErr: true},
		{name: "unbalanced brackets", input: `{"a":[1,2}`, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Canonicalize([]byte(tc.input))
			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("Canonicalize(%q) accepted invalid JSON", tc.input)
			case !tc.wantErr && err != nil:
				t.Fatalf("Canonicalize(%q) returned an error for valid JSON: %v", tc.input, err)
			}
		})
	}
}
