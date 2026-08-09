package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestResolverHandlesCrossBlockSelectors proves resolution is file-aware,
// not a selector->target map: the fixture registers the identical selector
// text against two files with different import bindings and requires the
// two resolutions to differ. A resolver that just stored and echoed back a
// single global mapping (a round-trip tautology) cannot pass this — the
// second file re-binds the same local alias to a different import path, so
// only genuinely per-file resolution produces the right answer for both.
func TestResolverHandlesCrossBlockSelectors(t *testing.T) {
	assertFileAwareResolution(t, "cross_block.json")
}

// TestResolverHandlesCrossVersionSelectors models the real ocpp1.6/logging
// cross-version case: get_log.go binds the local name "types" to
// ocpp1.6/types while log_status_notification.go binds the same local name
// to ocpp2.0.1/types. This is not a same-package enum — it is exactly the
// case that makes resolution a hard, per-file requirement rather than a
// convenience.
func TestResolverHandlesCrossVersionSelectors(t *testing.T) {
	assertFileAwareResolution(t, "cross_version.json")
}

func assertFileAwareResolution(t *testing.T, fixtureName string) {
	t.Helper()
	fixture := loadResolverImportFixture(t, fixtureName)
	if len(fixture.Resolutions) < 2 {
		t.Fatalf("fixture %s must register at least two files to prove per-file resolution", fixtureName)
	}

	resolver := newTypeResolver()
	for _, f := range fixture.Files {
		for _, imp := range f.Imports {
			resolver.RegisterImport(f.File, ImportBinding{Alias: imp.Alias, Path: imp.Path})
		}
	}

	results := make(map[string]string, len(fixture.Resolutions))
	for _, want := range fixture.Resolutions {
		got, err := resolver.Resolve(want.File, fixture.Selector)
		if err != nil {
			t.Fatalf("resolver is not implemented for selector %q in %s: %v", fixture.Selector, want.File, err)
		}
		if got != want.Resolved {
			t.Fatalf("selector %s in %s resolved to %q, want %q", fixture.Selector, want.File, got, want.Resolved)
		}
		results[want.File] = got
	}

	// Re-check divergence against the resolver's own recorded results
	// (results), not against the fixture's declared expectations again —
	// this is what actually proves per-file resolution happened, rather
	// than re-reading the same expectations the loop above already
	// verified them against.
	first := ""
	allSame := true
	for i, want := range fixture.Resolutions {
		resolved := results[want.File]
		if i == 0 {
			first = resolved
			continue
		}
		if resolved != first {
			allSame = false
		}
	}
	if allSame {
		t.Fatalf("fixture %s does not exercise per-file divergence: every resolution is %q — this would make the test a map round-trip tautology", fixtureName, first)
	}
}

func TestResolverReportsUnknownSelector(t *testing.T) {
	resolver := newTypeResolver()
	file := "ocpp1.6/unknown/file.go"
	selector := "unknown.Value"
	_, err := resolver.Resolve(file, selector)
	if err == nil {
		t.Fatal("unknown selector was not reported as a hard error")
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "unknown selector") {
		t.Fatalf("unknown selector error missing the required phrase: %v", err)
	}
	if !strings.Contains(err.Error(), selector) {
		t.Fatalf("unknown selector error does not name the offending selector %q: %v", selector, err)
	}
	if !strings.Contains(err.Error(), file) {
		t.Fatalf("unknown selector error does not name the file %q it was resolved for: %v", file, err)
	}
}

// TestPackageOfRejectsUnregisteredFile covers TypeResolver.PackageOf's hard
// error: a file whose own package identity was never recorded has no answer,
// and inventing one would silently mis-key the qualified registry lookup that
// identity feeds — two same-named types from different packages would collapse
// onto a single entry. PackageOf is the resolver's own bookkeeping rather
// than a piece of the analysis still to be written, so this covers behaviour
// the package already has, and it is here to keep that behaviour from being
// softened into a guess later.
func TestPackageOfRejectsUnregisteredFile(t *testing.T) {
	const corePackage = "github.com/enesismail/ocpp-go/ocpp1.6/core"
	resolver := newTypeResolver()
	registered := "ocpp1.6/core/boot_notification.go"
	resolver.RegisterFile(registered, corePackage)

	unregistered := "ocpp1.6/core/heartbeat.go"
	_, err := resolver.PackageOf(unregistered)
	if err == nil {
		t.Fatal("an unregistered file's package identity was answered instead of reported as a hard error")
	}
	if !strings.Contains(err.Error(), unregistered) {
		t.Fatalf("unregistered-file error does not name the file %q it was asked about: %v", unregistered, err)
	}

	// The registered sibling still resolves, so the error above is this one
	// file's missing registration rather than a resolver that answers nothing
	// at all.
	importPath, err := resolver.PackageOf(registered)
	if err != nil {
		t.Fatalf("registered file's package identity was not returned: %v", err)
	}
	if importPath != corePackage {
		t.Fatalf("registered file resolved to package %q, want %q", importPath, corePackage)
	}
}

