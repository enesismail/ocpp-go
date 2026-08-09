// Package gadgetblock is a fixture mirroring the real tree's functional-block
// shape — a CSMSHandler/ChargingStationHandler pair, an ocpp.NewProfile
// registration and one message (Gadget) — built around the one arrangement
// no other fixture carries: a struct-level validation rule registered
// against the message's *own response struct* rather than against the type
// of one of its fields. That rule belongs to no field, so it can only appear
// in a report as a row of the message root's own.
//
// The request side registers no such rule but does hold a composite whose
// type carries one, so the two kinds of struct-validator row occur side by
// side in a single run and a test can tell them apart.
//
// The tree root directory is deliberately named for the schema corpus whose
// independently measured numbers this tool records: a run over this fixture
// is therefore held to those numbers, which a two-file corpus cannot
// reproduce. That makes it also the fixture for a run that must refuse to
// publish.
package gadgetblock

import (
	"reflect"

	"github.com/enesismail/ocpp-go/internal/schemacheck/testdata/rootvalidator/ocpp2.0.1/types"
	"github.com/enesismail/ocpp-go/ocpp"
	"gopkg.in/go-playground/validator.v9"
)

const GadgetFeatureName = "Gadget"

// CSMSHandler is deliberately named identically to the real tree's own
// per-block interface, so direction detection's name-based dispatch applies
// unchanged.
type CSMSHandler interface {
	OnGadget(stationID string, request *GadgetRequest) (response *GadgetResponse, err error)
}

type ChargingStationHandler interface {
}

const ProfileName = "GadgetBlock"

var Profile = ocpp.NewProfile(
	ProfileName,
	GadgetFeature{},
)

// GadgetRequest holds a cross-package composite whose own type carries a
// struct-level rule — the field-anchored kind of struct-validator row.
type GadgetRequest struct {
	Part types.Part `json:"part" validate:"required"`
}

// GadgetResponse carries a struct-level rule registered against itself, not
// against any of its field types.
type GadgetResponse struct {
	Serial string `json:"serial" validate:"required,max=20"`
}

type GadgetFeature struct{}

func (f GadgetFeature) GetFeatureName() string        { return GadgetFeatureName }
func (f GadgetFeature) GetRequestType() reflect.Type  { return reflect.TypeOf(GadgetRequest{}) }
func (f GadgetFeature) GetResponseType() reflect.Type { return reflect.TypeOf(GadgetResponse{}) }

func (r GadgetRequest) GetFeatureName() string  { return GadgetFeatureName }
func (c GadgetResponse) GetFeatureName() string { return GadgetFeatureName }

func validateGadgetResponse(sl validator.StructLevel) {
	_ = sl
}

func init() {
	types.Validate.RegisterStructValidation(validateGadgetResponse, GadgetResponse{})
}
