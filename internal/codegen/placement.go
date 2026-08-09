package main

import (
	"fmt"
	"sort"
	"strings"
)

// ComputePlacement implements the reach-count placement rule over $ref'd
// definitions only: a root is never a member of any reach set and never
// enters this computation — it is always emitted directly in its own
// owning message file. A reserved definition (the shared CustomDataType)
// is never placed either, since nothing generated is ever emitted for it.
// For every other definition, the number of declared messages (the full
// set, not only the ones actually generated) whose Reach contains it
// decides its home: more than one message -> "types"; exactly one ->
// "message:<that message's name>"; zero is a hard failure, since a
// definition no message references means the walk or the manifest is
// wrong.
func ComputePlacement(ir IR) (PlacementPlan, error) {
	candidates := map[string]bool{}
	for _, definition := range ir.Definitions {
		if definition.Kind == DefinitionRoot || definition.Reserved {
			continue
		}
		candidates[definition.Name] = true
	}

	counts := map[string]int{}
	reachingMessage := map[string]string{}
	for _, message := range ir.Messages {
		for _, name := range message.Reach {
			if !candidates[name] {
				continue
			}
			counts[name]++
			if counts[name] == 1 {
				reachingMessage[name] = message.Name
			}
		}
	}

	names := make([]string, 0, len(candidates))
	for name := range candidates {
		names = append(names, name)
	}
	sort.Strings(names)

	home := map[string]string{}
	for _, name := range names {
		switch counts[name] {
		case 0:
			definition := findDefinitionByName(ir.Definitions, name)
			files := ""
			if definition != nil {
				files = strings.Join(definition.Files, ", ")
			}
			return PlacementPlan{}, fmt.Errorf("compute definition placement: %s is unreachable (declared in %s, but no message's reach set references it)", name, files)
		case 1:
			home[name] = "message:" + reachingMessage[name]
		default:
			home[name] = "types"
		}
	}
	return PlacementPlan{Home: home}, nil
}

// collisionEntry is one rendered identifier claimed within a single
// namespace scope (a Go package, or a struct's own field set), paired with
// a human-readable description used to name both sides of a collision.
type collisionEntry struct {
	key         string
	description string
}

// messageBlocks indexes the Go package (the manifest block) of every
// declared message that names one, keyed by message name.
func messageBlocks(ir IR) map[string]string {
	blocks := make(map[string]string, len(ir.Messages))
	for _, message := range ir.Messages {
		if strings.TrimSpace(message.Block) == "" {
			continue
		}
		blocks[message.Name] = message.Block
	}
	return blocks
}

// emissionPackage names the Go package a declaration is emitted into.
//
// A placement home names a FILE — "types" for the shared types file,
// "message:<name>" for one message's own file — while Go enforces
// package-level identifiers across every file of a package at once. Two
// messages declared in the same block are two files in one package, so a
// definition placed in one message's file and a root, constructor or
// Feature symbol emitted into another's are package-mates that must share
// a single collision index. Keying that index by the block is what makes
// them share it.
//
// A message whose IR row carries no block cannot be resolved to a package,
// so its own home is used as the key. Only a hand-built IR is ever in that
// state — the schema walk copies the manifest's block onto every row — and
// the fallback can only split one package into narrower scopes, never
// merge two real packages into one.
func emissionPackage(home string, blocks map[string]string) string {
	if home == "types" {
		return "types"
	}
	if name := strings.TrimPrefix(home, "message:"); name != home {
		if block, ok := blocks[name]; ok {
			return block
		}
	}
	return home
}

// CheckEmissionCollisions extends the naming transform's collision
// hard-fail across every namespace an emitted tree can collide in:
//   - one index per Go package (the shared types package, or one per block
//     of emitted messages), covering every package-level identifier that
//     package would emit: placed definitions and their constructors, the
//     constants each placed enum's const block declares, message roots and
//     their constructors, and the Feature struct/FeatureName const every
//     message file emits unconditionally;
//   - one index per struct, scoped to that one struct's own fields, since
//     two different structs may reuse a field name freely;
//   - one index across the whole emitted set for validator tag names,
//     matching the go-playground registry's own process-global scope.
//
// Every collision is reported naming both colliding identifiers, their
// kind, and the file(s) they came from.
func CheckEmissionCollisions(ir IR, placement PlacementPlan, config TransformConfig) error {
	if err := checkPackageCollisions(ir, placement, config); err != nil {
		return err
	}
	if err := checkFieldCollisions(ir, placement, config); err != nil {
		return err
	}
	return checkValidatorTagCollisions(ir, placement, config)
}

