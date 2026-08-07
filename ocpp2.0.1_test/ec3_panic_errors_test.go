package ocpp2_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/enesismail/ocpp-go/ocpp"
	ocpp2 "github.com/enesismail/ocpp-go/ocpp2.0.1"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/availability"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/provisioning"
	"github.com/enesismail/ocpp-go/ocppj"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const ec3Wait = 2 * time.Second

// ec3PrepareV2Server wires the server/client websocket mocks for an EC3 test.
// The mock closures capture LOCAL snapshots of suite.mockWsServer/mockWsClient
// (server/client below) rather than re-reading the suite fields at call time:
// a panic-recovery goroutine (RecoverPanicGoroutine) can outlive the test that
// spawned it, and by the time it runs, SetupTest may already have reassigned
// suite.mockWsServer/suite.mockWsClient to brand-new mocks for the NEXT test.
// Closing over the suite pointer's fields would race on that reassignment;
// closing over locals captured now does not.
func ec3PrepareV2Server(suite *OcppV2TestSuite, clientID string, forward bool) (MockWebSocket, chan string) {
	channel := NewMockWebSocket(clientID)
	writes := make(chan string, 32)
	server := suite.mockWsServer
	client := suite.mockWsClient
	server.On("Start", mock.AnythingOfType("int"), mock.AnythingOfType("string")).Return(nil)
	server.On("Stop").Return().Maybe()
	server.On("Write", mock.AnythingOfType("string"), mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		select {
		case writes <- args.String(0):
		default:
		}
		if forward {
			_ = client.MessageHandler(args.Get(1).([]byte))
		}
	})
	client.On("Start", mock.AnythingOfType("string")).Return(nil).Run(func(mock.Arguments) {
		server.NewClientHandler(channel)
	})
	client.On("Write", mock.Anything).Return(nil)
	// Stop/IsConnected are needed only by the charging station's teardown (see
	// ec3StopOnCleanup): ocppj.Client.Stop probes IsConnected and, when it
	// reports true, waits for a disconnect callback the mock never delivers.
	client.On("Stop").Return().Maybe()
	client.On("IsConnected").Return(false).Maybe()
	return channel, writes
}

// ec3StopOnCleanup registers a bounded teardown for a facade this test started.
// The Stop runs on its own goroutine behind an ec3Wait watchdog: Stop joins the
// in-flight handler generation, so a regressed build can wedge inside it, and a
// bare call would hang the whole suite instead of failing this test. Running it
// from t.Cleanup covers the t.Fatal paths too, so the facade's dispatcher and
// handler goroutines cannot outlive the test into the next SetupTest.
func ec3StopOnCleanup(t *testing.T, name string, stop func()) {
	t.Helper()
	t.Cleanup(func() {
		doneC := make(chan struct{})
		go func() {
			stop()
			close(doneC)
		}()
		select {
		case <-doneC:
		case <-time.After(ec3Wait):
			// Errorf rather than Fatal: a Fatal here would skip the remaining
			// cleanups and leave the other facade of the pair running.
			t.Errorf("%s Stop did not return", name)
		}
	})
}

func ec3FreshV2CSMS(server *MockWebsocketServer) (ocpp2.CSMS, *ocppj.Server) {
	endpoint := ocppj.NewServer(server, ocppj.NewDefaultServerDispatcher(ocppj.NewFIFOQueueMap(queueCapacity)), nil, availability.Profile, provisioning.Profile)
	return ocpp2.NewCSMS(endpoint, server), endpoint
}

func ec3ReadPanicError(t *testing.T, errorsC <-chan error) *ocppj.HandlerPanicError {
	t.Helper()
	select {
	case reported := <-errorsC:
		var panicErr *ocppj.HandlerPanicError
		require.ErrorAs(t, reported, &panicErr)
		return panicErr
	case <-time.After(ec3Wait):
		t.Fatalf("timed out waiting for recovered panic on Errors()")
		return nil
	}
}

