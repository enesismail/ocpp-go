package ws

import (
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"
)

// These tests cover S4 (ws server connection-lifecycle hygiene, #292/#387):
// handleDisconnect must (1) only delete a s.connections entry if it is still
// the socket disconnecting ("delete-if-me", no clobber of a newer entry for
// the same ID) and (2) must not fire disconnectedHandler for a socket that
// has already been superseded (no stale "disconnected" event observed after
// a newer "connected" event for the same ID).
//
// Tests 1 and 2 are deterministic package-internal tests: they establish one
// real connection (so s.connections[id] holds a genuine *webSocket that a
// real disconnect could legitimately touch), then synthesize a distinct stale
// *webSocket sharing the same ID and invoke s.handleDisconnect on it directly
// -- reproducing the "superseded socket disconnects late" race without any
// sleeps or timing-sensitive real reconnects. handleDisconnect only ever
// touches w.ID() before doing the identity check, so a bare &webSocket{id: id}
// literal is a valid, minimal stand-in for the stale socket.
//
// Coverage boundary (see PATCHES.md "Server connection-lifecycle hygiene"):
// these cover the primary `!isCurrent` guard deterministically. The SECOND
// suppression -- the re-check between handleDisconnect's own delete and the
// callback (ws/server.go, `superseded`) -- fires when a reconnector that has
// already finished its handshake is parked at connMutex.Lock and inserts inside
// the small delete->re-check window. That branch is real (it is the piece with
// live value today) but is not deterministically reproducible from a black-box
// test without a production test-seam between the delete-unlock and the
// re-check. It is left as a documented, accepted belt-and-suspenders guard; the
// zero-window replacement is the D2-time event-loop, not S4.

// disconnectRecorder is a mutex-guarded counter/flag used instead of an
// unbuffered channel, so that if disconnectedHandler is unexpectedly invoked
// asynchronously (e.g. by TearDownTest's real Stop()), recording it can never
// block and deadlock the suite's teardown.
type disconnectRecorder struct {
	mu    sync.Mutex
	count int
}

func (r *disconnectRecorder) record() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.count++
}

func (r *disconnectRecorder) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

// connectRealClientAndGetChannel connects a single real client to s.server
// (already started by the caller) for wsID, waiting on triggerC for the
// server's newClientHandler to confirm registration. It returns the real
// *webSocket registered in s.connections[wsID].
func (s *WebSocketSuite) connectRealClientAndGetChannel(wsID string, triggerC chan struct{}, port int) *webSocket {
	host := fmt.Sprintf("localhost:%v", port)
	u := url.URL{Scheme: "ws", Host: host, Path: testPath}
	s.client = newWebsocketClient(s.T(), func(data []byte) ([]byte, error) {
		return nil, nil
	})
	err := s.client.Start(u.String())
	s.Require().NoError(err)
	_, ok := <-triggerC
	s.Require().True(ok)
	channel, ok := s.server.GetChannel(wsID)
	s.Require().True(ok)
	s.Require().NotNil(channel)
	newWs, ok := channel.(*webSocket)
	s.Require().True(ok)
	return newWs
}

// TestHandleDisconnectSupersededSuppressed is S4 test 1: a stale socket
// (superseded by a newer registration for the same ID) must not fire
// disconnectedHandler, and must not clobber the current entry.
func (s *WebSocketSuite) TestHandleDisconnectSupersededSuppressed() {
	wsID := "testws"
	triggerC := make(chan struct{}, 1)
	rec := &disconnectRecorder{}
	s.server = newWebsocketServer(s.T(), nil)
	s.server.SetNewClientHandler(func(ws Channel) {
		triggerC <- struct{}{}
	})
	s.server.SetDisconnectedClientHandler(func(ws Channel) {
		rec.record()
	})
	port := s.startServer(s.server, serverPath)

	newWs := s.connectRealClientAndGetChannel(wsID, triggerC, port)
	s.Require().Equal(1, connectionCount(s.server))

	// Synthesize a distinct stale *webSocket sharing the same ID and drive
	// handleDisconnect directly, as if it were the delayed cleanup of an
	// already-superseded prior connection for this ID.
	staleWs := &webSocket{id: wsID}
	s.server.handleDisconnect(staleWs, errors.New("stale disconnect"))

	// Disconnect must be suppressed (no event for the superseded socket)...
	s.Equal(0, rec.Count())
	// ...and the current, live entry must be unchanged (no clobber).
	channel, ok := s.server.GetChannel(wsID)
	s.True(ok)
	s.Same(Channel(newWs), channel)
	s.Equal(1, connectionCount(s.server))
}

// TestHandleDisconnectDeleteIfMeNoClobber is S4 test 2: delete-if-me must not
// remove/clobber a newer entry registered under the same ID.
func (s *WebSocketSuite) TestHandleDisconnectDeleteIfMeNoClobber() {
	wsID := "testws"
	triggerC := make(chan struct{}, 1)
	rec := &disconnectRecorder{}
	s.server = newWebsocketServer(s.T(), nil)
	s.server.SetNewClientHandler(func(ws Channel) {
		triggerC <- struct{}{}
	})
	s.server.SetDisconnectedClientHandler(func(ws Channel) {
		rec.record()
	})
	port := s.startServer(s.server, serverPath)

	newWs := s.connectRealClientAndGetChannel(wsID, triggerC, port)

	// A different pointer, same ID: must not delete/clobber the real entry.
	staleWs := &webSocket{id: wsID}
	s.server.handleDisconnect(staleWs, errors.New("stale disconnect"))

	s.server.connMutex.RLock()
	current, ok := s.server.connections[wsID]
	s.server.connMutex.RUnlock()
	s.True(ok)
	s.Same(newWs, current)
	s.Equal(0, rec.Count())
	s.Equal(1, connectionCount(s.server))
}

// TestHandleDisconnectNormalFiresOnce is S4 test 3: a normal, single
// connect->disconnect must still fire disconnectedHandler exactly once and
// fully drain the connections map (guards against over-suppression).
func (s *WebSocketSuite) TestHandleDisconnectNormalFiresOnce() {
	triggerC := make(chan struct{}, 1)
	disconnectedServerC := make(chan struct{}, 1)
	rec := &disconnectRecorder{}
	s.server = newWebsocketServer(s.T(), nil)
	s.server.SetNewClientHandler(func(ws Channel) {
		triggerC <- struct{}{}
	})
	s.server.SetDisconnectedClientHandler(func(ws Channel) {
		rec.record()
		disconnectedServerC <- struct{}{}
	})
	s.client = newWebsocketClient(s.T(), nil)
	port := s.startServer(s.server, serverPath)

	host := fmt.Sprintf("localhost:%v", port)
	u := url.URL{Scheme: "ws", Host: host, Path: testPath}
	err := s.client.Start(u.String())
	s.Require().NoError(err)
	_, ok := <-triggerC
	s.Require().True(ok)
	s.Equal(1, connectionCount(s.server))

	// Real disconnect: stop the client, which closes the connection and lets
	// the server's writePump run cleanup -> handleDisconnect asynchronously.
	s.client.Stop()
	_, ok = <-disconnectedServerC
	s.Require().True(ok)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && connectionCount(s.server) != 0 {
		time.Sleep(10 * time.Millisecond)
	}
	s.Equal(0, connectionCount(s.server))
	s.Equal(1, rec.Count())
}
