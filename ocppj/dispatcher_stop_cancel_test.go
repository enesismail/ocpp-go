package ocppj_test

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/enesismail/ocpp-go/ocpp"
	"github.com/enesismail/ocpp-go/ocppj"
)

// DispatcherStopCancelTestSuite covers S2 (tasks/s2-pause-timeout.md): two
// non-breaking DefaultClientDispatcher changes.
//
//  1. On Stop(), the messagePump's channel-close branch cancels every
//     outstanding request (dispatched-but-pending AND still-queued) via the
//     new ocppj.ErrDispatcherStopped sentinel, instead of silently dropping
//     them.
//  2. An opt-in (*DefaultClientDispatcher).SetTimeoutOnPause(bool), which,
//     when enabled, makes Pause() park the timer at the real d.timeout
//     instead of the internal 24h tick, so a paused pending request still
//     times out promptly. Default (disabled) behavior is unchanged.
//
// The harness mirrors ClientDispatcherTestSuite in dispatcher_test.go (same
// mock client / queue / state wiring, same "wait for the mocked Write to
// learn a request became pending" mechanism), except the dispatcher field is
// kept as the concrete *ocppj.DefaultClientDispatcher rather than the
// ClientDispatcher interface, since SetTimeoutOnPause is deliberately NOT
// part of that interface (see the spec's rationale).
type DispatcherStopCancelTestSuite struct {
	suite.Suite
	endpoint        ocppj.Client
	queue           ocppj.RequestQueue
	dispatcher      *ocppj.DefaultClientDispatcher
	state           ocppj.ClientState
	websocketClient MockWebsocketClient
}

func (s *DispatcherStopCancelTestSuite) SetupTest() {
	s.endpoint = ocppj.Client{Id: "client1"}
	mockProfile := ocpp.NewProfile("mock", &MockFeature{})
	s.endpoint.AddProfile(mockProfile)
	s.queue = ocppj.NewFIFOClientQueue(10)
	s.dispatcher = ocppj.NewDefaultClientDispatcher(s.queue)
	s.state = ocppj.NewClientState()
	s.dispatcher.SetPendingRequestState(s.state)
	s.websocketClient = MockWebsocketClient{}
	s.dispatcher.SetNetworkClient(&s.websocketClient)
}

func (s *DispatcherStopCancelTestSuite) TearDownTest() {
	// Mirrors ClientDispatcherTestSuite.TearDownTest: join the messagePump
	// goroutine before the next SetupTest replaces the shared mock client.
	if s.dispatcher.IsRunning() {
		s.dispatcher.Stop()
	}
}

// newBundle creates a fresh Call/RequestBundle pair via the suite's endpoint,
// exactly like the "Create mock request" step used throughout dispatcher_test.go.
// It also returns the original ocpp.Request, so callers can assert (mirroring
// TestClientDispatcherTimeout in dispatcher_test.go) that a cancel callback
// receives that very same request, not just its ID.
func (s *DispatcherStopCancelTestSuite) newBundle() (ocppj.RequestBundle, string, ocpp.Request) {
	t := s.T()
	req := newMockRequest("somevalue")
	call, err := s.endpoint.CreateCall(req)
	require.NoError(t, err)
	requestID := call.UniqueId
	data, err := call.MarshalJSON()
	require.NoError(t, err)
	return ocppj.RequestBundle{Call: call, Data: data}, requestID, req
}

// cancelEvent captures one onRequestCanceled invocation for assertion back on
// the test goroutine. Callbacks run on the dispatcher's messagePump goroutine,
// so they must only use assert (never require/FailNow, which must not be
// called off the test goroutine).
type cancelEvent struct {
	requestID string
	request   ocpp.Request
	err       *ocpp.Error
}

// ---------------------------------------------------------------------
// Change 1 — Stop() cancels every outstanding request
// ---------------------------------------------------------------------

