package ocpp2_test

import (
	"fmt"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/enesismail/ocpp-go/ocpp2.0.1/authorization"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/types"
)

// Test
func (suite *OcppV2TestSuite) TestAuthorizeRequestValidation() {
	t := suite.T()
	var requestTable = []GenericTestEntry{
		{authorization.AuthorizeRequest{Certificate: "deadc0de", IdToken: types.IDToken{IDToken: "1234", Type: types.IDTokenTypeKeyCode, AdditionalInfo: []types.AdditionalInfo{{AdditionalIDToken: "0000", Type: "someType"}}}, CertificateHashData: []types.OCSPRequestData{{SerialNumber: "serial0", HashAlgorithm: types.HashAlgorithmTypeSHA256, IssuerNameHash: "hash0", IssuerKeyHash: "hash1", ResponderURL: "www.someurl.com"}}}, true},
		{authorization.AuthorizeRequest{Certificate: "deadc0de", IdToken: types.IDToken{IDToken: "1234", Type: types.IDTokenTypeKeyCode, AdditionalInfo: []types.AdditionalInfo{{AdditionalIDToken: "0000", Type: "someType"}}}}, true},
		{authorization.AuthorizeRequest{IdToken: types.IDToken{IDToken: "1234", Type: types.IDTokenTypeKeyCode, AdditionalInfo: []types.AdditionalInfo{{AdditionalIDToken: "0000", Type: "someType"}}}, CertificateHashData: []types.OCSPRequestData{{SerialNumber: "serial0", HashAlgorithm: types.HashAlgorithmTypeSHA256, IssuerNameHash: "hash0", IssuerKeyHash: "hash1", ResponderURL: "www.someurl.com"}}}, true},
		{authorization.AuthorizeRequest{IdToken: types.IDToken{IDToken: "1234", Type: types.IDTokenTypeKeyCode, AdditionalInfo: []types.AdditionalInfo{{AdditionalIDToken: "0000", Type: "someType"}}}, CertificateHashData: []types.OCSPRequestData{}}, true},
		{authorization.AuthorizeRequest{IdToken: types.IDToken{IDToken: "1234", Type: types.IDTokenTypeKeyCode, AdditionalInfo: []types.AdditionalInfo{{AdditionalIDToken: "0000", Type: "someType"}}}}, true},
		{authorization.AuthorizeRequest{}, false},
		{authorization.AuthorizeRequest{Certificate: newLongString(5501), IdToken: types.IDToken{IDToken: "1234", Type: types.IDTokenTypeKeyCode, AdditionalInfo: []types.AdditionalInfo{{AdditionalIDToken: "0000", Type: "someType"}}}}, false},
		{authorization.AuthorizeRequest{Certificate: "deadc0de", IdToken: types.IDToken{Type: types.IDTokenTypeKeyCode, AdditionalInfo: []types.AdditionalInfo{{AdditionalIDToken: "0000", Type: "someType"}}}}, false},
		{authorization.AuthorizeRequest{Certificate: "deadc0de", IdToken: types.IDToken{Type: types.IDTokenTypeKeyCode, AdditionalInfo: []types.AdditionalInfo{{AdditionalIDToken: "0000", Type: "someType"}}}, CertificateHashData: []types.OCSPRequestData{{HashAlgorithm: types.HashAlgorithmTypeSHA256, IssuerNameHash: "hash0", IssuerKeyHash: "hash1"}}}, false},
		{authorization.AuthorizeRequest{Certificate: "deadc0de", IdToken: types.IDToken{Type: types.IDTokenTypeKeyCode, AdditionalInfo: []types.AdditionalInfo{{AdditionalIDToken: "0000", Type: "someType"}}}, CertificateHashData: []types.OCSPRequestData{{SerialNumber: "s0", HashAlgorithm: types.HashAlgorithmTypeSHA256, IssuerNameHash: "h0", IssuerKeyHash: "h0.1"}, {SerialNumber: "s1", HashAlgorithm: types.HashAlgorithmTypeSHA256, IssuerNameHash: "h1", IssuerKeyHash: "h1.1"}, {SerialNumber: "s2", HashAlgorithm: types.HashAlgorithmTypeSHA256, IssuerNameHash: "h2", IssuerKeyHash: "h2.1"}, {SerialNumber: "s3", HashAlgorithm: types.HashAlgorithmTypeSHA256, IssuerNameHash: "h3", IssuerKeyHash: "h3.1"}, {SerialNumber: "s4", HashAlgorithm: types.HashAlgorithmTypeSHA256, IssuerNameHash: "h4", IssuerKeyHash: "h4.1"}}}, false},
	}
	ExecuteGenericTestTable(t, requestTable)
}

