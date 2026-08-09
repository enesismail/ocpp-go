// Package ireval evaluates the deliberately small constraint subset used by
// new-only parity fixtures. Its complete supported set is required, type,
// maxLength, minLength, minimum, maximum, minItems, maxItems, enum, and
// pattern. Every other schema keyword — format, exclusiveMinimum,
// exclusiveMaximum, additionalProperties, and any cross-field constraint —
// is out of scope and reported as Unsupported rather than silently ignored
// or silently enforced.
// A property reached through a $ref to another definition is resolved and
// its constraints evaluated as if declared inline; following a $ref is
// structural navigation, not an evaluated keyword, so it stays in scope even
// though the keywords it may lead to are limited to the same supported set.
// A $ref to an object definition also constrains the kind of the value that
// carries it: a present value that is not a JSON object is rejected under
// type, at that property's pointer, before any of the definition's own
// properties are looked at — the same constraint the message root places on
// the payload as a whole. An absent optional property stays unconstrained,
// as any absent optional property does.
//
// Unsupported is a last resort, not a first check: a keyword outside the
// supported set is reported only when it would actually decide the verdict
// for the payload being evaluated — that is, only once every supported
// keyword reached anywhere in the payload has been checked and none of them
// rejected it, and the unsupported keyword is the reason a definitive
// verdict cannot be reached otherwise. Meeting an unsupported keyword
// therefore does not end the evaluation: it is remembered, evaluation
// continues through the remaining properties, nested definitions and array
// elements, and a supported keyword that rejects the payload anywhere —
// including on a property declared after the one carrying the unsupported
// keyword — still decides it. Only a payload that survives every supported
// check it was subjected to, and met at least one unsupported keyword on
// the way, is reported Unsupported; the reported keyword and path are the
// first such keyword met, in evaluation order, so the verdict is
// deterministic. This matters concretely because
// additionalProperties:false is present on nearly every real definition in
// this corpus, but only ever bites a payload that actually adds a property
// the definition does not list: a definition carrying
// additionalProperties:false, evaluated against a payload that violates
// maxLength and adds no unlisted property, reports the maxLength verdict —
// additionalProperties never had a chance to be decisive, because the
// payload never triggered it. Without this rule, Unsupported would fire on
// almost every real definition in the corpus before a single supported
// keyword was ever checked, and the evaluator would never evaluate
// anything.
//
// The evaluator is kept in this internal package rather than in
// ocpp2.0.1_test/ir_eval.go because ocpp2.0.1_test/ today has no non-test
// ".go" file — every one of its 84 files is a "_test.go" external test
// file, which is why `go build ./...` builds nothing under that directory.
// Go does allow a directory to mix an ordinary package with its
// "_test"-suffixed external test package sharing the same directory; that
// split is the normal, common shape for a package with external tests, not
// a restriction this evaluator runs into. The actual reason ir_eval.go does
// not simply live there is that adding it would, for the first time, give
// that directory a real non-test package: it would convert
// ocpp2.0.1_test/ from a directory `go build ./...` skips entirely (nothing
// to build) into one it builds, changing that directory's build surface.
// That is a materially bigger structural change than adding one small,
// self-contained evaluator, so the evaluator gets its own directory
// instead — one that, by living under internal/, also keeps the helper
// un-importable from outside this module.
package ireval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Status is the result of evaluating a payload against one message root.
type Status string

const (
	Valid       Status = "valid"
	Invalid     Status = "invalid"
	Unsupported Status = "unsupported"
)

// Verdict reports a schema-subset decision. Keyword is populated for an
// unsupported keyword or the keyword that first rejected a payload.
type Verdict struct {
	Status  Status
	Keyword string
	Path    string
}

// Half selects which side of a message — its request root or its response
// root — Evaluate checks a payload against. A message's IR entry carries
// both roots (requestRoot and responseRoot), and the two are evaluated
// independently: a request payload is never checked against the response
// root, and vice versa.
type Half string

const (
	Request  Half = "request"
	Response Half = "response"
)

