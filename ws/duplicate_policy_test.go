package ws

import (
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

const duplicatePolicyWait = 5 * time.Second

func waitForDuplicatePolicyCondition(t *testing.T, name string, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(duplicatePolicyWait)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s", name)
		case <-tick.C:
		}
	}
}

func startDuplicatePolicyServer(t *testing.T, s *server) {
	t.Helper()
	go s.Start(serverPort, serverPath)
	waitForDuplicatePolicyCondition(t, "server listen address", func() bool {
		return s.Addr() != nil
	})
}

func duplicatePolicyURL() string {
	host := fmt.Sprintf("localhost:%v", serverPort)
	u := url.URL{Scheme: "ws", Host: host, Path: testPath}
	return u.String()
}

func connectDuplicatePolicyClient(t *testing.T, c *client) {
	t.Helper()
	require.NoError(t, c.Start(duplicatePolicyURL()))
}

func receiveDuplicatePolicySignal(t *testing.T, name string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(duplicatePolicyWait):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func receiveDuplicatePolicyID(t *testing.T, name string, ch <-chan string) string {
	t.Helper()
	select {
	case id := <-ch:
		return id
	case <-time.After(duplicatePolicyWait):
		t.Fatalf("timed out waiting for %s", name)
		return ""
	}
}

func newDuplicatePolicyClient(t *testing.T) *client {
	t.Helper()
	c := newWebsocketClient(t, nil)
	c.SetRequestedSubProtocol(defaultSubProtocol)
	return c
}

// TestDuplicatePolicyDefaultRejectNewUnchanged covers PR-D2 test 5. Without
// opting in to PR-D2 KeepNew, a duplicate connection for the same ID is still
// rejected and the original socket remains current.
func TestDuplicatePolicyDefaultRejectNewUnchanged(t *testing.T) {
	s := newWebsocketServer(t, nil)
	defer s.Stop()
	connectedC := make(chan string, 2)
	s.SetNewClientHandler(func(ch Channel) {
		connectedC <- ch.ID()
	})
	startDuplicatePolicyServer(t, s)

	oldClient := newDuplicatePolicyClient(t)
	defer oldClient.Stop()
	connectDuplicatePolicyClient(t, oldClient)
	require.Equal(t, "testws", receiveDuplicatePolicyID(t, "initial connect", connectedC))

	newClient := newDuplicatePolicyClient(t)
	defer newClient.Stop()
	// Finding 7: the rejected duplicate must not auto-reconnect and retry
	// forever, which would make "exactly one winner stays rejected" unstable.
	newClient.SetAutoReconnect(false)
	connectDuplicatePolicyClient(t, newClient)

	waitForDuplicatePolicyCondition(t, "duplicate rejection", func() bool {
		return !newClient.IsConnected() && connectionCount(s) == 1
	})
	require.True(t, oldClient.IsConnected())
	select {
	case id := <-connectedC:
		t.Fatalf("duplicate unexpectedly reached newClientHandler for %s", id)
	default:
	}
}

// TestDuplicatePolicyKeepNewEvictsOldAcceptsNew covers PR-D2 test 6. The
// future policy option must tear down the old connection before registering
// the replacement, leaving exactly one active socket and firing the new-client
// handler for the replacement.
func TestDuplicatePolicyKeepNewEvictsOldAcceptsNew(t *testing.T) {
	// PR-D2: WithDuplicateConnectionPolicy/KeepNew are the intended public surface.
	srv := NewServer(WithDuplicateConnectionPolicy(KeepNew))
	s, ok := srv.(*server)
	require.True(t, ok)
	defer s.Stop()
	connectedC := make(chan string, 2)
	s.SetNewClientHandler(func(ch Channel) {
		connectedC <- ch.ID()
	})
	startDuplicatePolicyServer(t, s)

	oldClient := newDuplicatePolicyClient(t)
	defer oldClient.Stop()
	connectDuplicatePolicyClient(t, oldClient)
	require.Equal(t, "testws", receiveDuplicatePolicyID(t, "initial connect", connectedC))

	newClient := newDuplicatePolicyClient(t)
	defer newClient.Stop()
	connectDuplicatePolicyClient(t, newClient)
	require.Equal(t, "testws", receiveDuplicatePolicyID(t, "replacement connect", connectedC))

	waitForDuplicatePolicyCondition(t, "old evicted and new retained", func() bool {
		return !oldClient.IsConnected() && newClient.IsConnected() && connectionCount(s) == 1
	})
}

// TestDuplicatePolicyTransitionGateRejectsNaturalDisconnectWindow covers
// PR-D2 test 7. A reconnect that lands after a natural disconnect freed the
// map slot but before the disconnect teardown chain returns must be rejected
// for both policies; the old normal disconnect path must still run.
func TestDuplicatePolicyTransitionGateRejectsNaturalDisconnectWindow(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []ServerOpt
	}{
		{name: "KeepCurrentDefault"},
		// PR-D2: KeepNew still rejects the slot-absent-but-gated natural-disconnect window.
		{name: "KeepNew", opts: []ServerOpt{WithDuplicateConnectionPolicy(KeepNew)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewServer(tc.opts...)
			s, ok := srv.(*server)
			require.True(t, ok)
			defer s.Stop()
			enteredDisconnect := make(chan struct{}, 1)
			releaseDisconnect := make(chan struct{})
			disconnectedC := make(chan struct{}, 1)
			s.SetDisconnectedClientHandler(func(ch Channel) {
				enteredDisconnect <- struct{}{}
				<-releaseDisconnect
				disconnectedC <- struct{}{}
			})
			startDuplicatePolicyServer(t, s)

			oldClient := newDuplicatePolicyClient(t)
			defer oldClient.Stop()
			connectDuplicatePolicyClient(t, oldClient)
			oldClient.Stop()
			require.Eventually(t, func() bool { return connectionCount(s) == 0 }, duplicatePolicyWait, 10*time.Millisecond)
			receiveDuplicatePolicySignal(t, "disconnect handler entry", enteredDisconnect)

			reconnect := newDuplicatePolicyClient(t)
			defer reconnect.Stop()
			connectDuplicatePolicyClient(t, reconnect)
			waitForDuplicatePolicyCondition(t, "gated reconnect rejection", func() bool {
				return !reconnect.IsConnected()
			})

			close(releaseDisconnect)
			receiveDuplicatePolicySignal(t, "disconnect handler completion", disconnectedC)
		})
	}
}

