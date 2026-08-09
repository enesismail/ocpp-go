package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Manifest is the generator input document, decoded from YAML and fully
// resolved: SchemaDir is always a usable path by the time LoadManifest
// returns it, never the raw value found in the YAML (see LoadManifest).
type Manifest struct {
	Version      string            `yaml:"version" json:"version"`
	SchemaDir    string            `yaml:"schemaDir" json:"schemaDir"`
	GoModule     string            `yaml:"goModule" json:"goModule"`
	GoTree       string            `yaml:"goTree" json:"goTree"`
	TypesPackage string            `yaml:"typesPackage" json:"typesPackage"`
	Messages     []ManifestMessage `yaml:"messages" json:"messages"`
}

// TypesImportPath resolves the manifest's shared types package into the Go
// import path a generated block-package file writes into its import block.
//
// The manifest models the module and the package separately: goModule is
// the module path declared by the go.mod the generated tree lives under,
// and typesPackage names the shared types package the emitter writes
// types_gen.go into. Only a module-qualified path is a legal Go import
// path, so a typesPackage spelled the way goTree is — relative to the
// module root, as the checked-in v201 manifest spells it
// ("ocpp2.0.1/types") — is completed with the module path here. A
// typesPackage that already carries the module path is returned unchanged,
// which is how a manifest that spells the package out in full stays
// correct. Emitting the raw value instead would write
// `import "ocpp2.0.1/types"` into every generated message file, an import
// no module can resolve.
//
// A manifest with no goModule cannot complete anything, so the value is
// returned as given; the manifest loader is where a missing module would be
// caught, not here.
func (m Manifest) TypesImportPath() string {
	pkg := strings.Trim(strings.TrimSpace(m.TypesPackage), "/")
	module := strings.Trim(strings.TrimSpace(m.GoModule), "/")
	if pkg == "" || module == "" {
		return pkg
	}
	if pkg == module || strings.HasPrefix(pkg, module+"/") {
		return pkg
	}
	return module + "/" + pkg
}

// ManifestMessage declares one request/response pair and its output policy.
type ManifestMessage struct {
	Name      string `yaml:"name" json:"name"`
	Block     string `yaml:"block" json:"block"`
	Direction string `yaml:"direction" json:"direction"`
	Request   string `yaml:"request" json:"request"`
	Response  string `yaml:"response" json:"response"`
	Emit      bool   `yaml:"emit" json:"emit"`
}

// legalDirections are the only values a row's direction may carry.
var legalDirections = map[string]bool{
	"CS->CSMS": true,
	"CSMS->CS": true,
	"both":     true,
}