// irBound mirrors internal/codegen's Bound — an exclusive JSON-Schema bound
// carried as a number in its own right rather than a boolean modifier on
// minimum/maximum. Its members are never inspected: exclusiveMinimum and
// exclusiveMaximum are outside this evaluator's supported set, so a
// property carrying either is reported through the Unsupported path without
// this evaluator ever reading Number or Boolean.
type irBound struct {
	Boolean *bool    `json:"boolean,omitempty"`
	Number  *float64 `json:"number,omitempty"`
}

// irProperty mirrors internal/codegen's Property — the JSON shape the
// generator's -dump-ir flag writes for one schema property. This evaluator
// reads IR documents as plain bytes (it does not import internal/codegen,
// which is package main), so the shape is reproduced here rather than
// shared.
type irProperty struct {
	Name    string `json:"name"`
	Pointer string `json:"pointer"`
	Type    string `json:"type,omitempty"`
	// Format is read but never interpreted: format is outside the supported
	// set, so a property carrying one is reported through the Unsupported
	// path rather than checked here. The field is needed to see that the
	// keyword is there at all — a payload whose date-time property holds
	// "not-a-date" satisfies every supported keyword, and reporting it Valid
	// would claim a check this evaluator never made.
	Format         string   `json:"format,omitempty"`
	Required       bool     `json:"required"`
	Enum           []string `json:"enum,omitempty"`
	Ref            string   `json:"ref,omitempty"`
	EnumDefinition string   `json:"enumDefinition,omitempty"`
	MaxLength      *int     `json:"maxLength,omitempty"`
	MinLength      *int     `json:"minLength,omitempty"`
	// Minimum and Maximum are read as json.Number rather than as float64:
	// the bound and the payload value are compared as exact rationals, so
	// the bound has to keep the digits the document spells it with. The
	// JSON shape is unchanged — a JSON number either way.
	Minimum              *json.Number `json:"minimum,omitempty"`
	Maximum              *json.Number `json:"maximum,omitempty"`
	ExclusiveMinimum     *irBound     `json:"exclusiveMinimum,omitempty"`
	ExclusiveMaximum     *irBound     `json:"exclusiveMaximum,omitempty"`
	MinItems             *int         `json:"minItems,omitempty"`
	MaxItems             *int         `json:"maxItems,omitempty"`
	Pattern              string       `json:"pattern,omitempty"`
	AdditionalProperties *bool        `json:"additionalProperties,omitempty"`
	Items                *irProperty  `json:"items,omitempty"`
	Untyped              bool         `json:"untyped,omitempty"`
}

// irDefinition mirrors internal/codegen's Definition.
type irDefinition struct {
	Name                 string       `json:"name"`
	Kind                 string       `json:"kind"`
	Properties           []irProperty `json:"properties,omitempty"`
	Values               []string     `json:"values,omitempty"`
	AdditionalProperties *bool        `json:"additionalProperties,omitempty"`
	Reserved             bool         `json:"reserved"`
	Occurrences          int          `json:"occurrences"`
	Files                []string     `json:"files,omitempty"`
}