// ec3AwaitWrite waits for a single write on writes (a server- or client-side
// CALL_ERROR produced by RecoverPanicGoroutine's sendCallError path) before
// the calling test is allowed to return. Without this, the recovery goroutine
// can still be mid-flight (about to call the mock's Write) when the test
// function returns and the suite's testify mocks get torn down/reassigned by
// the next test's SetupTest, or when that goroutine touches the current
// test's *testing.T after the test has already completed.
func ec3AwaitWrite(t *testing.T, writes <-chan string) {
	t.Helper()
	select {
	case <-writes:
	case <-time.After(ec3Wait):
		t.Fatal("timed out waiting for the panic-triggered CALL_ERROR write")
	}
}

// driveV2InboundPanic drives an inbound BootNotification CALL into the CSMS
// whose handler panics with value, and returns the writes channel fed by the
// server's mock Write (see ec3PrepareV2Server) so callers can await the
// resulting CALL_ERROR write before returning.
func (suite *OcppV2TestSuite) driveV2InboundPanic(clientID, messageID, value string) chan string {
	listener := &MockCSMSProvisioningHandler{}
	listener.On("OnBootNotification", mock.AnythingOfType("string"), mock.Anything).Run(func(mock.Arguments) { panic(value) })
	suite.csms.SetProvisioningHandler(listener)
	channel, writes := ec3PrepareV2Server(suite, clientID, true)
	suite.csms.Start(8887, "somePath")
	ec3StopOnCleanup(suite.T(), "CSMS", suite.csms.Stop)
	require.NoError(suite.T(), suite.chargingStation.Start("someUrl"))
	ec3StopOnCleanup(suite.T(), "charging station", suite.chargingStation.Stop)
	require.NoError(suite.T(), suite.mockWsServer.MessageHandler(channel, []byte(fmt.Sprintf(`[2,"%s","BootNotification",{"reason":"PowerUp","chargingStation":{"model":"model1","vendorName":"ABL"}}]`, messageID))))
	return writes
}

func (suite *OcppV2TestSuite) TestEC3PanicReportedOnErrorsByDefault() {
	errorsC := suite.csms.Errors()
	writes := suite.driveV2InboundPanic("ec3-default", "ec3-request", "ec3-default-panic")
	panicErr := ec3ReadPanicError(suite.T(), errorsC)
	require.Equal(suite.T(), ocppj.RequestHandlerKind, panicErr.Panic.Kind)
	require.Equal(suite.T(), "ec3-default", panicErr.Panic.ClientID)
	require.Equal(suite.T(), provisioning.BootNotificationFeatureName, panicErr.Panic.Action)
	require.Equal(suite.T(), "ec3-request", panicErr.Panic.RequestID)
	require.Equal(suite.T(), "ec3-default-panic", panicErr.Panic.Value)
	require.NotEmpty(suite.T(), panicErr.Panic.Stack)
	ec3AwaitWrite(suite.T(), writes)
}

func (suite *OcppV2TestSuite) TestEC3PanicReachesBothHookAndErrors() {
	hookC := make(chan ocppj.HandlerPanic, 1)
	suite.csms.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) { hookC <- hp })
	errorsC := suite.csms.Errors()
	writes := suite.driveV2InboundPanic("ec3-both", "ec3-both-request", "ec3-both-panic")
	select {
	case hp := <-hookC:
		panicErr := ec3ReadPanicError(suite.T(), errorsC)
		require.Equal(suite.T(), panicErr.Panic, hp)
	case <-time.After(ec3Wait):
		suite.T().Fatal("timed out waiting for facade panic hook")
	}
	ec3AwaitWrite(suite.T(), writes)
}

