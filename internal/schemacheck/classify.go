package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// classifyField derives a field's class and its citing rule from nothing but
// the independently observed facts in input: it never trusts a
// pre-computed answer, because the row's Rule is what a reader relies on to
// know *why* a field differs.
//
// The checks run in a fixed priority order, each one settling the
// classification outright when it fires:
//
//  1. presence — a field only one side declares is its own class, decided
//     before anything else about its shape is inspected.
//  2. a discovered struct-level validator — an OCPP-tree fact the schema has
//     no vocabulary for at all, so it always gets its own class regardless of
//     how well the field otherwise lines up with the schema.
//  3. wire-type mismatch — comparison is against wireType, normalized onto
//     the schema's own type vocabulary (see normalizeWireType), never
//     declaredType and never Go's own type spelling compared as raw text.
//  4. enum value-set drift — within a type both sides agree on, whether the
//     two sides accept the same set of values.
//  5. optionality — whether the field can encode a payload the schema
//     rejects: a property the schema requires going missing, or an optional
//     one arriving as null. Both are settled here, before any mapping
//     question, and neither depends on the field's type family.
//  6. constraint strictness — a stricter-than-schema hand-written bound is an
//     override candidate; a looser one is a bug. A bound only one side
//     declares is a divergence of exactly the same two kinds, not a skip.
//     Numeric/length/element-count bounds and syntax constraints (the
//     validator's own url/uri/email/... tokens against the schema's "format"
//     keyword) are both settled here, in that order.
//  7. declared-type shape — whether the schema-faithful mapping's output
//     (pointer-ness and literal type, including the date-time-specific
//     *types.DateTime rule and the named-enum-type rule) matches what is
//     actually written.
//
// A field that clears every check is IDENTICAL; one whose declared-type
// shape diverges in a way none of the above explains is UNEXPLAINED.
func classifyField(input ComparisonInput) (ClassificationResult, error) {
	if !input.GoPresent && !input.SchemaPresent {
		return ClassificationResult{}, fmt.Errorf("classify field %s: neither the Go tree nor the schema declares it", input.Go.JSONName)
	}

	if input.GoPresent && !input.SchemaPresent {
		return ClassificationResult{
			Class: FORK_BUG,
			Rule:  "Go field not in schema: the hand-written struct declares a field the schema does not authorize",
		}, nil
	}

	if !input.GoPresent && input.SchemaPresent {
		if input.Schema.Required {
			return ClassificationResult{
				Class: FORK_BUG,
				Rule:  "schema-required field missing from Go: the schema marks it required but the hand-written struct has no field for it",
			}, nil
		}
		return ClassificationResult{
			Class: ADDITIVE,
			Rule:  "schema has this optional field; a schema-faithful struct would gain it and nothing that exists today would break",
		}, nil
	}

	// Both sides present from here on.

	if len(input.Go.StructValidators) > 0 {
		return ClassificationResult{
			Class: STRUCT_VALIDATOR,
			Rule:  fmt.Sprintf("cross-field struct validation rule %s has no schema counterpart and needs human adjudication", strings.Join(input.Go.StructValidators, ", ")),
		}, nil
	}

	schemaType := ""
	if input.Schema.Type != nil {
		schemaType = *input.Schema.Type
	}
	wireType := ""
	if input.Go.WireType != "" {
		wireType = normalizeWireType(input.Go.WireType, input.Go.Slice)
	}
	if schemaType != "" && wireType != "" && wireType != schemaType {
		return ClassificationResult{
			Class: FORK_BUG,
			Rule:  fmt.Sprintf("wrong type: hand-written wire type %q (schema vocabulary %q) does not match schema type %q", input.Go.WireType, wireType, schemaType),
		}, nil
	}

	if result, matched := compareEnumValueSets(input); matched {
		return result, nil
	}

	if result, matched := compareRequiredness(input); matched {
		return result, nil
	}

	if result, matched := compareNullEmission(input); matched {
		return result, nil
	}

	if result, matched := compareConstraints(input); matched {
		return result, nil
	}

	if result, matched := compareSemanticConstraints(input); matched {
		return result, nil
	}

	dateTime := input.Schema.Format != nil && *input.Schema.Format == "date-time"

	if schemaType == "string" && dateTime {
		// The global mapping rule for a string+date-time schema property is
		// the wire-type-registered DateTime pointer ("string format:date-time
		// -> *types.DateTime"), never a plain string — the narrowing rule
		// that turns other optional strings into value+omitempty leaves this
		// one as a pointer instead, since the wire-type registry is what
		// carries the date-time formatting behavior. The field only reaches
		// this point with its wireType already agreeing with
		// the schema's "string" (the check above), so the only way left to
		// tell whether the DateTime rule was actually adopted is whether a
		// custom marshaler ran at all: it did when the field's own
		// pointer-stripped declared type is not its own wire type — exactly
		// how the wire-type resolver represents *types.DateTime -> "string" —
		// and it did not when the two already agree, meaning this is a plain
		// string field the narrower rule never touched.
		strippedDeclared := strings.TrimPrefix(input.Go.DeclaredType, "*")
		if strippedDeclared != input.Go.WireType && !input.Go.Pointer {
			// A custom marshaler did run, so this is a date-time type — but
			// the mapping's output is a *pointer* to it, and this field is a
			// value. The rule carries no required/optional split: the
			// narrowing that moves optional fields from pointer to
			// value + omitempty is confined to strings and enums, and a
			// date-time property is explicitly excluded from it, so the
			// pointer stands whether the property is required or not.
			// Swapping a value for a pointer changes the declared type, so
			// this is breaking for the same reason the plain-string case is.
			const expected = "*types.DateTime"
			before := FieldSignature{Present: true, Name: input.Go.Name, DeclaredType: input.Go.DeclaredType}
			after := FieldSignature{Present: true, Name: input.Go.Name, DeclaredType: expected}
			breaking, err := breakingPredicate(SCHEMA_FAITHFUL_CHANGE, before, after)
			if err != nil {
				return ClassificationResult{}, err
			}
			return ClassificationResult{
				Class:    SCHEMA_FAITHFUL_CHANGE,
				Rule:     "string format:date-time -> *types.DateTime (value modelled where the mapping is a pointer)",
				Breaking: breaking,
			}, nil
		}
		if strippedDeclared == input.Go.WireType {
			const expected = "*types.DateTime"
			before := FieldSignature{Present: true, Name: input.Go.Name, DeclaredType: input.Go.DeclaredType}
			after := FieldSignature{Present: true, Name: input.Go.Name, DeclaredType: expected}
			breaking, err := breakingPredicate(SCHEMA_FAITHFUL_CHANGE, before, after)
			if err != nil {
				return ClassificationResult{}, err
			}
			return ClassificationResult{
				Class:    SCHEMA_FAITHFUL_CHANGE,
				Rule:     "string format:date-time -> *types.DateTime",
				Breaking: breaking,
			}, nil
		}
		return ClassificationResult{Class: IDENTICAL}, nil
	}

	if schemaType == "string" && !dateTime {
		const expected = "string"
		actual := input.Go.DeclaredType
		actualBase := strings.TrimPrefix(actual, "*")

		if actual != expected {
			if actualBase == expected {
				// Only pointer-ness diverges from the schema-faithful
				// mapping's output — the narrowing rule (optional
				// strings/enums move from pointer to value + omitempty).
				before := FieldSignature{Present: true, Name: input.Go.Name, DeclaredType: actual}
				after := FieldSignature{Present: true, Name: input.Go.Name, DeclaredType: expected}
				breaking, err := breakingPredicate(SCHEMA_FAITHFUL_CHANGE, before, after)
				if err != nil {
					return ClassificationResult{}, err
				}
				optionality := "required"
				if !input.Schema.Required {
					optionality = "optional"
				}
				return ClassificationResult{
					Class:    SCHEMA_FAITHFUL_CHANGE,
					Rule:     fmt.Sprintf("%s string -> value + omitempty", optionality),
					Breaking: breaking,
				}, nil
			}
			// A named Go type carrying a recovered set of accepted values is
			// how this tree writes a schema enum: the declaration is a
			// scalar alias, which is why the wire type already agreed with
			// the schema's "string" further up, and the values themselves
			// came from the validator that enforces them. The schema-faithful
			// mapping writes the same property as its own named enum type,
			// named after the schema definition it comes from, so adopting it
			// swaps the hand-written type for a different one — a declared-type
			// change, and breaking for that reason.
			//
			// Both sides must actually carry a value set for this to be the
			// explanation, and by this point those sets are known to agree —
			// a drifted one was reported as a contradiction well before the
			// declared type was ever looked at. An empty Go set is the
			// distinguishing fact in the other direction: nothing established
			// that the named type is an enum at all, so such a row keeps
			// falling through to the class reserved for shapes nothing
			// accounts for, rather than being assumed into this rule.
			if len(input.Go.EnumValues) > 0 && len(input.Schema.Enum) > 0 {
				// The generated type's own name is derived from the schema
				// definition this property refers to, which a single field
				// comparison does not carry. The predicate needs only that
				// the two identities are distinct, which they are: one name
				// comes from the schema, the other was chosen by hand.
				const generatedEnumType = "generated named enum type"
				before := FieldSignature{Present: true, Name: input.Go.Name, DeclaredType: actual, EnumType: actualBase}
				after := FieldSignature{Present: true, Name: input.Go.Name, DeclaredType: generatedEnumType, EnumType: generatedEnumType}
				breaking, err := breakingPredicate(SCHEMA_FAITHFUL_CHANGE, before, after)
				if err != nil {
					return ClassificationResult{}, err
				}
				return ClassificationResult{
					Class:    SCHEMA_FAITHFUL_CHANGE,
					Rule:     "string enum -> named enum type",
					Breaking: breaking,
				}, nil
			}

			// The declared type diverges beyond pointer-ness and nothing
			// above explains why.
			return ClassificationResult{Class: UNEXPLAINED}, nil
		}
	}

	if schemaType == "" {
		// A property the schema leaves untyped carries an arbitrary JSON
		// value, and the only Go shape that can hold one is the empty
		// interface. It is already nilable, so an optional such property
		// needs no pointer to express absence — the pointer the optionality
		// rule would otherwise call for has nothing left to add.
		actual := input.Go.DeclaredType
		if actual == emptyInterfaceType || actual == "any" {
			return ClassificationResult{
				Class: IDENTICAL,
				Rule:  "untyped property -> " + emptyInterfaceType,
			}, nil
		}
		before := FieldSignature{Present: true, Name: input.Go.Name, DeclaredType: actual}
		after := FieldSignature{Present: true, Name: input.Go.Name, DeclaredType: emptyInterfaceType}
		breaking, err := breakingPredicate(SCHEMA_FAITHFUL_CHANGE, before, after)
		if err != nil {
			return ClassificationResult{}, err
		}
		return ClassificationResult{
			Class:    SCHEMA_FAITHFUL_CHANGE,
			Rule:     "untyped property -> " + emptyInterfaceType,
			Breaking: breaking,
		}, nil
	}

	if result, matched, err := compareOptionalityShape(input, schemaType); err != nil || matched {
		return result, err
	}

	return ClassificationResult{Class: IDENTICAL}, nil
}