// irMessage mirrors internal/codegen's Message.
type irMessage struct {
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

// irDocument mirrors internal/codegen's IR — the top-level -dump-ir shape.
type irDocument struct {
	Definitions []irDefinition `json:"definitions"`
	Messages    []irMessage    `json:"messages"`
}

// rootPointer is the JSON pointer of the payload's own top level — the
// location the property pointers internal/codegen writes for a root object's
// properties ("#/properties/<name>") are relative to.
const rootPointer = "#"

// Evaluate reads a deterministic internal/codegen IR document, selects the
// named message's request or response root according to half, and evaluates
// the payload against the ten supported property constraints. The payload
// must itself be the kind of value the selected root describes: a root
// describing an object rejects a payload that is not one, under the type
// keyword, before any property is looked at.
func Evaluate(irDoc []byte, message string, half Half, payload []byte) (Verdict, error) {
	var doc irDocument
	if err := json.Unmarshal(irDoc, &doc); err != nil {
		return Verdict{}, fmt.Errorf("ireval: decode IR document: %w", err)
	}

	msg, ok := findMessage(doc.Messages, message)
	if !ok {
		return Verdict{}, fmt.Errorf("ireval: message %q not found in IR document", message)
	}

	var rootName string
	switch half {
	case Request:
		rootName = msg.RequestRoot
	case Response:
		rootName = msg.ResponseRoot
	default:
		return Verdict{}, fmt.Errorf("ireval: unknown half %q", half)
	}

	root, ok := findDefinition(doc.Definitions, rootName)
	if !ok {
		return Verdict{}, fmt.Errorf("ireval: root definition %q not found in IR document", rootName)
	}

	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var value interface{}
	if err := dec.Decode(&value); err != nil {
		return Verdict{}, fmt.Errorf("ireval: decode payload: %w", err)
	}
	// A payload is one JSON value and nothing else. Whatever follows the
	// first value — a second value, or bytes that are not JSON at all —
	// means the input is not the document it was taken for, so it is an
	// error rather than a verdict delivered on the part that happened to
	// parse. Only trailing whitespace is allowed, which the decoder skips
	// before reporting the end of the stream.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		if err == nil {
			return Verdict{}, fmt.Errorf("ireval: decode payload: unexpected data after the first JSON value")
		}
		return Verdict{}, fmt.Errorf("ireval: decode payload: unexpected data after the first JSON value: %w", err)
	}

	// The root's own kind is a constraint on the payload as a whole. Without
	// this check a payload that is null, an array, a string or a number is
	// simply an object with no properties as far as the property walk is
	// concerned, so it satisfies every optional property and is reported
	// Valid — most visibly for a root that declares nothing required, where
	// any value at all would pass.
	if describesObject(root) {
		if _, isObject := value.(map[string]interface{}); !isObject {
			return Verdict{Status: Invalid, Keyword: "type", Path: rootPointer}, nil
		}
	}

	e := &evaluator{doc: &doc}
	verdict := e.evaluateObject(root, value)
	if verdict.Status == Invalid {
		return verdict, nil
	}
	// Nothing supported rejected the payload. If an unsupported keyword was
	// met on the way, it is the reason this is not a definitive Valid.
	if e.unsupported != nil {
		return *e.unsupported, nil
	}
	return Verdict{Status: Valid}, nil
}

// evaluator carries the loaded IR document so ref-following can look up a
// referenced definition by name, plus the first unsupported keyword met
// while evaluating one payload.
type evaluator struct {
	doc         *irDocument
	unsupported *Verdict
}

// deferUnsupported records that keyword, at path, is outside the supported
// set and stands between this payload and a definitive Valid. It never
// stops the evaluation and never overwrites an earlier record: a supported
// keyword may still reject the payload afterwards and win, and if none
// does, the first keyword recorded is the one reported.
func (e *evaluator) deferUnsupported(keyword, path string) {
	if e.unsupported == nil {
		e.unsupported = &Verdict{Status: Unsupported, Keyword: keyword, Path: path}
	}
}

// evaluateObject checks value — expected to be a JSON object — against def's
// declared properties, in declaration order, then against def's own
// additionalProperties keyword. additionalProperties is outside the
// supported keyword set: a payload adding a property the definition does not
// list, on a definition that closes itself, is recorded as an unsupported
// keyword rather than silently accepted or silently rejected, and decides
// the verdict only if nothing supported rejects the payload.
//
// Only an Invalid return is decisive. A Valid return says that nothing here
// rejected the payload — an unsupported keyword may still have been recorded
// on the evaluator, which the caller resolves once the whole payload has
// been walked.
func (e *evaluator) evaluateObject(def irDefinition, value interface{}) Verdict {
	obj, _ := value.(map[string]interface{})

	for _, prop := range def.Properties {
		propValue, present := obj[prop.Name]
		if !present {
			if prop.Required {
				return Verdict{Status: Invalid, Keyword: "required", Path: prop.Pointer}
			}
			continue
		}

		if verdict, decisive := e.evaluateProperty(prop, propValue); decisive {
			return verdict
		}
	}

	if def.AdditionalProperties != nil && !*def.AdditionalProperties {
		allowed := make(map[string]struct{}, len(def.Properties))
		for _, prop := range def.Properties {
			allowed[prop.Name] = struct{}{}
		}

		keys := make([]string, 0, len(obj))
		for key := range obj {
			keys = append(keys, key)
		}
		sort.Strings(keys)

		for _, key := range keys {
			if _, ok := allowed[key]; !ok {
				e.deferUnsupported("additionalProperties", "#/"+key)
				break
			}
		}
	}

	return Verdict{Status: Valid}
}

