package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// loadJSONDocument reads path and parses it as a generic JSON document, the
// shape every schema-reading function in this package starts from.
func loadJSONDocument(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("load JSON document %s: %w", path, err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("load JSON document %s: %w", path, err)
	}
	return document, nil
}

var knownSchemaDrafts = map[string]bool{
	"http://json-schema.org/draft-04/schema#": true,
	"http://json-schema.org/draft-06/schema#": true,
}

// detectSchemaShape reports five independently observed, per-file facts:
// the declared dialect, which key spells the document's own identifier,
// whether it carries a definitions block, whether it uses $ref anywhere, and
// which request-filename convention this one file's own name follows. None
// of the five is derived from any of the others — each is read straight off
// the document or the filename.
func detectSchemaShape(document map[string]any, filename string) (SchemaShape, error) {
	schema, _ := document["$schema"].(string)
	if !knownSchemaDrafts[schema] {
		return SchemaShape{}, fmt.Errorf("unknown schema draft %q in %s: dialect %s is not recognized", schema, filename, draftLabel(schema))
	}

	identifier := ""
	if _, ok := document["$id"]; ok {
		identifier = "$id"
	} else if _, ok := document["id"]; ok {
		identifier = "id"
	}

	_, hasDefinitions := document["definitions"]

	return SchemaShape{
		Schema:            schema,
		IdentifierKeyword: identifier,
		HasDefinitions:    hasDefinitions,
		HasReferences:     containsRef(document),
		FilenameShape:     filenameShape(filename),
	}, nil
}

// draftLabel pulls the "draft-NN" segment out of a $schema URL for use in an
// error message, falling back to the whole URL if the segment is absent.
func draftLabel(schemaURL string) string {
	for _, segment := range strings.Split(schemaURL, "/") {
		if strings.HasPrefix(segment, "draft-") {
			return segment
		}
	}
	return schemaURL
}

// filenameShape classifies a single file's own filename morphology: whether
// it is this schema set's request-filename convention says a request should
// carry a "Request" suffix or stay bare, this file just reads its own name.
func filenameShape(filename string) string {
	switch {
	case strings.HasSuffix(filename, "Response.json"):
		return "response"
	case strings.HasSuffix(filename, "Request.json"):
		return "request-suffixed"
	default:
		return "request-bare"
	}
}

// containsRef reports whether "$ref" appears anywhere in the document, at
// any nesting depth.
func containsRef(node any) bool {
	switch v := node.(type) {
	case map[string]any:
		if _, ok := v["$ref"]; ok {
			return true
		}
		for _, child := range v {
			if containsRef(child) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if containsRef(child) {
				return true
			}
		}
	}
	return false
}

// requestFilenameConvention derives, from a whole directory's filenames,
// whether that set's request files are uniformly named with a "Request"
// suffix or uniformly bare. It never looks at a single file's dialect or
// identifier spelling — only the set of filenames a request file belongs to
// determines this, and a set with no request files, or with both
// conventions present, has no single answer.
func requestFilenameConvention(filenames []string) (string, error) {
	if len(filenames) == 0 {
		return "", fmt.Errorf("determine request-filename convention: empty filename set")
	}
	shapes := map[string]bool{}
	for _, name := range filenames {
		shape := filenameShape(name)
		if shape == "response" {
			continue
		}
		shapes[shape] = true
	}
	switch len(shapes) {
	case 0:
		return "", fmt.Errorf("determine request-filename convention: empty set — no request filenames among %v", filenames)
	case 1:
		for shape := range shapes {
			return shape, nil
		}
	}
	return "", fmt.Errorf("determine request-filename convention: mixed/ambiguous filename set %v carries both request-bare and request-suffixed names", filenames)
}

// resolveLocalRef resolves a single-level local JSON pointer ("#/a/b/...")
// against document. A reference that is not local (does not start with
// "#/"), or whose target is itself another $ref (a multi-level chain), is a
// hard error rather than something this function silently follows or
// flattens.
func resolveLocalRef(document map[string]any, reference string) (map[string]any, error) {
	if !strings.HasPrefix(reference, "#/") {
		return nil, fmt.Errorf("non-local $ref %q: only local references (starting with \"#/\") are supported", reference)
	}
	var current any = document
	for _, segment := range strings.Split(strings.TrimPrefix(reference, "#/"), "/") {
		container, ok := current.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("local $ref %q could not be resolved: %q is not an object", reference, segment)
		}
		next, ok := container[segment]
		if !ok {
			return nil, fmt.Errorf("local $ref %q could not be resolved: missing %q", reference, segment)
		}
		current = next
	}
	resolved, ok := current.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("local $ref %q does not resolve to an object", reference)
	}
	if _, chained := resolved["$ref"]; chained {
		return nil, fmt.Errorf("multi-level $ref chain at %q: the resolved definition is itself a $ref, which is not supported", reference)
	}
	return resolved, nil
}

