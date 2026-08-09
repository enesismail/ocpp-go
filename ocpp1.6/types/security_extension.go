package types

import "gopkg.in/go-playground/validator.v9"

// Indicates the type of the signed certificate that is returned.
// When omitted the certificate is used for both the 15118 connection (if implemented) and the Charging Station to CSMS connection.
// This field is required when a typeOfCertificate was included in the SignCertificateRequest that requested this certificate to be signed AND both the 15118 connection and the Charging Station connection are implemented.
type CertificateSigningUse string

const (
	ChargingStationCert CertificateSigningUse = "ChargingStationCertificate"
)

func isValidCertificateSigningUse(fl validator.FieldLevel) bool {
	status := CertificateSigningUse(fl.Field().String())
	switch status {
	case ChargingStationCert:
		return true
	default:
		return false
	}
}

// Generic Status
type GenericStatus string

const (
	GenericStatusAccepted GenericStatus = "Accepted"
	GenericStatusRejected GenericStatus = "Rejected"
)

func isValidGenericStatus(fl validator.FieldLevel) bool {
	status := GenericStatus(fl.Field().String())
	switch status {
	case GenericStatusAccepted, GenericStatusRejected:
		return true
	default:
		return false
	}
}

// StatusInfo is an element providing more information about the message status.
type StatusInfo struct {
	ReasonCode     string `json:"reasonCode" validate:"required,max=20"`                 // A predefined code for the reason why the status is returned in this response. The string is case- insensitive.
	AdditionalInfo string `json:"additionalInfo,omitempty" validate:"omitempty,max=512"` // Additional text to provide detailed information.
}

// NewStatusInfo creates a StatusInfo struct.
// If no additional info need to be set, an empty string may be passed.
func NewStatusInfo(reasonCode string, additionalInfo string) *StatusInfo {
	return &StatusInfo{ReasonCode: reasonCode, AdditionalInfo: additionalInfo}
}

// Indicates the type of the requested certificate.
// It is used in GetInstalledCertificateIdsRequest and InstallCertificateRequest messages.
type CertificateUse string

const (
	CentralSystemRootCertificate CertificateUse = "CentralSystemRootCertificate"
	ManufacturerRootCertificate  CertificateUse = "ManufacturerRootCertificate"
)

func isValidCertificateUse(fl validator.FieldLevel) bool {
	use := CertificateUse(fl.Field().String())
	switch use {
	case CentralSystemRootCertificate, ManufacturerRootCertificate:
		return true
	default:
		return false
	}
}

// Hash Algorithms
type HashAlgorithmType string

const (
	SHA256 HashAlgorithmType = "SHA256"
	SHA384 HashAlgorithmType = "SHA384"
	SHA512 HashAlgorithmType = "SHA512"
)

func isValidHashAlgorithm(fl validator.FieldLevel) bool {
	algorithm := HashAlgorithmType(fl.Field().String())
	switch algorithm {
	case SHA256, SHA384, SHA512:
		return true
	default:
		return false
	}
}

// CertificateHashDataType
type CertificateHashData struct {
	HashAlgorithm  HashAlgorithmType `json:"hashAlgorithm" validate:"required,hashAlgorithm16"`
	IssuerNameHash string            `json:"issuerNameHash" validate:"required,max=128"`
	IssuerKeyHash  string            `json:"issuerKeyHash" validate:"required,max=128"`
	SerialNumber   string            `json:"serialNumber" validate:"required,max=40"`
}

// init registers this file's own validator locally: hashAlgorithm16 is
// CertificateHashData's field, and this file is the type's sole owner, so
// the registration lives beside it rather than in types.go's central init
// (the pattern every non-types-package file in ocpp1.6 already follows for
// its own tags, e.g. delete_certificate.go's deleteCertificateStatus16).
// Deliberately local: Validate is one process-wide validator shared with
// ocpp2.0.1, and this field's tag previously named a validator this package
// never registered — it worked only while ocpp2.0.1 happened to register
// the same bare name. Owning hashAlgorithm16 here removes that coupling.
func init() {
	_ = Validate.RegisterValidation("hashAlgorithm16", isValidHashAlgorithm)
}
