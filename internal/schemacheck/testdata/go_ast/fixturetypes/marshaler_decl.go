// Package fixturetypes is a stand-in for the package that actually declares
// a custom-marshaled wire type (mirrors ocpp2.0.1/types/datetime.go): the
// type is declared and its MarshalJSON/UnmarshalJSON pair defined here, in
// the package the message files import, not in the message file itself.
package fixturetypes

type WireTime struct {
	Seconds int
}

func (t WireTime) MarshalJSON() ([]byte, error)     { return nil, nil }
func (t *WireTime) UnmarshalJSON(data []byte) error { return nil }