// evaluateProperty checks a single present property value against prop's
// supported constraints, in an order that lets any supported keyword decide
// the verdict before an unsupported one (exclusiveMinimum, exclusiveMaximum,
// format) is ever consulted. decisive reports whether verdict is final: it is true
// only for a rejection, so an unsupported keyword — recorded on the
// evaluator, not returned — leaves the caller free to keep evaluating
// sibling properties, any one of which may still reject the payload.
func (e *evaluator) evaluateProperty(prop irProperty, value interface{}) (verdict Verdict, decisive bool) {
	if prop.Type != "" && !typeMatches(prop.Type, value) {
		return Verdict{Status: Invalid, Keyword: "type", Path: prop.Pointer}, true
	}

	enumValues := prop.Enum
	if len(enumValues) == 0 && prop.Ref != "" {
		if target, ok := e.doc.definitionByName(refName(prop.Ref)); ok && target.Kind == "enum" {
			enumValues = target.Values
		}
	}
	if len(enumValues) > 0 {
		s, ok := value.(string)
		if !ok || !containsString(enumValues, s) {
			return Verdict{Status: Invalid, Keyword: "enum", Path: prop.Pointer}, true
		}
	}

	if prop.Pattern != "" {
		s, ok := value.(string)
		if !ok || !matchesPattern(prop.Pattern, s) {
			return Verdict{Status: Invalid, Keyword: "pattern", Path: prop.Pointer}, true
		}
	}

	if prop.MinLength != nil || prop.MaxLength != nil {
		if s, ok := value.(string); ok {
			length := utf8.RuneCountInString(s)
			if prop.MinLength != nil && length < *prop.MinLength {
				return Verdict{Status: Invalid, Keyword: "minLength", Path: prop.Pointer}, true
			}
			if prop.MaxLength != nil && length > *prop.MaxLength {
				return Verdict{Status: Invalid, Keyword: "maxLength", Path: prop.Pointer}, true
			}
		}
	}

	// A bound applies to numbers only, so a value of any other kind passes
	// it untouched, as it does under the schema itself. Value and bound are
	// both compared as the exact rationals their lexemes spell.
	if prop.Minimum != nil || prop.Maximum != nil {
		if n, isNumber := value.(json.Number); isNumber {
			if v, readable := exactNumber(n); readable {
				if prop.Minimum != nil {
					if bound, boundReadable := exactNumber(*prop.Minimum); boundReadable && v.Cmp(bound) < 0 {
						return Verdict{Status: Invalid, Keyword: "minimum", Path: prop.Pointer}, true
					}
				}
				if prop.Maximum != nil {
					if bound, boundReadable := exactNumber(*prop.Maximum); boundReadable && v.Cmp(bound) > 0 {
						return Verdict{Status: Invalid, Keyword: "maximum", Path: prop.Pointer}, true
					}
				}
			}
		}
	}

	if prop.MinItems != nil || prop.MaxItems != nil || prop.Items != nil {
		if arr, ok := value.([]interface{}); ok {
			if prop.MinItems != nil && len(arr) < *prop.MinItems {
				return Verdict{Status: Invalid, Keyword: "minItems", Path: prop.Pointer}, true
			}
			if prop.MaxItems != nil && len(arr) > *prop.MaxItems {
				return Verdict{Status: Invalid, Keyword: "maxItems", Path: prop.Pointer}, true
			}
			if prop.Items != nil {
				for _, item := range arr {
					if itemVerdict, itemDecisive := e.evaluateProperty(*prop.Items, item); itemDecisive {
						return itemVerdict, true
					}
				}
			}
		}
	}

	// A $ref to an object definition constrains the value carrying it before
	// it constrains anything inside that value: the definition describes an
	// object, so a value of any other kind fails here, on type, at this
	// property's own pointer — the same check the root's kind gets against
	// the payload as a whole, applied one level down. Without it a
	// non-object reaches the property walk, which reads it as an object with
	// no properties: every optional property is vacuously satisfied, and a
	// definition whose properties are all optional (ModemType, reached
	// through ChargingStationType.modem, is one) reports the payload Valid.
	// A definition that does declare a required property would still reject
	// such a value, but under a misleading required verdict naming a
	// property the value was never in a position to carry.
	//
	// Array elements are evaluated through this same function, so an element
	// whose items subschema refs an object definition is checked here too,
	// at the element subschema's pointer.
	if prop.Ref != "" {
		if target, ok := e.doc.definitionByName(refName(prop.Ref)); ok && describesObject(target) {
			if _, isObject := value.(map[string]interface{}); !isObject {
				return Verdict{Status: Invalid, Keyword: "type", Path: prop.Pointer}, true
			}
			if nested := e.evaluateObject(target, value); nested.Status == Invalid {
				return nested, true
			}
		}
	}

	// Every supported keyword on this property has now been checked and
	// none of them rejected the value. The property carries a constraint
	// this evaluator does not implement, so this property cannot return a
	// definitive Valid — but a sibling, or a later element, may still
	// reject the payload outright, so the keyword is recorded and the walk
	// goes on.
	if prop.ExclusiveMinimum != nil {
		e.deferUnsupported("exclusiveMinimum", prop.Pointer)
	}
	if prop.ExclusiveMaximum != nil {
		e.deferUnsupported("exclusiveMaximum", prop.Pointer)
	}
	// format narrows a string beyond anything the supported keywords say —
	// a date-time property's value is a string of the right length and
	// shape whether or not it spells a date — so a present property
	// carrying one cannot be reported Valid on the strength of the
	// supported checks alone.
	if prop.Format != "" {
		e.deferUnsupported("format", prop.Pointer)
	}

	return Verdict{Status: Valid}, false
}

