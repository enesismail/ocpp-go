package ocpp2_test

import (
	"bytes"
	"fmt"
	"net"
	"net/url"
	"testing"
	"time"

	ocpp2 "github.com/enesismail/ocpp-go/ocpp2.0.1"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/types"
	"github.com/enesismail/ocpp-go/ws"
)

// reservedPort binds a TCP listener on ":0" (all interfaces, same shape
// Start uses), reads the OS-assigned port, and closes the listener. The
// probe-then-release TOCTOU window is accepted (same trade the ws suite makes).
func reservedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("reservedPort: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

func csAwaitListening(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", port), 100*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server did not start listening")
}

func csStopOnCleanup(t *testing.T, name string, stop func()) {
	t.Helper()
	t.Cleanup(func() {
		doneC := make(chan struct{})
		go func() {
			stop()
			close(doneC)
		}()
		select {
		case <-doneC:
		case <-time.After(2 * time.Second):
			t.Errorf("%s Stop did not return", name)
		}
	})
}

// authorizeFrame returns a valid OCPP-J CALL frame for Authorize with the
// given message ID. The payload is minimal but passes OCPP 2.0.1 validation.
func authorizeFrame(msgID string) []byte {
	return []byte(fmt.Sprintf(
		`[2,"%s","Authorize",{"idToken":{"idToken":"test","type":"Central"}}]`,
		msgID,
	))
}

