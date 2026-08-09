// Package main reads a YAML manifest and JSON Schema documents and builds a
// deterministic intermediate representation for code generation.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run is the whole command apart from its process wiring, so the argument
// surface can be exercised in one process by a test instead of through a
// built binary. usage receives the flag package's own parse errors and usage
// text; main passes standard error.
//
// -dump-ir dispatches into the rendering path (MarshalIR), so the command's
// one output is reachable from the command line by the same function the
// dump's shape is pinned against. It requires -manifest: a dump rendered
// from no manifest could only be the zero-value IR, and a command that
// exits zero having produced an empty dump hands its caller nothing while
// looking like success.
func run(args []string, usage io.Writer) error {
	flags := flag.NewFlagSet("codegen", flag.ContinueOnError)
	flags.SetOutput(usage)
	manifestPath := flags.String("manifest", "", "path of the YAML manifest listing the messages to read")
	schemaDir := flags.String("schemas", "", "directory holding the schema files, replacing the manifest's own schemaDir")
	dumpDir := flags.String("dump-ir", "", "directory to write the intermediate-representation dump into")
	overridesReportPath := flags.String("overrides-report", "", "path to write the deterministic tag-override report into")
	if err := flags.Parse(args); err != nil {
		// -help and -h are answered, not refused: the flag set has already
		// written the usage text to the writer above, so there is nothing
		// left to report, and a command that exits non-zero for its own
		// help is one every script calling it has to special-case.
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("parse arguments: %w", err)
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q: this command takes flags only", flags.Arg(0))
	}
	if *dumpDir != "" && *overridesReportPath != "" {
		return fmt.Errorf("choose one of -dump-ir or -overrides-report, not both")
	}
	if *dumpDir != "" {
		if *manifestPath == "" {
			return fmt.Errorf("-dump-ir requires -manifest: without a manifest there is nothing to dump")
		}
		return dumpIR(*dumpDir, *manifestPath, *schemaDir)
	}
	if *overridesReportPath != "" {
		return writeOverridesReport(*overridesReportPath)
	}
	return fmt.Errorf("read manifest %q with schema directory %q: %w", *manifestPath, *schemaDir, errNotImplemented)
}

// marshalIR is the seam dumpIR renders through: a test can replace it to
// prove the command really reaches this function, whatever MarshalIR does.
var marshalIR = MarshalIR

// dumpIR renders the intermediate representation and writes it into dir.
// Rendering goes through MarshalIR — the one function that produces the
// dump's bytes — so the file this command writes and the shape the tests pin
// can never drift into two separate renderings.
//
// dumpIR loads the manifest (schemaDirOverride replacing the manifest's own
// schemaDir exactly the way LoadManifest always treats that argument), walks
// the schema directory it names, and renders the result. run guarantees
// manifestPath is non-empty before dispatching here.
func dumpIR(dir, manifestPath, schemaDirOverride string) error {
	manifest, err := LoadManifest(manifestPath, schemaDirOverride)
	if err != nil {
		return fmt.Errorf("load manifest %s for %s: %w", manifestPath, dir, err)
	}
	ir, err := WalkSchemas(manifest)
	if err != nil {
		return fmt.Errorf("walk schema directory %s for %s: %w", manifest.SchemaDir, dir, err)
	}

	rendered, err := marshalIR(ir)
	if err != nil {
		return fmt.Errorf("render the intermediate representation for %s: %w", dir, err)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	outPath := filepath.Join(dir, "ir.json")
	if err := os.WriteFile(outPath, rendered, 0o644); err != nil {
		return fmt.Errorf("write %d rendered bytes into %s: %w", len(rendered), outPath, err)
	}
	return nil
}

// checkedInConfigDir resolves internal/codegen/config -- where the checked-in
// v201 manifest, the tag-override table and the initialism table all live --
// against the Go module root, not against the process's current working
// directory. The command exposes no flag for any of the three because there
// is exactly one of each, forever, but "exactly one, forever" only holds
// relative to a fixed point: joining a bare "config/..." onto whatever
// directory the shell happened to be in when -overrides-report was invoked
// makes that fixed point move with the caller, so the same invocation reads
// the real checked-in table from internal/codegen/ and silently reads
// nothing (or, worse, another config/ tree entirely) from anywhere else,
// including the repository root -overrides-report is documented to work
// from.
//
// The module root is found the same way LoadManifest finds the one goTree is
// resolved against -- walking up from a starting directory until a go.mod
// is found -- except the starting directory here is the working directory
// itself, because this resolution has no manifest file of its own to anchor
// on; findModuleRoot's walk, defined in manifest.go, is shared rather than
// duplicated. -overrides-report is only ever meaningful from inside this
// repository's own checkout, so a missing go.mod is a hard failure with a
// message that says so, rather than falling back to a guess.
func checkedInConfigDir() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve the checked-in generator config directory: get working directory: %w", err)
	}
	moduleRoot, err := findModuleRoot(cwd)
	if err != nil {
		return "", fmt.Errorf("resolve the checked-in generator config directory: %w", err)
	}
	return filepath.Join(moduleRoot, "internal", "codegen", "config"), nil
}

