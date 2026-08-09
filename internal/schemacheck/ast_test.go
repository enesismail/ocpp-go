package main

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestGoASTWalkerFindsFieldsTagsAndMethods(t *testing.T) {
	path := fixturePath(t, "go_ast", "fields.go")
	resolver := newTypeResolver()
	registry := newWireTypeRegistry()
	structs, err := walkGoFile(path, newResolution(resolver, registry))
	if err != nil {
		t.Fatalf("Go AST walker is not implemented for %s: %v", path, err)
	}
	// Embedded, Payload, ClearDisplayFeature, Get15118EVCertificateFeature,
	// PartialFeature — every declared struct, including the empty
	// method-holder types, must come back.
	if len(structs) != 5 {
		t.Fatalf("Go AST walker found %d structs, want 5: %#v", len(structs), structs)
	}
	payload := findStruct(structs, "Payload")
	if payload == nil {
		t.Fatal("Go AST walker did not discover Payload")
	}

	// The flattened field set must be exactly ID, Values, Code, in
	// declaration order — Embedded is anonymous (untagged, real Go
	// embedding, not a fabricated ",inline" tag), so per encoding/json
	// semantics its own field Code is promoted straight into Payload's
	// field list with no "Embedded." prefix.
	if len(payload.Fields) != 3 {
		t.Fatalf("Payload has %d flattened fields, want 3 (ID, Values, Code): %#v", len(payload.Fields), payload.Fields)
	}
	if payload.Fields[0].Name != "ID" || payload.Fields[1].Name != "Values" || payload.Fields[2].Name != "Code" {
		t.Fatalf("flattened field set/order = [%s %s %s], want [ID Values Code]",
			payload.Fields[0].Name, payload.Fields[1].Name, payload.Fields[2].Name)
	}
	if payload.Fields[0].JSONName != "id" || !payload.Fields[0].Pointer || !payload.Fields[0].Omitempty {
		t.Fatalf("field tags or pointer shape were not preserved: %#v", payload.Fields[0])
	}
	if len(payload.Fields[0].Validate) != 1 || payload.Fields[0].Validate[0] != "max=20" {
		t.Fatalf("validation tags were not tokenized: %#v", payload.Fields[0].Validate)
	}
	values := payload.Fields[1]
	if !values.Slice || values.ElementType != "string" {
		t.Fatalf("slice field shape was not captured: %#v", values)
	}
	code := payload.Fields[2]
	if code.JSONName != "code" {
		t.Fatalf("flattened embedded field lost its own tag: %#v", code)
	}

	methods, err := discoverMethodSets(path)
	if err != nil {
		t.Fatalf("method-set discovery is not implemented for %s: %v", path, err)
	}

	clearDisplay := findMethodSet(methods, "ClearDisplayFeature")
	if clearDisplay == nil || !containsAll(clearDisplay.Methods, "GetFeatureName", "GetRequestType", "GetResponseType") {
		t.Fatalf("complete method set is incomplete: %#v", clearDisplay)
	}
	if clearDisplay.RequestType != "Payload" || clearDisplay.ResponseType != "Embedded" {
		t.Fatalf("method-set type names were not resolved: request=%q response=%q", clearDisplay.RequestType, clearDisplay.ResponseType)
	}
	// The whole point of this fixture: the resolved feature-name constant
	// disagrees with the type name. A discoverer that derives the name from
	// TypeName ("ClearDisplayFeature" / "ClearDisplay") must fail here.
	if clearDisplay.FeatureName != "ClearDisplayMessage" {
		t.Fatalf("feature name was not resolved from GetFeatureName's returned constant: got %q, want %q", clearDisplay.FeatureName, "ClearDisplayMessage")
	}

	digits := findMethodSet(methods, "Get15118EVCertificateFeature")
	if digits == nil || !containsAll(digits.Methods, "GetFeatureName", "GetRequestType", "GetResponseType") {
		t.Fatalf("digits-in-name method set is incomplete: %#v", digits)
	}
	if digits.FeatureName != "Get15118EVCertificate" {
		t.Fatalf("digits-in-name feature name = %q, want %q", digits.FeatureName, "Get15118EVCertificate")
	}

	partial := findMethodSet(methods, "PartialFeature")
	if partial == nil {
		t.Fatal("partial method set (GetFeatureName only) was not discovered at all")
	}
	if !containsAll(partial.Methods, "GetFeatureName") {
		t.Fatalf("partial method set lost its one real method: %#v", partial)
	}
	if containsAll(partial.Methods, "GetRequestType") || containsAll(partial.Methods, "GetResponseType") {
		t.Fatalf("partial method set was reported as implementing methods it does not have: %#v", partial)
	}
	if partial.FeatureName != "Partial" {
		t.Fatalf("partial feature name = %q, want %q", partial.FeatureName, "Partial")
	}
	if partial.RequestType != "" || partial.ResponseType != "" {
		t.Fatalf("partial method set should have no resolved request/response type: %#v", partial)
	}
}

