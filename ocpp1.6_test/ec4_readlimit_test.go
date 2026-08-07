package ocpp16_test

import (
	"bytes"
	"fmt"
	"net"
	"net/url"
	"testing"
	"time"

	ocpp16 "github.com/enesismail/ocpp-go/ocpp1.6"
	"github.com/enesismail/ocpp-go/ocpp1.6/types"
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

func cpAwaitListening(t *testing.T, port int) {
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

func cpStopOnCleanup(t *testing.T, name string, stop func()) {
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

// bootNotificationFrame returns a valid OCPP-J CALL frame for
// BootNotification with the given message ID. The payload is minimal but
// passes OCPP validation.
func bootNotificationFrame(msgID string) []byte {
	return []byte(fmt.Sprintf(
		`[2,"%s","BootNotification",{"chargePointVendor":"test","chargePointModel":"test"}]`,
		msgID,
	))
}

// TestFacadeReadLimitThroughSuppliedServer proves that a ReadLimit applied via
// ws.NewServer(ws.WithReadLimit(n)) — a server the caller constructs and passes
// to NewCentralSystem — reaches the live connection. It also proves WriteWait is
// not zeroed (the under-limit arm round-trips a response, which requires a live
// write deadline).
func TestFacadeReadLimitThroughSuppliedServer(t *testing.T) {
	const limit int64 = 1024
	srv := ws.NewServer(ws.WithReadLimit(limit))
	cs := ocpp16.NewCentralSystem(nil, srv)

	disconnectedC := make(chan struct{}, 1)
	cs.SetChargePointDisconnectedHandler(func(_ ocpp16.ChargePointConnection) {
		disconnectedC <- struct{}{}
	})

	// The listen path is a mux pattern and each client dials a DISTINCT id:
	// the channel identity is path.Base(url.Path) (ws/server.go), so two
	// clients on one fixed path would collide on the same charge-point id and
	// the second would be rejected as a duplicate connection (default
	// KeepCurrent policy) without ever registering — its drop then never
	// reaches the disconnected handler this test asserts on.
	cpStopOnCleanup(t, "central system", cs.Stop)
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

	u := url.URL{Scheme: "ws", Host: fmt.Sprintf("localhost:%d", port), Path: "/ws/cp-under"}

	// --- Arm (a): under-limit valid frame round-trips ---
	client := ws.NewClient()
	client.SetAutoReconnect(false)
	client.SetRequestedSubProtocol(types.V16Subprotocol)

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

	err = client.Write(bootNotificationFrame("msg-a"))
	if err != nil {
		t.Fatalf("client.Write (under-limit): %v", err)
	}

	select {
	case data := <-received:
		if len(data) == 0 {
			t.Fatal("received empty response for under-limit frame")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for BootNotification response (WriteWait may be zeroed)")
	}

	// --- Arm (b): over-limit write drops the connection ---
	client2 := ws.NewClient()
	client2.SetAutoReconnect(false)
	client2.SetRequestedSubProtocol(types.V16Subprotocol)

	u.Path = "/ws/cp-over"
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

// TestNewDefaultCentralSystemAppliesReadLimit is the constructor-wiring arm
// that uses NewDefaultCentralSystem(WithReadLimit(…)) and a reserved port.
// It asserts the limited system drops an over-limit write, and a control
// (unlimited) system does not — distinguishing "option was applied" from
// "the message was too big for some other reason".
func TestNewDefaultCentralSystemAppliesReadLimit(t *testing.T) {
	const limit int64 = 1024
	oversized := bytes.Repeat([]byte("z"), int(limit*4))

	// --- Limited system ---
	limitedPort := reservedPort(t)
	limitedCS := ocpp16.NewDefaultCentralSystem(ocpp16.WithReadLimit(limit))

	limitedDisconnectedC := make(chan struct{}, 1)
	limitedCS.SetChargePointDisconnectedHandler(func(_ ocpp16.ChargePointConnection) {
		limitedDisconnectedC <- struct{}{}
	})

	cpStopOnCleanup(t, "limited central system", limitedCS.Stop)
	go limitedCS.Start(limitedPort, "/ws")

	// --- Control (unlimited) system ---
	controlPort := reservedPort(t)
	controlCS := ocpp16.NewDefaultCentralSystem()

	// We just need to prove the control does NOT drop the connection — the
	// disconnect handler firing (or not) is the signal.
	controlDisconnectedC := make(chan struct{}, 1)
	controlCS.SetChargePointDisconnectedHandler(func(_ ocpp16.ChargePointConnection) {
		controlDisconnectedC <- struct{}{}
	})

	cpStopOnCleanup(t, "control central system", controlCS.Stop)
	go controlCS.Start(controlPort, "/ws")

	cpAwaitListening(t, limitedPort)
	cpAwaitListening(t, controlPort)

	// --- Dial limited system, write oversized → should disconnect ---
	limitedClient := ws.NewClient()
	limitedClient.SetAutoReconnect(false)
	limitedClient.SetRequestedSubProtocol(types.V16Subprotocol)

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
	controlClient.SetRequestedSubProtocol(types.V16Subprotocol)

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
}

// TestNewDefaultCentralSystemNoOptsMatchesLegacyDefault is a cheap smoke test:
// NewDefaultCentralSystem() returns a non-nil CentralSystem that starts and
// stops cleanly — the delegation to NewCentralSystem(nil, ws.NewServer())
// did not change the default path.
func TestNewDefaultCentralSystemNoOptsMatchesLegacyDefault(t *testing.T) {
	cs := ocpp16.NewDefaultCentralSystem()
	if cs == nil {
		t.Fatal("NewDefaultCentralSystem returned nil")
	}

	port := reservedPort(t)
	cpStopOnCleanup(t, "central system", cs.Stop)
	go cs.Start(port, "/ws")

	cpAwaitListening(t, port)
}
