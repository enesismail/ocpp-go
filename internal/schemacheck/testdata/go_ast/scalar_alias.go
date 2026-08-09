package fixture

// BootReason models the idiom the real message tree writes protocol enums in:
// a named type whose underlying type is a scalar, together with a const block
// of its permitted values. A field of this type puts a JSON string on the
// wire, not an object, so the wire type has to follow the declaration through
// to the scalar rather than stopping at the name.
type BootReason string

const (
	BootReasonPowerUp     BootReason = "PowerUp"
	BootReasonLocalReset  BootReason = "LocalReset"
	BootReasonRemoteReset BootReason = "RemoteReset"
)

// Interval and Enabled are the same idiom for the two other scalar kinds a
// schema property can be typed as.
type Interval int

type Enabled bool

// Chained is declared in terms of another named scalar alias rather than a
// builtin directly, so reaching its scalar means following the chain rather
// than taking a single hop.
type Chained BootReason

// Counter is declared through byte, one of the two aliases Go itself defines,
// which has to fold onto uint8 to be recognizable as an integer at all.
type Counter byte

// Opaque is the negative control: a named type whose declaration is a struct,
// not a scalar. It carries no single scalar kind to inherit and must keep
// being walked as the struct it is.
type Opaque struct {
	Code string `json:"code"`
}

type AliasPayload struct {
	Reason   BootReason `json:"reason" validate:"required"`
	Interval Interval   `json:"interval"`
	Enabled  Enabled    `json:"enabled"`
	Chained  Chained    `json:"chained"`
	Counter  Counter    `json:"counter"`
	Nested   Opaque     `json:"nested"`
}
