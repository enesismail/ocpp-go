package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

// WalkSchemas builds the complete IR for the manifest's schema directory.
// manifest.SchemaDir is used exactly as given — WalkSchemas does not
// interpret it as relative to anything; that resolution is LoadManifest's
// job, done before a Manifest reaches this function. It reads exactly the
// request/response files the manifest's rows name, nothing else in
// SchemaDir: fixture directories under this package's testdata also hold
// files that exist only to make some other row or file fail on purpose, and
// a walk that scanned the directory instead of following the manifest would
// trip over them.
func WalkSchemas(manifest Manifest) (IR, error) {
	documents := make(map[string]SchemaDocument)
	var allDefinitions []Definition

	readFile := func(filename string) (SchemaDocument, error) {
		if document, ok := documents[filename]; ok {
			return document, nil
		}
		document, err := WalkSchema(filepath.Join(manifest.SchemaDir, filename))
		if err != nil {
			return SchemaDocument{}, err
		}
		documents[filename] = document
		allDefinitions = append(allDefinitions, document.Root)
		allDefinitions = append(allDefinitions, document.Definitions...)
		return document, nil
	}

	messages := make([]Message, 0, len(manifest.Messages))
	for _, row := range manifest.Messages {
		requestDoc, err := readFile(row.Request)
		if err != nil {
			return IR{}, err
		}
		responseDoc, err := readFile(row.Response)
		if err != nil {
			return IR{}, err
		}

		roots := []string{requestDoc.Root.Name, responseDoc.Root.Name}
		sort.Strings(roots)

		messages = append(messages, Message{
			Name:         row.Name,
			Block:        row.Block,
			Direction:    row.Direction,
			Request:      row.Request,
			Response:     row.Response,
			RequestRoot:  requestDoc.Root.Name,
			ResponseRoot: responseDoc.Root.Name,
			Roots:        roots,
			Reach:        computeReach(requestDoc, responseDoc),
			Emit:         row.Emit,
		})
	}

	definitions, err := Deduplicate(allDefinitions)
	if err != nil {
		return IR{}, err
	}

	sort.Slice(messages, func(i, j int) bool { return messages[i].Name < messages[j].Name })

	return IR{Definitions: definitions, Messages: messages}, nil
}

