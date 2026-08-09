package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// A comparison says how a hand-written field differs from its schema; it does
// not say whether that difference is a defect, who introduced it, or how much
// it matters. Those are judgements made by reading the schema, this project's
// own change log and the upstream release the tree derives from, and they
// cannot be derived from the two sides of the comparison alone.
//
// This file is where those judgements enter the report: a registry file,
// written and reviewed by hand, that the run reads and merges onto the rows it
// computed. Keeping them in a file the tool consumes — rather than editing the
// generated report afterwards — is what makes the report reproducible: the
// comparison is recomputed from the tree and the schemas, the judgements are
// recomputed from the registry, and re-running the same two inputs reproduces
// the same bytes.
//
// The registry is validated against the run it is merged onto rather than
// trusted, because a registry and a tree drift apart silently: a row that
// disappears leaves an entry pointing at nothing, and a row that appears takes
// a default that was never reviewed for it. Both are reported as failures
// naming the specific entries, and neither is repaired by guessing.

const (
	SPEC_FIDELITY      = "SPEC_FIDELITY"
	DELIBERATE         = "DELIBERATE"
	UPSTREAM_INHERITED = "UPSTREAM_INHERITED"
	// NOT_A_DIVERGENCE is the judgement for a row that is not a difference at
	// all — the hand-written field says what the schema says, or a difference
	// the comparison could not settle mechanically was checked by hand and
	// found to be none. The three registries above ask where a divergence came
	// from, a question such a row does not raise; recording the answer anyway
	// is what lets every row be accounted for, so a row that later becomes a
	// difference cannot slip in unread.
	NOT_A_DIVERGENCE = "NOT_A_DIVERGENCE"

	BLOCKING_INTEROP = "BLOCKING_INTEROP"
	CORRECTNESS      = "CORRECTNESS"
	COSMETIC         = "COSMETIC"
	// NONE is NOT_A_DIVERGENCE's severity, and is only ever paired with it:
	// there is no such thing as a finding of no severity, nor a non-difference
	// that has one.
	NONE = "NONE"
)

// classificationValues and severityValues are the two closed vocabularies a
// registry entry may use. An unrecognized value is a typo or an invented
// category, and either would put a word in the report that nothing else in it
// defines.
var (
	classificationValues = []string{SPEC_FIDELITY, DELIBERATE, UPSTREAM_INHERITED, NOT_A_DIVERGENCE}
	severityValues       = []string{BLOCKING_INTEROP, CORRECTNESS, COSMETIC, NONE}
	comparisonClasses    = []string{IDENTICAL, SCHEMA_FAITHFUL_CHANGE, FORK_BUG, ADDITIVE, OVERRIDE_CANDIDATE, STRUCT_VALIDATOR, UNEXPLAINED}
	messageSides         = []string{"request", "response"}
)

// classificationRegistry is the on-disk form of the hand-made judgements.
//
// Version is the report version the registry was written against, checked
// against the run's own so a registry cannot be merged onto a different
// corpus, where its message and field names would match by coincidence or not
// at all.
//
// Classes carries one judgement for a whole comparison class — the shape a
// large, homogeneous group of rows takes, where repeating the same three
// values per row would obscure rather than document the reasoning. Fields
// carries per-row judgements, and always wins over the class default.
//
// RequireExplicit names the classes no default may cover, so every row of them
// has to be judged individually: the classes whose rows are the findings
// themselves, where a default would silently rate a defect nobody read.
type classificationRegistry struct {
	Version         string                  `yaml:"version"`
	Classes         map[string]classDefault `yaml:"classes"`
	RequireExplicit []string                `yaml:"requireExplicit"`
	Fields          []fieldClassification   `yaml:"fields"`
}

// classDefault is one class-wide judgement, together with the exact set of
// rows it was reviewed against.
//
// Covers is that set, written out one entry per row and sorted by row, and it
// is the guard that stops the registry silently absorbing rows nobody read. A
// count would not do it: a tree edit that removes one row of a class and adds
// another of the same class leaves the count untouched, and the new row would
// inherit a judgement written about a different field. Naming the rows makes
// that swap two failures — one row listed that no longer exists, one row
// present that is not listed — and makes the failure message say which.
//
// Each entry also pins the row's rule, for the reason given on coveredRow: a
// class is not a defect, and two rows of the same class can differ in what is
// actually wrong with them. That is not hypothetical here — the
// override-candidate class holds both numeric floors the schema does not
// declare and value-syntax constraints it does not declare, which are separate
// findings with separate reasoning.
//
// The rows are sorted so the file diffs cleanly and can be regenerated
// mechanically, and unique so no row is claimed twice.
type classDefault struct {
	Classification string       `yaml:"classification"`
	Severity       string       `yaml:"severity"`
	Note           string       `yaml:"note"`
	Covers         []coveredRow `yaml:"covers"`
}

