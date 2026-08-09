package main

import "testing"

// TestDiscoverDirectionsRecognizes16HandlerNames covers 1.6's own
// handler-interface pair (CentralSystemHandler/ChargePointHandler), which
// shares no name with 2.0.1's CSMSHandler/ChargingStationHandler — a 1.6 run
// found every discovered direction empty until handlerInterfaceDirections
// (discover.go) learned the second pair. The fixture tree mirrors
// TestDiscoverDirectionsOverFixtureTree's shape one handler pair over, so a
// regression in either pair's own branch is caught independently of the
// other.
func TestDiscoverDirectionsRecognizes16HandlerNames(t *testing.T) {
	dir := "testdata/handler_directions"
	goFiles, err := listGoFiles(dir)
	if err != nil {
		t.Fatalf("listGoFiles: %v", err)
	}
	moduleRoot, err := findModuleRoot(dir)
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
	pkg := modulePath + "/internal/schemacheck/testdata/handler_directions"

	if got, ok := directions[pkg+".WidgetRequest"]; !ok || got != "CP->CS" {
		t.Fatalf("WidgetRequest direction = %q (ok=%v), want CP->CS (declared only on CentralSystemHandler)", got, ok)
	}
	if got, ok := directions[pkg+".GadgetRequest"]; !ok || got != "CS->CP" {
		t.Fatalf("GadgetRequest direction = %q (ok=%v), want CS->CP (declared only on ChargePointHandler)", got, ok)
	}
}
