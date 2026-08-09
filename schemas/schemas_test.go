package schemas_test

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

const (
	expectedV201Files        = 128
	expectedV16BaseFiles     = 56
	expectedV16SecurityFiles = 22
)

// The vendored corpus is flat: every schema file sits directly inside one of
// these three set directories, which is also the shape SHA256SUMS records.
var vendoredSetDirs = []string{"v201", "v16/base", "v16/security"}

// The only directories schemas/ may hold: the three sets, the v16 parent the
// two 1.6 sets share, and schemas/ itself — reported as "." by filepath.Rel
// when the walk visits its own root.
var permittedSchemasDirs = []string{".", "v201", "v16", "v16/base", "v16/security"}

func containsPath(paths []string, candidate string) bool {
	for _, p := range paths {
		if p == candidate {
			return true
		}
	}
	return false
}

// sitsDirectlyInVendoredSet reports whether a slash-separated path relative to
// schemas/ names a file lying immediately inside one of the three sets, as
// opposed to one nested further down. Slash-path rules are used deliberately:
// the manifest records slash paths on every operating system.
func sitsDirectlyInVendoredSet(relativeToSchemas string) bool {
	return containsPath(vendoredSetDirs, path.Dir(relativeToSchemas))
}

func TestSchemaManifestIntegrity(t *testing.T) {
	root := repositoryRoot(t)
	manifestPath := filepath.Join(root, "schemas", "SHA256SUMS")
	manifest, err := os.Open(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			t.Fatal("schemas/SHA256SUMS not found — schemas are not vendored yet")
		}
		t.Fatalf("open schemas/SHA256SUMS: %v", err)
	}
	defer manifest.Close()

	listed := make(map[string]string)
	scanner := bufio.NewScanner(manifest)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		fields := strings.Fields(text)
		if len(fields) != 2 || len(fields[0]) != sha256.Size*2 {
			t.Fatalf("malformed SHA256SUMS line %d: %q", line, scanner.Text())
		}
		if _, err := hex.DecodeString(fields[0]); err != nil {
			t.Fatalf("malformed SHA256SUMS hash on line %d: %v", line, err)
		}
		// shasum/sha256sum emit a leading "*" on the path field for
		// binary-mode entries (as opposed to a plain path for text-mode
		// entries); strip it so both forms parse to the same path.
		pathField := strings.TrimPrefix(fields[1], "*")
		path := filepath.Clean(filepath.FromSlash(pathField))
		if _, exists := listed[path]; exists {
			t.Fatalf("duplicate SHA256SUMS entry: %s", fields[1])
		}
		listed[path] = strings.ToLower(fields[0])
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read schemas/SHA256SUMS: %v", err)
	}
	if len(listed) == 0 {
		t.Fatal("schemas/SHA256SUMS contains no schema entries")
	}

	// The manifest is a schema-file integrity list, not a whole-tree
	// inventory: NOTICE, SHA256SUMS itself and this guard test's own source
	// are deliberately excluded from it, and a manifest that accidentally
	// listed one of them would defeat that separation.
	for _, name := range []string{"NOTICE", "SHA256SUMS", "schemas_test.go"} {
		nonSchemaPath := filepath.Clean(filepath.Join("schemas", name))
		if _, ok := listed[nonSchemaPath]; ok {
			t.Fatalf("schemas/SHA256SUMS must not list %s", nonSchemaPath)
		}
	}

	// Hash-checking an entry proves the named file has not changed; it does
	// not prove the entry belongs in this manifest at all. SHA256SUMS is the
	// integrity contract for the vendored schema corpus and for nothing else,
	// so every entry must name a .json file inside one of the three vendored
	// sets: an entry naming, say, go.mod would carry a perfectly valid hash
	// and would quietly extend the manifest's protection to a file it was
	// never meant to cover, and a path escaping schemas/ would point the hash
	// check outside the corpus entirely. This is the positive counterpart to
	// the NOTICE / SHA256SUMS / schemas_test.go exclusion above — those three
	// names are excluded by name, and everything that is not a vendored
	// schema file is excluded with them.
	var foreignEntries []string
	for relative := range listed {
		slashed := filepath.ToSlash(relative)
		withinSchemas := strings.TrimPrefix(slashed, "schemas/")
		switch {
		case !strings.HasPrefix(slashed, "schemas/"):
			foreignEntries = append(foreignEntries, fmt.Sprintf("SHA256SUMS entry is not under schemas/: %s", slashed))
		case strings.ToLower(filepath.Ext(slashed)) != ".json":
			foreignEntries = append(foreignEntries, fmt.Sprintf("SHA256SUMS entry is not a .json schema file: %s", slashed))
		case !strings.HasPrefix(withinSchemas, "v201/") &&
			!strings.HasPrefix(withinSchemas, "v16/base/") &&
			!strings.HasPrefix(withinSchemas, "v16/security/"):
			foreignEntries = append(foreignEntries, fmt.Sprintf(
				"SHA256SUMS entry is outside the three vendored sets (must live under v201/, v16/base/ or v16/security/): %s", slashed))
		case !sitsDirectlyInVendoredSet(withinSchemas):
			// The prefix test above is satisfied by any depth beneath a set,
			// so the manifest is held to the same flat layout the tree walk
			// below enforces.
			foreignEntries = append(foreignEntries, fmt.Sprintf(
				"SHA256SUMS entry names a file below a set directory rather than directly inside one: %s", slashed))
		}
	}
	// Map iteration order is unspecified, so the collected messages are sorted
	// to keep a failing run's output stable between invocations.
	sort.Strings(foreignEntries)
	for _, msg := range foreignEntries {
		t.Error(msg)
	}

	for relative, expectedHash := range listed {
		path := filepath.Join(root, relative)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				t.Fatalf("manifest lists missing file %s", relative)
			}
			t.Fatalf("read manifest file %s: %v", relative, err)
		}
		actualHash := fmt.Sprintf("%x", sha256.Sum256(data))
		if actualHash != expectedHash {
			t.Fatalf("hash mismatch for %s: manifest=%s actual=%s", relative, expectedHash, actualHash)
		}
	}

	counts := map[string]int{
		"v201":         0,
		"v16/base":     0,
		"v16/security": 0,
	}
	schemasRoot := filepath.Join(root, "schemas")
	// filepath.Walk's own callback must not call t.Fatal: Fatal unwinds the
	// calling goroutine immediately, which would abandon the walk after the
	// first offending file instead of reporting every purity violation in
	// the tree in one run. Errors are collected here and reported with
	// t.Error once the walk completes.
	var walkErrors []string
	err = filepath.Walk(schemasRoot, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relativeToSchemas, relErr := filepath.Rel(schemasRoot, path)
		if relErr != nil {
			return relErr
		}
		relativeToSchemas = filepath.ToSlash(relativeToSchemas)
		relativeToRoot, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		relativeToRoot = filepath.Clean(relativeToRoot)

		// filepath.Walk reports every entry with lstat information and never
		// follows a link, so a symbolic link arrives here as a link rather
		// than as whatever it points at — and that distinction is exactly
		// what has to be checked. A link to a byte-identical file passes a
		// hash comparison, passes a diff, and still ends the one property
		// this corpus is vendored for: that schemas/ is a self-contained pile
		// of real bytes anyone can copy. Copy a tree with a link in it and
		// what travels is a pointer to something outside the corpus, or to
		// nothing at all. This runs before the directory branch below because
		// a link to a directory is not reported as a directory.
		if info.Mode()&os.ModeSymlink != 0 {
			walkErrors = append(walkErrors, fmt.Sprintf(
				"symbolic link under schemas/: %s (every vendored path must be a real directory or a real file)",
				filepath.ToSlash(relativeToRoot)))
			return nil
		}

		if info.IsDir() {
			// The vendored corpus is redistributed byte-identical; every
			// semantic correction lives in
			// internal/codegen/config/overrides.yaml, never a directory
			// that shadows it here. This walk already reports "schemas
			// directory not found" below when nothing is vendored yet
			// (os.IsNotExist), so an absent schemas/ tree is not a
			// violation of this check either — there is nothing in it to
			// violate — and only a directory that genuinely exists, named
			// "overrides", is a hard failure.
			if info.Name() == "overrides" {
				walkErrors = append(walkErrors, fmt.Sprintf(
					"found an overrides directory at %s; a tag-override belongs in internal/codegen/config/overrides.yaml, never in an overrides/ directory anywhere under schemas/",
					filepath.ToSlash(relativeToRoot)))
			}
			// schemas/ mirrors the layout of the archives it came from and
			// nothing else: v201 flat, v16 split into base and security. A
			// new directory here is either a schema set nobody wrote down, or
			// somewhere for a file to hide from the per-set counts below,
			// which only tally files directly inside the three sets.
			if !containsPath(permittedSchemasDirs, relativeToSchemas) {
				walkErrors = append(walkErrors, fmt.Sprintf(
					"unexpected directory under schemas/: %s (the tree holds only %s and the v16 parent they share)",
					filepath.ToSlash(relativeToRoot), strings.Join(vendoredSetDirs, ", ")))
			}
			return nil
		}

		if strings.ToLower(filepath.Ext(path)) != ".json" {
			// schemas/ holds .json files plus NOTICE, SHA256SUMS and this
			// guard test — nothing else — and those three names are only
			// legitimate directly at the schemas/ root, never inside a
			// nested set directory (a file named "NOTICE" under v201/ would
			// otherwise slip past this check).
			if relativeToSchemas == "NOTICE" || relativeToSchemas == "SHA256SUMS" || relativeToSchemas == "schemas_test.go" {
				return nil
			}
			walkErrors = append(walkErrors, fmt.Sprintf(
				"unexpected non-schema file under schemas/: %s (only NOTICE, SHA256SUMS and schemas_test.go may sit alongside the vendored .json files, and only at the schemas/ root)",
				filepath.ToSlash(relativeToRoot)))
			return nil
		}

		if _, ok := listed[relativeToRoot]; !ok {
			walkErrors = append(walkErrors, fmt.Sprintf("schema file is missing from SHA256SUMS: %s", filepath.ToSlash(relativeToRoot)))
		}

		// Membership of a set is decided by prefix below, which a file nested
		// one or more levels deeper would satisfy just as well: v201/x/y.json
		// would be hashed, counted and declared a 2.0.1 schema. The sets are
		// flat in the archives and flat here, so flatness is asserted in its
		// own right rather than left to follow from the prefix.
		if strings.HasPrefix(relativeToSchemas, "v201/") ||
			strings.HasPrefix(relativeToSchemas, "v16/base/") ||
			strings.HasPrefix(relativeToSchemas, "v16/security/") {
			if !sitsDirectlyInVendoredSet(relativeToSchemas) {
				walkErrors = append(walkErrors, fmt.Sprintf(
					"schema file below a set directory: %s (a schema file must sit directly inside %s)",
					filepath.ToSlash(relativeToRoot), strings.Join(vendoredSetDirs, ", ")))
			}
		}

		switch {
		case strings.HasPrefix(relativeToSchemas, "v201/"):
			counts["v201"]++
		case strings.HasPrefix(relativeToSchemas, "v16/base/"):
			counts["v16/base"]++
		case strings.HasPrefix(relativeToSchemas, "v16/security/"):
			counts["v16/security"]++
		default:
			walkErrors = append(walkErrors, fmt.Sprintf(
				"schema file outside the three vendored sets (must live under v201/, v16/base/ or v16/security/): %s",
				filepath.ToSlash(relativeToRoot)))
		}
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			t.Fatal("schemas directory not found — schemas are not vendored yet")
		}
		t.Fatalf("walk schemas: %v", err)
	}
	for _, msg := range walkErrors {
		t.Error(msg)
	}

	if counts["v201"] != expectedV201Files {
		t.Fatalf("schemas/v201 file count: got %d, want %d", counts["v201"], expectedV201Files)
	}
	if counts["v16/base"] != expectedV16BaseFiles {
		t.Fatalf("schemas/v16/base file count: got %d, want %d", counts["v16/base"], expectedV16BaseFiles)
	}
	if counts["v16/security"] != expectedV16SecurityFiles {
		t.Fatalf("schemas/v16/security file count: got %d, want %d", counts["v16/security"], expectedV16SecurityFiles)
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller could not locate schemas/schemas_test.go")
	}
	directory := filepath.Dir(filename)
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatal("could not find go.mod above schemas/schemas_test.go")
		}
		directory = parent
	}
}