// coveredRow is one row a class default was reviewed against: its key, and the
// rule the comparison stated about it at the time.
//
// The rule is the fingerprint of what is actually wrong with the row, and it is
// pinned for the same reason a per-row judgement pins one. The comparison class
// says what kind of answer the comparator reached; the rule says which defect it
// reached that answer about, parameters included. A row can keep its class while
// its defect changes completely — a length bound that becomes a wrong type is
// still a contradiction of the schema, a numeric floor that becomes a syntax
// constraint is still stricter than the schema — and the judgement written about
// the first would then be printed beside the second.
//
// Rule text is the right fingerprint because it is a deterministic function of
// the two sides being compared and of nothing else: no timestamps, no paths, no
// iteration order. Two runs over the same tree and schemas produce the same
// text, which is what the reports' byte-identity across regenerations already
// depends on, so pinning it adds no instability of its own. It changes exactly
// when what the comparison says about the row changes, which is exactly when the
// judgement needs reading again.
type coveredRow struct {
	Row  string `yaml:"row"`
	Rule string `yaml:"rule"`
	// Evidence is the same mechanism the per-row entries carry, available here
	// so the requirement is one rule and not two. No covered row needs it
	// today: the classes with defaults are all classes the comparator has an
	// account for.
	Evidence []evidenceItem `yaml:"evidence,omitempty"`
}

// evidenceItem is one fact a hand-made judgement was made from, recorded so it
// can be re-read on every run. Kind selects how it is checked, Location names
// what to look at where the kind needs it, and Expected is the content
// recorded when the judgement was made. See evidence.go for why judgements on
// rows the comparator has no account for need this and other judgements do not.
type evidenceItem struct {
	Kind     string   `yaml:"kind"`
	Location string   `yaml:"location,omitempty"`
	Expected []string `yaml:"expected,omitempty"`
	// ExpectedText carries the one kind of evidence that is a text rather than
	// a set: a function's whole source. It is written as a YAML block scalar,
	// so the registry shows the function as a reader would read it and a
	// changed line shows up as a changed line in the diff.
	ExpectedText string `yaml:"expectedText,omitempty"`
}

// fieldClassification is one per-row judgement, addressed the way the report
// addresses a row: the message's feature name, which side of it, and the
// field's own dotted path (empty for a rule that belongs to the message root
// rather than to any one field).
type fieldClassification struct {
	Message string `yaml:"message"`
	Side    string `yaml:"side"`
	Path    string `yaml:"path"`
	// Class records the comparison class the judgement was written against.
	// It is checked, not decoration: a judgement reasons about a specific
	// difference, and a row whose comparison has since changed is a different
	// difference wearing the same address. Without it a field that turned from
	// identical into a contradiction would keep whatever it was rated before,
	// and the report would carry a severity nobody assigned to what the row
	// now says.
	Class string `yaml:"class"`
	// Rule pins the defect the judgement was written about, as the comparison
	// stated it. Class alone is too coarse: a row can stay in its class while
	// what is wrong with it changes entirely, and the judgement's own wording
	// — which names a bound, a token, a type — would then describe something
	// the row no longer says. See coveredRow for why the rule text is the
	// fingerprint rather than some digest of the row.
	Rule string `yaml:"rule"`
	// Evidence records the facts a hand-made judgement was made from, for the
	// rows where the comparator reached no account and the rule is therefore
	// empty. Required there, allowed anywhere.
	Evidence       []evidenceItem `yaml:"evidence,omitempty"`
	Classification string         `yaml:"classification"`
	Severity       string         `yaml:"severity"`
	Note           string         `yaml:"note"`
}

func (f fieldClassification) key() rowKey {
	return rowKey{message: f.Message, side: f.Side, path: f.Path}
}

func (f fieldClassification) String() string {
	return f.key().String()
}

// rowKey addresses one row of the report the way a person would name it, and
// is also the form written into a class default's coverage list: the message's
// feature name, which side of it, and the field's dotted path. A rule that
// belongs to the message root rather than to a field has no path, and its key
// is the two-part form — nothing else in the grammar has two parts, so the two
// spellings never collide.
type rowKey struct {
	message string
	side    string
	path    string
}

