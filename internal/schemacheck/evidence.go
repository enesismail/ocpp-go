package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
	"sort"
	"strings"
)

// Some rows carry no account from the comparison at all. `UNEXPLAINED` is the
// class for exactly that: the comparator states, in so many words, that nothing
// it knows how to check accounted for the row. Its rule is empty by contract,
// because there is no rule.
//
// A judgement on such a row is therefore pinned to nothing. Class and rule are
// what every other judgement is checked against, and here the class says only
// "no answer" and the rule says nothing at all — so the underlying facts can
// change completely while both pins keep matching. The real case: the accepted
// value set of `TriggerMessageRequest.RequestedMessage` cannot be extracted
// (its validator switches on constants imported from other packages), the
// judgement that it matches the schema's enum was made by hand, and deleting a
// value from that switch leaves the row `UNEXPLAINED` with an empty rule —
// unchanged pins, stale judgement, an implementation that now rejects a
// schema-legal value.
//
// Evidence closes that. A hand-made judgement standing in for a comparator
// account records the facts it was made from, in a form this file can re-read
// on every run, and a change in those facts fails the run and asks for the
// judgement to be made again.
//
// `IDENTICAL` is the other class with an empty rule and is deliberately not
// treated this way. Its emptiness means the opposite thing: the comparison ran
// every check it has and found no difference, so the class name is itself the
// full positive assertion, and any change to the row moves it out of the class
// — which the class pin already catches. `UNEXPLAINED` is an admission that
// the instrument gave up; `IDENTICAL` is a claim that it did not.

// A judgement that rests on what a validator does rests on three separate
// facts, each recorded in a different place in the tree, and pinning any two of
// them leaves the third free to move:
//
//	go-validate       the field names a tag
//	validator-binding the tag is bound to a function (or to nothing)
//	validator-body    that function decides what it decides
//
// Rebinding the tag to a new, reject-all function leaves the field's tag
// untouched and leaves the previously-bound function's source untouched, so a
// judgement pinning only the ends of the chain survives it. The middle link is
// the binding, and the requirement below is structural rather than per-row: an
// entry that pins a body without pinning the binding that reaches it is
// rejected, whatever row it belongs to.
const (
	evidenceGoValidate       = "go-validate"
	evidenceValidatorBinding = "validator-binding"
	evidenceValidatorBody    = "validator-body"
)

var evidenceKinds = []string{evidenceGoValidate, evidenceValidatorBinding, evidenceValidatorBody}

// treeEvidence holds the tree-wide facts an evidence item can be checked
// against, read once per run from the same Go files the comparison reads.
type treeEvidence struct {
	// functionBodies maps "<file>:<function>" to that function's whole source
	// text, normalized. The whole function, not the values it happens to
	// compare against: which values a validator names is only half of what it
	// decides, and the other half is what it does with them. A fingerprint
	// taken over the case-clause expressions alone leaves the accepting branch
	// free to start rejecting — every schema-legal value refused, the recorded
	// values unchanged, the pin still matching.
	//
	// The text is kept verbatim rather than digested, for the same reason the
	// rule pins are: a failure has to say what moved. These are short
	// functions, so both versions fit in the failure message.
	functionBodies map[string]string
	// bindings maps a validator tag name to every registration of it in the
	// tree, as "<function> at <file>", sorted. The function is carried,
	// not just the site: which function a tag reaches is the fact a body pin
	// depends on, and rebinding a tag changes it while changing neither the
	// field that names the tag nor the source of the function that used to be
	// bound. An empty list is itself an assertion — the tag is bound to nothing
	// — which is what the unregistered-tag judgements in this corpus rest on.
	//
	// The line is deliberately absent from the compared form: where in a file a
	// registration sits is not what a judgement rests on, and including it
	// would fail the pin every time an unrelated edit above it moved a line.
	// bindingSites carries the same registrations with their lines, for the
	// failure message only — a reader still has to be sent to the exact place.
	bindings     map[string][]string
	bindingSites map[string][]string
}

