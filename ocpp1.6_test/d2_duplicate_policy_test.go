package ocpp16_test

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/enesismail/ocpp-go/ocpp"
	ocpp16 "github.com/enesismail/ocpp-go/ocpp1.6"
	"github.com/enesismail/ocpp-go/ocpp1.6/core"
	"github.com/enesismail/ocpp-go/ocppj"
	"github.com/enesismail/ocpp-go/ws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type d2AsyncResult struct {
	response ocpp.Response
	err      error
}

// newD2KeepNewServer returns a real ws.Server constructed with the PR-D2
// KeepNew policy. Finding 6 (structural honesty): every test in this file
// exercises the facade/dispatcher layer through the MOCK ws server
// (suite.mockWsServer) via directly-called NewClientHandler/
// DisconnectedClientHandler/MessageHandler -- that layer's behavior (async
// callback drain, dispatcher timeout/cancel identity, FIFO dispatch ordering)
// is policy-independent, so driving it through the mock is the correct,
// legitimate way to test it. The *ws.Server this constructs is discarded
// (`_ = newD2KeepNewServer()`) in every caller: it is referenced ONLY to
// anchor this file to the not-yet-implemented PR-D2 compile-red surface
// (ws.WithDuplicateConnectionPolicy/ws.KeepNew), and does NOT drive any real
// eviction in these tests. Real ws-layer eviction (the transition gate, the
// teardown latch, the handleMessage currentness guard) is covered by the
// white-box tests in ws/duplicate_policy_test.go, which construct and start a
// real *server with this same policy.
func newD2KeepNewServer() ws.Server {
	// PR-D2: WithDuplicateConnectionPolicy/KeepNew are the intended public surface.
	return ws.NewServer(ws.WithDuplicateConnectionPolicy(ws.KeepNew))
}

func receiveD2AsyncResult(t *testing.T, name string, ch <-chan d2AsyncResult) d2AsyncResult {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(inboundOrderingWaitTimeout):
		t.Fatalf("timed out waiting for %s", name)
		return d2AsyncResult{}
	}
}

func assertNoD2AsyncResult(t *testing.T, name string, ch <-chan d2AsyncResult) {
	t.Helper()
	select {
	case result := <-ch:
		t.Fatalf("%s fired unexpectedly: response=%#v err=%#v", name, result.response, result.err)
	case <-time.After(200 * time.Millisecond):
	}
}

func d2ChangeAvailabilityCallback(ch chan<- d2AsyncResult) func(ocpp.Response, error) {
	return func(response ocpp.Response, err error) {
		ch <- d2AsyncResult{response: response, err: err}
	}
}

func d2SuccessfulChangeAvailabilityResult(messageID string) []byte {
	return []byte(fmt.Sprintf(`[3,"%s",{"status":"Accepted"}]`, messageID))
}