func (k rowKey) String() string {
	if k.path == "" {
		return k.message + "." + k.side
	}
	return k.message + "." + k.side + "." + k.path
}

// parseRowKey reads a coverage entry back into the row it addresses. Feature
// names and sides contain no dots and the path may contain many, so the split
// is bounded at three parts and the remainder is the path, whole.
func parseRowKey(text string) (rowKey, error) {
	parts := strings.SplitN(text, ".", 3)
	if len(parts) < 2 {
		return rowKey{}, fmt.Errorf("%q is not a row key (expected Message.side or Message.side.path)", text)
	}
	key := rowKey{message: parts[0], side: parts[1]}
	if len(parts) == 3 {
		key.path = parts[2]
	}
	if key.message == "" {
		return rowKey{}, fmt.Errorf("%q names no message", text)
	}
	if !containsString(messageSides, key.side) {
		return rowKey{}, fmt.Errorf("%q names side %q, which is neither %s", text, key.side, strings.Join(messageSides, " nor "))
	}
	return key, nil
}

// loadClassificationRegistry reads and decodes a registry file. Unknown fields
// are rejected rather than ignored: a misspelled key in a hand-written file
// would otherwise take a judgement out of the report without saying so.
func loadClassificationRegistry(path string) (*classificationRegistry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read classifications %s: %w", path, err)
	}
	defer file.Close()

	dec := yaml.NewDecoder(file)
	dec.KnownFields(true)
	var registry classificationRegistry
	if err := dec.Decode(&registry); err != nil {
		return nil, fmt.Errorf("parse classifications %s: %w", path, err)
	}
	return &registry, nil
}

