// Contains common and shared data types between OCPP 2.0 messages.
package types

import (
	"gopkg.in/go-playground/validator.v9"

	"github.com/enesismail/ocpp-go/ocppj"
)

const (
	V2Subprotocol   = "ocpp2.0"
	V201Subprotocol = "ocpp2.0.1"
)

type PropertyViolation struct {
	error
	Property string
}

func (e *PropertyViolation) Error() string {
	return ""
}

// CertificateHashDataChain
type CertificateHashDataChain struct {
	CertificateType          CertificateUse        `json:"certificateType" validate:"required,certificateUse"`
	CertificateHashData      CertificateHashData   `json:"certificateHashData" validate:"required"`
	ChildCertificateHashData []CertificateHashData `json:"childCertificateHashData,omitempty" validate:"omitempty,dive"`
}

// Certificate15118EVStatus
type Certificate15118EVStatus string

const (
	Certificate15188EVStatusAccepted Certificate15118EVStatus = "Accepted"
	Certificate15118EVStatusFailed   Certificate15118EVStatus = "Failed"
)

func isValidCertificate15118EVStatus(fl validator.FieldLevel) bool {
	status := Certificate15118EVStatus(fl.Field().String())
	switch status {
	case Certificate15188EVStatusAccepted, Certificate15118EVStatusFailed:
		return true
	default:
		return false
	}
}

// Indicates the type of the requested certificate.
// It is used in GetInstalledCertificateIdsRequest and InstallCertificateRequest messages.
type CertificateUse string

const (
	V2GRootCertificate          CertificateUse = "V2GRootCertificate"
	MORootCertificate           CertificateUse = "MORootCertificate"
	CSOSubCA1                   CertificateUse = "CSOSubCA1"
	CSOSubCA2                   CertificateUse = "CSOSubCA2"
	CSMSRootCertificate         CertificateUse = "CSMSRootCertificate"
	V2GCertificateChain         CertificateUse = "V2GCertificateChain"
	ManufacturerRootCertificate CertificateUse = "ManufacturerRootCertificate"
)

func isValidCertificateUse(fl validator.FieldLevel) bool {
	use := CertificateUse(fl.Field().String())
	switch use {
	case V2GRootCertificate, MORootCertificate, CSOSubCA1, CSOSubCA2, CSMSRootCertificate, V2GCertificateChain, ManufacturerRootCertificate:
		return true
	default:
		return false
	}
}

//TODO: remove SignatureMethod (obsolete from 2.0.1 onwards)

// Enumeration of the cryptographic method used to create the digital signature.
// The list is expected to grow in future OCPP releases to allow other signature methods used by Smart Meters.
type SignatureMethod string

const (
	SignatureECDSAP256SHA256 SignatureMethod = "ECDSAP256SHA256" // The encoded data is hashed with the SHA-256 hash function, and the hash value is then signed with the ECDSA algorithm using the NIST P-256 elliptic curve.
	SignatureECDSAP384SHA384 SignatureMethod = "ECDSAP384SHA384" // The encoded data is hashed with the SHA-384 hash function, and the hash value is then signed with the ECDSA algorithm using the NIST P-384 elliptic curve.
	SignatureECDSA192SHA256  SignatureMethod = "ECDSA192SHA256"  // The encoded data is hashed with the SHA-256 hash function, and the hash value is then signed with the ECDSA algorithm using a 192-bit elliptic curve.
)

func isValidSignatureMethod(fl validator.FieldLevel) bool {
	signature := SignatureMethod(fl.Field().String())
	switch signature {
	case SignatureECDSA192SHA256, SignatureECDSAP256SHA256, SignatureECDSAP384SHA384:
		return true
	default:
		return false
	}
}

//TODO: remove EncodingMethod (obsolete from 2.0.1 onwards)

// Enumeration of the method used to encode the meter value into binary data before applying the digital signature algorithm.
// If the EncodingMethod is set to Other, the CSMS MAY try to determine the encoding method from the encodedMeterValue field.
type EncodingMethod string

const (
	EncodingOther              EncodingMethod = "Other"                // Encoding method is not included in the enumeration.
	EncodingDLMSMessage        EncodingMethod = "DLMS Message"         // The data is encoded in a digitally signed DLMS message, as described in the DLMS Green Book 8.
	EncodingCOSEMProtectedData EncodingMethod = "COSEM Protected Data" // The data is encoded according to the COSEM data protection methods, as described in the DLMS Blue Book 12.
	EncodingEDL                EncodingMethod = "EDL"                  // The data is encoded in the format used by EDL meters.
)

func isValidEncodingMethod(fl validator.FieldLevel) bool {
	encoding := EncodingMethod(fl.Field().String())
	switch encoding {
	case EncodingCOSEMProtectedData, EncodingEDL, EncodingDLMSMessage, EncodingOther:
		return true
	default:
		return false
	}
}

// Validator used for validating all OCPP 2.0 messages.
// Any additional custom validations must be added to this object for automatic validation.
var Validate = ocppj.Validate

func init() {
	_ = Validate.RegisterValidation("signatureMethod", isValidSignatureMethod)
	_ = Validate.RegisterValidation("encodingMethod", isValidEncodingMethod)
	_ = Validate.RegisterValidation("certificateUse", isValidCertificateUse)
	_ = Validate.RegisterValidation("15118EVCertificate", isValidCertificate15118EVStatus)
}
