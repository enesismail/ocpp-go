package main

import (
	"os"
	"strings"
	"testing"
)

// TestDiscoverDirectionsOverFixtureTree covers the CSMSHandler/
// ChargingStationHandler dispatch rule (discover.go) against the real
// fixture file rather than a synthetic AST, so a change to interface
// parsing that only shows up against genuine Go syntax is still caught.
func TestDiscoverDirectionsOverFixtureTree(t *testing.T) {
	goFiles, err := listGoFiles("testdata/orchestration/tree")
	if err != nil {
		t.Fatalf("listGoFiles: %v", err)
	}
	moduleRoot, err := findModuleRoot("testdata/orchestration/tree")
	if err != nil {
		t.Fatalf("findModuleRoot: %v", err)
	}
	modulePath, err := readModulePath(moduleRoot)
	if err != nil {
		t.Fatalf("readModulePath: %v", err)
	}
	resolver := newTypeResolver()
	for _, file := range goFiles {
		importPath, err := importPathForFile(file, moduleRoot, modulePath)
		if err != nil {
			t.Fatalf("importPathForFile(%s): %v", file, err)
		}
		resolver.RegisterFile(file, importPath)
	}

	directions, err := discoverDirections(goFiles, resolver)
	if err != nil {
		t.Fatalf("discoverDirections: %v", err)
	}
	pkg := modulePath + "/internal/schemacheck/testdata/orchestration/tree/widgetblock"
	got, ok := directions[pkg+".WidgetRequest"]
	if !ok {
		t.Fatalf("no direction recorded for %s.WidgetRequest: %v", pkg, directions)
	}
	if got != "CS->CSMS" {
		t.Fatalf("direction = %q, want CS->CSMS (WidgetRequest is declared only on CSMSHandler)", got)
	}
}

// TestDiscoverDirectionsReportsBidirectional covers the real DataTransfer
// shape: a request type declared on both CSMSHandler and
// ChargingStationHandler (either endpoint may send it to the other) must
// report "bidirectional", not silently pick one side or the other.
func TestDiscoverDirectionsReportsBidirectional(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir+"/both.go", `package fixture

type CSMSHandler interface {
	OnDataTransfer(stationID string, request *DataTransferRequest) (response *DataTransferResponse, err error)
}

type ChargingStationHandler interface {
	OnDataTransfer(request *DataTransferRequest) (response *DataTransferResponse, err error)
}
`)
	goFiles, err := listGoFiles(dir)
	if err != nil {
		t.Fatalf("listGoFiles: %v", err)
	}
	resolver := newTypeResolver()
	resolver.RegisterFile(goFiles[0], "example.com/fixture")

	directions, err := discoverDirections(goFiles, resolver)
	if err != nil {
		t.Fatalf("discoverDirections: %v", err)
	}
	got := directions["example.com/fixture.DataTransferRequest"]
	if got != "bidirectional" {
		t.Fatalf("direction = %q, want bidirectional", got)
	}
}

// TestCrossCheckFeaturesAndProfilesCatchesBothDirections covers message
// discovery's "hard fail either direction" requirement: a Feature type with
// a complete method set that no profile registers, and a profile
// registration naming a type with no complete method set, must each
// produce an error naming the offending identifier.
func TestCrossCheckFeaturesAndProfilesCatchesBothDirections(t *testing.T) {
	resolver := newTypeResolver()
	resolver.RegisterFile("pkg/a.go", "example.com/pkg")

	t.Run("discovered but unregistered", func(t *testing.T) {
		features := []featureInfo{{typeName: "OrphanFeature", file: "pkg/a.go"}}
		var regs []profileRegistration
		err := crossCheckFeaturesAndProfiles(features, regs, resolver)
		if err == nil {
			t.Fatal("an unregistered discovered Feature type did not produce an error")
		}
		if !strings.Contains(err.Error(), "OrphanFeature") {
			t.Fatalf("error %q does not name the offending type", err.Error())
		}
	})

	t.Run("registered but not discovered", func(t *testing.T) {
		var features []featureInfo
		regs := []profileRegistration{{name: "Pkg", featureTypes: []string{"GhostFeature"}, file: "pkg/a.go"}}
		err := crossCheckFeaturesAndProfiles(features, regs, resolver)
		if err == nil {
			t.Fatal("a registered-but-undiscovered Feature type did not produce an error")
		}
		if !strings.Contains(err.Error(), "GhostFeature") {
			t.Fatalf("error %q does not name the offending type", err.Error())
		}
	})

	t.Run("matched pair is not an error", func(t *testing.T) {
		features := []featureInfo{{typeName: "RealFeature", file: "pkg/a.go"}}
		regs := []profileRegistration{{name: "Pkg", featureTypes: []string{"RealFeature"}, file: "pkg/a.go"}}
		if err := crossCheckFeaturesAndProfiles(features, regs, resolver); err != nil {
			t.Fatalf("a matched discovery/registration pair reported an error: %v", err)
		}
	})
}

func TestTreeImportPathIsNotCWDSensitive(t *testing.T) {
	moduleRoot, err := findModuleRoot(".")
	if err != nil {
		t.Fatalf("findModuleRoot: %v", err)
	}
	modulePath, err := readModulePath(moduleRoot)
	if err != nil {
		t.Fatalf("readModulePath: %v", err)
	}
	got, err := treeImportPath("testdata/orchestration/tree", moduleRoot, modulePath)
	if err != nil {
		t.Fatalf("treeImportPath: %v", err)
	}
	want := modulePath + "/internal/schemacheck/testdata/orchestration/tree"
	if got != want {
		t.Fatalf("treeImportPath = %q, want %q — a tree root given relative to the package directory (not the module root) must still resolve to its real import path", got, want)
	}
}

func TestHasAllMethods(t *testing.T) {
	methods := []string{"GetFeatureName", "GetRequestType"}
	if hasAllMethods(methods, "GetFeatureName", "GetRequestType", "GetResponseType") {
		t.Fatal("hasAllMethods reported true for an incomplete method set")
	}
	if !hasAllMethods(methods, "GetFeatureName") {
		t.Fatal("hasAllMethods reported false for a subset that is actually present")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture file %s: %v", path, err)
	}
}
