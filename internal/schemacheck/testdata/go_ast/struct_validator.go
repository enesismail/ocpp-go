package fixture

// structLevel and structValidatorRegistry are lightweight local stand-ins
// for go-playground/validator's StructLevel/RegisterStructValidation idiom
// (mirrors ocpp2.0.1/types/types.go:757-758's isValidIdToken registration),
// present only so the AST shape of a RegisterStructValidation call —
// binding a validator function to a target struct value — matches the real
// tree without requiring the fixture to import the real validator package.
type structLevel interface{}

type structValidatorRegistry interface {
	RegisterStructValidation(fn func(structLevel), types ...interface{})
}

var StructValidate structValidatorRegistry

type Foo struct {
	Value string `json:"value"`
}

func validateFoo(sl structLevel) {}

func init() {
	StructValidate.RegisterStructValidation(validateFoo, Foo{})
}