func (suite *OcppV2TestSuite) TestEC3PanickingHookDoesNotSuppressErrorsReport() {
	suite.csms.SetOnHandlerPanic(func(ocppj.HandlerPanic) { panic("ec3-hook-panic") })
	errorsC := suite.csms.Errors()
	writes := suite.driveV2InboundPanic("ec3-hook", "ec3-hook-request", "ec3-handler-panic")
	panicErr := ec3ReadPanicError(suite.T(), errorsC)
	require.Equal(suite.T(), "ec3-handler-panic", panicErr.Panic.Value)
	ec3AwaitWrite(suite.T(), writes)
}

func (suite *OcppV2TestSuite) TestEC3SetOnHandlerPanicAfterConstructionKeepsRouting() {
	hookC := make(chan ocppj.HandlerPanic, 1)
	suite.csms.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) { hookC <- hp })
	errorsC := suite.csms.Errors()
	writes := suite.driveV2InboundPanic("ec3-after", "ec3-after-request", "ec3-after-panic")
	select {
	case <-hookC:
	case <-time.After(ec3Wait):
		suite.T().Fatal("timed out waiting for facade panic hook")
	}
	_ = ec3ReadPanicError(suite.T(), errorsC)
	ec3AwaitWrite(suite.T(), writes)
}

func (suite *OcppV2TestSuite) TestEC3EndpointHookRegisteredBeforeConstructionSurvives() {
	clientID := "ec3-before"
	endpoint := ocppj.NewServer(suite.mockWsServer, ocppj.NewDefaultServerDispatcher(ocppj.NewFIFOQueueMap(queueCapacity)), nil, provisioning.Profile)
	prevC := make(chan ocppj.HandlerPanic, 1)
	endpoint.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) { prevC <- hp })
	csms := ocpp2.NewCSMS(endpoint, suite.mockWsServer)
	listener := &MockCSMSProvisioningHandler{}
	listener.On("OnBootNotification", mock.AnythingOfType("string"), mock.Anything).Run(func(mock.Arguments) { panic("ec3-before-panic") })
	csms.SetProvisioningHandler(listener)
	errorsC := csms.Errors()
	channel, writes := ec3PrepareV2Server(suite, clientID, true)
	csms.Start(8887, "somePath")
	ec3StopOnCleanup(suite.T(), "CSMS", csms.Stop)
	require.NoError(suite.T(), suite.chargingStation.Start("someUrl"))
	ec3StopOnCleanup(suite.T(), "charging station", suite.chargingStation.Stop)
	require.NoError(suite.T(), suite.mockWsServer.MessageHandler(channel, []byte(`[2,"ec3-before-request","BootNotification",{"reason":"PowerUp","chargingStation":{"model":"model1","vendorName":"ABL"}}]`)))
	select {
	case <-prevC:
	case <-time.After(ec3Wait):
		suite.T().Fatal("endpoint hook registered before construction was not called")
	}
	_ = ec3ReadPanicError(suite.T(), errorsC)
	ec3AwaitWrite(suite.T(), writes)
}

