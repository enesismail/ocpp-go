package ocpp2_test

import (
	"fmt"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/enesismail/ocpp-go/ocpp2.0.1/provisioning"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/types"
)

// Tests
func (suite *OcppV2TestSuite) TestBootNotificationRequestValidation() {
	t := suite.T()
	var requestTable = []GenericTestEntry{
		{provisioning.BootNotificationRequest{Reason: provisioning.BootReasonTypePowerUp, ChargingStation: provisioning.ChargingStation{SerialNumber: "number", Model: "test", VendorName: "test", FirmwareVersion: "version", Modem: &provisioning.Modem{Iccid: "test", Imsi: "test"}}}, true},
		{provisioning.BootNotificationRequest{Reason: provisioning.BootReasonTypePowerUp, ChargingStation: provisioning.ChargingStation{SerialNumber: "number", Model: "test", VendorName: "test", FirmwareVersion: "version", Modem: &provisioning.Modem{Iccid: "test"}}}, true},
		{provisioning.BootNotificationRequest{Reason: provisioning.BootReasonTypePowerUp, ChargingStation: provisioning.ChargingStation{SerialNumber: "number", Model: "test", VendorName: "test", FirmwareVersion: "version", Modem: &provisioning.Modem{Imsi: "test"}}}, true},
		{provisioning.BootNotificationRequest{Reason: provisioning.BootReasonTypePowerUp, ChargingStation: provisioning.ChargingStation{SerialNumber: "number", Model: "test", VendorName: "test", FirmwareVersion: "version", Modem: &provisioning.Modem{}}}, true},
		{provisioning.BootNotificationRequest{Reason: provisioning.BootReasonTypePowerUp, ChargingStation: provisioning.ChargingStation{SerialNumber: "number", Model: "test", VendorName: "test", FirmwareVersion: "version"}}, true},
		{provisioning.BootNotificationRequest{Reason: provisioning.BootReasonTypePowerUp, ChargingStation: provisioning.ChargingStation{SerialNumber: "number", Model: "test", VendorName: "test"}}, true},
		{provisioning.BootNotificationRequest{Reason: provisioning.BootReasonTypePowerUp, ChargingStation: provisioning.ChargingStation{Model: "test", VendorName: "test"}}, true},
		{provisioning.BootNotificationRequest{Reason: provisioning.BootReasonTypePowerUp, ChargingStation: provisioning.ChargingStation{Model: "test"}}, false},
		{provisioning.BootNotificationRequest{Reason: provisioning.BootReasonTypePowerUp, ChargingStation: provisioning.ChargingStation{VendorName: "test"}}, false},
		{provisioning.BootNotificationRequest{ChargingStation: provisioning.ChargingStation{Model: "test", VendorName: "test"}}, false},
		{provisioning.BootNotificationRequest{Reason: provisioning.BootReasonTypePowerUp, ChargingStation: provisioning.ChargingStation{Model: ">20..................", VendorName: "test"}}, false},
		{provisioning.BootNotificationRequest{Reason: provisioning.BootReasonTypePowerUp, ChargingStation: provisioning.ChargingStation{Model: "test", VendorName: ">50................................................"}}, false},
		{provisioning.BootNotificationRequest{Reason: provisioning.BootReasonTypePowerUp, ChargingStation: provisioning.ChargingStation{SerialNumber: ">25.......................", Model: "test", VendorName: "test"}}, false},
		{provisioning.BootNotificationRequest{Reason: provisioning.BootReasonTypePowerUp, ChargingStation: provisioning.ChargingStation{Model: "test", VendorName: "test", FirmwareVersion: ">50................................................"}}, false},
		{provisioning.BootNotificationRequest{Reason: provisioning.BootReasonTypePowerUp, ChargingStation: provisioning.ChargingStation{Model: "test", VendorName: "test", Modem: &provisioning.Modem{Iccid: ">20.................."}}}, false},
		{provisioning.BootNotificationRequest{Reason: provisioning.BootReasonTypePowerUp, ChargingStation: provisioning.ChargingStation{Model: "test", VendorName: "test", Modem: &provisioning.Modem{Imsi: ">20.................."}}}, false},
		{provisioning.BootNotificationRequest{Reason: "invalidReason", ChargingStation: provisioning.ChargingStation{Model: "test", VendorName: "test"}}, false},
	}
	ExecuteGenericTestTable(t, requestTable)
}