// computeReach walks a message's request and response root properties,
// following every $ref (and array item $ref) transitively, and returns the
// sorted set of named definitions reached. Neither root itself is ever a
// member: the traversal starts from their properties, never adds a root's
// own name, and every $ref in this corpus is local to the file it appears
// in, so requestDoc's own definitions resolve request-side refs and
// responseDoc's own definitions resolve response-side refs.
func computeReach(requestDoc, responseDoc SchemaDocument) []string {
	lookup := make(map[string]Definition, len(requestDoc.Definitions)+len(responseDoc.Definitions))
	for _, definition := range requestDoc.Definitions {
		lookup[definition.Name] = definition
	}
	for _, definition := range responseDoc.Definitions {
		lookup[definition.Name] = definition
	}

	reached := make(map[string]bool)
	var visit func(properties []Property)
	visit = func(properties []Property) {
		for _, property := range properties {
			if property.Ref != "" {
				name := strings.TrimPrefix(property.Ref, "#/definitions/")
				if !reached[name] {
					reached[name] = true
					if definition, ok := lookup[name]; ok {
						visit(definition.Properties)
					}
				}
			}
			if property.Items != nil {
				visit([]Property{*property.Items})
			}
		}
	}
	visit(requestDoc.Root.Properties)
	visit(responseDoc.Root.Properties)

	result := make([]string, 0, len(reached))
	for name := range reached {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

// Deduplicate merges same-named definitions only when their complete
// structure agrees and records deterministic occurrence metadata. "Complete
// structure" includes an enum's value list: two same-named enum definitions
// whose values differ are as much a conflict as two object definitions whose
// properties differ, and merging them into the union of both value sets
// would invent an enum neither schema declares.
//
// A conflict is a hard failure naming both files and the JSON pointer of the
// first difference. A property-level difference is named by that property's
// own pointer ("#/definitions/SharedType/properties/id"); a value-set
// difference is named by the enum keyword the values were read from
// ("#/definitions/StatusEnumType/enum") together with the differing values
// themselves, because a pointer alone says where to look but not which of
// the two lists is the unexpected one.
func Deduplicate(definitions []Definition) ([]Definition, error) {
	type group struct {
		canonical  Definition
		files      map[string]bool
		firstFiles string
	}

	order := make([]string, 0, len(definitions))
	groups := make(map[string]*group, len(definitions))

	for _, definition := range definitions {
		current, ok := groups[definition.Name]
		if !ok {
			canonical := definition
			canonical.Files = nil
			canonical.Occurrences = 0
			current = &group{canonical: canonical, files: map[string]bool{}, firstFiles: strings.Join(definition.Files, ",")}
			groups[definition.Name] = current
			order = append(order, definition.Name)
		} else if equal, pointer, detail := definitionsStructurallyEqual(current.canonical, definition); !equal {
			message := fmt.Sprintf("deduplicate definitions: %s and %s disagree at %s for definition %s",
				current.firstFiles, strings.Join(definition.Files, ","), pointer, definition.Name)
			if detail != "" {
				message += ": " + detail
			}
			return nil, errors.New(message)
		}
		for _, file := range definition.Files {
			current.files[file] = true
		}
	}

	result := make([]Definition, 0, len(order))
	for _, name := range order {
		group := groups[name]
		files := make([]string, 0, len(group.files))
		for file := range group.files {
			files = append(files, file)
		}
		sort.Strings(files)
		merged := group.canonical
		merged.Files = files
		merged.Occurrences = len(files)
		result = append(result, merged)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

// definitionsStructurallyEqual compares two same-named definitions on every
// field that describes their shape (never on Files or Occurrences, which
// record where they came from rather than what they are). It returns the
// JSON pointer of the first disagreement and, for an enum value-set
// conflict, a detail string naming both value lists — a pointer alone says
// where to look but not which list is the unexpected one.
func definitionsStructurallyEqual(a, b Definition) (equal bool, pointer string, detail string) {
	if a.Kind != b.Kind {
		return false, fmt.Sprintf("#/definitions/%s", a.Name), fmt.Sprintf("kind %s vs %s", a.Kind, b.Kind)
	}
	if !boolPointersEqual(a.AdditionalProperties, b.AdditionalProperties) {
		return false, fmt.Sprintf("#/definitions/%s/additionalProperties", a.Name), ""
	}
	if a.Kind == DefinitionEnum {
		if !reflect.DeepEqual(a.Values, b.Values) {
			return false, fmt.Sprintf("#/definitions/%s/enum", a.Name), fmt.Sprintf("%v vs %v", a.Values, b.Values)
		}
		return true, "", ""
	}
	if len(a.Properties) != len(b.Properties) {
		return false, fmt.Sprintf("#/definitions/%s/properties", a.Name), fmt.Sprintf("%d properties vs %d", len(a.Properties), len(b.Properties))
	}
	for i := range a.Properties {
		if !reflect.DeepEqual(a.Properties[i], b.Properties[i]) {
			pointer := a.Properties[i].Pointer
			if pointer == "" {
				pointer = b.Properties[i].Pointer
			}
			return false, pointer, ""
		}
	}
	return true, "", ""
}

func boolPointersEqual(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// referenceHop is one $ref followed during cycle detection: the definition
// it names, and the JSON pointer of the property carrying it.
type referenceHop struct {
	name    string
	pointer string
}

// DetectCycles rejects self-referential and mutually recursive reference
// paths while permitting ordinary transitive object nesting. Every $ref in
// this corpus is local to the file its definitions came from, so a cycle can
// only ever run through definitions passed in together from the same file.
//
// A cycle is reported as both chains that describe it: the definition names
// in the order they were entered, and the JSON pointers of the $refs that
// joined them. The names say what the loop is; the pointers say which
// property to edit to break it, which matters most in the case that
// motivates the check — a definition reached from several places, where the
// name alone leaves every reference to it a candidate.
func DetectCycles(definitions []Definition) error {
	byName := make(map[string]Definition, len(definitions))
	for _, definition := range definitions {
		byName[definition.Name] = definition
	}

	const (
		unvisited = iota
		visiting
		done
	)
	state := make(map[string]int, len(definitions))
	var trail []referenceHop

	var visit func(hop referenceHop) error
	visit = func(hop referenceHop) error {
		switch state[hop.name] {
		case done:
			return nil
		case visiting:
			start := indexOfHop(trail, hop.name)
			names := make([]string, 0, len(trail)-start+1)
			for _, entry := range trail[start:] {
				names = append(names, entry.name)
			}
			names = append(names, hop.name)
			pointers := make([]string, 0, len(trail)-start)
			for _, entry := range trail[start+1:] {
				pointers = append(pointers, entry.pointer)
			}
			pointers = append(pointers, hop.pointer)
			return fmt.Errorf("definitions reference cycle: %s (through %s)",
				strings.Join(names, " -> "), strings.Join(pointers, " -> "))
		}
		state[hop.name] = visiting
		trail = append(trail, hop)
		if definition, ok := byName[hop.name]; ok {
			for _, next := range referencedHops(definition) {
				if err := visit(next); err != nil {
					return err
				}
			}
		}
		trail = trail[:len(trail)-1]
		state[hop.name] = done
		return nil
	}

	for _, definition := range definitions {
		if state[definition.Name] == unvisited {
			if err := visit(referenceHop{name: definition.Name}); err != nil {
				return err
			}
		}
	}
	return nil
}

// referencedHops lists every definition this definition's properties $ref,
// each paired with the JSON pointer of the property carrying the reference.
// A property read from a schema file already carries its own pointer; one
// built in memory need not, so the pointer is reconstructed from the
// enclosing definition and property names when it is absent, and the
// reported chain reads the same either way.
func referencedHops(definition Definition) []referenceHop {
	prefix := fmt.Sprintf("#/definitions/%s/properties", definition.Name)
	if definition.Kind == DefinitionRoot {
		prefix = "#/properties"
	}
	var hops []referenceHop
	for _, property := range definition.Properties {
		pointer := property.Pointer
		if pointer == "" {
			pointer = prefix + "/" + property.Name
		}
		hops = append(hops, propertyHops(property, pointer)...)
	}
	return hops
}

// propertyHops collects the references one property carries, following an
// array's element schema to whatever depth it nests.
func propertyHops(property Property, pointer string) []referenceHop {
	var hops []referenceHop
	if property.Ref != "" {
		hops = append(hops, referenceHop{name: strings.TrimPrefix(property.Ref, "#/definitions/"), pointer: pointer})
	}
	if property.Items != nil {
		itemPointer := property.Items.Pointer
		if itemPointer == "" {
			itemPointer = pointer + "/items"
		}
		hops = append(hops, propertyHops(*property.Items, itemPointer)...)
	}
	return hops
}

func indexOfHop(trail []referenceHop, target string) int {
	for i, hop := range trail {
		if hop.name == target {
			return i
		}
	}
	return -1
}
