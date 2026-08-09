package fixture

import shared "example.com/sharedtypes"

// CrossFilePayload models the shapes a single file's AST cannot account for
// on its own. Every field's type is declared somewhere else: Status and
// Detail in another package, Sibling in another file of this same package —
// the real cases being the shared types package's enum aliases and the
// aliases one message file declares and another uses.
type CrossFilePayload struct {
	// Status names a scalar alias in another package. What it puts on the
	// wire is that scalar, which only the tree-wide registry can say.
	Status shared.Status `json:"status" validate:"required,sharedStatus"`
	// Detail names a type in that same package that nothing registered, so
	// nothing is known about it and it stays opaque.
	Detail shared.Detail `json:"detail"`
	// Sibling names a type declared in another file of this same package,
	// reached by a bare name rather than a selector.
	Sibling SiblingStatus `json:"sibling" validate:"required,siblingStatus"`
}