// describesObject reports whether def constrains a JSON object: a message
// root and a named object definition both do, an enum definition does not.
func describesObject(def irDefinition) bool {
	return def.Kind == "root" || def.Kind == "object"
}

// exactNumber converts a JSON number's lexeme to the rational it spells,
// with no rounding: comparing through float64 would make 1e-400 and 0 the
// same value, and two integers 1 apart the same value once past float64's
// exact-integer range, so a payload could satisfy a bound it violates, or
// be taken for an integer it is not. The boolean is false only for a lexeme
// big.Rat cannot read, which neither encoding/json's decoder nor a JSON
// number in an IR document produces.
func exactNumber(n json.Number) (*big.Rat, bool) {
	return new(big.Rat).SetString(n.String())
}

func (doc *irDocument) definitionByName(name string) (irDefinition, bool) {
	return findDefinition(doc.Definitions, name)
}

func findMessage(messages []irMessage, name string) (irMessage, bool) {
	for _, m := range messages {
		if m.Name == name {
			return m, true
		}
	}
	return irMessage{}, false
}

func findDefinition(definitions []irDefinition, name string) (irDefinition, bool) {
	for _, d := range definitions {
		if d.Name == name {
			return d, true
		}
	}
	return irDefinition{}, false
}

func containsString(values []string, s string) bool {
	for _, v := range values {
		if v == s {
			return true
		}
	}
	return false
}

// refName extracts the definition name from a JSON pointer ref such as
// "#/definitions/StatusEnumType".
func refName(ref string) string {
	if idx := strings.LastIndex(ref, "/"); idx >= 0 {
		return ref[idx+1:]
	}
	return ref
}

func matchesPattern(pattern, value string) bool {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false
	}
	return re.MatchString(value)
}

// typeMatches reports whether value's decoded JSON kind matches want, one of
// the JSON-Schema primitive type names. An empty want (no type declared on
// the property) is unconstrained.
func typeMatches(want string, value interface{}) bool {
	switch want {
	case "":
		return true
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "integer":
		n, ok := value.(json.Number)
		if !ok {
			return false
		}
		// Draft-06 calls a number an integer when its value has no
		// fractional part, however it is spelled, so 1.0 and 1e2 are
		// integers and 1.5 is not. The test is made on the exact value
		// the lexeme spells: rounding it to float64 first would report
		// 1e-400 as the integer 0.
		v, exact := exactNumber(n)
		return exact && v.IsInt()
	case "number":
		_, ok := value.(json.Number)
		return ok
	case "array":
		_, ok := value.([]interface{})
		return ok
	case "object":
		_, ok := value.(map[string]interface{})
		return ok
	case "null":
		return value == nil
	default:
		return true
	}
}