// emptyInterfaceType is the Go shape an untyped schema property maps to, as
// the AST walker spells it.
const emptyInterfaceType = "interface{}"

// compareOptionalityShape decides a field's pointer-ness against the shape the
// mapping gives its schema property, and it does so in that order: the
// faithful shape is derived from the schema's own facts first, and only then
// is the hand-written shape compared against it. Deriving it the other way
// round — reading the Go field and looking for reasons to excuse it — is how a
// pointer field slips past a rule whose exemption was only ever about values.
//
// For an optional property the mapping reaches for a pointer, so absence is
// expressible. Two things move it off that default:
//
//   - the narrowing to value + omitempty, which is confined to strings and
//     enums, whose empty value the schema has no way to mean. A number or a
//     boolean is different: 0 and false are ordinary values the schema admits,
//     so omitempty would drop a legal one and a reader could not tell it from
//     absence. A composite is different again — omitempty has no effect on a
//     struct at all, so a struct value cannot express absence in any form.
//   - a numeric floor that excludes zero, where omitempty becomes unambiguous
//     again because the schema never admits the value it would drop. That
//     branch is real but never fires against this corpus, whose every floor is
//     zero itself.
//
// A required property is never absent, so the mapping models a composite as a
// value; a pointer there expresses a state the schema does not have.
//
// The rules key off the schema's own optionality and the Go field's shape, not
// its type name.
func compareOptionalityShape(input ComparisonInput, schemaType string) (ClassificationResult, bool, error) {
	if input.Go.Slice {
		// A slice is already nilable and already omittable, so it expresses
		// both absence and emptiness on its own; the mapping for an array
		// property is therefore the bare slice, whether the property is
		// required or optional. A pointer wrapped around it adds a second,
		// redundant way to spell absence and changes the declared type.
		if schemaType == "array" && input.Go.Pointer {
			return schemaFaithfulShape(input, strings.TrimPrefix(input.Go.DeclaredType, "*"),
				"array -> slice (a slice already expresses absence, so the mapping wraps it in no pointer)")
		}
		return ClassificationResult{}, false, nil
	}
	switch schemaType {
	case "object", "integer", "number", "boolean":
	default:
		return ClassificationResult{}, false, nil
	}

	if input.Schema.Required {
		if schemaType == "object" && input.Go.Pointer {
			return schemaFaithfulShape(input, strings.TrimPrefix(input.Go.DeclaredType, "*"),
				"required composite -> value (the property is always present, so nothing needs a pointer to express absence)")
		}
		return ClassificationResult{}, false, nil
	}

	if zeroExcludedByFloor(schemaType, input.Schema.Constraints) {
		// The narrowed shape: value + omitempty, unambiguous because the
		// schema never admits the value omitempty would drop.
		if input.Go.Pointer {
			return schemaFaithfulShape(input, strings.TrimPrefix(input.Go.DeclaredType, "*"),
				fmt.Sprintf("optional %s whose schema floor excludes zero -> value + omitempty (omitempty is unambiguous, so the pointer expresses nothing the value cannot)", schemaType))
		}
		return ClassificationResult{}, false, nil
	}

	// The faithful shape is a pointer from here on. Whether that pointer can
	// actually express absence is not asked here: a pointer with nothing to
	// omit it encodes as null, which contradicts the schema outright, and a
	// contradiction is settled before any mapping question — see
	// compareNullEmission, which every row passes through well before this.
	if input.Go.Pointer {
		return ClassificationResult{}, false, nil
	}

	if schemaType == "object" && !omitemptyCanOmit(input.Go) {
		// omitempty has no effect on a struct, so a composite value always
		// encodes and absence is unrepresentable however the field is tagged.
		return schemaFaithfulShape(input, "*"+input.Go.DeclaredType,
			"optional composite -> pointer (a struct value always encodes, and omitempty does not apply to structs, so absence cannot be expressed)")
	}
	if input.Go.Omitempty && omitemptyCanOmit(input.Go) {
		zeroValue := "0"
		if schemaType == "boolean" {
			zeroValue = "false"
		}
		return schemaFaithfulShape(input, "*"+input.Go.DeclaredType,
			fmt.Sprintf("optional %s -> pointer (value + omitempty drops %s, which the schema admits)", schemaType, zeroValue))
	}
	// A value carrying no omitempty always encodes. It cannot express absence
	// either, but emitting a property the schema permits is never invalid, so
	// it contradicts nothing — the same asymmetry compareRequiredness applies
	// to its own converse direction.
	return ClassificationResult{}, false, nil
}

