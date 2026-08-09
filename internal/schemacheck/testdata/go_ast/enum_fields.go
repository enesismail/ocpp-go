package fixture

// The interfaces and registry variable below are lightweight local stand-ins
// for the go-playground/validator idiom the real tree uses, present only so
// the AST shape matches without the fixture importing the real package. This
// fixture differs from enums.go in what it is for: enums.go covers extracting
// a validator's accepted value set on its own, while this one covers that set
// reaching the *fields* that carry the tag.
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

type UploadState string

const (
	UploadStateIdle     UploadState = "Idle"
	UploadStateUploaded UploadState = "Uploaded"
)

func isValidUploadState(fl FieldLevel) bool {
	state := UploadState(fl.Field().String())
	switch state {
	case UploadStateIdle, UploadStateUploaded:
		return true
	default:
		return false
	}
}

func init() {
	_ = Validate.RegisterValidation("uploadState", isValidUploadState)
}

type EnumPayload struct {
	// State carries the registered tag at field level, so the values that tag
	// accepts are this field's own value set.
	State UploadState `json:"state" validate:"required,uploadState"`
	// Plain carries no registered tag at all — "required" and "max=20" name
	// no validator with an accepted value set — so it has no extractable one.
	Plain string `json:"plain" validate:"required,max=20"`
	// States carries the registered tag after "dive", which hands it to the
	// slice's elements. It describes what each element accepts, never what
	// the slice itself does, so it is not this field's value set.
	States []UploadState `json:"states,omitempty" validate:"omitempty,max=4,dive,uploadState"`
}
