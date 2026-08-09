package main

type Report struct {
	Version   string `json:"version"`
	GoTree    string `json:"goTree"`
	SchemaDir string `json:"schemaDir"`
	// Invocation is the command that reproduces this report's comparison,
	// spelled with the flags that decide what it says and without the ones
	// that only decide where it is written — so two runs of the same
	// comparison into different directories record the same invocation.
	Invocation string `json:"invocation"`
	// SchemaCorpus identifies the exact schema files the comparison read, so
	// a reader can establish that a re-run used the same inputs rather than a
	// later revision of the same publication.
	SchemaCorpus []SchemaCorpusDir `json:"schemaCorpus"`
	// Classifications names the registry of hand-made judgements merged onto
	// the rows, when one was. It is recorded apart from Invocation because it
	// annotates the comparison rather than performing it: the field-level
	// findings reproduce from Invocation alone.
	Classifications string       `json:"classifications,omitempty"`
	Coverage        Coverage     `json:"coverage"`
	Summary         Summary      `json:"summary"`
	SelfCheck       []SelfCheck  `json:"selfCheck"`
	Messages        []Message    `json:"messages"`
	SharedTypes     []SharedType `json:"sharedTypes"`
}

// SchemaCorpusDir is one schema directory's identity: how many documents it
// contributed and a digest over their contents. The digest is taken over the
// per-file SHA-256s paired with their names, sorted, so it changes if any file
// changes, is added, is removed or is renamed, and does not depend on the order
// the directory happened to be read in.
type SchemaCorpusDir struct {
	Dir    string `json:"dir"`
	Files  int    `json:"files"`
	SHA256 string `json:"sha256"`
}

type Coverage struct {
	Messages            int      `json:"messages"`
	SchemaFiles         int      `json:"schemaFiles"`
	UnpairedMessages    []string `json:"unpairedMessages"`
	UnpairedSchemaFiles []string `json:"unpairedSchemaFiles"`
}

type Summary struct {
	ByClass                    ClassCounts            `json:"byClass"`
	BreakingFields             int                    `json:"breakingFields"`
	MessagesWithBreakingFields int                    `json:"messagesWithBreakingFields"`
	MessagesNeedingOverrides   int                    `json:"messagesNeedingOverrides"`
	OverrideDensity            float64                `json:"overrideDensity"`
	ComplexityDistribution     ComplexityDistribution `json:"complexityDistribution"`
	PrototypeCount             int                    `json:"prototypeCount"`
}

type ClassCounts struct {
	IDENTICAL              int `json:"IDENTICAL"`
	SCHEMA_FAITHFUL_CHANGE int `json:"SCHEMA_FAITHFUL_CHANGE"`
	FORK_BUG               int `json:"FORK_BUG"`
	ADDITIVE               int `json:"ADDITIVE"`
	OVERRIDE_CANDIDATE     int `json:"OVERRIDE_CANDIDATE"`
	STRUCT_VALIDATOR       int `json:"STRUCT_VALIDATOR"`
	UNEXPLAINED            int `json:"UNEXPLAINED"`
}

type ComplexityDistribution struct {
	Min    int `json:"min"`
	P25    int `json:"p25"`
	Median int `json:"median"`
	P75    int `json:"p75"`
	Max    int `json:"max"`
}

type SelfCheck struct {
	ID       string `json:"id"`
	Claim    string `json:"claim"`
	Expected int    `json:"expected"`
	Actual   int    `json:"actual"`
	Status   string `json:"status"`
}

type Message struct {
	FeatureName string      `json:"featureName"`
	Profile     string      `json:"profile"`
	Direction   string      `json:"direction"`
	GoPackage   string      `json:"goPackage"`
	GoFile      string      `json:"goFile"`
	Complexity  int         `json:"complexity"`
	Prototype   bool        `json:"prototype"`
	Request     MessageSide `json:"request"`
	Response    MessageSide `json:"response"`
}

type MessageSide struct {
	GoType     string  `json:"goType"`
	SchemaFile string  `json:"schemaFile"`
	Fields     []Field `json:"fields"`
}