// 1. Stop cancels a dispatched-but-pending request: the cancel callback fires
// with an error matching ErrDispatcherStopped (and NOT ErrRequestTimeout /
// a server CALLERROR), and pending state is cleared afterward.
func (s *DispatcherStopCancelTestSuite) TestStopCancelsPendingRequest() {
	t := s.T()
	bundle, requestID, req := s.newBundle()

	sent := make(chan bool, 1)
	s.websocketClient.On("Write", mock.Anything).Run(func(args mock.Arguments) {
		sent <- true
	}).Return(nil)

	canceled := make(chan cancelEvent, 1)
	s.dispatcher.SetOnRequestCanceled(func(rID string, request ocpp.Request, err *ocpp.Error) {
		canceled <- cancelEvent{requestID: rID, request: request, err: err}
	})

	s.dispatcher.Start()
	require.True(t, s.dispatcher.IsRunning())
	require.NoError(t, s.dispatcher.SendRequest(bundle))

	// dispatchNextRequest marks the request pending BEFORE writing to the
	// network, so once the mocked Write fires, the request is guaranteed to
	// be pending (no response ever arrives for it in this test).
	select {
	case <-sent:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the request to be dispatched")
	}
	require.True(t, s.state.HasPendingRequest())

	s.dispatcher.Stop()

	select {
	case ev := <-canceled:
		assert.Equal(t, requestID, ev.requestID)
		assert.True(t, errors.Is(ev.err, ocppj.ErrDispatcherStopped), "expected ErrDispatcherStopped, got %v", ev.err)
		assert.False(t, errors.Is(ev.err, ocppj.ErrRequestTimeout), "a Stop-cancel must not also match ErrRequestTimeout")
		assert.Equal(t, req, ev.request)
		assert.Equal(t, MockFeatureName, ev.request.GetFeatureName())
	case <-time.After(2 * time.Second):
		t.Fatal("expected the pending request to be canceled by Stop")
	}
	assert.False(t, s.state.HasPendingRequest())
	assert.True(t, s.queue.IsEmpty())
}

// 2. Stop cancels BOTH a dispatched-but-pending request AND a request still
// waiting behind it in the queue.
func (s *DispatcherStopCancelTestSuite) TestStopCancelsPendingAndQueuedRequest() {
	t := s.T()
	bundle1, requestID1, _ := s.newBundle()
	bundle2, requestID2, _ := s.newBundle()

	sent := make(chan bool, 1)
	s.websocketClient.On("Write", mock.Anything).Run(func(args mock.Arguments) {
		sent <- true
	}).Return(nil)

	canceled := make(chan cancelEvent, 2)
	s.dispatcher.SetOnRequestCanceled(func(rID string, request ocpp.Request, err *ocpp.Error) {
		canceled <- cancelEvent{requestID: rID, request: request, err: err}
	})

	s.dispatcher.Start()
	require.True(t, s.dispatcher.IsRunning())

	require.NoError(t, s.dispatcher.SendRequest(bundle1))
	select {
	case <-sent:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the first request to be dispatched")
	}
	require.True(t, s.state.HasPendingRequest())

	// bundle2 queues behind the still-pending bundle1: the pump only
	// dispatches again once CompleteRequest signals readyForDispatch, which
	// never happens in this test, so Write is invoked exactly once. Push
	// happens synchronously inside SendRequest, so the queue already holds
	// both bundles as soon as it returns.
	require.NoError(t, s.dispatcher.SendRequest(bundle2))
	assert.Equal(t, 2, s.queue.Size())

	s.dispatcher.Stop()

	got := map[string]cancelEvent{}
	for i := 0; i < 2; i++ {
		select {
		case ev := <-canceled:
			got[ev.requestID] = ev
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for cancel #%d of 2", i+1)
		}
	}
	require.Contains(t, got, requestID1)
	require.Contains(t, got, requestID2)
	for _, ev := range got {
		assert.True(t, errors.Is(ev.err, ocppj.ErrDispatcherStopped), "expected ErrDispatcherStopped, got %v", ev.err)
		assert.False(t, errors.Is(ev.err, ocppj.ErrRequestTimeout))
	}
	assert.False(t, s.state.HasPendingRequest())
	assert.True(t, s.queue.IsEmpty())
}

