package main

// SymbolTable is the syntax-derived inventory of one source tree's exported
// package-level declarations, produced by extract.go. It never depends on
// the tree type-checking or resolving its imports: everything in it comes
// from go/ast alone, which lets the same extractor run against a real,
// buildable module (the "old" side, today's hand-written ocpp2.0.1/) and an
// ad hoc scratch tree with no go.mod of its own (the "new" side, the
// generator's scratch output) without needing the second one to be a real
// module.
type SymbolTable struct {
	Structs map[string]StructSymbol `json:"structs"`
	Enums   map[string]EnumSymbol   `json:"enums"`
	Aliases map[string]AliasSymbol  `json:"aliases"`
	Funcs   map[string]FuncSymbol   `json:"funcs"`

	// RegisteredValidations maps a validate-tag token (the string literal
	// passed as RegisterValidation's first argument) to the name of the
	// function passed as its second argument, e.g. "genericStatus" ->
	// "isValidGenericStatus".
	RegisteredValidations map[string]string `json:"registeredValidations"`

	// StructValidations is the set of RegisterStructValidation calls found,
	// keyed by the target type's name (the composite literal in the call's
	// second argument), valued by the validating function's name.
	StructValidations map[string]string `json:"structValidations"`
}

func newSymbolTable() *SymbolTable {
	return &SymbolTable{
		Structs:               map[string]StructSymbol{},
		Enums:                 map[string]EnumSymbol{},
		Aliases:               map[string]AliasSymbol{},
		Funcs:                 map[string]FuncSymbol{},
		RegisteredValidations: map[string]string{},
		StructValidations:     map[string]string{},
	}
}

// StructSymbol is one exported struct type: its declared fields, in source
// order, each carrying the JSON and validate tag content that field diffing
// and rename pairing key off.
type StructSymbol struct {
	Name    string        `json:"name"`
	Package string        `json:"package"`
	File    string        `json:"file"`
	Fields  []FieldSymbol `json:"fields"`
}

// FieldSymbol is one struct field. JSONTag is the bare value of the field's
// `json:"..."` tag (before any comma option, e.g. "chargingPriority" not
// "chargingPriority,omitempty") -- it is the join key rename pairing uses,
// since a schema-derived JSON key never changes across a Go identifier
// rename or a naming-transform pass.
type FieldSymbol struct {
	GoName      string `json:"goName"`
	GoType      string `json:"goType"`
	JSONTag     string `json:"jsonTag"`
	JSONOptions string `json:"jsonOptions,omitempty"`
	ValidateTag string `json:"validateTag"`
	Embedded    bool   `json:"embedded"`
}

// EnumSymbol is one exported named-string enum type: its declared consts,
// in source order, each carrying the literal wire VALUE (not the
// identifier) that rename pairing keys off -- the naming transform renames
// Go identifiers but never the OCPP wire strings they hold.
type EnumSymbol struct {
	Name       string   `json:"name"`
	Package    string   `json:"package"`
	File       string   `json:"file"`
	Underlying string   `json:"underlying"`
	ConstNames []string `json:"constNames"`
	Values     []string `json:"values"`
}

// AliasSymbol is a `type X = Y` declaration.
type AliasSymbol struct {
	Name    string `json:"name"`
	Package string `json:"package"`
	File    string `json:"file"`
	Target  string `json:"target"`
}

// FuncSymbol is one top-level exported function. ReturnsStructName is set
// when the function's sole non-error result is a (possibly pointer-to)
// named struct type -- the shape every constructor in this codebase has --
// and is what constructor-signature diffing pairs old and new functions by,
// rather than trusting the function's own name to survive a type rename.
type FuncSymbol struct {
	Name              string   `json:"name"`
	Package           string   `json:"package"`
	File              string   `json:"file"`
	Params            []string `json:"params"`
	Results           []string `json:"results"`
	ReturnsStructName string   `json:"returnsStructName,omitempty"`
}