// checkPackageCollisions builds one index per emitted Go package and reports
// the first identifier two declarations both claim.
//
// Constants are indexed alongside types and functions because Go gives them
// one flat package namespace, and because a constant is the one identifier
// here that is not derived from a name: an enum's const names come from its
// raw schema VALUES, whose own punctuation is discarded, so two values that
// differ only in a separator ("A-B" and "A.B") derive one identifier while
// every other index — the enum's type name, its validator tag — stays
// unambiguous. Reporting such a collision by identifier alone would leave
// the reader without the two values that produced it, which is why each
// constant entry names the value it came from.
func checkPackageCollisions(ir IR, placement PlacementPlan, config TransformConfig) error {
	blocks := messageBlocks(ir)
	packages := map[string][]collisionEntry{}
	// Every description ends with the file the identifier is emitted into,
	// named by its placement home: one package now holds declarations from
	// several files, so naming the package alone would leave a reader of the
	// failure without the two files to open.
	add := func(pkg, home, key, description string) {
		packages[pkg] = append(packages[pkg], collisionEntry{key: key, description: description + ", emitted in " + home})
	}

	placedNames := sortedPlacedNames(placement)
	for _, name := range placedNames {
		home := placement.Home[name]
		pkg := emissionPackage(home, blocks)
		definition := findDefinitionByName(ir.Definitions, name)
		if definition == nil {
			return fmt.Errorf("check emission collisions: placed definition %s not found in the IR", name)
		}
		goName, err := TransformIdentifier(definition.Name, definition.Kind, config)
		if err != nil {
			return fmt.Errorf("check emission collisions: %w", err)
		}
		files := strings.Join(definition.Files, ", ")
		add(pkg, home, goName, fmt.Sprintf("%s %s (%s)", string(definition.Kind), definition.Name, files))
		if definition.Kind == DefinitionObject {
			ctorName := "New" + goName
			add(pkg, home, ctorName, fmt.Sprintf("constructor %s (%s)", ctorName, files))
		}
		if definition.Kind == DefinitionEnum {
			// The const names come from the same builder the emitter renders
			// the const block with, so the index can never be checking names
			// the emitted file does not actually declare.
			constNames := buildEnumConstNames(goName, definition.Values, config)
			for i, constName := range constNames {
				add(pkg, home, constName, fmt.Sprintf("enum constant %s from value %q of enum %s (%s)", constName, definition.Values[i], definition.Name, files))
			}
		}
	}

	for _, message := range ir.Messages {
		home := "message:" + message.Name
		pkg := emissionPackage(home, blocks)
		for _, rootName := range []string{message.RequestRoot, message.ResponseRoot} {
			if rootName == "" {
				continue
			}
			files := ""
			if root := findDefinitionByName(ir.Definitions, rootName); root != nil {
				files = strings.Join(root.Files, ", ")
			}
			add(pkg, home, rootName, fmt.Sprintf("root %s (%s)", rootName, files))
			ctorName := "New" + rootName
			add(pkg, home, ctorName, fmt.Sprintf("constructor %s (%s)", ctorName, files))
		}
		featureName := message.Name + "Feature"
		add(pkg, home, featureName, "feature "+featureName)
		featureConstName := message.Name + "FeatureName"
		add(pkg, home, featureConstName, "feature-name const "+featureConstName)
	}

	pkgNames := make([]string, 0, len(packages))
	for pkg := range packages {
		pkgNames = append(pkgNames, pkg)
	}
	sort.Strings(pkgNames)
	for _, pkg := range pkgNames {
		seen := map[string]collisionEntry{}
		for _, entry := range packages[pkg] {
			if prior, ok := seen[entry.key]; ok {
				return fmt.Errorf("check emission collisions: in package %s: %s collides with %s", pkg, prior.description, entry.description)
			}
			seen[entry.key] = entry
		}
	}
	return nil
}

// checkFieldCollisions checks every struct this task ever renders — placed
// object definitions and every message root — for two properties that
// render the same Go field name. The index is scoped to one definition at
// a time: two different structs may reuse a field name freely.
func checkFieldCollisions(ir IR, placement PlacementPlan, config TransformConfig) error {
	var structs []Definition
	for _, name := range sortedPlacedNames(placement) {
		definition := findDefinitionByName(ir.Definitions, name)
		if definition != nil && definition.Kind == DefinitionObject {
			structs = append(structs, *definition)
		}
	}
	for _, message := range ir.Messages {
		for _, rootName := range []string{message.RequestRoot, message.ResponseRoot} {
			if rootName == "" {
				continue
			}
			if root := findDefinitionByName(ir.Definitions, rootName); root != nil {
				structs = append(structs, *root)
			}
		}
	}

	for _, definition := range structs {
		seen := map[string]Property{}
		for _, property := range definition.Properties {
			fieldName, err := transformPropertyName(property.Name, config)
			if err != nil {
				return fmt.Errorf("check emission collisions: %w", err)
			}
			if prior, ok := seen[fieldName]; ok {
				return fmt.Errorf("check emission collisions: field %s on %s %s (%s) claimed by both properties %s and %s",
					fieldName, string(definition.Kind), definition.Name, strings.Join(definition.Files, ", "), prior.Name, property.Name)
			}
			seen[fieldName] = property
		}
	}
	return nil
}

// checkValidatorTagCollisions checks every placed enum's derived tag
// name for a duplicate across the whole emitted set, independent of
// package: the go-playground registry is process-global, so two enums in
// two different packages deriving the same tag string is still a
// collision, one a per-package index can never see.
func checkValidatorTagCollisions(ir IR, placement PlacementPlan, config TransformConfig) error {
	seen := map[string]Definition{}
	for _, name := range sortedPlacedNames(placement) {
		definition := findDefinitionByName(ir.Definitions, name)
		if definition == nil || definition.Kind != DefinitionEnum {
			continue
		}
		tag, err := validatorTagName(definition.Name, config)
		if err != nil {
			return fmt.Errorf("check emission collisions: %w", err)
		}
		if prior, ok := seen[tag]; ok {
			return fmt.Errorf("check emission collisions: validator %s registered by both enum %s (%s) and enum %s (%s)",
				tag, prior.Name, strings.Join(prior.Files, ", "), definition.Name, strings.Join(definition.Files, ", "))
		}
		seen[tag] = *definition
	}
	return nil
}

func sortedPlacedNames(placement PlacementPlan) []string {
	names := make([]string, 0, len(placement.Home))
	for name := range placement.Home {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
