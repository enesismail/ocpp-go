package main

import (
	"strings"
	"testing"
)

const (
	crossFilePackage  = "example.com/fixture"
	sharedPackage     = "example.com/sharedtypes"
	otherPackage      = "example.com/othertypes"
	sharedStatusValue = "example.com/sharedtypes.Status"
	siblingStatusType = "example.com/fixture.SiblingStatus"
)

// crossFileWalk walks the cross-file fixture with a resolution that knows what
// an orchestrator walking the whole tree would know: this file's own package
// identity, its import bindings, the scalar aliases the other file and the
// other package declare, and the value sets of the validators registered
// alongside them.
func crossFileWalk(t *testing.T) ([]StructInfo, string) {
	t.Helper()
	path := fixturePath(t, "go_ast", "cross_file_alias.go")
	resolver := newTypeResolver()
	resolver.RegisterFile(path, crossFilePackage)
	resolver.RegisterImport(path, ImportBinding{Alias: "shared", Path: sharedPackage})

	resolution := newResolution(resolver, newWireTypeRegistry())
	resolution.RegisterScalarAlias(sharedStatusValue, "string")
	resolution.RegisterScalarAlias(siblingStatusType, "string")
	resolution.RegisterEnum("sharedStatus", []string{"Accepted", "Rejected"})
	resolution.RegisterEnum("siblingStatus", []string{"Idle", "Busy"})

	structs, err := walkGoFile(path, resolution)
	if err != nil {
		t.Fatalf("Go AST walker is not implemented for %s: %v", path, err)
	}
	return structs, path
}

// TestCrossFileScalarAliasesFoldToTheirScalar covers the walk over named types
// a single file's AST cannot resolve: one in another package reached by a
// selector, one in a sibling file of the same package reached by a bare name.
// Both are declared as scalars, so both put a scalar on the wire, and a walk
// that stopped at the name would leave the schema-vocabulary normalizer
// reading each as an object.
func TestCrossFileScalarAliasesFoldToTheirScalar(t *testing.T) {
	structs, _ := crossFileWalk(t)
	payload := findStruct(structs, "CrossFilePayload")
	if payload == nil {
		t.Fatalf("CrossFilePayload was not discovered: %#v", structs)
	}

	cases := []struct {
		field            string
		wantDeclaredType string
		wantWireType     string
		wantSchemaKind   string
		wantEnumValues   int
	}{
		// Another package, reached by a selector.
		{field: "Status", wantDeclaredType: "shared.Status", wantWireType: "string", wantSchemaKind: "string", wantEnumValues: 2},
		// Another file of this same package, reached by a bare name.
		{field: "Sibling", wantDeclaredType: "SiblingStatus", wantWireType: "string", wantSchemaKind: "string", wantEnumValues: 2},
		// Nothing registered it, so nothing is known: the opaque treatment a
		// named type has always had must be preserved, or the registry would
		// have become a licence to guess.
		{field: "Detail", wantDeclaredType: "shared.Detail", wantWireType: "shared.Detail", wantSchemaKind: "object", wantEnumValues: 0},
	}
	for _, test := range cases {
		t.Run(test.field, func(t *testing.T) {
			field := findGoField(payload.Fields, test.field)
			if field == nil {
				t.Fatalf("field %s was not walked: %#v", test.field, payload.Fields)
			}
			if field.DeclaredType != test.wantDeclaredType {
				t.Fatalf("declaredType = %q, want %q — the literal type as written must survive resolution", field.DeclaredType, test.wantDeclaredType)
			}
			if field.WireType != test.wantWireType {
				t.Fatalf("wireType = %q, want %q", field.WireType, test.wantWireType)
			}
			if got := normalizeWireType(field.WireType, field.Slice); got != test.wantSchemaKind {
				t.Fatalf("wireType %q normalizes to schema kind %q, want %q", field.WireType, got, test.wantSchemaKind)
			}
			if len(field.EnumValues) != test.wantEnumValues {
				t.Fatalf("field carries %d accepted values, want %d: %#v", len(field.EnumValues), test.wantEnumValues, field.EnumValues)
			}
		})
	}
}

