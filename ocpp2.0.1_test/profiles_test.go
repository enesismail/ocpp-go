package ocpp2_test

import (
	"testing"

	"github.com/enesismail/ocpp-go/ocpp"
	ocpp2 "github.com/enesismail/ocpp-go/ocpp2.0.1"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/authorization"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/availability"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/data"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/diagnostics"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/display"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/firmware"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/iso15118"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/localauth"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/meter"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/provisioning"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/remotecontrol"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/reservation"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/security"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/smartcharging"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/tariffcost"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/transactions"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultProfilesReturnsOCPP201ProfilesInOrder(t *testing.T) {
	expected := []*ocpp.Profile{
		authorization.Profile,
		availability.Profile,
		data.Profile,
		diagnostics.Profile,
		display.Profile,
		firmware.Profile,
		iso15118.Profile,
		localauth.Profile,
		meter.Profile,
		provisioning.Profile,
		remotecontrol.Profile,
		reservation.Profile,
		security.Profile,
		smartcharging.Profile,
		tariffcost.Profile,
		transactions.Profile,
	}

	got := ocpp2.DefaultProfiles()

	require.Len(t, got, 16)
	require.Len(t, got, len(expected))
	for i := range expected {
		assert.NotNil(t, got[i])
		assert.Same(t, expected[i], got[i])
	}
}

func TestDefaultProfilesReturnsFreshOCPP201Slice(t *testing.T) {
	p := ocpp2.DefaultProfiles()
	p[0] = nil

	assert.Same(t, authorization.Profile, ocpp2.DefaultProfiles()[0])
}