// omitemptyCanOmit reports whether a json omitempty tag can actually take this
// field off the wire. encoding/json omits a field only when its value is the
// empty one for its kind, and it recognizes an empty value for just a few
// kinds: a nil pointer or interface, a nil-or-empty slice or map, an empty
// string, a zero number, and false. A struct has no empty value at all, so a
// struct held by value always marshals — as {} if nothing was set — however it
// is tagged.
//
// Every rule that reasons about omission has to ask this first, in both
// directions: a shape omitempty cannot omit neither lies about a required
// property (it never omits one) nor needs a pointer to stop dropping an
// optional value (it never drops one). Reading the tag alone reports payloads
// that cannot occur.
func omitemptyCanOmit(field GoField) bool {
	// Every test below reads the field's DECLARED Go kind. What a type
	// marshals to is irrelevant here: encoding/json decides emptiness from the
	// value's kind before any MarshalJSON is consulted, so a struct that
	// marshals to a JSON string is still a struct and still never omitted.
	if field.Pointer || field.Slice {
		return true
	}
	declared := strings.TrimPrefix(field.DeclaredType, "*")
	if declared == emptyInterfaceType || declared == "any" ||
		strings.HasPrefix(declared, "map[") || strings.HasPrefix(declared, "[]") {
		return true
	}
	if field.CustomMarshaled {
		// Held by value, and its wire type came from the marshaler registry
		// rather than from its own declaration — so the declared kind is the
		// struct wrapping that marshaler, not the scalar it produces.
		return false
	}
	if _, builtin := goBasicTypeNames[declared]; builtin {
		return true
	}
	// A named type. The walker folds a scalar alias's wireType down to the Go
	// basic type it is declared as, so a wireType that is itself a Go basic
	// type name is the alias case — an empty value exists and omitempty omits
	// it. Anything else is a struct held by value.
	_, alias := goBasicTypeNames[field.WireType]
	return alias
}

