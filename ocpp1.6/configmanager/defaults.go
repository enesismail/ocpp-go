package configmanager

import (
	"fmt"
	"strings"

	"github.com/enesismail/ocpp-go/ocpp1.6/core"
	"github.com/enesismail/ocpp-go/ocpp1.6/firmware"
	"github.com/enesismail/ocpp-go/ocpp1.6/localauth"
	"github.com/enesismail/ocpp-go/ocpp1.6/smartcharging"
	"github.com/enesismail/ocpp-go/ocpp1.6/types"
)

// NewEmptyConfiguration returns a Config with no keys, at version 1 (the
// Version=1 default that the source expressed via a `default:"1"` struct
// tag now lives here, per D3).
func NewEmptyConfiguration() Config {
	return Config{
		Version: 1,
		Keys:    []core.ConfigurationKey{},
	}
}

// DefaultConfigurationFromProfiles builds a Config containing the default
// configuration keys for the given profile names (see the Default*
// functions below). At least one profile must be given.
//
// Known limitation carried from the source: an unrecognized profile name
// makes this function return an error, whereas GetMandatoryKeysForProfile
// silently ignores unrecognized names. This inconsistency is documented
// rather than "resolved" behind a policy no test pins down.
func DefaultConfigurationFromProfiles(profiles ...string) (*Config, error) {
	keys := []core.ConfigurationKey{}

	if len(profiles) == 0 {
		return nil, fmt.Errorf("no profiles provided")
	}

	for _, profile := range profiles {
		switch profile {
		case core.ProfileName:
			keys = append(keys, DefaultCoreConfiguration()...)
		case localauth.ProfileName:
			keys = append(keys, DefaultLocalAuthConfiguration()...)
		case smartcharging.ProfileName:
			keys = append(keys, DefaultSmartChargingConfiguration()...)
		case firmware.ProfileName:
			keys = append(keys, DefaultFirmwareConfiguration()...)
		case ISO15118ProfileName:
			keys = append(keys, DefaultISO15118Configuration()...)
		default:
			return nil, fmt.Errorf("unknown profile %v", profile)
		}
	}

	return &Config{
		Version: 1,
		Keys:    keys,
	}, nil
}

// DefaultCoreConfiguration returns the default Core profile configuration
// keys.
//
// Known limitation carried from the source: SupportedFeatureProfiles is
// hardcoded to "Core" regardless of which profiles were actually requested
// from DefaultConfigurationFromProfiles — a stale advertisement if e.g.
// SmartCharging keys were also included. GetConfigurationMaxKeys is set
// explicitly to a non-nil default ("100", carried from the source) so a
// CSMS reading it never sees a nil/unset value and assumes "unlimited"
// (DB3).
func DefaultCoreConfiguration() []core.ConfigurationKey {
	standardMeasurands := strings.Join([]string{
		string(types.MeasurandVoltage),
		string(types.MeasurandCurrentImport),
		string(types.MeasurandPowerActiveImport),
		string(types.MeasurandEnergyActiveImportInterval),
		string(types.MeasurandSoC),
	}, ",")

	return []core.ConfigurationKey{
		{Key: AuthorizeRemoteTxRequests.String(), Readonly: false, Value: ptr("true")},
		{Key: ClockAlignedDataInterval.String(), Readonly: false, Value: ptr("0")},
		{Key: ConnectionTimeOut.String(), Readonly: false, Value: ptr("60")},
		{Key: GetConfigurationMaxKeys.String(), Readonly: false, Value: ptr("100")},
		{Key: HeartbeatInterval.String(), Readonly: false, Value: ptr("60")},
		{Key: LocalPreAuthorize.String(), Readonly: false, Value: ptr("false")},
		{Key: MeterValuesAlignedData.String(), Readonly: false, Value: ptr("true")},
		{Key: MeterValuesSampledData.String(), Readonly: false, Value: ptr(standardMeasurands)},
		{Key: MeterValueSampleInterval.String(), Readonly: false, Value: ptr("20")},
		{Key: NumberOfConnectors.String(), Readonly: true, Value: ptr("1")},
		{Key: ResetRetries.String(), Readonly: false, Value: ptr("3")},
		{Key: ConnectorPhaseRotation.String(), Readonly: true, Value: ptr("Unknown")},
		{Key: StopTransactionOnEVSideDisconnect.String(), Readonly: false, Value: ptr("true")},
		{Key: StopTransactionOnInvalidId.String(), Readonly: false, Value: ptr("true")},
		{Key: StopTxnAlignedData.String(), Readonly: false, Value: ptr(standardMeasurands)},
		{Key: StopTxnSampledData.String(), Readonly: false, Value: ptr(standardMeasurands)},
		{Key: SupportedFeatureProfiles.String(), Readonly: true, Value: ptr("Core")},
		{Key: TransactionMessageAttempts.String(), Readonly: false, Value: ptr("3")},
		{Key: TransactionMessageRetryInterval.String(), Readonly: false, Value: ptr("30")},
		{Key: UnlockConnectorOnEVSideDisconnect.String(), Readonly: false, Value: ptr("true")},
	}
}

