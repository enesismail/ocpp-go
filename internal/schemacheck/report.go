package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// marshalReportJSON renders report as a deterministic JSON document:
// encoding/json marshals a struct's fields in declaration order and a slice
// in its own order, so byte-identical output across runs follows directly
// from every slice in report having already been sorted by the
// orchestration that built it — this function adds no ordering of its own,
// only stable formatting.
func marshalReportJSON(report *Report) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(report); err != nil {
		return nil, fmt.Errorf("marshal report: %w", err)
	}
	return buf.Bytes(), nil
}

// renderMarkdownReport writes the human-readable summary, verdict-first —
// the three roll-ups, then the complexity distribution, the class
// histogram, the per-message table, the FORK-BUG list grouped by sub-kind,
// the override candidates, the self-check table, and the method.
func renderMarkdownReport(report *Report, selfCheck selfCheckResult) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# %s divergence census\n\n", report.Version)
	fmt.Fprintf(&b, "Compares the hand-written `%s` message tree against the JSON Schema documents in `%s`, field by field.\n\n", report.GoTree, report.SchemaDir)

	writeProvenance(&b, report)

	b.WriteString("## Roll-ups\n\n")
	fmt.Fprintf(&b, "- **Epoch-break size**: %d breaking field(s) across %d of %d messages\n",
		report.Summary.BreakingFields, report.Summary.MessagesWithBreakingFields, report.Coverage.Messages)
	fmt.Fprintf(&b, "- **FORK-BUG count**: %d\n", report.Summary.ByClass.FORK_BUG)
	fmt.Fprintf(&b, "- **Override density**: %d of %d messages need at least one override (%.1f%%; a density above roughly one third is generally read as a sign the global mapping rules have failed)\n\n",
		report.Summary.MessagesNeedingOverrides, report.Coverage.Messages, report.Summary.OverrideDensity*100)

	b.WriteString("## Complexity distribution\n\n")
	d := report.Summary.ComplexityDistribution
	fmt.Fprintf(&b, "min=%d p25=%d median=%d p75=%d max=%d, over %d messages. %d are `prototype: true`.\n\n",
		d.Min, d.P25, d.Median, d.P75, d.Max, report.Coverage.Messages, report.Summary.PrototypeCount)
	b.WriteString("| Message | Prototype | Complexity |\n|---|---|---|\n")
	byComplexity := append([]Message(nil), report.Messages...)
	sort.SliceStable(byComplexity, func(i, j int) bool { return byComplexity[i].Complexity < byComplexity[j].Complexity })
	for _, m := range byComplexity {
		fmt.Fprintf(&b, "| %s | %t | %d |\n", m.FeatureName, m.Prototype, m.Complexity)
	}
	b.WriteString("\n")

	b.WriteString("## Class histogram\n\n")
	b.WriteString("| Class | Count |\n|---|---|\n")
	fmt.Fprintf(&b, "| IDENTICAL | %d |\n", report.Summary.ByClass.IDENTICAL)
	fmt.Fprintf(&b, "| SCHEMA_FAITHFUL_CHANGE | %d |\n", report.Summary.ByClass.SCHEMA_FAITHFUL_CHANGE)
	fmt.Fprintf(&b, "| FORK_BUG | %d |\n", report.Summary.ByClass.FORK_BUG)
	fmt.Fprintf(&b, "| ADDITIVE | %d |\n", report.Summary.ByClass.ADDITIVE)
	fmt.Fprintf(&b, "| OVERRIDE_CANDIDATE | %d |\n", report.Summary.ByClass.OVERRIDE_CANDIDATE)
	fmt.Fprintf(&b, "| STRUCT_VALIDATOR | %d |\n", report.Summary.ByClass.STRUCT_VALIDATOR)
	fmt.Fprintf(&b, "| UNEXPLAINED | %d |\n\n", report.Summary.ByClass.UNEXPLAINED)

	b.WriteString("## Per-message table\n\n")
	b.WriteString("| Message | Profile | Direction | Complexity | Fields |\n|---|---|---|---|---|\n")
	for _, m := range report.Messages {
		fmt.Fprintf(&b, "| %s | %s | %s | %d | %d |\n", m.FeatureName, m.Profile, m.Direction, m.Complexity, len(m.Request.Fields)+len(m.Response.Fields))
	}
	b.WriteString("\n")

	b.WriteString("## FORK-BUG list\n\n")
	writeClassList(&b, report, FORK_BUG, true)

	b.WriteString("## Override candidates\n\n")
	writeClassList(&b, report, OVERRIDE_CANDIDATE, false)

	writeStructValidatorList(&b, report)

	unexplained := collectClass(report, UNEXPLAINED)
	if len(unexplained) > 0 {
		b.WriteString("## Unexplained rows\n\n")
		for _, row := range unexplained {
			fmt.Fprintf(&b, "- `%s` %s.%s — no rule accounted for this row; needs triage.\n", row.message, row.side, row.displayPath())
		}
		b.WriteString("\n")
	}

	b.WriteString("## Self-check\n\n")
	b.WriteString("| Check | Claim | Expected | Actual | Status |\n|---|---|---|---|---|\n")
	for _, c := range report.SelfCheck {
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %s |\n", c.ID, c.Claim, c.Expected, c.Actual, c.Status)
	}
	comp := selfCheck.composition
	fmt.Fprintf(&b, "\nM2's full composition: %d object, %d plain-string, %d string-enum, %d integer, %d number, %d boolean, %d untyped, %d arrays (sums to %d).\n\n",
		comp.object, comp.plainString, comp.stringEnum, comp.integer, comp.number, comp.boolean, comp.untyped, comp.array, comp.total)
	if len(selfCheck.structuralConflicts) > 0 {
		b.WriteString("Structural conflicts found in the corpus-wide dedup pass:\n\n")
		for _, c := range selfCheck.structuralConflicts {
			fmt.Fprintf(&b, "- %s\n", c)
		}
		b.WriteString("\n")
	}
	if len(selfCheck.untypedPaths) > 0 {
		b.WriteString("Untyped-by-design properties:\n\n")
		for _, p := range selfCheck.untypedPaths {
			fmt.Fprintf(&b, "- %s\n", p)
		}
		b.WriteString("\n")
	}

	b.WriteString("## Shared types\n\n")
	if len(report.SharedTypes) == 0 {
		b.WriteString("None reached.\n\n")
	} else {
		b.WriteString("| Go type | Schema definition | Occurrences | Conflicts |\n|---|---|---|---|\n")
		for _, st := range report.SharedTypes {
			fmt.Fprintf(&b, "| %s | %s | %d | %d |\n", st.GoType, st.SchemaDefinition, st.Occurrences, len(st.StructuralConflicts))
		}
		b.WriteString("\n")
	}

	b.WriteString("## Method\n\n")
	b.WriteString("Messages are discovered by method set (a type implementing GetFeatureName/GetRequestType/GetResponseType), " +
		"cross-checked against every ocpp.NewProfile registration in the tree; naming conventions are never used for discovery. " +
		"Each request/response struct is walked field by field, following named struct types anywhere in the tree (not only the " +
		"declaring file) and array element types, with a registry of known custom-marshaler types (e.g. a DateTime wrapper) so a " +
		"field that marshals to a scalar is compared as that scalar rather than walked as a struct. Each field is paired against " +
		"the corresponding schema property by a JSON-name-dotted path and classified by a fixed priority of checks: presence, a " +
		"discovered struct-level validator, wire-type mismatch, enum value-set drift, optionality, constraint strictness, and " +
		"finally the declared-type shape a schema-faithful mapping would produce. Coverage, the self-check and every roll-up in " +
		"this document are computed straight from that classified field list — nothing here is asserted independently of it.\n\n")

	writeLimitations(&b, report)

	return b.String()
}