// TestCrossFileAliasFlowsIntoTheEnumRules is the point of the fold: once a
// cross-file alias carries its scalar, it reaches the same two outcomes a
// same-file alias does — a mapping change when the two value sets agree, and a
// contradiction of the schema when they do not. Before the fold both were
// unreachable, because the field was rejected as a wrong type long before
// either rule was consulted.
func TestCrossFileAliasFlowsIntoTheEnumRules(t *testing.T) {
	structs, _ := crossFileWalk(t)
	payload := findStruct(structs, "CrossFilePayload")
	if payload == nil {
		t.Fatalf("CrossFilePayload was not discovered: %#v", structs)
	}
	status := findGoField(payload.Fields, "Status")
	if status == nil {
		t.Fatalf("Status field was not walked: %#v", payload.Fields)
	}

	classify := func(t *testing.T, schemaEnum []string) ClassificationResult {
		t.Helper()
		stringType := "string"
		result, err := classifyField(ComparisonInput{
			Go: *status,
			Schema: SchemaField{
				Pointer: "#/properties/status", Type: &stringType,
				Required: true, Enum: schemaEnum,
			},
			GoPresent:     true,
			SchemaPresent: true,
		})
		if err != nil {
			t.Fatalf("classification is not implemented: %v", err)
		}
		return result
	}

	t.Run("matching value sets are a mapping change", func(t *testing.T) {
		result := classify(t, []string{"Accepted", "Rejected"})
		if result.Class != SCHEMA_FAITHFUL_CHANGE {
			t.Fatalf("a cross-file alias backing a schema enum classified as %q (rule: %s), want %q", result.Class, result.Rule, SCHEMA_FAITHFUL_CHANGE)
		}
		if !strings.Contains(strings.ToLower(result.Rule), "named enum type") {
			t.Fatalf("rule %q does not name the mapping that produces the change", result.Rule)
		}
		if !result.Breaking {
			t.Fatal("adopting the generated enum type changes the field's declared type, so this row must be breaking")
		}
	})

	t.Run("drifting value sets are a contradiction", func(t *testing.T) {
		result := classify(t, []string{"Accepted", "Rejected", "Pending"})
		if result.Class != FORK_BUG {
			t.Fatalf("a cross-file alias whose values drift from the schema's classified as %q (rule: %s), want %q", result.Class, result.Rule, FORK_BUG)
		}
		if !strings.Contains(result.Rule, "enum value-set drift") {
			t.Fatalf("rule %q reports the mapping change instead of the value-set contradiction", result.Rule)
		}
		if !strings.Contains(result.Rule, "Pending") {
			t.Fatalf("rule %q does not name the value the tree rejects", result.Rule)
		}
	})
}

// TestScalarAliasRegistryIsKeyedPerFileImportBinding proves the registry is
// keyed by what per-file resolution produces, not by the selector text: two
// files write the identical selector "shared.Status" while binding "shared" to
// different import paths, and only one of those paths is registered. A
// registry keyed by bare name — or consulted before resolution — would fold
// both, inventing a scalar for a package nothing was ever declared about.
func TestScalarAliasRegistryIsKeyedPerFileImportBinding(t *testing.T) {
	registeredPath := fixturePath(t, "go_ast", "cross_file_alias.go")
	otherPath := fixturePath(t, "go_ast", "cross_file_alias_other.go")

	resolver := newTypeResolver()
	resolver.RegisterFile(registeredPath, crossFilePackage)
	resolver.RegisterImport(registeredPath, ImportBinding{Alias: "shared", Path: sharedPackage})
	resolver.RegisterFile(otherPath, crossFilePackage)
	// The same local alias, bound to a different import path in this file.
	resolver.RegisterImport(otherPath, ImportBinding{Alias: "shared", Path: otherPackage})

	resolution := newResolution(resolver, newWireTypeRegistry())
	resolution.RegisterScalarAlias(sharedStatusValue, "string")
	resolution.RegisterScalarAlias(siblingStatusType, "string")

	registeredStructs, err := walkGoFile(registeredPath, resolution)
	if err != nil {
		t.Fatalf("walking the registered-binding file failed: %v", err)
	}
	otherStructs, err := walkGoFile(otherPath, resolution)
	if err != nil {
		t.Fatalf("walking the other-binding file failed: %v", err)
	}

	registered := findGoField(findStruct(registeredStructs, "CrossFilePayload").Fields, "Status")
	other := findGoField(findStruct(otherStructs, "OtherPackagePayload").Fields, "Status")
	if registered == nil || other == nil {
		t.Fatal("one of the two Status fields was not walked")
	}
	if registered.DeclaredType != other.DeclaredType {
		t.Fatalf("the two fixtures no longer write the identical selector: %q vs %q", registered.DeclaredType, other.DeclaredType)
	}
	if registered.WireType != "string" {
		t.Fatalf("the registered binding's field has wireType %q, want %q", registered.WireType, "string")
	}
	if other.WireType == "string" {
		t.Fatalf("the unregistered binding's field also folded to %q; the same selector text resolved to the same thing in both files, so the registry is not keyed by per-file resolution", other.WireType)
	}
	if normalizeWireType(other.WireType, other.Slice) != "object" {
		t.Fatalf("the unregistered binding's field normalizes to %q, want the unchanged opaque treatment", normalizeWireType(other.WireType, other.Slice))
	}
}

// TestBareIdentRegistryLookupDegradesWithoutPackageIdentity pins the fallback:
// a walk driven without package identities is still a valid walk of one file,
// so a bare name must fall back to reading the file's own declarations rather
// than failing or inventing an identity to look up.
func TestBareIdentRegistryLookupDegradesWithoutPackageIdentity(t *testing.T) {
	path := fixturePath(t, "go_ast", "scalar_alias.go")
	// Deliberately no RegisterFile: this walk has no package identity at all.
	structs, err := walkGoFile(path, newResolution(newTypeResolver(), newWireTypeRegistry()))
	if err != nil {
		t.Fatalf("a walk without a registered package identity failed: %v", err)
	}
	payload := findStruct(structs, "AliasPayload")
	if payload == nil {
		t.Fatalf("AliasPayload was not discovered: %#v", structs)
	}
	reason := findGoField(payload.Fields, "Reason")
	if reason == nil || reason.WireType != "string" {
		t.Fatalf("a same-file alias stopped resolving once the registry lookup was added: %#v", reason)
	}
	if findGoFieldByPath(payload.Fields, "Nested.Code") == nil {
		t.Fatalf("a same-file struct stopped being recursed into: %#v", payload.Fields)
	}
}