// DefaultLocalAuthConfiguration returns the default LocalAuthListManagement
// profile configuration keys.
func DefaultLocalAuthConfiguration() []core.ConfigurationKey {
	return []core.ConfigurationKey{
		{Key: LocalAuthListEnabled.String(), Readonly: false, Value: ptr("true")},
		{Key: LocalAuthListMaxLength.String(), Readonly: true, Value: ptr("100")},
		{Key: SendLocalListMaxLength.String(), Readonly: true, Value: ptr("100")},
	}
}

// DefaultSmartChargingConfiguration returns the default SmartCharging
// profile configuration keys.
func DefaultSmartChargingConfiguration() []core.ConfigurationKey {
	return []core.ConfigurationKey{
		{Key: ChargeProfileMaxStackLevel.String(), Readonly: true, Value: ptr("5")},
		{Key: ChargingScheduleAllowedChargingRateUnit.String(), Readonly: true, Value: ptr("Current,Power")},
		{Key: ChargingScheduleMaxPeriods.String(), Readonly: true, Value: ptr("6")},
		{Key: MaxChargingProfilesInstalled.String(), Readonly: true, Value: ptr("5")},
	}
}

// DefaultFirmwareConfiguration returns the default FirmwareManagement
// profile configuration keys.
func DefaultFirmwareConfiguration() []core.ConfigurationKey {
	return []core.ConfigurationKey{
		{Key: SupportedFileTransferProtocols.String(), Readonly: true, Value: ptr("HTTP,HTTPS,FTP,FTPS,SFTP")},
	}
}

// DefaultISO15118Configuration returns default configuration keys for the
// ISO15118ProfileName sentinel (D4a). This function is NET NEW: the source
// has no ISO15118 defaults at all (only the unreachable "todo" mandatory-key
// wiring fixed in keys.go). Its two MandatoryISO15118Keys entries
// (ISO15118PnCEnabled, ContractValidationOffline) are what make
// NewManagerV16(cfg, ISO15118ProfileName) validate; the remaining entries
// are included for completeness. Every value here was INFERRED (no
// upstream source to carry it from) — treat these as reasonable, documented
// starting points, not spec-mandated defaults, and override them via
// SetConfiguration/UpdateKey as needed.
func DefaultISO15118Configuration() []core.ConfigurationKey {
	return []core.ConfigurationKey{
		{Key: CentralContractValidationAllowed.String(), Readonly: false, Value: ptr("true")},
		{Key: CertificateSignedMaxChainSize.String(), Readonly: true, Value: ptr("5")},
		{Key: CertSigningWaitMinimum.String(), Readonly: false, Value: ptr("300")},
		{Key: CertSigningRepeatTimes.String(), Readonly: false, Value: ptr("3")},
		{Key: CertificateStoreMaxLength.String(), Readonly: true, Value: ptr("10")},
		{Key: ContractValidationOffline.String(), Readonly: false, Value: ptr("true")},
		{Key: ISO15118PnCEnabled.String(), Readonly: false, Value: ptr("false")},
	}
}