// TestD2NoTimeoutCrossTalkOldTimeoutDoesNotFireNewCallback covers PR-D2 test
// 8. After a KeepNew eviction, old timeout/cancel state must not invoke a
// callback that belongs to the replacement connection, and the replacement's
// own request must complete normally.
//
// Finding 3: the previous version drained old via a DISCONNECT
// (DisconnectedClientHandler) immediately after SendRequestAsync, so old's
// pending request was cleared by ClearClientPendingRequest/the facade drain,
// not by gap-4's actual hazard -- a stale dispatcher timeout/cancel token.
// That never exercised the merged PR-0 token-identity protection this test is
// supposed to guard. Reworked to drive a REAL server-dispatcher timeout
// (mirrors ocppj/local_transport_test.go's TestServerTimeoutClassifiedAsRequestTimeout
// and ocppj/callback_panic_test.go: a short SetTimeout set before Start, then
// letting the pump's real waitForTimeout fire): old's own request times out
// for real and its callback observes the timeout, only THEN is old evicted
// and a same-ID replacement established, and new's own request must still
// complete normally with no stale cross-talk into new's callback.
//
// See Finding 6 (newD2KeepNewServer doc comment): this test exercises the
// facade/dispatcher layer via the MOCK ws server; it does not drive real
// ws-layer eviction.
func (suite *OcppV16TestSuite) TestD2NoTimeoutCrossTalkOldTimeoutDoesNotFireNewCallback() {
	t := suite.T()
	_ = newD2KeepNewServer()
	wsID := "test_id"
	oldChannel := NewMockWebSocket(wsID)
	newChannel := NewMockWebSocket(wsID)
	setupDefaultCentralSystemHandlers(suite, nil, expectedCentralSystemOptions{clientId: wsID})
	suite.centralSystem.SetChargePointDisconnectedHandler(func(chargePoint ocpp16.ChargePointConnection) {})
	// Must be set before Start so the dispatcher pump's waitForTimeout
	// goroutine actually races old's request to a REAL timeout, rather than
	// old's request being cleared by a disconnect drain.
	suite.serverDispatcher.SetTimeout(150 * time.Millisecond)
	suite.centralSystem.Start(8887, "somePath")
	defer suite.centralSystem.Stop()
	suite.mockWsServer.NewClientHandler(oldChannel)

	oldResultC := make(chan d2AsyncResult, 1)
	err := suite.centralSystem.SendRequestAsync(wsID, core.NewChangeAvailabilityRequest(1, core.AvailabilityTypeOperative), d2ChangeAvailabilityCallback(oldResultC))
	require.NoError(t, err)

	// Never respond to old's request: let the real dispatcher timeout fire and
	// cancel it (gap-4's hazard is a stale timeout/cancel token, so the token
	// must genuinely exist before we can assert it doesn't cross-talk).
	oldResult := receiveD2AsyncResult(t, "old dispatcher timeout", oldResultC)
	require.Nil(t, oldResult.response)
	require.Error(t, oldResult.err)
	oldOcppErr, ok := oldResult.err.(*ocpp.Error)
	require.True(t, ok)
	assert.True(t, errors.Is(oldOcppErr, ocppj.ErrRequestTimeout),
		"old's own callback should observe a real dispatcher timeout, got %#v", oldOcppErr)

	// Only now evict old and establish the same-ID replacement.
	suite.mockWsServer.DisconnectedClientHandler(oldChannel)
	suite.mockWsServer.NewClientHandler(newChannel)

	newResultC := make(chan d2AsyncResult, 1)
	err = suite.centralSystem.SendRequestAsync(wsID, core.NewChangeAvailabilityRequest(1, core.AvailabilityTypeInoperative), d2ChangeAvailabilityCallback(newResultC))
	require.NoError(t, err)
	err = suite.mockWsServer.MessageHandler(newChannel, d2SuccessfulChangeAvailabilityResult(defaultMessageId))
	require.NoError(t, err)
	newResult := receiveD2AsyncResult(t, "new replacement response", newResultC)
	require.NoError(t, newResult.err)
	assert.IsType(t, &core.ChangeAvailabilityConfirmation{}, newResult.response)
	assertNoD2AsyncResult(t, "old callback after new response", oldResultC)
}

// TestD2OldCallbacksDrainedExactlyOnce covers PR-D2 test 9. Option A requires
// the facade callbackQueue drain to be installed unconditionally, so eviction
// drains old async callbacks exactly once both with and without a user
// disconnect handler.
//
// See Finding 6 (newD2KeepNewServer doc comment): this drives the drain via
// the MOCK ws server's DisconnectedClientHandler (Option-A drain is a
// policy-independent facade/dispatcher behavior), not real ws-layer eviction.
func (suite *OcppV16TestSuite) TestD2OldCallbacksDrainedExactlyOnce() {
	for _, installUserHandler := range []bool{false, true} {
		suite.T().Run(fmt.Sprintf("user_handler_%v", installUserHandler), func(t *testing.T) {
			suite.SetupTest()
			_ = newD2KeepNewServer()
			wsID := "test_id"
			channel := NewMockWebSocket(wsID)
			setupDefaultCentralSystemHandlers(suite, nil, expectedCentralSystemOptions{clientId: wsID})
			if installUserHandler {
				suite.centralSystem.SetChargePointDisconnectedHandler(func(chargePoint ocpp16.ChargePointConnection) {})
			}
			suite.centralSystem.Start(8887, "somePath")
			defer suite.centralSystem.Stop()
			suite.mockWsServer.NewClientHandler(channel)

			var callbackCount int32
			resultC := make(chan d2AsyncResult, 2)
			err := suite.centralSystem.SendRequestAsync(wsID, core.NewChangeAvailabilityRequest(1, core.AvailabilityTypeOperative), func(response ocpp.Response, err error) {
				atomic.AddInt32(&callbackCount, 1)
				resultC <- d2AsyncResult{response: response, err: err}
			})
			require.NoError(t, err)
			suite.mockWsServer.DisconnectedClientHandler(channel)

			result := receiveD2AsyncResult(t, "old callback drain", resultC)
			require.Nil(t, result.response)
			require.Error(t, result.err)
			assert.Equal(t, int32(1), atomic.LoadInt32(&callbackCount))
			assertNoD2AsyncResult(t, "second old drain callback", resultC)
		})
	}
}

