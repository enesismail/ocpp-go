// Package types is a shared-type package fixture, named to match the real
// tree's own ocpp2.0.1/types convention (buildSharedTypes looks for a
// package literally named "types" under the tree root): a scalar-alias
// enum, a composite struct referenced across package boundaries, and a
// struct-level validator registered against that composite — enough for an
// orchestration test to exercise the tree-wide scalar-alias registry,
// cross-file/cross-package struct stitching, STRUCT_VALIDATOR annotation
// and shared-type occurrence tracking without touching the real ocpp2.0.1
// tree.
package types

import "gopkg.in/go-playground/validator.v9"

var Validate = validator.New()

// Status is a protocol-style enum: a named scalar alias with a const block
// and a registered validator, the idiom the real message tree writes every
// enum in.
type Status string

const (
	StatusOK  Status = "OK"
	StatusBad Status = "Bad"
)

func isValidStatus(fl validator.FieldLevel) bool {
	switch Status(fl.Field().String()) {
	case StatusOK, StatusBad:
		return true
	}
	return false
}

// Detail is a shared composite, referenced from another package's message
// struct both as a plain field and as a slice element — the two shapes
// expand.go's stitching must reach that walkGoFile alone never recurses
// into for a cross-package type.
type Detail struct {
	Code  string `json:"code" validate:"required,max=20"`
	Notes string `json:"notes,omitempty" validate:"max=100"`
}

func isValidDetail(sl validator.StructLevel) {
	_ = sl
}

func init() {
	_ = Validate.RegisterValidation("widgetStatus", isValidStatus)
	Validate.RegisterStructValidation(isValidDetail, Detail{})
}
