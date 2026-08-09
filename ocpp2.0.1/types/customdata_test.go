package types

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCustomDataRoundTripsThreeVendorExtensions(t *testing.T) {
	input := []byte(`{"vendorId":"acme","com.acme.alpha":1.00,"com.acme.beta":"\u0061","com.acme.gamma":{"enabled":true}}`)

	var got CustomData
	require.NoError(t, json.Unmarshal(input, &got), "customData with three extension keys must unmarshal")
	require.Equal(t, "acme", got.VendorId)
	require.Equal(t, json.RawMessage(`1.00`), got.Extensions["com.acme.alpha"])
	// The wire bytes spell this value as the JSON escape \u0061, so capture must
	// hand back that escape spelling, not the "a" it decodes to.
	require.Equal(t, json.RawMessage(`"\u0061"`), got.Extensions["com.acme.beta"])
	require.Equal(t, json.RawMessage(`{"enabled":true}`), got.Extensions["com.acme.gamma"])
	_, hasVendorIDEntry := got.Extensions["vendorId"]
	require.False(t, hasVendorIDEntry, "vendorId is the dedicated field, not an extension entry")

	output, err := json.Marshal(got)
	require.NoError(t, err, "customData with three extension keys must marshal")
	require.JSONEq(t, string(input), string(output))
	// The input's keys are already in the contract's declared order
	// (vendorId, then extensions sorted alphabetically), so a correct
	// marshal reproduces it byte-for-byte. This is a stronger check than
	// JSONEq above: JSONEq treats "1.00" and "1" as the same JSON value,
	// which would hide a lexeme-preservation regression; comparing the raw
	// bytes catches it, pinning that a JSON number lexeme such as 1.00
	// survives the round trip as 1.00, not a re-formatted 1, and that the
	// string escape \u0061 survives as \u0061, not as the "a" it stands for.
	require.Equal(t, string(input), string(output), "customData marshal must preserve extension value lexemes exactly")
}

func TestCustomDataMarshallingOrdersExtensionKeysDeterministically(t *testing.T) {
	data := CustomData{
		VendorId: "acme",
		Extensions: map[string]json.RawMessage{
			"com.acme.zeta":  json.RawMessage(`3`),
			"com.acme.alpha": json.RawMessage(`1`),
			"com.acme.mu":    json.RawMessage(`2`),
		},
	}

	first, err := json.Marshal(data)
	require.NoError(t, err, "first customData marshal must succeed")
	second, err := json.Marshal(data)
	require.NoError(t, err, "second customData marshal must succeed")
	require.Equal(t, string(first), string(second), "customData marshal output must be byte-stable")
	require.Equal(t, `{"vendorId":"acme","com.acme.alpha":1,"com.acme.mu":2,"com.acme.zeta":3}`, string(first))
}

func TestCustomDataRequiresVendorID(t *testing.T) {
	err := Validate.Struct(CustomData{})
	require.Error(t, err, "customData without vendorId must fail the required validation")
}

func TestCustomDataWithVendorIDPassesValidation(t *testing.T) {
	err := Validate.Struct(CustomData{VendorId: "acme"})
	require.NoError(t, err, "customData with a non-empty vendorId must pass validation")
}

func TestCustomDataVendorIDAtMaxLengthPassesValidation(t *testing.T) {
	err := Validate.Struct(CustomData{VendorId: strings.Repeat("v", 255)})
	require.NoError(t, err, "a 255-character vendorId is exactly at the schema's bound and must pass validation")
}

func TestCustomDataVendorIDBeyondMaxLengthFailsValidation(t *testing.T) {
	err := Validate.Struct(CustomData{VendorId: strings.Repeat("v", 256)})
	require.Error(t, err, "a 256-character vendorId is one past the schema's 255-character bound and must fail validation")
}

func TestNilCustomDataIsOmittedFromContainingPayload(t *testing.T) {
	var envelope struct {
		CustomData *CustomData `json:"customData,omitempty"`
	}

	encoded, err := json.Marshal(envelope)
	require.NoError(t, err)
	require.Equal(t, `{}`, string(encoded), "nil customData must not emit null or an empty object")
}

func TestNonNilCustomDataIsEmittedFromContainingPayload(t *testing.T) {
	// The positive counterpart to TestNilCustomDataIsOmittedFromContainingPayload:
	// that test alone would pass just as well if customData were never
	// emitted at all, which is not the contract. This pins the other half.
	envelope := struct {
		CustomData *CustomData `json:"customData,omitempty"`
	}{
		CustomData: &CustomData{VendorId: "x"},
	}

	encoded, err := json.Marshal(envelope)
	require.NoError(t, err)
	require.Equal(t, `{"customData":{"vendorId":"x"}}`, string(encoded), "a non-nil customData must be emitted under its field name")
}

func TestCustomDataMarshalAppliesDefaultJSONEscaping(t *testing.T) {
	// Documents a deliberate contract choice, so a future implementation
	// change to it is a conscious one rather than an accidental drift: an
	// extension value carrying HTML-significant bytes ('<', '>', '&') comes
	// back through Marshal escaped, matching what a plain call to
	// encoding/json's default (SetEscapeHTML(true)) marshaler would produce
	// for the same string. This is the same default-escaping choice
	// internal/codegen/ir.go's MarshalIR pins for the IR dump, recorded here
	// so the two don't drift into disagreeing silently. It is also the one
	// case where CustomData's otherwise byte-exact lexeme preservation
	// (TestCustomDataRoundTripsThreeVendorExtensions) does not hold: the
	// escaped output is not byte-identical to the unescaped input.
	input := []byte(`{"vendorId":"acme","com.acme.delta":"a<b&c"}`)

	var got CustomData
	require.NoError(t, json.Unmarshal(input, &got))
	require.Equal(t, json.RawMessage(`"a<b&c"`), got.Extensions["com.acme.delta"], "unmarshal must capture the extension's raw bytes unaltered")

	output, err := json.Marshal(got)
	require.NoError(t, err)
	require.Equal(t, `{"vendorId":"acme","com.acme.delta":"a\u003cb\u0026c"}`, string(output), "marshal must apply encoding/json's default HTML escaping to extension bytes")
}
