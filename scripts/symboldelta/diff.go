package main

import (
	"sort"
	"strconv"
	"strings"
)

// DeltaClass names one of nine kinds of use-site change this tool detects
// between an old and a new symbol table, plus two bookkeeping classes
// (added/removed with no pairing found) that are not swept but are reported
// so a genuine deletion is never silently absorbed into a false-positive
// rename.
type DeltaClass string

const (
	ClassRenamedType            DeltaClass = "renamed_type"
	ClassRenamedField           DeltaClass = "renamed_field"
	ClassChangedPointerness     DeltaClass = "changed_pointerness"
	ClassAddedField             DeltaClass = "added_field"
	ClassRemovedField           DeltaClass = "removed_field"
	ClassChangedConstructorSig  DeltaClass = "changed_constructor_signature"
	ClassRenamedValidatorTag    DeltaClass = "renamed_validator_tag"
	ClassRemovedStructValidator DeltaClass = "removed_struct_validator"
	ClassAddedType              DeltaClass = "added_type"
	ClassRemovedType            DeltaClass = "removed_type"
	ClassAmbiguousRename        DeltaClass = "ambiguous_rename"
)

// DeltaRow is one row of the symbol-delta inventory: one class, one
// old/new name pair (or just one side, for additions/removals), and enough
// detail to explain it without re-deriving it.
type DeltaRow struct {
	Class    DeltaClass `json:"class"`
	OldName  string     `json:"oldName,omitempty"`
	NewName  string     `json:"newName,omitempty"`
	OwnerOld string     `json:"ownerOld,omitempty"` // struct/enum this field or const belongs to, old name
	OwnerNew string     `json:"ownerNew,omitempty"` // same, new name
	Detail   string     `json:"detail,omitempty"`
}

// renamePairing is the result of matching old-only structs/enums against
// new-only ones by content rather than by name.
type renamePairing struct {
	structPairs map[string]string // old name -> new name
	enumPairs   map[string]string // old name -> new name
	// ambiguous lists every old name where content matching found more
	// than one equally-good new candidate -- these are deliberately left
	// unpaired rather than resolved by an arbitrary tiebreak.
	ambiguous []DeltaRow
	// ambiguousOld/NewNames record which names took part in a tie, so the
	// removed/added bookkeeping below doesn't double-report them: a tied
	// old name is "ambiguous", not "removed", and a tied new name is a
	// contested candidate, not a clean "added".
	ambiguousOldStructNames map[string]bool
	ambiguousOldEnumNames   map[string]bool
	ambiguousNewStructNames map[string]bool
	ambiguousNewEnumNames   map[string]bool
}

// computeDelta compares old against new and returns every delta row,
// sorted deterministically (class, then old name, then new name) so two
// runs over unchanged inputs produce byte-identical output.
func computeDelta(old, new_ *SymbolTable) []DeltaRow {
	pairing := pairRenames(old, new_)
	var rows []DeltaRow

	rows = append(rows, pairing.ambiguous...)
	rows = append(rows, typeRenameRows(old, new_, pairing)...)
	rows = append(rows, fieldRows(old, new_, pairing)...)
	rows = append(rows, constructorRows(old, new_, pairing)...)
	rows = append(rows, validatorTagRows(old, new_, pairing)...)
	rows = append(rows, structValidatorRows(old, new_, pairing)...)

	// The full key (class, both owners, both names, detail) is compared, not
	// just class+names: several rows legitimately tie on class+old/new name
	// alone (e.g. many structs each gain an added "CustomData" field, so
	// many added_field rows share NewName="CustomData" with different
	// owners), and sort.Slice is not a stable sort -- feeding it rows built
	// by ranging over Go maps (whose iteration order is randomised per
	// process) without a fully discriminating key produces a different tie
	// order on every run, silently breaking the run-to-run determinism this
	// tool's output is required to have.
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Class != b.Class {
			return a.Class < b.Class
		}
		if a.OwnerOld != b.OwnerOld {
			return a.OwnerOld < b.OwnerOld
		}
		if a.OwnerNew != b.OwnerNew {
			return a.OwnerNew < b.OwnerNew
		}
		if a.OldName != b.OldName {
			return a.OldName < b.OldName
		}
		if a.NewName != b.NewName {
			return a.NewName < b.NewName
		}
		return a.Detail < b.Detail
	})
	return rows
}

