// Command symboldelta is a standalone-module rename/change sweep tool for
// this repository's code-generation work: it extracts a syntax-level symbol
// table from a source tree,
// diffs two such tables into a rename/change inventory, and sweeps a live,
// buildable tree for every AST use-site of that inventory's rows, using
// golang.org/x/tools/go/packages for real type-checker resolution during
// the sweep specifically (Uses/Defs/Selections maps, not lexical text
// matching -- see sweep.go). It is a standalone module (its own go.mod,
// requiring golang.org/x/tools) so the root module's dependency graph is
// never touched; it is never imported by anything this repository ships,
// only ever invoked as `go run .` from inside this directory.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("symboldelta", flag.ContinueOnError)

	extract := flags.Bool("extract", false, "extract a symbol table from -root (recursive) or -file (repeatable, explicit list) into -out")
	root := flags.String("root", "", "source tree root for -extract (recursive walk; mutually exclusive with -file)")
	var explicitFiles stringSlice
	flags.Var(&explicitFiles, "file", "explicit file to extract, relative to -root or absolute (repeatable; mutually exclusive with recursive -root use -- pass -root as the common ancestor for relative reporting when combined with -file)")

	diff := flags.Bool("diff", false, "diff -old against -new, writing delta rows into -out")
	oldPath := flags.String("old", "", "old side symbol table (-extract output) for -diff")
	newPath := flags.String("new", "", "new side symbol table (-extract output) for -diff")

	sweep := flags.Bool("sweep", false, "sweep -pkg (repeatable) for use-sites of -delta's rows into -out")
	deltaPath := flags.String("delta", "", "delta rows (-diff output) for -sweep")
	dir := flags.String("dir", "", "working directory go/packages resolves -pkg patterns from")
	var pkgPatterns stringSlice
	flags.Var(&pkgPatterns, "pkg", "package pattern to sweep (repeatable)")
	typesPrefix := flags.String("types-prefix", "", "import-path prefix a resolved type/func must carry to count as a hit (the 1.6/2.0.1 same-package-name guard)")

	out := flags.String("out", "", "output path")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	modes := 0
	for _, b := range []bool{*extract, *diff, *sweep} {
		if b {
			modes++
		}
	}
	if modes != 1 {
		return fmt.Errorf("choose exactly one of -extract, -diff, -sweep")
	}
	if *out == "" {
		return fmt.Errorf("-out is required")
	}

	switch {
	case *extract:
		if *root == "" {
			return fmt.Errorf("-extract requires -root")
		}
		var table *SymbolTable
		var err error
		if len(explicitFiles) > 0 {
			files := make([]string, len(explicitFiles))
			for i, f := range explicitFiles {
				if filepath.IsAbs(f) {
					files[i] = f
				} else {
					files[i] = filepath.Join(*root, f)
				}
			}
			table, err = extractFiles(*root, files)
		} else {
			table, err = extractTree(*root)
		}
		if err != nil {
			return fmt.Errorf("extract %s: %w", *root, err)
		}
		return writeJSON(*out, table)

	case *diff:
		if *oldPath == "" || *newPath == "" {
			return fmt.Errorf("-diff requires -old and -new")
		}
		var oldTable, newTable SymbolTable
		if err := readJSON(*oldPath, &oldTable); err != nil {
			return fmt.Errorf("read -old %s: %w", *oldPath, err)
		}
		if err := readJSON(*newPath, &newTable); err != nil {
			return fmt.Errorf("read -new %s: %w", *newPath, err)
		}
		rows := computeDelta(&oldTable, &newTable)
		if err := writeJSON(*out, rows); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "symboldelta: %d delta rows written to %s\n", len(rows), *out)
		printClassCounts(rows)
		return nil

	case *sweep:
		if *deltaPath == "" {
			return fmt.Errorf("-sweep requires -delta")
		}
		if len(pkgPatterns) == 0 {
			return fmt.Errorf("-sweep requires at least one -pkg")
		}
		if *typesPrefix == "" {
			return fmt.Errorf("-sweep requires -types-prefix")
		}
		var rows []DeltaRow
		if err := readJSON(*deltaPath, &rows); err != nil {
			return fmt.Errorf("read -delta %s: %w", *deltaPath, err)
		}
		result, err := sweepTree(*dir, pkgPatterns, *typesPrefix, rows)
		if err != nil {
			return fmt.Errorf("sweep %v: %w", pkgPatterns, err)
		}
		if err := writeJSON(*out, result); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "symboldelta: %d hits across %d files written to %s\n", len(result.Hits), len(result.Files), *out)
		if len(result.UnhitRows) > 0 {
			fmt.Fprintf(os.Stderr, "symboldelta: %d delta rows had no hit in the swept trees:\n", len(result.UnhitRows))
			for _, r := range result.UnhitRows {
				fmt.Fprintf(os.Stderr, "  %s\n", r)
			}
		}
		return nil
	}
	return nil
}

func printClassCounts(rows []DeltaRow) {
	counts := map[DeltaClass]int{}
	for _, r := range rows {
		counts[r.Class]++
	}
	classes := make([]string, 0, len(counts))
	for c := range counts {
		classes = append(classes, string(c))
	}
	sort.Strings(classes)
	for _, c := range classes {
		fmt.Fprintf(os.Stderr, "  %-32s %d\n", c, counts[DeltaClass(c)])
	}
}

func writeJSON(path string, v interface{}) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create directory for %s: %w", path, err)
		}
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func readJSON(path string, v interface{}) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