// collectTreeEvidence reads the tree once and extracts the facts evidence
// items are checked against. It is called only when a classification registry
// is being applied, so a run that merges no judgements pays nothing for it.
func collectTreeEvidence(tree string) (*treeEvidence, error) {
	goFiles, err := listGoFiles(tree)
	if err != nil {
		return nil, err
	}
	evidence := &treeEvidence{
		functionBodies: map[string]string{},
		bindings:       map[string][]string{},
		bindingSites:   map[string][]string{},
	}
	for _, path := range goFiles {
		fset, file, err := parseASTFileWithPositions(path)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		for name, body := range functionBodies(fset, file) {
			evidence.functionBodies[path+":"+name] = body
		}
		for tag, bound := range validationBindings(fset, file) {
			for _, binding := range bound {
				evidence.bindings[tag] = append(evidence.bindings[tag], fmt.Sprintf("%s at %s", binding.function, path))
				evidence.bindingSites[tag] = append(evidence.bindingSites[tag], fmt.Sprintf("%s at %s:%s", binding.function, path, binding.line))
			}
		}
	}
	for tag := range evidence.bindings {
		sort.Strings(evidence.bindings[tag])
		sort.Strings(evidence.bindingSites[tag])
	}
	return evidence, nil
}

// functionBodies renders every top-level function in file to normalized source
// text, keyed by name.
//
// Normalization is what stops the pin from firing on changes that are not
// changes: the text comes back through go/format, so reindentation and spacing
// churn produce identical output; the doc comment is dropped, because what a
// function is documented to do is not what it does; and blank lines are
// dropped, because printing a declaration on its own already loses interior
// comments and leaves their blank lines behind, which would otherwise make a
// comment edit look like a behavioural one. Everything that remains is the
// declaration and its statements, and any change to those changes the text.
func functionBodies(fset *token.FileSet, file *ast.File) map[string]string {
	bodies := map[string]string{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		undocumented := *fn
		undocumented.Doc = nil
		var buf bytes.Buffer
		if err := format.Node(&buf, fset, &undocumented); err != nil {
			continue
		}
		bodies[fn.Name.Name] = dropBlankLines(buf.String())
	}
	return bodies
}

// dropBlankLines removes empty and whitespace-only lines and any trailing
// whitespace, leaving one normalized line per line of code.
func dropBlankLines(text string) string {
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, " \t\r")
		if line == "" {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

type validationBinding struct {
	function string
	line     string
}

// validationBindings finds every RegisterValidation call in file and reports,
// per tag name, what each call binds the tag to and where. A tag registered
// more than once in one file yields more than one binding: the library lets a
// later registration silently replace an earlier one, so recording only the
// last would hide exactly the edit most likely to change behaviour.
func validationBindings(fset *token.FileSet, file *ast.File) map[string][]validationBinding {
	bindings := map[string][]validationBinding{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "RegisterValidation" {
			return true
		}
		tag, ok := stringLiteralValue(call.Args[0])
		if !ok {
			return true
		}
		bindings[tag] = append(bindings[tag], validationBinding{
			function: boundFunctionName(call.Args[1]),
			line:     fmt.Sprintf("%d", fset.Position(call.Pos()).Line),
		})
		return true
	})
	return bindings
}

// boundFunctionName names what a registration binds a tag to. A plain
// identifier is the usual and the useful case — it is the name a body pin is
// keyed on. Anything else (a function literal, a method value) has no name to
// pin a body by, so its source text is reported instead, flattened to one line
// so a binding list stays one entry per line.
func boundFunctionName(expr ast.Expr) string {
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return strings.Join(strings.Fields(exprString(expr)), " ")
}

// bindingFunction recovers the function name from a recorded binding entry,
// which is what lets the chain rule check that a pinned body is the body of a
// function the pinned binding actually reaches.
func bindingFunction(binding string) (string, bool) {
	name, _, ok := strings.Cut(binding, " at ")
	if !ok || name == "" {
		return "", false
	}
	return name, true
}

