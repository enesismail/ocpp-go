package ocpp2_test

// PR-E1c (tasks/e1c-context-aware-send.md) RED-FIRST test suite — OCPP 2.0.1
// charging-station (client) facade.
//
// RED-FIRST discipline: every test below references the PR-E1c surface exactly
// as the spec names it. Against today's codebase:
//   - ChargingStation.SendRequestCtx does not exist
//   - ChargingStation.SendRequestAsyncCtx does not exist
//
// This file is EXPECTED to fail compilation — that IS the intended red
// state pinning the PR-E1c contract.
//
// Spec tests implemented: 4.
// Test 8 (prefer-response fast-path) moves to the white-box helper test in
// ocpp2.0.1/context_awaitresult_test.go (package ocpp2) — the e2e version
// was a false-pass (canceled after result already arrived).
//
// NOTE: the 2.0.1 facade uses separate responseHandler/errorHandler channels,
// so the ordering between a raced late response and a cancel is nondeterministic
// on 2.0.1 (exactly-one still holds via CompleteRequest). Tests here do not
// assume response-before-cancel ordering.

import (
	"context"
	"errors"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/enesismail/ocpp-go/ocpp"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/availability"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/types"
	"github.com/enesismail/ocpp-go/ocppj"
)

// Test4ChargingStationSendRequestCtxCanceledContext tests that
// cs.SendRequestCtx with a mid-flight-canceled context returns a *ocpp.Error
// matching context.Canceled. (spec test 4 — 2.0.1 facade)
//
// The test dispatches a Heartbeat request, blocks the response, cancels the
// ctx, and asserts the error matches context.Canceled (via the new ctx.Done()
// arm in the facade's sync-send select, which extends E1b's stopC select).
func (suite *OcppV2TestSuite) Test4ChargingStationSendRequestCtxCanceledContext() {
	t := suite.T()
	wsURL := "someUrl"

	// Write succeeds but never forwards a response, so SendRequestCtx blocks
	// on its internal select until the ctx fires.
	suite.mockWsClient.On("Start", mock.AnythingOfType("string")).Return(nil)
	suite.mockWsClient.On("Write", mock.Anything).Return(nil)
	suite.mockWsClient.On("Stop").Return()
	suite.mockWsClient.On("IsConnected").Return(false)

	err := suite.chargingStation.Start(wsURL)
	require.Nil(t, err)
	defer suite.chargingStation.Stop()

	ctx, cancel := context.WithCancel(context.Background())

	type sendResult struct {
		resp ocpp.Response
		err  error
	}
	resultC := make(chan sendResult, 1)
	go func() {
		// PR-E1c: cs.SendRequestCtx(ctx, request) — the new ctx-first API.
		resp, err := suite.chargingStation.SendRequestCtx(ctx, availability.NewHeartbeatRequest())
		resultC <- sendResult{resp: resp, err: err}
	}()

	// Give the request time to be dispatched.
	time.Sleep(100 * time.Millisecond)

	// Cancel the context — this must unblock SendRequestCtx.
	cancel()

	select {
	case result := <-resultC:
		assert.Nil(t, result.resp, "should not return a response on ctx cancel")
		require.Error(t, result.err, "must return an error on ctx cancel")
		assert.True(t, errors.Is(result.err, context.Canceled),
			"error must match context.Canceled, got %v", result.err)
		assert.True(t, errors.Is(result.err, ocppj.ErrRequestCanceled),
			"error must match ErrRequestCanceled, got %v", result.err)
	case <-time.After(e1bBound):
		t.Fatal("SendRequestCtx did not unblock on ctx cancel")
	}
}

// Test4ChargingStationSendRequestCtxNilCtxBehavesAsBackground verifies that
// passing nil ctx to SendRequestCtx behaves as context.Background().
// (spec test 4 — nil ctx == Background)
func (suite *OcppV2TestSuite) Test4ChargingStationSendRequestCtxNilCtxBehavesAsBackground() {
	t := suite.T()
	wsID := "test_id"
	wsURL := "someUrl"
	channel := NewMockWebSocket(wsID)

	heartbeatConf := availability.NewHeartbeatResponse(*types.NewDateTime(time.Now()))

	csmsAvailabilityHandler := &MockCSMSAvailabilityHandler{}
	csmsAvailabilityHandler.On("OnHeartbeat", mock.AnythingOfType("string"), mock.Anything).Return(heartbeatConf, nil)

	setupDefaultCSMSHandlers(suite, expectedCSMSOptions{clientId: wsID, forwardWrittenMessage: true}, csmsAvailabilityHandler)
	setupDefaultChargingStationHandlers(suite, expectedChargingStationOptions{serverUrl: wsURL, clientId: wsID, createChannelOnStart: true, channel: channel, forwardWrittenMessage: true})

	suite.csms.Start(8887, "somePath")
	err := suite.chargingStation.Start(wsURL)
	require.Nil(t, err)

	// PR-E1c: SendRequestCtx with nil ctx must behave as Background().
	resp, err := suite.chargingStation.SendRequestCtx(nil, availability.NewHeartbeatRequest())
	require.NoError(t, err)
	require.NotNil(t, resp)
	_, ok := resp.(*availability.HeartbeatResponse)
	assert.True(t, ok, "SendRequestCtx(nil, ...) must return a typed confirmation")

	suite.csms.Stop()
}

// Test4ChargingStationSendRequestUntouched verifies that the existing
// SendRequest (ctx-less) still works unchanged on the 2.0.1 facade — it
// delegates to SendRequestCtx with context.Background().
// (spec test 4 — SendRequest/typed helpers unchanged)
//
// This is the 2.0.1 mirror of 1.6's Test4ChargePointSendRequestUntouched.
func (suite *OcppV2TestSuite) Test4ChargingStationSendRequestUntouched() {
	t := suite.T()
	wsID := "test_id"
	wsURL := "someUrl"
	channel := NewMockWebSocket(wsID)

	heartbeatConf := availability.NewHeartbeatResponse(*types.NewDateTime(time.Now()))

	csmsAvailabilityHandler := &MockCSMSAvailabilityHandler{}
	csmsAvailabilityHandler.On("OnHeartbeat", mock.AnythingOfType("string"), mock.Anything).Return(heartbeatConf, nil)

	setupDefaultCSMSHandlers(suite, expectedCSMSOptions{clientId: wsID, forwardWrittenMessage: true}, csmsAvailabilityHandler)
	setupDefaultChargingStationHandlers(suite, expectedChargingStationOptions{serverUrl: wsURL, clientId: wsID, createChannelOnStart: true, channel: channel, forwardWrittenMessage: true})

	suite.csms.Start(8887, "somePath")
	err := suite.chargingStation.Start(wsURL)
	require.Nil(t, err)

	// The existing SendRequest (not SendRequestCtx) must still work.
	resp, err := suite.chargingStation.SendRequest(availability.NewHeartbeatRequest())
	require.NoError(t, err)
	require.NotNil(t, resp)
	_, ok := resp.(*availability.HeartbeatResponse)
	assert.True(t, ok, "SendRequest must still return a typed confirmation")

	suite.csms.Stop()
}