func (suite *OcppV2TestSuite) TestBootNotificationConfirmationValidation() {
	t := suite.T()
	var confirmationTable = []GenericTestEntry{
		{provisioning.BootNotificationResponse{CurrentTime: types.NewDateTime(time.Now()), Interval: 60, Status: provisioning.RegistrationStatusTypeAccepted}, true},
		// Interval omitted (zero value): was accepted under master's hand-written tag
		// (validate:"gte=0", no "required"), which diverged from the schema's own
		// "interval" required:true. The generated field carries "required,gte=0"
		// (a bound-preserving override applies here), matching the schema
		// faithfully -- a zero Interval is now correctly rejected. Semantic
		// change, not mechanical.
		{provisioning.BootNotificationResponse{CurrentTime: types.NewDateTime(time.Now()), Status: provisioning.RegistrationStatusTypeAccepted}, false},
		{provisioning.BootNotificationResponse{CurrentTime: types.NewDateTime(time.Now()), Interval: -1, Status: provisioning.RegistrationStatusTypeAccepted}, false},
		{provisioning.BootNotificationResponse{CurrentTime: types.NewDateTime(time.Now()), Interval: 60, Status: "invalidRegistrationStatus"}, false},
		{provisioning.BootNotificationResponse{CurrentTime: types.NewDateTime(time.Now()), Interval: 60}, false},
		{provisioning.BootNotificationResponse{Interval: 60, Status: provisioning.RegistrationStatusTypeAccepted}, false},
	}
	ExecuteGenericTestTable(t, confirmationTable)
}

func (suite *OcppV2TestSuite) TestBootNotificationE2EMocked() {
	t := suite.T()
	wsId := "test_id"
	messageId := "1234"
	wsUrl := "someUrl"
	interval := 60
	reason := provisioning.BootReasonTypePowerUp
	chargePointModel := "model1"
	chargePointVendor := "ABL"
	registrationStatus := provisioning.RegistrationStatusTypeAccepted
	currentTime := types.NewDateTime(time.Now())
	// Field order (chargingStation before reason) matches the generated
	// struct's declaration order, which derives from the schema; the
	// literal below is written in that order. JSON content and assertion
	// strength are unchanged -- only the byte order of an exact-string
	// comparison.
	requestJson := fmt.Sprintf(`[2,"%v","%v",{"chargingStation":{"model":"%v","vendorName":"%v"},"reason":"%v"}]`, messageId, provisioning.BootNotificationFeatureName, chargePointModel, chargePointVendor, reason)
	responseJson := fmt.Sprintf(`[3,"%v",{"currentTime":"%v","interval":%v,"status":"%v"}]`, messageId, currentTime.FormatTimestamp(), interval, registrationStatus)
	bootNotificationConfirmation := provisioning.NewBootNotificationResponse(currentTime, interval, registrationStatus)
	channel := NewMockWebSocket(wsId)

	handler := &MockCSMSProvisioningHandler{}
	handler.On("OnBootNotification", mock.AnythingOfType("string"), mock.Anything).Return(bootNotificationConfirmation, nil).Run(func(args mock.Arguments) {
		request := args.Get(1).(*provisioning.BootNotificationRequest)
		assert.Equal(t, reason, request.Reason)
		assert.Equal(t, chargePointVendor, request.ChargingStation.VendorName)
		assert.Equal(t, chargePointModel, request.ChargingStation.Model)
	})
	setupDefaultCSMSHandlers(suite, expectedCSMSOptions{clientId: wsId, rawWrittenMessage: []byte(responseJson), forwardWrittenMessage: true}, handler)
	setupDefaultChargingStationHandlers(suite, expectedChargingStationOptions{serverUrl: wsUrl, clientId: wsId, createChannelOnStart: true, channel: channel, rawWrittenMessage: []byte(requestJson), forwardWrittenMessage: true})
	// Run test
	suite.csms.Start(8887, "somePath")
	err := suite.chargingStation.Start(wsUrl)
	require.Nil(t, err)
	confirmation, err := suite.chargingStation.BootNotification(reason, chargePointModel, chargePointVendor)
	require.Nil(t, err)
	require.NotNil(t, confirmation)
	assert.Equal(t, registrationStatus, confirmation.Status)
	assert.Equal(t, interval, confirmation.Interval)
	assertDateTimeEquality(t, currentTime, confirmation.CurrentTime)
}

func (suite *OcppV2TestSuite) TestBootNotificationInvalidEndpoint() {
	messageId := defaultMessageId
	chargePointModel := "model1"
	chargePointVendor := "ABL"
	reason := provisioning.BootReasonTypePowerUp
	bootNotificationRequest := provisioning.NewBootNotificationRequest(provisioning.ChargingStation{Model: chargePointModel, VendorName: chargePointVendor}, reason)
	// Field order (chargingStation before reason) matches the generated
	// struct's declaration order, which derives from the schema; the
	// literal below is written in that order. JSON content and assertion
	// strength are unchanged -- only the byte order of an exact-string
	// comparison.
	requestJson := fmt.Sprintf(`[2,"%v","%v",{"chargingStation":{"model":"%v","vendorName":"%v"},"reason":"%v"}]`, messageId, provisioning.BootNotificationFeatureName, chargePointModel, chargePointVendor, reason)
	testUnsupportedRequestFromCentralSystem(suite, bootNotificationRequest, requestJson, messageId)
}