// 3. Stop cancels a request that was queued but NEVER dispatched (paused
// before it was ever sent), proving cancellation doesn't depend on a request
// having become pending first.
func (s *DispatcherStopCancelTestSuite) TestStopCancelsQueuedNeverDispatchedRequest() {
	t := s.T()
	bundle, requestID, _ := s.newBundle()

	// Were Write ever wrongly invoked here, its Run callback would execute on
	// the dispatcher's messagePump goroutine, not the test goroutine.
	// require.Fail/t.FailNow must only ever be called from the test
	// goroutine, so record the unexpected call on an atomic flag instead and
	// assert it below, from the test goroutine, after Stop().
	var writeCalled int32
	s.websocketClient.On("Write", mock.Anything).Run(func(args mock.Arguments) {
		atomic.StoreInt32(&writeCalled, 1)
	}).Return(nil)

	canceled := make(chan cancelEvent, 1)
	s.dispatcher.SetOnRequestCanceled(func(rID string, request ocpp.Request, err *ocpp.Error) {
		canceled <- cancelEvent{requestID: rID, request: request, err: err}
	})

	s.dispatcher.Start()
	require.True(t, s.dispatcher.IsRunning())

	s.dispatcher.Pause()
	require.True(t, s.dispatcher.IsPaused())

	require.NoError(t, s.dispatcher.SendRequest(bundle))
	// SendRequest pushes onto the queue synchronously before returning; the
	// paused pump will never pick it up for dispatch.
	assert.Equal(t, 1, s.queue.Size())
	assert.False(t, s.state.HasPendingRequest())

	s.dispatcher.Stop()

	select {
	case ev := <-canceled:
		assert.Equal(t, requestID, ev.requestID)
		assert.True(t, errors.Is(ev.err, ocppj.ErrDispatcherStopped), "expected ErrDispatcherStopped, got %v", ev.err)
		assert.False(t, errors.Is(ev.err, ocppj.ErrRequestTimeout))
	case <-time.After(2 * time.Second):
		t.Fatal("expected the queued-but-never-dispatched request to be canceled by Stop")
	}
	assert.True(t, s.queue.IsEmpty())
	assert.Equal(t, int32(0), atomic.LoadInt32(&writeCalled), "write should never be called for a request queued while paused")
}

// 4. Stop with nothing outstanding (no pending, no queued request) must not
// fire any spurious cancel.
func (s *DispatcherStopCancelTestSuite) TestStopWithNothingOutstandingFiresNoCancel() {
	t := s.T()
	var cancelCount int32
	s.dispatcher.SetOnRequestCanceled(func(rID string, request ocpp.Request, err *ocpp.Error) {
		atomic.AddInt32(&cancelCount, 1)
	})

	s.dispatcher.Start()
	require.True(t, s.dispatcher.IsRunning())

	s.dispatcher.Stop()

	assert.Equal(t, int32(0), atomic.LoadInt32(&cancelCount))
}

// 5. Stop() must clear pending/queue state regardless of whether a
// SetOnRequestCanceled callback is registered at all. A broken
// implementation that only clears pending/queue state inside the
// `d.onRequestCancel != nil` branch would pass every other Stop-cancel test
// above (they all register a callback) yet leave stale state behind when no
// callback is set.
func (s *DispatcherStopCancelTestSuite) TestStopClearsStateWithoutCancelCallback() {
	t := s.T()
	bundle, _, _ := s.newBundle()

	sent := make(chan bool, 1)
	s.websocketClient.On("Write", mock.Anything).Run(func(args mock.Arguments) {
		sent <- true
	}).Return(nil)

	// Deliberately NOT calling SetOnRequestCanceled: the dispatcher must
	// still clear its pending/queue state when Stop() cancels this request.

	s.dispatcher.Start()
	require.True(t, s.dispatcher.IsRunning())
	require.NoError(t, s.dispatcher.SendRequest(bundle))

	select {
	case <-sent:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the request to be dispatched")
	}
	require.True(t, s.state.HasPendingRequest())

	s.dispatcher.Stop()

	assert.False(t, s.state.HasPendingRequest())
	assert.True(t, s.queue.IsEmpty())
}