// Finding 2: TestD2LateInboundOnOldDoesNotMisrouteToNewCallback (formerly
// here, covering PR-D2 test 10) has been removed from this file. It injected
// the late old frame via mockWsServer.MessageHandler, which bypasses the real
// ws.server.handleMessage entirely -- the layer where the item-5 currentness
// guard (`s.connections[w.ID()] == w`) actually lives -- so it could never
// decisively validate that guard. The decisive version is now the white-box
// test TestD2HandleMessageDropsLateInboundFromSupersededSocket in
// ws/duplicate_policy_test.go, which drives s.handleMessage directly with a
// stale *webSocket for an id whose connections[] entry is a different,
// current *webSocket.

// TestD2WedgedPumpFallbackFreshConnectionDispatchesFirstRequest is NOT a
// red-first PR-D2 test (Finding 5 relabeling: it was previously described as
// the "facade companion for PR-D2 test 13b" alongside a since-removed
// ws-level dup that turned out to block the wrong thing). It genuinely wedges
// the single server dispatcher pump in Write to another client -- something
// only reachable from this package via the mock ws server, not from
// ws/duplicate_policy_test.go, where the dispatcher is internal -- then
// disconnects the old same-ID client while the pump is stuck, releases the
// pump, and asserts a fresh same-ID connection still dispatches its first
// request normally. That is exactly the ALREADY-MERGED PR-0 FIFO-safety
// guarantee (the delete marker is enqueued on requestChannel before
// DeleteClientAndWait returns/times out, so FIFO orders it before new's first
// dispatch even under a transient pump stall -- dispatcher.go item 3). It
// passes today against merged PR-0 and is kept here as a regression guard
// that PR-D2's eviction latch depends on, not as a red-first PR-D2 assertion.
// (Finding 6: newD2KeepNewServer() below is discarded and only anchors this
// file to the PR-D2 compile-red surface -- see its doc comment.)
func (suite *OcppV16TestSuite) TestD2WedgedPumpFallbackFreshConnectionDispatchesFirstRequest() {
	t := suite.T()
	_ = newD2KeepNewServer()
	oldID := "test_id"
	otherID := "other_id"
	oldChannel := NewMockWebSocket(oldID)
	otherChannel := NewMockWebSocket(otherID)
	freshChannel := NewMockWebSocket(oldID)

	enteredOtherWrite := make(chan struct{}, 1)
	releaseOtherWrite := make(chan struct{})
	freshWriteC := make(chan []byte, 1)
	suite.centralSystem.SetNewChargePointHandler(func(chargePoint ocpp16.ChargePointConnection) {})
	suite.mockWsServer.On("Start", mock.AnythingOfType("int"), mock.AnythingOfType("string")).Return()
	suite.mockWsServer.On("Stop").Return()
	suite.mockWsServer.On("Write", mock.AnythingOfType("string"), mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		clientID := args.String(0)
		data := args.Get(1).([]byte)
		switch clientID {
		case otherID:
			enteredOtherWrite <- struct{}{}
			<-releaseOtherWrite
		case oldID:
			freshWriteC <- data
		}
	})
	suite.centralSystem.Start(8887, "somePath")
	defer suite.centralSystem.Stop()
	suite.mockWsServer.NewClientHandler(oldChannel)
	suite.mockWsServer.NewClientHandler(otherChannel)

	err := suite.centralSystem.SendRequestAsync(otherID, core.NewChangeAvailabilityRequest(1, core.AvailabilityTypeOperative), func(ocpp.Response, error) {})
	require.NoError(t, err)
	select {
	case <-enteredOtherWrite:
	case <-time.After(inboundOrderingWaitTimeout):
		t.Fatal("timed out waiting for dispatcher pump to enter blocked Write")
	}

	disconnectDone := make(chan struct{})
	go func() {
		suite.mockWsServer.DisconnectedClientHandler(oldChannel)
		close(disconnectDone)
	}()
	select {
	case <-disconnectDone:
		t.Fatal("old disconnect completed while dispatcher pump was still wedged")
	case <-time.After(200 * time.Millisecond):
	}

	close(releaseOtherWrite)
	select {
	case <-disconnectDone:
	case <-time.After(inboundOrderingWaitTimeout):
		t.Fatal("timed out waiting for old disconnect after pump recovery")
	}

	suite.mockWsServer.NewClientHandler(freshChannel)
	err = suite.centralSystem.SendRequestAsync(oldID, core.NewChangeAvailabilityRequest(1, core.AvailabilityTypeInoperative), func(ocpp.Response, error) {})
	require.NoError(t, err)
	select {
	case data := <-freshWriteC:
		assert.Contains(t, string(data), core.ChangeAvailabilityFeatureName)
	case <-time.After(inboundOrderingWaitTimeout):
		t.Fatal("timed out waiting for fresh same-ID first request dispatch")
	}
}
