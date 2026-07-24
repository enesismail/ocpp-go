package configmanager

import (
	"github.com/enesismail/ocpp-go/ocpp1.6/core"
	"github.com/enesismail/ocpp-go/ocpp1.6/firmware"
	"github.com/enesismail/ocpp-go/ocpp1.6/localauth"
	"github.com/enesismail/ocpp-go/ocpp1.6/smartcharging"
)

// Key identifies an OCPP 1.6 configuration key by its exact (case-sensitive)
// name, e.g. "HeartbeatInterval".
type Key string

// String returns the key's name.
func (k Key) String() string {
	return string(k)
}

// ISO15118ProfileName is a synthetic profile-name sentinel (D4a). Neither
// this fork nor upstream lorenzodonini/ocpp-go ships an ISO15118 profile
// package, so there is no real ProfileName constant to switch on for the
// ISO15118 keys below. Passing ISO15118ProfileName to
// GetMandatoryKeysForProfile, DefaultConfigurationFromProfiles, or
// NewManagerV16's profiles argument opts into the ISO15118 mandatory keys
// and default configuration.
const ISO15118ProfileName = "ISO15118"

const (
	/* ----------------- Core keys ----------------------- */

	AllowOfflineTxForUnknownId        = Key("AllowOfflineTxForUnknownId")
	AuthorizationCacheEnabled         = Key("AuthorizationCacheEnabled")
	AuthorizeRemoteTxRequests         = Key("AuthorizeRemoteTxRequests")
	BlinkRepeat                       = Key("BlinkRepeat")
	ClockAlignedDataInterval          = Key("ClockAlignedDataInterval")
	ConnectionTimeOut                 = Key("ConnectionTimeOut")
	GetConfigurationMaxKeys           = Key("GetConfigurationMaxKeys")
	HeartbeatInterval                 = Key("HeartbeatInterval")
	LightIntensity                    = Key("LightIntensity")
	LocalAuthorizeOffline             = Key("LocalAuthorizeOffline")
	LocalPreAuthorize                 = Key("LocalPreAuthorize")
	MaxEnergyOnInvalidId              = Key("MaxEnergyOnInvalidId")
	MeterValuesAlignedData            = Key("MeterValuesAlignedData")
	MeterValuesAlignedDataMaxLength   = Key("MeterValuesAlignedDataMaxLength")
	MeterValuesSampledData            = Key("MeterValuesSampledData")
	MeterValuesSampledDataMaxLength   = Key("MeterValuesSampledDataMaxLength")
	MeterValueSampleInterval          = Key("MeterValueSampleInterval")
	MinimumStatusDuration             = Key("MinimumStatusDuration")
	NumberOfConnectors                = Key("NumberOfConnectors")
	ResetRetries                      = Key("ResetRetries")
	ConnectorPhaseRotation            = Key("ConnectorPhaseRotation")
	ConnectorPhaseRotationMaxLength   = Key("ConnectorPhaseRotationMaxLength")
	StopTransactionOnEVSideDisconnect = Key("StopTransactionOnEVSideDisconnect")
	StopTransactionOnInvalidId        = Key("StopTransactionOnInvalidId")
	StopTxnAlignedData                = Key("StopTxnAlignedData")
	StopTxnAlignedDataMaxLength       = Key("StopTxnAlignedDataMaxLength")
	StopTxnSampledData                = Key("StopTxnSampledData")
	StopTxnSampledDataMaxLength       = Key("StopTxnSampledDataMaxLength")
	SupportedFeatureProfiles          = Key("SupportedFeatureProfiles")
	SupportedFeatureProfilesMaxLength = Key("SupportedFeatureProfilesMaxLength")
	TransactionMessageAttempts        = Key("TransactionMessageAttempts")
	TransactionMessageRetryInterval   = Key("TransactionMessageRetryInterval")
	UnlockConnectorOnEVSideDisconnect = Key("UnlockConnectorOnEVSideDisconnect")
	WebSocketPingInterval             = Key("WebSocketPingInterval")

	/* ----------------- LocalAuthList keys ----------------------- */

	LocalAuthListEnabled   = Key("LocalAuthListEnabled")
	LocalAuthListMaxLength = Key("LocalAuthListMaxLength")
	SendLocalListMaxLength = Key("SendLocalListMaxLength")

	/* ----------------- Reservation keys ----------------------- */

	ReserveConnectorZeroSupported = Key("ReserveConnectorZeroSupported")

	/* ----------------- Firmware keys ----------------------- */

	SupportedFileTransferProtocols = Key("SupportedFileTransferProtocols")

	/* ----------------- SmartCharging keys ----------------------- */

	ChargeProfileMaxStackLevel              = Key("ChargeProfileMaxStackLevel")
	ChargingScheduleAllowedChargingRateUnit = Key("ChargingScheduleAllowedChargingRateUnit")
	ChargingScheduleMaxPeriods              = Key("ChargingScheduleMaxPeriods")
	MaxChargingProfilesInstalled            = Key("MaxChargingProfilesInstalled")
	ConnectorSwitch3to1PhaseSupported       = Key("ConnectorSwitch3to1PhaseSupported")

	/* ----------------- ISO15118 keys (D4a) ----------------------- */

	CentralContractValidationAllowed = Key("CentralContractValidationAllowed")
	CertificateSignedMaxChainSize    = Key("CertificateSignedMaxChainSize")
	CertSigningWaitMinimum           = Key("CertSigningWaitMinimum")
	CertSigningRepeatTimes           = Key("CertSigningRepeatTimes")
	CertificateStoreMaxLength        = Key("CertificateStoreMaxLength")
	ContractValidationOffline        = Key("ContractValidationOffline")
	ISO15118PnCEnabled               = Key("ISO15118PnCEnabled")

	/* ----------------- Security extension keys -----------------------
	   Bare constants only (D4c): the source has no mandatory-key slice and
	   no defaults for these, and this fork's security.ProfileName has no
	   wiring here either — see PATCHES.md. */

	AuthorizationData              = Key("AuthorizationData")
	AdditionalRootCertificateCheck = Key("AdditionalRootCertificateCheck")
	CpoName                        = Key("CpoName")
	SecurityProfile                = Key("SecurityProfile")
)