// zeroExcludedByFloor reports whether the schema's own lower bound rules the
// zero value out, which is what makes value + omitempty unambiguous for a
// numeric property. Both spellings of an exclusive bound are honoured: the
// draft-06 standalone number and the draft-04 boolean modifier on minimum.
// Only numbers have a floor at all — a boolean's false and a composite's zero
// object are never excluded this way.
func zeroExcludedByFloor(schemaType string, c Constraints) bool {
	if schemaType != "integer" && schemaType != "number" {
		return false
	}
	if c.Minimum != nil && *c.Minimum > 0 {
		return true
	}
	if c.ExclusiveMinimum != nil {
		if c.ExclusiveMinimum.Number != nil && *c.ExclusiveMinimum.Number >= 0 {
			return true
		}
		if c.ExclusiveMinimum.Bool != nil && *c.ExclusiveMinimum.Bool && c.Minimum != nil && *c.Minimum >= 0 {
			return true
		}
	}
	return false
}

// schemaFaithfulShape builds a SCHEMA_FAITHFUL_CHANGE row for a field whose
// declared type the mapping would rewrite, running the change through the
// breaking predicate rather than asserting an answer.
func schemaFaithfulShape(input ComparisonInput, expected, rule string) (ClassificationResult, bool, error) {
	before := FieldSignature{Present: true, Name: input.Go.Name, DeclaredType: input.Go.DeclaredType}
	after := FieldSignature{Present: true, Name: input.Go.Name, DeclaredType: expected}
	breaking, err := breakingPredicate(SCHEMA_FAITHFUL_CHANGE, before, after)
	if err != nil {
		return ClassificationResult{}, false, err
	}
	return ClassificationResult{Class: SCHEMA_FAITHFUL_CHANGE, Rule: rule, Breaking: breaking}, true, nil
}

// breakingPredicate is the complete, mechanical predicate for whether
// adopting a schema-faithful field would change the Go API surface. It is
// meaningful only for SCHEMA_FAITHFUL_CHANGE rows; every other class is
// always non-breaking, even when the same before/after pair would have been
// breaking had it belonged to that class. A change confined to the validate
// tag's text is deliberately absent from every comparison below — it never
// makes a row breaking.
func breakingPredicate(class string, before, after FieldSignature) (bool, error) {
	if class != SCHEMA_FAITHFUL_CHANGE {
		return false, nil
	}
	switch {
	case before.Present != after.Present:
		return true, nil
	case before.Name != after.Name:
		return true, nil
	case before.DeclaredType != after.DeclaredType:
		return true, nil
	case before.ConstructorSignature != after.ConstructorSignature:
		return true, nil
	case before.EnumType != after.EnumType:
		return true, nil
	default:
		return false, nil
	}
}

// goIntegerWireTypes are the Go builtin integer type names that can appear
// as a non-slice field's wireType — int and its explicitly-sized and
// unsigned variants — every one of which carries JSON Schema's "integer" on
// the wire.
var goIntegerWireTypes = map[string]bool{
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
	"uintptr": true,
}

// goNumberWireTypes are the Go builtin floating-point type names, which
// carry JSON Schema's "number" (a broader numeric type than "integer").
var goNumberWireTypes = map[string]bool{
	"float32": true, "float64": true,
}

// schemaTypeVocabulary is the complete "type" vocabulary the draft-06 JSON
// Schema corpus uses. A wireType already spelled this way — the wire-type
// registry's own mapping, e.g. *types.DateTime -> "string" — passes through
// normalizeWireType unchanged, so normalization never re-maps a value that
// was already produced in schema vocabulary.
var schemaTypeVocabulary = map[string]bool{
	"string": true, "integer": true, "number": true, "boolean": true, "array": true, "object": true,
}