func (suite *OcppV2TestSuite) TestEC3PanicStormWithUndrainedErrorsDoesNotWedge() {
	t := suite.T()
	// hookC carries one signal per recovered panic; sized to the exact expected
	// count (18) since the hook callback sends unconditionally (see below).
	hookC := make(chan struct{}, 18)
	suite.csms.SetOnHandlerPanic(func(ocppj.HandlerPanic) { hookC <- struct{}{} })
	_ = suite.csms.Errors() // Intentionally never drained.
	listener := &MockCSMSProvisioningHandler{}
	listener.On("OnBootNotification", mock.AnythingOfType("string"), mock.Anything).Run(func(mock.Arguments) { panic("ec3-storm-panic") })
	suite.csms.SetProvisioningHandler(listener)
	channel, writes := ec3PrepareV2Server(suite, "ec3-storm", false)
	suite.csms.Start(8887, "somePath")
	ec3StopOnCleanup(t, "CSMS", suite.csms.Stop)
	require.NoError(t, suite.chargingStation.Start("someUrl"))
	ec3StopOnCleanup(t, "charging station", suite.chargingStation.Stop)
	for i := 0; i < 18; i++ {
		id := fmt.Sprintf("ec3-storm-%d", i)
		require.NoError(t, suite.mockWsServer.MessageHandler(channel, []byte(fmt.Sprintf(`[2,"%s","BootNotification",{"reason":"PowerUp","chargingStation":{"model":"model1","vendorName":"ABL"}}]`, id))))
	}
	for i := 0; i < 18; i++ {
		select {
		case <-hookC:
		case <-time.After(ec3Wait):
			t.Fatalf("recovered panic hook count = %d, want 18", i)
		}
	}
	// This is a one-shot length assertion, not a spin-poll: it asserts the
	// buffered-but-undrained Errors() channel is full at its cap (16) and the
	// excess (18 panics - 16 cap = 2) was silently dropped, per the
	// non-blocking send contract documented on csms.error/Errors.
	require.Equal(t, 16, len(suite.csms.Errors()))
	// Drain every storm-triggered CALL_ERROR write before moving on, so none
	// of the 18 recovery goroutines can still be mid-flight (about to call the
	// mock's Write) once this test function returns.
	stormWrites := 0
	stormDeadline := time.After(ec3Wait)
	for stormWrites < 18 {
		select {
		case got := <-writes:
			if got == "ec3-storm" {
				stormWrites++
			}
		case <-stormDeadline:
			t.Fatalf("only observed %d/18 storm CALL_ERROR writes", stormWrites)
		}
	}
	suite.mockWsServer.NewClientHandler(NewMockWebSocket("ec3-fresh"))
	_, err := suite.ocppjServer.SendRequest("ec3-fresh", availability.NewHeartbeatRequest())
	require.NoError(t, err)
	select {
	case got := <-writes:
		require.Equal(t, "ec3-fresh", got)
	case <-time.After(ec3Wait):
		t.Fatal("following request did not dispatch after the panic storm")
	}
}

func (suite *OcppV2TestSuite) TestEC3StopDrainPanicReportedOnErrors() {
	t := suite.T()
	clientID := "ec3-stop-drain"
	csms, _ := ec3FreshV2CSMS(suite.mockWsServer)
	errorsC := csms.Errors()
	suite.mockWsServer.On("Start", mock.AnythingOfType("int"), mock.AnythingOfType("string")).Return(nil)
	suite.mockWsServer.On("Stop").Return().Maybe()
	suite.mockWsServer.On("Write", clientID, mock.Anything).Return(nil)
	csms.Start(8887, "somePath")
	ec3StopOnCleanup(t, "CSMS", csms.Stop)
	suite.mockWsServer.NewClientHandler(NewMockWebSocket(clientID))
	id := defaultMessageId
	err := csms.SendRequestAsync(clientID, availability.NewChangeAvailabilityRequest(availability.OperationalStatusOperative), func(ocpp.Response, error) {
		panic("ec3-stop-drain-panic")
	})
	require.NoError(t, err)
	stopDoneC := make(chan struct{})
	go func() {
		csms.Stop()
		close(stopDoneC)
	}()
	select {
	case <-stopDoneC:
	case <-time.After(ec3Wait):
		t.Fatal("CSMS Stop did not return")
	}

	// Two entries are possible here and neither their arrival times nor their
	// order are fixed: the MANDATORY recovered panic from the stop-drain cancel
	// callback, and an OPTIONAL "no handler available for canceled request"
	// report when the drain races the disconnect path. classify counts whichever
	// arrives.
	count := 0
	panicCount := 0
	classify := func(reported error) {
		var panicErr *ocppj.HandlerPanicError
		if errors.As(reported, &panicErr) {
			kindOK := panicErr.Panic.Kind == ocppj.ErrorHandlerKind || panicErr.Panic.Kind == ocppj.DisconnectHandlerKind
			idOK := (panicErr.Panic.Kind == ocppj.ErrorHandlerKind && panicErr.Panic.RequestID == id) || (panicErr.Panic.Kind == ocppj.DisconnectHandlerKind && panicErr.Panic.RequestID == "")
			if kindOK && idOK {
				count++
				panicCount++
			}
			return
		}
		if strings.Contains(reported.Error(), "no handler available for canceled request") {
			count++
		}
	}

	// Phase 1 - await the mandatory panic entry under the FULL ec3Wait
	// watchdog. The cancel callback is dispatched off the dispatcher pump, so it
	// can legitimately be scheduled long after Stop() has returned: entering the
	// short quiet window below first would let that window close with nothing
	// observed at all and fail a healthy build.
	panicDeadline := time.After(ec3Wait)
	for panicCount == 0 {
		select {
		case reported := <-errorsC:
			classify(reported)
		case <-panicDeadline:
			t.Fatal("stop-drain Errors() never reported the recovered panic")
		}
	}

	// Phase 2 - the optional second entry. Errors() never closes, so a
	// restarting quiet window is the terminator: once nothing more arrives for
	// 300ms, the final count has settled.
	quiet := time.NewTimer(300 * time.Millisecond)
	defer quiet.Stop()
	settleDeadline := time.After(ec3Wait)
	for {
		select {
		case reported := <-errorsC:
			classify(reported)
			if !quiet.Stop() {
				select {
				case <-quiet.C:
				default:
				}
			}
			quiet.Reset(300 * time.Millisecond)
		case <-quiet.C:
			require.Contains(t, []int{1, 2}, count)
			require.GreaterOrEqual(t, panicCount, 1)
			return
		case <-settleDeadline:
			t.Fatal("stop-drain Errors() did not reach a quiet period")
		}
	}
}

