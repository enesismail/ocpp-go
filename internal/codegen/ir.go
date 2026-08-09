package main

import (
	"encoding/json"
	"fmt"
)

// DefinitionKind identifies the namespace entry represented by a Definition.
type DefinitionKind string

const (
	DefinitionObject DefinitionKind = "object"
	DefinitionEnum   DefinitionKind = "enum"
	DefinitionRoot   DefinitionKind = "root"
)

// Bound stores an exclusive JSON-Schema bound (exclusiveMinimum or
// exclusiveMaximum) as read from a draft-06 document, where the value is a
// number in its own right rather than a boolean modifier on minimum/maximum.
// This corpus is draft-06 only, and the reader enforces that at the whole
// document's $schema keyword (a draft-04 document is a hard failure naming
// the file and the dialect found, before any property is read) rather than
// per property — so Boolean is never populated on a Property returned from a
// successful read; Number is the only member such a read sets. Boolean stays
// on this type as the explicit negative control: an implementation that
// forgot the whole-document dialect check would otherwise be free to
// misinterpret a draft-04-shaped bound as a draft-06 one, and a test
// asserting Boolean is nil catches exactly that.
type Bound struct {
	Boolean *bool    `json:"boolean,omitempty"`
	Number  *float64 `json:"number,omitempty"`
}

// Property is the complete schema information needed by later mapping and
// rendering stages. Items describes an array element when the property is an
// array, including an object or referenced composite element.
type Property struct {
	Name string `json:"name"`
	// Pointer is the JSON pointer this property was read from, in one of two
	// grammars depending on where the enclosing object lives:
	// "#/definitions/<Name>/properties/<prop>" for a property declared on a
	// named definition, and "#/properties/<prop>" for a property declared on
	// a schema file's root object. Pointer never carries a filename: the same
	// shared definition is read out of many files and deduplicated into one
	// Definition, and a file-qualified pointer would make every file's copy
	// look like a distinct location, turning ordinary, expected dedup into a
	// false structural conflict.
	Pointer              string    `json:"pointer"`
	Type                 string    `json:"type,omitempty"`
	Format               string    `json:"format,omitempty"`
	Required             bool      `json:"required"`
	Enum                 []string  `json:"enum,omitempty"`
	Ref                  string    `json:"ref,omitempty"`
	EnumDefinition       string    `json:"enumDefinition,omitempty"`
	MaxLength            *int      `json:"maxLength,omitempty"`
	MinLength            *int      `json:"minLength,omitempty"`
	Minimum              *float64  `json:"minimum,omitempty"`
	Maximum              *float64  `json:"maximum,omitempty"`
	ExclusiveMinimum     *Bound    `json:"exclusiveMinimum,omitempty"`
	ExclusiveMaximum     *Bound    `json:"exclusiveMaximum,omitempty"`
	MinItems             *int      `json:"minItems,omitempty"`
	MaxItems             *int      `json:"maxItems,omitempty"`
	Pattern              string    `json:"pattern,omitempty"`
	AdditionalProperties *bool     `json:"additionalProperties,omitempty"`
	Items                *Property `json:"items,omitempty"`
	Untyped              bool      `json:"untyped,omitempty"`
}

// Definition is a deduplicated schema object, enum, or message root.
// Properties retain declaration order; Files and other naturally set-like
// data are slices so consumers receive deterministic values in memory.
type Definition struct {
	Name       string         `json:"name"`
	Kind       DefinitionKind `json:"kind"`
	Properties []Property     `json:"properties,omitempty"`
	Values     []string       `json:"values,omitempty"`
	// AdditionalProperties is the definition's own additionalProperties
	// keyword, captured wherever it appears — on a named object definition,
	// on an enum definition, or on a schema file's root object. draft-06
	// also allows a schema in this position; this corpus writes only the
	// boolean form, so a bool is what the IR carries. nil means the keyword
	// was absent, which is not the same as false: false says the schema
	// closes the object, absent says it said nothing, and collapsing the two
	// would report every object in the corpus as closed.
	AdditionalProperties *bool `json:"additionalProperties,omitempty"`
	Reserved             bool  `json:"reserved"`
	// Occurrences counts how many schema files defined this name (before
	// dedup collapsed them into this one Definition) — it is a corpus-wide
	// count of appearances, not the number of messages whose reach set
	// includes this definition. A definition that appears in both the
	// request and the response file of the same message has Occurrences 2
	// (two files defined it) but contributes only once to that message's
	// Reach (Reach is a set: the definition is referenced, not referenced
	// twice). Sorted for deterministic output.
	Occurrences int `json:"occurrences"`
	// Files lists the basenames (not full paths — e.g. "BootNotificationRequest.json",
	// never a directory-qualified path) of every schema file that defined this
	// name, one entry per file counted in Occurrences, sorted for
	// deterministic output.
	Files []string `json:"files,omitempty"`
}

// Message is the IR view of one manifest row. RequestRoot and ResponseRoot
// are the semantic fields naming this message's two root definitions in the
// shared definition index (see Definition, kind root); Roots is the same two
// names as a sorted slice — sorted by name, independent of request/response
// order — for callers that want to range over both without caring which is
// which. Reach contains only referenced definitions, never the message's own
// roots, and is sorted for deterministic output.
type Message struct {
	Name         string   `json:"name"`
	Block        string   `json:"block"`
	Direction    string   `json:"direction"`
	Request      string   `json:"request"`
	Response     string   `json:"response"`
	RequestRoot  string   `json:"requestRoot"`
	ResponseRoot string   `json:"responseRoot"`
	Roots        []string `json:"roots"`
	Reach        []string `json:"reach"`
	Emit         bool     `json:"emit"`
}

// IR is the deterministic hand-off between schema reading and code
// emission. Definitions is sorted by Name and Messages is sorted by Name —
// both independent of manifest declaration order or schema file iteration
// order — so two walks over identical inputs, in separate processes, produce
// byte-identical -dump-ir output.
type IR struct {
	Definitions []Definition `json:"definitions"`
	Messages    []Message    `json:"messages"`
}

// MarshalIR renders the deterministic JSON document -dump-ir writes and the
// golden test compares byte-for-byte, so both share this one rendering path
// instead of the golden test reimplementing its own marshal call. The
// encoding pins encoding/json's default behavior, including its HTML
// escaping of '<', '>' and '&': a direction value written by the manifest
// as "CS->CSMS" is rendered into the dump with its '>' escaped to the
// literal six characters backslash-u-zero-zero-three-e, not left as '>'.
// None of the IR's field values depend on those characters staying
// unescaped, and keeping the standard library's default rather than opting
// out of it with SetEscapeHTML(false) is the pinned choice, recorded here so
// a future change to it is deliberate and re-cuts the golden file rather
// than drifting unnoticed.
func MarshalIR(ir IR) ([]byte, error) {
	rendered, err := json.MarshalIndent(ir, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal IR: %w", err)
	}
	return rendered, nil
}
