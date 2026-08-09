package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"sort"
	"strings"
)

// compareOptions is the parsed, validated form of the CLI flags run()
// collects — the seam between flag parsing and the orchestration this file
// performs, so orchestration tests can drive it without going through
// flag.FlagSet.
type compareOptions struct {
	tree          string
	schemaDirs    []string
	rawSchemaFlag string
}

// prototypeFeatures names the four messages implemented first, ahead of the
// other sixty, so their measured effort can anchor an estimate for the
// rest: a low-complexity floor (Heartbeat) and a high-complexity ceiling
// among the other three.
var prototypeFeatures = map[string]bool{
	"BootNotification":   true,
	"TransactionEvent":   true,
	"SetChargingProfile": true,
	"Heartbeat":          true,
}

// deriveVersionLabel turns a Go message tree's root directory name into the
// report's "version" label by the same mechanical transform for every tree
// this tool is pointed at: drop a leading "ocpp", drop every ".", prepend
// "v" — "ocpp2.0.1" becomes "v201" (matching schemas/v201's own name),
// "ocpp1.6" becomes "v16" (matching a 1.6 comparison's schemas/v16). No
// per-tree lookup table: the same rule must produce the right label for
// whatever tree a future run names.
func deriveVersionLabel(treeRoot string) string {
	base := treeRoot
	if idx := strings.LastIndexAny(treeRoot, `/\`); idx >= 0 {
		base = treeRoot[idx+1:]
	}
	base = strings.TrimPrefix(base, "ocpp")
	base = strings.ReplaceAll(base, ".", "")
	return "v" + base
}

// runComparison is the top-level orchestration pipeline: discover the Go tree
// and the schema corpus, pair every message's fields against its schema,
// classify every row, run the self-checks, and assemble the finished
// Report. It performs no I/O beyond reading the tree and the schema
// directories named in opts — writing the report is the caller's job. The
// second return value carries the self-check detail (composition breakdown,
// structural conflicts) the Markdown report needs beyond what Report.SelfCheck
// itself holds, computed once here rather than by re-walking the schema
// corpus a second time.
func runComparison(opts compareOptions) (*Report, selfCheckResult, error) {
	moduleRoot, err := findModuleRoot(opts.tree)
	if err != nil {
		return nil, selfCheckResult{}, err
	}
	modulePath, err := readModulePath(moduleRoot)
	if err != nil {
		return nil, selfCheckResult{}, err
	}

	goFiles, err := listGoFiles(opts.tree)
	if err != nil {
		return nil, selfCheckResult{}, err
	}
	schemaFiles, err := listSchemaFiles(opts.schemaDirs)
	if err != nil {
		return nil, selfCheckResult{}, err
	}

	idx, err := buildTreeIndex(goFiles, moduleRoot, modulePath)
	if err != nil {
		return nil, selfCheckResult{}, err
	}

	features, err := discoverAllFeatures(goFiles, opts.tree)
	if err != nil {
		return nil, selfCheckResult{}, err
	}
	regs, err := discoverProfileRegistrations(goFiles)
	if err != nil {
		return nil, selfCheckResult{}, err
	}
	if err := crossCheckFeaturesAndProfiles(features, regs, idx.resolver); err != nil {
		return nil, selfCheckResult{}, err
	}

	directions, err := discoverDirections(goFiles, idx.resolver)
	if err != nil {
		return nil, selfCheckResult{}, err
	}
	for i := range features {
		pkg, err := idx.resolver.PackageOf(features[i].file)
		if err != nil {
			return nil, selfCheckResult{}, err
		}
		if dir, ok := directions[pkg+"."+features[i].requestType]; ok {
			features[i].direction = dir
		}
	}

	schemaByName, err := loadSchemas(schemaFiles)
	if err != nil {
		return nil, selfCheckResult{}, err
	}
	schemaNames := make([]string, 0, len(schemaByName))
	for name := range schemaByName {
		schemaNames = append(schemaNames, name)
	}
	sort.Strings(schemaNames)
	allDocs := make([]SchemaDocument, 0, len(schemaNames))
	for _, name := range schemaNames {
		allDocs = append(allDocs, schemaByName[name])
	}

	selfCheck := runSelfChecks(allDocs)

	requestSchemas, responseSchemas, err := indexSchemasByFeature(schemaByName)
	if err != nil {
		return nil, selfCheckResult{}, err
	}

	usedSchemaFiles := map[string]bool{}
	var messages []Message
	var unpairedMessages []string

	for _, f := range features {
		reqDoc, reqOK := requestSchemas[f.featureName]
		respDoc, respOK := responseSchemas[f.featureName]
		if !reqOK || !respOK {
			unpairedMessages = append(unpairedMessages, f.featureName)
			continue
		}
		usedSchemaFiles[baseName(reqDoc.File)] = true
		usedSchemaFiles[baseName(respDoc.File)] = true

		reqSide, _, err := buildMessageSide(idx, f.requestType, f.file, reqDoc)
		if err != nil {
			return nil, selfCheckResult{}, fmt.Errorf("message %s request: %w", f.featureName, err)
		}
		respSide, _, err := buildMessageSide(idx, f.responseType, f.file, respDoc)
		if err != nil {
			return nil, selfCheckResult{}, fmt.Errorf("message %s response: %w", f.featureName, err)
		}

		messages = append(messages, Message{
			FeatureName: f.featureName,
			Profile:     f.profile,
			Direction:   f.direction,
			GoPackage:   f.goPackage,
			GoFile:      baseName(f.file),
			Complexity:  messageComplexity(reqDoc, respDoc),
			Prototype:   prototypeFeatures[f.featureName],
			Request:     reqSide,
			Response:    respSide,
		})
	}
	sort.Slice(messages, func(i, j int) bool { return messages[i].FeatureName < messages[j].FeatureName })
	sort.Strings(unpairedMessages)

	var unpairedSchemaFiles []string
	for _, name := range schemaNames {
		if !usedSchemaFiles[name] {
			unpairedSchemaFiles = append(unpairedSchemaFiles, name)
		}
	}
	sort.Strings(unpairedSchemaFiles)

	summary, err := buildSummary(messages)
	if err != nil {
		return nil, selfCheckResult{}, err
	}

	deduped := dedupCorpus(allDocs)
	var conflicts []string
	for k, dp := range deduped {
		if dp.conflict {
			conflicts = append(conflicts, fmt.Sprintf("%s/%s: conflicting shapes across %v", k.scope, k.name, dp.conflicts))
		}
	}
	sort.Strings(conflicts)
	reachCounts := definitionReachCounts(allDocs)
	treeImport, err := treeImportPath(opts.tree, moduleRoot, modulePath)
	if err != nil {
		return nil, selfCheckResult{}, err
	}
	sharedTypes := buildSharedTypes(reachCounts, conflicts, treeImport+"/types", idx)

	corpus, err := schemaCorpusIdentity(opts.schemaDirs)
	if err != nil {
		return nil, selfCheckResult{}, err
	}

	report := &Report{
		Version:      deriveVersionLabel(opts.tree),
		GoTree:       opts.tree,
		SchemaDir:    opts.rawSchemaFlag,
		Invocation:   invocationOf(opts),
		SchemaCorpus: corpus,
		Coverage: Coverage{
			Messages:            len(features),
			SchemaFiles:         len(schemaFiles),
			UnpairedMessages:    orEmpty(unpairedMessages),
			UnpairedSchemaFiles: orEmpty(unpairedSchemaFiles),
		},
		Summary:     summary,
		SelfCheck:   selfCheck.checks,
		Messages:    messages,
		SharedTypes: sharedTypes,
	}
	return report, selfCheck, nil
}

// selfCheckCorpus names the schema corpus the M1-M7 expectations in
// selfcheck.go were measured over, spelled as the version label
// deriveVersionLabel produces for it.
//
// Those seven numbers are measurements of one specific corpus, not
// properties every schema set shares: reproducing them is what earns a run
// over *that* corpus the right to be believed, and holding a different
// corpus to them asks a question they cannot answer. A run over any other
// corpus therefore still reports its self-check rows — the comparison is
// worth seeing — but is not held back by them; that corpus's own
// independently measured expectations have to be recorded here first.
const selfCheckCorpus = "v201"

// publicationFailures reports every reason report must not be written out,
// as sentences naming the specific failure. A report is a claim that the
// whole corpus was read and that the instrument reproduced the numbers an
// independent pass over the same files produced; a run that did neither
// still produces a Report value — the detail is needed to diagnose the
// failure — but publishing it would put an incomplete or unverified report
// on disk looking exactly like a complete, verified one, with the failure
// demoted to an annotation a reader has to notice.
//
// Two kinds prevent publication:
//
//   - coverage gaps: a discovered message with no schema pair, or a schema
//     file no message consumed. Either means some part of one input was
//     never compared, so every count in the report is a count over an
//     unstated subset. Checked for every corpus.
//   - self-check failures: the instrument disagreeing with the independently
//     measured numbers for its corpus. Checked only for the corpus those
//     numbers describe (see selfCheckCorpus), and a disagreement is
//     adjudicated by hand against the specific files — deciding which side
//     is wrong — never by adjusting the tool until it agrees.
func publicationFailures(report *Report) []string {
	var failures []string
	for _, name := range report.Coverage.UnpairedMessages {
		failures = append(failures, fmt.Sprintf("message %s has no schema pair, so its fields were never compared", name))
	}
	for _, name := range report.Coverage.UnpairedSchemaFiles {
		failures = append(failures, fmt.Sprintf("schema file %s was not consumed by any message, so its properties were never compared", name))
	}
	if report.Version == selfCheckCorpus {
		for _, check := range report.SelfCheck {
			if check.Status != "pass" {
				failures = append(failures, fmt.Sprintf("self-check %s (%s) expected %d, measured %d", check.ID, check.Claim, check.Expected, check.Actual))
			}
		}
	}
	return failures
}

// orEmpty returns an empty (non-nil) slice in place of nil, so the report's
// JSON encodes coverage.unpairedMessages/unpairedSchemaFiles as "[]" rather
// than "null" when nothing is unpaired.
func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func baseName(path string) string {
	idx := strings.LastIndexAny(path, `/\`)
	if idx < 0 {
		return path
	}
	return path[idx+1:]
}

// buildSummary computes the three roll-ups, the class histogram, the
// complexity distribution and the prototype count from the finished
// message list.
func buildSummary(messages []Message) (Summary, error) {
	var classCounts ClassCounts
	breakingFields := 0
	messagesWithBreaking := map[string]bool{}
	messagesNeedingOverrides := map[string]bool{}
	prototypeCount := 0
	complexities := make([]int, 0, len(messages))

	tally := func(featureName string, fields []Field) {
		for _, field := range fields {
			switch field.Class {
			case IDENTICAL:
				classCounts.IDENTICAL++
			case SCHEMA_FAITHFUL_CHANGE:
				classCounts.SCHEMA_FAITHFUL_CHANGE++
			case FORK_BUG:
				classCounts.FORK_BUG++
			case ADDITIVE:
				classCounts.ADDITIVE++
			case OVERRIDE_CANDIDATE:
				classCounts.OVERRIDE_CANDIDATE++
				messagesNeedingOverrides[featureName] = true
			case STRUCT_VALIDATOR:
				classCounts.STRUCT_VALIDATOR++
			case UNEXPLAINED:
				classCounts.UNEXPLAINED++
			}
			if field.Breaking {
				breakingFields++
				messagesWithBreaking[featureName] = true
			}
		}
	}

	for _, m := range messages {
		tally(m.FeatureName, m.Request.Fields)
		tally(m.FeatureName, m.Response.Fields)
		if m.Prototype {
			prototypeCount++
		}
		complexities = append(complexities, m.Complexity)
	}

	distribution, err := complexityDistributionOf(complexities)
	if err != nil {
		return Summary{}, err
	}

	density := 0.0
	if len(messages) > 0 {
		density = float64(len(messagesNeedingOverrides)) / float64(len(messages))
	}

	return Summary{
		ByClass:                    classCounts,
		BreakingFields:             breakingFields,
		MessagesWithBreakingFields: len(messagesWithBreaking),
		MessagesNeedingOverrides:   len(messagesNeedingOverrides),
		OverrideDensity:            density,
		ComplexityDistribution:     distribution,
		PrototypeCount:             prototypeCount,
	}, nil
}

// schemaCorpusIdentity digests every schema document each directory
// contributed, so the report records which revision of a published schema set
// it compared against rather than only the path it read. Naming a directory
// identifies where the files came from; it says nothing about what they
// contained, and a published schema set is republished.
//
// The digest is deliberately taken over the files as read rather than over a
// checked-in manifest: a manifest states what the files are supposed to be,
// which is a different claim from what the comparison actually consumed.
func schemaCorpusIdentity(dirs []string) ([]SchemaCorpusDir, error) {
	corpus := make([]SchemaCorpusDir, 0, len(dirs))
	for _, dir := range dirs {
		files, err := listSchemaFiles([]string{dir})
		if err != nil {
			return nil, err
		}
		lines := make([]string, 0, len(files))
		for _, path := range files {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("digest schema %s: %w", path, err)
			}
			lines = append(lines, fmt.Sprintf("%x  %s", sha256.Sum256(data), baseName(path)))
		}
		sort.Strings(lines)
		joined := strings.Join(lines, "\n") + "\n"
		corpus = append(corpus, SchemaCorpusDir{
			Dir:    dir,
			Files:  len(files),
			SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(joined))),
		})
	}
	return corpus, nil
}

// invocationOf spells the command that reproduces this comparison, and spells
// it so that it runs: -out is required, so a command printed without it exits
// with a usage message instead of a report. The directory named is a fixed
// placeholder rather than the one this run happened to use — the report has to
// read the same however it was invoked, and where the output goes is the one
// input that changes nothing about what the report says. -format is omitted
// because its default writes both documents. -classifications is omitted for a
// different reason — it annotates the comparison rather than performing it —
// and is recorded in its own field.
func invocationOf(opts compareOptions) string {
	return fmt.Sprintf("go run ./internal/schemacheck -tree %s -schemas %s -out ./schemacheck-out", opts.tree, opts.rawSchemaFlag)
}
