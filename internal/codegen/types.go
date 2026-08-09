package main

// TransformConfig contains the configured words that are rendered as
// initialisms in exported Go identifiers.
type TransformConfig struct {
	Initialisms []string `yaml:"initialisms"`
}

// PackageContext describes the package in which a declaration will be
// rendered. References to shared support types depend on this context.
//
// The zero value (PackageContext{}) is not an unconfigured placeholder: it
// is itself a meaningful, deliberate context. InTypes false is the default,
// so a zero-value PackageContext reads as "a block-package file" — the
// common case, since most emitted files live under a message's own block
// package, not under types/ itself. In that default block-package reading,
// a qualified reference is spelled with the literal package identifier
// "types" (TypesPackage's own import path only matters for the generated
// import statement, never for how the identifier is written in source). A
// caller that wants the types/-internal, unqualified spelling must set
// InTypes true explicitly; there is no zero value that means that.
type PackageContext struct {
	PackageName  string
	TypesPackage string
	InTypes      bool
}

// PlacementPlan records the package home of each non-root, non-reserved
// definition. The values are either "types" or "message:<name>".
type PlacementPlan struct {
	Home map[string]string
}

// MappingContext bundles the IR index, package context and naming
// transform for RenderProperty, the one function that takes it as a single
// argument. RenderDefinition needs the same three inputs but takes them as
// separate parameters (ir IR, context PackageContext, transform
// TransformConfig) instead of this bundle — it has no property-level
// caller that would benefit from having them pre-packaged together the way
// RenderProperty's own callers do. Transform, wherever it arrives, is what
// lets both functions reach the configured initialism table
// (config/transform.yaml): naming a field or a $ref target without it
// would silently fall back to plain title-casing, which is wrong for any
// name the table recognizes (evseId must render EVSEID, never EvseId).
type MappingContext struct {
	Definitions []Definition
	Package     PackageContext
	Transform   TransformConfig
}

// FieldMapping is the immutable result of mapping one schema property. It
// is deliberately placement-agnostic: GoType, JSONTag and ValidateTag are
// the same no matter which file the property's declaring definition ends
// up rendered into, because MapProperty (the function that produces a
// FieldMapping) never receives a PackageContext or a PlacementPlan — see
// MapProperty's own doc comment. JSONTag and ValidateTag hold the complete
// key:"value" fragment for each tag (for example `json:"name"` and
// `validate:"required"`), ready to be joined into one backtick-delimited
// struct tag; ValidateTag is the empty string for a property that renders
// no validate tag at all (a required boolean renders no validate tag,
// because go-playground/validator can't distinguish an explicit false from
// an unset zero value on a plain bool).
type FieldMapping struct {
	FieldName   string
	GoType      string
	JSONTag     string
	ValidateTag string
}

// OverrideRow is the small application hook shared with configuration
// loading. It can contribute validator tokens after the base mapping.
type OverrideRow struct {
	Version    string `yaml:"-"`
	Definition string `yaml:"definition"`
	Property   string `yaml:"property"`
	Rule       string `yaml:"rule"`
	Tag        string `yaml:"tag"`
	From       string `yaml:"from,omitempty"`
	Rationale  string `yaml:"rationale"`
	Source     string `yaml:"source"`
	Author     string `yaml:"author"`
	Date       string `yaml:"date"`
}

// EmitterOptions carries transform data and already-decoded tag rows.
type EmitterOptions struct {
	Transform TransformConfig
	Overrides []OverrideRow
}
