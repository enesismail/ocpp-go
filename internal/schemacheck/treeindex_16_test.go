package main

import "testing"

// TestBuildTreeIndexResolvesValidatorAcrossPackageFiles covers a shape
// ocpp1.6/types actually has and ocpp2.0.1 does not: a
// Validate.RegisterValidation call in one file naming a validator function
// declared in a *different* file of the same package. Before treeindex.go
// grew a package-scoped function-value index, extractEnumsFromFile's
// single-file funcs lookup silently found nothing for such a tag — the
// affected fields fell to UNEXPLAINED with an empty accepted-value set
// instead of being compared against the schema at all.
func TestBuildTreeIndexResolvesValidatorAcrossPackageFiles(t *testing.T) {
	dir := "testdata/validator_registry"
	goFiles, err := listGoFiles(dir)
	if err != nil {
		t.Fatalf("listGoFiles: %v", err)
	}
	if len(goFiles) != 2 {
		t.Fatalf("fixture must have exactly 2 files (the call site and the declaration site): %v", goFiles)
	}
	moduleRoot, err := findModuleRoot(dir)
	if err != nil {
		t.Fatalf("findModuleRoot: %v", err)
	}
	modulePath, err := readModulePath(moduleRoot)
	if err != nil {
		t.Fatalf("readModulePath: %v", err)
	}

	idx, err := buildTreeIndex(goFiles, moduleRoot, modulePath)
	if err != nil {
		t.Fatalf("buildTreeIndex: %v", err)
	}

	values, ok := idx.resolution.Enums["widgetStatus"]
	if !ok {
		t.Fatal(`buildTreeIndex did not register "widgetStatus" at all — the cross-file RegisterValidation call was not resolved against the sibling file's isValidWidgetStatus function`)
	}
	want := []string{"OK", "Broken"}
	if len(values) != len(want) || values[0] != want[0] || values[1] != want[1] {
		t.Fatalf("widgetStatus accepted values = %v, want %v", values, want)
	}
}