// TestRegisteredCustomMarshalerUsesWireType covers the real field shape: a
// pointer to a package-qualified registered type (mirroring *types.DateTime),
// not a bare local type name.
func TestRegisteredCustomMarshalerUsesWireType(t *testing.T) {
	path := fixturePath(t, "go_ast", "registered_marshaler.go")
	resolver := newTypeResolver()
	// The registered field's type, *fixturetypes.WireTime, is only
	// resolvable because this file's own import binding — the local alias
	// "fixturetypes" bound to import path "example.com/fixturetypes" — is
	// registered with the resolver. Without this line the walker has no way
	// to turn the selector "fixturetypes.WireTime" into the registry's
	// qualified key below, which is exactly what wires the resolver into
	// field walking instead of leaving it an island beside a walker that
	// only ever consults the registry.
	resolver.RegisterImport(path, ImportBinding{Alias: "fixturetypes", Path: "example.com/fixturetypes"})
	registry := newWireTypeRegistry()
	// The registry is keyed by the type's resolved, package-qualified
	// identity (import path + "." + type name) rather than its bare name —
	// bare-name keying would collapse two same-named types declared in
	// different packages (the real case: a DateTime type declared once per
	// OCPP version) onto a single entry.
	registry.Register("example.com/fixturetypes.WireTime", WireType{Type: "string", Format: "date-time"})
	structs, err := walkGoFile(path, newResolution(resolver, registry))
	if err != nil {
		t.Fatalf("registered custom marshaler was not handled: %v", err)
	}
	payload := findStruct(structs, "RegisteredPayload")
	if payload == nil || len(payload.Fields) == 0 {
		t.Fatal("registered custom-marshaler fixture was not walked")
	}

	when := findGoField(payload.Fields, "When")
	if when == nil {
		t.Fatalf("When field was not found: %#v", payload.Fields)
	}
	// declaredType must retain the literal, pointer-qualified, package-qualified
	// type exactly as written; wireType is the post-resolution wire shape.
	if when.DeclaredType != "*fixturetypes.WireTime" {
		t.Fatalf("declared type did not preserve pointer-ness/qualification: got %q, want %q", when.DeclaredType, "*fixturetypes.WireTime")
	}
	if when.WireType != "string" {
		t.Fatalf("wire type was not resolved via the registry: got %q, want %q", when.WireType, "string")
	}

	// Positive recursion case: Nested is a named, non-registered struct
	// field, so it must be recursed into and its own leaf field produced
	// with a "Nested."-prefixed dotted path.
	nestedCode := findGoFieldByPath(payload.Fields, "Nested.Code")
	if nestedCode == nil {
		t.Fatalf("named nested struct field was not recursively walked into a Nested.-prefixed leaf: %#v", payload.Fields)
	}

	// Path must genuinely be populated and dotted somewhere, or the
	// anti-recursion check below would be vacuously true.
	if nestedCode.Path == "" || !strings.Contains(nestedCode.Path, ".") {
		t.Fatalf("GoField.Path was not populated with a dotted path: %#v", nestedCode)
	}

	// Anti-recursion case: no field anywhere may have a path that recursed
	// into the registered wire type.
	for _, info := range structs {
		for _, field := range info.Fields {
			if strings.HasPrefix(field.Path, "When.") {
				t.Fatalf("registered custom-marshaler field was recursively walked: %#v", field)
			}
		}
	}
}