// normalizeWireType maps a field's wireType — Go vocabulary exactly as the
// AST walker produces it (a builtin scalar name such as "int" or "bool", a
// same-file struct's bare type name, a resolved-but-unregistered
// package-qualified type reference, or an already-schema-vocabulary string
// the wire-type registry supplied, e.g. "string" for a *types.DateTime
// field) — onto the JSON Schema type vocabulary the schema side reports
// (string, integer, number, boolean, array, object), so the two sides
// compare like for like instead of comparing Go spelling against schema
// spelling as raw text, where only a field that happened to be Go's own
// "string" could ever agree with a schema's "string".
//
// slice is the field's own Go.Slice flag, the authoritative array signal: a
// slice's wireType is its full element-type expression (e.g. "[]string"),
// never the bare word "array", so it has to be passed in rather than
// guessed at from wireType's own spelling.
//
// Every named Go type that is not a recognized builtin scalar — a same-file
// struct name, a package-qualified struct reference, or any other named
// type the walker did not fold into a builtin or a registry entry — maps to
// "object": the schema-side counterpart of a hand-written struct field is
// always a JSON object.
func normalizeWireType(wireType string, slice bool) string {
	if slice {
		return "array"
	}
	if schemaTypeVocabulary[wireType] {
		return wireType
	}
	if goIntegerWireTypes[wireType] {
		return "integer"
	}
	if goNumberWireTypes[wireType] {
		return "number"
	}
	if wireType == "bool" {
		return "boolean"
	}
	return "object"
}

// isNumericWireType reports whether wireType normalizes to a JSON Schema
// numeric type ("integer" or "number") — the fact minMaxSchemaBound needs to
// decide what a Go field's validator.v9 min/max tokens are bounding.
func isNumericWireType(wireType string) bool {
	switch normalizeWireType(wireType, false) {
	case "integer", "number":
		return true
	default:
		return false
	}
}

// compareEnumValueSets compares the set of values the hand-written tree
// actually accepts for a field — recovered from the validator its validate
// tag names, not from the tag's name alone, which says nothing — against the
// set the schema enumerates. Any difference in either direction is a
// contradiction of the schema rather than a deliberate narrowing: a value the
// schema lists and the tree rejects makes the fork refuse a legal message
// (the real case: an upload-log status the schema added and the tree never
// gained), and a value the tree accepts and the schema does not lets an
// illegal one through.
//
// Both sides must declare a value set for there to be anything to compare. A
// field whose tags name no discovered validator has no extractable set, and a
// schema property with no "enum" keyword places no restriction to drift from;
// in neither case is silence evidence of agreement, so neither is reported as
// a divergence.
func compareEnumValueSets(input ComparisonInput) (result ClassificationResult, matched bool) {
	if len(input.Go.EnumValues) == 0 || len(input.Schema.Enum) == 0 {
		return ClassificationResult{}, false
	}
	missing := setDifference(input.Schema.Enum, input.Go.EnumValues)
	extra := setDifference(input.Go.EnumValues, input.Schema.Enum)
	if len(missing) == 0 && len(extra) == 0 {
		return ClassificationResult{}, false
	}

	var parts []string
	if len(missing) > 0 {
		parts = append(parts, fmt.Sprintf("the schema accepts %s, which the hand-written validator rejects", strings.Join(missing, ", ")))
	}
	if len(extra) > 0 {
		parts = append(parts, fmt.Sprintf("the hand-written validator accepts %s, which the schema does not list", strings.Join(extra, ", ")))
	}
	return ClassificationResult{
		Class: FORK_BUG,
		Rule:  "enum value-set drift: " + strings.Join(parts, "; "),
	}, true
}

// setDifference returns the members of a that b does not contain, sorted, so
// the reported drift is deterministic regardless of declaration order.
func setDifference(a, b []string) []string {
	present := make(map[string]bool, len(b))
	for _, value := range b {
		present[value] = true
	}
	var missing []string
	seen := make(map[string]bool, len(a))
	for _, value := range a {
		if present[value] || seen[value] {
			continue
		}
		seen[value] = true
		missing = append(missing, value)
	}
	sort.Strings(missing)
	return missing
}

// hasFieldLevelRequired reports whether the field's own validate tag carries a
// bare "required" token — the one thing that stops a shape which *could* put
// an invalid payload on the wire from ever actually doing so, because
// validation rejects the offending value before the struct is ever encoded.
//
// Both optionality rules read it, and must: they are one rule seen from two
// sides, and a validator that rules out the zero value rules out the nil just
// as completely. Element-level tokens (after "dive") constrain a slice's
// elements, never the field itself, so they cannot supply that guarantee —
// the same exclusion every other tag reader in this package applies.
func hasFieldLevelRequired(field GoField) bool {
	for _, token := range fieldLevelValidateTokens(field.Validate) {
		if token == "required" {
			return true
		}
	}
	return false
}

// compareRequiredness catches the optionality lie: a property the schema
// requires, modelled by a Go field the encoder is free to leave out. A field
// tagged json:",omitempty" is dropped whenever it holds its zero value —
// a nil pointer, an empty string, a zero number — so such a field paired
// against a required property can produce a payload the schema rejects.
//
// Whether the tag can fire at all is a question about the field's shape, not
// about the tag's presence — see omitemptyCanOmit. A shape encoding/json has
// no empty value for is never omitted however it is tagged, so tagging it
// omitempty misleads a reader but takes nothing off the wire, and reporting it
// as a lie would be a finding about a payload that cannot occur.
//
// The one thing that makes an omittable pairing honest is a bare "required"
// token in the validate tag: it rejects the zero value before the struct is
// ever encoded, so the omitempty can never fire on a value that passed
// validation. Element-level tokens (after "dive") constrain the elements,
// not the field, and so cannot supply that guarantee.
//
// The converse — a schema-optional property modelled by a Go field that
// always encodes — is deliberately not reported here: emitting a property the
// schema permits but does not require never produces an invalid payload, so
// it contradicts nothing.
func compareRequiredness(input ComparisonInput) (result ClassificationResult, matched bool) {
	if !input.Schema.Required || !input.Go.Omitempty || !omitemptyCanOmit(input.Go) {
		return ClassificationResult{}, false
	}
	if hasFieldLevelRequired(input.Go) {
		return ClassificationResult{}, false
	}
	shape := "a value type"
	if input.Go.Pointer {
		shape = "a pointer"
	}
	return ClassificationResult{
		Class: FORK_BUG,
		Rule: fmt.Sprintf("optionality lie: the schema marks this property required, but the Go field is %s tagged json omitempty with no required validation, so the encoder omits it at its zero value",
			shape),
	}, true
}

