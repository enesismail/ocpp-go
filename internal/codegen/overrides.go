package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"
)

// OverrideConfig is the complete public configuration document. The two
// sections use the same governed row shape; the allowlist is empty until a
// real schema difference has a recorded owner and source.
type OverrideConfig struct {
	Version        string        `yaml:"version"`
	TagOverrides   []OverrideRow `yaml:"tagOverrides"`
	DedupAllowlist []OverrideRow `yaml:"dedupAllowlist"`
}

// OverrideTarget identifies the schema property to which a row applies.
// Roots and shared definitions deliberately share this key shape.
type OverrideTarget struct {
	Definition string
	Property   string
}

// OverrideMapping is the information produced by the emitter mapping seam
// (MapProperty/BaseTagSet, see emitter_stub.go) that governance checks
// need after schema reachability has been established. GoKind carries the
// same vocabulary FieldMapping.GoType does -- "int", "string",
// "[]Entry" -- and never a JSON-Schema type name such as "integer": a
// governance check judges a property's kind by the one spelling the emitter
// actually produces, so it never has to translate between a schema-type
// name and a Go one. FieldName is fed from that same mapping result's
// FieldMapping.FieldName -- the generated Go struct field's own exported
// name ("Interval", "ChargingPriority") -- so a report naming where a row
// landed reads it from the one mapping every other governance check already
// consults, instead of re-deriving it (or worse, re-title-casing the schema
// property name) a second way.
type OverrideMapping struct {
	BaseTags  []string
	GoKind    string
	FieldName string
}

// OverrideMappings supplies second-pass inputs without asking configuration
// loading to invoke an unfinished emitter. Production code can populate it
// from the same mapping result used to render a field. A nil OverrideMappings
// runs the loader's first pass alone -- every check that needs nothing but
// the row itself and the schema it targets. The second pass, which judges a
// row against the Go kind and base tag set its property maps to, needs a
// mapped Go kind and base tag set for every (definition, property) pair, and
// those exist only once the emitter's mapping has run. The two passes are
// order-free by design, so this loader can land, and be exercised end to
// end, before that mapping seam exists; see LoadOverrides.
//
// A non-nil OverrideMappings is a promise, not a best-effort hint: every row
// LoadOverrides resolves in the second pass must have an entry in the map
// keyed by its own (definition, property) target. A target absent from a
// non-nil map is a caller error -- an incomplete production mapping result,
// not a legitimate "no base tags" reading -- so LoadOverrides hard-fails
// naming the missing target rather than silently treating the absence as
// OverrideMapping's zero value (no base tags, no Go kind, no field name).
type OverrideMappings map[OverrideTarget]OverrideMapping

// LoadOverrides is the loader and validation seam for the tag-override
// file. It decodes the exact document shape, resolves both root and shared
// definition targets in ir, and -- when mappings is non-nil -- additionally
// applies the second-pass base-tag and Go-kind checks that need the
// emitter's mapping output.
//
// registeredValidators names every validator name the emitted tree itself
// registers beyond the fixed tag grammar (required, omitempty, max=, min=,
// gte=, lte=, dive), which is how a row may legitimately name a tag the
// grammar alone does not cover. It is a global set, independent of any single
// (definition, property) pair, which is why it travels as a parameter of
// its own rather than as a field on OverrideMappings, whose every other
// field is scoped to one target.
//
// The file itself must hold exactly one YAML document and declare a
// recognized version (see checkOverrideConfigVersion): both are checked
// immediately after decoding, before any row is read, since a second
// document or an unrecognized version puts every row that follows in
// question -- a second document's rows would otherwise load silently
// unread, and a row checked against ir without knowing which schema
// generation ir itself came from could pass by accident.
//
// Validation runs in two passes. The first -- record completeness, date
// parseability, rule vocabulary, tighten's from requirement, tag grammar,
// duplicate keys, unknown YAML fields, and schema reachability -- needs
// nothing but the decoded document and ir, so it always runs, and succeeds
// on its own when mappings is nil. The second -- from-token presence and
// direction, field-kind applicability, and duplicate-family detection --
// needs the base tag set and mapped Go kind mappings carries, so it runs
// only when mappings is non-nil.
func LoadOverrides(path string, ir IR, mappings OverrideMappings, registeredValidators []string) (OverrideConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return OverrideConfig{}, fmt.Errorf("load overrides %s: %w", path, err)
	}

	var config OverrideConfig
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if decodeErr := decoder.Decode(&config); decodeErr != nil {
		return OverrideConfig{}, wrapOverridesDecodeError(path, data, decodeErr)
	}
	// The override table is one document, and only the first one is
	// decoded. A second document -- appended by a merge, or left behind by
	// an edit -- would otherwise go unread along with every row it
	// declares, silently dropping them rather than holding them to the same
	// strict-field check the first document above was just held to.
	var extraDocument yaml.Node
	if err := decoder.Decode(&extraDocument); !errors.Is(err, io.EOF) {
		if err != nil {
			return OverrideConfig{}, wrapOverridesDecodeError(path, data, err)
		}
		return OverrideConfig{}, fmt.Errorf("load overrides %s: the file holds more than one YAML document; the override table is a single document", path)
	}

	if err := checkOverrideConfigVersion(path, config.Version); err != nil {
		return OverrideConfig{}, err
	}

	// Version is excluded from decoding (OverrideRow.Version carries
	// yaml:"-") because it is a document-level fact, not a per-row one; it
	// is populated here so every loaded row still carries it, which the
	// duplicate-key check below and the report both need.
	for i := range config.TagOverrides {
		config.TagOverrides[i].Version = config.Version
	}
	for i := range config.DedupAllowlist {
		config.DedupAllowlist[i].Version = config.Version
	}

	if err := validateTagOverrideRows(path, config.TagOverrides, ir, registeredValidators); err != nil {
		return OverrideConfig{}, err
	}
	if err := validateDedupAllowlistRows(path, config.DedupAllowlist, ir); err != nil {
		return OverrideConfig{}, err
	}
	if err := checkDuplicateOverrideKeys(path, config.TagOverrides); err != nil {
		return OverrideConfig{}, err
	}

	sortOverrideRows(config.TagOverrides)
	sortOverrideRows(config.DedupAllowlist)

	if mappings != nil {
		if err := applySecondPassChecks(path, config.TagOverrides, mappings, registeredValidators); err != nil {
			return OverrideConfig{}, err
		}
	}

	return config, nil
}

// legalOverrideVersions names every schema generation the checked-in tag
// grammar and reachability checks are meaningful against. Today that is
// v201 alone, matching the one checked-in manifest (config/v201.yaml); a
// second generation (a future config/v21.yaml) adds a second literal here
// at the point it lands, the same way it adds a second manifest file rather
// than changing the one manifest format.
//
// This is deliberately not read off ir: WalkSchemas' IR carries no version
// of its own (see ir.go), because a version is a fact about which manifest
// produced ir, not about the schema definitions ir holds. An override's
// declared version is instead the row's own audience-facing statement of
// which generation it was written against -- checked here against the set
// of generations this loader is prepared to reason about at all, before any
// row's reachability is checked against whatever ir the caller actually
// supplies.
var legalOverrideVersions = map[string]bool{
	"v201": true,
}

