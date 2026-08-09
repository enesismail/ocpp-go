package main

import "fmt"

type StructInfo struct {
	Name    string
	Fields  []GoField
	Methods []string
}

// MethodSetInfo describes a type discovered by the GetFeatureName/GetRequestType/
// GetResponseType method-set rule. FeatureName is the resolved *value* of the
// constant GetFeatureName returns — never derived from TypeName or the file
// name, because real Feature types disagree with both (ClearDisplayFeature
// returns "ClearDisplayMessage"; Get15118EVCertificateFeature contains
// digits no [A-Za-z]* pattern would survive).
type MethodSetInfo struct {
	TypeName     string
	Methods      []string
	FeatureName  string
	RequestType  string
	ResponseType string
}

type WireType struct {
	Type   string
	Format string
}

type WireTypeRegistry struct {
	Types map[string]WireType
}

func newWireTypeRegistry() *WireTypeRegistry {
	return &WireTypeRegistry{Types: make(map[string]WireType)}
}

func (r *WireTypeRegistry) Register(goType string, wireType WireType) {
	if r.Types == nil {
		r.Types = make(map[string]WireType)
	}
	r.Types[goType] = wireType
}

// MarshalerInfo names a type discovered defining a MarshalJSON/UnmarshalJSON
// pair, and where it was found — used both to report the set of known
// custom marshalers and to build the hard-error message naming the file and
// line of an unregistered one.
type MarshalerInfo struct {
	Type string
	File string
	Line int
}

// ImportBinding records that, within a single Go file, the local identifier
// Alias (the selector prefix used in that file, e.g. "types") is bound to
// the import path Path. Go import bindings are per-file, not per-package, so
// the same Alias can be bound to different Paths in different files of the
// same tree (the real case: ocpp1.6/logging/get_log.go binds "types" to
// ocpp1.6/types while ocpp1.6/logging/log_status_notification.go binds the
// same local name "types" to ocpp2.0.1/types).
type ImportBinding struct {
	Alias string
	Path  string
}

// TypeResolver resolves a "pkg.Type" selector to a fully-qualified type
// identity, per file. It cannot be a single global selector->target map:
// the same selector text resolves to different targets in different files,
// because it is the *file's own* import bindings that give the selector
// meaning, not the selector text alone — a selector the tool cannot resolve
// is a hard failure naming the file, line and selector, never a silent
// guess. (See also the wire-type registry above, which is a distinct
// mechanism keyed by Go type, not by selector.)
//
// TypeResolver also tracks a second, distinct per-file fact: which package a
// file itself belongs to (RegisterFile/PackageOf, below). That is needed to
// build the wire-type registry's qualified key (import path + "." + declared
// type name) for a type declared *in* the file being scanned, as opposed to
// RegisterImport's bindings, which resolve a selector referring to a type
// declared in some *other* file's package.
type TypeResolver struct {
	imports  map[string][]ImportBinding
	packages map[string]string
}

func newTypeResolver() *TypeResolver {
	return &TypeResolver{
		imports:  make(map[string][]ImportBinding),
		packages: make(map[string]string),
	}
}

// RegisterImport records an import binding discovered in file's AST.
func (r *TypeResolver) RegisterImport(file string, binding ImportBinding) {
	if r.imports == nil {
		r.imports = make(map[string][]ImportBinding)
	}
	r.imports[file] = append(r.imports[file], binding)
}

// RegisterFile records that file itself belongs to the package at
// importPath — the file's own package identity, as opposed to a package it
// imports. This is what lets a type *declared in* file be keyed into the
// wire-type registry under its qualified identity (importPath + "." + the
// declared type name), the same qualified form RegisterImport's bindings
// produce for a selector referring to a type declared elsewhere.
func (r *TypeResolver) RegisterFile(file, importPath string) {
	if r.packages == nil {
		r.packages = make(map[string]string)
	}
	r.packages[file] = importPath
}