// TestDuplicatePolicyOldDisconnectDoesNotClobberNew covers PR-D2 test 11. A
// late disconnect from the evicted socket must not delete the replacement or
// emit an extra disconnected event for the live ID.
func TestDuplicatePolicyOldDisconnectDoesNotClobberNew(t *testing.T) {
	// PR-D2: WithDuplicateConnectionPolicy/KeepNew are the intended public surface.
	srv := NewServer(WithDuplicateConnectionPolicy(KeepNew))
	s, ok := srv.(*server)
	require.True(t, ok)
	defer s.Stop()
	disconnectedC := make(chan string, 4)
	s.SetDisconnectedClientHandler(func(ch Channel) {
		disconnectedC <- ch.ID()
	})
	startDuplicatePolicyServer(t, s)

	oldClient := newDuplicatePolicyClient(t)
	defer oldClient.Stop()
	connectDuplicatePolicyClient(t, oldClient)
	oldChannel, ok := s.GetChannel("testws")
	require.True(t, ok)
	oldWS := oldChannel.(*webSocket)

	newClient := newDuplicatePolicyClient(t)
	defer newClient.Stop()
	connectDuplicatePolicyClient(t, newClient)
	waitForDuplicatePolicyCondition(t, "replacement current", func() bool {
		ch, ok := s.GetChannel("testws")
		return ok && ch != oldChannel && newClient.IsConnected()
	})

	// Finding 1: the eviction itself is a LEGITIMATE disconnect of old (old was
	// still current when its teardown ran) -- handleDisconnect fires
	// disconnectedHandler(old) exactly once for it, and that event sits
	// buffered in disconnectedC by the time new is current. Drain and assert
	// exactly that one legit event BEFORE probing the manual late-disconnect
	// case below, or the legit event is misread as the spurious one.
	require.Equal(t, "testws", receiveDuplicatePolicyID(t, "legit eviction disconnect for old", disconnectedC))

	// Now simulate a late, redundant handleDisconnect call for old (e.g. its
	// own cleanup running again) arriving AFTER the legit eviction disconnect
	// has already been observed: it must not clobber new, and must not emit a
	// second (spurious) disconnected event.
	s.handleDisconnect(oldWS, websocket.ErrCloseSent)
	ch, ok := s.GetChannel("testws")
	require.True(t, ok)
	require.NotSame(t, oldWS, ch)
	require.Equal(t, 1, connectionCount(s))
	select {
	case id := <-disconnectedC:
		t.Fatalf("late old disconnect emitted a second, spurious event for %s", id)
	default:
	}
}