// pairRenames matches every old-only struct/enum (no same-name counterpart
// in new) against the best-fitting new-only one, using content that a
// Go-identifier rename never changes: a struct's set of JSON tag keys (the
// schema property names), and an enum's set of literal wire values. Both
// signals come from the wire contract, not the Go spelling, which is
// exactly what a naming-transform rename preserves and a real type deletion
// or addition would not.
//
// A candidate is only auto-paired when it is the STRICT, UNIQUE best match
// for that old symbol (no other new-side candidate ties its score). This
// matters concretely on this corpus: OCPP reuses the bare two-value
// {"Accepted","Rejected"} enum shape across several unrelated schema types
// (GenericStatusEnumType, ChargingProfileStatusEnumType,
// RequestStartStopStatusEnumType and others), so a plain best-score pick
// would silently mispair three old enums against one new one depending on
// map/slice iteration order -- wrong, and non-deterministic besides. A tie
// is reported as ClassAmbiguousRename instead of guessed at.
func pairRenames(old, new_ *SymbolTable) renamePairing {
	pairing := renamePairing{
		structPairs:             map[string]string{},
		enumPairs:               map[string]string{},
		ambiguousOldStructNames: map[string]bool{},
		ambiguousOldEnumNames:   map[string]bool{},
		ambiguousNewStructNames: map[string]bool{},
		ambiguousNewEnumNames:   map[string]bool{},
	}

	oldOnlyStructs := onlyIn(keysOfStructs(old.Structs), keysOfStructs(new_.Structs))
	newOnlyStructs := onlyIn(keysOfStructs(new_.Structs), keysOfStructs(old.Structs))
	for _, oldName := range oldOnlyStructs {
		oldKeys := jsonKeySet(old.Structs[oldName])
		best, tied := bestCandidates(newOnlyStructs, 0.5, func(newName string) float64 {
			return jaccard(oldKeys, jsonKeySet(new_.Structs[newName]))
		})
		switch len(best) {
		case 0:
			// handled by typeRenameRows as removed_type
		case 1:
			pairing.structPairs[oldName] = best[0]
		default:
			pairing.ambiguousOldStructNames[oldName] = true
			for _, n := range best {
				pairing.ambiguousNewStructNames[n] = true
			}
			pairing.ambiguous = append(pairing.ambiguous, DeltaRow{
				Class: ClassAmbiguousRename, OldName: oldName,
				Detail: "struct: tied at score " + formatScore(tied) + " between " + strings.Join(best, ", ") + " -- adjudicate by hand, not auto-paired",
			})
		}
	}

	oldOnlyEnums := onlyIn(keysOfEnums(old.Enums), keysOfEnums(new_.Enums))
	newOnlyEnums := onlyIn(keysOfEnums(new_.Enums), keysOfEnums(old.Enums))
	for _, oldName := range oldOnlyEnums {
		oldVals := stringSet(old.Enums[oldName].Values)
		best, tied := bestCandidates(newOnlyEnums, 0.5, func(newName string) float64 {
			return jaccard(oldVals, stringSet(new_.Enums[newName].Values))
		})
		switch len(best) {
		case 0:
			// handled by typeRenameRows as removed_type
		case 1:
			pairing.enumPairs[oldName] = best[0]
		default:
			pairing.ambiguousOldEnumNames[oldName] = true
			for _, n := range best {
				pairing.ambiguousNewEnumNames[n] = true
			}
			pairing.ambiguous = append(pairing.ambiguous, DeltaRow{
				Class: ClassAmbiguousRename, OldName: oldName,
				Detail: "enum: tied at score " + formatScore(tied) + " between " + strings.Join(best, ", ") + " -- adjudicate by hand, not auto-paired",
			})
		}
	}
	return pairing
}

// bestCandidates scores every candidate with score, keeping only those at or
// above threshold, and returns every candidate that achieves the maximum
// score (sorted, so the result -- and therefore whether len==1 -- never
// depends on map or slice iteration order) along with that maximum.
func bestCandidates(candidates []string, threshold float64, score func(string) float64) ([]string, float64) {
	max := 0.0
	scores := make(map[string]float64, len(candidates))
	for _, c := range candidates {
		s := score(c)
		scores[c] = s
		if s > max {
			max = s
		}
	}
	if max < threshold {
		return nil, max
	}
	var best []string
	for _, c := range candidates {
		if scores[c] == max {
			best = append(best, c)
		}
	}
	sort.Strings(best)
	return best, max
}

func formatScore(s float64) string {
	return strconv.FormatFloat(s, 'f', 2, 64)
}