// LoadManifest reads the manifest at path and resolves its schema directory.
//
// Decoding happens in two passes over the same YAML document: a strict
// (*yaml.Decoder).KnownFields(true) struct decode, which rejects any
// top-level or row field the Manifest/ManifestMessage structs do not
// declare; and a raw yaml.Node walk, which records which of a row's required
// fields (name, block, direction, request, response, emit) were actually
// present in the source. The second pass exists because the struct decode
// alone cannot tell an omitted `emit:` apart from an explicit `emit: false`
// — both decode to the same Go zero value.
//
// schemaDir resolution follows two rules. By default, the manifest's own
// schemaDir value is resolved relative to the directory containing the
// manifest file itself (not the process's current working directory, and
// not any notion of a repository root). When schemaDirOverride is non-empty,
// it replaces the manifest's schemaDir entirely and is used exactly as
// given — this is what a command-line flag supplies. A Manifest built
// in-process (not read from a file, as in tests that exercise WalkSchemas
// directly) is not subject to either rule: WalkSchemas takes SchemaDir as
// already resolved.
//
// goTree resolution follows a rule of its own, distinct from schemaDir's: a
// row's block is checked against a directory under goTree, and goTree itself
// is always resolved against the Go module root — found by walking up from
// the manifest file's own directory until a go.mod is found — never against
// the process's current working directory and never joined with the
// manifest's directory the way schemaDir is. A manifest that does not live
// inside the module whose go.mod it needs cannot be loaded.
//
// Every hard-fail names the offending row: by the row's own name field when
// it is present and non-empty, or by its 1-indexed position ("row N") when
// the name itself is the missing or empty field. A duplicate-name conflict
// is named by both: the shared name no longer identifies a single row once
// two rows carry it, so the error also cites the 1-indexed position of the
// second row that collides with the first. A double-claimed file is named by
// the second row that claims it, on the same reasoning: the filename says
// what collided, and the row that arrived at an already-claimed file is the
// one whoever edits the manifest has to change. The unclaimed-file sweep is
// the single check with no offending row to name — no row claims the file,
// by definition — so its error names the file alone.
//
// The unclaimed-file and double-claimed-file checks list schemaDir
// non-recursively: only the files directly inside it are matched against
// the manifest's request/response columns, so a schema vendored into a
// subdirectory of schemaDir is invisible to both checks. A row naming a file
// that is not in schemaDir is reported against that row before the
// unclaimed-file sweep runs: a row pointing at the wrong filename always
// leaves the file it should have claimed unclaimed, and reporting the sweep
// first would name a file nobody mistyped.
func LoadManifest(path string, schemaDirOverride string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("load manifest %s: %v", path, err)
	}

	var manifest Manifest
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, wrapManifestDecodeError(path, err)
	}
	// The manifest is one document, and only the first one is decoded. A
	// second document — appended by a merge, or left behind by an edit —
	// would otherwise go unread along with every field it declares, and the
	// strict-field check the first document was just held to would say
	// nothing about it.
	var extraDocument yaml.Node
	if err := decoder.Decode(&extraDocument); !errors.Is(err, io.EOF) {
		if err != nil {
			return Manifest{}, wrapManifestDecodeError(path, err)
		}
		return Manifest{}, fmt.Errorf("load manifest %s: the file holds more than one YAML document; the manifest is a single document", path)
	}

	var rawDoc yaml.Node
	if err := yaml.Unmarshal(data, &rawDoc); err != nil {
		return Manifest{}, fmt.Errorf("load manifest %s: %v", path, err)
	}
	emitPresence, err := readEmitPresence(&rawDoc)
	if err != nil {
		return Manifest{}, fmt.Errorf("load manifest %s: %v", path, err)
	}

	if schemaDirOverride != "" {
		manifest.SchemaDir = schemaDirOverride
	} else {
		// schemaDir is resolved relative to the manifest file's own
		// directory exactly as path names it (relative stays relative,
		// absolute stays absolute) — this is what TestManifestRoundTripsAllFields
		// pins. The module-root walk below needs an absolute starting point
		// instead: a relative directory's own filepath.Dir chain collapses to
		// "." long before it could reach the repository's go.mod.
		manifest.SchemaDir = filepath.Join(filepath.Dir(path), manifest.SchemaDir)
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("load manifest %s: %v", path, err)
	}
	moduleRoot, err := findModuleRoot(filepath.Dir(absPath))
	if err != nil {
		return Manifest{}, fmt.Errorf("load manifest %s: %v", path, err)
	}

	if err := validateManifest(path, manifest, moduleRoot, emitPresence); err != nil {
		return Manifest{}, err
	}

	return manifest, nil
}

// wrapManifestDecodeError turns yaml.v3's own decode failure — including its
// multi-line KnownFields report — into the loader's one-line convention.
func wrapManifestDecodeError(path string, err error) error {
	message := strings.Join(strings.Fields(strings.ReplaceAll(err.Error(), "\n", " ")), " ")
	if strings.Contains(message, "not found in type") {
		return fmt.Errorf("load manifest %s: unexpected field: %s", path, message)
	}
	return fmt.Errorf("load manifest %s: invalid manifest: %s", path, message)
}

// readEmitPresence walks the raw YAML document (not the decoded struct) to
// determine, per row, whether an `emit:` key was actually written in the
// source. An omitted key and an explicit `emit: false` both decode to the
// same Go zero value, so this is the only way to tell them apart.
func readEmitPresence(doc *yaml.Node) ([]bool, error) {
	if len(doc.Content) == 0 {
		return nil, fmt.Errorf("empty manifest document")
	}
	root := doc.Content[0]
	messages := mappingValue(root, "messages")
	if messages == nil {
		return nil, nil
	}
	presence := make([]bool, len(messages.Content))
	for i, row := range messages.Content {
		presence[i] = mappingHasKey(row, "emit")
	}
	return presence, nil
}

// mappingValue returns the value node paired with key in a YAML mapping
// node, or nil when the mapping carries no such key.
func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// mappingHasKey reports whether a YAML mapping node declares key at all,
// independent of the value it carries.
func mappingHasKey(mapping *yaml.Node, key string) bool {
	return mappingValue(mapping, key) != nil
}

// findModuleRoot walks up from dir until it finds a directory containing
// go.mod, returning that directory. It never consults the process's working
// directory: the walk always starts from the manifest file's own directory.
func findModuleRoot(dir string) (string, error) {
	current := dir
	for {
		if info, err := os.Stat(filepath.Join(current, "go.mod")); err == nil && !info.IsDir() {
			return current, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no go.mod found above %s", dir)
		}
		current = parent
	}
}