// TestD2HandleMessageDropsLateInboundFromSupersededSocket covers PR-D2 test
// 10 (moved here from the facade layer -- Finding 2). The original facade
// test injected the late old frame via mockWsServer.MessageHandler, which
// bypasses the real ws.server.handleMessage entirely -- so it could never
// validate the item-5 currentness guard the guard is supposed to exercise.
// This white-box test drives s.handleMessage directly, mirroring
// server_reconnect_test.go's pattern (a synthesized *webSocket sharing an ID
// already registered, driven straight at the package-internal method): it
// registers a synthesized "new" *webSocket as the current connections[id]
// entry, then calls s.handleMessage with a DIFFERENT *webSocket for the same
// id (standing in for old's superseded socket) and asserts the message
// handler is NOT invoked -- the item-5 `s.connections[w.ID()] == w`
// currentness guard must drop it. A positive control (same call via the
// current socket) proves the guard isn't just globally swallowing messages.
func TestD2HandleMessageDropsLateInboundFromSupersededSocket(t *testing.T) {
	wsID := "testws"
	s := newWebsocketServer(t, nil)
	var invoked int32
	s.SetMessageHandler(func(ch Channel, data []byte) error {
		atomic.AddInt32(&invoked, 1)
		return nil
	})

	// Register newWs as the CURRENT connections[wsID] entry, as the real
	// accept path does after eviction registers the replacement
	// (ws/server.go wsHandler, connections[ws.id] = ws).
	newWs := &webSocket{id: wsID}
	s.connMutex.Lock()
	s.connections[wsID] = newWs
	s.connMutex.Unlock()

	// A distinct *webSocket sharing the same ID stands in for old's
	// superseded socket -- late inbound data that finished reading off the
	// wire after new registered (PR-D2 item 5).
	staleOldWs := &webSocket{id: wsID}
	require.NoError(t, s.handleMessage(staleOldWs, []byte("late old frame")))
	require.Equal(t, int32(0), atomic.LoadInt32(&invoked),
		"handleMessage must drop inbound data from a socket superseded in s.connections")

	// Positive control: the same call via the current socket must still reach
	// the handler -- the guard must not globally suppress delivery.
	require.NoError(t, s.handleMessage(newWs, []byte("current frame")))
	require.Equal(t, int32(1), atomic.LoadInt32(&invoked),
		"handleMessage must still deliver inbound data from the current socket")
}

