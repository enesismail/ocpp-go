package ocpp2_test

// PR-E2c (tasks/e2-server-context-aware-send.md, "## C. E2c - the feature")
// RED-FIRST test suite - OCPP 2.0.1 CSMS (server) facade. This is the SERVER
// mirror of the already-merged CLIENT-side E1c facade suite
// (ocpp2.0.1_test/context_send_test.go).
//
// RED-FIRST discipline: every test below references the PR-E2c surface
// exactly as the spec names it. Against today's codebase:
//   - CSMS.SendRequestAsyncCtx does not exist
//
// This file is EXPECTED to fail compilation - that IS the intended red state
// pinning the PR-E2c contract.
//
// Spec tests implemented (facade minimum per the task brief): C7.1, C7.2 (+
// C7.15 flush), C7.7.
//
// NOTE: the 2.0.1 facade uses separate responseHandler/errorHandler channels
// (see context_send_test.go's own note), so tests here do not assume any
// particular internal ordering beyond what each assertion states.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/enesismail/ocpp-go/ocpp"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/availability"
	"github.com/enesismail/ocpp-go/ocppj"
)

const e2cFacadeBound = 2 * time.Second
const e2cFacadeSilence = 300 * time.Millisecond

// e2cWireUpServerAndClient stubs the mock ws server/client so a charging
// station "connects" to the CSMS, and every server-side Write is recorded on
// writtenC. Mirrors ocpp1.6_test/e2c_context_send_test.go's helper of the
// same name (separate package, so duplicated rather than shared).
func e2cWireUpServerAndClient(suite *OcppV2TestSuite, channel MockWebSocket, writtenC chan []byte) {
	suite.mockWsServer.On("Start", mock.AnythingOfType("int"), mock.AnythingOfType("string")).Return(nil)
	suite.mockWsServer.On("Stop").Return()
	suite.mockWsServer.On("Write", mock.AnythingOfType("string"), mock.Anything).
		Return(nil).Run(func(args mock.Arguments) {
		data, _ := args.Get(1).([]byte)
		writtenC <- data
	})

	suite.mockWsClient.On("Start", mock.AnythingOfType("string")).Return(nil).Run(func(args mock.Arguments) {
		suite.mockWsServer.NewClientHandler(channel)
	})
	suite.mockWsClient.On("Write", mock.Anything).Return(nil)
	suite.mockWsClient.On("Stop").Return()
	suite.mockWsClient.On("IsConnected").Return(false)
}

// ============================================================================
// C7.1 - Cancel a DISPATCHED request
// ============================================================================

// TestE2cCancelDispatchedServerRequest verifies that csms.SendRequestAsyncCtx
// with a mid-flight-canceled context delivers exactly one callback, with an
// error matching both context.Canceled and ocppj.ErrRequestCanceled.
func (suite *OcppV2TestSuite) TestE2cCancelDispatchedServerRequest() {
	t := suite.T()
	wsID := "e2c-t1-cs"
	channel := NewMockWebSocket(wsID)

	writtenC := make(chan []byte, 8)
	e2cWireUpServerAndClient(suite, channel, writtenC)

	suite.csms.Start(8887, "somePath")
	defer suite.csms.Stop()
	err := suite.chargingStation.Start("someUrl")
	require.NoError(t, err)
	defer suite.chargingStation.Stop()

	ctx, cancel := context.WithCancel(context.Background())

	type result struct {
		confirmation ocpp.Response
		err          error
	}
	resultC := make(chan result, 1)
	// PR-E2c: CSMS.SendRequestAsyncCtx(ctx, clientId, request, callback).
	err = suite.csms.SendRequestAsyncCtx(ctx, wsID,
		availability.NewChangeAvailabilityRequest(availability.OperationalStatusOperative),
		func(conf ocpp.Response, err error) {
			resultC <- result{confirmation: conf, err: err}
		})
	require.NoError(t, err)

	select {
	case <-writtenC:
	case <-time.After(e2cFacadeBound):
		t.Fatal("timed out waiting for the request to be dispatched")
	}

	cancel()

	select {
	case res := <-resultC:
		assert.Nil(t, res.confirmation, "should not receive a confirmation on cancel")
		require.Error(t, res.err, "must receive an error on cancel")
		assert.True(t, errors.Is(res.err, context.Canceled), "error must match context.Canceled, got %v", res.err)
		assert.True(t, errors.Is(res.err, ocppj.ErrRequestCanceled), "error must match ErrRequestCanceled, got %v", res.err)
	case <-time.After(e2cFacadeBound):
		t.Fatal("SendRequestAsyncCtx callback never fired on ctx cancel")
	}

	select {
	case res := <-resultC:
		t.Fatalf("callback fired more than once: %+v", res)
	case <-time.After(e2cFacadeSilence):
	}
}

