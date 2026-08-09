// Package types is a shared-type package fixture, named to match the real
// tree's own convention: it supplies one composite that a message in another
// package references and that carries its own struct-level validation rule,
// so a run over this fixture produces a struct-validator row of the ordinary,
// field-anchored kind alongside the message-root one the sibling package
// registers.
package types

import "gopkg.in/go-playground/validator.v9"

var Validate = validator.New()

// Part is a shared composite whose own type carries a struct-level rule —
// the shape that surfaces as a struct-validator row on the *field* that
// holds it.
type Part struct {
	Code string `json:"code" validate:"required,max=20"`
}

func isValidPart(sl validator.StructLevel) {
	_ = sl
}

func init() {
	Validate.RegisterStructValidation(isValidPart, Part{})
}
