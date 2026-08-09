// Package fixtureregistry is a two-file fixture reproducing the real
// ocpp1.6/types shape: a RegisterValidation call in one file naming a
// validator function declared in a sibling file of the same package
// (values.go), rather than in this file. A tree-wide, package-scoped
// validator registry has to see across this file boundary to resolve the
// "widgetStatus" tag's accepted value set at all.
package fixtureregistry

import "gopkg.in/go-playground/validator.v9"

var Validate = validator.New()

func init() {
	_ = Validate.RegisterValidation("widgetStatus", isValidWidgetStatus)
}