func (suite *OcppV2TestSuite) TestAuthorizeConfirmationValidation() {
	t := suite.T()
	var confirmationTable = []GenericTestEntry{
		{authorization.AuthorizeResponse{CertificateStatus: authorization.CertificateStatusAccepted, IdTokenInfo: types.IDTokenInfo{Status: types.AuthorizationStatusTypeAccepted}}, true},
		{authorization.AuthorizeResponse{CertificateStatus: authorization.CertificateStatusAccepted, IdTokenInfo: types.IDTokenInfo{Status: types.AuthorizationStatusTypeAccepted}}, true},
		{authorization.AuthorizeResponse{IdTokenInfo: types.IDTokenInfo{Status: types.AuthorizationStatusTypeAccepted}}, true},
		{authorization.AuthorizeResponse{}, false},
		{authorization.AuthorizeResponse{CertificateStatus: "invalidCertificateStatus", IdTokenInfo: types.IDTokenInfo{Status: types.AuthorizationStatusTypeAccepted}}, false},
		{authorization.AuthorizeResponse{CertificateStatus: authorization.CertificateStatusAccepted, IdTokenInfo: types.IDTokenInfo{Status: "invalidTokenInfoStatus"}}, false},
	}
	ExecuteGenericTestTable(t, confirmationTable)
}