// checkOverrideConfigVersion requires that a loaded override document
// declare one of legalOverrideVersions. An unrecognized version is caught
// before a single row loads: every row's reachability is about to be
// checked against ir, built by walking one particular schema generation, so
// a document whose own version names no generation this loader recognizes
// could otherwise load rows that pass reachability by coincidence against a
// schema they were never written for, rather than failing loudly on the one
// fact -- the document's own version -- that would have caught the mismatch
// immediately.
func checkOverrideConfigVersion(path, version string) error {
	if legalOverrideVersions[version] {
		return nil
	}
	legal := make([]string, 0, len(legalOverrideVersions))
	for known := range legalOverrideVersions {
		legal = append(legal, known)
	}
	sort.Strings(legal)
	return fmt.Errorf("load overrides %s: unknown version %q, want one of %v", path, version, legal)
}

// rowKey names a row's own (definition, property) key the way every
// governance failure message reports it: "Definition.Property" when a
// property is named, or the bare definition when it is not -- the shape a
// dedup allowlist entry always uses, and the shape a tagOverrides row
// missing its own property falls back to, since there is no property to
// complete it.
func rowKey(definition, property string) string {
	if property == "" {
		return definition
	}
	return definition + "." + property
}

// checkRowIdentityFields enforces that a tagOverrides row carries its own
// key fields (definition, property) and its tag. These are checked
// separately from the governance-record fields below (rule, rationale,
// source, author, date): a row missing them is not an incomplete record, it
// names nothing to record against. This check does not apply to a dedup
// allowlist entry, which deliberately never carries a property or a tag.
func checkRowIdentityFields(path, key string, row OverrideRow) error {
	if strings.TrimSpace(row.Definition) == "" {
		return fmt.Errorf("load overrides %s: a tagOverrides row is missing its definition", path)
	}
	if strings.TrimSpace(row.Property) == "" {
		return fmt.Errorf("load overrides %s: row %s is missing property, the other half of its key", path, key)
	}
	if strings.TrimSpace(row.Tag) == "" {
		return fmt.Errorf("load overrides %s: row %s is missing tag", path, key)
	}
	return nil
}

// checkRecordCompleteness requires that every row, in either section,
// carries a non-empty rule, rationale, source, author and date. A field
// absent from the YAML and a field present but set to "" decode to the same
// empty Go string, so one check catches both.
func checkRecordCompleteness(path, key string, row OverrideRow) error {
	fields := []struct{ name, value string }{
		{"rule", row.Rule},
		{"rationale", row.Rationale},
		{"source", row.Source},
		{"author", row.Author},
		{"date", row.Date},
	}
	for _, field := range fields {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("load overrides %s: row %s: %s is required and must be non-empty", path, key, field.name)
		}
	}
	return nil
}

// checkDateParseable requires a row's date to be a plain ISO calendar date.
func checkDateParseable(path, key string, row OverrideRow) error {
	if _, err := time.Parse("2006-01-02", row.Date); err != nil {
		return fmt.Errorf("load overrides %s: row %s: date %q is not a valid date (want YYYY-MM-DD): %v", path, key, row.Date, err)
	}
	return nil
}

// checkRuleValue requires rule to be add or tighten and nothing else.
func checkRuleValue(path, key string, row OverrideRow) error {
	switch row.Rule {
	case "add", "tighten":
		return nil
	default:
		return fmt.Errorf("load overrides %s: row %s: unknown rule %q, want %q or %q", path, key, row.Rule, "add", "tighten")
	}
}

// checkTightenHasFrom requires that a tighten row names the base token it
// replaces.
func checkTightenHasFrom(path, key string, row OverrideRow) error {
	if row.Rule == "tighten" && strings.TrimSpace(row.From) == "" {
		return fmt.Errorf("load overrides %s: row %s: rule tighten requires from, naming the base tag it replaces", path, key)
	}
	return nil
}

// tagFamily returns the validator family a token names: the text before
// "=" for a valued token ("gte=0" -> "gte"), or the whole token for a bare
// one ("dive" -> "dive").
func tagFamily(token string) string {
	if index := strings.Index(token, "="); index >= 0 {
		return token[:index]
	}
	return token
}