// setupEC3V2ClientFor starts cs and installs, BEFORE Start, a single
// availability-handler mock whose OnChangeAvailability panics with whatever
// value it receives from the returned panicValues channel. Handler setters
// have a documented before-Start contract (no synchronization once the
// asyncCallbackHandler goroutine is running), so the handler must be installed
// exactly once, before Start, rather than being swapped in later by each
// driven panic.
func (suite *OcppV2TestSuite) setupEC3V2ClientFor(cs ocpp2.ChargingStation) (chan []byte, chan string) {
	suite.mockWsClient.On("Start", mock.AnythingOfType("string")).Return(nil)
	suite.mockWsClient.On("Stop").Return().Maybe()
	suite.mockWsClient.On("IsConnected").Return(false).Maybe()
	writeC := make(chan []byte, 16)
	suite.mockWsClient.On("Write", mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		writeC <- append([]byte(nil), args.Get(0).([]byte)...)
	})
	panicValues := make(chan string, 4)
	listener := &MockChargingStationAvailabilityHandler{}
	listener.On("OnChangeAvailability", mock.Anything).Run(func(mock.Arguments) { panic(<-panicValues) })
	cs.SetAvailabilityHandler(listener)
	require.NoError(suite.T(), cs.Start("someUrl"))
	// Stop also releases any producer left parked in the blocking error() (its
	// stopC arm), so no goroutine outlives the test on the t.Fatal paths either.
	ec3StopOnCleanup(suite.T(), "charging station", cs.Stop)
	return writeC, panicValues
}

func (suite *OcppV2TestSuite) setupEC3V2Client() (chan []byte, chan string) {
	return suite.setupEC3V2ClientFor(suite.chargingStation)
}

// driveV2OrphanResponse drives one ORDINARY (non-panic) error() producer: a
// heartbeat sent through the raw ocppj client has no facade callback
// registered, so the facade's asyncCallbackHandler goroutine reports "no
// handler available for incoming response" through the BLOCKING cs.error().
func (suite *OcppV2TestSuite) driveV2OrphanResponse(writeC chan []byte, label string) {
	t := suite.T()
	t.Helper()
	_, err := suite.ocppjClient.SendRequest(availability.NewHeartbeatRequest())
	require.NoError(t, err)
	select {
	case <-writeC:
	case <-time.After(ec3Wait):
		t.Fatalf("%s existing error probe was not written", label)
	}
	require.NoError(t, suite.mockWsClient.MessageHandler([]byte(`[3,"1234",{"currentTime":"2026-01-01T00:00:00Z"}]`)))
}

