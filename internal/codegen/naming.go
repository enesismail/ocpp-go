package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// LoadTransformConfig reads the fixed initialism table the naming
// transform runs against (config/transform.yaml). Decoding is strict: an
// unrecognized top-level field is a hard failure naming the field, exactly
// the same discipline LoadManifest already applies to the manifest
// document, collapsed to one line so the caller's error chain stays
// readable.
//
// The file holds one document. Only the first is decoded, so a second one --
// appended by a merge, or left behind by an edit -- would otherwise go
// unread along with the initialism list it declares, and the strict-field
// check the first document was just held to would say nothing about it. A
// silently dropped second list is the worst of the three outcomes here: the
// naming transform would keep running against a table nobody meant it to
// use, renaming identifiers across the whole generated tree.
func LoadTransformConfig(path string) (TransformConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return TransformConfig{}, fmt.Errorf("load transform config %s: %w", path, err)
	}

	var config TransformConfig
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return TransformConfig{}, fmt.Errorf("load transform config %s: %s", path, collapseYAMLError(err))
	}
	var extraDocument yaml.Node
	if err := decoder.Decode(&extraDocument); !errors.Is(err, io.EOF) {
		if err != nil {
			return TransformConfig{}, fmt.Errorf("load transform config %s: %s", path, collapseYAMLError(err))
		}
		return TransformConfig{}, fmt.Errorf("load transform config %s: the file holds more than one YAML document; the initialism table is a single document", path)
	}
	return config, nil
}

// TransformIdentifier renders a schema definition name into its exported Go
// spelling. The suffix strip runs first, keyed only on the definition's
// kind (never on a pattern match against the name itself): an object
// definition loses a trailing "Type"; an enum definition loses a trailing
// "Enum" while keeping "Type" (so "IdTokenEnumType" strips to
// "IdTokenType", not "IdToken"); a root keeps its name as given, since
// roots are named directly from the manifest message they belong to
// wherever this task renders one, never through this function. After the
// strip, the remaining name is split into words at case and digit
// boundaries and each word is corrected against the configured initialism
// table (or title-cased when no entry matches) and rejoined.
func TransformIdentifier(name string, kind DefinitionKind, config TransformConfig) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("transform identifier: empty name")
	}

	base := name
	switch kind {
	case DefinitionObject:
		if len(base) > len("Type") && strings.HasSuffix(base, "Type") {
			base = strings.TrimSuffix(base, "Type")
		}
	case DefinitionEnum:
		if strings.HasSuffix(base, "EnumType") {
			base = strings.TrimSuffix(base, "EnumType") + "Type"
		}
	case DefinitionRoot:
		// No suffix stripping: a root's rendered name is derived directly
		// from the manifest message name wherever this task actually
		// renders one; this branch exists only so the function has a
		// defined, non-error answer if a caller passes DefinitionRoot.
	default:
		return "", fmt.Errorf("transform identifier %q: unknown definition kind %q", name, kind)
	}

	return strings.Join(transformWords(base, config), ""), nil
}

// splitWords splits s into words at case boundaries (a lowercase-to-
// uppercase transition, or the last uppercase letter before a following
// lowercase run within an all-caps stretch) and at digit boundaries. It
// performs no case correction and no initialism lookup: it only decides
// where one word ends and the next begins, preserving every rune exactly
// as given, so it doubles as the plain word-splitter a banner phrase needs
// (no initialism correction there) and as the first step of the naming
// transform (which does correct each word afterward).
func splitWords(s string) []string {
	runes := []rune(s)
	if len(runes) == 0 {
		return nil
	}
	var words []string
	start := 0
	for i := 1; i < len(runes); i++ {
		prev, cur := runes[i-1], runes[i]
		boundary := false
		switch {
		case unicode.IsDigit(prev) != unicode.IsDigit(cur):
			boundary = true
		case unicode.IsLower(prev) && unicode.IsUpper(cur):
			boundary = true
		case unicode.IsUpper(prev) && unicode.IsUpper(cur) && i+1 < len(runes) && unicode.IsLower(runes[i+1]):
			boundary = true
		}
		if boundary {
			words = append(words, string(runes[start:i]))
			start = i
		}
	}
	words = append(words, string(runes[start:]))
	return words
}

// matchInitialism looks word up in the configured table case-insensitively
// and returns the table's own canonical spelling. Table entries are
// compared by whole-word equality (never as a prefix of a longer word), so
// only a word whose entire text matches an entry is corrected — this is
// what keeps a value like "Idle" untouched even though "ID" is a table
// entry two characters shorter. When more than one entry could match (not
// possible for the fixed v201 table, since no two entries share a
// spelling), the longest is preferred.
func matchInitialism(word string, initialisms []string) (string, bool) {
	best := ""
	for _, entry := range initialisms {
		if strings.EqualFold(word, entry) && len(entry) > len(best) {
			best = entry
		}
	}
	return best, best != ""
}