func TestUnregisteredCustomMarshalerIsHardError(t *testing.T) {
	path := fixturePath(t, "go_ast", "unregistered_marshaler.go")
	resolver := newTypeResolver()
	// The file's own package identity is registered so this stays a genuine
	// unregistered-*marshaler* failure — registry has nothing under any key
	// for this type — rather than an unregistered-*file* failure that would
	// be testing PackageOf's own hard error instead of this one.
	resolver.RegisterFile(path, "example.com/fixture")
	registry := newWireTypeRegistry()
	_, err := discoverCustomMarshalers(path, newResolution(resolver, registry))
	if err == nil {
		t.Fatal("unregistered custom marshaler did not produce an error")
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "unregistered custom marshaler") {
		t.Fatalf("unregistered custom marshaler did not produce the required hard error phrase: %v", err)
	}
	if !strings.Contains(err.Error(), "UnknownWireValue") {
		t.Fatalf("hard error does not name the offending type: %v", err)
	}
	if !strings.Contains(err.Error(), "unregistered_marshaler.go:7") {
		t.Fatalf("hard error does not name the file:line of the MarshalJSON declaration as %q: %v", "unregistered_marshaler.go:7", err)
	}
}

// TestDiscoverCustomMarshalersReportsRegisteredTypes covers the success
// path: given a registry that already knows about the type under its
// qualified identity, discovery must report it (bare type name, file, line)
// rather than erroring.
func TestDiscoverCustomMarshalersReportsRegisteredTypes(t *testing.T) {
	path := fixturePath(t, "go_ast", "fixturetypes", "marshaler_decl.go")
	resolver := newTypeResolver()
	// WireTime is declared IN this file, so what makes its qualified key
	// resolvable is the file's own package identity (RegisterFile), not an
	// import binding — this file imports nothing.
	resolver.RegisterFile(path, "example.com/fixturetypes")
	registry := newWireTypeRegistry()
	registry.Register("example.com/fixturetypes.WireTime", WireType{Type: "string", Format: "date-time"})
	infos, err := discoverCustomMarshalers(path, newResolution(resolver, registry))
	if err != nil {
		t.Fatalf("registered custom-marshaler discovery is not implemented for %s: %v", path, err)
	}
	// MarshalerInfo.Type stays the type's BARE declared name — the qualified
	// form is only the registry's lookup key, never what is reported back.
	info := findMarshalerInfo(infos, "WireTime")
	if info == nil {
		t.Fatalf("registered custom marshaler WireTime was not reported: %#v", infos)
	}
	if info.Line != 11 {
		t.Fatalf("registered custom marshaler line = %d, want 11 (the MarshalJSON declaration)", info.Line)
	}
	if !strings.HasSuffix(filepath.ToSlash(info.File), "marshaler_decl.go") {
		t.Fatalf("registered custom marshaler file = %q, want it to name marshaler_decl.go", info.File)
	}
}