// ---------------------------------------------------------------------
// Change 2 — opt-in: time out during pause
// ---------------------------------------------------------------------

// 6. With SetTimeoutOnPause(true) and a short SetTimeout, a pending request
// still times out (~d.timeout) while paused, instead of being parked for 24h.
func (s *DispatcherStopCancelTestSuite) TestOptInTimeoutDuringPause() {
	t := s.T()
	bundle, requestID, req := s.newBundle()

	// 500ms mirrors the shipping TestClientPauseDispatcher exactly, keeping a
	// full 300ms margin over the 200ms settle below: the pump's own
	// post-dispatch timer can never fire during the settle, and the
	// startTime-relative elapsed>=timeout assertion can't be tripped by a
	// settle-sleep overrun firing the pump's timer before startTime is taken.
	timeout := 500 * time.Millisecond
	s.dispatcher.SetTimeout(timeout)
	s.dispatcher.SetTimeoutOnPause(true)

	sent := make(chan bool, 1)
	s.websocketClient.On("Write", mock.Anything).Run(func(args mock.Arguments) {
		sent <- true
	}).Return(nil)

	canceled := make(chan cancelEvent, 1)
	s.dispatcher.SetOnRequestCanceled(func(rID string, request ocpp.Request, err *ocpp.Error) {
		canceled <- cancelEvent{requestID: rID, request: request, err: err}
	})

	s.dispatcher.Start()
	require.True(t, s.dispatcher.IsRunning())
	require.NoError(t, s.dispatcher.SendRequest(bundle))

	select {
	case <-sent:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the request to be dispatched")
	}
	require.True(t, s.state.HasPendingRequest())

	// The mocked Write's "sent" signal fires INSIDE Write, synchronously
	// inside dispatchNextRequest — which runs BEFORE the pump reaches its own
	// post-dispatch timer.Reset(d.timeout) (dispatcher.go messagePump,
	// ~:271-274). Without a settling gap, Pause() below could race ahead of
	// that reset and then get silently overwritten by it, which would let an
	// impl that ignores the opt-in false-pass (the pump's short d.timeout
	// would "coincidentally" still fire, for the wrong reason).
	//
	// There is no deterministic "timer armed" signal exposed to test against
	// (the reset is welded to dispatch two lines after the only observable
	// event, with nothing test-visible after it), so we mirror the settling
	// window of the shipping TestClientPauseDispatcher (dispatcher_test.go:382)
	// verbatim: 200ms sleep before Pause, with a timeout comfortably above it.
	// That test already depends on Pause() winning this exact reset race and
	// passes in CI, so this is no weaker than the established convention.
	time.Sleep(200 * time.Millisecond)

	startTime := time.Now()
	s.dispatcher.Pause()
	require.True(t, s.dispatcher.IsPaused())

	select {
	case ev := <-canceled:
		elapsed := time.Since(startTime)
		assert.Equal(t, requestID, ev.requestID)
		assert.True(t, errors.Is(ev.err, ocppj.ErrRequestTimeout), "expected ErrRequestTimeout, got %v", ev.err)
		assert.False(t, errors.Is(ev.err, ocppj.ErrDispatcherStopped))
		assert.GreaterOrEqual(t, elapsed, timeout)
		assert.Equal(t, req, ev.request)
		assert.Equal(t, MockFeatureName, ev.request.GetFeatureName())
	case <-time.After(2 * time.Second):
		t.Fatal("expected the paused request to time out with SetTimeoutOnPause(true) rather than hang")
	}
	assert.False(t, s.state.HasPendingRequest())
}