// writeStructValidatorList renders every struct-level validation rule the
// walk found, one entry per row, with the pointers a human needs to
// adjudicate it: which message and side it applies to, the path and Go field
// (or the message root, for a rule registered against the request/response
// struct itself), the package and file the struct is declared in, and the
// schema pointer the rule has no counterpart at. Unlike every other class,
// this one has no mechanical resolution at all — the schema vocabulary
// cannot express a cross-field rule — so each row is listed individually
// rather than only counted.
func writeStructValidatorList(b *strings.Builder, report *Report) {
	b.WriteString("## Struct-level validation rules\n\n")
	rows := collectClass(report, STRUCT_VALIDATOR)
	if len(rows) == 0 {
		b.WriteString("None.\n\n")
		return
	}
	fmt.Fprintf(b, "%d row(s). The JSON Schema vocabulary has no way to express a cross-field rule, so none of these can be "+
		"resolved mechanically: each needs a human decision on whether the rule is a constraint the schema simply cannot state "+
		"(keep it) or a hand-written stricture the schema does not support (drop it).\n\n", len(rows))
	b.WriteString("| Message | Side | Path | Go field | Message file | Schema pointer | Rule |\n|---|---|---|---|---|---|---|\n")
	for _, row := range rows {
		fmt.Fprintf(b, "| %s | %s | %s | %s | %s/%s | %s | %s |\n",
			row.message, row.side, row.displayPath(), row.field.Go.Name,
			row.goPackage, row.goFile, row.field.Schema.Pointer, row.field.Rule)
	}
	b.WriteString("\n")
}

