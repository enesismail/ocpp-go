package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunRejectsMissingFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"all missing", nil, "-tree, -schemas, -out"},
		{"tree missing", []string{"-schemas", "testdata/orchestration/schemas", "-out", t.TempDir()}, "-tree"},
		{"schemas missing", []string{"-tree", "testdata/orchestration/tree", "-out", t.TempDir()}, "-schemas"},
		{"out missing", []string{"-tree", "testdata/orchestration/tree", "-schemas", "testdata/orchestration/schemas"}, "-out"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var usage bytes.Buffer
			err := run(c.args, &usage)
			if err == nil {
				t.Fatal("missing required flag(s) did not produce an error")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not name the missing flag(s) %q", err.Error(), c.want)
			}
		})
	}
}

func TestRunRejectsBadFormat(t *testing.T) {
	var usage bytes.Buffer
	err := run([]string{
		"-tree", "testdata/orchestration/tree",
		"-schemas", "testdata/orchestration/schemas",
		"-out", t.TempDir(),
		"-format", "yaml",
	}, &usage)
	if err == nil {
		t.Fatal("an unrecognized -format value did not produce an error")
	}
	if !strings.Contains(err.Error(), "-format") {
		t.Fatalf("error %q does not name the offending flag", err.Error())
	}
}

func TestRunUnknownFlagIsRejected(t *testing.T) {
	var usage bytes.Buffer
	err := run([]string{"-nonsense", "1"}, &usage)
	if err == nil {
		t.Fatal("an unrecognized flag did not produce an error")
	}
}

// TestRunWritesBothReportsByDefault covers the full CLI-to-disk path: -tree
// and -schemas point at the small fixture pair under testdata/orchestration, -out
// is a fresh temp directory, and -format is left at its "both" default.
func TestRunWritesBothReportsByDefault(t *testing.T) {
	out := t.TempDir()
	var usage bytes.Buffer
	err := run([]string{
		"-tree", "testdata/orchestration/tree",
		"-schemas", "testdata/orchestration/schemas",
		"-out", out,
	}, &usage)
	if err != nil {
		t.Fatalf("run: %v (usage output: %s)", err, usage.String())
	}

	jsonPath := filepath.Join(out, "divergence-census-vtree.json")
	mdPath := filepath.Join(out, "divergence-census-vtree.md")
	assertFileExistsNonEmpty(t, jsonPath)
	assertFileExistsNonEmpty(t, mdPath)
}

func TestRunFormatJSONWritesOnlyJSON(t *testing.T) {
	out := t.TempDir()
	var usage bytes.Buffer
	err := run([]string{
		"-tree", "testdata/orchestration/tree",
		"-schemas", "testdata/orchestration/schemas",
		"-out", out,
		"-format", "json",
	}, &usage)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	assertFileExistsNonEmpty(t, filepath.Join(out, "divergence-census-vtree.json"))
	assertFileAbsent(t, filepath.Join(out, "divergence-census-vtree.md"))
}

func TestRunFormatMDWritesOnlyMarkdown(t *testing.T) {
	out := t.TempDir()
	var usage bytes.Buffer
	err := run([]string{
		"-tree", "testdata/orchestration/tree",
		"-schemas", "testdata/orchestration/schemas",
		"-out", out,
		"-format", "md",
	}, &usage)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	assertFileExistsNonEmpty(t, filepath.Join(out, "divergence-census-vtree.md"))
	assertFileAbsent(t, filepath.Join(out, "divergence-census-vtree.json"))
}

// TestRunProducesByteIdenticalJSONAcrossInvocations covers determinism at
// the full CLI level: two runs, writing into two different output
// directories, must produce byte-identical bytes on disk.
func TestRunProducesByteIdenticalJSONAcrossInvocations(t *testing.T) {
	outA := t.TempDir()
	outB := t.TempDir()
	var usage bytes.Buffer
	for _, out := range []string{outA, outB} {
		if err := run([]string{
			"-tree", "testdata/orchestration/tree",
			"-schemas", "testdata/orchestration/schemas",
			"-out", out,
			"-format", "json",
		}, &usage); err != nil {
			t.Fatalf("run into %s: %v", out, err)
		}
	}
	dataA, err := os.ReadFile(filepath.Join(outA, "divergence-census-vtree.json"))
	if err != nil {
		t.Fatal(err)
	}
	dataB, err := os.ReadFile(filepath.Join(outB, "divergence-census-vtree.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dataA, dataB) {
		t.Fatal("two CLI invocations over identical inputs, writing to different directories, produced different JSON bytes")
	}
}

func assertFileExistsNonEmpty(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
	if info.Size() == 0 {
		t.Fatalf("%s exists but is empty", path)
	}
}

func assertFileAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("expected %s not to exist", path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", path, err)
	}
}
