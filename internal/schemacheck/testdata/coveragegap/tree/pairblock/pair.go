// Package pairblock is a fixture for the coverage half of the refusal to
// publish an incomplete comparison: two messages, so that a schema set
// missing one message's request half still leaves the other message fully
// paired. That is what makes the
// resulting run a *partial* comparison — one that computes every roll-up
// over a subset without saying so — rather than one that simply has nothing
// to compare and fails on its own.
//
// The tree root directory is deliberately not named for the corpus whose
// measured numbers this tool records, so a run over it is not held to those
// numbers and incomplete coverage is the only thing that can hold it back.
package pairblock

import (
	"reflect"

	"github.com/enesismail/ocpp-go/ocpp"
)

const (
	AlphaFeatureName = "Alpha"
	BetaFeatureName  = "Beta"
)

type CSMSHandler interface {
	OnAlpha(stationID string, request *AlphaRequest) (response *AlphaResponse, err error)
	OnBeta(stationID string, request *BetaRequest) (response *BetaResponse, err error)
}

type ChargingStationHandler interface {
}

const ProfileName = "PairBlock"

var Profile = ocpp.NewProfile(
	ProfileName,
	AlphaFeature{},
	BetaFeature{},
)

type AlphaRequest struct {
	Name string `json:"name" validate:"required,max=20"`
}

type AlphaResponse struct {
	Accepted bool `json:"accepted" validate:"required"`
}

type BetaRequest struct {
	Name string `json:"name" validate:"required,max=20"`
}

type BetaResponse struct {
	Accepted bool `json:"accepted" validate:"required"`
}

type AlphaFeature struct{}

func (f AlphaFeature) GetFeatureName() string        { return AlphaFeatureName }
func (f AlphaFeature) GetRequestType() reflect.Type  { return reflect.TypeOf(AlphaRequest{}) }
func (f AlphaFeature) GetResponseType() reflect.Type { return reflect.TypeOf(AlphaResponse{}) }

type BetaFeature struct{}

func (f BetaFeature) GetFeatureName() string        { return BetaFeatureName }
func (f BetaFeature) GetRequestType() reflect.Type  { return reflect.TypeOf(BetaRequest{}) }
func (f BetaFeature) GetResponseType() reflect.Type { return reflect.TypeOf(BetaResponse{}) }

func (r AlphaRequest) GetFeatureName() string  { return AlphaFeatureName }
func (c AlphaResponse) GetFeatureName() string { return AlphaFeatureName }
func (r BetaRequest) GetFeatureName() string   { return BetaFeatureName }
func (c BetaResponse) GetFeatureName() string  { return BetaFeatureName }