// definitionNameFromRef extracts <Name> out of a "#/definitions/<Name>"
// reference. Any other local pointer shape (nothing else appears in this
// corpus) reports ok=false, and callers fall back to treating the
// recursion as un-named (no new enclosing definition).
func definitionNameFromRef(ref string) (string, bool) {
	const prefix = "#/definitions/"
	if !strings.HasPrefix(ref, prefix) {
		return "", false
	}
	name := strings.TrimPrefix(ref, prefix)
	if name == "" || strings.Contains(name, "/") {
		return "", false
	}
	return name, true
}

// schemaWalker carries the one piece of state the recursive property walk
// needs beyond its own arguments: the whole document, so that a $ref
// anywhere in the walk can be resolved back against its root.
type schemaWalker struct {
	root map[string]any
	doc  *SchemaDocument
}

// walkSchema reads path, resolves every local $ref it finds, and returns a
// flat, fully walked list of every property the document declares — both
// the object/array-typed properties that merely contain further properties
// and the scalar leaves at the bottom of that nesting, each with its own
// occurrence pointer.
func walkSchema(path string) (SchemaDocument, error) {
	document, err := loadJSONDocument(path)
	if err != nil {
		return SchemaDocument{}, err
	}
	shape, err := detectSchemaShape(document, filepath.Base(path))
	if err != nil {
		return SchemaDocument{}, err
	}

	doc := &SchemaDocument{File: path, Shape: shape, RootType: typeOf(document)}
	w := &schemaWalker{root: document, doc: doc}
	w.recordIgnoredKeywords(document, "#")
	if err := w.walkObjectProperties(document, "#/properties", "", ""); err != nil {
		return SchemaDocument{}, err
	}
	return *doc, nil
}

// recordIgnoredKeywords notes every occurrence of a vendor/documentation-only
// keyword on obj, keyed by the pointer obj itself was reached at — a keyword
// seen at two different pointers is recorded twice, never collapsed into one
// document-wide flag.
func (w *schemaWalker) recordIgnoredKeywords(obj map[string]any, pointer string) {
	for _, keyword := range []string{"javaType", "comment", "description"} {
		if _, ok := obj[keyword]; ok {
			w.doc.IgnoredKeywords = append(w.doc.IgnoredKeywords, pointer+":"+keyword)
		}
	}
}

// walkObjectProperties walks one "properties" block belonging to obj —
// either the document root or a $ref'd/inline object definition — carrying
// obj's own "required" array down to mark each property it names.
// pointerPrefix is the pointer segment every property in this block is
// declared under (e.g. "#/properties" at the root, or
// "#/definitions/ModemType/properties" for a block reached through a $ref
// to ModemType); enclosingDefinition is the name of that reached-through
// definition, or "" for the root/an inline object. pathPrefix is the dotted
// logical path this block was reached by, which — unlike pointerPrefix —
// differs between two references to the same definition and is therefore
// what keeps their properties distinguishable.
func (w *schemaWalker) walkObjectProperties(obj map[string]any, pointerPrefix, enclosingDefinition, pathPrefix string) error {
	propsRaw, ok := obj["properties"]
	if !ok {
		return nil
	}
	props, ok := propsRaw.(map[string]any)
	if !ok {
		return nil
	}
	required := stringSet(obj["required"])
	for _, name := range sortedKeys(props) {
		val, ok := props[name].(map[string]any)
		if !ok {
			continue
		}
		pointer := pointerPrefix + "/" + name
		path := name
		if pathPrefix != "" {
			path = pathPrefix + "." + name
		}
		if err := w.walkPropertyOccurrence(val, pointer, path, name, enclosingDefinition, required[name]); err != nil {
			return err
		}
	}
	return nil
}

// walkPropertyOccurrence records the row for one named property occurrence
// (pointer is where it is textually declared) and recurses into whatever it
// contains: a $ref is resolved first — the row itself still carries the
// *referencing* occurrence's own pointer and enclosingDefinition, but
// further recursion into the resolved definition's own nested properties is
// rooted at that definition's own pointer and carries its name as the new
// enclosing definition, because that is where those nested properties are
// themselves textually declared.
//
// The dotted path threads through that resolution rather than restarting at
// it: a definition's properties are declared once but reached once per
// reference, so the path is what tells those reaches apart. A schema that
// refers to the same definition from three places yields three occurrences
// of each of its properties, sharing a pointer and differing in path.
func (w *schemaWalker) walkPropertyOccurrence(val map[string]any, pointer, path, name, enclosingDefinition string, required bool) error {
	target := val
	recursionPrefix := pointer
	recursionEnclosing := enclosingDefinition

	if refRaw, ok := val["$ref"]; ok {
		refPath, ok := refRaw.(string)
		if !ok {
			return fmt.Errorf("schema walk %s: $ref at %s is not a string", pointer, pointer)
		}
		resolved, err := resolveLocalRef(w.root, refPath)
		if err != nil {
			return err
		}
		target = resolved
		if defName, ok := definitionNameFromRef(refPath); ok {
			recursionPrefix = "#/definitions/" + defName
			recursionEnclosing = defName
		}
	}

	w.recordIgnoredKeywords(target, pointer)
	w.doc.Properties = append(w.doc.Properties, buildSchemaProperty(target, pointer, path, name, required, enclosingDefinition))

	switch typeOf(target) {
	case "object":
		return w.walkObjectProperties(target, recursionPrefix+"/properties", recursionEnclosing, path)
	case "array":
		if items, ok := target["items"].(map[string]any); ok {
			return w.walkArrayItems(items, recursionPrefix+"/items", recursionEnclosing, path)
		}
	}
	return nil
}

