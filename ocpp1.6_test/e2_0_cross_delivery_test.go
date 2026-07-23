package ocpp16_test

// E2-0 (tasks/e2-0-requestid-keyed-callbacks.md) facade test suite.
//
// Written red-first against the pre-E2-0 API; now green. Tests 1 and 2 pin the
// read goroutine via the internal/testhooks response seams; test 3 (WireError)
// and the cascade variant exercise the ID-keyed error/response routing.
//
// Spec tests implemented: 1, 2, 3 (+ different-type cascade).

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/enesismail/ocpp-go/internal/testhooks"
	"github.com/enesismail/ocpp-go/ocpp"
	"github.com/enesismail/ocpp-go/ocpp1.6/core"
	"github.com/enesismail/ocpp-go/ocpp1.6/types"
	"github.com/enesismail/ocpp-go/ocppj"
	"github.com/enesismail/ocpp-go/ws"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Test 1 — Client cross-delivery regression (§0)
// ============================================================================

// TestE2_0_ClientCrossDeliveryRegression reproduces the client-side callback
// mis-pairing documented in spec §0. The read goroutine is pinned between
// CompleteRequest and the cp.incoming send via internal/testhooks.ChargePointResponse.
// The hook gives the pump time to process a queued expired-ctx request, whose
// cancel then enters cp.incoming ahead of the earlier response. With ID-keyed
// dequeue each caller receives ITS OWN result; before E2-0 the callbacks were swapped.
func (suite *OcppV16TestSuite) TestE2_0_ClientCrossDeliveryRegression() {
	t := suite.T()

	// --- Sequential message IDs so we can address each request individually ---
	var idSeq int
	seqGen := func() string {
		idSeq++
		return fmt.Sprintf("e2-0-cr-%d", idSeq)
	}
	ocppj.SetMessageIdGenerator(seqGen)
	defer func() {
		ocppj.SetMessageIdGenerator(suite.messageIdGenerator.generateId)
	}()

	writeC := make(chan []byte, 8)
	startStandaloneChargePoint(suite, writeC)
	// Stop the charge point on every exit path (see local_transport_test.go):
	// startStandaloneChargePoint only stubs Start/Write, so stub Stop/IsConnected
	// here too, and IsConnected must be false so Stop() returns immediately.
	suite.mockWsClient.On("IsConnected").Return(false)
	suite.mockWsClient.On("Stop").Return()
	defer suite.chargePoint.Stop()

	// --- Install the response-hook seam ---
	// The hook is invoked at the top of the SetResponseHandler closure in
	// v16.go, after CompleteRequest and before the cp.incoming send. It is nil in
	// production; the test sets it to orchestrate the exact interleaving that
	// triggers the §0 inversion.
	hookCalled := make(chan string, 1) // delivers requestId
	hookProceed := make(chan struct{})
	// openGate is idempotent and deferred, so an early t.Fatal between here and
	// the deliberate open never leaves the hook goroutine parked on hookProceed.
	var gateOnce sync.Once
	openGate := func() { gateOnce.Do(func() { close(hookProceed) }) }
	defer openGate()
	testhooks.ChargePointResponse = func(conf ocpp.Response, requestId string) {
		hookCalled <- requestId
		<-hookProceed
	}
	defer func() { testhooks.ChargePointResponse = nil }()

	// cancelSeen closes when the FIRST of the two callbacks runs. While the hook
	// pins the read goroutine, the only callback that can fire is the pump's
	// cancel of R2: on master it steals cb1 (R1's), on a fixed impl it addresses
	// cb2 (R2's). Either way its firing deterministically marks "the pump's
	// cancel has been delivered", so the gate opens without a scheduler-dependent
	// spin (the previous awaitBoundedYields gate watched r2Result, which on
	// master cannot fill until AFTER the gate — so it never actually gated).
	cancelSeen := make(chan struct{})
	var cancelOnce sync.Once
	markCancel := func() { cancelOnce.Do(func() { close(cancelSeen) }) }

	// --- Send R1 (live ctx) ---
	type result struct {
		confirmation ocpp.Response
		err          error
	}
	r1Result := make(chan result, 1)
	err := suite.chargePoint.SendRequestAsyncCtx(context.Background(), core.NewHeartbeatRequest(),
		func(conf ocpp.Response, err error) {
			markCancel()
			r1Result <- result{confirmation: conf, err: err}
		})
	require.NoError(t, err)

	// Wait for R1 to be dispatched (written). The first generated ID is R1's.
	select {
	case <-writeC:
	case <-time.After(inboundOrderingWaitTimeout):
		t.Fatal("timed out waiting for R1 to be dispatched")
	}
	r1ID := "e2-0-cr-1"

	// --- Send R2 (ALREADY EXPIRED ctx) ---
	ctxR2, cancelR2 := context.WithCancel(context.Background())
	cancelR2() // immediately expired
	r2Result := make(chan result, 1)
	err = suite.chargePoint.SendRequestAsyncCtx(ctxR2, core.NewHeartbeatRequest(),
		func(conf ocpp.Response, err error) {
			markCancel()
			r2Result <- result{confirmation: conf, err: err}
		})
	require.NoError(t, err)
	// r2ID intentionally unused — the test asserts by callback contents,
	// not by ID.

	// R2 does NOT produce a write (it's dropped pre-write by the E1c pump
	// arm), so we do not wait for writeC here.

	// --- Deliver R1's CALL RESULT ---
	// This triggers CompleteRequest(R1) → signals readyForDispatch → hook.
	// The hook fires on a goroutine (the test delivers via a separate
	// goroutine because the hook blocks). While the hook is blocked, the
	// pump processes R2: ctx expired → pre-write drop → fireRequestCancel
	// → onRequestTimeout → cp.incoming <- incomingError. Then hook returns,
	// and R1's response enters cp.incoming.
	// Result: cp.incoming = [cancelErr(R2), response(R1)].
	currentTime := types.NewDateTime(time.Now())
	callResultJson := fmt.Sprintf(`[3,"%s",{"currentTime":"%s"}]`, r1ID, currentTime.FormatTimestamp())

	go func() {
		// Deliver on a separate goroutine because the hook blocks
		// and the test goroutine must wait for both the hook and
		// the pump to do their work before unblocking the hook.
		_ = suite.mockWsClient.MessageHandler([]byte(callResultJson))
	}()

	// Wait for the hook to fire — confirms CompleteRequest(R1) ran and the read
	// goroutine is now pinned in the hook.
	select {
	case hookedID := <-hookCalled:
		assert.Equal(t, r1ID, hookedID, "hook must fire for R1's CALL_RESULT")
	case <-time.After(inboundOrderingWaitTimeout):
		t.Fatal("timed out waiting for response hook to fire")
	}

	// Wait until the pump's cancel of R2 has actually reached a callback, then
	// open the gate so R1's response follows it into cp.incoming. Deterministic
	// on both master (cb1 stolen) and a fixed impl (cb2). The fallback is
	// bounded and non-fatal so a broken interim impl that delivers no cancel
	// unblocks the gate rather than hanging with it shut.
	select {
	case <-cancelSeen:
	case <-time.After(inboundOrderingWaitTimeout):
	}
	openGate()

	// --- Collect results ---
	var r1Got, r2Got result
	timeout := time.After(inboundOrderingWaitTimeout)

	for i := 0; i < 2; i++ {
		select {
		case res := <-r1Result:
			r1Got = res
		case res := <-r2Result:
			r2Got = res
		case <-timeout:
			t.Fatalf("timed out collecting both callbacks; got r1=%+v r2=%+v (on master R1 is stolen: r1 holds a cancel error, r2 holds R1's confirmation)", r1Got, r2Got)
		}
	}

	// --- Assert each caller received ITS OWN result ---
	// After E2-0:
	//   R1 → HeartbeatConfirmation (not error)
	//   R2 → error (not confirmation)
	// On master (the inversion):
	//   R1 → error, R2 → HeartbeatConfirmation
	assert.NotNil(t, r1Got.confirmation, "R1 must receive its HeartbeatConfirmation (not an error)")
	assert.Nil(t, r1Got.err, "R1 must NOT receive an error")
	_, ok := r1Got.confirmation.(*core.HeartbeatConfirmation)
	assert.True(t, ok, "R1 must receive a HeartbeatConfirmation")

	assert.Nil(t, r2Got.confirmation, "R2 must NOT receive a confirmation (its ctx was expired)")
	assert.Error(t, r2Got.err, "R2 must receive a cancel error")
}

