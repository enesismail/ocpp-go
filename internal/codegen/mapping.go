package main

import (
	"fmt"
	"strconv"
	"strings"
)

// findDefinitionByName returns the definition named name, or nil when no
// such definition is present. Every $ref resolution in this package goes
// through this one lookup so a missing target is reported the same way
// everywhere.
func findDefinitionByName(definitions []Definition, name string) *Definition {
	for i := range definitions {
		if definitions[i].Name == name {
			return &definitions[i]
		}
	}
	return nil
}

// refTargetName strips the "#/definitions/" prefix from a $ref string,
// returning "" when ref is empty.
func refTargetName(ref string) string {
	if ref == "" {
		return ""
	}
	return strings.TrimPrefix(ref, "#/definitions/")
}

// formatBoundNumber renders a schema numeric bound the way a gte=/lte=
// token needs it: the shortest decimal representation that round-trips
// (1 -> "1", 1.5 -> "1.5", 0 -> "0" — never a trailing ".0").
func formatBoundNumber(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// BaseTagSet returns the tags derived solely from a property and its schema
// constraints. It is the stable lookup used by configuration application:
// every override-governance check that needs to know a tighten row's "from"
// token is really present, or that an add row's validator family isn't
// already covered, resolves a (definition, property) through this same
// function, so those checks are validated against exactly what the mapping
// produced, not a second, independently maintained list.
//
// Numeric bound tokens compose in a fixed, canonical order, matching the
// existing hand-written tree rather than schema or declaration order:
// length/cardinality bounds render min= before max= (types.go:412's
// "min=1,max=1024", never the reverse), and value bounds render gte=
// before lte= (smartcharging/notify_ev_charging_needs.go:66's
// "gte=0,lte=100"). Every golden fixture in testdata/golden follows this
// ordering; a rendering that emits max before min, or lte before gte, is
// wrong even when the token set is otherwise correct.
//
// An array's set runs slice tokens first, then dive, then the tokens its
// ELEMENT schema derives — see arrayElementTokens for why the boundary
// matters and when dive is emitted at all.
//
// The set returned here never includes the enum validator token (that
// composition needs the naming transform and the full definition index,
// which this function deliberately does not take — see MapProperty) and
// never includes an override's contribution: an override composes after
// this base set, through ApplyTagOverride.
func BaseTagSet(definition Definition, property Property) ([]string, error) {
	var tokens []string
	switch {
	case property.Required && property.Type == "boolean":
		// A required bool renders no validate tag at all: go-playground's
		// validator cannot distinguish an explicit false from an unset
		// zero value on a plain bool, so "required" would reject a
		// legitimately false value.
	case property.Required:
		tokens = append(tokens, "required")
	default:
		tokens = append(tokens, "omitempty")
	}

	if property.Type == "array" {
		if property.MinItems != nil {
			tokens = append(tokens, fmt.Sprintf("min=%d", *property.MinItems))
		}
		if property.MaxItems != nil {
			tokens = append(tokens, fmt.Sprintf("max=%d", *property.MaxItems))
		}
		tokens = append(tokens, arrayElementTokens(property)...)
		return tokens, nil
	}

	tokens = append(tokens, scalarBoundTokens(property)...)
	return tokens, nil
}

// scalarBoundTokens renders the validator tokens one schema's own bounds
// produce: length bounds (minLength/maxLength) as min=/max=, value bounds
// (minimum/maximum) as gte=/lte=, in the canonical order BaseTagSet's own
// documentation fixes. Which pair a schema declares is what makes the
// mapping kind-sensitive — a string carries lengths, a number carries
// values — so no branch on the declared type is needed or wanted here: a
// schema that declared both would legitimately render both.
//
// pattern is deliberately absent. It maps to no validator token at all
// (validator.v9 has no pattern builtin, and an unregistered tag panics at
// validation time rather than failing to compile), so a pattern stays
// enforced by the schema-validation layer, never by a generated tag.
//
// This is the one derivation used for a property's own bounds and for an
// array element's alike: an element's bounds describe a value exactly the
// way a scalar property's describe one, so deriving them twice, from two
// pieces of code, could only ever produce two answers to one question.
func scalarBoundTokens(property Property) []string {
	var tokens []string
	if property.MinLength != nil {
		tokens = append(tokens, fmt.Sprintf("min=%d", *property.MinLength))
	}
	if property.MaxLength != nil {
		tokens = append(tokens, fmt.Sprintf("max=%d", *property.MaxLength))
	}
	if property.Minimum != nil {
		tokens = append(tokens, "gte="+formatBoundNumber(*property.Minimum))
	}
	if property.Maximum != nil {
		tokens = append(tokens, "lte="+formatBoundNumber(*property.Maximum))
	}
	return tokens
}

// arrayElementTokens renders the trailing part of an array's tag: dive,
// followed by the tokens that describe one element.
//
// dive is the boundary between the two halves of a slice's tag. Every token
// in front of it is applied to the slice (min=/max= there are its
// cardinality), and every token after it is applied to each value. An
// element bound rendered without dive therefore does not tighten what the
// schema tightened — it silently says something else entirely: maxLength 512
// on the elements of an array becomes a cap of 512 elements, and a 513-
// character value validates cleanly.
//
// So dive is emitted whenever the element has anything of its own to say:
// a $ref'd element type (a composite the validator must descend into, or an
// enum whose registered check MapProperty appends after this set), or an
// element schema declaring bounds. An array whose elements say nothing —
// plain strings with no bounds — emits no dive, because there is nothing
// for the validator to do once it has descended.
func arrayElementTokens(property Property) []string {
	if property.Items == nil {
		return nil
	}
	bounds := scalarBoundTokens(*property.Items)
	if property.Items.Ref == "" && property.Items.EnumDefinition == "" && len(bounds) == 0 {
		return nil
	}
	return append([]string{"dive"}, bounds...)
}

// ApplyTagOverride composes one override row onto a base tag set. "add"
// appends row.Tag; "tighten" substitutes row.Tag in place of the base
// token named by row.From, never appending it as an extra token alongside
// the token it replaces. An override row can never name "required" or
// "omitempty" as its own tag: those are optionality's tags, decided by
// the mapping rules from the schema itself, not validation-strictness
// tags an override may add or tighten.
func ApplyTagOverride(base []string, row OverrideRow) ([]string, error) {
	if row.Tag == "required" || row.Tag == "omitempty" {
		return nil, fmt.Errorf("apply tag override %s.%s: rule %q may not name %q, an optionality tag — required/omitempty are decided by the mapping rules, never by an override",
			row.Definition, row.Property, row.Rule, row.Tag)
	}

	result := append([]string(nil), base...)
	switch row.Rule {
	case "add":
		result = append(result, row.Tag)
	case "tighten":
		index := -1
		for i, token := range result {
			if token == row.From {
				index = i
				break
			}
		}
		if index < 0 {
			return nil, fmt.Errorf("apply tag override %s.%s: tighten names from token %q, which is not present in the base tag set %v",
				row.Definition, row.Property, row.From, base)
		}
		result[index] = row.Tag
	default:
		return nil, fmt.Errorf("apply tag override %s.%s: unknown override rule %q", row.Definition, row.Property, row.Rule)
	}
	return result, nil
}

// validatorTagName derives the registered validator tag name for an enum
// definition, keyed on its raw schema name: transform to the enum's Go
// type name, lower its leading word run, and append the literal "201"
// version suffix that keeps the tag from colliding with 1.6's own
// process-global validator registry.
func validatorTagName(enumDefinitionName string, transform TransformConfig) (string, error) {
	goName, err := TransformIdentifier(enumDefinitionName, DefinitionEnum, transform)
	if err != nil {
		return "", err
	}
	return lowerCamel(goName) + "201", nil
}

// buildJSONTag renders a property's complete json:"..." fragment.
func buildJSONTag(property Property) string {
	if property.Required {
		return fmt.Sprintf("json:%q", property.Name)
	}
	return fmt.Sprintf("json:%q", property.Name+",omitempty")
}

// mapGoType computes the placement-agnostic Go type spelling for one
// property, implementing the schema-to-Go mapping table's rows except the
// enum-ref/composite-ref cases' own field naming (handled by the caller,
// MapProperty, alongside the tag composition). DateTime and CustomData are
// rendered in their block-package spelling ("*types.DateTime",
// "*types.CustomData") unconditionally: neither is ever subject to
// reach-count placement, since neither is generated at all — both are
// hand-written residents of the types package — so this is the one
// context-free answer a placement-agnostic function can give; a caller
// rendering inside types/ itself strips the "types." prefix afterward
// (RenderProperty, RenderMessage's own field assembly).
func mapGoType(property Property, definitions []Definition, transform TransformConfig) (string, error) {
	switch {
	case property.Untyped:
		return "any", nil
	case property.Format == "date-time":
		return "*types.DateTime", nil
	case property.Ref != "":
		return mapRefType(property, definitions, transform)
	case property.Type == "array":
		return mapArrayType(property, definitions, transform)
	case property.Type == "integer":
		return mapNumericType("int", property), nil
	case property.Type == "number":
		return mapNumericType("float64", property), nil
	case property.Type == "boolean":
		if property.Required {
			return "bool", nil
		}
		return "*bool", nil
	case property.Type == "string":
		return "string", nil
	default:
		return "", fmt.Errorf("map property %q: unsupported schema type %q", property.Name, property.Type)
	}
}

// mapNumericType implements the narrowed optionality rule for integer and
// number properties: a required field is always a value; an optional field
// is a pointer iff the schema's own minimum admits zero (no minimum at
// all, or minimum <= 0) and a value (with omitempty) otherwise.
func mapNumericType(base string, property Property) string {
	if property.Required {
		return base
	}
	if property.Minimum == nil || *property.Minimum <= 0 {
		return "*" + base
	}
	return base
}

// mapRefType resolves a direct (non-array) $ref: an enum ref renders as its
// own Go type name (always a value, never a pointer, the same rule an
// optional string enum follows); a reserved ref renders the hand-written
// CustomData type unconditionally, in its block-package spelling; an
// ordinary object ref renders as a pointer when optional and a value when
// required, always unqualified (RenderMessage's own placement-aware step
// adds a "types." qualifier when the target is placed in types/).
func mapRefType(property Property, definitions []Definition, transform TransformConfig) (string, error) {
	if property.EnumDefinition != "" {
		return TransformIdentifier(property.EnumDefinition, DefinitionEnum, transform)
	}

	name := refTargetName(property.Ref)
	target := findDefinitionByName(definitions, name)
	if target == nil {
		return "", fmt.Errorf("map property: $ref %q does not resolve to a known definition", property.Ref)
	}
	if target.Reserved {
		return "*types.CustomData", nil
	}

	goName, err := TransformIdentifier(target.Name, DefinitionObject, transform)
	if err != nil {
		return "", err
	}
	if property.Required {
		return goName, nil
	}
	return "*" + goName, nil
}

// mapArrayType renders an array property as "[]<element>", never a
// pointer to the slice itself.
func mapArrayType(property Property, definitions []Definition, transform TransformConfig) (string, error) {
	if property.Items == nil {
		return "", fmt.Errorf("map property %q: array has no items schema", property.Name)
	}
	elem, err := mapArrayElementType(*property.Items, definitions, transform)
	if err != nil {
		return "", err
	}
	return "[]" + elem, nil
}

// mapArrayElementType renders an array's element type. Elements are always
// values, never pointers, whichever kind of element schema they name.
func mapArrayElementType(item Property, definitions []Definition, transform TransformConfig) (string, error) {
	switch {
	case item.EnumDefinition != "":
		return TransformIdentifier(item.EnumDefinition, DefinitionEnum, transform)
	case item.Ref != "":
		name := refTargetName(item.Ref)
		target := findDefinitionByName(definitions, name)
		if target == nil {
			return "", fmt.Errorf("map property: array element $ref %q does not resolve to a known definition", item.Ref)
		}
		if target.Reserved {
			return "types.CustomData", nil
		}
		return TransformIdentifier(target.Name, DefinitionObject, transform)
	case item.Format == "date-time":
		return "*types.DateTime", nil
	case item.Type == "integer":
		return "int", nil
	case item.Type == "number":
		return "float64", nil
	case item.Type == "boolean":
		return "bool", nil
	case item.Type == "string":
		return "string", nil
	default:
		return "", fmt.Errorf("map property: unsupported array element type %q", item.Type)
	}
}

// MapProperty computes the schema-derived Go kind, field name, JSON tag and
// base validate tag for one property, and nothing about where that
// declaration will live.
//
// This is a deliberate design choice, not an oversight: MapProperty is the
// seam every override-governance check evaluates against, and every one of
// those checks asks "does this override's tag fit the Go kind and base tag
// set this property already maps to" — a question with the same answer no
// matter which package the generated declaration ends up in. Folding a
// PackageContext into this function's signature would make that seam
// placement-dependent for no reason, and would force override-governance
// fixtures to fabricate a package they don't care about. MapProperty
// therefore takes the flat list of IR definitions (to resolve what a $ref
// names and which kind it is) and the transform config (to name the field
// itself, including any initialism), but no PackageContext and no
// PlacementPlan.
//
// RenderProperty and RenderDefinition are the two callers that turn a
// FieldMapping into literal source text: they compose MapProperty's result
// with their own PackageContext to decide whether a composite, DateTime,
// CustomData or Validate reference is written qualified (types.X) or bare
// (X) — see PackageContext's doc comment.
func MapProperty(definition Definition, property Property, definitions []Definition, transform TransformConfig, overrides []OverrideRow) (FieldMapping, error) {
	fieldName, err := transformPropertyName(property.Name, transform)
	if err != nil {
		return FieldMapping{}, fmt.Errorf("map property %s.%s: %w", definition.Name, property.Name, err)
	}

	goType, err := mapGoType(property, definitions, transform)
	if err != nil {
		return FieldMapping{}, fmt.Errorf("map property %s.%s: %w", definition.Name, property.Name, err)
	}

	tokens, err := BaseTagSet(definition, property)
	if err != nil {
		return FieldMapping{}, fmt.Errorf("map property %s.%s: %w", definition.Name, property.Name, err)
	}
	// An enum-typed property carries the validator token its enum registers,
	// and so does an array whose ELEMENTS are that enum: the base tag set
	// has already appended dive for a $ref'd element type, and every token
	// after dive is applied to each element rather than to the slice, so the
	// same derivation lands the enum check exactly where the values are. An
	// array of enum values that carried only dive would accept any string
	// the schema's enum does not list.
	enumDefinition := property.EnumDefinition
	if enumDefinition == "" && property.Type == "array" && property.Items != nil {
		enumDefinition = property.Items.EnumDefinition
	}
	if enumDefinition != "" {
		tagName, err := validatorTagName(enumDefinition, transform)
		if err != nil {
			return FieldMapping{}, fmt.Errorf("map property %s.%s: %w", definition.Name, property.Name, err)
		}
		tokens = append(tokens, tagName)
	}

	for _, row := range overrides {
		if row.Definition != definition.Name || row.Property != property.Name {
			continue
		}
		tokens, err = ApplyTagOverride(tokens, row)
		if err != nil {
			return FieldMapping{}, err
		}
	}

	validateTag := ""
	if len(tokens) > 0 {
		validateTag = `validate:"` + strings.Join(tokens, ",") + `"`
	}

	return FieldMapping{
		FieldName:   fieldName,
		GoType:      goType,
		JSONTag:     buildJSONTag(property),
		ValidateTag: validateTag,
	}, nil
}