// driveV2ClientPanic pushes value for the pre-installed availability handler
// (see setupEC3V2Client) to panic with, injects the inbound ChangeAvailability
// CALL, and awaits the resulting CALL_ERROR write before returning, so the
// asyncCallbackHandler's RecoverPanicGoroutine cannot still be mid-flight once
// the calling test function returns.
func (suite *OcppV2TestSuite) driveV2ClientPanic(writeC chan []byte, panicValues chan string, id string, value string) {
	panicValues <- value
	require.NoError(suite.T(), suite.mockWsClient.MessageHandler([]byte(fmt.Sprintf(`[2,"%s","ChangeAvailability",{"operationalStatus":"Operative"}]`, id))))
	select {
	case <-writeC:
	case <-time.After(ec3Wait):
		suite.T().Fatal("timed out waiting for the CALL_ERROR write after client panic")
	}
}

func (suite *OcppV2TestSuite) TestEC3ClientPanicRouteIsNonBlocking() {
	t := suite.T()
	errorsC := suite.chargingStation.Errors()
	hookC := make(chan struct{}, 4)
	suite.chargingStation.SetOnHandlerPanic(func(ocppj.HandlerPanic) { hookC <- struct{}{} })
	writeC, panicValues := suite.setupEC3V2Client()

	// Establish FIRST that the ordinary error() path really is blocking on a
	// full cap-1 errC. Without this, every assertion below would also hold for a
	// (wrong) globally non-blocking error(), leaving the guard vacuous. Two
	// ordinary producers are driven back to back and are processed in FIFO order
	// by the single asyncCallbackHandler goroutine: the first fills errC, the
	// second must then PARK inside error().
	suite.driveV2OrphanResponse(writeC, "first")
	suite.driveV2OrphanResponse(writeC, "second")

	// The park is observable: that same goroutine is the sole consumer of
	// cs.incoming, so while it is parked an inbound CALL whose handler panics
	// produces no CALL_ERROR write at all. A non-blocking error() would instead
	// drop the second report and let this write through immediately.
	panicValues <- "ec3-client-parked-panic"
	require.NoError(t, suite.mockWsClient.MessageHandler([]byte(`[2,"ec3-client-parked","ChangeAvailability",{"operationalStatus":"Operative"}]`)))
	select {
	case <-writeC:
		t.Fatal("ordinary error() did not block while the Errors() buffer was full")
	case <-time.After(150 * time.Millisecond):
	}

	// Release the parked producer with a single drain; its error takes the freed
	// slot and the queued CALL is finally handled. The panic route therefore runs
	// with errC full again and must NOT park - both the CALL_ERROR write and the
	// hook have to arrive.
	select {
	case <-errorsC:
	case <-time.After(ec3Wait):
		t.Fatal("first existing error did not fill Errors()")
	}
	select {
	case <-writeC:
	case <-time.After(ec3Wait):
		t.Fatal("the parked producer was never released")
	}
	select {
	case <-hookC:
	case <-time.After(ec3Wait):
		t.Fatal("panic hook did not fire for the CALL queued behind the parked producer")
	}

	suite.driveV2ClientPanic(writeC, panicValues, "ec3-client-one", "ec3-client-one-panic")
	suite.driveV2ClientPanic(writeC, panicValues, "ec3-client-two", "ec3-client-two-panic")
	for i := 0; i < 2; i++ {
		select {
		case <-hookC:
		case <-time.After(ec3Wait):
			t.Fatalf("client panic hook count = %d, want 2", i)
		}
	}
	// A last ordinary producer, deliberately left parked on the full buffer: the
	// teardown registered by setupEC3V2Client releases it through Stop's stopC
	// arm, so it does not outlive the test.
	suite.driveV2OrphanResponse(writeC, "third")
	time.Sleep(100 * time.Millisecond)
}