func typeRenameRows(old, new_ *SymbolTable, pairing renamePairing) []DeltaRow {
	var rows []DeltaRow
	for oldName, newName := range pairing.structPairs {
		rows = append(rows, DeltaRow{Class: ClassRenamedType, OldName: oldName, NewName: newName, Detail: "struct, paired by JSON tag key set"})
	}
	for oldName, newName := range pairing.enumPairs {
		rows = append(rows, DeltaRow{Class: ClassRenamedType, OldName: oldName, NewName: newName, Detail: "enum, paired by wire value set"})
	}
	for _, oldName := range onlyIn(keysOfStructs(old.Structs), keysOfStructs(new_.Structs)) {
		_, paired := pairing.structPairs[oldName]
		if !paired && !pairing.ambiguousOldStructNames[oldName] {
			rows = append(rows, DeltaRow{Class: ClassRemovedType, OldName: oldName, Detail: "struct: no JSON-tag-set match found in new (Jaccard < 0.5 for every candidate)"})
		}
	}
	for _, newName := range onlyIn(keysOfStructs(new_.Structs), keysOfStructs(old.Structs)) {
		paired := false
		for _, n := range pairing.structPairs {
			if n == newName {
				paired = true
				break
			}
		}
		if !paired && !pairing.ambiguousNewStructNames[newName] {
			rows = append(rows, DeltaRow{Class: ClassAddedType, NewName: newName, Detail: "struct: no counterpart in old"})
		}
	}
	for _, oldName := range onlyIn(keysOfEnums(old.Enums), keysOfEnums(new_.Enums)) {
		_, paired := pairing.enumPairs[oldName]
		if !paired && !pairing.ambiguousOldEnumNames[oldName] {
			rows = append(rows, DeltaRow{Class: ClassRemovedType, OldName: oldName, Detail: "enum: no wire-value-set match found in new"})
		}
	}
	for _, newName := range onlyIn(keysOfEnums(new_.Enums), keysOfEnums(old.Enums)) {
		paired := false
		for _, n := range pairing.enumPairs {
			if n == newName {
				paired = true
				break
			}
		}
		if !paired && !pairing.ambiguousNewEnumNames[newName] {
			rows = append(rows, DeltaRow{Class: ClassAddedType, NewName: newName, Detail: "enum: no counterpart in old"})
		}
	}
	return rows
}

// fieldRows diffs fields within every matched struct pair (same-name
// matches and content-paired renames alike), joining old and new fields by
// JSON tag key -- the one thing a Go-level rename never changes -- rather
// than by Go field name.
func fieldRows(old, new_ *SymbolTable, pairing renamePairing) []DeltaRow {
	var rows []DeltaRow
	pairs := map[string]string{}
	for name := range old.Structs {
		if _, ok := new_.Structs[name]; ok {
			pairs[name] = name
		}
	}
	for o, n := range pairing.structPairs {
		pairs[o] = n
	}
	for oldOwner, newOwner := range pairs {
		oldFields := byJSONKey(old.Structs[oldOwner].Fields)
		newFields := byJSONKey(new_.Structs[newOwner].Fields)
		for key, of := range oldFields {
			nf, ok := newFields[key]
			if !ok {
				rows = append(rows, DeltaRow{Class: ClassRemovedField, OwnerOld: oldOwner, OwnerNew: newOwner, OldName: of.GoName, Detail: "json:" + key})
				continue
			}
			if of.GoName != nf.GoName {
				rows = append(rows, DeltaRow{Class: ClassRenamedField, OwnerOld: oldOwner, OwnerNew: newOwner, OldName: of.GoName, NewName: nf.GoName, Detail: "json:" + key})
			}
			oldPtr := isPointerType(of.GoType)
			newPtr := isPointerType(nf.GoType)
			if oldPtr != newPtr {
				rows = append(rows, DeltaRow{
					Class: ClassChangedPointerness, OwnerOld: oldOwner, OwnerNew: newOwner,
					OldName: of.GoName, NewName: nf.GoName,
					Detail: of.GoType + " -> " + nf.GoType,
				})
			}
		}
		for key, nf := range newFields {
			if _, ok := oldFields[key]; !ok {
				rows = append(rows, DeltaRow{Class: ClassAddedField, OwnerOld: oldOwner, OwnerNew: newOwner, NewName: nf.GoName, Detail: "json:" + key})
			}
		}
	}
	return rows
}

// constructorRows pairs New* functions by the struct type they return
// (through the same struct-rename pairing field diffing uses) and flags a
// signature change by parameter count or parameter type list.
func constructorRows(old, new_ *SymbolTable, pairing renamePairing) []DeltaRow {
	var rows []DeltaRow
	returnsOld := map[string]FuncSymbol{}
	for _, fn := range old.Funcs {
		if fn.ReturnsStructName != "" {
			returnsOld[fn.ReturnsStructName] = fn
		}
	}
	returnsNew := map[string]FuncSymbol{}
	for _, fn := range new_.Funcs {
		if fn.ReturnsStructName != "" {
			returnsNew[fn.ReturnsStructName] = fn
		}
	}
	structPairs := map[string]string{}
	for name := range old.Structs {
		if _, ok := new_.Structs[name]; ok {
			structPairs[name] = name
		}
	}
	for o, n := range pairing.structPairs {
		structPairs[o] = n
	}
	for oldType, newType := range structPairs {
		of, oldHas := returnsOld[oldType]
		nf, newHas := returnsNew[newType]
		switch {
		case oldHas && !newHas:
			rows = append(rows, DeltaRow{Class: ClassChangedConstructorSig, OwnerOld: oldType, OwnerNew: newType, OldName: of.Name, Detail: "constructor removed"})
		case !oldHas && newHas:
			rows = append(rows, DeltaRow{Class: ClassChangedConstructorSig, OwnerOld: oldType, OwnerNew: newType, NewName: nf.Name, Detail: "constructor added"})
		case oldHas && newHas && !sameStrings(of.Params, nf.Params):
			rows = append(rows, DeltaRow{
				Class: ClassChangedConstructorSig, OwnerOld: oldType, OwnerNew: newType,
				OldName: of.Name, NewName: nf.Name,
				Detail: joinParams(of.Params) + " -> " + joinParams(nf.Params),
			})
		}
	}
	return rows
}