// PackageOf reports the import path of the package file belongs to, as
// previously recorded by RegisterFile. A file whose package identity was
// never registered is a hard error naming the file — unresolvable package
// identity must never be guessed, since guessing it would silently mis-key
// the qualified registry lookup it feeds.
func (r *TypeResolver) PackageOf(file string) (string, error) {
	importPath, ok := r.packages[file]
	if !ok {
		return "", fmt.Errorf("unregistered file: no package registered for %s", file)
	}
	return importPath, nil
}

// Resolution carries the two pieces of type-resolution context field
// walking needs together: the per-file import-binding resolver (a
// package-qualified field type must resolve through the declaring file's
// own import bindings, never a bare-name guess across files) and the
// wire-type registry (custom-marshaled types keyed by their resolved,
// package-qualified identity — two types that share a bare name but come
// from different packages, the real case being a DateTime type declared
// once per OCPP version, must never collapse onto a single registry key).
// Field walking is unusable without both reachable together, which is why
// they travel as one value instead of the resolver sitting unused beside a
// walker that only ever sees the registry.
// Enums is the tree-wide validator-tag registry: a validator tag token (e.g.
// "bootReason") mapped to the set of values the validator registered under
// it accepts. A file's own RegisterValidation calls are discovered while it
// is walked and always take precedence for that file; this registry is how
// tags registered in some *other* file reach a field that carries them —
// the real case being the shared types package, which registers many tags
// the per-message files then use.
//
// ScalarAliases is the tree-wide scalar-alias registry: a named type's
// resolved, package-qualified identity (import path + "." + type name, the
// same keying the wire-type registry uses) mapped to the Go basic type it is
// declared as. A file walk reads its own declarations directly and needs no
// registry for them; this one is how a named type declared in *another* file
// — a sibling file of the same package, or another package entirely — is
// known to put a scalar on the wire rather than an object. Keying by resolved
// identity rather than bare name is what stops two same-named types from
// different packages collapsing onto one entry, exactly as for wire types.
type Resolution struct {
	Resolver      *TypeResolver
	Registry      *WireTypeRegistry
	Enums         map[string][]string
	ScalarAliases map[string]string
}

func newResolution(resolver *TypeResolver, registry *WireTypeRegistry) *Resolution {
	return &Resolution{
		Resolver:      resolver,
		Registry:      registry,
		Enums:         map[string][]string{},
		ScalarAliases: map[string]string{},
	}
}

// RegisterScalarAlias records that the type at identity — its resolved,
// package-qualified form — is declared as the Go basic type underlying, so a
// field of that type elsewhere in the tree carries that scalar on the wire.
func (r *Resolution) RegisterScalarAlias(identity, underlying string) {
	if r.ScalarAliases == nil {
		r.ScalarAliases = make(map[string]string)
	}
	r.ScalarAliases[identity] = underlying
}

// scalarAliasFor reports the Go basic type registered for identity, tolerating
// a Resolution built without a registry. A type nobody registered is not
// guessed at: it keeps the opaque treatment a named type has always had.
func (r *Resolution) scalarAliasFor(identity string) (string, bool) {
	if r == nil {
		return "", false
	}
	underlying, ok := r.ScalarAliases[identity]
	return underlying, ok
}

// RegisterEnum records the accepted value set of the validator registered
// under tag, for tags discovered outside the file currently being walked.
func (r *Resolution) RegisterEnum(tag string, values []string) {
	if r.Enums == nil {
		r.Enums = make(map[string][]string)
	}
	r.Enums[tag] = values
}

// enumRegistry returns the tree-wide validator-tag registry, tolerating a
// Resolution built without one.
func (r *Resolution) enumRegistry() map[string][]string {
	if r == nil {
		return nil
	}
	return r.Enums
}

type SchemaShape struct {
	Schema            string
	IdentifierKeyword string
	HasDefinitions    bool
	HasReferences     bool
	// FilenameShape classifies this file's own filename morphology — a
	// per-file fact: "response" for a name ending "Response.json",
	// "request-suffixed" for a name ending "Request.json" (the 2.0.1
	// convention), or "request-bare" for a name with neither suffix (the 1.6
	// convention, e.g. "BootNotification.json"). Which convention an entire
	// schema *set* uses is a separate, per-set computation — see
	// requestFilenameConvention — never sniffed from a single file's $id.
	FilenameShape string
}