// bindingFile recovers the file from a recorded binding entry, so the body
// location a binding implies can be derived from it.
func bindingFile(binding string) (string, bool) {
	_, file, ok := strings.Cut(binding, " at ")
	if !ok || file == "" {
		return "", false
	}
	return file, true
}

// verifyEvidence re-reads each pinned fact and reports every one that no longer
// holds, quoting both what was recorded and what was found — the same shape the
// rule pins fail in, because a judgement that has to be made again is only
// actionable if the failure says what moved.
func verifyEvidence(subject string, items []evidenceItem, field Field, tree *treeEvidence) []string {
	var failures []string
	for _, item := range items {
		if item.Kind == evidenceValidatorBody {
			observed, ok := tree.functionBodies[item.Location]
			if !ok {
				failures = append(failures, fmt.Sprintf(
					"%s: no function at %s, so the evidence cannot be checked", subject, item.Location))
				continue
			}
			recorded := dropBlankLines(item.ExpectedText)
			if recorded != observed {
				failures = append(failures, fmt.Sprintf(
					"%s: the %s evidence at %s has changed, so the hand-made judgement resting on it has to be made again%s",
					subject, item.Kind, item.Location, describeBodyChange(recorded, observed)))
			}
			continue
		}
		observed, err := observeEvidence(item, field, tree)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", subject, err))
			continue
		}
		if !equalStrings(item.Expected, observed) {
			where := ""
			if item.Kind == evidenceValidatorBinding {
				where = fmt.Sprintf(" (registration sites: %v)", orEmpty(tree.bindingSites[item.Location]))
			}
			failures = append(failures, fmt.Sprintf(
				"%s: the %s evidence at %s has changed, so the hand-made judgement resting on it has to be made again — recorded %v, found %v%s",
				subject, item.Kind, item.Location, item.Expected, observed, where))
		}
	}
	return failures
}

// bodyPrintLimit is how many lines of a changed function are printed in full
// before the failure switches to naming the differing lines instead. The
// validators this pins are a dozen lines at most, so in practice both versions
// are printed whole and the reader diffs them by eye.
const bodyPrintLimit = 40

// describeBodyChange renders the difference between a recorded and an observed
// function, which is the whole point of recording the text rather than a digest:
// a reader has to be able to see what changed to decide whether the judgement
// still holds.
func describeBodyChange(recorded, observed string) string {
	recordedLines := strings.Split(recorded, "\n")
	observedLines := strings.Split(observed, "\n")
	if len(recordedLines) <= bodyPrintLimit && len(observedLines) <= bodyPrintLimit {
		return fmt.Sprintf("\n    recorded:\n%s\n    found:\n%s", indentLines(recordedLines), indentLines(observedLines))
	}
	var diff []string
	for i := 0; i < len(recordedLines) || i < len(observedLines); i++ {
		var was, now string
		if i < len(recordedLines) {
			was = recordedLines[i]
		}
		if i < len(observedLines) {
			now = observedLines[i]
		}
		if was != now {
			diff = append(diff, fmt.Sprintf("      line %d: recorded %q, found %q", i+1, was, now))
		}
	}
	return "\n" + strings.Join(diff, "\n")
}

func indentLines(lines []string) string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		out = append(out, "      "+line)
	}
	return strings.Join(out, "\n")
}

func observeEvidence(item evidenceItem, field Field, tree *treeEvidence) ([]string, error) {
	switch item.Kind {
	case evidenceGoValidate:
		return append([]string(nil), fieldLevelValidateTokens(field.Go.Validate)...), nil
	case evidenceValidatorBinding:
		return orEmpty(tree.bindings[item.Location]), nil
	default:
		return nil, fmt.Errorf("unknown evidence kind %q", item.Kind)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func stringLiteralValue(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	return strings.Trim(lit.Value, "`\""), true
}
