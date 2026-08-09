// Package widgetblock is an orchestration-test fixture mirroring the real
// tree's functional-block shape: a CSMSHandler/ChargingStationHandler pair,
// an ocpp.NewProfile registration, and one message (Widget) whose request
// references the sibling types package both as a plain composite
// field and as a slice element, so an end-to-end run exercises discovery,
// direction detection, cross-file/cross-package stitching and
// STRUCT_VALIDATOR annotation together, the way the real tree does.
package widgetblock

import (
	"reflect"

	"github.com/enesismail/ocpp-go/internal/schemacheck/testdata/orchestration/tree/types"
	"github.com/enesismail/ocpp-go/ocpp"
)

const WidgetFeatureName = "Widget"

// CSMSHandler is deliberately named identically to the real tree's own
// per-block interface, so discoverDirections' name-based dispatch applies
// unchanged.
type CSMSHandler interface {
	OnWidget(stationID string, request *WidgetRequest) (response *WidgetResponse, err error)
}

type ChargingStationHandler interface {
}

const ProfileName = "WidgetBlock"

var Profile = ocpp.NewProfile(
	ProfileName,
	WidgetFeature{},
)

// WidgetRequest exercises: a plain required string (name), a cross-package
// enum whose value set matches the schema's exactly (status), a cross-package
// composite carrying its own struct-level validator (detail), a slice of
// that same composite (tags), and a field the schema does not authorize at
// all (secret).
type WidgetRequest struct {
	Name   string         `json:"name" validate:"required,max=50"`
	Status types.Status   `json:"status,omitempty" validate:"omitempty,widgetStatus"`
	Detail types.Detail   `json:"detail" validate:"required"`
	Tags   []types.Detail `json:"tags,omitempty" validate:"omitempty,dive"`
	Secret string         `json:"secret"`
}

// WidgetResponse exercises: a plain required boolean (accepted) and a
// numeric field whose upper bound is stricter than the schema's (level).
type WidgetResponse struct {
	Accepted bool `json:"accepted" validate:"required"`
	Level    int  `json:"level,omitempty" validate:"omitempty,min=1,max=5"`
}

type WidgetFeature struct{}

func (f WidgetFeature) GetFeatureName() string        { return WidgetFeatureName }
func (f WidgetFeature) GetRequestType() reflect.Type  { return reflect.TypeOf(WidgetRequest{}) }
func (f WidgetFeature) GetResponseType() reflect.Type { return reflect.TypeOf(WidgetResponse{}) }

func (r WidgetRequest) GetFeatureName() string  { return WidgetFeatureName }
func (c WidgetResponse) GetFeatureName() string { return WidgetFeatureName }