type SchemaDocument struct {
	File  string
	Shape SchemaShape
	// RootType is the "type" keyword the document declares at its own root
	// (in practice "object" throughout this corpus), read rather than
	// assumed. It is the schema-side counterpart of the message struct as a
	// whole, which is what a rule registered against that struct — as
	// opposed to against one of its fields — is compared against.
	RootType   string
	Properties []SchemaProperty
	// IgnoredKeywords records every occurrence of a vendor/documentation-only
	// keyword (javaType, comment, description) this walk saw and
	// deliberately did not treat as a constraint — ignored on purpose, not
	// by accident. Each entry is "<pointer>:<keyword>" (the document root
	// uses pointer "#"), so a keyword seen on more than one property is
	// recorded once per occurrence rather than collapsed into one
	// document-wide flag.
	IgnoredKeywords []string
}

type SchemaProperty struct {
	// Pointer is the JSON pointer to where this property occurs — i.e. where
	// it is textually declared in the schema document. For a property whose
	// value is a bare $ref, that is the pointer to the referencing property
	// itself (e.g. "#/properties/status"), never the $ref target's own
	// pointer; recursing into a $ref'd object definition's own properties
	// (e.g. "#/definitions/ModemType/properties/iccid") yields pointers
	// rooted at that definition, because that is where those properties are
	// themselves textually declared.
	Pointer string
	// Path is the dotted logical path this occurrence was reached by, from
	// the document root (e.g. "chargingStation.modem.iccid") — the same
	// grammar the Go side's field paths use, and what pairs the two sides
	// within a path rather than by bare name.
	//
	// It is a different fact from Pointer and cannot be derived from it. A
	// definition reached through several different properties yields the same
	// Pointer every time, because Pointer says where a property is textually
	// declared, and a shared definition is declared once however often it is
	// referenced. Path says how this particular occurrence was reached, so
	// three references to the same definition are three distinguishable
	// occurrences. Both are kept: Pointer is the identity a per-file,
	// per-declaration count is taken over; Path is the identity a per-reach
	// one is.
	Path string
	Name string
	Type string
	// Format carries the schema "format" keyword (e.g. "date-time"), needed
	// by the *types.DateTime wire-type rule.
	Format   *string
	Required bool
	Enum     []string
	// additionalProperties has exactly one home: Constraints.AdditionalProperties
	// (contract.go) — the shape shared with the rest of this package's
	// constraint reporting. It is not duplicated as a field of its own here.
	Constraints Constraints
	// EnclosingDefinition is the name of the "definitions" entry this
	// property was reached through (e.g. "ModemType"), or "" for a property
	// declared directly under the document's own root "properties". It is
	// the source for the (definition name, property name) dedup identity
	// used by the corpus-wide dedup pass.
	EnclosingDefinition string
}

// ComparisonInput carries only the raw, independently-observed facts about
// one field: what the Go side declares and what the schema side requires.
// It deliberately has no pre-computed answer field (no GlobalRule, no
// class-discriminator flag) — classifyField must derive the class itself by
// applying the mapping rules to these facts, or the classifier tests would
// only be testing a label pass-through. The one AST-discovered fact that
// legitimately feeds STRUCT_VALIDATOR classification is Go.StructValidators
// (populated by struct-validator discovery), which is a fact about what the
// tree contains, not a pre-computed classification.
type ComparisonInput struct {
	Go            GoField     `json:"go"`
	Schema        SchemaField `json:"schema"`
	GoPresent     bool        `json:"goPresent"`
	SchemaPresent bool        `json:"schemaPresent"`
	Detail        string      `json:"detail"`
}

type ClassificationResult struct {
	Class    string
	Rule     string
	Detail   string
	Breaking bool
}

type FieldSignature struct {
	Present              bool
	Name                 string
	DeclaredType         string
	ValidateTag          string
	EnumType             string
	ConstructorSignature string
}