// validatorTagRows pairs old and new RegisterValidation entries through the
// enum they validate (an "isValid<EnumName>" function name in both the
// hand-written and the generated code -- the emitter follows the same
// convention the hand-written source already used, verified against
// internal/codegen/emit.go's renderTypesFileBody), and flags a tag-token
// rename.
func validatorTagRows(old, new_ *SymbolTable, pairing renamePairing) []DeltaRow {
	var rows []DeltaRow
	oldFnToTag := map[string]string{}
	for tag, fn := range old.RegisteredValidations {
		oldFnToTag[fn] = tag
	}
	newFnToTag := map[string]string{}
	for tag, fn := range new_.RegisteredValidations {
		newFnToTag[fn] = tag
	}

	enumPairs := map[string]string{}
	for name := range old.Enums {
		if _, ok := new_.Enums[name]; ok {
			enumPairs[name] = name
		}
	}
	for o, n := range pairing.enumPairs {
		enumPairs[o] = n
	}
	for oldEnum, newEnum := range enumPairs {
		oldFn := "isValid" + oldEnum
		newFn := "isValid" + newEnum
		oldTag, oldOK := oldFnToTag[oldFn]
		newTag, newOK := newFnToTag[newFn]
		if oldOK && newOK && oldTag != newTag {
			rows = append(rows, DeltaRow{Class: ClassRenamedValidatorTag, OwnerOld: oldEnum, OwnerNew: newEnum, OldName: oldTag, NewName: newTag})
		}
	}
	return rows
}

// structValidatorRows flags a RegisterStructValidation entry present in old
// with no counterpart in new for the same (possibly renamed) target
// struct -- the class the deliberate validateHeartbeatResponse removal belongs to.
func structValidatorRows(old, new_ *SymbolTable, pairing renamePairing) []DeltaRow {
	var rows []DeltaRow
	structPairs := map[string]string{}
	for name := range old.Structs {
		if _, ok := new_.Structs[name]; ok {
			structPairs[name] = name
		}
	}
	for o, n := range pairing.structPairs {
		structPairs[o] = n
	}
	for target, fn := range old.StructValidations {
		newTarget, known := structPairs[target]
		if !known {
			newTarget = target
		}
		if _, stillThere := new_.StructValidations[newTarget]; !stillThere {
			rows = append(rows, DeltaRow{Class: ClassRemovedStructValidator, OwnerOld: target, OwnerNew: newTarget, OldName: fn})
		}
	}
	return rows
}

// --- small set helpers ---

func keysOfStructs(m map[string]StructSymbol) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func keysOfEnums(m map[string]EnumSymbol) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// onlyIn returns the members of a that are not in b.
func onlyIn(a, b []string) []string {
	bs := map[string]bool{}
	for _, x := range b {
		bs[x] = true
	}
	var out []string
	for _, x := range a {
		if !bs[x] {
			out = append(out, x)
		}
	}
	return out
}

func jsonKeySet(s StructSymbol) map[string]bool {
	set := map[string]bool{}
	for _, f := range s.Fields {
		if f.JSONTag != "" && f.JSONTag != "-" {
			set[f.JSONTag] = true
		}
	}
	return set
}

func stringSet(values []string) map[string]bool {
	set := map[string]bool{}
	for _, v := range values {
		set[v] = true
	}
	return set
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func byJSONKey(fields []FieldSymbol) map[string]FieldSymbol {
	m := map[string]FieldSymbol{}
	for _, f := range fields {
		if f.JSONTag != "" && f.JSONTag != "-" {
			m[f.JSONTag] = f
		}
	}
	return m
}

func isPointerType(t string) bool {
	return len(t) > 0 && t[0] == '*'
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func joinParams(params []string) string {
	if len(params) == 0 {
		return "()"
	}
	out := "("
	for i, p := range params {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out + ")"
}
