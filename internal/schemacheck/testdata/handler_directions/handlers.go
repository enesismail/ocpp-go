// Package fixturehandlers reproduces OCPP 1.6's own handler-interface naming
// (CentralSystemHandler/ChargePointHandler), distinct from 2.0.1's
// CSMSHandler/ChargingStationHandler pair, so discoverDirections' 1.6 branch
// is exercised against real Go syntax rather than only unit-tested in
// isolation.
package fixturehandlers

// CentralSystemHandler is implemented by the party a Charge Point sends
// WidgetRequest to.
type CentralSystemHandler interface {
	OnWidget(request *WidgetRequest) (*WidgetResponse, error)
}

// ChargePointHandler is implemented by the party a Central System sends
// GadgetRequest to.
type ChargePointHandler interface {
	OnGadget(request *GadgetRequest) (*GadgetResponse, error)
}

type WidgetRequest struct{}
type WidgetResponse struct{}
type GadgetRequest struct{}
type GadgetResponse struct{}