// rowLabel names a row the way every hard-fail message does: by its own
// name field when present and non-empty, or by its 1-indexed position
// otherwise.
func rowLabel(row ManifestMessage, index int) string {
	if strings.TrimSpace(row.Name) != "" {
		return row.Name
	}
	return fmt.Sprintf("row %d", index+1)
}

// validateManifest runs every manifest-level and per-row hard-fail check
// the manifest format defines, stopping at the first violation.
func validateManifest(path string, manifest Manifest, moduleRoot string, emitPresence []bool) error {
	claimedNames := make(map[string]int, len(manifest.Messages))
	claimedFiles := make(map[string]int, len(manifest.Messages)*2)

	for index, row := range manifest.Messages {
		label := rowLabel(row, index)

		if strings.TrimSpace(row.Name) == "" {
			return fmt.Errorf("load manifest %s: %s is missing required field name", path, label)
		}
		if strings.TrimSpace(row.Block) == "" {
			return fmt.Errorf("load manifest %s: %s is missing required field block", path, label)
		}
		if strings.TrimSpace(row.Direction) == "" {
			return fmt.Errorf("load manifest %s: %s is missing required field direction", path, label)
		}
		if strings.TrimSpace(row.Request) == "" {
			return fmt.Errorf("load manifest %s: %s is missing required field request", path, label)
		}
		if strings.TrimSpace(row.Response) == "" {
			return fmt.Errorf("load manifest %s: %s is missing required field response", path, label)
		}
		if index >= len(emitPresence) || !emitPresence[index] {
			return fmt.Errorf("load manifest %s: %s is missing required field emit", path, label)
		}

		if !legalDirections[row.Direction] {
			return fmt.Errorf("load manifest %s: %s has illegal direction %q (want CS->CSMS, CSMS->CS, or both)", path, label, row.Direction)
		}

		blockDir := filepath.Join(moduleRoot, manifest.GoTree, row.Block)
		if info, err := os.Stat(blockDir); err != nil || !info.IsDir() {
			return fmt.Errorf("load manifest %s: %s names unknown block %q (no directory %s under %s)", path, label, row.Block, row.Block, manifest.GoTree)
		}

		requestPath := filepath.Join(manifest.SchemaDir, row.Request)
		if info, err := os.Stat(requestPath); err != nil || info.IsDir() {
			return fmt.Errorf("load manifest %s: %s names request file %s not found in %s", path, label, row.Request, manifest.SchemaDir)
		}
		responsePath := filepath.Join(manifest.SchemaDir, row.Response)
		if info, err := os.Stat(responsePath); err != nil || info.IsDir() {
			return fmt.Errorf("load manifest %s: %s names response file %s not found in %s", path, label, row.Response, manifest.SchemaDir)
		}

		if firstIndex, seen := claimedNames[row.Name]; seen {
			return fmt.Errorf("load manifest %s: row %d duplicates message name %q first used by row %d", path, index+1, row.Name, firstIndex+1)
		}
		claimedNames[row.Name] = index

		if firstIndex, seen := claimedFiles[row.Request]; seen {
			return fmt.Errorf("load manifest %s: row %s claims file %s already claimed by row %s", path, label, row.Request, rowLabel(manifest.Messages[firstIndex], firstIndex))
		}
		claimedFiles[row.Request] = index
		if firstIndex, seen := claimedFiles[row.Response]; seen {
			return fmt.Errorf("load manifest %s: row %s claims file %s already claimed by row %s", path, label, row.Response, rowLabel(manifest.Messages[firstIndex], firstIndex))
		}
		claimedFiles[row.Response] = index
	}

	return checkUnclaimedFiles(path, manifest.SchemaDir, claimedFiles)
}

// checkUnclaimedFiles sweeps schemaDir's own directory (non-recursively)
// for a .json file no row claimed as a request or response.
func checkUnclaimedFiles(path, schemaDir string, claimedFiles map[string]int) error {
	entries, err := os.ReadDir(schemaDir)
	if err != nil {
		return fmt.Errorf("load manifest %s: read schema directory %s: %v", path, schemaDir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if _, claimed := claimedFiles[entry.Name()]; !claimed {
			return fmt.Errorf("load manifest %s: schema file %s in %s is claimed by no row", path, entry.Name(), schemaDir)
		}
	}
	return nil
}