// applyClassifications merges registry onto report's rows and reports every
// way the two disagree, as sentences naming the specific entry or row. It
// either annotates the whole report or changes nothing: a registry that does
// not describe this run is not partially applied, since a half-annotated
// report reads exactly like a fully annotated one whose missing rows were
// judged to be nothing.
func applyClassifications(report *Report, registry *classificationRegistry, tree *treeEvidence) error {
	if failures := validateRegistry(registry); len(failures) > 0 {
		return registryError(failures)
	}
	if registry.Version != report.Version {
		return registryError([]string{fmt.Sprintf(
			"registry was written for %s but this run compared %s, so its entries describe a different corpus",
			registry.Version, report.Version)})
	}

	explicit := map[rowKey]fieldClassification{}
	var failures []string
	for _, entry := range registry.Fields {
		if _, dup := explicit[entry.key()]; dup {
			failures = append(failures, fmt.Sprintf("%s is listed more than once, so which judgement applies is undecided", entry))
			continue
		}
		explicit[entry.key()] = entry
	}

	requireExplicit := map[string]bool{}
	for _, class := range registry.RequireExplicit {
		requireExplicit[class] = true
	}

	// coveredBy maps every row a class default claims to the class that claims
	// it. Validation has already established that the keys parse, are unique
	// within a class and are claimed by only one class, so a lookup here has
	// exactly one answer.
	type coverage struct {
		class    string
		rule     string
		evidence []evidenceItem
	}
	coveredBy := map[rowKey]coverage{}
	for _, class := range sortedClassDefaultKeys(registry.Classes) {
		for _, entry := range registry.Classes[class].Covers {
			key, err := parseRowKey(entry.Row)
			if err != nil {
				continue
			}
			coveredBy[key] = coverage{class: class, rule: entry.Rule, evidence: entry.Evidence}
		}
	}

	used := map[rowKey]bool{}
	covered := map[rowKey]bool{}
	var unjudged []string

	for m := range report.Messages {
		message := &report.Messages[m]
		for _, side := range []struct {
			name   string
			fields []Field
		}{
			{"request", message.Request.Fields},
			{"response", message.Response.Fields},
		} {
			for f := range side.fields {
				field := &side.fields[f]
				key := rowKey{message: message.FeatureName, side: side.name, path: field.Path}
				if entry, ok := explicit[key]; ok {
					used[key] = true
					if entry.Class != field.Class {
						unjudged = append(unjudged, fmt.Sprintf(
							"%s was judged as %s but this run classifies it %s, so the judgement was written about a different comparison",
							key, entry.Class, field.Class))
						continue
					}
					if entry.Rule != field.Rule {
						unjudged = append(unjudged, fmt.Sprintf(
							"%s was judged about the defect %q but this run reports %q, so the judgement was written about a different defect",
							key, entry.Rule, field.Rule))
						continue
					}
					if stale := verifyEvidence(key.String(), entry.Evidence, *field, tree); len(stale) > 0 {
						unjudged = append(unjudged, stale...)
						continue
					}
					field.Classification = entry.Classification
					field.Severity = entry.Severity
					field.ClassificationNote = entry.Note
					continue
				}
				if entry, ok := coveredBy[key]; ok {
					covered[key] = true
					if entry.class != field.Class {
						unjudged = append(unjudged, fmt.Sprintf(
							"%s is listed under the %s default but this run classifies it %s, so the judgement it would take was written about a different comparison",
							key, entry.class, field.Class))
						continue
					}
					if entry.rule != field.Rule {
						unjudged = append(unjudged, fmt.Sprintf(
							"%s is listed under the %s default against the defect %q but this run reports %q, so the judgement it would take was written about a different defect",
							key, entry.class, entry.rule, field.Rule))
						continue
					}
					if stale := verifyEvidence(key.String(), entry.evidence, *field, tree); len(stale) > 0 {
						unjudged = append(unjudged, stale...)
						continue
					}
					fallback := registry.Classes[entry.class]
					field.Classification = fallback.Classification
					field.Severity = fallback.Severity
					field.ClassificationNote = fallback.Note
					continue
				}
				if requireExplicit[field.Class] {
					unjudged = append(unjudged, fmt.Sprintf(
						"%s is %s, a class the registry requires be judged row by row, and it has no entry",
						key, field.Class))
					continue
				}
				if _, ok := registry.Classes[field.Class]; ok {
					unjudged = append(unjudged, fmt.Sprintf(
						"%s is %s but the %s default does not list it, so it is a row that appeared since the registry was written and would inherit a judgement made about other rows",
						key, field.Class, field.Class))
					continue
				}
				// Every row is judged or the run fails. A row nothing accounts
				// for is not a row that was decided to be nothing: it is a row
				// nobody read, and leaving it unannotated is how a new
				// difference reaches the report without ever entering the
				// counts the report is read for.
				unjudged = append(unjudged, fmt.Sprintf(
					"%s is %s and the registry judges it neither individually nor by class, so nothing states whether it is a finding",
					key, field.Class))
			}
		}
	}
	failures = append(failures, unjudged...)

	for _, entry := range registry.Fields {
		if !used[entry.key()] {
			failures = append(failures, fmt.Sprintf(
				"%s has a judgement but no such row exists in this run, so the registry is describing a tree that has since changed", entry))
		}
	}

	for _, class := range sortedClassDefaultKeys(registry.Classes) {
		for _, entry := range registry.Classes[class].Covers {
			key, err := parseRowKey(entry.Row)
			if err != nil {
				continue
			}
			if !covered[key] {
				failures = append(failures, fmt.Sprintf(
					"%s is listed under the %s default but no such row exists in this run, so the registry is describing a tree that has since changed", key, class))
			}
		}
	}

	sort.Strings(failures)
	if len(failures) > 0 {
		clearClassifications(report)
		return registryError(failures)
	}
	return nil
}