// ============================================================================
// Test 2 — Server cross-delivery regression
// ============================================================================

// TestE2_0_ServerCrossDeliveryRegression is the server-side equivalent of
// test 1. It reproduces the callback mis-pairing on the central-system side
// via the Write-error window (spec §0 "Server — same shape"). A hook pins
// the CS response handler between CompleteRequest and Dequeue so the pump
// can dispatch (and cancel) a queued R2 whose Write fails. On master the
// pump-side typed Dequeue steals the still-pending R1's callback; after
// E2-0 the cancel is by exact requestID.
//
// The server hook is invoked at the top of the response-handler closure
// (v16.go:399 area), after CompleteRequest and before
// handleIncomingConfirmation's Dequeue.
//
// The response handler is pinned via internal/testhooks.CentralSystemResponse.
func (suite *OcppV16TestSuite) TestE2_0_ServerCrossDeliveryRegression() {
	t := suite.T()

	// --- Sequential message IDs ---
	var idSeq int
	seqGen := func() string {
		idSeq++
		return fmt.Sprintf("e2-0-sr-%d", idSeq)
	}
	ocppj.SetMessageIdGenerator(seqGen)
	defer func() {
		ocppj.SetMessageIdGenerator(suite.messageIdGenerator.generateId)
	}()

	wsID := "test-cp"
	channel := NewMockWebSocket(wsID)

	// Wire the mocks: server writes to mockWsServer.Write. R1's Write
	// succeeds; R2's Write fails, triggering the pump-side cancel.
	suite.mockWsServer.On("Start", mock.AnythingOfType("int"), mock.AnythingOfType("string")).Return(nil)
	suite.mockWsServer.On("Stop").Return()
	suite.mockWsServer.On("Write", mock.AnythingOfType("string"), mock.Anything).
		Return(nil).Once()
	suite.mockWsServer.On("Write", mock.AnythingOfType("string"), mock.Anything).
		Return(fmt.Errorf("write failed for R2"))

	suite.mockWsClient.On("Start", mock.AnythingOfType("string")).Return(nil).Run(func(args mock.Arguments) {
		suite.mockWsServer.NewClientHandler(channel)
	})
	suite.mockWsClient.On("Write", mock.Anything).Return(nil)
	suite.mockWsClient.On("Stop").Return()
	// False so the charge point's Stop() returns immediately rather than blocking
	// on a disconnect signal this mock never fires (ocppj.Client.Stop). The charge
	// point here is a passive peer; the round trip is driven by manual injection.
	suite.mockWsClient.On("IsConnected").Return(false)

	suite.centralSystem.Start(8887, "somePath")
	defer suite.centralSystem.Stop()

	err := suite.chargePoint.Start("someUrl")
	require.NoError(t, err)
	defer suite.chargePoint.Stop()

	// --- Install the server response-hook seam ---
	hookCalled := make(chan string, 1)
	hookProceed := make(chan struct{})
	var gateOnce sync.Once
	openGate := func() { gateOnce.Do(func() { close(hookProceed) }) }
	defer openGate()
	testhooks.CentralSystemResponse = func(client ws.Channel, conf ocpp.Response, requestId string) {
		hookCalled <- requestId
		<-hookProceed
	}
	defer func() { testhooks.CentralSystemResponse = nil }()

	// cancelSeen closes when the first callback runs (the pump-side cancel of
	// R2 after its Write fails). See test 1 for the rationale — this replaces a
	// gate that watched r2Result, which on master cannot fill before the gate.
	cancelSeen := make(chan struct{})
	var cancelOnce sync.Once
	markCancel := func() { cancelOnce.Do(func() { close(cancelSeen) }) }

	// --- Send R1 and R2 from server to charge point ---
	type result struct {
		confirmation ocpp.Response
		err          error
	}

	r1Result := make(chan result, 1)
	err = suite.centralSystem.SendRequestAsync(wsID,
		core.NewChangeAvailabilityRequest(1, core.AvailabilityTypeOperative),
		func(conf ocpp.Response, err error) {
			markCancel()
			r1Result <- result{confirmation: conf, err: err}
		})
	require.NoError(t, err)
	r1ID := "e2-0-sr-1"

	r2Result := make(chan result, 1)
	err = suite.centralSystem.SendRequestAsync(wsID,
		core.NewChangeAvailabilityRequest(1, core.AvailabilityTypeInoperative),
		func(conf ocpp.Response, err error) {
			markCancel()
			r2Result <- result{confirmation: conf, err: err}
		})
	require.NoError(t, err)

	// --- Inject R1's CALL RESULT into the server ---
	callResultJson := fmt.Sprintf(`[3,"%s",{"status":"%s"}]`, r1ID, core.AvailabilityStatusAccepted)

	go func() {
		_ = suite.mockWsServer.MessageHandler(channel, []byte(callResultJson))
	}()

	// Wait for the hook to fire.
	select {
	case hookedID := <-hookCalled:
		assert.Equal(t, r1ID, hookedID, "server hook must fire for R1's CALL_RESULT")
	case <-time.After(inboundOrderingWaitTimeout):
		t.Fatal("timed out waiting for server response hook")
	}

	// Wait until R2's write-failure cancel has reached a callback, then open the
	// gate so R1's response follows. Deterministic on both sides; bounded,
	// non-fatal fallback.
	select {
	case <-cancelSeen:
	case <-time.After(inboundOrderingWaitTimeout):
	}
	openGate()

	// --- Collect results ---
	var r1Got, r2Got result
	timeout := time.After(inboundOrderingWaitTimeout)
	for i := 0; i < 2; i++ {
		select {
		case res := <-r1Result:
			r1Got = res
		case res := <-r2Result:
			r2Got = res
		case <-timeout:
			t.Fatalf("timed out collecting both server-side callbacks; got r1=%+v r2=%+v (on master R1 is stolen)", r1Got, r2Got)
		}
	}

	// --- Assert each caller received ITS OWN result ---
	assert.NotNil(t, r1Got.confirmation, "R1 must receive its ChangeAvailabilityConfirmation (not R2's error)")
	assert.Nil(t, r1Got.err, "R1 must NOT receive an error")
	_, ok := r1Got.confirmation.(*core.ChangeAvailabilityConfirmation)
	assert.True(t, ok, "R1 must receive a ChangeAvailabilityConfirmation")

	assert.Nil(t, r2Got.confirmation, "R2 must NOT receive a confirmation (its write failed)")
	assert.Error(t, r2Got.err, "R2 must receive a cancel/error")
}