// TestDiscoverCustomMarshalersRejectsBareNameRegistration guards against an
// implementation that tries the qualified key and silently falls back to
// the type's bare name: registering the registry entry under the bare name
// "WireTime" instead of the qualified identity
// "example.com/fixturetypes.WireTime" must still be treated as
// unregistered, identically to TestUnregisteredCustomMarshalerIsHardError.
func TestDiscoverCustomMarshalersRejectsBareNameRegistration(t *testing.T) {
	path := fixturePath(t, "go_ast", "fixturetypes", "marshaler_decl.go")
	resolver := newTypeResolver()
	resolver.RegisterFile(path, "example.com/fixturetypes")
	registry := newWireTypeRegistry()
	registry.Register("WireTime", WireType{Type: "string", Format: "date-time"})
	_, err := discoverCustomMarshalers(path, newResolution(resolver, registry))
	if err == nil {
		t.Fatal("a bare-name registry entry satisfied the qualified-identity lookup")
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "unregistered custom marshaler") {
		t.Fatalf("bare-name-only registration did not produce the required hard error phrase: %v", err)
	}
	if !strings.Contains(err.Error(), "WireTime") {
		t.Fatalf("hard error does not name the offending type: %v", err)
	}
}

// TestResolveWireTypeSucceedsOnRegisteredIdentity and
// TestResolveWireTypeFailsOnUnregisteredIdentity cover resolveWireType's two
// outcomes directly, in isolation from discovery/walking: identity here is
// already in the resolved, package-qualified form that
// discoverCustomMarshalers/walkGoFile are documented to pass it.
func TestResolveWireTypeSucceedsOnRegisteredIdentity(t *testing.T) {
	registry := newWireTypeRegistry()
	registry.Register("example.com/fixturetypes.WireTime", WireType{Type: "string", Format: "date-time"})
	wireType, err := resolveWireType("example.com/fixturetypes.WireTime", registry)
	if err != nil {
		t.Fatalf("wire type resolution is not implemented: %v", err)
	}
	if wireType.Type != "string" || wireType.Format != "date-time" {
		t.Fatalf("resolved wire type = %#v, want {string date-time}", wireType)
	}
}

func TestResolveWireTypeFailsOnUnregisteredIdentity(t *testing.T) {
	registry := newWireTypeRegistry()
	_, err := resolveWireType("example.com/fixturetypes.WireTime", registry)
	if err == nil {
		t.Fatal("unregistered wire-type identity did not produce an error")
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "unregistered wire type") {
		t.Fatalf("unregistered wire-type error missing the required phrase: %v", err)
	}
	if !strings.Contains(err.Error(), "example.com/fixturetypes.WireTime") {
		t.Fatalf("unregistered wire-type error does not name the offending identity: %v", err)
	}
}

func TestEnumExtractionResolvesAcceptedValues(t *testing.T) {
	path := fixturePath(t, "go_ast", "enums.go")
	enums, err := extractEnums(path)
	if err != nil {
		t.Fatalf("enum extraction is not implemented for %s: %v", path, err)
	}
	// The map must be keyed by the validator TAG token ("statusThing"), not
	// the Go function name ("isValidStatusThing") — an implementation that
	// keyed by function name would fail this and pass the inverse check.
	if _, wrongKey := enums["isValidStatusThing"]; wrongKey {
		t.Fatalf("enum extraction keyed by function name instead of the validator tag token: %#v", enums)
	}
	got := strings.Join(enums["statusThing"], ",")
	if got != "Accepted,Rejected" {
		t.Fatalf("enum extraction for tag %q returned %q, want Accepted,Rejected", "statusThing", got)
	}
}

// TestSelfReferentialStructCycleTerminates covers cycle detection: a struct
// referencing itself through a named field must not send the walker into
// infinite recursion, and the paths it does produce must stay stable.
func TestSelfReferentialStructCycleTerminates(t *testing.T) {
	path := fixturePath(t, "go_ast", "cycle.go")
	resolver := newTypeResolver()
	registry := newWireTypeRegistry()
	structs, err := walkGoFile(path, newResolution(resolver, registry))
	if err != nil {
		t.Fatalf("cyclic struct walk is not implemented for %s: %v", path, err)
	}
	node := findStruct(structs, "TreeNode")
	if node == nil {
		t.Fatal("self-referential struct was not discovered")
	}
	value := findGoField(node.Fields, "Value")
	if value == nil || value.Path != "Value" {
		t.Fatalf("cyclic struct's own field lost a stable path: %#v", node.Fields)
	}
	child := findGoField(node.Fields, "Child")
	if child == nil {
		t.Fatal("self-referential field was dropped instead of being kept as a terminal field")
	}
	if strings.Contains(child.Path, "Child.Child") {
		t.Fatalf("self-referential recursion did not terminate: %#v", child)
	}
}