// loadOverrides is the command seam for the checked-in tag-override table,
// mirroring marshalIR's seam above: a test can replace it to prove
// -overrides-report reaches LoadOverrides without depending on a real
// checked-in config file or a real schema walk existing yet.
var loadOverrides = LoadOverrides

// renderOverridesReport is the command seam for the deterministic report.
// Keeping it separate from flag parsing and from loading lets a test prove
// -overrides-report dispatches to the renderer without depending on a real
// schema walk or a real checked-in config file existing yet -- the same
// reasoning marshalIR's seam applies to -dump-ir.
var renderOverridesReport = RenderOverridesReport

// writeOverridesReport walks the real, checked-in schema corpus and
// initialism table to build the same IR and per-property mapping the
// generator itself would, loads the checked-in tag-override table against
// them through the seams above, and writes the rendered report into path.
// Loading happens first and its error takes priority over rendering's: a
// failed load never produced a config worth rendering, so rendering is not
// even attempted once loading has already failed.
func writeOverridesReport(path string) error {
	configDir, err := checkedInConfigDir()
	if err != nil {
		return fmt.Errorf("locate the checked-in generator config for the overrides report: %w", err)
	}
	manifestConfigPath := filepath.Join(configDir, "v201.yaml")
	transformConfigPath := filepath.Join(configDir, "transform.yaml")
	overridesConfigPath := filepath.Join(configDir, "overrides.yaml")

	manifest, err := LoadManifest(manifestConfigPath, "")
	if err != nil {
		return fmt.Errorf("load manifest %s for the overrides report: %w", manifestConfigPath, err)
	}
	ir, err := WalkSchemas(manifest)
	if err != nil {
		return fmt.Errorf("walk schema directory %s for the overrides report: %w", manifest.SchemaDir, err)
	}
	transform, err := LoadTransformConfig(transformConfigPath)
	if err != nil {
		return fmt.Errorf("load transform config %s for the overrides report: %w", transformConfigPath, err)
	}
	mappings, registeredValidators, err := buildOverrideMappings(ir, transform)
	if err != nil {
		return fmt.Errorf("map schema properties for the overrides report: %w", err)
	}

	config, loadErr := loadOverrides(overridesConfigPath, ir, mappings, registeredValidators)
	if loadErr != nil {
		return fmt.Errorf("load %s for the overrides report: %w", overridesConfigPath, loadErr)
	}
	rendered, err := renderOverridesReport(config, ir, mappings)
	if err != nil {
		return fmt.Errorf("render overrides report (loaded from %s): %w", overridesConfigPath, err)
	}
	if err := os.WriteFile(path, rendered, 0o644); err != nil {
		return fmt.Errorf("write %d rendered bytes into %s: %w", len(rendered), path, err)
	}
	return nil
}
