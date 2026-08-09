package fixture

type Inner struct {
	Code string `json:"code"`
}

// TaggedEmbedPayload embeds Inner *with* a JSON name. encoding/json does not
// flatten such an embed: it marshals as an ordinary named field, so Code
// appears under "inner", never promoted into the payload's own namespace.
type TaggedEmbedPayload struct {
	Inner `json:"inner"`
	Note  string `json:"note"`
}

// EmbedTagged and EmbedUntagged both promote a field that marshals under the
// name "Note" at the same depth — one because its tag says so, the other by
// falling back to its Go field name. encoding/json does not call that
// ambiguous: a tag is a statement of intent, so the tagged one wins.
type EmbedTagged struct {
	Note string `json:"Note"`
}

type EmbedUntagged struct {
	Note string
}

type TaggedDominancePayload struct {
	EmbedTagged
	EmbedUntagged
}
