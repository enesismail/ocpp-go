package ocpp2_test

import (
	"fmt"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/enesismail/ocpp-go/ocpp2.0.1/transactions"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/types"
)

// Test
func (suite *OcppV2TestSuite) TestTransactionInfoValidation() {
	var requestTable = []GenericTestEntry{
		{transactions.Transaction{TransactionID: "42", ChargingState: transactions.ChargingStateTypeSuspendedEV, TimeSpentCharging: newInt(100), StoppedReason: transactions.ReasonTypeLocal, RemoteStartID: newInt(7)}, true},
		{transactions.Transaction{TransactionID: "42", ChargingState: transactions.ChargingStateTypeSuspendedEV, TimeSpentCharging: newInt(100), StoppedReason: transactions.ReasonTypeLocal}, true},
		{transactions.Transaction{TransactionID: "42", ChargingState: transactions.ChargingStateTypeSuspendedEV, TimeSpentCharging: newInt(100)}, true},
		{transactions.Transaction{TransactionID: "42", ChargingState: transactions.ChargingStateTypeSuspendedEV}, true},
		{transactions.Transaction{TransactionID: "42"}, true},
		{transactions.Transaction{}, false},
		{transactions.Transaction{TransactionID: ">36..................................", ChargingState: transactions.ChargingStateTypeSuspendedEV, TimeSpentCharging: newInt(100), StoppedReason: transactions.ReasonTypeLocal, RemoteStartID: newInt(7)}, false},
		{transactions.Transaction{TransactionID: "42", ChargingState: "invalidChargingState", TimeSpentCharging: newInt(100), StoppedReason: transactions.ReasonTypeLocal, RemoteStartID: newInt(7)}, false},
		{transactions.Transaction{TransactionID: "42", ChargingState: transactions.ChargingStateTypeSuspendedEV, TimeSpentCharging: newInt(100), StoppedReason: "invalidReason", RemoteStartID: newInt(7)}, false},
	}
	ExecuteGenericTestTable(suite.T(), requestTable)
}