// ============================================================================
// Test 3 — wire CALL_ERROR routes to the in-flight caller by ID
// ============================================================================

// TestE2_0_WireErrorRoutesByID exercises the CALL_ERROR path end-to-end through
// the facade: a wire CALL_ERROR for the in-flight request R1 reaches R1's caller,
// and a queued sibling R2 remains correctly addressable afterward.
//
// NOTE ON SCOPE — this is a positive/guard test, NOT a red-first regression pin.
// An earlier draft ("untyped path regression") tried to demonstrate the untyped
// Dequeue("main","") mispairing by delivering a CALL_ERROR *for a queued R2* while
// R1 was in flight. That scenario is structurally infeasible: the client
// dispatcher is single-in-flight, so R2 is never dispatched (never written, never
// "pending") until R1 completes, and ocppj discards a CALL_ERROR whose id is not a
// pending request. Consequently a wire CALL_ERROR can only ever target the
// in-flight request — which is always the OLDEST registered callback — so the
// untyped dequeue happens to be correct for wire errors and no mispairing is
// reachable through this path. The untyped-mispairing REGRESSION is instead pinned
// by TestE2_0_ClientCrossDeliveryRegression and ...DifferentTypeCascade, which
// reach the same untyped incomingError arm via the pump-side cancel of a queued
// request (a younger callback) while an older callback is in flight.
func (suite *OcppV16TestSuite) TestE2_0_WireErrorRoutesByID() {
	t := suite.T()

	// --- Sequential message IDs ---
	var idSeq int
	seqGen := func() string {
		idSeq++
		return fmt.Sprintf("e2-0-ut-%d", idSeq)
	}
	ocppj.SetMessageIdGenerator(seqGen)
	defer func() {
		ocppj.SetMessageIdGenerator(suite.messageIdGenerator.generateId)
	}()

	writeC := make(chan []byte, 8)
	startStandaloneChargePoint(suite, writeC)
	suite.mockWsClient.On("IsConnected").Return(false)
	suite.mockWsClient.On("Stop").Return()
	defer suite.chargePoint.Stop()

	type result struct {
		confirmation ocpp.Response
		err          error
	}

	// --- Send R1 (in-flight) and R2 (queued behind it) ---
	r1Result := make(chan result, 1)
	err := suite.chargePoint.SendRequestAsyncCtx(context.Background(), core.NewHeartbeatRequest(),
		func(conf ocpp.Response, err error) {
			r1Result <- result{confirmation: conf, err: err}
		})
	require.NoError(t, err)

	select {
	case <-writeC:
	case <-time.After(inboundOrderingWaitTimeout):
		t.Fatal("timed out waiting for R1 to be dispatched")
	}
	r1ID := "e2-0-ut-1"

	r2Result := make(chan result, 1)
	err = suite.chargePoint.SendRequestAsyncCtx(context.Background(), core.NewHeartbeatRequest(),
		func(conf ocpp.Response, err error) {
			r2Result <- result{confirmation: conf, err: err}
		})
	require.NoError(t, err)
	r2ID := "e2-0-ut-2"
	// R2 stays QUEUED (single in-flight) — no write until R1 completes.

	// --- Deliver a CALL_ERROR for the in-flight R1 ---
	callErrorJson := fmt.Sprintf(`[4,"%s","%s","%s",{}]`, r1ID, ocppj.GenericError, "server rejected R1")
	err = suite.mockWsClient.MessageHandler([]byte(callErrorJson))
	require.NoError(t, err)

	// R1's caller receives the error (routed by R1's id).
	select {
	case res := <-r1Result:
		assert.Nil(t, res.confirmation, "R1 must NOT receive a confirmation")
		require.Error(t, res.err, "R1 must receive the CALL_ERROR")
		ocppErr, ok := res.err.(*ocpp.Error)
		require.True(t, ok, "R1's error must be an *ocpp.Error")
		assert.Equal(t, ocppj.GenericError, ocppErr.Code)
		assert.Equal(t, r1ID, ocppErr.MessageId)
	case <-time.After(inboundOrderingWaitTimeout):
		t.Fatal("timed out waiting for R1's error callback")
	}

	// R1 completing lets R2 dispatch. Wait for its write, then answer it.
	select {
	case <-writeC:
	case <-time.After(inboundOrderingWaitTimeout):
		t.Fatal("timed out waiting for R2 to be dispatched after R1 completed")
	}

	currentTime := types.NewDateTime(time.Now())
	callResultJson := fmt.Sprintf(`[3,"%s",{"currentTime":"%s"}]`, r2ID, currentTime.FormatTimestamp())
	err = suite.mockWsClient.MessageHandler([]byte(callResultJson))
	require.NoError(t, err)

	// R2's caller receives its own confirmation — undisturbed by R1's error.
	select {
	case res := <-r2Result:
		assert.NoError(t, res.err, "R2 must NOT receive an error")
		require.NotNil(t, res.confirmation, "R2 must receive its HeartbeatConfirmation")
		_, ok := res.confirmation.(*core.HeartbeatConfirmation)
		assert.True(t, ok, "R2 must receive a HeartbeatConfirmation")
	case <-time.After(inboundOrderingWaitTimeout):
		t.Fatal("timed out waiting for R2's confirmation")
	}
}