// writeLimitations states what this comparison does not look at. It exists
// because the class histogram invites exactly one wrong reading: that a
// count of zero rows nothing accounted for means the two sides agree. It
// does not. The instrument compares *declarations* — Go struct fields and
// their tags against schema properties and their keywords — and every
// question below is outside that comparison entirely, so no row anywhere in
// this document is evidence either way about it.
func writeLimitations(b *strings.Builder, report *Report) {
	b.WriteString("## Limitations\n\n")
	fmt.Fprintf(b, "This is a comparison of declarations, not of behaviour. `UNEXPLAINED` is %d, which means every difference "+
		"found was accounted for by a named rule — it does **not** mean the hand-written tree and the schema agree in meaning. "+
		"The following are outside the comparison and are not analysed anywhere in this document:\n\n",
		report.Summary.ByClass.UNEXPLAINED)
	for _, limitation := range []string{
		"**Validator function bodies.** Only one thing is read out of a validator: the set of constant values a tag-registered " +
			"function's switch accepts, which is what makes enum value-set drift detectable. Nothing else a validator does is examined.",
		"**Cross-field constraints.** A rule that makes one field's validity depend on another's — a field required only for " +
			"certain values of a sibling, a mutually-exclusive pair, a conditional bound — has no vocabulary on either side. " +
			"Struct-level rules are listed above so a human can adjudicate them; they are not evaluated here. Conditional " +
			"requirements stated only in the specification prose, with no schema keyword behind them, are invisible to both sides " +
			"of the comparison.",
		"**Custom marshaler behaviour beyond the recorded wire type.** A type with a custom MarshalJSON/UnmarshalJSON pair is " +
			"compared as the wire type the registry records for it. The marshaler body is never read, so a marshaler that emits " +
			"something other than its registered wire type, or that renames or reshapes what it emits, would not be detected.",
		"**Runtime conformance.** No payload is marshalled, unmarshalled or validated. Nothing here is evidence that a message " +
			"round-trips, that validation accepts what the schema accepts, or that the two agree on any concrete document.",
		"**Constraint keywords outside the compared set.** Stricter/looser comparison is defined over `maxLength`, `minLength`, " +
			"`minimum`, `maximum`, `minItems` and `maxItems` only. `pattern` and `additionalProperties` are read and recorded but " +
			"not compared, and presence divergence is settled by the optionality rules rather than by strictness.",
		"**Documentation keywords.** `javaType`, `comment` and `description` are read and deliberately ignored; nothing they say " +
			"constrains anything here.",
		"**Anything the walk cannot reach.** Field pairing is by JSON name within a path. A field the Go tree never declares and " +
			"the schema never declares is invisible by construction, and a property reached only underneath a composite that has " +
			"no counterpart on the other side is reported once, at that composite, rather than once per property inside it.",
		"**Nothing found here is fixed here.** Every difference is recorded; no production file is changed by this tool, which " +
			"reads the Go tree and the schema directories and writes only to its output directory.",
	} {
		fmt.Fprintf(b, "- %s\n", limitation)
	}
	b.WriteString("\n")
}

// classRow is one classified field row lifted out of the per-message
// structure, carrying the message-level facts (feature name, side, and the
// Go package and file the struct is declared in) a flat, cross-message list
// needs in order to point a reader at the code.
type classRow struct {
	message   string
	side      string
	goPackage string
	goFile    string
	field     Field
}

// goSource spells where the row's Go field is declared. It is the field's
// own declaring file, not the message file: a row reached through a shared
// composite is declared in the package that declares the composite, and
// citing the message would send a reader to a file the field is not in.
func (r classRow) goSource() string {
	if r.field.Go.File == "" {
		return r.goPackage + "/" + r.goFile
	}
	if r.field.Go.Line == 0 {
		return r.field.Go.File
	}
	return fmt.Sprintf("%s:%d", r.field.Go.File, r.field.Go.Line)
}