// TestWalkGoFileHardFailsOnUnresolvedImportBinding pins the observable
// error shape for a field walk over a file that uses a package-qualified
// selector whose import binding was never registered with the resolver: the
// hard error must name this file, the field's line, and the unresolved
// selector, never silently skip the field or guess its kind. It does not
// prove the walker actually consults res.Resolver rather than reaching the
// same failure by some other means (e.g. inspecting the file's own AST
// directly) — only that this specific failure case produces the documented
// file:line + selector error.
func TestWalkGoFileHardFailsOnUnresolvedImportBinding(t *testing.T) {
	path := fixturePath(t, "go_ast", "unresolved_import.go")
	// Deliberately no RegisterImport call: this file's own import binding
	// for the "unknownpkg" alias is unknown to the resolver.
	resolver := newTypeResolver()
	registry := newWireTypeRegistry()
	_, err := walkGoFile(path, newResolution(resolver, registry))
	if err == nil {
		t.Fatal("a field using a selector with no registered import binding did not produce a hard error")
	}
	if !strings.Contains(err.Error(), "unresolved_import.go:12") {
		t.Fatalf("hard error does not name the file:line of the unresolved field as %q: %v", "unresolved_import.go:12", err)
	}
	if !strings.Contains(err.Error(), "unknownpkg.Thing") {
		t.Fatalf("hard error does not name the offending selector %q: %v", "unknownpkg.Thing", err)
	}
}

// TestStructValidatorRegistrationIsDiscovered covers discoverStructValidators:
// a StructValidate.RegisterStructValidation(validateFoo, Foo{}) call must be
// recorded against its target type.
func TestStructValidatorRegistrationIsDiscovered(t *testing.T) {
	path := fixturePath(t, "go_ast", "struct_validator.go")
	targets, err := discoverStructValidators(path)
	if err != nil {
		t.Fatalf("struct-validator discovery is not implemented for %s: %v", path, err)
	}
	if len(targets) != 1 || targets[0] != "Foo" {
		t.Fatalf("RegisterStructValidation target was not recorded: %#v", targets)
	}
}

func fixturePath(t *testing.T, parts ...string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(1)
	if !ok {
		t.Fatal("could not locate test fixture directory")
	}
	return filepath.Join(filepath.Dir(file), "testdata", filepath.Join(parts...))
}

func findStruct(structs []StructInfo, name string) *StructInfo {
	for i := range structs {
		if structs[i].Name == name {
			return &structs[i]
		}
	}
	return nil
}

func findMethodSet(methods []MethodSetInfo, name string) *MethodSetInfo {
	for i := range methods {
		if methods[i].TypeName == name {
			return &methods[i]
		}
	}
	return nil
}

func findGoField(fields []GoField, name string) *GoField {
	for i := range fields {
		if fields[i].Name == name {
			return &fields[i]
		}
	}
	return nil
}

func findGoFieldByPath(fields []GoField, path string) *GoField {
	for i := range fields {
		if fields[i].Path == path {
			return &fields[i]
		}
	}
	return nil
}

func findMarshalerInfo(infos []MarshalerInfo, typeName string) *MarshalerInfo {
	for i := range infos {
		if infos[i].Type == typeName {
			return &infos[i]
		}
	}
	return nil
}

func containsAll(values []string, wanted ...string) bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	for _, value := range wanted {
		if !set[value] {
			return false
		}
	}
	return true
}
