package fixture

import "reflect"

type Embedded struct {
	Code string `json:"code" validate:"required"`
}

type Payload struct {
	ID     *string  `json:"id,omitempty" validate:"max=20"`
	Values []string `json:"values,omitempty" validate:"min=1,dive"`
	Embedded
}

// ClearDisplayFeature models the real counter-example where the returned
// feature-name constant disagrees with the type name: the type is named
// ClearDisplayFeature but GetFeatureName resolves to "ClearDisplayMessage".
// Discovery must resolve the returned identifier to its constant string —
// never derive a name from the type or the file.
type ClearDisplayFeature struct{}

const clearDisplayMessageFeatureName = "ClearDisplayMessage"

func (f ClearDisplayFeature) GetFeatureName() string        { return clearDisplayMessageFeatureName }
func (f ClearDisplayFeature) GetRequestType() reflect.Type  { return reflect.TypeOf(Payload{}) }
func (f ClearDisplayFeature) GetResponseType() reflect.Type { return reflect.TypeOf(Embedded{}) }

// Get15118EVCertificateFeature models the digits-in-name counter-example
// class, which breaks any [A-Za-z]*Feature style discovery pattern.
type Get15118EVCertificateFeature struct{}

const get15118EVCertificateFeatureName = "Get15118EVCertificate"

func (f Get15118EVCertificateFeature) GetFeatureName() string {
	return get15118EVCertificateFeatureName
}
func (f Get15118EVCertificateFeature) GetRequestType() reflect.Type {
	return reflect.TypeOf(Payload{})
}
func (f Get15118EVCertificateFeature) GetResponseType() reflect.Type {
	return reflect.TypeOf(Embedded{})
}

// PartialFeature implements only GetFeatureName, so method-set discovery
// must be able to tell a partial (any-of-three) match apart from a complete
// (all-three) one instead of requiring every method to be present.
type PartialFeature struct{}

const partialFeatureName = "Partial"

func (f PartialFeature) GetFeatureName() string { return partialFeatureName }
