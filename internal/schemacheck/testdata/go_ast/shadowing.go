package fixture

// ShadowedEmbed models the shallower-wins case: its own Code field shares a
// JSON name with a field ShadowingPayload declares directly, one embedding
// hop shallower.
type ShadowedEmbed struct {
	Code string `json:"code" validate:"len=4"`
}

// ShadowingPayload's own Code field is declared directly (depth 0); its
// embedded ShadowedEmbed's Code field is promoted one hop deeper (depth 1).
// encoding/json marshals only the shallower one, so the flattened field set
// must keep ShadowingPayload's own Code — validate:"required", not
// ShadowedEmbed's validate:"len=4" — and drop the promoted one entirely.
type ShadowingPayload struct {
	Code string `json:"code" validate:"required"`
	ShadowedEmbed
}

// EmbedA and EmbedB both promote a Note field at the same depth (1) into
// AmbiguousPayload — a true same-depth collision, which encoding/json
// resolves by dropping every field carrying that name rather than
// guessing which one the caller meant.
type EmbedA struct {
	Note string `json:"note"`
}

type EmbedB struct {
	Note string `json:"note"`
}

type AmbiguousPayload struct {
	EmbedA
	EmbedB
}
