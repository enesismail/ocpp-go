package ocpp16_test

import (
	"testing"

	"github.com/enesismail/ocpp-go/ocpp"
	ocpp16 "github.com/enesismail/ocpp-go/ocpp1.6"
	"github.com/enesismail/ocpp-go/ocpp1.6/certificates"
	"github.com/enesismail/ocpp-go/ocpp1.6/core"
	"github.com/enesismail/ocpp-go/ocpp1.6/extendedtriggermessage"
	"github.com/enesismail/ocpp-go/ocpp1.6/firmware"
	"github.com/enesismail/ocpp-go/ocpp1.6/localauth"
	"github.com/enesismail/ocpp-go/ocpp1.6/logging"
	"github.com/enesismail/ocpp-go/ocpp1.6/remotetrigger"
	"github.com/enesismail/ocpp-go/ocpp1.6/reservation"
	"github.com/enesismail/ocpp-go/ocpp1.6/securefirmware"
	"github.com/enesismail/ocpp-go/ocpp1.6/security"
	"github.com/enesismail/ocpp-go/ocpp1.6/smartcharging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultProfilesReturnsOCPP16ProfilesInOrder(t *testing.T) {
	expected := []*ocpp.Profile{
		core.Profile,
		localauth.Profile,
		firmware.Profile,
		reservation.Profile,
		remotetrigger.Profile,
		smartcharging.Profile,
		logging.Profile,
		security.Profile,
		extendedtriggermessage.Profile,
		certificates.Profile,
		securefirmware.Profile,
	}

	got := ocpp16.DefaultProfiles()

	require.Len(t, got, 11)
	require.Len(t, got, len(expected))
	for i := range expected {
		assert.NotNil(t, got[i])
		assert.Same(t, expected[i], got[i])
	}
}

func TestDefaultProfilesReturnsFreshOCPP16Slice(t *testing.T) {
	p := ocpp16.DefaultProfiles()
	p[0] = nil

	assert.Same(t, core.Profile, ocpp16.DefaultProfiles()[0])
}