// TestDuplicatePolicyConcurrentDuplicatesSingleWinner covers PR-D2 test 12.
// Concurrent replacements for one ID must leave exactly one live socket; all
// losers must be rejected/closed rather than orphaned.
func TestDuplicatePolicyConcurrentDuplicatesSingleWinner(t *testing.T) {
	for _, n := range []int{2, 3} {
		t.Run(fmt.Sprintf("%d_new_duplicates", n), func(t *testing.T) {
			// PR-D2: WithDuplicateConnectionPolicy/KeepNew are the intended public surface.
			srv := NewServer(WithDuplicateConnectionPolicy(KeepNew))
			s, ok := srv.(*server)
			require.True(t, ok)
			defer s.Stop()
			startDuplicatePolicyServer(t, s)

			oldClient := newDuplicatePolicyClient(t)
			defer oldClient.Stop()
			// Finding 7: oldClient is the loser of the eviction race here too --
			// without disabling auto-reconnect it could reconnect and evict the
			// settled winner, making "exactly one winner" unstable.
			oldClient.SetAutoReconnect(false)
			connectDuplicatePolicyClient(t, oldClient)

			clients := make([]*client, n)
			var wg sync.WaitGroup
			startC := make(chan struct{})
			for i := range clients {
				clients[i] = newDuplicatePolicyClient(t)
				defer clients[i].Stop()
				// Finding 7: every contender but the eventual winner is a loser
				// that must not auto-reconnect and re-evict.
				clients[i].SetAutoReconnect(false)
				wg.Add(1)
				go func(c *client) {
					defer wg.Done()
					<-startC
					_ = c.Start(duplicatePolicyURL())
				}(clients[i])
			}
			close(startC)
			wg.Wait()

			waitForDuplicatePolicyCondition(t, "single duplicate winner", func() bool {
				live := 0
				for _, c := range clients {
					if c.IsConnected() {
						live++
					}
				}
				return live == 1 && !oldClient.IsConnected() && connectionCount(s) == 1
			})
		})
	}
}

// duplicatePolicyEvictionLatchTimeout is a short, test-only eviction latch
// timeout (Finding 4 / spec design decision 2): production defaults to a
// floor of >= WriteWait(~10s) + DeleteClientAndWait's 2s + a disconnect-
// handler budget (~12s+), which would make the barrier-timeout fallback tests
// impractically slow (and, at the previous 5s duplicatePolicyWait, a false
// fail, since the wait timed out before the floor could ever be reached).
// WithDuplicateConnectionEvictionTimeout is the required test hook that makes
// the eviction latch timeout construction-configurable, so tests can set it
// well under the production floor for a fast, deterministic fallback-reject.
const duplicatePolicyEvictionLatchTimeout = 300 * time.Millisecond

// TestDuplicatePolicyBarrierTimeoutBlockingDisconnectHandler covers PR-D2 test
// 13a. A blocking user disconnect handler must make eviction fail safe by
// rejecting the new connection, while the transition gate is released after
// the timeout so the ID can connect after teardown completes.
func TestDuplicatePolicyBarrierTimeoutBlockingDisconnectHandler(t *testing.T) {
	// PR-D2: WithDuplicateConnectionPolicy/KeepNew are the intended public
	// surface; WithDuplicateConnectionEvictionTimeout is the red-first test
	// hook added per Finding 4 (not yet implemented -- see
	// tasks/d2-evict-old-duplicate-policy.md PR-D2 item 3 / design decision 2).
	srv := NewServer(
		WithDuplicateConnectionPolicy(KeepNew),
		WithDuplicateConnectionEvictionTimeout(duplicatePolicyEvictionLatchTimeout),
	)
	s, ok := srv.(*server)
	require.True(t, ok)
	defer s.Stop()
	enteredDisconnect := make(chan struct{}, 1)
	releaseDisconnect := make(chan struct{})
	s.SetDisconnectedClientHandler(func(ch Channel) {
		enteredDisconnect <- struct{}{}
		<-releaseDisconnect
	})
	startDuplicatePolicyServer(t, s)

	oldClient := newDuplicatePolicyClient(t)
	defer oldClient.Stop()
	connectDuplicatePolicyClient(t, oldClient)

	firstReplacement := newDuplicatePolicyClient(t)
	defer firstReplacement.Stop()
	connectDuplicatePolicyClient(t, firstReplacement)
	receiveDuplicatePolicySignal(t, "disconnect handler entry", enteredDisconnect)
	waitForDuplicatePolicyCondition(t, "timeout rejected replacement", func() bool {
		return !firstReplacement.IsConnected()
	})

	close(releaseDisconnect)
	fresh := newDuplicatePolicyClient(t)
	defer fresh.Stop()
	connectDuplicatePolicyClient(t, fresh)
	waitForDuplicatePolicyCondition(t, "id connectable after timeout cleanup", func() bool {
		return fresh.IsConnected() && connectionCount(s) == 1
	})
}