// compareNullEmission is compareRequiredness's mirror. There, a required
// property could go missing; here, an optional one arrives as null.
//
// A pointer with no json omitempty is never left out: encoding/json writes a
// nil as null. Every JSON Schema type names a set of instances that excludes
// null — a nil *int is as invalid against "integer" as a nil *T is against
// "object" — so such a field can put a value on the wire the schema rejects.
// Only a property the schema leaves untyped admits anything at all, and that
// is the one case exempted below.
//
// It is deliberately not a rule about any one type family. Which family a row
// belongs to decides which *mapping* applies to it, and those live in the
// declared-shape stage; whether a nil can reach the wire as null is settled by
// pointer-ness and the tag alone, identically for a *DateTime, a *string, a
// *[]T, a *int and a composite. Asking it inside the family stages meant every
// family that answered first — date-time, string, array — answered without
// ever asking, so the rule was only ever reachable for the families that
// happened to fall through to the end.
//
// It runs where its mirror runs, before the constraint and mapping stages, on
// the priority this classifier applies throughout: a contradiction of the
// schema outranks a difference in how the schema would be modelled.
func compareNullEmission(input ComparisonInput) (result ClassificationResult, matched bool) {
	if input.Schema.Required || !input.Go.Pointer || input.Go.Omitempty {
		return ClassificationResult{}, false
	}
	if hasFieldLevelRequired(input.Go) {
		// Validation rejects the nil before anything is encoded, so the null
		// this rule is about can never reach the wire. The mirror rule takes
		// the same token as its answer, and for the same reason: a field that
		// cannot hold the offending value contradicts nothing.
		return ClassificationResult{}, false
	}
	schemaType := ""
	if input.Schema.Type != nil {
		schemaType = *input.Schema.Type
	}
	if schemaType == "" {
		// An untyped property constrains nothing, so null is a legal instance
		// of it and no contradiction arises.
		return ClassificationResult{}, false
	}
	return ClassificationResult{
		Class: FORK_BUG,
		Rule: fmt.Sprintf("optionality lie: the schema marks this property optional and types it %s, which admits no null, but the Go field is a pointer with no json omitempty, so a nil encodes as null instead of being omitted",
			schemaType),
	}, true
}

// compareConstraints applies the standard validator-tag-to-schema-keyword
// mapping between the Go field's validate tag tokens and the schema's
// constraints. Which schema keyword a bound token maps to is decided by the
// field's own Go kind, matching validator.v9's own kind-sensitive reading of
// those tokens: minItems/maxItems on a slice (v9 bounds element count there),
// minimum/maximum on a numeric field (v9 bounds the value, never a length —
// the real case: IdTokenInfo.ChargingPriority's validate:"min=-9,max=9"), and
// minLength/maxLength otherwise, which is v9's remaining, string case.
//
// Both directions of divergence are reported, and so is a bound only one side
// declares: a hand-written bound the schema does not impose is stricter than
// the schema, and a schema bound the hand-written code does not impose is
// looser than it. Skipping one-sided bounds would silently drop exactly the
// cases this comparison exists to find — a required element-count floor the
// tree never adopted is invisible precisely because there is no Go token to
// compare against.
//
// matched is true only when a divergence was found and decided the
// classification; a field neither side bounds, or whose bounds agree, leaves
// the classification to later checks.
func compareConstraints(input ComparisonInput) (result ClassificationResult, matched bool) {
	tokens := parseValidateNumbers(fieldLevelValidateTokens(input.Go.Validate))

	// Upper before lower, so a field diverging in both directions reports a
	// single, deterministic reason.
	for _, upper := range []bool{true, false} {
		schemaName, schemaValue := schemaBoundForKind(input, upper)
		goBound, goDeclared := effectiveGoBound(tokens, upper)

		switch {
		case goDeclared && schemaValue != nil:
			switch compareBoundToSchema(goBound, *schemaValue, upper) {
			case boundStricter:
				return stricterResult(goBound, schemaName, formatNumber(*schemaValue)), true
			case boundLooser:
				return looserResult(goBound, schemaName, formatNumber(*schemaValue)), true
			}
		case goDeclared && schemaValue == nil:
			return stricterResult(goBound, schemaName, "no bound"), true
		case !goDeclared && schemaValue != nil:
			return ClassificationResult{
				Class: FORK_BUG,
				Rule:  fmt.Sprintf("hand-written code declares no %s bound where the schema sets %s=%s, so it is looser than the schema", schemaName, schemaName, formatNumber(*schemaValue)),
			}, true
		}
	}
	return ClassificationResult{}, false
}