// walkArrayItems continues the walk through an array's element schema.
// Unlike a named property, the items schema itself never becomes its own
// row — it has no property name a Go field could ever pair against — it is
// purely a pointer-path segment on the way to whatever named properties (or
// further items) it contains.
func (w *schemaWalker) walkArrayItems(items map[string]any, pointer, enclosingDefinition, pathPrefix string) error {
	target := items
	recursionPrefix := pointer
	recursionEnclosing := enclosingDefinition

	if refRaw, ok := items["$ref"]; ok {
		refPath, ok := refRaw.(string)
		if !ok {
			return fmt.Errorf("schema walk %s: $ref is not a string", pointer)
		}
		resolved, err := resolveLocalRef(w.root, refPath)
		if err != nil {
			return err
		}
		target = resolved
		if defName, ok := definitionNameFromRef(refPath); ok {
			recursionPrefix = "#/definitions/" + defName
			recursionEnclosing = defName
		}
	}

	w.recordIgnoredKeywords(target, pointer)

	switch typeOf(target) {
	case "object":
		return w.walkObjectProperties(target, recursionPrefix+"/properties", recursionEnclosing, pathPrefix)
	case "array":
		if nested, ok := target["items"].(map[string]any); ok {
			return w.walkArrayItems(nested, recursionPrefix+"/items", recursionEnclosing, pathPrefix)
		}
	}
	return nil
}

func typeOf(obj map[string]any) string {
	t, _ := obj["type"].(string)
	return t
}

func buildSchemaProperty(target map[string]any, pointer, path, name string, required bool, enclosingDefinition string) SchemaProperty {
	prop := SchemaProperty{
		Pointer:             pointer,
		Path:                path,
		Name:                name,
		Type:                typeOf(target),
		Required:            required,
		EnclosingDefinition: enclosingDefinition,
		Constraints:         buildConstraints(target),
	}
	if format, ok := target["format"].(string); ok {
		prop.Format = &format
	}
	if enumRaw, ok := target["enum"].([]any); ok {
		for _, v := range enumRaw {
			if s, ok := v.(string); ok {
				prop.Enum = append(prop.Enum, s)
			}
		}
	}
	return prop
}

func buildConstraints(target map[string]any) Constraints {
	var c Constraints
	if v, ok := target["maxLength"].(float64); ok {
		n := int(v)
		c.MaxLength = &n
	}
	if v, ok := target["minLength"].(float64); ok {
		n := int(v)
		c.MinLength = &n
	}
	if v, ok := target["minimum"].(float64); ok {
		c.Minimum = &v
	}
	if v, ok := target["maximum"].(float64); ok {
		c.Maximum = &v
	}
	if v, ok := target["minItems"].(float64); ok {
		n := int(v)
		c.MinItems = &n
	}
	if v, ok := target["maxItems"].(float64); ok {
		n := int(v)
		c.MaxItems = &n
	}
	if s, ok := target["pattern"].(string); ok {
		c.Pattern = &s
	}
	if b, ok := target["additionalProperties"].(bool); ok {
		c.AdditionalProperties = &b
	}
	c.ExclusiveMinimum = buildExclusiveBound(target["exclusiveMinimum"])
	c.ExclusiveMaximum = buildExclusiveBound(target["exclusiveMaximum"])
	return c
}

// buildExclusiveBound carries whichever dialect form the source document
// used: a draft-04 boolean modifier or a draft-06+ standalone number. The
// JSON value's own Go type after unmarshaling into `any` — bool vs
// float64 — is exactly the signal that tells the two forms apart.
func buildExclusiveBound(raw any) *ExclusiveBound {
	switch v := raw.(type) {
	case bool:
		b := v
		return &ExclusiveBound{Bool: &b}
	case float64:
		n := v
		return &ExclusiveBound{Number: &n}
	default:
		return nil
	}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func stringSet(raw any) map[string]bool {
	set := map[string]bool{}
	arr, ok := raw.([]any)
	if !ok {
		return set
	}
	for _, v := range arr {
		if s, ok := v.(string); ok {
			set[s] = true
		}
	}
	return set
}