var (
	MandatoryCoreKeys = []Key{
		AuthorizeRemoteTxRequests,
		ClockAlignedDataInterval,
		ConnectionTimeOut,
		GetConfigurationMaxKeys,
		HeartbeatInterval,
		LocalPreAuthorize,
		MeterValuesAlignedData,
		MeterValuesSampledData,
		MeterValueSampleInterval,
		NumberOfConnectors,
		ResetRetries,
		ConnectorPhaseRotation,
		StopTransactionOnEVSideDisconnect,
		StopTransactionOnInvalidId,
		StopTxnAlignedData,
		StopTxnSampledData,
		SupportedFeatureProfiles,
		TransactionMessageAttempts,
		TransactionMessageRetryInterval,
		UnlockConnectorOnEVSideDisconnect,
	}

	MandatoryLocalAuthKeys = []Key{
		LocalAuthListEnabled,
		LocalAuthListMaxLength,
		SendLocalListMaxLength,
	}

	MandatorySmartChargingKeys = []Key{
		MaxChargingProfilesInstalled,
		ChargingScheduleMaxPeriods,
		ChargingScheduleAllowedChargingRateUnit,
		ChargeProfileMaxStackLevel,
	}

	MandatoryFirmwareKeys = []Key{
		SupportedFileTransferProtocols,
	}

	MandatoryISO15118Keys = []Key{
		ISO15118PnCEnabled,
		ContractValidationOffline,
	}

	// Security extension keys have no mandatory subset (matches source).
)

// GetMandatoryKeysForProfile returns the union of mandatory keys for the
// given profile names. Unknown/unrecognized profile names are silently
// ignored (matches the source; contrast with DefaultConfigurationFromProfiles,
// which errors on an unknown profile — a known upstream inconsistency,
// carried forward and documented rather than "fixed" without a test to pin
// the resulting policy).
func GetMandatoryKeysForProfile(profiles ...string) []Key {
	var mandatoryKeys []Key

	for _, profile := range profiles {
		switch profile {
		case core.ProfileName:
			mandatoryKeys = append(mandatoryKeys, MandatoryCoreKeys...)
		case smartcharging.ProfileName:
			mandatoryKeys = append(mandatoryKeys, MandatorySmartChargingKeys...)
		case localauth.ProfileName:
			mandatoryKeys = append(mandatoryKeys, MandatoryLocalAuthKeys...)
		case firmware.ProfileName:
			mandatoryKeys = append(mandatoryKeys, MandatoryFirmwareKeys...)
		case ISO15118ProfileName:
			// D4a: this is the fix for the source's carried bug — the
			// "todo IS15118 mandatory keys validation" comment sat
			// unreachably inside the firmware case, since no ISO15118
			// profile name existed to switch on. It now has its own
			// reachable case.
			mandatoryKeys = append(mandatoryKeys, MandatoryISO15118Keys...)
		}
	}

	return mandatoryKeys
}