func (suite *OcppV2TestSuite) TestAuthorizeE2EMocked() {
	t := suite.T()
	wsId := "test_id"
	messageId := defaultMessageId
	wsUrl := "someUrl"
	certificate := "deadc0de"
	additionalInfo := types.AdditionalInfo{AdditionalIDToken: "at1", Type: "some"}
	idToken := types.IDToken{IDToken: "tok1", Type: types.IDTokenTypeKeyCode, AdditionalInfo: []types.AdditionalInfo{additionalInfo}}
	certHashData := types.OCSPRequestData{HashAlgorithm: types.HashAlgorithmTypeSHA256, IssuerNameHash: "h0", IssuerKeyHash: "h0.1", SerialNumber: "s0", ResponderURL: "http://www.test.org"}
	status := types.AuthorizationStatusTypeAccepted
	certificateStatus := authorization.CertificateStatusAccepted
	requestJson := fmt.Sprintf(`[2,"%v","%v",{"certificate":"%v","idToken":{"additionalInfo":[{"additionalIdToken":"%v","type":"%v"}],"idToken":"%v","type":"%v"},"iso15118CertificateHashData":[{"hashAlgorithm":"%v","issuerNameHash":"%v","issuerKeyHash":"%v","serialNumber":"%v","responderURL":"%v"}]}]`,
		messageId, authorization.AuthorizeFeatureName, certificate, additionalInfo.AdditionalIDToken, additionalInfo.Type, idToken.IDToken, idToken.Type, certHashData.HashAlgorithm, certHashData.IssuerNameHash, certHashData.IssuerKeyHash, certHashData.SerialNumber, certHashData.ResponderURL)
	responseJson := fmt.Sprintf(`[3,"%v",{"certificateStatus":"%v","idTokenInfo":{"status":"%v"}}]`,
		messageId, certificateStatus, status)
	authorizeConfirmation := authorization.NewAuthorizationResponse(types.IDTokenInfo{Status: status})
	authorizeConfirmation.CertificateStatus = certificateStatus
	requestRaw := []byte(requestJson)
	responseRaw := []byte(responseJson)
	channel := NewMockWebSocket(wsId)

	handler := &MockCSMSAuthorizationHandler{}
	handler.On("OnAuthorize", mock.AnythingOfType("string"), mock.Anything).Return(authorizeConfirmation, nil).Run(func(args mock.Arguments) {
		request := args.Get(1).(*authorization.AuthorizeRequest)
		assert.Equal(t, certificate, request.Certificate)
		assert.Equal(t, idToken.IDToken, request.IdToken.IDToken)
		assert.Equal(t, idToken.Type, request.IdToken.Type)
		require.Len(t, request.IdToken.AdditionalInfo, 1)
		assert.Equal(t, idToken.AdditionalInfo[0].AdditionalIDToken, request.IdToken.AdditionalInfo[0].AdditionalIDToken)
		assert.Equal(t, idToken.AdditionalInfo[0].Type, request.IdToken.AdditionalInfo[0].Type)
		require.Len(t, request.CertificateHashData, 1)
		assert.Equal(t, certHashData.HashAlgorithm, request.CertificateHashData[0].HashAlgorithm)
		assert.Equal(t, certHashData.IssuerNameHash, request.CertificateHashData[0].IssuerNameHash)
		assert.Equal(t, certHashData.IssuerKeyHash, request.CertificateHashData[0].IssuerKeyHash)
		assert.Equal(t, certHashData.SerialNumber, request.CertificateHashData[0].SerialNumber)
		assert.Equal(t, certHashData.ResponderURL, request.CertificateHashData[0].ResponderURL)
	})
	setupDefaultCSMSHandlers(suite, expectedCSMSOptions{clientId: wsId, rawWrittenMessage: responseRaw, forwardWrittenMessage: true}, handler)
	setupDefaultChargingStationHandlers(suite, expectedChargingStationOptions{serverUrl: wsUrl, clientId: wsId, createChannelOnStart: true, channel: channel, rawWrittenMessage: requestRaw, forwardWrittenMessage: true})
	// Run Test
	suite.csms.Start(8887, "somePath")
	err := suite.chargingStation.Start(wsUrl)
	require.Nil(t, err)
	response, err := suite.chargingStation.Authorize(idToken.IDToken, idToken.Type, func(request *authorization.AuthorizeRequest) {
		request.IdToken.AdditionalInfo = []types.AdditionalInfo{additionalInfo}
		request.Certificate = certificate
		request.CertificateHashData = []types.OCSPRequestData{certHashData}
	})
	require.Nil(t, err)
	require.NotNil(t, response)
	assert.Equal(t, certificateStatus, response.CertificateStatus)
	assert.Equal(t, status, response.IdTokenInfo.Status)
}

func (suite *OcppV2TestSuite) TestAuthorizeInvalidEndpoint() {
	messageId := defaultMessageId
	certificate := "deadc0de"
	additionalInfo := types.AdditionalInfo{AdditionalIDToken: "at1", Type: "some"}
	idToken := types.IDToken{IDToken: "tok1", Type: types.IDTokenTypeKeyCode, AdditionalInfo: []types.AdditionalInfo{additionalInfo}}
	certHashData := types.OCSPRequestData{HashAlgorithm: types.HashAlgorithmTypeSHA256, IssuerNameHash: "h0", IssuerKeyHash: "h0.1", SerialNumber: "s0", ResponderURL: "http://www.test.org"}
	authorizeRequest := authorization.NewAuthorizationRequest(idToken.IDToken, idToken.Type)
	requestJson := fmt.Sprintf(`[2,"%v","%v",{"certificate":"%v","idToken":{"additionalInfo":[{"additionalIdToken":"%v","type":"%v"}],"idToken":"%v","type":"%v"},"iso15118CertificateHashData":[{"hashAlgorithm":"%v","issuerNameHash":"%v","issuerKeyHash":"%v","serialNumber":"%v","responderURL":"%v"}]}]`,
		messageId, authorization.AuthorizeFeatureName, certificate, additionalInfo.AdditionalIDToken, additionalInfo.Type, idToken.IDToken, idToken.Type, certHashData.HashAlgorithm, certHashData.IssuerNameHash, certHashData.IssuerKeyHash, certHashData.SerialNumber, certHashData.ResponderURL)
	testUnsupportedRequestFromCentralSystem(suite, authorizeRequest, requestJson, messageId)
}
