package fixture

// SkippedEmbed's own field would be promoted straight into the enclosing
// payload's JSON namespace if the embedding itself were not suppressed.
type SkippedEmbed struct {
	Promoted string `json:"promoted"`
}

// SkipPayload holds the three tag shapes that look alike and mean different
// things to encoding/json: a tag of exactly "-" (never encoded), a tag of
// "-," (encoded, under the literal one-character name "-"), and no tag at all
// (encoded, under the Go field name).
type SkipPayload struct {
	Kept        string `json:"kept"`
	Internal    string `json:"-"`
	LiteralDash string `json:"-,"`
	Untagged    string
	// An embedded field tagged "-" is suppressed along with everything it
	// would otherwise promote.
	SkippedEmbed `json:"-"`
}