// validateRegistry checks everything about the registry that can be checked
// without a run to compare it against: closed vocabularies, addressable sides,
// well-formed and non-overlapping coverage lists, and the one contradiction
// the two class-level mechanisms can express — a class that both carries a
// default and is declared to need row-by-row judgement.
func validateRegistry(registry *classificationRegistry) []string {
	var failures []string
	if registry.Version == "" {
		failures = append(failures, "registry names no version, so nothing states which corpus it was written against")
	}

	requireExplicit := map[string]bool{}
	for _, class := range registry.RequireExplicit {
		if !containsString(comparisonClasses, class) {
			failures = append(failures, fmt.Sprintf("requireExplicit names %q, which is not a comparison class", class))
			continue
		}
		requireExplicit[class] = true
	}

	explicit := map[rowKey]bool{}
	for _, entry := range registry.Fields {
		explicit[entry.key()] = true
	}

	claimedBy := map[rowKey]string{}
	for _, class := range sortedClassDefaultKeys(registry.Classes) {
		fallback := registry.Classes[class]
		if !containsString(comparisonClasses, class) {
			failures = append(failures, fmt.Sprintf("classes names %q, which is not a comparison class", class))
		}
		if requireExplicit[class] {
			failures = append(failures, fmt.Sprintf(
				"%s carries a class-wide default and is also listed in requireExplicit; the two say opposite things about the same class", class))
		}
		failures = append(failures, validateJudgement("the "+class+" default", fallback.Classification, fallback.Severity)...)

		if len(fallback.Covers) == 0 {
			failures = append(failures, fmt.Sprintf(
				"the %s default lists no covers, so it states no set of rows it was reviewed against and would apply to whatever this run produced", class))
			continue
		}
		seen := map[rowKey]bool{}
		for i, entry := range fallback.Covers {
			key, err := parseRowKey(entry.Row)
			if err != nil {
				failures = append(failures, fmt.Sprintf("the %s default's covers list: %v", class, err))
				continue
			}
			if i > 0 && fallback.Covers[i-1].Row >= entry.Row {
				failures = append(failures, fmt.Sprintf(
					"the %s default's covers list is not in sorted order at %q, so it cannot be regenerated or diffed reliably", class, entry.Row))
			}
			if seen[key] {
				failures = append(failures, fmt.Sprintf("%s is listed twice under the %s default", key, class))
				continue
			}
			seen[key] = true
			if other, dup := claimedBy[key]; dup {
				failures = append(failures, fmt.Sprintf(
					"%s is listed under both the %s and %s defaults, so which judgement it takes is undecided", key, other, class))
				continue
			}
			claimedBy[key] = class
			failures = append(failures, validateEvidence(key.String(), class, entry.Rule, entry.Evidence)...)
			if explicit[key] {
				failures = append(failures, fmt.Sprintf(
					"%s has a per-row judgement and is also listed under the %s default; the per-row one would win and the coverage entry would be a claim nothing checks", key, class))
			}
		}
	}

	for _, entry := range registry.Fields {
		if entry.Message == "" {
			failures = append(failures, "a field entry names no message")
		}
		if !containsString(messageSides, entry.Side) {
			failures = append(failures, fmt.Sprintf("%s names side %q, which is neither %s", entry, entry.Side, strings.Join(messageSides, " nor ")))
		}
		if !containsString(comparisonClasses, entry.Class) {
			failures = append(failures, fmt.Sprintf("%s records class %q, which is not a comparison class", entry, entry.Class))
		}
		failures = append(failures, validateEvidence(entry.String(), entry.Class, entry.Rule, entry.Evidence)...)
		failures = append(failures, validateJudgement(entry.String(), entry.Classification, entry.Severity)...)
	}
	sort.Strings(failures)
	return failures
}

// unaccountedClasses are the comparison classes that state no finding of their
// own. A judgement on a row of one of them is standing in for an account the
// comparator did not reach, so it has to record the facts it was made from —
// otherwise class and rule both match forever while the facts move underneath.
// The reasoning, and why IDENTICAL's own empty rule is a different thing, is on
// the block comment at the top of evidence.go.
var unaccountedClasses = []string{UNEXPLAINED}

// validateEvidence checks an evidence block's own well-formedness, and enforces
// the one place a block is not optional.
func validateEvidence(subject, class, rule string, items []evidenceItem) []string {
	var failures []string
	for _, item := range items {
		if !containsString(evidenceKinds, item.Kind) {
			failures = append(failures, fmt.Sprintf("%s records evidence of kind %q, which is not one of %s",
				subject, item.Kind, strings.Join(evidenceKinds, ", ")))
			continue
		}
		if item.Kind != evidenceGoValidate && item.Location == "" {
			failures = append(failures, fmt.Sprintf("%s records %s evidence with no location, so there is nothing to re-read", subject, item.Kind))
		}
		// Each kind pins either a set or a text, never both and never neither:
		// an item recording the wrong one, or nothing, reads as a pinned fact
		// while pinning nothing.
		if item.Kind == evidenceValidatorBody {
			if strings.TrimSpace(item.ExpectedText) == "" {
				failures = append(failures, fmt.Sprintf("%s records %s evidence with no expectedText, so no source is pinned", subject, item.Kind))
			}
			if len(item.Expected) > 0 {
				failures = append(failures, fmt.Sprintf("%s records %s evidence with a list; this kind pins a function's source as expectedText", subject, item.Kind))
			}
			continue
		}
		if item.ExpectedText != "" {
			failures = append(failures, fmt.Sprintf("%s records %s evidence with expectedText; this kind pins a set as expected", subject, item.Kind))
		}
	}
	failures = append(failures, validateEvidenceChain(subject, items)...)
	if rule == "" && containsString(unaccountedClasses, class) && len(items) == 0 {
		failures = append(failures, fmt.Sprintf(
			"%s is %s with no rule and records no evidence, so the judgement is pinned to nothing and would survive any change to the facts it was made from",
			subject, class))
	}
	return failures
}