func (suite *OcppV2TestSuite) TestEC3ClientPanicBeforeErrorsArmedIsSilent() {
	hookC := make(chan struct{}, 1)
	suite.chargingStation.SetOnHandlerPanic(func(ocppj.HandlerPanic) { hookC <- struct{}{} })
	writeC, panicValues := suite.setupEC3V2Client()
	suite.driveV2ClientPanic(writeC, panicValues, "ec3-client-silent", "ec3-client-silent-panic")
	select {
	case <-hookC:
	case <-time.After(ec3Wait):
		suite.T().Fatal("client panic hook did not fire")
	}
	errorsC := suite.chargingStation.Errors()
	select {
	case reported := <-errorsC:
		suite.T().Fatalf("unexpected retroactive client panic report: %v", reported)
	default:
	}
}

func (suite *OcppV2TestSuite) TestEC3ClientPanicReportedOnErrorsByDefault() {
	t := suite.T()
	// Errors() is armed before the panic, with its cap-1 buffer empty, so the
	// non-blocking report must land: this is the positive delivery guard for the
	// client route (the non-blocking test above always runs it against a FULL
	// buffer, where drops are by design).
	errorsC := suite.chargingStation.Errors()
	writeC, panicValues := suite.setupEC3V2Client()
	suite.driveV2ClientPanic(writeC, panicValues, "ec3-client-report", "ec3-client-report-panic")
	panicErr := ec3ReadPanicError(t, errorsC)
	require.Equal(t, ocppj.RequestHandlerKind, panicErr.Panic.Kind)
	require.Equal(t, "", panicErr.Panic.ClientID)
	require.Equal(t, availability.ChangeAvailabilityFeatureName, panicErr.Panic.Action)
	require.Equal(t, "ec3-client-report", panicErr.Panic.RequestID)
	require.Equal(t, "ec3-client-report-panic", panicErr.Panic.Value)
	require.NotEmpty(t, panicErr.Panic.Stack)
}

func (suite *OcppV2TestSuite) TestEC3ClientSetOnHandlerPanicAfterConstructionKeepsRouting() {
	t := suite.T()
	hookC := make(chan ocppj.HandlerPanic, 1)
	suite.chargingStation.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) { hookC <- hp })
	errorsC := suite.chargingStation.Errors()
	writeC, panicValues := suite.setupEC3V2Client()
	suite.driveV2ClientPanic(writeC, panicValues, "ec3-client-after", "ec3-client-after-panic")
	panicErr := ec3ReadPanicError(t, errorsC)
	select {
	case hp := <-hookC:
		require.Equal(t, panicErr.Panic, hp)
	case <-time.After(ec3Wait):
		t.Fatal("client facade panic hook did not fire")
	}
}

func (suite *OcppV2TestSuite) TestEC3ClientEndpointHookRegisteredBeforeConstructionSurvives() {
	t := suite.T()
	endpoint := ocppj.NewClient("test_id", suite.mockWsClient, ocppj.NewDefaultClientDispatcher(ocppj.NewFIFOClientQueue(queueCapacity)), nil, availability.Profile)
	prevC := make(chan ocppj.HandlerPanic, 1)
	endpoint.SetOnHandlerPanic(func(hp ocppj.HandlerPanic) { prevC <- hp })
	chargingStation := ocpp2.NewChargingStation("test_id", endpoint, suite.mockWsClient)
	errorsC := chargingStation.Errors()
	writeC, panicValues := suite.setupEC3V2ClientFor(chargingStation)
	suite.driveV2ClientPanic(writeC, panicValues, "ec3-client-before", "ec3-client-before-panic")
	select {
	case <-prevC:
	case <-time.After(ec3Wait):
		t.Fatal("endpoint hook registered before construction was not called")
	}
	_ = ec3ReadPanicError(t, errorsC)
}