// TestDuplicatePolicyKeepNewStopRejectsReplacementAfterShutdownPass covers the
// Stop race where a KeepNew replacement is upgraded but not yet registered
// while eviction waits for the old socket's teardown. Stop's one-shot close
// pass must prevent that replacement from registering after the pass has
// already missed it.
func TestDuplicatePolicyKeepNewStopRejectsReplacementAfterShutdownPass(t *testing.T) {
	srv := NewServer(WithDuplicateConnectionPolicy(KeepNew))
	s, ok := srv.(*server)
	require.True(t, ok)

	connectedC := make(chan string, 2)
	s.SetNewClientHandler(func(ch Channel) {
		connectedC <- ch.ID()
	})
	enteredDisconnect := make(chan struct{}, 1)
	releaseDisconnect := make(chan struct{})
	s.SetDisconnectedClientHandler(func(ch Channel) {
		enteredDisconnect <- struct{}{}
		<-releaseDisconnect
	})
	startDuplicatePolicyServer(t, s)

	oldClient := newDuplicatePolicyClient(t)
	oldClient.SetAutoReconnect(false)
	defer oldClient.Stop()
	connectDuplicatePolicyClient(t, oldClient)
	require.Equal(t, "testws", receiveDuplicatePolicyID(t, "initial connect", connectedC))

	replacement := newDuplicatePolicyClient(t)
	replacement.SetAutoReconnect(false)
	defer replacement.Stop()
	connectDuplicatePolicyClient(t, replacement)
	receiveDuplicatePolicySignal(t, "old disconnect handler entry", enteredDisconnect)

	stopDone := make(chan struct{})
	go func() {
		s.Stop()
		close(stopDone)
	}()

	waitForDuplicatePolicyCondition(t, "shutdown close pass", func() bool {
		s.connMutex.RLock()
		stopped := s.stopped
		s.connMutex.RUnlock()
		return stopped
	})

	close(releaseDisconnect)
	receiveDuplicatePolicySignal(t, "server stop completion", stopDone)

	waitForDuplicatePolicyCondition(t, "replacement rejection after stop", func() bool {
		return !replacement.IsConnected() && connectionCount(s) == 0
	})
	select {
	case id := <-connectedC:
		t.Fatalf("replacement unexpectedly reached newClientHandler after Stop for %s", id)
	default:
	}
}

// Finding 5: TestDuplicatePolicyBarrierTimeoutWedgedPumpFallback (formerly
// here, covering PR-D2 test 13b at the ws boundary) has been removed. It
// blocked the same thing as 13a above -- the USER DISCONNECT HANDLER -- not
// the dispatcher pump, so it was a mislabeled duplicate of 13a rather than a
// real wedged-pump test. The ws server's dispatcher is internal (owned by
// ocppj, wired in via onClientConnected/onClientDisconnected), so genuinely
// wedging the real pump is not reachable from this package; the facade-level
// companion, TestD2WedgedPumpFallbackFreshConnectionDispatchesFirstRequest
// (ocpp1.6_test/d2_duplicate_policy_test.go), drives the real pump stall via
// the mock ws server/dispatcher and is the correct home for that case.

// TestDuplicatePolicyNoDeadlockUnderConcurrentEvictionLoad covers PR-D2 test
// 14. Repeated duplicate evictions plus Stop must never wedge connMutex,
// websocket cleanup, or the server stop path.
func TestDuplicatePolicyNoDeadlockUnderConcurrentEvictionLoad(t *testing.T) {
	// PR-D2: WithDuplicateConnectionPolicy/KeepNew are the intended public surface.
	srv := NewServer(WithDuplicateConnectionPolicy(KeepNew))
	s, ok := srv.(*server)
	require.True(t, ok)
	startDuplicatePolicyServer(t, s)

	oldClient := newDuplicatePolicyClient(t)
	connectDuplicatePolicyClient(t, oldClient)
	defer oldClient.Stop()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := newDuplicatePolicyClient(t)
			defer c.Stop()
			_ = c.Start(duplicatePolicyURL())
		}()
	}
	doneC := make(chan struct{})
	go func() {
		wg.Wait()
		s.Stop()
		close(doneC)
	}()
	select {
	case <-doneC:
	case <-time.After(duplicatePolicyWait):
		t.Fatal("concurrent duplicate eviction load deadlocked")
	}
}
