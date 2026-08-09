// Command schemacheck compares a hand-written OCPP Go message tree against
// the published JSON Schema documents for the same protocol version. For
// every field on every request/response struct it reports whether the
// hand-written field matches what the schema says, and — where the two
// differ — which of a small set of reasons explains the difference: a
// mechanical mapping rule the hand-written code simply predates, a place
// where the hand-written code contradicts the schema outright, a field the
// schema adds that the Go tree does not yet have, a place where the Go tree
// is deliberately stricter than the schema, a cross-field validation rule
// the schema has no equivalent for, or a difference nothing above explains.
//
// Usage:
//
//	schemacheck -tree <dir> -schemas <dir>[,<dir>...] -out <dir> [-format json|md|both] [-classifications <file>]
//
// -tree is the root of the Go message tree to read (e.g. ocpp2.0.1).
// -schemas is a comma-separated list of JSON Schema directories to compare
// it against — more than one lets a single run span a version whose schema
// set is itself split across several directories. -out is the directory the
// report is written into. -format selects which report file(s) to write:
// the machine-readable JSON document, the human-readable Markdown summary,
// or both (the default).
//
// -classifications names a YAML registry of hand-made judgements — whether a
// difference is a defect, where it came from and how much it matters — to
// merge onto the computed rows. Those judgements cannot be derived from the
// comparison, so they are read from a file that is reviewed and kept beside
// the report rather than typed into the generated document afterwards, which
// is what lets the whole report be regenerated from its inputs. The registry
// is checked against the run it is merged onto: an entry addressing a row this
// run does not have, or a class default covering a different number of rows
// than it was reviewed against, fails the run rather than annotating it.
//
// A report is written only when the run behind it is complete and
// self-consistent. If any message has no schema counterpart, if any schema
// file is consumed by no message, or if the run fails to reproduce the
// independently measured numbers recorded for its corpus, the command
// reports every such failure and exits non-zero without writing anything —
// an incomplete or unverified comparison is not published in a form a later
// reader could mistake for a good one.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	IDENTICAL              = "IDENTICAL"
	SCHEMA_FAITHFUL_CHANGE = "SCHEMA_FAITHFUL_CHANGE"
	FORK_BUG               = "FORK_BUG"
	ADDITIVE               = "ADDITIVE"
	OVERRIDE_CANDIDATE     = "OVERRIDE_CANDIDATE"
	STRUCT_VALIDATOR       = "STRUCT_VALIDATOR"
	UNEXPLAINED            = "UNEXPLAINED"
)

func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run parses flags and validates them; it is the seam CLI-level tests would
// exercise. usage is where flag.FlagSet writes its usage text.
func run(args []string, usage io.Writer) error {
	fs := flag.NewFlagSet("schemacheck", flag.ContinueOnError)
	fs.SetOutput(usage)
	tree := fs.String("tree", "", "root of the Go message tree to read (e.g. ocpp2.0.1)")
	schemas := fs.String("schemas", "", "comma-separated list of JSON Schema directories to compare against")
	out := fs.String("out", "", "directory to write the report into")
	format := fs.String("format", "both", "which report to write: json, md, or both")
	classifications := fs.String("classifications", "", "YAML registry of hand-made classification/severity judgements to merge onto the computed rows")
	fs.Usage = func() {
		fmt.Fprintln(usage, "schemacheck compares a hand-written OCPP Go message tree against its published JSON Schema documents.")
		fmt.Fprintln(usage, "usage: schemacheck -tree <dir> -schemas <dir>[,<dir>...] -out <dir> [-format json|md|both] [-classifications <file>]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	var missing []string
	if *tree == "" {
		missing = append(missing, "-tree")
	}
	if *schemas == "" {
		missing = append(missing, "-schemas")
	}
	if *out == "" {
		missing = append(missing, "-out")
	}
	if len(missing) > 0 {
		fs.Usage()
		return fmt.Errorf("schemacheck: missing required flag(s): %s", strings.Join(missing, ", "))
	}
	switch *format {
	case "json", "md", "both":
	default:
		return fmt.Errorf("schemacheck: -format must be json, md, or both, got %q", *format)
	}

	opts := compareOptions{
		tree:          *tree,
		schemaDirs:    strings.Split(*schemas, ","),
		rawSchemaFlag: *schemas,
	}
	report, selfCheck, err := runComparison(opts)
	if err != nil {
		return fmt.Errorf("schemacheck: %w", err)
	}

	// Nothing is created, let alone written, until the comparison is known to
	// be publishable: a run that could not read the whole corpus, or that
	// could not reproduce the numbers independently measured over it, must
	// leave no report behind for a later reader to mistake for a good one.
	if failures := publicationFailures(report); len(failures) > 0 {
		return fmt.Errorf("schemacheck: refusing to write a report for %s:\n  - %s",
			report.Version, strings.Join(failures, "\n  - "))
	}

	// The hand-made judgements go on only once the comparison underneath them
	// is known to be publishable, and a registry that does not describe this
	// run stops the report just as a coverage gap does: an annotation merged
	// from a stale registry is worse than none, since it reads as reviewed.
	if *classifications != "" {
		registry, err := loadClassificationRegistry(*classifications)
		if err != nil {
			return fmt.Errorf("schemacheck: %w", err)
		}
		// Some judgements stand in for an account the comparison could not
		// reach, and are pinned to facts read straight from the tree rather
		// than to anything in the report. Those facts are collected here, only
		// when a registry is actually being applied.
		treeFacts, err := collectTreeEvidence(*tree)
		if err != nil {
			return fmt.Errorf("schemacheck: %w", err)
		}
		if err := applyClassifications(report, registry, treeFacts); err != nil {
			return fmt.Errorf("schemacheck: %w", err)
		}
		report.Classifications = *classifications
	}

	base := "divergence-census-" + report.Version

	// Render every requested document before creating anything on disk, so a
	// failure while rendering the second one cannot leave the first behind as
	// a half-written report.
	type document struct {
		path    string
		content []byte
	}
	var documents []document
	if *format == "json" || *format == "both" {
		data, err := marshalReportJSON(report)
		if err != nil {
			return fmt.Errorf("schemacheck: %w", err)
		}
		documents = append(documents, document{path: filepath.Join(*out, base+".json"), content: data})
	}
	if *format == "md" || *format == "both" {
		documents = append(documents, document{path: filepath.Join(*out, base+".md"), content: []byte(renderMarkdownReport(report, selfCheck))})
	}

	if err := os.MkdirAll(*out, 0o755); err != nil {
		return fmt.Errorf("schemacheck: create output directory %s: %w", *out, err)
	}
	for _, doc := range documents {
		if err := os.WriteFile(doc.path, doc.content, 0o644); err != nil {
			return fmt.Errorf("schemacheck: write %s: %w", doc.path, err)
		}
	}

	return nil
}