// writeProvenance states what would have to be true for someone else to get
// this document back: the command, and the identity of the schema files it
// read. Without them the report names its inputs only by directory, which
// says nothing about which revision of a published schema set was actually
// compared.
func writeProvenance(b *strings.Builder, report *Report) {
	b.WriteString("## Provenance\n\n")
	fmt.Fprintf(b, "Reproduce with:\n\n```\n%s\n```\n\n", report.Invocation)
	if report.Classifications != "" {
		fmt.Fprintf(b, "Hand-made classification and severity judgements were merged from `%s`; the field-level comparison below reproduces from the command alone.\n\n", report.Classifications)
	}
	b.WriteString("Schema corpus read by that run:\n\n")
	b.WriteString("| Directory | Documents | SHA-256 over the directory's file digests |\n|---|---|---|\n")
	for _, dir := range report.SchemaCorpus {
		fmt.Fprintf(b, "| `%s` | %d | `%s` |\n", dir.Dir, dir.Files, dir.SHA256)
	}
	b.WriteString("\nEach digest is taken over the directory's `sha256  filename` lines, sorted, so it moves if any document changes, arrives, leaves or is renamed.\n\n")
}

// displayPath spells a row's path for a human list. Every row but one names
// a field; a rule registered against a message struct as a whole sits at the
// message root, whose path is empty, and is spelled as such rather than
// printed as nothing.
func (r classRow) displayPath() string {
	if r.field.Path == rootMessagePath {
		return "(message root)"
	}
	return r.field.Path
}

func collectClass(report *Report, class string) []classRow {
	var rows []classRow
	for _, m := range report.Messages {
		for _, f := range m.Request.Fields {
			if f.Class == class {
				rows = append(rows, classRow{message: m.FeatureName, side: "request", goPackage: m.GoPackage, goFile: m.GoFile, field: f})
			}
		}
		for _, f := range m.Response.Fields {
			if f.Class == class {
				rows = append(rows, classRow{message: m.FeatureName, side: "response", goPackage: m.GoPackage, goFile: m.GoFile, field: f})
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].message != rows[j].message {
			return rows[i].message < rows[j].message
		}
		if rows[i].side != rows[j].side {
			return rows[i].side < rows[j].side
		}
		return rows[i].field.Path < rows[j].field.Path
	})
	return rows
}

// writeClassList renders every field of class across all messages, grouped
// by sub-kind when groupBySubKind is set (the FORK-BUG list, grouped by the
// distinguishing token in Rule per the report's own convention — there is
// no dedicated sub-kind field, so the Rule text is the grouping key).
func writeClassList(b *strings.Builder, report *Report, class string, groupBySubKind bool) {
	rows := collectClass(report, class)
	if len(rows) == 0 {
		b.WriteString("None.\n\n")
		return
	}
	if !groupBySubKind {
		for _, row := range rows {
			fmt.Fprintf(b, "- `%s` %s.%s (%s, %s): %s\n", row.message, row.side, row.displayPath(), row.goSource(), row.field.Schema.Pointer, row.field.Rule)
		}
		b.WriteString("\n")
		return
	}
	byRule := map[string][]classRow{}
	var ruleOrder []string
	for _, row := range rows {
		key := subKindOf(row.field.Rule)
		if _, ok := byRule[key]; !ok {
			ruleOrder = append(ruleOrder, key)
		}
		byRule[key] = append(byRule[key], row)
	}
	sort.Strings(ruleOrder)
	for _, key := range ruleOrder {
		fmt.Fprintf(b, "**%s** (%d)\n\n", key, len(byRule[key]))
		for _, row := range byRule[key] {
			fmt.Fprintf(b, "- `%s` %s.%s (%s %s, %s): %s\n", row.message, row.side, row.displayPath(), row.field.Go.Name, row.goSource(), row.field.Schema.Pointer, row.field.Rule)
		}
		b.WriteString("\n")
	}
}

// subKindOf reduces a FORK_BUG row's Rule text to the short sub-kind label
// the class list groups by. Most FORK_BUG rules in classify.go lead with a
// short label before a colon ("wrong type: ...", "optionality lie: ...",
// "enum value-set drift: ...", "Go field not in schema: ...",
// "schema-required field missing from Go: ..."); the two constraint-bound
// rules (stricterResult/looserResult's sibling, looserResult, and the
// no-Go-bound case) do not, so they are matched by their own distinguishing
// phrase instead, falling back to the colon split for anything else.
func subKindOf(rule string) string {
	lower := strings.ToLower(rule)
	if strings.Contains(lower, "looser than schema") || strings.Contains(lower, "so it is looser than the schema") {
		return "constraint looser than schema"
	}
	if idx := strings.Index(rule, ":"); idx >= 0 {
		return rule[:idx]
	}
	return rule
}
