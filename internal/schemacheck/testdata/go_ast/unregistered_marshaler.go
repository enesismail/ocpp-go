package fixture

type UnknownWireValue struct {
	Value string
}

func (UnknownWireValue) MarshalJSON() ([]byte, error) { return nil, nil }
func (*UnknownWireValue) UnmarshalJSON([]byte) error  { return nil }

type UnknownPayload struct {
	Value UnknownWireValue `json:"value"`
}