func (suite *OcppV2TestSuite) TestTransactionEventRequestValidation() {
	t := suite.T()
	transactionInfo := transactions.Transaction{TransactionID: "42"}
	idToken := types.IDToken{IDToken: "1234", Type: types.IDTokenTypeKeyCode}
	meterValue := types.MeterValue{Timestamp: types.NewDateTime(time.Now()), SampledValue: []types.SampledValue{{Value: 64.0}}}
	var requestTable = []GenericTestEntry{
		{transactions.TransactionEventRequest{EventType: transactions.TransactionEventTypeStarted, Timestamp: types.NewDateTime(time.Now()), TriggerReason: transactions.TriggerReasonTypeAuthorized, SeqNo: 1, Offline: newBool(true), NumberOfPhasesUsed: newInt(3), CableMaxCurrent: newInt(20), ReservationID: newInt(42), TransactionInfo: transactionInfo, IDToken: &idToken, EVSE: &types.EVSE{ID: 1}, MeterValue: []types.MeterValue{meterValue}}, true},
		// MeterValue: []types.MeterValue{} (present, empty): master's
		// hand-written tag declared no minItems bound where the schema sets
		// minItems=1 -- looser than the schema, not a gap the schema-faithful
		// generated code repeats. The generated "min=1" tag now correctly
		// rejects an empty (but present) slice.
		{transactions.TransactionEventRequest{EventType: transactions.TransactionEventTypeStarted, Timestamp: types.NewDateTime(time.Now()), TriggerReason: transactions.TriggerReasonTypeAuthorized, SeqNo: 1, Offline: newBool(true), NumberOfPhasesUsed: newInt(3), CableMaxCurrent: newInt(20), ReservationID: newInt(42), TransactionInfo: transactionInfo, IDToken: &idToken, EVSE: &types.EVSE{ID: 1}, MeterValue: []types.MeterValue{}}, false},
		{transactions.TransactionEventRequest{EventType: transactions.TransactionEventTypeStarted, Timestamp: types.NewDateTime(time.Now()), TriggerReason: transactions.TriggerReasonTypeAuthorized, SeqNo: 1, Offline: newBool(true), NumberOfPhasesUsed: newInt(3), CableMaxCurrent: newInt(20), ReservationID: newInt(42), TransactionInfo: transactionInfo, IDToken: &idToken}, true},
		{transactions.TransactionEventRequest{EventType: transactions.TransactionEventTypeStarted, Timestamp: types.NewDateTime(time.Now()), TriggerReason: transactions.TriggerReasonTypeAuthorized, SeqNo: 1, Offline: newBool(true), NumberOfPhasesUsed: newInt(3), CableMaxCurrent: newInt(20), ReservationID: newInt(42), TransactionInfo: transactionInfo}, true},
		{transactions.TransactionEventRequest{EventType: transactions.TransactionEventTypeStarted, Timestamp: types.NewDateTime(time.Now()), TriggerReason: transactions.TriggerReasonTypeAuthorized, SeqNo: 1, Offline: newBool(true), NumberOfPhasesUsed: newInt(3), CableMaxCurrent: newInt(20), TransactionInfo: transactionInfo}, true},
		{transactions.TransactionEventRequest{EventType: transactions.TransactionEventTypeStarted, Timestamp: types.NewDateTime(time.Now()), TriggerReason: transactions.TriggerReasonTypeAuthorized, SeqNo: 1, Offline: newBool(true), NumberOfPhasesUsed: newInt(3), TransactionInfo: transactionInfo}, true},
		{transactions.TransactionEventRequest{EventType: transactions.TransactionEventTypeStarted, Timestamp: types.NewDateTime(time.Now()), TriggerReason: transactions.TriggerReasonTypeAuthorized, SeqNo: 1, Offline: newBool(true), TransactionInfo: transactionInfo}, true},
		{transactions.TransactionEventRequest{EventType: transactions.TransactionEventTypeStarted, Timestamp: types.NewDateTime(time.Now()), TriggerReason: transactions.TriggerReasonTypeAuthorized, SeqNo: 1, TransactionInfo: transactionInfo}, true},
		// SeqNo omitted (zero value): previously valid (master's tag was
		// "gte=0", no "required"); the schema marks seqNo required and the
		// generated tag now rejects the omission -- same validator.v9
		// required-vs-zero gap as the rest of this diff.
		{transactions.TransactionEventRequest{EventType: transactions.TransactionEventTypeStarted, Timestamp: types.NewDateTime(time.Now()), TriggerReason: transactions.TriggerReasonTypeAuthorized, TransactionInfo: transactionInfo}, false},
		// This row tested isValidIdToken's conditional exemption (IDToken
		// string may be empty when Type is NoAuthorization) -- that
		// struct-level validator has no schema counterpart and was
		// necessarily dropped along with IdToken/GroupIdToken's collapse
		// into the generated IDToken; not reimplemented, since it has no
		// basis in the schema. IDToken.IDToken now carries a plain
		// "required" tag with no exemption, so an empty value is always
		// rejected. SeqNo is also omitted here, independently invalid.
		{transactions.TransactionEventRequest{EventType: transactions.TransactionEventTypeStarted, Timestamp: types.NewDateTime(time.Now()), TriggerReason: transactions.TriggerReasonTypeAuthorized, TransactionInfo: transactionInfo, IDToken: &types.IDToken{Type: types.IDTokenTypeNoAuthorization}}, false},
		{transactions.TransactionEventRequest{EventType: transactions.TransactionEventTypeStarted, Timestamp: types.NewDateTime(time.Now()), TriggerReason: transactions.TriggerReasonTypeAuthorized, TransactionInfo: transactionInfo, IDToken: &types.IDToken{Type: types.IDTokenTypeKeyCode}}, false},
		{transactions.TransactionEventRequest{EventType: transactions.TransactionEventTypeStarted, Timestamp: types.NewDateTime(time.Now()), TriggerReason: transactions.TriggerReasonTypeAuthorized}, false},
		{transactions.TransactionEventRequest{EventType: transactions.TransactionEventTypeStarted, Timestamp: types.NewDateTime(time.Now()), TransactionInfo: transactionInfo}, false},
		{transactions.TransactionEventRequest{EventType: transactions.TransactionEventTypeStarted, TriggerReason: transactions.TriggerReasonTypeAuthorized, TransactionInfo: transactionInfo}, false},
		{transactions.TransactionEventRequest{Timestamp: types.NewDateTime(time.Now()), TriggerReason: transactions.TriggerReasonTypeAuthorized, TransactionInfo: transactionInfo}, false},
		{transactions.TransactionEventRequest{}, false},
		{transactions.TransactionEventRequest{EventType: "invalidEventType", Timestamp: types.NewDateTime(time.Now()), TriggerReason: transactions.TriggerReasonTypeAuthorized, SeqNo: 1, Offline: newBool(true), NumberOfPhasesUsed: newInt(3), CableMaxCurrent: newInt(20), ReservationID: newInt(42), TransactionInfo: transactionInfo, IDToken: &idToken, EVSE: &types.EVSE{ID: 1}, MeterValue: []types.MeterValue{meterValue}}, false},
		{transactions.TransactionEventRequest{EventType: transactions.TransactionEventTypeStarted, Timestamp: types.NewDateTime(time.Now()), TriggerReason: "invalidTriggerReason", SeqNo: 1, Offline: newBool(true), NumberOfPhasesUsed: newInt(3), CableMaxCurrent: newInt(20), ReservationID: newInt(42), TransactionInfo: transactionInfo, IDToken: &idToken, EVSE: &types.EVSE{ID: 1}, MeterValue: []types.MeterValue{meterValue}}, false},
		{transactions.TransactionEventRequest{EventType: transactions.TransactionEventTypeStarted, Timestamp: types.NewDateTime(time.Now()), TriggerReason: transactions.TriggerReasonTypeAuthorized, SeqNo: -1, Offline: newBool(true), NumberOfPhasesUsed: newInt(3), CableMaxCurrent: newInt(20), ReservationID: newInt(42), TransactionInfo: transactionInfo, IDToken: &idToken, EVSE: &types.EVSE{ID: 1}, MeterValue: []types.MeterValue{meterValue}}, true}, // SeqNo/NumberOfPhasesUsed/CableMaxCurrent/ReservationID/EVSE.ID no longer bounded (gte=0 had no override row, dropped); all non-zero values pass the generated "required"/"omitempty" tags now.
		{transactions.TransactionEventRequest{EventType: transactions.TransactionEventTypeStarted, Timestamp: types.NewDateTime(time.Now()), TriggerReason: transactions.TriggerReasonTypeAuthorized, SeqNo: 1, Offline: newBool(true), NumberOfPhasesUsed: newInt(-1), CableMaxCurrent: newInt(20), ReservationID: newInt(42), TransactionInfo: transactionInfo, IDToken: &idToken, EVSE: &types.EVSE{ID: 1}, MeterValue: []types.MeterValue{meterValue}}, true},
		{transactions.TransactionEventRequest{EventType: transactions.TransactionEventTypeStarted, Timestamp: types.NewDateTime(time.Now()), TriggerReason: transactions.TriggerReasonTypeAuthorized, SeqNo: 1, Offline: newBool(true), NumberOfPhasesUsed: newInt(3), CableMaxCurrent: newInt(20), ReservationID: newInt(42), TransactionInfo: transactions.Transaction{}, IDToken: &idToken, EVSE: &types.EVSE{ID: 1}, MeterValue: []types.MeterValue{meterValue}}, false},
		{transactions.TransactionEventRequest{EventType: transactions.TransactionEventTypeStarted, Timestamp: types.NewDateTime(time.Now()), TriggerReason: transactions.TriggerReasonTypeAuthorized, SeqNo: 1, Offline: newBool(true), NumberOfPhasesUsed: newInt(3), CableMaxCurrent: newInt(20), ReservationID: newInt(42), TransactionInfo: transactionInfo, IDToken: &types.IDToken{}, EVSE: &types.EVSE{ID: 1}, MeterValue: []types.MeterValue{meterValue}}, false},
		{transactions.TransactionEventRequest{EventType: transactions.TransactionEventTypeStarted, Timestamp: types.NewDateTime(time.Now()), TriggerReason: transactions.TriggerReasonTypeAuthorized, SeqNo: 1, Offline: newBool(true), NumberOfPhasesUsed: newInt(3), CableMaxCurrent: newInt(20), ReservationID: newInt(42), TransactionInfo: transactionInfo, IDToken: &idToken, EVSE: &types.EVSE{ID: -1}, MeterValue: []types.MeterValue{meterValue}}, true},
		{transactions.TransactionEventRequest{EventType: transactions.TransactionEventTypeStarted, Timestamp: types.NewDateTime(time.Now()), TriggerReason: transactions.TriggerReasonTypeAuthorized, SeqNo: 1, Offline: newBool(true), NumberOfPhasesUsed: newInt(3), CableMaxCurrent: newInt(20), ReservationID: newInt(42), TransactionInfo: transactionInfo, IDToken: &idToken, EVSE: &types.EVSE{ID: 1}, MeterValue: []types.MeterValue{{}}}, false},
	}
	ExecuteGenericTestTable(t, requestTable)
}