// 7. With SetTimeoutOnPause(true), Resume() grants a pending request a fresh
// timeout window rather than immediately consuming a stale paused-timer fire.
func (s *DispatcherStopCancelTestSuite) TestOptInPauseThenResumeGrantsFreshWindow() {
	t := s.T()
	bundle, _, _ := s.newBundle()

	// Mirror TestOptInTimeoutDuringPause's timeout and settling convention:
	// wait for the mocked Write, then give the pump a bounded window to finish
	// its post-dispatch timer.Reset(d.timeout) before Pause() touches the timer.
	timeout := 500 * time.Millisecond
	s.dispatcher.SetTimeout(timeout)
	s.dispatcher.SetTimeoutOnPause(true)

	sent := make(chan bool, 1)
	s.websocketClient.On("Write", mock.Anything).Run(func(args mock.Arguments) {
		sent <- true
	}).Return(nil)

	canceled := make(chan cancelEvent, 1)
	s.dispatcher.SetOnRequestCanceled(func(rID string, request ocpp.Request, err *ocpp.Error) {
		canceled <- cancelEvent{requestID: rID, request: request, err: err}
	})

	s.dispatcher.Start()
	require.True(t, s.dispatcher.IsRunning())
	require.NoError(t, s.dispatcher.SendRequest(bundle))

	select {
	case <-sent:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the request to be dispatched")
	}
	require.True(t, s.state.HasPendingRequest())

	// See TestOptInTimeoutDuringPause: settle before Pause() so Pause(), not
	// the pump's post-dispatch reset, is the last timer-setter before Resume().
	time.Sleep(200 * time.Millisecond)

	s.dispatcher.Pause()
	require.True(t, s.dispatcher.IsPaused())

	s.dispatcher.Resume()
	assert.False(t, s.dispatcher.IsPaused())
	assert.True(t, s.state.HasPendingRequest())

	select {
	case ev := <-canceled:
		t.Fatalf("unexpected cancel immediately after Resume with SetTimeoutOnPause(true): %v", ev.err)
	case <-time.After(150 * time.Millisecond):
		// Expected: Resume() grants a fresh timeout window, so the pending
		// request is still awaiting response/timeout in this short window.
	}
	assert.True(t, s.state.HasPendingRequest())
}

// 8. Without the opt-in, Pause() keeps parking the timer at the internal 24h
// tick: a pending request must NOT time out within a short window, even with
// a very short SetTimeout. Preserves the intentional "don't annoy
// intermittently-connected charge points" behavior.
func (s *DispatcherStopCancelTestSuite) TestDefaultPauseDoesNotTimeOut() {
	t := s.T()
	bundle, _, _ := s.newBundle()

	// Timeout/settle/window numbers are mirrored verbatim from the shipping
	// TestClientPauseDispatcher (dispatcher_test.go:382): a 500ms real timeout,
	// a 200ms settle before Pause(), and an 800ms observation window. If the
	// default (disabled) opt-in ever regressed and leaked the real timeout into
	// Pause(), the parked 500ms timer would fire ~700ms after Pause — well
	// inside the 800ms window below — instead of parking at the 24h tick. The
	// 500ms timeout sits comfortably above the 200ms settle so the pump's own
	// post-dispatch timer never fires during the settle, and the whole shape
	// matches a test that already passes in CI, so it isn't flaky under load.
	s.dispatcher.SetTimeout(500 * time.Millisecond)

	sent := make(chan bool, 1)
	s.websocketClient.On("Write", mock.Anything).Run(func(args mock.Arguments) {
		sent <- true
	}).Return(nil)

	canceled := make(chan cancelEvent, 1)
	s.dispatcher.SetOnRequestCanceled(func(rID string, request ocpp.Request, err *ocpp.Error) {
		canceled <- cancelEvent{requestID: rID, request: request, err: err}
	})

	s.dispatcher.Start()
	require.True(t, s.dispatcher.IsRunning())
	require.NoError(t, s.dispatcher.SendRequest(bundle))

	select {
	case <-sent:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the request to be dispatched")
	}
	require.True(t, s.state.HasPendingRequest())

	// See TestOptInTimeoutDuringPause: give the pump a bounded settling
	// window to finish its post-dispatch timer.Reset(d.timeout) before we
	// Pause, so Pause() (not the pump) ends up as the last timer-setter.
	// 200ms mirrors TestClientPauseDispatcher exactly.
	time.Sleep(200 * time.Millisecond)

	s.dispatcher.Pause()
	require.True(t, s.dispatcher.IsPaused())

	select {
	case ev := <-canceled:
		t.Fatalf("unexpected cancel while paused without SetTimeoutOnPause: %v", ev.err)
	case <-time.After(800 * time.Millisecond):
		// Expected: default Pause() parks the timer at the 24h tick, so no
		// cancel fires within this comfortably-wide window even though the
		// real timeout is only 500ms.
	}
	assert.True(t, s.state.HasPendingRequest())
	assert.False(t, s.queue.IsEmpty())
}