type Field struct {
	Path     string      `json:"path"`
	Go       GoField     `json:"go"`
	Schema   SchemaField `json:"schema"`
	Class    string      `json:"class"`
	Rule     string      `json:"rule"`
	Detail   string      `json:"detail"`
	Breaking bool        `json:"breaking"`
	// Classification, Severity and ClassificationNote are not computed from
	// the two sides of the comparison: they say whether a difference is a
	// defect, where it came from, and how much it matters, which are read off
	// the schema, this project's own change log and the upstream release the
	// tree derives from. They are merged in from the registry file named by
	// -classifications (see classifications.go) and are absent from a run that
	// names none, so a report never implies a judgement nobody made.
	Classification     string `json:"classification,omitempty"`
	Severity           string `json:"severity,omitempty"`
	ClassificationNote string `json:"classificationNote,omitempty"`
}

type GoField struct {
	Path         string `json:"-"`
	Name         string `json:"name"`
	DeclaredType string `json:"declaredType"`
	WireType     string `json:"wireType"`
	Pointer      bool   `json:"pointer"`
	Slice        bool   `json:"slice"`
	// CustomMarshaled records that this field's wireType came from the
	// custom-marshaler registry rather than from its own declaration. It is
	// the one fact about a field's declared Go kind that cannot be recovered
	// from declaredType and wireType afterwards: a custom-marshaled struct
	// (declaredType "DateTime", wireType "string") and a named scalar alias
	// (declaredType "BootReason", wireType "string") are otherwise identical
	// in every recorded field, yet only the alias has an empty value
	// encoding/json can omit. Not part of the report's field record — it is
	// walker-internal, like Path.
	CustomMarshaled bool   `json:"-"`
	ElementType     string `json:"elementType,omitempty"`
	// File and Line locate the field's own declaration. They are recorded
	// because a row's declaring file is not always the message file it is
	// reported under — a field reached through a shared composite is declared
	// wherever that composite is — so a report that cited only the message
	// would send a reader to a file the field is not in.
	File             string   `json:"file,omitempty"`
	Line             int      `json:"line,omitempty"`
	JSONName         string   `json:"jsonName"`
	Omitempty        bool     `json:"omitempty"`
	Validate         []string `json:"validate"`
	EnumValues       []string `json:"enumValues"`
	StructValidators []string `json:"structValidators"`
}

type SchemaField struct {
	Pointer     string      `json:"pointer"`
	Type        *string     `json:"type"`
	Required    bool        `json:"required"`
	Format      *string     `json:"format"`
	Enum        []string    `json:"enum"`
	Constraints Constraints `json:"constraints"`
}

type Constraints struct {
	MaxLength *int     `json:"maxLength,omitempty"`
	MinLength *int     `json:"minLength,omitempty"`
	Minimum   *float64 `json:"minimum,omitempty"`
	Maximum   *float64 `json:"maximum,omitempty"`
	// ExclusiveMinimum/ExclusiveMaximum carry whichever dialect form the
	// source schema used: draft-04 models them as a boolean modifier paired
	// with Minimum/Maximum ("the minimum applies, exclusively"); draft-06+
	// redefines them as a standalone number that replaces Minimum/Maximum
	// outright. Exactly one of ExclusiveBound.Bool / .Number is set,
	// matching the form the source document used — never both.
	ExclusiveMinimum     *ExclusiveBound `json:"exclusiveMinimum,omitempty"`
	ExclusiveMaximum     *ExclusiveBound `json:"exclusiveMaximum,omitempty"`
	MinItems             *int            `json:"minItems,omitempty"`
	MaxItems             *int            `json:"maxItems,omitempty"`
	Pattern              *string         `json:"pattern,omitempty"`
	AdditionalProperties *bool           `json:"additionalProperties,omitempty"`
}

// ExclusiveBound is documented on Constraints.ExclusiveMinimum/ExclusiveMaximum
// above: it carries either the draft-04 boolean-flag form or the draft-06+
// standalone numeric form of an exclusive bound, never both at once.
type ExclusiveBound struct {
	Bool   *bool    `json:"bool,omitempty"`
	Number *float64 `json:"number,omitempty"`
}

type SharedType struct {
	GoType              string   `json:"goType"`
	SchemaDefinition    string   `json:"schemaDefinition"`
	Occurrences         int      `json:"occurrences"`
	StructuralConflicts []string `json:"structuralConflicts"`
}