// boundTagValuePattern matches the one decimal spelling this generator's own
// mapping ever renders a value bound in (formatBoundNumber): an optional
// minus sign, a digit run carrying no redundant leading zero, and an
// optional fractional part. Every other spelling a float parser would also
// accept -- "1e5", "0x1p2", "+5", "Inf", "NaN", ".5", "5." -- is rejected,
// because none of them is a spelling the base mapping can produce and the
// validator reads a bound parameter with a base-0 integer parse for every
// integer field: it would read "010" as octal 8, and panic outright on
// "1e5", "Inf" or "NaN" the first time the field is validated.
var boundTagValuePattern = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?$`)

// cardinalityTagValuePattern matches the value a min=/max= token carries: a
// non-negative whole number with no redundant leading zero. Those two
// families are length and item-cardinality bounds (see
// checkFieldKindApplicability), derived from minLength/maxLength and
// minItems/maxItems, none of which is ever negative or fractional; the
// validator reads them with the same base-0 integer parse, which panics on a
// fractional value.
var cardinalityTagValuePattern = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

// checkTagTokenShape requires that a token be exactly one validator token
// and nothing more. The validator reads a struct tag by splitting it on ","
// into tokens and on "|" into alternatives, and reads a token's parameter as
// the text following its first "=" -- replacing the literal texts "0x2C" and
// "0x7C" inside that parameter with "," and "|". A row declares one tag, and
// its governance record answers for one constraint, so a value carrying any
// of that structure would compose into validators the record never mentions:
// "gte=0,dive" is the case that matters, since it loads a dive onto whatever
// field the row names and the validator panics on a dive it cannot descend
// into. Whitespace and non-printable runes are excluded for the same reason
// they can never be right: the validator carries a parameter through
// verbatim, so a trailing space alone turns a numeric bound into a parse
// failure at validation time.
func checkTagTokenShape(path, key, fieldLabel, token string) error {
	for _, r := range token {
		switch {
		case r == ',' || r == '|':
			return fmt.Errorf("load overrides %s: row %s: %s %q is not a single validator token: %q separates one validator token from the next, so this row would compose into more than the one constraint its record accounts for",
				path, key, fieldLabel, token, string(r))
		case unicode.IsSpace(r) || !unicode.IsPrint(r):
			return fmt.Errorf("load overrides %s: row %s: %s %q carries whitespace or a non-printable character, which a validator token never does",
				path, key, fieldLabel, token)
		}
	}
	if strings.Count(token, "=") > 1 {
		return fmt.Errorf("load overrides %s: row %s: %s %q carries more than one \"=\": a validator token names one parameter, and everything after the first \"=\" is read as that parameter's whole value",
			path, key, fieldLabel, token)
	}
	return nil
}

// checkValuedTagToken requires that a token of a valued family carry a value
// of the shape that family's own parameter takes, and that it carry one at
// all: a bare "gte" names a bound with nothing to bound against.
//
// A value of the right shape can still be outside the range the validator can
// read it into, which panics exactly the way a malformed one does, so the
// shape check is followed by a range check against the parser that family's
// own parameter reaches. min= and max= constrain a string's length or a
// slice's item count, and both of those are read with a whole-number parse,
// so their range is fixed here. gte= and lte= constrain a numeric field,
// whose parse depends on that field's Go kind (see
// checkBoundValueFitsGoKind); the widest of the three is the float parse, so
// what is checked here is the one bound every reading shares -- a value a
// float parse itself overflows on is out of range for all of them.
func checkValuedTagToken(path, key, fieldLabel, token, family string, pattern *regexp.Regexp, shape string) error {
	index := strings.Index(token, "=")
	if index < 0 {
		return fmt.Errorf("load overrides %s: row %s: %s %q carries no value: %s= takes %s", path, key, fieldLabel, token, family, shape)
	}
	value := token[index+1:]
	if !pattern.MatchString(value) {
		return fmt.Errorf("load overrides %s: row %s: %s %q carries the value %q, which is not %s", path, key, fieldLabel, token, value, shape)
	}
	switch family {
	case "min", "max":
		if _, err := strconv.ParseInt(value, 10, 64); err != nil {
			return fmt.Errorf("load overrides %s: row %s: %s %q carries the value %q, which is outside the range a length or item-count bound is read into (%d to %d)",
				path, key, fieldLabel, token, value, int64(math.MinInt64), int64(math.MaxInt64))
		}
	case "gte", "lte":
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsInf(parsed, 0) || math.IsNaN(parsed) {
			return fmt.Errorf("load overrides %s: row %s: %s %q carries the value %q, which is outside the range any numeric bound is read into (a value larger than %v cannot be represented)",
				path, key, fieldLabel, token, value, math.MaxFloat64)
		}
	}
	return nil
}

// checkTagVocabulary requires that a tag or from token is either one of the
// fixed grammar families (max=, min=, gte=, lte=, dive) with a well-formed
// value, or, spelled exactly, a name registeredValidators recognizes.
// required and omitempty are recognized tokens the base mapping itself
// emits, but a row may never write either: optionality is decided by the
// schema and the mapping rules, never by an override.
//
// Recognition is by whole token, never by the family alone: a registered
// validator name is a bare token that takes no parameter, so accepting
// "<registered name>=<anything>" on the strength of its family would let a
// row append arbitrary text to a tag that reaches generated code.
func checkTagVocabulary(path, key, fieldLabel, token string, registeredValidators []string) error {
	if token == "required" || token == "omitempty" {
		return fmt.Errorf("load overrides %s: row %s: %s %q is an optionality tag; optionality is decided by the base mapping, never by an override", path, key, fieldLabel, token)
	}
	if err := checkTagTokenShape(path, key, fieldLabel, token); err != nil {
		return err
	}

	switch family := tagFamily(token); family {
	case "dive":
		if token == "dive" {
			return nil
		}
		return fmt.Errorf("load overrides %s: row %s: %s %q carries a value: dive takes none", path, key, fieldLabel, token)
	case "max", "min":
		return checkValuedTagToken(path, key, fieldLabel, token, family, cardinalityTagValuePattern, "a non-negative whole number")
	case "gte", "lte":
		return checkValuedTagToken(path, key, fieldLabel, token, family, boundTagValuePattern, "a decimal number")
	default:
		if containsString(registeredValidators, token) {
			return nil
		}
	}
	return fmt.Errorf("load overrides %s: row %s: %s %q is not a recognized validator token", path, key, fieldLabel, token)
}

// findPropertyByName returns the property named name within properties, or
// nil when no such property is declared.
func findPropertyByName(properties []Property, name string) *Property {
	for i := range properties {
		if properties[i].Name == name {
			return &properties[i]
		}
	}
	return nil
}

// checkReachability requires that definition resolve to some definition in
// ir -- a message root or a $ref'd shared definition alike, since both live
// in the same flat index (see ir.go, Definition). property, when named,
// must be declared on whatever definition resolved. A dedup allowlist entry
// calls this with property empty, resolving the definition alone.
func checkReachability(path, key, definition, property string, ir IR) error {
	target := findDefinitionByName(ir.Definitions, definition)
	if target == nil {
		return fmt.Errorf("load overrides %s: row %s: definition %q does not resolve to any message root or shared definition the schemas declare", path, key, definition)
	}
	if property == "" {
		return nil
	}
	if findPropertyByName(target.Properties, property) == nil {
		return fmt.Errorf("load overrides %s: row %s: property %q is not declared on %s", path, key, property, definition)
	}
	return nil
}

// validateTagOverrideRows runs the checks that need nothing but the decoded
// row and the schema it targets -- record completeness, date and rule
// vocabulary, tighten's from requirement, tag grammar, and schema
// reachability -- over every tagOverrides row, in file order, stopping at
// the first failure.
func validateTagOverrideRows(path string, rows []OverrideRow, ir IR, registeredValidators []string) error {
	for _, row := range rows {
		key := rowKey(row.Definition, row.Property)
		if err := checkRowIdentityFields(path, key, row); err != nil {
			return err
		}
		if err := checkRecordCompleteness(path, key, row); err != nil {
			return err
		}
		if err := checkDateParseable(path, key, row); err != nil {
			return err
		}
		if err := checkRuleValue(path, key, row); err != nil {
			return err
		}
		if err := checkTightenHasFrom(path, key, row); err != nil {
			return err
		}
		if err := checkTagVocabulary(path, key, "tag", row.Tag, registeredValidators); err != nil {
			return err
		}
		if row.Rule == "tighten" {
			if err := checkTagVocabulary(path, key, "from", row.From, registeredValidators); err != nil {
				return err
			}
		}
		if err := checkReachability(path, key, row.Definition, row.Property, ir); err != nil {
			return err
		}
	}
	return nil
}

// validateDedupAllowlistRows runs the record-completeness, date, rule and
// reachability checks over every dedup allowlist entry: the same governance
// record a tagOverrides row requires, minus the property/tag/grammar checks
// that only apply to a row naming a validator tag, since an allowlist entry
// never names one.
func validateDedupAllowlistRows(path string, rows []OverrideRow, ir IR) error {
	for _, row := range rows {
		key := rowKey(row.Definition, "")
		if strings.TrimSpace(row.Definition) == "" {
			return fmt.Errorf("load overrides %s: a dedupAllowlist entry is missing its definition", path)
		}
		if err := checkRecordCompleteness(path, key, row); err != nil {
			return err
		}
		if err := checkDateParseable(path, key, row); err != nil {
			return err
		}
		if err := checkRuleValue(path, key, row); err != nil {
			return err
		}
		if err := checkReachability(path, key, row.Definition, "", ir); err != nil {
			return err
		}
	}
	return nil
}

// checkDuplicateOverrideKeys requires that two tagOverrides rows may not
// share (version, definition, property, tag) -- the tag is part of the key,
// so two rows differing only in the validator token they add are not a
// collision.
func checkDuplicateOverrideKeys(path string, rows []OverrideRow) error {
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		key := strings.Join([]string{row.Version, row.Definition, row.Property, row.Tag}, "\x00")
		if seen[key] {
			return fmt.Errorf("load overrides %s: duplicate tag-override row for version %s, %s, tag %s: a row must be unique across (version, definition, property, tag)",
				path, row.Version, rowKey(row.Definition, row.Property), row.Tag)
		}
		seen[key] = true
	}
	return nil
}

// sortOverrideRows sorts rows deterministically by (definition, property,
// tag), the same discipline this generator's other deterministic output
// applies, so two loads of the same document in separate processes apply
// rows in the same order regardless of the file's own listing order.
func sortOverrideRows(rows []OverrideRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Definition != rows[j].Definition {
			return rows[i].Definition < rows[j].Definition
		}
		if rows[i].Property != rows[j].Property {
			return rows[i].Property < rows[j].Property
		}
		return rows[i].Tag < rows[j].Tag
	})
}

// numericGoKinds names every numeric Go type this generator's mapping ever
// produces, in its base (unpointered) spelling.
var numericGoKinds = map[string]bool{
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
	"float32": true, "float64": true,
}

// isNumericGoKind reports whether kind is a numeric Go type, pointer or
// value alike -- an optional numeric property whose range admits zero maps
// to a pointer (mapNumericType), and a kind check judges the underlying
// type the pointer carries, not the pointer itself.
func isNumericGoKind(kind string) bool {
	return numericGoKinds[strings.TrimPrefix(kind, "*")]
}

// isSliceOrMapGoKind reports whether kind is a slice or map spelling.
func isSliceOrMapGoKind(kind string) bool {
	return strings.HasPrefix(kind, "[]") || strings.HasPrefix(kind, "map[")
}

// isStringGoKind reports whether kind is a string spelling, pointer or
// value alike (this generator's own mapping never produces *string, but a
// kind check should not silently stop recognizing string the day it does).
func isStringGoKind(kind string) bool {
	return strings.TrimPrefix(kind, "*") == "string"
}

// integerGoKinds names every whole-number Go type this generator's mapping
// recognizes, in its base (unpointered) spelling -- the kinds whose bound
// parameter the validator reads with an integer parse rather than a float
// one.
var integerGoKinds = map[string]bool{
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
}

// unsignedGoKinds names the subset of those whose parse rejects a sign.
var unsignedGoKinds = map[string]bool{
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
}

// checkBoundValueFitsGoKind requires that a gte=/lte= value be a bound the
// field's own Go kind can actually be compared against. The validator reads
// a bound parameter with the parse its field kind calls for -- a whole-number
// parse for an integer field, an unsigned one for an unsigned field, a float
// parse only for a float field -- and panics when the parameter does not fit
// it. So "gte=0.5" on an int field, "gte=-1" on a uint one, or a whole number
// one past that parse's own limit, is grammatical and kind-applicable and
// still takes down the first validation of that message; the row is rejected
// here instead, where the reason can be stated.
func checkBoundValueFitsGoKind(path, key, tag, goKind string) error {
	index := strings.Index(tag, "=")
	if index < 0 {
		return nil
	}
	value := tag[index+1:]
	base := strings.TrimPrefix(goKind, "*")
	if !integerGoKinds[base] {
		return nil
	}
	if strings.Contains(value, ".") {
		return fmt.Errorf("load overrides %s: row %s: tag %q does not apply to Go kind %q: a bound on a whole-number field must itself be a whole number", path, key, tag, goKind)
	}
	if unsignedGoKinds[base] {
		if strings.HasPrefix(value, "-") {
			return fmt.Errorf("load overrides %s: row %s: tag %q does not apply to Go kind %q: a bound on an unsigned field may not be negative", path, key, tag, goKind)
		}
		if _, err := strconv.ParseUint(value, 10, 64); err != nil {
			return fmt.Errorf("load overrides %s: row %s: tag %q does not apply to Go kind %q: the bound %q is outside the range an unsigned field's bound is read into (0 to %d)",
				path, key, tag, goKind, value, uint64(math.MaxUint64))
		}
		return nil
	}
	if _, err := strconv.ParseInt(value, 10, 64); err != nil {
		return fmt.Errorf("load overrides %s: row %s: tag %q does not apply to Go kind %q: the bound %q is outside the range a whole-number field's bound is read into (%d to %d)",
			path, key, tag, goKind, value, int64(math.MinInt64), int64(math.MaxInt64))
	}
	return nil
}

// checkFieldKindApplicability requires that dive only loads onto a slice or
// map field; gte=/lte= only onto a numeric field; max=/min= onto a string
// (length) or a slice (item cardinality) field, never a numeric one, which
// takes gte=/lte= instead. A family outside this set (a registered
// validator name) carries no kind constraint here.
func checkFieldKindApplicability(path, key, tag, goKind string) error {
	family := tagFamily(tag)
	switch family {
	case "dive":
		if !isSliceOrMapGoKind(goKind) {
			return fmt.Errorf("load overrides %s: row %s: tag %q does not apply to Go kind %q: dive only applies to a slice or map field", path, key, tag, goKind)
		}
	case "gte", "lte":
		if !isNumericGoKind(goKind) {
			return fmt.Errorf("load overrides %s: row %s: tag %q does not apply to Go kind %q: %s= constrains a numeric field", path, key, tag, goKind, family)
		}
		return checkBoundValueFitsGoKind(path, key, tag, goKind)
	case "max", "min":
		if !isStringGoKind(goKind) && !isSliceOrMapGoKind(goKind) {
			return fmt.Errorf("load overrides %s: row %s: tag %q does not apply to Go kind %q: %s= constrains a string or slice field, not a numeric one (use gte=/lte= there)", path, key, tag, goKind, family)
		}
	}
	return nil
}

// containsString reports whether values contains wanted, by exact match.
func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// parseNumericTagValue extracts the numeric value out of a "family=value"
// token.
func parseNumericTagValue(token string) (float64, error) {
	index := strings.Index(token, "=")
	if index < 0 {
		return 0, fmt.Errorf("token %q carries no value", token)
	}
	return strconv.ParseFloat(token[index+1:], 64)
}

// checkTightenSource requires that a tighten row's from token genuinely be
// the base token being replaced, that the replacement constrain the same
// thing, and that it genuinely tighten it.
//
// from must be present in mapping's base tag set verbatim -- checking
// anything else before checking presence would judge a token that was never
// really there. The replacement must then name the SAME validator family as
// from: a tighten replaces a constraint with a stricter one, and two
// different families are two different constraints, so swapping one for
// another is a substitution the base mapping's own token disappears in.
// Replacing "lte=100" with "gte=50" would otherwise read as a tightening --
// 50 is below 100 -- while actually deleting a ceiling and leaving a floor,
// so every value the ceiling excluded would then validate. Within one
// family, a valued bound must move strictly the tightening way: a floor
// (gte/min) may only rise, a ceiling (lte/max) may only fall. A bare family
// has no value to move, so for it the equality check above is the whole of
// the direction rule -- replacing a token with itself changes nothing and is
// not a tightening either way.
func checkTightenSource(path, key string, row OverrideRow, mapping OverrideMapping, tokens []string, registeredValidators []string) error {
	if !containsString(mapping.BaseTags, row.From) {
		return fmt.Errorf("load overrides %s: row %s: tighten names from token %q, which is not present in the base tag set %v", path, key, row.From, mapping.BaseTags)
	}
	if _, occurrences := scopeOfToken(tokens, row.From); occurrences > 1 {
		return fmt.Errorf("load overrides %s: row %s: tighten names from token %q, which the tag set %v this row composes onto carries on both sides of dive: one occurrence bounds the list and the other bounds each of its values, and a row names a token, not which of the two it meant",
			path, key, row.From, tokens)
	}

	family := ownershipFamily(row.From, registeredValidators)
	if replacement := ownershipFamily(row.Tag, registeredValidators); replacement != family {
		return fmt.Errorf("load overrides %s: row %s: tighten replacing %q with %q crosses validator families (%s to %s); a tighten replaces a constraint with a stricter one of the same family, and %s= would drop the %s= constraint altogether",
			path, key, row.From, row.Tag, family, replacement, replacement, family)
	}
	if row.Tag == row.From {
		return fmt.Errorf("load overrides %s: row %s: tighten replacing %q with %q leaves the constraint unchanged; a tighten must strictly tighten it", path, key, row.From, row.Tag)
	}

	switch family {
	case "gte", "min", "lte", "max":
	default:
		// A registered validator names one enum's value set. Replacing it
		// with another enum's is not a tightening the loader can establish:
		// a tighten must be shown to be strictly stricter, and nothing the
		// mapping carries says one enum's values are a subset of another's --
		// the field's own declared type is what fixes them. Replacing it with
		// ITSELF is already refused above as leaving the constraint
		// unchanged, so every remaining replacement crosses value sets.
		if containsString(registeredValidators, row.From) {
			return fmt.Errorf("load overrides %s: row %s: tighten replacing %q with %q swaps one enum's permitted values for another's, which cannot be shown to be stricter; a field's permitted values follow from its own declared type",
				path, key, row.From, row.Tag)
		}
		return nil
	}
	fromValue, fromErr := parseNumericTagValue(row.From)
	tagValue, tagErr := parseNumericTagValue(row.Tag)
	if fromErr != nil || tagErr != nil {
		// Unreachable through LoadOverrides, whose tag grammar has already
		// required a well-formed value of both tokens; reported rather than
		// skipped so a future caller reaching this function directly cannot
		// buy itself an unchecked direction by supplying an unparseable bound.
		return fmt.Errorf("load overrides %s: row %s: tighten replacing %q with %q compares two %s bounds, and one of them carries no numeric value", path, key, row.From, row.Tag, family)
	}
	switch family {
	case "gte", "min":
		if !(tagValue > fromValue) {
			return fmt.Errorf("load overrides %s: row %s: tighten replacing %q with %q does not raise the floor; gte/min may only increase", path, key, row.From, row.Tag)
		}
	case "lte", "max":
		if !(tagValue < fromValue) {
			return fmt.Errorf("load overrides %s: row %s: tighten replacing %q with %q does not lower the ceiling; lte/max may only decrease", path, key, row.From, row.Tag)
		}
	}
	return nil
}

// familyOwner records the token that currently holds one validator family in
// one scope of one property, and what put it there: the base mapping, or an
// earlier override row named by its own key.
type familyOwner struct {
	token string
	row   string
}

// familyKey names one validator family in one dive scope. dive divides a
// slice's tag into two namespaces -- every token in front of it constrains
// the list (min= there is an item count), every token after it constrains
// each value (min= there is a length) -- so one family appearing on both
// sides is two constraints, not a duplicate, and they cannot share an owner.
//
// dive itself is keyed outside every scope (scope -1). It is the operator
// that creates the scopes rather than a constraint inside one, and this
// generator's mapping never produces a slice of slices (an array element is
// a scalar, an enum or a composite), so a second dive is never a legitimate
// descent into a second level -- it is a descent into something that cannot
// be descended into, which the validator panics on.
type familyKey struct {
	scope  int
	family string
}

func ownershipKey(scope int, family string) familyKey {
	if family == "dive" {
		return familyKey{scope: -1, family: family}
	}
	return familyKey{scope: scope, family: family}
}

// enumValueSetFamily is the ownership family every registered validator token
// shares. Every name this generator registers is one enum's own check (see
// buildOverrideMappings, which derives the registry from the enum definitions
// alone), and what such a check constrains is which value set the field's
// value must belong to. A field has one value set: the mapping derives
// exactly one enum token for an enum-typed property, from that property's own
// $ref. Two tokens on one field would mean the value must satisfy both value
// sets at once -- an intersection that is empty for any two distinct enums,
// so the field could never validate -- which is why the two do not each get a
// family of their own the way two different bound families do.
const enumValueSetFamily = "enum value set"

// ownershipFamily names the family a token holds for ownership purposes.
func ownershipFamily(token string, registeredValidators []string) string {
	if containsString(registeredValidators, token) {
		return enumValueSetFamily
	}
	return tagFamily(token)
}

// validatedGoKind reports the Go kind the validator will actually apply a
// token to: the field's own kind for a token in front of dive, and the
// element kind for one after it, since every token past dive is applied to
// each value rather than to the container.
func validatedGoKind(goKind string, scope int) string {
	if scope > 0 {
		return strings.TrimPrefix(goKind, "[]")
	}
	return goKind
}

// carriesRegisteredTokenInScope reports whether a tag set already names a
// registered validator inside the given dive scope.
func carriesRegisteredTokenInScope(tokens []string, scope int, registeredValidators []string) bool {
	current := 0
	for _, token := range tokens {
		if current == scope && containsString(registeredValidators, token) {
			return true
		}
		if token == "dive" {
			current++
		}
	}
	return false
}

// checkRegisteredTokenTarget requires that a registered validator token load
// only onto a field it can actually read.
//
// The generated check is a string switch: it reads its field as
// fl.Field().String() and compares the result against the enum's own
// constants (see the isValid function the emitter builds for every enum).
// reflect's String is the one getter that does not panic on a mismatched
// kind -- it returns "<int Value>" for an int -- so a token on a numeric,
// boolean or composite field does not fail loudly at all: it silently matches
// none of the enum's values and rejects every payload the field ever carries.
//
// The kinds it can read are therefore the string-kinded ones, which in this
// generator's mapping table are exactly two: the "string" spelling itself
// (pointer or value alike), and a generated enum's own named type, which is
// declared as `type X string`. The second cannot be recognized from its
// spelling -- an enum's Go name looks like any other named type -- so it is
// recognized by the one fact that distinguishes it: the mapping appends that
// enum's own token to the property's base tag set, in the same dive scope the
// row's token would land in. A slice of either is reached through dive, where
// the element is what gets read, which is why the kind judged here is the
// validated one and not the field's own.
func checkRegisteredTokenTarget(path, key, token, goKind string, tokens []string, scope int, registeredValidators []string) error {
	if !containsString(registeredValidators, token) {
		return nil
	}
	kind := validatedGoKind(goKind, scope)
	if isStringGoKind(kind) || carriesRegisteredTokenInScope(tokens, scope, registeredValidators) {
		return nil
	}
	return fmt.Errorf("load overrides %s: row %s: tag %q does not apply to Go kind %q: a registered validator reads its field as a string and compares it against a fixed set of values, so it loads only onto a string field, a field of the generated type that registered it, or the elements of a slice of either",
		path, key, token, kind)
}

// diveCount reports how many dive tokens tokens carries, which is also the
// scope index a token appended to the end of that list lands in.
func diveCount(tokens []string) int {
	count := 0
	for _, token := range tokens {
		if token == "dive" {
			count++
		}
	}
	return count
}

// scopeOfToken reports which dive scope the named token sits in, and how many
// times it occurs. A token occurring more than once occurs in more than one
// scope (a base tag set never repeats a token within one scope), which makes
// naming it ambiguous -- see checkTightenSource.
func scopeOfToken(tokens []string, wanted string) (scope, occurrences int) {
	scope, occurrences = 0, 0
	found := -1
	current := 0
	for _, token := range tokens {
		if token == wanted {
			occurrences++
			if found < 0 {
				found = current
			}
		}
		if token == "dive" {
			current++
		}
	}
	if found < 0 {
		return 0, 0
	}
	return found, occurrences
}

// baseFamilyOwners indexes a base tag set by (dive scope, validator family).
// The first occurrence within a scope wins, so the token a failure message
// names is the one a reader finds first in that half of the tag.
func baseFamilyOwners(baseTags []string, registeredValidators []string) map[familyKey]familyOwner {
	owners := make(map[familyKey]familyOwner, len(baseTags))
	scope := 0
	for _, token := range baseTags {
		family := ownershipFamily(token, registeredValidators)
		if key := ownershipKey(scope, family); owners[key].token == "" {
			owners[key] = familyOwner{token: token}
		}
		if token == "dive" {
			scope++
		}
	}
	return owners
}

// checkNoDuplicateFamily requires that an add row introduce a validator
// family the property does not already carry -- neither from the base
// mapping nor from an override row applied to that same property before it.
// A property carries one row per validator family: a second row of a family
// already present appends a duplicate rather than composing a new
// constraint, so what the field ends up enforcing is spread across two
// governance records, neither of which states it.
//
// Which of the two the row collides with decides what to tell the author. A
// base tag is a constraint the schema itself produced, and replacing it is
// what tighten is for. An earlier row's token is one this table wrote, and
// two rows of one family is not a tightening of anything -- the constraint
// the field must end up with is a single fact and belongs in a single row.
func checkNoDuplicateFamily(path, key string, row OverrideRow, family string, scope int, owners map[familyKey]familyOwner) error {
	owner, taken := owners[ownershipKey(scope, family)]
	if !taken {
		return nil
	}
	if family == enumValueSetFamily {
		// The advice the other two messages give -- replace the existing
		// token with tighten -- is not available here: which values a field
		// may hold follows from its own type in the schema, and no row can
		// show one enum's values to be a subset of another's (see
		// checkTightenSource).
		return fmt.Errorf("load overrides %s: row %s: add tag %q gives the field a second %s alongside %q, which %s already carries it as; a value would have to satisfy both, and a field's permitted values follow from its own declared type",
			path, key, row.Tag, family, owner.token, ownerDescription(owner))
	}
	if owner.row == "" {
		return fmt.Errorf("load overrides %s: row %s: add tag %q duplicates the %s family the base tag set already carries as %q; use tighten with from: %s instead",
			path, key, row.Tag, family, owner.token, owner.token)
	}
	return fmt.Errorf("load overrides %s: row %s: add tag %q duplicates the %s family row %s already added as %q; one property carries one row per validator family, so state the constraint the field must end up with in a single row",
		path, key, row.Tag, family, owner.row, owner.token)
}

// ownerDescription names what put a token on a field, for a message that has
// to read naturally either way.
func ownerDescription(owner familyOwner) string {
	if owner.row == "" {
		return "the base tag set"
	}
	return "row " + owner.row
}

// checkTightenOwnership requires that the family a tighten replaces still be
// held by the base mapping rather than by an earlier row. Two tighten rows
// naming one base token both pass the source check -- the token really is in
// the base tag set, for both of them -- and the composition then fails on the
// second one, because the first already substituted the token away. Catching
// it here is what keeps the loader's verdict and the emitter's behavior the
// same: a table this check accepted would otherwise fail at generation time,
// after governance had signed it off.
func checkTightenOwnership(path, key string, row OverrideRow, family string, scope int, owners map[familyKey]familyOwner) error {
	owner, taken := owners[ownershipKey(scope, family)]
	if !taken || owner.row == "" {
		return nil
	}
	return fmt.Errorf("load overrides %s: row %s: tighten replacing %q with %q collides with row %s, which already replaced the %s family with %q; one property carries one row per validator family, so state the constraint the field must end up with in a single row",
		path, key, row.From, row.Tag, owner.row, family, owner.token)
}

// checkAddedTokenScope rejects an add whose token would land on the far side
// of dive from the constraint the mapping rules say it names.
//
// An override composes by appending, so an added token always joins the end
// of the tag -- which, once the base set carries dive, is the element half:
// what it constrains is each value in the list, not the list. But the
// mapping rules read min=/max= on a slice property as its ITEM-CARDINALITY
// bound, the one the base set itself renders from minItems/maxItems in front
// of dive. For a slice whose elements carry constraints of their own, those
// two readings disagree, and the row format carries no field that could say
// which was meant: definition, property, tag, rule and from are all it has,
// and strict decoding means it can never quietly grow a sixth.
//
// So the row is refused rather than resolved. Element-scoped bounds are not
// expressible by a row today; making them expressible is a change to the row
// format, which is not something a loader may decide on an author's behalf by
// picking whichever side its own composition rule happens to reach.
func checkAddedTokenScope(path, key string, row OverrideRow, goKind string, tokens []string) error {
	if diveCount(tokens) == 0 {
		return nil
	}
	switch tagFamily(row.Tag) {
	case "min", "max":
		return fmt.Errorf("load overrides %s: row %s: add tag %q cannot be placed: on a property mapping to %q the tag set %v this row composes onto already dives, so an appended token constrains each value in the list, while a %s= row on a slice property names the list's own item count; the row format cannot say which of the two it means",
			path, key, row.Tag, goKind, tokens, tagFamily(row.Tag))
	}
	return nil
}

// applySecondPassChecks runs the checks that need the base tag set and
// mapped Go kind for each row's own target -- field-kind applicability, the
// tighten-source checks, and duplicate-family detection -- over every
// tagOverrides row. mappings is a promise (see OverrideMappings' own doc
// comment): a target absent from it is a caller error, not license to treat
// the row as if it had no base tags.
//
// Rows are walked in the order they will be applied (the deterministic sort
// LoadOverrides has already put them in, which is also the order the emitter
// composes them in), and each property's family ownership is carried forward
// across its own rows: what a row is judged against is the base tag set plus
// every row already applied to that property, not the base tag set alone.
// The distinction only exists for a property carrying more than one row, and
// there it is the whole of the rule -- checking each row against the base set
// in isolation lets two rows of one family through, and the field then
// carries both of them.
//
// Ownership is held per dive scope, not per property: the two halves of a
// slice's tag constrain different things, so a row replacing the list's own
// item count and a row replacing each element's length are two rows of one
// family that do not collide (see familyKey).
//
// Which scope a row lands in is read off the tag set the row actually
// composes onto -- the base set with every earlier row already applied -- and
// not off the base set alone, for the same reason ownership is carried
// forward: dive is itself a token a row may add. A row adding dive moves the
// end of the tag into the element half, so the next row's token lands
// somewhere the base set gives no sign of. Judged against the base alone, an
// added min= on such a target reads as the list's item count while composing
// into each value's length -- the check passes and the generated field
// enforces something else, which is the one outcome this stage exists to
// prevent.
//
// Both rules take ownership. add introduces a family into the scope its token
// lands in; tighten replaces a token already there, and then owns that family
// itself, because the token it named is gone from the composed tag and a
// second row naming it would compose against something that is no longer
// present.
func applySecondPassChecks(path string, rows []OverrideRow, mappings OverrideMappings, registeredValidators []string) error {
	owners := make(map[OverrideTarget]map[familyKey]familyOwner, len(rows))
	composed := make(map[OverrideTarget][]string, len(rows))
	for _, row := range rows {
		key := rowKey(row.Definition, row.Property)
		target := OverrideTarget{Definition: row.Definition, Property: row.Property}
		mapping, ok := mappings[target]
		if !ok {
			return fmt.Errorf("load overrides %s: row %s: missing mapping: the supplied override mappings carry no entry for this row's target", path, key)
		}
		targetOwners, started := owners[target]
		if !started {
			targetOwners = baseFamilyOwners(mapping.BaseTags, registeredValidators)
			owners[target] = targetOwners
			composed[target] = mapping.BaseTags
		}
		tokens := composed[target]

		if err := checkFieldKindApplicability(path, key, row.Tag, mapping.GoKind); err != nil {
			return err
		}

		scope := diveCount(tokens)
		if row.Rule == "tighten" {
			scope, _ = scopeOfToken(tokens, row.From)
		}
		if err := checkRegisteredTokenTarget(path, key, row.Tag, mapping.GoKind, tokens, scope, registeredValidators); err != nil {
			return err
		}

		family := ownershipFamily(row.Tag, registeredValidators)
		switch row.Rule {
		case "tighten":
			if err := checkTightenSource(path, key, row, mapping, tokens, registeredValidators); err != nil {
				return err
			}
			if err := checkTightenOwnership(path, key, row, family, scope, targetOwners); err != nil {
				return err
			}
		case "add":
			if err := checkAddedTokenScope(path, key, row, mapping.GoKind, tokens); err != nil {
				return err
			}
			if err := checkNoDuplicateFamily(path, key, row, family, scope, targetOwners); err != nil {
				return err
			}
		}
		targetOwners[ownershipKey(scope, family)] = familyOwner{token: row.Tag, row: key}

		// Carry the row through exactly as the emitter will, so the next row
		// on this target is judged against the tag set it really composes
		// onto. Composition cannot fail here: the checks above have already
		// established the row's tag and, for a tighten, that the token it
		// names is still present.
		next, err := ApplyTagOverride(tokens, row)
		if err != nil {
			return fmt.Errorf("load overrides %s: row %s: %w", path, key, err)
		}
		composed[target] = next
	}
	return nil
}

// unknownFieldPattern matches one line of a yaml.v3 strict-decode
// TypeError -- "line 5: field goType not found in type main.OverrideRow" --
// extracting the line number and the field name.
var unknownFieldPattern = regexp.MustCompile(`^line (\d+): field (\S+) not found in type `)

// overrideRowLocation is one row's own starting line (from the raw node
// tree) and the key it declares, used to attribute a strict-decode failure
// to the row that produced it.
type overrideRowLocation struct {
	line int
	key  string
}

// rowLineRanges walks data as a raw YAML node tree (ignoring strictness) and
// records each tagOverrides/dedupAllowlist entry's own starting line and
// key. A malformed document -- one that fails even a non-strict parse, which
// a document that only failed strict decoding never is -- yields no
// locations, and the caller falls back to reporting a field name alone.
func rowLineRanges(data []byte) []overrideRowLocation {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil || len(root.Content) == 0 {
		return nil
	}
	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return nil
	}

	var locations []overrideRowLocation
	for i := 0; i+1 < len(doc.Content); i += 2 {
		sectionKey, sectionValue := doc.Content[i], doc.Content[i+1]
		if sectionKey.Value != "tagOverrides" && sectionKey.Value != "dedupAllowlist" {
			continue
		}
		if sectionValue.Kind != yaml.SequenceNode {
			continue
		}
		for _, item := range sectionValue.Content {
			locations = append(locations, overrideRowLocation{line: item.Line, key: rowNodeKey(item)})
		}
	}
	sort.Slice(locations, func(i, j int) bool { return locations[i].line < locations[j].line })
	return locations
}

// rowNodeKey reads a row mapping node's own definition/property values
// directly off the node tree, independent of whatever field made the whole
// document fail strict decoding.
func rowNodeKey(item *yaml.Node) string {
	if item.Kind != yaml.MappingNode {
		return ""
	}
	var definition, property string
	for i := 0; i+1 < len(item.Content); i += 2 {
		k, v := item.Content[i], item.Content[i+1]
		switch k.Value {
		case "definition":
			definition = v.Value
		case "property":
			property = v.Value
		}
	}
	return rowKey(definition, property)
}

// rowForLine returns the key of the last row starting at or before line --
// the row whose own block a line at that position falls inside, since a
// block sequence's entries never overlap. found is false when line precedes
// every row (a document-level field, not a row's own).
func rowForLine(locations []overrideRowLocation, line int) (key string, found bool) {
	for _, location := range locations {
		if location.line > line {
			break
		}
		key, found = location.key, true
	}
	return key, found
}

// collapseYAMLError flattens a yaml.v3 error's own multi-line text onto one
// line, the same discipline LoadTransformConfig already applies.
func collapseYAMLError(err error) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(err.Error(), "\n", " ")), " ")
}

// wrapOverridesDecodeError turns a strict-decode failure into a hard
// failure naming the offending field and, when the field sits inside a
// tagOverrides or dedupAllowlist row, that row's own key -- a
// document-level field (one not nested inside any row) is named on its own,
// since there is no row to attribute it to. Every message stays on one
// line: yaml.v3's own TypeError text is multi-line raw output, and a caller
// reading this error should never see that leak through.
func wrapOverridesDecodeError(path string, data []byte, decodeErr error) error {
	typeErr, ok := decodeErr.(*yaml.TypeError)
	if !ok {
		return fmt.Errorf("load overrides %s: %s", path, collapseYAMLError(decodeErr))
	}

	locations := rowLineRanges(data)
	parts := make([]string, 0, len(typeErr.Errors))
	for _, raw := range typeErr.Errors {
		match := unknownFieldPattern.FindStringSubmatch(raw)
		if match == nil {
			parts = append(parts, raw)
			continue
		}
		line, _ := strconv.Atoi(match[1])
		field := match[2]
		if owner, found := rowForLine(locations, line); found {
			parts = append(parts, fmt.Sprintf("row %s carries unrecognized field %q", owner, field))
		} else {
			parts = append(parts, fmt.Sprintf("unrecognized field %q", field))
		}
	}
	return fmt.Errorf("load overrides %s: %s", path, strings.Join(parts, "; "))
}

// messagesReaching returns every declared message whose reach set names
// definitionName.
func messagesReaching(ir IR, definitionName string) []Message {
	var reaching []Message
	for _, message := range ir.Messages {
		for _, reached := range message.Reach {
			if reached == definitionName {
				reaching = append(reaching, message)
				break
			}
		}
	}
	return reaching
}

// derivedFilePath names the generated file a definition's fields land in,
// the way the emitter's own placement and file-naming rules decide it --
// reused here rather than re-derived, so the report can never name a file
// the emitter itself would not write to.
//
// A message root is always emitted directly into its own owning message's
// file; a $ref'd shared definition follows the reach-count rule (more than
// one declared message reaching it goes to the shared types file, exactly
// one goes to that one message's own file). This mirrors placement.go's
// ComputePlacement narrowly, for a single name, rather than calling it
// outright: ComputePlacement demands every non-root, non-reserved
// definition in ir be reachable by some message before it will place any of
// them, a whole-corpus requirement this per-row lookup has no need to
// impose on a caller's fixture.
func derivedFilePath(ir IR, definitionName string) (string, error) {
	for _, message := range ir.Messages {
		if message.RequestRoot == definitionName || message.ResponseRoot == definitionName {
			return message.Block + "/" + snakeCase(message.Name) + ".go", nil
		}
	}

	reaching := messagesReaching(ir, definitionName)
	switch len(reaching) {
	case 0:
		return "", fmt.Errorf("derive file path for %s: no declared message reaches it", definitionName)
	case 1:
		return reaching[0].Block + "/" + snakeCase(reaching[0].Name) + ".go", nil
	default:
		return "types/types_gen.go", nil
	}
}

// composeTargetTags folds every loaded row onto the base tag set of the
// property it targets, exactly the way the emitter does it, and returns the
// final token list each target ends up carrying.
//
// The emitter applies overrides in MapProperty by walking the row slice it
// was handed, in that slice's own order, applying every row whose
// (definition, property) matches the property being mapped -- so when two
// rows name one property, what that field carries is the second row composed
// on top of the first, not either row composed alone. The slice both sides
// walk is the one LoadOverrides returns, sorted by (definition, property,
// tag), which is what makes the order a fact rather than an accident of file
// listing. Reproducing that fold here, rather than rendering each row
// against the base set independently, is what keeps the report's claim and
// the emitted tag the same string: a per-row rendering of two rows on one
// property reports two tags the generated code never carries and omits the
// one it does.
func composeTargetTags(rows []OverrideRow, mappings OverrideMappings) (map[OverrideTarget][]string, error) {
	composed := make(map[OverrideTarget][]string, len(rows))
	for _, row := range rows {
		key := rowKey(row.Definition, row.Property)
		target := OverrideTarget{Definition: row.Definition, Property: row.Property}
		tokens, applied := composed[target]
		if !applied {
			mapping, ok := mappings[target]
			if !ok {
				return nil, fmt.Errorf("row %s: no mapping entry for this target", key)
			}
			tokens = mapping.BaseTags
		}
		next, err := ApplyTagOverride(tokens, row)
		if err != nil {
			return nil, fmt.Errorf("row %s: %w", key, err)
		}
		composed[target] = next
	}
	return composed, nil
}

// RenderOverridesReport renders the deterministic tag-override report from
// an already-loaded config and the same ir/mappings LoadOverrides checked it
// against. It is a pure renderer: it does no file I/O and resolves no path,
// so the same function backs both the byte-for-byte determinism test and
// the -overrides-report command, which loads the checked-in config through
// LoadOverrides itself before calling this seam (see main.go).
//
// Each row is rendered as its own key, the rule it applied, the tag its
// property ends up carrying, the generated file and field it lands on, and
// its full governance record -- so a reader answers "why does this field
// carry this tag" from the report alone.
//
// The reported tag is the property's FINAL composed tag (composeTargetTags),
// never the row's own raw token and never that token composed against the
// base set alone: the reader is being told what the generated field carries,
// which for a property carrying more than one row is all of them composed.
// Every row naming that property therefore reports the same tag, since there
// is only one tag to report; which token each row contributed is what its
// own rule and record say.
func RenderOverridesReport(config OverrideConfig, ir IR, mappings OverrideMappings) ([]byte, error) {
	composed, err := composeTargetTags(config.TagOverrides, mappings)
	if err != nil {
		return nil, fmt.Errorf("render overrides report: %w", err)
	}

	var buf bytes.Buffer
	for i, row := range config.TagOverrides {
		key := rowKey(row.Definition, row.Property)
		target := OverrideTarget{Definition: row.Definition, Property: row.Property}
		mapping, ok := mappings[target]
		if !ok {
			return nil, fmt.Errorf("render overrides report: row %s: no mapping entry for this target", key)
		}
		file, err := derivedFilePath(ir, row.Definition)
		if err != nil {
			return nil, fmt.Errorf("render overrides report: row %s: %w", key, err)
		}

		if i > 0 {
			buf.WriteByte('\n')
		}
		fmt.Fprintf(&buf, "%s\n", key)
		fmt.Fprintf(&buf, "  rule: %s\n", row.Rule)
		fmt.Fprintf(&buf, "  tag: %s\n", strings.Join(composed[target], ","))
		fmt.Fprintf(&buf, "  file: %s\n", file)
		fmt.Fprintf(&buf, "  field: %s\n", mapping.FieldName)
		fmt.Fprintf(&buf, "  rationale: %s\n", strings.TrimSpace(row.Rationale))
		fmt.Fprintf(&buf, "  source: %s\n", row.Source)
		fmt.Fprintf(&buf, "  author: %s\n", row.Author)
		fmt.Fprintf(&buf, "  date: %s\n", row.Date)
	}
	return buf.Bytes(), nil
}

// buildOverrideMappings computes the second pass's own inputs -- the base
// tag set, mapped Go kind and field name for every property in ir, and
// every validator name the emitted tree would register -- by calling the
// same MapProperty the emitter itself uses, with no override rows applied,
// so the governance surface never re-derives a mapping rule of its own.
func buildOverrideMappings(ir IR, transform TransformConfig) (OverrideMappings, []string, error) {
	mappings := make(OverrideMappings)
	for _, definition := range ir.Definitions {
		if definition.Kind == DefinitionEnum {
			continue
		}
		for _, property := range definition.Properties {
			mapping, err := MapProperty(definition, property, ir.Definitions, transform, nil)
			if err != nil {
				return nil, nil, fmt.Errorf("map %s.%s: %w", definition.Name, property.Name, err)
			}
			mappings[OverrideTarget{Definition: definition.Name, Property: property.Name}] = OverrideMapping{
				BaseTags:  splitValidateTag(mapping.ValidateTag),
				GoKind:    mapping.GoType,
				FieldName: mapping.FieldName,
			}
		}
	}

	var registeredValidators []string
	for _, definition := range ir.Definitions {
		if definition.Kind != DefinitionEnum {
			continue
		}
		tag, err := validatorTagName(definition.Name, transform)
		if err != nil {
			return nil, nil, fmt.Errorf("derive validator tag for %s: %w", definition.Name, err)
		}
		registeredValidators = append(registeredValidators, tag)
	}
	sort.Strings(registeredValidators)

	return mappings, registeredValidators, nil
}

// splitValidateTag turns a rendered validate:"a,b" fragment back into its
// own token list, the base tag set governance checks compose against.
func splitValidateTag(validateTag string) []string {
	inner := strings.TrimSuffix(strings.TrimPrefix(validateTag, `validate:"`), `"`)
	if inner == "" {
		return []string{}
	}
	return strings.Split(inner, ",")
}