// validateEvidenceChain enforces the structural requirement: a judgement that
// rests on a validator has to pin the whole chain from the field to the
// behaviour, not its ends. Three links, three checks, and each of them is a way
// a judgement has already been found to survive a change it should not have.
//
//   - the field names a tag: without it the field could name a different
//     validator entirely and every other pin would still match;
//   - the tag reaches a function: without it the tag can be rebound to a new
//     function, leaving the old function's pinned source untouched;
//   - each reached function decides something: without it the bound function's
//     behaviour is unpinned, which is where this started.
//
// A binding pinned as empty asserts the tag reaches nothing, so it demands no
// body — there is none to pin.
func validateEvidenceChain(subject string, items []evidenceItem) []string {
	var failures []string
	var pinsValidator bool
	var tokensPinned bool
	bodies := map[string]bool{}
	boundFunctions := map[string]string{}

	for _, item := range items {
		switch item.Kind {
		case evidenceGoValidate:
			tokensPinned = true
		case evidenceValidatorBody:
			pinsValidator = true
			bodies[item.Location] = true
		case evidenceValidatorBinding:
			pinsValidator = true
			for _, binding := range item.Expected {
				name, ok := bindingFunction(binding)
				if !ok {
					failures = append(failures, fmt.Sprintf(
						"%s records the binding %q, which does not name a function; a binding reads \"<function> at <file>:<line>\"", subject, binding))
					continue
				}
				file, ok := bindingFile(binding)
				if !ok {
					failures = append(failures, fmt.Sprintf(
						"%s records the binding %q, which does not name a file; a binding reads \"<function> at <file>:<line>\"", subject, binding))
					continue
				}
				boundFunctions[file+":"+name] = binding
			}
		}
	}

	if !pinsValidator {
		return failures
	}
	if !tokensPinned {
		failures = append(failures, fmt.Sprintf(
			"%s pins a validator but not the field's own %s tokens, so the field could name a different tag with every other pin still matching",
			subject, evidenceGoValidate))
	}
	for location := range bodies {
		if _, bound := boundFunctions[location]; !bound {
			failures = append(failures, fmt.Sprintf(
				"%s pins the body at %s but no %s evidence shows a tag reaching it, so the tag could be rebound to another function and this pin would still match",
				subject, location, evidenceValidatorBinding))
		}
	}
	for location, binding := range boundFunctions {
		if !bodies[location] {
			failures = append(failures, fmt.Sprintf(
				"%s pins the binding %q but not that function's body, so what it decides is unpinned",
				subject, binding))
		}
	}
	return failures
}

func validateJudgement(subject, classification, severity string) []string {
	var failures []string
	if !containsString(classificationValues, classification) {
		failures = append(failures, fmt.Sprintf("%s carries classification %q, which is not one of %s", subject, classification, strings.Join(classificationValues, ", ")))
	}
	if !containsString(severityValues, severity) {
		failures = append(failures, fmt.Sprintf("%s carries severity %q, which is not one of %s", subject, severity, strings.Join(severityValues, ", ")))
	}
	// The two only ever appear together. A row rated NONE that is still called
	// a divergence, or one called NOT_A_DIVERGENCE that carries a severity,
	// would be counted by one axis of the report and not the other.
	if (classification == NOT_A_DIVERGENCE) != (severity == NONE) {
		failures = append(failures, fmt.Sprintf(
			"%s pairs classification %q with severity %q; %s and %s are only ever used together",
			subject, classification, severity, NOT_A_DIVERGENCE, NONE))
	}
	return failures
}

// clearClassifications undoes a partial merge, so a failed application leaves
// the report exactly as the comparison produced it.
func clearClassifications(report *Report) {
	for m := range report.Messages {
		for _, fields := range [][]Field{report.Messages[m].Request.Fields, report.Messages[m].Response.Fields} {
			for f := range fields {
				fields[f].Classification = ""
				fields[f].Severity = ""
				fields[f].ClassificationNote = ""
			}
		}
	}
}

func sortedClassDefaultKeys(m map[string]classDefault) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func registryError(failures []string) error {
	return fmt.Errorf("classifications do not describe this run:\n  - %s", strings.Join(failures, "\n  - "))
}