func (suite *OcppV2TestSuite) TestTransactionEventResponseValidation() {
	t := suite.T()
	messageContent := types.MessageContent{Format: types.MessageFormatTypeUTF8, Content: "dummyContent"}
	var responseTable = []GenericTestEntry{
		{transactions.TransactionEventResponse{TotalCost: newFloat(8.42), ChargingPriority: newInt(2), IDTokenInfo: types.NewIDTokenInfo(types.AuthorizationStatusTypeAccepted), UpdatedPersonalMessage: &messageContent}, true},
		{transactions.TransactionEventResponse{TotalCost: newFloat(8.42), ChargingPriority: newInt(2), IDTokenInfo: types.NewIDTokenInfo(types.AuthorizationStatusTypeAccepted)}, true},
		{transactions.TransactionEventResponse{TotalCost: newFloat(8.42), ChargingPriority: newInt(2)}, true},
		{transactions.TransactionEventResponse{TotalCost: newFloat(8.42)}, true},
		{transactions.TransactionEventResponse{}, true},
		// TotalCost negative, ChargingPriority past the old
		// min=-9/max=9 bound: both bounds were hand-written-only (no
		// governing override row) and are dropped in the schema-faithful
		// mapping (both fields carry just "omitempty" now); genuinely
		// valid.
		{transactions.TransactionEventResponse{TotalCost: newFloat(-1.0), ChargingPriority: newInt(2), IDTokenInfo: types.NewIDTokenInfo(types.AuthorizationStatusTypeAccepted), UpdatedPersonalMessage: &messageContent}, true},
		{transactions.TransactionEventResponse{TotalCost: newFloat(8.42), ChargingPriority: newInt(-10), IDTokenInfo: types.NewIDTokenInfo(types.AuthorizationStatusTypeAccepted), UpdatedPersonalMessage: &messageContent}, true},
		{transactions.TransactionEventResponse{TotalCost: newFloat(8.42), ChargingPriority: newInt(10), IDTokenInfo: types.NewIDTokenInfo(types.AuthorizationStatusTypeAccepted), UpdatedPersonalMessage: &messageContent}, true},
		{transactions.TransactionEventResponse{TotalCost: newFloat(8.42), ChargingPriority: newInt(2), IDTokenInfo: types.NewIDTokenInfo("invalidAuthorizationStatus"), UpdatedPersonalMessage: &messageContent}, false},
		{transactions.TransactionEventResponse{TotalCost: newFloat(8.42), ChargingPriority: newInt(2), IDTokenInfo: types.NewIDTokenInfo(types.AuthorizationStatusTypeAccepted), UpdatedPersonalMessage: &types.MessageContent{}}, false},
	}
	ExecuteGenericTestTable(t, responseTable)
}