// TestFacadeReadLimitThroughSuppliedServer proves that a ReadLimit applied via
// ws.NewServer(ws.WithServerReadLimit(n)) — a server the caller constructs and
// passes to NewCSMS — reaches the live connection. It also proves WriteWait is
// not zeroed (the under-limit arm round-trips a response, which requires a live
// write deadline).
func TestFacadeReadLimitThroughSuppliedServer(t *testing.T) {
	const limit int64 = 1024
	srv := ws.NewServer(ws.WithServerReadLimit(limit))
	cs := ocpp2.NewCSMS(nil, srv)

	disconnectedC := make(chan struct{}, 1)
	cs.SetChargingStationDisconnectedHandler(func(_ ocpp2.ChargingStationConnection) {
		disconnectedC <- struct{}{}
	})

	// The listen path is a mux pattern and each client dials a DISTINCT id:
	// the channel identity is path.Base(url.Path) (ws/server.go), so two
	// clients on one fixed path would collide on the same charging-station id
	// and the second would be rejected as a duplicate connection (default
	// KeepCurrent policy) without ever registering — its drop then never
	// reaches the disconnected handler this test asserts on.
	csStopOnCleanup(t, "CSMS", cs.Stop)
	go cs.Start(0, "/ws/{id}")

	// Poll srv.Addr() for the bound port.
	var port int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if addr := srv.Addr(); addr != nil {
			port = addr.Port
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if port == 0 {
		t.Fatal("server did not start listening")
	}

	u := url.URL{Scheme: "ws", Host: fmt.Sprintf("localhost:%d", port), Path: "/ws/cs-under"}

	// --- Arm (a): under-limit valid frame round-trips ---
	client := ws.NewClient()
	client.SetAutoReconnect(false)
	client.SetRequestedSubProtocol(types.V201Subprotocol)

	received := make(chan []byte, 1)
	client.SetMessageHandler(func(data []byte) error {
		received <- data
		return nil
	})

	err := client.Start(u.String())
	if err != nil {
		t.Fatalf("client.Start: %v", err)
	}
	defer client.Stop()

	err = client.Write(authorizeFrame("msg-a"))
	if err != nil {
		t.Fatalf("client.Write (under-limit): %v", err)
	}

	select {
	case data := <-received:
		if len(data) == 0 {
			t.Fatal("received empty response for under-limit frame")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for Authorize response (WriteWait may be zeroed)")
	}

	// --- Arm (b): over-limit write drops the connection ---
	client2 := ws.NewClient()
	client2.SetAutoReconnect(false)
	client2.SetRequestedSubProtocol(types.V201Subprotocol)

	u.Path = "/ws/cs-over"
	err = client2.Start(u.String())
	if err != nil {
		t.Fatalf("client2.Start: %v", err)
	}
	defer client2.Stop()

	oversized := bytes.Repeat([]byte("x"), int(limit*4))
	err = client2.Write(oversized)
	if err != nil {
		t.Fatalf("client2.Write (over-limit): %v", err)
	}

	select {
	case <-disconnectedC:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for server to drop the over-limit connection")
	}
}

// TestNewDefaultCSMSAppliesReadLimit is the constructor-wiring arm that uses
// NewDefaultCSMS(WithServerReadLimit(…)) and a reserved port. It asserts the
// limited system drops an over-limit write, and a control (unlimited) system
// does not.
func TestNewDefaultCSMSAppliesReadLimit(t *testing.T) {
	const limit int64 = 1024
	oversized := bytes.Repeat([]byte("z"), int(limit*4))

	// --- Limited system ---
	limitedPort := reservedPort(t)
	limitedCS := ocpp2.NewDefaultCSMS(ocpp2.WithServerReadLimit(limit))

	limitedDisconnectedC := make(chan struct{}, 1)
	limitedCS.SetChargingStationDisconnectedHandler(func(_ ocpp2.ChargingStationConnection) {
		limitedDisconnectedC <- struct{}{}
	})

	csStopOnCleanup(t, "limited CSMS", limitedCS.Stop)
	go limitedCS.Start(limitedPort, "/ws")

	// --- Control (unlimited) system ---
	// reservedPort probes and releases, so two draws can collide. On a collision
	// the control server never binds, awaitListening succeeds against the LIMITED
	// server, and the control arm below passes vacuously — re-draw instead.
	controlPort := reservedPort(t)
	for i := 0; controlPort == limitedPort && i < 10; i++ {
		controlPort = reservedPort(t)
	}
	if controlPort == limitedPort {
		t.Fatalf("could not reserve a control port distinct from the limited port %d", limitedPort)
	}
	controlCS := ocpp2.NewDefaultCSMS()

	controlDisconnectedC := make(chan struct{}, 1)
	controlCS.SetChargingStationDisconnectedHandler(func(_ ocpp2.ChargingStationConnection) {
		controlDisconnectedC <- struct{}{}
	})

	csStopOnCleanup(t, "control CSMS", controlCS.Stop)
	go controlCS.Start(controlPort, "/ws")

	csAwaitListening(t, limitedPort)
	csAwaitListening(t, controlPort)

	// --- Dial limited system, write oversized → should disconnect ---
	limitedClient := ws.NewClient()
	limitedClient.SetAutoReconnect(false)
	limitedClient.SetRequestedSubProtocol(types.V201Subprotocol)

	limitedURL := url.URL{Scheme: "ws", Host: fmt.Sprintf("localhost:%d", limitedPort), Path: "/ws"}
	err := limitedClient.Start(limitedURL.String())
	if err != nil {
		t.Fatalf("limitedClient.Start: %v", err)
	}
	defer limitedClient.Stop()

	err = limitedClient.Write(oversized)
	if err != nil {
		t.Fatalf("limitedClient.Write: %v", err)
	}

	select {
	case <-limitedDisconnectedC:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for limited system to drop the over-limit connection")
	}

	// --- Dial control system, write same oversized → should NOT disconnect ---
	controlClient := ws.NewClient()
	controlClient.SetAutoReconnect(false)
	controlClient.SetRequestedSubProtocol(types.V201Subprotocol)

	controlURL := url.URL{Scheme: "ws", Host: fmt.Sprintf("localhost:%d", controlPort), Path: "/ws"}
	err = controlClient.Start(controlURL.String())
	if err != nil {
		t.Fatalf("controlClient.Start: %v", err)
	}
	defer controlClient.Stop()

	err = controlClient.Write(oversized)
	if err != nil {
		t.Fatalf("controlClient.Write (control): %v", err)
	}

	select {
	case <-controlDisconnectedC:
		t.Fatal("unlimited control system unexpectedly dropped the connection")
	case <-time.After(2 * time.Second):
		// Expected: no disconnect, message was delivered.
	}

	// The absence of a disconnect only means something if the control client is
	// still connected: a client that was rejected at dial (or dropped by some
	// other server) would also produce no event on controlDisconnectedC.
	if !controlClient.IsConnected() {
		t.Fatal("control client is not connected: the no-disconnect arm passed vacuously")
	}
}

// TestNewDefaultCSMSNoOptsMatchesLegacyDefault is a cheap smoke test:
// NewDefaultCSMS() returns a non-nil CSMS that starts and stops cleanly.
func TestNewDefaultCSMSNoOptsMatchesLegacyDefault(t *testing.T) {
	cs := ocpp2.NewDefaultCSMS()
	if cs == nil {
		t.Fatal("NewDefaultCSMS returned nil")
	}

	port := reservedPort(t)
	csStopOnCleanup(t, "CSMS", cs.Stop)
	go cs.Start(port, "/ws")

	csAwaitListening(t, port)
}