// ============================================================================
// Test E2-0 Different-Type Cascade (§0 "worse, and it cascades")
// ============================================================================

// TestE2_0_ClientCrossDeliveryDifferentTypeCascade reproduces the
// different-type cascade variant: R1 is a HeartbeatRequest, R2 is a
// BootNotificationRequest (different feature type). On master, the untyped
// cancel dequeue steals R1's callback; R1's response then finds NO callback
// of Heartbeat type ("no handler available"); cb2 (R2's callback) is
// orphaned. After E2-0, each caller receives its own result — neither
// swapped nor orphaned.
//
// The read goroutine is pinned via internal/testhooks.ChargePointResponse.
func (suite *OcppV16TestSuite) TestE2_0_ClientCrossDeliveryDifferentTypeCascade() {
	t := suite.T()

	var idSeq int
	seqGen := func() string {
		idSeq++
		return fmt.Sprintf("e2-0-dt-%d", idSeq)
	}
	ocppj.SetMessageIdGenerator(seqGen)
	defer func() {
		ocppj.SetMessageIdGenerator(suite.messageIdGenerator.generateId)
	}()

	writeC := make(chan []byte, 8)
	startStandaloneChargePoint(suite, writeC)
	suite.mockWsClient.On("IsConnected").Return(false)
	suite.mockWsClient.On("Stop").Return()
	defer suite.chargePoint.Stop()

	hookCalled := make(chan string, 1)
	hookProceed := make(chan struct{})
	var gateOnce sync.Once
	openGate := func() { gateOnce.Do(func() { close(hookProceed) }) }
	defer openGate()
	testhooks.ChargePointResponse = func(conf ocpp.Response, requestId string) {
		hookCalled <- requestId
		<-hookProceed
	}
	defer func() { testhooks.ChargePointResponse = nil }()

	cancelSeen := make(chan struct{})
	var cancelOnce sync.Once
	markCancel := func() { cancelOnce.Do(func() { close(cancelSeen) }) }

	// --- Send R1 (live ctx, Heartbeat) ---
	type result struct {
		confirmation ocpp.Response
		err          error
	}
	r1Result := make(chan result, 1)
	err := suite.chargePoint.SendRequestAsyncCtx(context.Background(), core.NewHeartbeatRequest(),
		func(conf ocpp.Response, err error) {
			markCancel()
			r1Result <- result{confirmation: conf, err: err}
		})
	require.NoError(t, err)

	select {
	case <-writeC:
	case <-time.After(inboundOrderingWaitTimeout):
		t.Fatal("timed out waiting for R1 to be dispatched")
	}
	r1ID := "e2-0-dt-1"

	// --- Send R2 (EXPIRED ctx, BootNotification — DIFFERENT type) ---
	ctxR2, cancelR2 := context.WithCancel(context.Background())
	cancelR2()
	r2Result := make(chan result, 1)
	err = suite.chargePoint.SendRequestAsyncCtx(ctxR2, core.NewBootNotificationRequest("model", "vendor"),
		func(conf ocpp.Response, err error) {
			markCancel()
			r2Result <- result{confirmation: conf, err: err}
		})
	require.NoError(t, err)

	// --- Deliver R1's CALL RESULT ---
	currentTime := types.NewDateTime(time.Now())
	callResultJson := fmt.Sprintf(`[3,"%s",{"currentTime":"%s"}]`, r1ID, currentTime.FormatTimestamp())

	go func() {
		_ = suite.mockWsClient.MessageHandler([]byte(callResultJson))
	}()

	select {
	case hookedID := <-hookCalled:
		assert.Equal(t, r1ID, hookedID, "hook must fire for R1's CALL_RESULT")
	case <-time.After(inboundOrderingWaitTimeout):
		t.Fatal("timed out waiting for response hook to fire")
	}

	// Gate on the cancel actually reaching a callback (see test 1).
	select {
	case <-cancelSeen:
	case <-time.After(inboundOrderingWaitTimeout):
	}
	openGate()

	// --- Collect results ---
	// On master the cascade orphans R2's callback: the untyped cancel steals
	// cb1 (R1's), R1's Heartbeat response then finds no Heartbeat callback and
	// is dropped, and cb2 (R2's BootNotification callback) is never addressed —
	// so r2Result never fills and the second iteration times out. The message
	// names that signature.
	var r1Got, r2Got result
	timeout := time.After(inboundOrderingWaitTimeout)

	for i := 0; i < 2; i++ {
		select {
		case res := <-r1Result:
			r1Got = res
		case res := <-r2Result:
			r2Got = res
		case <-timeout:
			t.Fatalf("timed out collecting both callbacks; got r1=%+v r2=%+v (master cascade: cb1 stolen by R2's cancel, R1's response dropped as \"no handler\", cb2 orphaned)", r1Got, r2Got)
		}
	}

	// --- Assert each caller received ITS OWN result ---
	assert.NotNil(t, r1Got.confirmation, "R1 must receive its HeartbeatConfirmation (not an error)")
	assert.Nil(t, r1Got.err, "R1 must NOT receive an error")
	_, ok := r1Got.confirmation.(*core.HeartbeatConfirmation)
	assert.True(t, ok, "R1 must receive a HeartbeatConfirmation")

	assert.Nil(t, r2Got.confirmation, "R2 must NOT receive a confirmation (its ctx was expired)")
	assert.Error(t, r2Got.err, "R2 must receive a cancel error")
}