// titleCaseWord uppercases word's leading rune and leaves the rest exactly
// as given, which is enough for every word this task ever titles: each one
// already arrives correctly cased from splitWords's boundary rules or from
// an all-lowercase schema property/definition name.
func titleCaseWord(word string) string {
	if word == "" {
		return word
	}
	r := []rune(word)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// transformWords splits name and corrects each resulting word against the
// initialism table, title-casing any word the table does not recognize.
func transformWords(name string, config TransformConfig) []string {
	words := splitWords(name)
	out := make([]string, len(words))
	for i, w := range words {
		if canon, ok := matchInitialism(w, config.Initialisms); ok {
			out[i] = canon
		} else {
			out[i] = titleCaseWord(w)
		}
	}
	return out
}

// transformPropertyName renders a schema property name into its exported Go
// field name. Properties carry no suffix to strip (that only applies to a
// definition's own object/enum naming); this is the plain
// word-split-and-correct transform alone.
func transformPropertyName(name string, config TransformConfig) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("transform property name: empty name")
	}
	return strings.Join(transformWords(name, config), ""), nil
}

// goKeywords names every Go reserved word. A lowered identifier this task
// derives (a constructor parameter, an enum switch variable) is never
// itself schema-authored — it is mechanically produced from a property or
// type name the schema is free to spell any way it likes, "type" among
// them — so a bare keyword collision is a real, if rare, corpus hazard
// rather than a hypothetical one worth skipping.
var goKeywords = map[string]bool{
	"break": true, "case": true, "chan": true, "const": true, "continue": true,
	"default": true, "defer": true, "else": true, "fallthrough": true, "for": true,
	"func": true, "go": true, "goto": true, "if": true, "import": true,
	"interface": true, "map": true, "package": true, "range": true, "return": true,
	"select": true, "struct": true, "switch": true, "type": true, "var": true,
}

// escapeGoKeyword appends a trailing underscore to name when it is itself a
// Go reserved word, leaving every other identifier untouched.
func escapeGoKeyword(name string) string {
	if goKeywords[name] {
		return name + "_"
	}
	return name
}

// lowerCamel lowers only the leading word run of an already-rendered
// PascalCase Go name (a multi-letter initialism counts as one word run:
// "IDTokenType" lowers to "idTokenType", not "iDTokenType") and leaves
// every later word exactly as rendered. It reuses splitWords on the
// rendered name itself, which correctly finds the same word boundary a
// human reads: the initialism table is not consulted again here, since the
// name arriving is already in its final, corrected spelling.
func lowerCamel(name string) string {
	words := splitWords(name)
	if len(words) == 0 {
		return name
	}
	words[0] = strings.ToLower(words[0])
	return strings.Join(words, "")
}

// snakeCase renders a PascalCase message name into the lowercase,
// underscore-separated form its generated file is named after
// ("CompileOther" -> "compile_other"). It reuses splitWords's own case- and
// digit-boundary rules — the same ones the identifier transform runs on —
// rather than a simpler per-rune scan, so a digit run and an all-caps
// initialism split the same way here as everywhere else in this file:
// "Get15118EVCertificate" becomes "get_15118_ev_certificate", not
// "get15118_e_v_certificate".
func snakeCase(name string) string {
	words := splitWords(name)
	for i, w := range words {
		words[i] = strings.ToLower(w)
	}
	return strings.Join(words, "_")
}

// splitToPhrase renders a PascalCase message name into the space-separated
// banner phrase ("WidgetCharge" -> "Widget Charge"). No initialism
// correction or case change is applied beyond splitWords's own boundary
// rules: the banner reproduces the message name's own casing, it does not
// re-derive it.
func splitToPhrase(name string) string {
	return strings.Join(splitWords(name), " ")
}

// enumValueSegments splits a raw schema enum value on runs of
// non-alphanumeric characters, discarding the separators themselves. This
// is deliberately a different, simpler split than splitWords: an enum
// value like "Transaction.Begin" or "L1-N" is segmented by its own
// separator punctuation, not by a case-boundary scan.
func enumValueSegments(value string) []string {
	var segments []string
	var current []rune
	flush := func() {
		if len(current) > 0 {
			segments = append(segments, string(current))
			current = nil
		}
	}
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			current = append(current, r)
		} else {
			flush()
		}
	}
	flush()
	return segments
}

// enumValueGoWords renders the words that, concatenated after an enum's own
// Go type name, produce that enum's const name for one raw schema value:
// each punctuation-delimited segment is corrected against the initialism
// table exactly as an identifier word is (matchInitialism, else
// titleCaseWord — which preserves a segment's own interior casing, since it
// only ever touches the leading rune).
func enumValueGoWords(value string, config TransformConfig) []string {
	segments := enumValueSegments(value)
	out := make([]string, len(segments))
	for i, seg := range segments {
		if canon, ok := matchInitialism(seg, config.Initialisms); ok {
			out[i] = canon
		} else {
			out[i] = titleCaseWord(seg)
		}
	}
	return out
}