// ---------------------------------------------------------------------
// Panic safety of the new cancellation site
// ---------------------------------------------------------------------

// 9. A panicking onRequestCanceled callback, invoked from the new Stop
// close-branch cancellation site, must be recovered (the same
// recoverCancelCallback/SetOnHandlerPanic guard already used at the timeout
// site): Stop() must not crash or hang, and every other outstanding request
// must still be canceled.
func (s *DispatcherStopCancelTestSuite) TestStopRecoversFromPanickingCancelCallback() {
	t := s.T()

	// SetOnHandlerPanic is only exposed on the *ocppj.Client facade, which
	// forwards to the dispatcher's unexported onHandlerPanic field via a type
	// assertion on *DefaultClientDispatcher (see ocppj/panic.go). The suite's
	// s.endpoint is a bare struct literal never wired to a dispatcher, so
	// build a real Client wired to the suite's dispatcher instead, exactly
	// like OcppJTestSuite.SetupTest does in ocppj_test.go.
	mockProfile := ocpp.NewProfile("mock", &MockFeature{})
	client := ocppj.NewClient("client1", &s.websocketClient, s.dispatcher, s.state, mockProfile)

	mkBundle := func() (ocppj.RequestBundle, string) {
		req := newMockRequest("somevalue")
		call, err := client.CreateCall(req)
		require.NoError(t, err)
		data, err := call.MarshalJSON()
		require.NoError(t, err)
		return ocppj.RequestBundle{Call: call, Data: data}, call.UniqueId
	}
	bundle1, requestID1 := mkBundle()
	bundle2, requestID2 := mkBundle()

	sent := make(chan bool, 1)
	s.websocketClient.On("Write", mock.Anything).Run(func(args mock.Arguments) {
		sent <- true
	}).Return(nil)

	panicC := make(chan ocppj.HandlerPanic, 2)
	client.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) {
		panicC <- hp
	})

	var cancelCount int32
	s.dispatcher.SetOnRequestCanceled(func(rID string, request ocpp.Request, err *ocpp.Error) {
		atomic.AddInt32(&cancelCount, 1)
		panic("boom: simulated panic in onRequestCanceled for " + rID)
	})

	s.dispatcher.Start()
	require.True(t, s.dispatcher.IsRunning())

	require.NoError(t, s.dispatcher.SendRequest(bundle1))
	select {
	case <-sent:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the first request to be dispatched")
	}
	require.True(t, s.state.HasPendingRequest())

	require.NoError(t, s.dispatcher.SendRequest(bundle2))
	assert.Equal(t, 2, s.queue.Size())

	// Stop() must return even though every cancel callback it triggers panics.
	stopDone := make(chan struct{})
	go func() {
		s.dispatcher.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop() did not return; the pump appears stuck after a panicking cancel callback")
	}

	assert.Equal(t, int32(2), atomic.LoadInt32(&cancelCount), "both outstanding requests should still be canceled despite the panic")

	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case hp := <-panicC:
			assert.Equal(t, ocppj.CancelHandlerKind, hp.Kind)
			seen[hp.RequestID] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for reported panic #%d of 2", i+1)
		}
	}
	assert.True(t, seen[requestID1])
	assert.True(t, seen[requestID2])
	assert.True(t, s.queue.IsEmpty())
}

func TestDispatcherStopCancelSuite(t *testing.T) {
	suite.Run(t, new(DispatcherStopCancelTestSuite))
}
