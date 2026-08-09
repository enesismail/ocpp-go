package types_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/go-playground/validator.v9"

	"github.com/enesismail/ocpp-go/ocpp1.6/types"
)

// These tests run in a binary whose import graph contains ocpp1.6/types but
// NOT ocpp2.0.1: the hashAlgorithm16 validator they exercise must therefore
// be registered by this package itself. That is the property under guard —
// before the local registration existed, the field's tag named a validator
// only ocpp2.0.1's types package registered, so validating any certificate
// hash in a 1.6-only build panicked ("Undefined validation function")
// instead of validating.

func validCertificateHashData() types.CertificateHashData {
	return types.CertificateHashData{
		HashAlgorithm:  types.SHA256,
		IssuerNameHash: "1a2b3c",
		IssuerKeyHash:  "4d5e6f",
		SerialNumber:   "1234567890",
	}
}

func fieldErrors(t *testing.T, err error) validator.ValidationErrors {
	t.Helper()
	require.Error(t, err)
	verr, ok := err.(validator.ValidationErrors)
	require.True(t, ok, "expected validator.ValidationErrors, got %T: %v", err, err)
	return verr
}

func TestCertificateHashDataAcceptsEveryDeclaredAlgorithm(t *testing.T) {
	for _, algorithm := range []types.HashAlgorithmType{types.SHA256, types.SHA384, types.SHA512} {
		data := validCertificateHashData()
		data.HashAlgorithm = algorithm
		assert.NoError(t, types.Validate.Struct(data), "hash algorithm %v must validate", algorithm)
	}
}

func TestCertificateHashDataRejectsUnknownAlgorithm(t *testing.T) {
	data := validCertificateHashData()
	data.HashAlgorithm = "MD5"
	verr := fieldErrors(t, types.Validate.Struct(data))
	require.Len(t, verr, 1)
	assert.Equal(t, "HashAlgorithm", verr[0].Field())
	assert.Equal(t, "hashAlgorithm16", verr[0].Tag())
}

func TestCertificateHashDataRequiresAlgorithm(t *testing.T) {
	data := validCertificateHashData()
	data.HashAlgorithm = ""
	verr := fieldErrors(t, types.Validate.Struct(data))
	require.Len(t, verr, 1)
	assert.Equal(t, "HashAlgorithm", verr[0].Field())
	assert.Equal(t, "required", verr[0].Tag())
}