// schemaBoundForKind picks the schema constraint this field's Go kind makes
// the bound tokens mean: an element-count bound for a slice, a numeric value
// bound for a number, and a string-length bound otherwise.
func schemaBoundForKind(input ComparisonInput, upper bool) (schemaName string, schemaValue *float64) {
	switch {
	case input.Go.Slice:
		if upper {
			return "maxItems", intToFloat(input.Schema.Constraints.MaxItems)
		}
		return "minItems", intToFloat(input.Schema.Constraints.MinItems)
	case isNumericWireType(input.Go.WireType):
		if upper {
			return "maximum", input.Schema.Constraints.Maximum
		}
		return "minimum", input.Schema.Constraints.Minimum
	default:
		if upper {
			return "maxLength", intToFloat(input.Schema.Constraints.MaxLength)
		}
		return "minLength", intToFloat(input.Schema.Constraints.MinLength)
	}
}

// goBound is one directional bound as a validate tag declares it: which token
// carried it, its value, and whether that token excludes the value itself
// (validator.v9's gt/lt) rather than admitting it (min/max/gte/lte/len).
type goBound struct {
	token     string
	value     float64
	exclusive bool
}

func (b goBound) describe() string {
	return b.token + "=" + formatNumber(b.value)
}

// upperBoundTokens and lowerBoundTokens are every validate token that
// constrains a field from above / below. Both directions include "len",
// which pins a field to an exact length or value and therefore bounds it
// from both sides at once. Recognizing gt/lt matters as much as recognizing
// the inclusive tokens: without them a field the tree really does bound
// would be reported as bounding nothing at all.
var (
	upperBoundTokens = []string{"max", "lte", "lt", "len"}
	lowerBoundTokens = []string{"min", "gte", "gt", "len"}
)

// exclusiveBoundTokens are the validate tokens that exclude their own value.
var exclusiveBoundTokens = map[string]bool{"gt": true, "lt": true}

// effectiveGoBound reduces every token bounding the field in one direction to
// the single tightest one, which is what the tree actually enforces when more
// than one is present (validator.v9 requires all of them to pass).
func effectiveGoBound(tokens map[string]float64, upper bool) (goBound, bool) {
	candidates := lowerBoundTokens
	if upper {
		candidates = upperBoundTokens
	}
	var tightest goBound
	found := false
	for _, token := range candidates {
		value, ok := tokens[token]
		if !ok {
			continue
		}
		candidate := goBound{token: token, value: value, exclusive: exclusiveBoundTokens[token]}
		if !found || tighterBound(candidate, tightest, upper) {
			tightest, found = candidate, true
		}
	}
	return tightest, found
}

// tighterBound reports whether candidate admits a strictly smaller value set
// than incumbent in the given direction. At equal values an exclusive bound
// is the tighter of the two, since it rules the value itself out.
func tighterBound(candidate, incumbent goBound, upper bool) bool {
	if candidate.value != incumbent.value {
		if upper {
			return candidate.value < incumbent.value
		}
		return candidate.value > incumbent.value
	}
	return candidate.exclusive && !incumbent.exclusive
}

type boundVerdict int

const (
	boundSame boundVerdict = iota
	boundStricter
	boundLooser
)

// compareBoundToSchema places a Go bound against the schema's own, which is
// always inclusive (JSON Schema spells an exclusive bound with a separate
// keyword, and this corpus uses none). An exclusive Go bound at the schema's
// own value is therefore strictly tighter than it, never equal to it.
func compareBoundToSchema(bound goBound, schemaValue float64, upper bool) boundVerdict {
	if bound.value == schemaValue {
		if bound.exclusive {
			return boundStricter
		}
		return boundSame
	}
	if upper == (bound.value < schemaValue) {
		return boundStricter
	}
	return boundLooser
}

func stricterResult(bound goBound, schemaName, schemaValue string) ClassificationResult {
	return ClassificationResult{
		Class: OVERRIDE_CANDIDATE,
		Rule:  fmt.Sprintf("hand-written constraint %s is stricter than schema %s=%s and looks deliberate", bound.describe(), schemaName, schemaValue),
	}
}

func looserResult(bound goBound, schemaName, schemaValue string) ClassificationResult {
	return ClassificationResult{
		Class: FORK_BUG,
		Rule:  fmt.Sprintf("hand-written constraint %s is looser than schema %s=%s", bound.describe(), schemaName, schemaValue),
	}
}

