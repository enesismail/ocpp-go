package types

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// vendorIDProperty is the one property name CustomData owns outright. It is
// reserved: it is carried by the VendorId field and never by Extensions, on
// both sides of the round trip.
const vendorIDProperty = "vendorId"

// CustomData carries the required vendor identifier and extension properties
// not defined by the base OCPP schema. Extension values retain their JSON
// representation so a later implementation can round-trip them without
// converting numbers or strings through Go values.
//
// "vendorId" is reserved to the VendorId field and is not an extension
// property: unmarshaling routes it to VendorId rather than into Extensions,
// and marshaling emits VendorId's value under that name, skipping any
// Extensions entry stored under the same key. A single JSON object cannot
// meaningfully carry the name twice — a decoder resolving a duplicate key
// keeps only one of the two values, so emitting both would make the vendor
// identifier a receiver's parsing detail rather than this value's own field.
type CustomData struct {
	// VendorId carries the schema's own 255-character bound on
	// CustomDataType.vendorId (internal/codegen's custom_data.json fixture
	// records this from the OCA schema shape).
	VendorId string `json:"vendorId" validate:"required,max=255"`
	// Extensions holds the extension properties, keyed by property name.
	// An entry under the reserved name "vendorId" is not an extension and
	// is never emitted; the VendorId field is the only source of that
	// property on the wire.
	Extensions map[string]json.RawMessage `json:"-"`
}

// MarshalJSON writes vendorId first, followed by the extension properties in
// ascending key order, so the output is byte-stable across repeated calls.
// An Extensions entry stored under the reserved name "vendorId" is skipped:
// the VendorId field is the only source of that property, so the output
// carries the name exactly once whatever Extensions holds.
// An extension entry holding no bytes at all (a nil or empty
// json.RawMessage) marshals as null. Every other entry must hold exactly one
// well-formed JSON value, and marshaling reports an error naming the
// property when it does not: the stored bytes become the property's value by
// being written into the object as they are, so bytes spelling more than one
// value — or no complete value at all — would land in the object's own
// structure rather than under the property, adding members the caller never
// named or leaving the output invalid JSON.
// A checked entry's bytes are written back unchanged; encoding/json still
// runs its usual compaction and HTML escaping over the bytes this
// method returns, exactly as it would for any other MarshalJSON
// implementation, so an extension value carrying '<', '>' or '&' comes back
// escaped even though it is stored unescaped, and a lexeme that was already
// valid JSON (a number formatted as 1.00, or a string already carrying a
// unicode escape) passes through unchanged.
func (d CustomData) MarshalJSON() ([]byte, error) {
	vendorID, err := json.Marshal(d.VendorId)
	if err != nil {
		return nil, err
	}

	keys := make([]string, 0, len(d.Extensions))
	for key := range d.Extensions {
		if key == vendorIDProperty {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var buf bytes.Buffer
	buf.WriteByte('{')
	buf.WriteString(`"` + vendorIDProperty + `":`)
	buf.Write(vendorID)

	for _, key := range keys {
		encodedKey, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		buf.WriteByte(',')
		buf.Write(encodedKey)
		buf.WriteByte(':')
		if raw := d.Extensions[key]; len(raw) > 0 {
			if !json.Valid(raw) {
				return nil, fmt.Errorf("extension property %q does not hold a single JSON value", key)
			}
			buf.Write(raw)
		} else {
			buf.WriteString("null")
		}
	}
	buf.WriteByte('}')

	return buf.Bytes(), nil
}

// UnmarshalJSON captures the reserved vendorId property into its dedicated
// field and every other property into Extensions, keyed by property name,
// each holding the raw
// bytes exactly as they appeared on the wire. Decoding into json.RawMessage
// preserves the original lexeme of every extension value, including number
// formatting (1.00 stays 1.00) and string escape sequences (a unicode escape
// stays a unicode escape rather than decoding to the character it stands
// for), so a later marshal reproduces them unchanged.
func (d *CustomData) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	var vendorID string
	if value, ok := raw[vendorIDProperty]; ok {
		if err := json.Unmarshal(value, &vendorID); err != nil {
			return err
		}
		delete(raw, vendorIDProperty)
	}

	extensions := make(map[string]json.RawMessage, len(raw))
	for key, value := range raw {
		extensions[key] = value
	}

	d.VendorId = vendorID
	d.Extensions = extensions

	return nil
}