// ============================================================================
// C7.2 + C7.15 - Cancel a QUEUED (behind an in-flight) request
// ============================================================================

// TestE2cCancelQueuedServerRequestNeverWritten verifies that a request
// queued behind an in-flight one, whose ctx is canceled while still queued,
// is never written to the wire. Per C7.15, the in-flight front is flushed to
// completion first, so the "never written" assertion is decisive rather than
// vacuous.
func (suite *OcppV2TestSuite) TestE2cCancelQueuedServerRequestNeverWritten() {
	t := suite.T()

	var idSeq int
	seqGen := func() string {
		idSeq++
		return fmt.Sprintf("e2c-t2-%d", idSeq)
	}
	ocppj.SetMessageIdGenerator(seqGen)
	defer func() { ocppj.SetMessageIdGenerator(suite.messageIdGenerator.generateId) }()

	wsID := "e2c-t2-cs"
	channel := NewMockWebSocket(wsID)

	writtenC := make(chan []byte, 8)
	e2cWireUpServerAndClient(suite, channel, writtenC)

	suite.csms.Start(8887, "somePath")
	defer suite.csms.Stop()
	err := suite.chargingStation.Start("someUrl")
	require.NoError(t, err)
	defer suite.chargingStation.Stop()

	type result struct {
		confirmation ocpp.Response
		err          error
	}

	r1Result := make(chan result, 1)
	err = suite.csms.SendRequestAsync(wsID,
		availability.NewChangeAvailabilityRequest(availability.OperationalStatusOperative),
		func(conf ocpp.Response, err error) { r1Result <- result{confirmation: conf, err: err} })
	require.NoError(t, err)
	r1ID := "e2c-t2-1"

	select {
	case <-writtenC:
	case <-time.After(e2cFacadeBound):
		t.Fatal("timed out waiting for R1 to be dispatched")
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	r2Result := make(chan result, 1)
	// PR-E2c
	err = suite.csms.SendRequestAsyncCtx(ctx2, wsID,
		availability.NewChangeAvailabilityRequest(availability.OperationalStatusInoperative),
		func(conf ocpp.Response, err error) { r2Result <- result{confirmation: conf, err: err} })
	require.NoError(t, err)

	// Cancel R2 while it is still queued behind the in-flight R1.
	cancel2()

	// Not yet decisive - R1 is still in-flight regardless.
	select {
	case data := <-writtenC:
		t.Fatalf("unexpected write while R1 is still in-flight: %s", string(data))
	case <-time.After(e2cFacadeSilence):
	}

	// Flush (C7.15): deliver R1's genuine CALL_RESULT, letting the pump
	// advance to R2.
	callResultJson := fmt.Sprintf(`[3,"%s",{"status":"%s"}]`, r1ID, availability.ChangeAvailabilityStatusAccepted)
	go func() {
		_ = suite.mockWsServer.MessageHandler(channel, []byte(callResultJson))
	}()

	select {
	case res := <-r1Result:
		assert.NoError(t, res.err)
		require.NotNil(t, res.confirmation)
	case <-time.After(e2cFacadeBound):
		t.Fatal("timed out waiting for R1's confirmation")
	}

	select {
	case res := <-r2Result:
		assert.Nil(t, res.confirmation, "R2 must not receive a confirmation - it was canceled while queued")
		require.Error(t, res.err)
		assert.True(t, errors.Is(res.err, context.Canceled))
	case data := <-writtenC:
		t.Fatalf("PR-E2c REGRESSION: canceled queued request R2 was written to the wire: %s", string(data))
	case <-time.After(e2cFacadeBound):
		t.Fatal("neither R2's cancellation nor a write was observed")
	}

	select {
	case data := <-writtenC:
		t.Fatalf("unexpected extra write: %s", string(data))
	case <-time.After(e2cFacadeSilence):
	}
}

// ============================================================================
// C7.7 - SendRequestAsyncCtx(nil, ...) behaves as Background;
// SendRequestAsync unchanged.
// ============================================================================

func (suite *OcppV2TestSuite) TestE2cSendRequestAsyncCtxNilBehavesAsBackground() {
	t := suite.T()
	wsID := "e2c-t7-cs"
	channel := NewMockWebSocket(wsID)

	writtenC := make(chan []byte, 8)
	e2cWireUpServerAndClient(suite, channel, writtenC)

	suite.csms.Start(8887, "somePath")
	defer suite.csms.Stop()
	err := suite.chargingStation.Start("someUrl")
	require.NoError(t, err)
	defer suite.chargingStation.Stop()

	type result struct {
		confirmation ocpp.Response
		err          error
	}

	resultC := make(chan result, 1)
	// PR-E2c: nil ctx must behave as context.Background().
	err = suite.csms.SendRequestAsyncCtx(nil, wsID,
		availability.NewChangeAvailabilityRequest(availability.OperationalStatusOperative),
		func(conf ocpp.Response, err error) { resultC <- result{confirmation: conf, err: err} })
	require.NoError(t, err)

	select {
	case <-writtenC:
	case <-time.After(e2cFacadeBound):
		t.Fatal("timed out waiting for the request to be dispatched")
	}

	callResultJson := fmt.Sprintf(`[3,"%s",{"status":"%s"}]`, defaultMessageId, availability.ChangeAvailabilityStatusAccepted)
	err = suite.mockWsServer.MessageHandler(channel, []byte(callResultJson))
	require.NoError(t, err)

	select {
	case res := <-resultC:
		require.NoError(t, res.err)
		require.NotNil(t, res.confirmation)
		_, ok := res.confirmation.(*availability.ChangeAvailabilityResponse)
		assert.True(t, ok, "SendRequestAsyncCtx(nil, ...) must deliver a typed confirmation")
	case <-time.After(e2cFacadeBound):
		t.Fatal("timed out waiting for the confirmation")
	}

	// The existing SendRequestAsync (ctx-less) must still work unchanged.
	resultC2 := make(chan result, 1)
	err = suite.csms.SendRequestAsync(wsID,
		availability.NewChangeAvailabilityRequest(availability.OperationalStatusInoperative),
		func(conf ocpp.Response, err error) { resultC2 <- result{confirmation: conf, err: err} })
	require.NoError(t, err)
	select {
	case <-writtenC:
	case <-time.After(e2cFacadeBound):
		t.Fatal("timed out waiting for the SendRequestAsync regression request to be dispatched")
	}
	callResultJson2 := fmt.Sprintf(`[3,"%s",{"status":"%s"}]`, defaultMessageId, availability.ChangeAvailabilityStatusAccepted)
	err = suite.mockWsServer.MessageHandler(channel, []byte(callResultJson2))
	require.NoError(t, err)
	select {
	case res := <-resultC2:
		require.NoError(t, res.err)
		require.NotNil(t, res.confirmation)
	case <-time.After(e2cFacadeBound):
		t.Fatal("timed out waiting for the SendRequestAsync regression confirmation")
	}
}