// syntaxTokenFormats is every validator token that constrains a string's
// *syntax* — the shape of the characters, as opposed to its length, its
// numeric value, or its membership of a set — mapped to the JSON Schema
// "format" values that state the same constraint. It is transcribed from the
// validator library's own registered-validator table in the vendored source,
// not from memory, and it is deliberately exhaustive: a token missing from
// here is a Go-side constraint the comparison would drop in silence, which is
// indistinguishable in the output from the two sides agreeing. The real case
// this closes: a string property the schema bounds only by length, carried by
// a Go field additionally tagged "url", which rejects every schema-legal value
// that is not a URL.
//
// A token whose value is nil has no schema counterpart at all — JSON Schema
// has no "format" for it — so declaring it is always a constraint beyond what
// the schema states. Several tokens map to more than one format because the
// dialects spell the same constraint differently (the internationalized
// spellings, and the reference-permitting form of a URI).
//
// Deliberately absent, each for its own reason rather than by oversight:
// presence, comparison and cross-field tokens (required, len, min/max/gte/...,
// the *field family) constrain no syntax; the substring tokens (contains,
// excludes, startswith, endswith) constrain content, and no schema keyword
// states them; "oneof" restricts a value set, whose schema counterpart is
// "enum" and whose comparison therefore belongs to the value-set stage that
// runs before this one; "unique" bounds a collection, not a string; and the
// schema's own "date-time" format has no validator token in this library —
// the hand-written tree expresses it through a custom-marshaling type instead,
// which the declared-shape stage compares directly.
var syntaxTokenFormats = map[string][]string{
	"alpha":            nil,
	"alphanum":         nil,
	"alphaunicode":     nil,
	"alphanumunicode":  nil,
	"numeric":          nil,
	"number":           nil,
	"hexadecimal":      nil,
	"hexcolor":         nil,
	"rgb":              nil,
	"rgba":             nil,
	"hsl":              nil,
	"hsla":             nil,
	"e164":             nil,
	"email":            {"email", "idn-email"},
	"url":              {"uri", "iri"},
	"uri":              {"uri", "uri-reference", "iri", "iri-reference"},
	"urn_rfc2141":      {"uri"},
	"file":             nil,
	"base64":           nil,
	"base64url":        nil,
	"isbn":             nil,
	"isbn10":           nil,
	"isbn13":           nil,
	"eth_addr":         nil,
	"btc_addr":         nil,
	"btc_addr_bech32":  nil,
	"uuid":             {"uuid"},
	"uuid3":            {"uuid"},
	"uuid4":            {"uuid"},
	"uuid5":            {"uuid"},
	"uuid_rfc4122":     {"uuid"},
	"uuid3_rfc4122":    {"uuid"},
	"uuid4_rfc4122":    {"uuid"},
	"uuid5_rfc4122":    {"uuid"},
	"ascii":            nil,
	"printascii":       nil,
	"multibyte":        nil,
	"datauri":          nil,
	"latitude":         nil,
	"longitude":        nil,
	"ssn":              nil,
	"ipv4":             {"ipv4"},
	"ipv6":             {"ipv6"},
	"ip":               {"ipv4", "ipv6"},
	"cidrv4":           nil,
	"cidrv6":           nil,
	"cidr":             nil,
	"tcp4_addr":        nil,
	"tcp6_addr":        nil,
	"tcp_addr":         nil,
	"udp4_addr":        nil,
	"udp6_addr":        nil,
	"udp_addr":         nil,
	"ip4_addr":         {"ipv4"},
	"ip6_addr":         {"ipv6"},
	"ip_addr":          {"ipv4", "ipv6"},
	"unix_addr":        nil,
	"mac":              nil,
	"hostname":         {"hostname", "idn-hostname"},
	"hostname_rfc1123": {"hostname", "idn-hostname"},
	"fqdn":             {"hostname", "idn-hostname"},
	"html":             nil,
	"html_encoded":     nil,
	"url_encoded":      nil,
	"dir":              nil,
}

// compareSemanticConstraints is compareConstraints' counterpart for the
// constraints that carry no number: a validator token restricting what a
// string may *look like*, against the schema's own "format" keyword.
//
// The direction rules are the ones the numeric comparison already applies. A
// syntax constraint the hand-written code declares and the schema does not
// makes the tree reject values the schema admits — stricter than the schema,
// which is what the override-candidate class records. The converse is
// deliberately not reported: "format" is an annotation in the dialects this
// corpus uses, so a schema that names one is not obliging anyone to enforce
// it, and a Go field that does not enforce it emits nothing the schema
// forbids.
//
// Only a string-typed property is considered, matching what these tokens
// actually constrain; the schema's format keyword on any other type has no
// bearing here. A field carrying several syntax tokens reports the first in
// declaration order, so the reason is deterministic and reads as the tag does.
func compareSemanticConstraints(input ComparisonInput) (result ClassificationResult, matched bool) {
	if input.Schema.Type == nil || *input.Schema.Type != "string" {
		return ClassificationResult{}, false
	}
	schemaFormat := ""
	if input.Schema.Format != nil {
		schemaFormat = *input.Schema.Format
	}
	for _, token := range fieldLevelValidateTokens(input.Go.Validate) {
		name, _, _ := strings.Cut(token, "=")
		counterparts, known := syntaxTokenFormats[name]
		if !known {
			continue
		}
		if schemaFormat != "" && containsString(counterparts, schemaFormat) {
			continue
		}
		return ClassificationResult{
			Class: OVERRIDE_CANDIDATE,
			Rule: fmt.Sprintf("hand-written constraint %s restricts the value's syntax where the schema declares %s, so it is stricter than the schema and looks deliberate",
				name, describeSchemaFormat(schemaFormat)),
		}, true
	}
	return ClassificationResult{}, false
}

// describeSchemaFormat spells the schema side of a syntax comparison, which
// is either a named format or the absence of one — and the absence is the
// finding, so it has to be stated rather than left to be inferred from a
// blank.
func describeSchemaFormat(schemaFormat string) string {
	if schemaFormat == "" {
		return "no format"
	}
	return "format=" + schemaFormat
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// parseValidateNumbers extracts every "token=number" entry from a validate
// tag's token list (e.g. "max=20" out of ["required", "max=20"]), keyed by
// the token name. Tokens with no "=" or a non-numeric value (e.g. "dive",
// "required") are not constraint bounds and are skipped rather than erroring
// — the validate tag is a mixed bag and only some of its tokens are bounds.
func parseValidateNumbers(validate []string) map[string]float64 {
	values := make(map[string]float64, len(validate))
	for _, tok := range validate {
		key, raw, ok := strings.Cut(tok, "=")
		if !ok {
			continue
		}
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			continue
		}
		values[key] = n
	}
	return values
}

func intToFloat(v *int) *float64 {
	if v == nil {
		return nil
	}
	f := float64(*v)
	return &f
}

func formatNumber(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}