func TestNearestRankPercentileWholeRank(t *testing.T) {
	got, err := nearestRankPercentile([]int{1, 2, 3, 4, 5, 6, 7, 8}, 50)
	if err != nil {
		t.Fatalf("nearest-rank percentile is not implemented: %v", err)
	}
	if got != 4 {
		t.Fatalf("whole-rank median=%d, want 4", got)
	}
}

func TestNearestRankPercentileRoundedRank(t *testing.T) {
	got, err := nearestRankPercentile([]int{10, 20, 30, 40, 50, 60}, 25)
	if err != nil {
		t.Fatalf("nearest-rank percentile is not implemented: %v", err)
	}
	if got != 20 {
		t.Fatalf("non-whole-rank p25=%d, want 20", got)
	}
}

// TestNearestRankPercentileP75 pins the third quartile exactly, in both rank
// forms, so the summary's p75 rests on the same hand-checkable arithmetic as
// its p25 and median rather than on whichever value the implementation happens
// to land on. Nearest rank is ceil(percentile/100 × N) taken 1-indexed into
// the sorted values: 75% of 8 is 6 exactly, selecting the 6th of 1..8; 75% of
// 6 is 4.5, which rounds up to the 5th of the six ten-step values.
func TestNearestRankPercentileP75(t *testing.T) {
	t.Run("whole rank", func(t *testing.T) {
		got, err := nearestRankPercentile([]int{1, 2, 3, 4, 5, 6, 7, 8}, 75)
		if err != nil {
			t.Fatalf("nearest-rank percentile is not implemented: %v", err)
		}
		if got != 6 {
			t.Fatalf("whole-rank p75=%d, want 6", got)
		}
	})
	t.Run("rounded rank", func(t *testing.T) {
		got, err := nearestRankPercentile([]int{10, 20, 30, 40, 50, 60}, 75)
		if err != nil {
			t.Fatalf("nearest-rank percentile is not implemented: %v", err)
		}
		if got != 50 {
			t.Fatalf("non-whole-rank p75=%d, want 50", got)
		}
	})
}

// TestNearestRankPercentileUnsortedInputIsNotMutated feeds the same values
// as TestNearestRankPercentileRoundedRank but shuffled, so a correct
// implementation must sort internally (on a copy) rather than assume
// pre-sorted input or sort in place and corrupt the caller's slice.
func TestNearestRankPercentileUnsortedInputIsNotMutated(t *testing.T) {
	input := []int{50, 10, 30, 20, 60, 40}
	original := append([]int(nil), input...)

	got, err := nearestRankPercentile(input, 25)
	if err != nil {
		t.Fatalf("nearest-rank percentile is not implemented: %v", err)
	}
	if got != 20 {
		t.Fatalf("unsorted p25=%d, want 20", got)
	}
	for i := range input {
		if input[i] != original[i] {
			t.Fatalf("nearest-rank percentile mutated its input slice: got %v, want %v", input, original)
		}
	}
}

func TestNearestRankPercentileSingleElement(t *testing.T) {
	got, err := nearestRankPercentile([]int{42}, 50)
	if err != nil {
		t.Fatalf("nearest-rank percentile is not implemented: %v", err)
	}
	if got != 42 {
		t.Fatalf("single-element percentile=%d, want 42", got)
	}
}

// TestNearestRankPercentileEmptyListIsHardError documents the chosen
// behavior for an undefined input: nearest-rank has no index to select from
// an empty list, so this is specified as a hard error rather than a
// zero-value result that could be mistaken for a real complexity score of 0.
func TestNearestRankPercentileEmptyListIsHardError(t *testing.T) {
	_, err := nearestRankPercentile(nil, 50)
	if err == nil {
		t.Fatal("empty-list percentile did not return the documented hard error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "empty") {
		t.Fatalf("empty-list percentile error does not name the reason (empty input): %v", err)
	}
}

type resolverImportFixture struct {
	Files []struct {
		File    string `json:"file"`
		Imports []struct {
			Alias string `json:"alias"`
			Path  string `json:"path"`
		} `json:"imports"`
	} `json:"files"`
	Selector    string `json:"selector"`
	Resolutions []struct {
		File     string `json:"file"`
		Resolved string `json:"resolved"`
	} `json:"resolutions"`
}

func loadResolverImportFixture(t *testing.T, name string) resolverImportFixture {
	t.Helper()
	data, err := os.ReadFile(fixturePath(t, "resolver", name))
	if err != nil {
		t.Fatalf("read resolver fixture %s: %v", name, err)
	}
	var fixture resolverImportFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("parse resolver fixture %s: %v", name, err)
	}
	if fixture.Selector == "" {
		t.Fatalf("resolver fixture %s has no selector", name)
	}
	if len(fixture.Files) == 0 {
		t.Fatalf("resolver fixture %s registers no files", name)
	}
	return fixture
}
