package fixture

import fixturetypes "example.com/fixturetypes"

type NestedValue struct {
	Code string `json:"code" validate:"required"`
}

// RegisteredPayload models the real shape a custom-marshaled field takes in
// the tree: a pointer to a package-qualified type from another package
// (mirroring *types.DateTime), not a bare local type name. Nested is a
// named (non-anonymous) field of a plain struct type, declared in this same
// file, so it is the positive-recursion case: its own field must be walked
// and produced as a "Nested."-prefixed leaf path, in contrast to When, whose
// type is a registered wire type and must not be recursed into at all.
type RegisteredPayload struct {
	When   *fixturetypes.WireTime `json:"when"`
	Nested NestedValue            `json:"nested"`
}
