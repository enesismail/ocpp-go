package fixtureregistry

import "gopkg.in/go-playground/validator.v9"

type WidgetStatus string

const (
	WidgetStatusOK     WidgetStatus = "OK"
	WidgetStatusBroken WidgetStatus = "Broken"
)

func isValidWidgetStatus(fl validator.FieldLevel) bool {
	status := WidgetStatus(fl.Field().String())
	switch status {
	case WidgetStatusOK, WidgetStatusBroken:
		return true
	default:
		return false
	}
}