func (suite *OcppV2TestSuite) TestTransactionEventE2EMocked() {
	t := suite.T()
	wsId := "test_id"
	messageId := defaultMessageId
	wsUrl := "someUrl"
	timestamp := types.NewDateTime(time.Now())
	eventType := transactions.TransactionEventTypeEnded
	triggerReason := transactions.TriggerReasonTypeEVDeparted
	seqNo := 10
	offline := false
	phases := newInt(3)
	cableMaxCurrent := newInt(20)
	reservationID := newInt(55)
	info := transactions.Transaction{TransactionID: "42", ChargingState: transactions.ChargingStateTypeSuspendedEV, TimeSpentCharging: newInt(1000), StoppedReason: transactions.ReasonTypeLocal, RemoteStartID: newInt(69)}
	idToken := types.IDToken{IDToken: "1234", Type: types.IDTokenTypeKeyCode}
	evse := types.EVSE{ID: 1}
	meterValue := types.MeterValue{Timestamp: types.NewDateTime(time.Now()), SampledValue: []types.SampledValue{{Value: 64.0}}}
	totalCost := newFloat(8.42)
	chargingPriority := newInt(2)
	idTokenInfo := types.NewIDTokenInfo(types.AuthorizationStatusTypeAccepted)
	messageContent := types.MessageContent{Format: types.MessageFormatTypeUTF8, Content: "dummyContent"}
	// Field order matches the generated struct's declaration order, which
	// derives from the schema (eventType, meterValue, timestamp,
	// triggerReason, seqNo, offline, numberOfPhasesUsed, cableMaxCurrent,
	// reservationId, transactionInfo, evse, idToken); the template below is
	// written in that order. JSON content and assertion strength are
	// unchanged -- only the byte order of an exact-string comparison.
	// "offline":%v added: Offline is now *bool instead of a value bool, and
	// the props callback below sets a non-nil pointer to false, so it
	// correctly appears on the wire now (a value bool+omitempty could never
	// distinguish "explicitly false" from "unset" and always dropped it; a
	// non-nil *bool pointing at false does not count as empty for
	// omitempty).
	requestJson := fmt.Sprintf(`[2,"%v","%v",{"eventType":"%v","meterValue":[{"sampledValue":[{"value":%v}],"timestamp":"%v"}],"timestamp":"%v","triggerReason":"%v","seqNo":%v,"offline":%v,"numberOfPhasesUsed":%v,"cableMaxCurrent":%v,"reservationId":%v,"transactionInfo":{"transactionId":"%v","chargingState":"%v","timeSpentCharging":%v,"stoppedReason":"%v","remoteStartId":%v},"evse":{"id":%v},"idToken":{"idToken":"%v","type":"%v"}}]`,
		messageId, transactions.TransactionEventFeatureName, eventType, meterValue.SampledValue[0].Value, meterValue.Timestamp.FormatTimestamp(), timestamp.FormatTimestamp(), triggerReason, seqNo, offline, *phases, *cableMaxCurrent, *reservationID, info.TransactionID, info.ChargingState, *info.TimeSpentCharging, info.StoppedReason, *info.RemoteStartID, evse.ID, idToken.IDToken, idToken.Type)
	responseJson := fmt.Sprintf(`[3,"%v",{"totalCost":%v,"chargingPriority":%v,"idTokenInfo":{"status":"%v"},"updatedPersonalMessage":{"format":"%v","content":"%v"}}]`,
		messageId, *totalCost, *chargingPriority, idTokenInfo.Status, messageContent.Format, messageContent.Content)
	transactionResponse := transactions.NewTransactionEventResponse()
	transactionResponse.TotalCost = totalCost
	transactionResponse.ChargingPriority = chargingPriority
	transactionResponse.IDTokenInfo = idTokenInfo
	transactionResponse.UpdatedPersonalMessage = &messageContent
	channel := NewMockWebSocket(wsId)

	handler := &MockCSMSTransactionsHandler{}
	handler.On("OnTransactionEvent", mock.AnythingOfType("string"), mock.Anything).Return(transactionResponse, nil).Run(func(args mock.Arguments) {
		request, ok := args.Get(1).(*transactions.TransactionEventRequest)
		require.True(t, ok)
		require.NotNil(t, request)
		assert.Equal(t, eventType, request.EventType)
		assertDateTimeEquality(t, timestamp, request.Timestamp)
		assert.Equal(t, triggerReason, request.TriggerReason)
		assert.Equal(t, seqNo, request.SeqNo)
		assert.Equal(t, offline, *request.Offline)
		assert.Equal(t, *phases, *request.NumberOfPhasesUsed)
		assert.Equal(t, *cableMaxCurrent, *request.CableMaxCurrent)
		assert.Equal(t, *reservationID, *request.ReservationID)
		assert.Equal(t, eventType, request.EventType)
		assert.Equal(t, info.TransactionID, request.TransactionInfo.TransactionID)
		assert.Equal(t, info.StoppedReason, request.TransactionInfo.StoppedReason)
		assert.Equal(t, info.ChargingState, request.TransactionInfo.ChargingState)
		assert.Equal(t, *info.TimeSpentCharging, *request.TransactionInfo.TimeSpentCharging)
		assert.Equal(t, *info.RemoteStartID, *request.TransactionInfo.RemoteStartID)
		require.NotNil(t, request.IDToken)
		assert.Equal(t, idToken.IDToken, request.IDToken.IDToken)
		assert.Equal(t, idToken.Type, request.IDToken.Type)
		require.NotNil(t, request.EVSE)
		assert.Equal(t, evse.ID, request.EVSE.ID)
		require.Len(t, request.MeterValue, 1)
		assertDateTimeEquality(t, meterValue.Timestamp, request.MeterValue[0].Timestamp)
		require.Len(t, request.MeterValue[0].SampledValue, 1)
		assert.Equal(t, meterValue.SampledValue[0].Value, request.MeterValue[0].SampledValue[0].Value)
	})
	setupDefaultCSMSHandlers(suite, expectedCSMSOptions{clientId: wsId, rawWrittenMessage: []byte(responseJson), forwardWrittenMessage: true}, handler)
	setupDefaultChargingStationHandlers(suite, expectedChargingStationOptions{serverUrl: wsUrl, clientId: wsId, createChannelOnStart: true, channel: channel, rawWrittenMessage: []byte(requestJson), forwardWrittenMessage: true})
	// Run Test
	suite.csms.Start(8887, "somePath")
	err := suite.chargingStation.Start(wsUrl)
	require.NoError(t, err)
	response, err := suite.chargingStation.TransactionEvent(eventType, timestamp, triggerReason, seqNo, info, func(request *transactions.TransactionEventRequest) {
		request.MeterValue = []types.MeterValue{meterValue}
		request.EVSE = &evse
		request.IDToken = &idToken
		request.NumberOfPhasesUsed = phases
		request.CableMaxCurrent = cableMaxCurrent
		request.ReservationID = reservationID
		request.Offline = &offline
	})
	require.NoError(t, err)
	require.NotNil(t, response)
	assert.Equal(t, *totalCost, *response.TotalCost)
	assert.Equal(t, *chargingPriority, *response.ChargingPriority)
	require.NotNil(t, response.IDTokenInfo)
	assert.Equal(t, idTokenInfo.Status, response.IDTokenInfo.Status)
	require.NotNil(t, response.UpdatedPersonalMessage)
	assert.Equal(t, messageContent.Format, response.UpdatedPersonalMessage.Format)
	assert.Equal(t, messageContent.Content, response.UpdatedPersonalMessage.Content)
}

