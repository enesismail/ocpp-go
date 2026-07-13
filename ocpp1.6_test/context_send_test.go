package ocpp16_test

// PR-E1c (tasks/e1c-context-aware-send.md) RED-FIRST test suite — OCPP 1.6
// charge-point (client) facade.
//
// RED-FIRST discipline: every test below references the PR-E1c surface exactly
// as the spec names it. Against today's codebase:
//   - ChargePoint.SendRequestCtx does not exist
//   - ChargePoint.SendRequestAsyncCtx does not exist
//
// This file is EXPECTED to fail compilation — that IS the intended red
// state pinning the PR-E1c contract.
//
// Spec tests implemented: 4.
// Test 8 (prefer-response fast-path) moves to the white-box helper test in
// ocpp1.6/context_awaitresult_test.go (package ocpp16) — the e2e version
// was a false-pass (canceled after result already arrived).

import (
	"context"
	"errors"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/enesismail/ocpp-go/ocpp"
	"github.com/enesismail/ocpp-go/ocpp1.6/core"
	"github.com/enesismail/ocpp-go/ocpp1.6/types"
	"github.com/enesismail/ocpp-go/ocppj"
)

// Test4ChargePointSendRequestCtxCanceledContext tests that cp.SendRequestCtx
// with a mid-flight-canceled context returns a *ocpp.Error matching
// context.Canceled. (spec test 4 — 1.6 facade)
//
// The test dispatches a Heartbeat request, blocks the response, cancels the
// ctx, and asserts the error matches context.Canceled (via the new ctx.Done()
// arm in the facade's sync-send select).
func (suite *OcppV16TestSuite) Test4ChargePointSendRequestCtxCanceledContext() {
	t := suite.T()
	wsURL := "someUrl"

	// Write succeeds but never forwards a response, so SendRequestCtx blocks
	// on its internal select until the ctx fires.
	suite.mockWsClient.On("Start", mock.AnythingOfType("string")).Return(nil)
	suite.mockWsClient.On("Write", mock.Anything).Return(nil)
	suite.mockWsClient.On("Stop").Return()
	suite.mockWsClient.On("IsConnected").Return(false)

	err := suite.chargePoint.Start(wsURL)
	require.Nil(t, err)
	defer suite.chargePoint.Stop()

	ctx, cancel := context.WithCancel(context.Background())

	type sendResult struct {
		resp ocpp.Response
		err  error
	}
	resultC := make(chan sendResult, 1)
	go func() {
		// PR-E1c: cp.SendRequestCtx(ctx, request) — the new ctx-first API.
		resp, err := suite.chargePoint.SendRequestCtx(ctx, core.NewHeartbeatRequest())
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
	case <-time.After(panicWaitTimeout):
		t.Fatal("SendRequestCtx did not unblock on ctx cancel")
	}
}

// Test4ChargePointSendRequestUntouched verifies that the existing
// SendRequest (ctx-less) still works unchanged — it delegates to
// SendRequestCtx with context.Background(). (spec test 4 — nil ctx)
//
// This is a regression guard: the typed helpers and existing callers
// must not break.
func (suite *OcppV16TestSuite) Test4ChargePointSendRequestUntouched() {
	t := suite.T()
	wsID := "test_id"
	wsURL := "someUrl"
	channel := NewMockWebSocket(wsID)

	currentTime := types.NewDateTime(time.Now())
	bootConf := core.NewBootNotificationConfirmation(currentTime, 60, core.RegistrationStatusAccepted)

	coreListener := &MockCentralSystemCoreListener{}
	coreListener.On("OnBootNotification", mock.AnythingOfType("string"), mock.Anything).Return(bootConf, nil)

	setupDefaultCentralSystemHandlers(suite, coreListener, expectedCentralSystemOptions{clientId: wsID, forwardWrittenMessage: true})
	setupDefaultChargePointHandlers(suite, nil, expectedChargePointOptions{serverUrl: wsURL, clientId: wsID, createChannelOnStart: true, channel: channel, forwardWrittenMessage: true})

	suite.centralSystem.Start(8887, "somePath")
	err := suite.chargePoint.Start(wsURL)
	require.Nil(t, err)

	// The existing SendRequest (not SendRequestCtx) must still work.
	resp, err := suite.chargePoint.SendRequest(core.NewBootNotificationRequest("model1", "ABL"))
	require.NoError(t, err)
	require.NotNil(t, resp)
	_, ok := resp.(*core.BootNotificationConfirmation)
	assert.True(t, ok, "SendRequest must still return a typed confirmation")

	suite.centralSystem.Stop()
}

// Test4ChargePointSendRequestCtxNilCtxBehavesAsBackground verifies that
// passing nil ctx to SendRequestCtx behaves as context.Background().
// (spec test 4 — nil ctx == Background)
func (suite *OcppV16TestSuite) Test4ChargePointSendRequestCtxNilCtxBehavesAsBackground() {
	t := suite.T()
	wsID := "test_id"
	wsURL := "someUrl"
	channel := NewMockWebSocket(wsID)

	currentTime := types.NewDateTime(time.Now())
	bootConf := core.NewBootNotificationConfirmation(currentTime, 60, core.RegistrationStatusAccepted)

	coreListener := &MockCentralSystemCoreListener{}
	coreListener.On("OnBootNotification", mock.AnythingOfType("string"), mock.Anything).Return(bootConf, nil)

	setupDefaultCentralSystemHandlers(suite, coreListener, expectedCentralSystemOptions{clientId: wsID, forwardWrittenMessage: true})
	setupDefaultChargePointHandlers(suite, nil, expectedChargePointOptions{serverUrl: wsURL, clientId: wsID, createChannelOnStart: true, channel: channel, forwardWrittenMessage: true})

	suite.centralSystem.Start(8887, "somePath")
	err := suite.chargePoint.Start(wsURL)
	require.Nil(t, err)

	// PR-E1c: SendRequestCtx with nil ctx must behave as Background().
	resp, err := suite.chargePoint.SendRequestCtx(nil, core.NewBootNotificationRequest("model1", "ABL"))
	require.NoError(t, err)
	require.NotNil(t, resp)
	_, ok := resp.(*core.BootNotificationConfirmation)
	assert.True(t, ok, "SendRequestCtx(nil, ...) must return a typed confirmation")

	suite.centralSystem.Stop()
}

// Test 8 (prefer-response fast-path) moves to the white-box helper test in
// ocpp1.6/context_awaitresult_test.go (package ocpp16) — the e2e version
// was a false-pass (canceled after result already arrived; the middle
// SendRequestAsyncCtx scenario was dead code).
