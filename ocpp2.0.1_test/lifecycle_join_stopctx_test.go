package ocpp2_test

// PR-L1 (tasks/facade-lifecycle-hardening.md §2) COMPILE-RED test file.
//
// StopCtx(ctx context.Context) error does not exist yet on
// ocpp2.ChargingStation - referencing it here fails the WHOLE package's
// compilation. That is the deliberate, isolated compile-red this file carries;
// keeping it separate from lifecycle_join_test.go lets that file's RUNTIME reds
// be observed independently (e.g. by temporarily moving this file aside).
// Mirrors the sibling ocpp1.6_test file of the same name.
//
// Spec items pinned: §Tests item 4 - "StopCtx with an already-expired ctx
// returns ctx.Err() promptly, and a retry StopCtx then succeeds" (spec §2).
//
// WHY BOTH TESTS PIN THE HANDLER FIRST (load-bearing, do not simplify): without
// a pinned handler, handlerDone is typically ALREADY CLOSED when the join's
// select runs. With an expired ctx both arms are then ready, and the spec's own
// illustrated shape - `select { case <-handlerDone: case <-ctx.Done(): }` -
// picks pseudo-randomly, returning nil roughly half the time. The test would
// flake ~50% against a perfectly compliant implementation. Pinning keeps
// handlerDone OPEN so the ctx arm is the only ready one under any compliant
// shape.

import (
	"context"
	"sync"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/enesismail/ocpp-go/ocpp"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/availability"
	"github.com/enesismail/ocpp-go/ocppj"
)

// l1PinHandler starts the charging station and parks its asyncCallbackHandler
// inside a callback blocked on the returned gate. The returned release func is
// idempotent.
func l1PinHandler(suite *OcppV2TestSuite, idPrefix string) (releaseGate func()) {
	t := suite.T()
	writtenC := make(chan []byte, 8)
	ocppj.SetMessageIdGenerator(l2SequentialMessageIds(idPrefix))
	t.Cleanup(func() { ocppj.SetMessageIdGenerator(suite.messageIdGenerator.generateId) })

	l2StartStandaloneChargingStation(suite, writtenC)

	gateC := make(chan struct{})
	var once sync.Once
	releaseGate = func() { once.Do(func() { close(gateC) }) }

	pinnedC := make(chan struct{})
	err := suite.chargingStation.SendRequestAsync(availability.NewHeartbeatRequest(), func(response ocpp.Response, err error) {
		close(pinnedC)
		<-gateC
	})
	require.NoError(t, err)
	l2WaitForWrite(t, writtenC, "timed out waiting for the pinning request to be written")
	err = suite.mockWsClient.MessageHandler([]byte(l2HeartbeatCallResultJson(idPrefix + "-0")))
	require.NoError(t, err)
	l2WaitOrFail(t, pinnedC, "timed out waiting for the async handler to be pinned")
	return releaseGate
}

// TestL1StopCtxExpiredContextReturnsPromptly pins spec §2: StopCtx(ctx) with an
// already-expired ctx returns ctx.Err() promptly rather than blocking on the
// join.
func (suite *OcppV2TestSuite) TestL1StopCtxExpiredContextReturnsPromptly() {
	t := suite.T()
	suite.mockWsClient.On("Stop").Return()
	suite.mockWsClient.On("IsConnected").Return(false)

	releaseGate := l1PinHandler(suite, "l1ctx1")
	// Always release, so a broken early-return implementation cannot leak a
	// still-running facade into sibling tests.
	defer releaseGate()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	<-ctx.Done()

	errC := make(chan error, 1)
	go func() { errC <- suite.chargingStation.StopCtx(ctx) }()

	select {
	case err := <-errC:
		require.ErrorIs(t, err, context.DeadlineExceeded,
			"StopCtx with an expired ctx must report ctx.Err() - the handler is pinned, so the join cannot have completed")
	case <-time.After(e1bBound):
		t.Fatal("StopCtx(expired ctx) did not return within the bounded deadline")
	}
}

// TestL1StopCtxRetryAfterExpirySucceeds pins spec §2's "a retry StopCtx is the
// supported way to reach a clean stop": after an expired-ctx call returns
// ctx.Err() (stopC is closed regardless, so the handler can now finish), a
// SECOND StopCtx with a live context must succeed.
func (suite *OcppV2TestSuite) TestL1StopCtxRetryAfterExpirySucceeds() {
	t := suite.T()
	suite.mockWsClient.On("Stop").Return()
	suite.mockWsClient.On("IsConnected").Return(false)

	releaseGate := l1PinHandler(suite, "l1ctx2")
	defer releaseGate()

	expiredCtx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	<-expiredCtx.Done()

	firstErrC := make(chan error, 1)
	go func() { firstErrC <- suite.chargingStation.StopCtx(expiredCtx) }()
	select {
	case err := <-firstErrC:
		require.Error(t, err, "the first StopCtx must report the expired ctx (handler still pinned)")
	case <-time.After(e1bBound):
		t.Fatal("first StopCtx(expired ctx) did not return within the bounded deadline")
	}

	// Let the pinned handler finish so the retry's join can complete.
	releaseGate()

	secondErrC := make(chan error, 1)
	go func() { secondErrC <- suite.chargingStation.StopCtx(context.Background()) }()
	select {
	case err := <-secondErrC:
		require.NoError(t, err, "retry StopCtx with a live context must succeed once the handler has exited")
	case <-time.After(e1bBound):
		t.Fatal("retry StopCtx did not return within the bounded deadline (deadlock)")
	}
}