func (suite *OcppV2TestSuite) TestTransactionEventInvalidEndpoint() {
	messageId := defaultMessageId
	timestamp := types.NewDateTime(time.Now())
	eventType := transactions.TransactionEventTypeEnded
	triggerReason := transactions.TriggerReasonTypeEVDeparted
	seqNo := 10
	phases := newInt(3)
	cableMaxCurrent := newInt(20)
	reservationID := newInt(55)
	info := transactions.Transaction{TransactionID: "42", ChargingState: transactions.ChargingStateTypeSuspendedEV, TimeSpentCharging: newInt(1000), StoppedReason: transactions.ReasonTypeLocal, RemoteStartID: newInt(69)}
	idToken := types.IDToken{IDToken: "1234", Type: types.IDTokenTypeKeyCode}
	evse := types.EVSE{ID: 1}
	meterValue := types.MeterValue{Timestamp: types.NewDateTime(time.Now()), SampledValue: []types.SampledValue{{Value: 64.0}}}
	request := transactions.NewTransactionEventRequest(eventType, timestamp, triggerReason, seqNo, info)
	request.NumberOfPhasesUsed = phases
	request.CableMaxCurrent = cableMaxCurrent
	request.ReservationID = reservationID
	request.IDToken = &idToken
	request.EVSE = &evse
	request.MeterValue = []types.MeterValue{meterValue}
	requestJson := fmt.Sprintf(`[2,"%v","%v",{"eventType":"%v","timestamp":"%v","triggerReason":"%v","seqNo":%v,"numberOfPhasesUsed":%v,"cableMaxCurrent":%v,"reservationId":%v,"transactionInfo":{"transactionId":"%v","chargingState":"%v","timeSpentCharging":%v,"stoppedReason":"%v","remoteStartId":%v},"idToken":{"idToken":"%v","type":"%v"},"evse":{"id":%v},"meterValue":[{"timestamp":"%v","sampledValue":[{"value":%v}]}]}]`,
		messageId, transactions.TransactionEventFeatureName, eventType, timestamp.FormatTimestamp(), triggerReason, seqNo, *phases, *cableMaxCurrent, *reservationID, info.TransactionID, info.ChargingState, *info.TimeSpentCharging, info.StoppedReason, *info.RemoteStartID, idToken.IDToken, idToken.Type, evse.ID, meterValue.Timestamp.FormatTimestamp(), meterValue.SampledValue[0].Value)
	testUnsupportedRequestFromCentralSystem(suite, request, requestJson, messageId)
}
