package fixture

// FieldLevel and validationRegistry are lightweight local stand-ins for the
// go-playground/validator idiom the real tree uses (Validate.RegisterValidation
// registering a tag against a func(validator.FieldLevel) bool). The fixture
// is only ever AST-parsed, never compiled against the real validator
// package, so these stand-ins exist purely to give the parser the same
// shapes: a RegisterValidation call binding a tag token to a function, and
// that function doing a conversion line followed by a switch/case list.
type FieldLevel interface {
	Field() Value
}

type Value interface {
	String() string
}

type validationRegistry interface {
	RegisterValidation(tag string, fn func(FieldLevel) bool) error
}

var Validate validationRegistry

type Status string

const (
	StatusAccepted Status = "Accepted"
	StatusRejected Status = "Rejected"
)

func isValidStatusThing(fl FieldLevel) bool {
	status := Status(fl.Field().String())
	switch status {
	case StatusAccepted, StatusRejected:
		return true
	default:
		return false
	}
}

func init() {
	_ = Validate.RegisterValidation("statusThing", isValidStatusThing)
}
