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
	return channel, writes
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
	require.NoError(suite.T(), suite.chargingStation.Start("someUrl"))
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
	require.NoError(suite.T(), suite.chargingStation.Start("someUrl"))
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
	require.NoError(t, suite.chargingStation.Start("someUrl"))
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

	count := 0
	panicCount := 0
	quiet := time.NewTimer(300 * time.Millisecond)
	defer quiet.Stop()
	for {
		select {
		case reported := <-errorsC:
			var panicErr *ocppj.HandlerPanicError
			if errors.As(reported, &panicErr) {
				kindOK := panicErr.Panic.Kind == ocppj.ErrorHandlerKind || panicErr.Panic.Kind == ocppj.DisconnectHandlerKind
				idOK := (panicErr.Panic.Kind == ocppj.ErrorHandlerKind && panicErr.Panic.RequestID == id) || (panicErr.Panic.Kind == ocppj.DisconnectHandlerKind && panicErr.Panic.RequestID == "")
				if kindOK && idOK {
					count++
					panicCount++
				}
			} else if strings.Contains(reported.Error(), "no handler available for canceled request") {
				count++
			}
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
		case <-time.After(ec3Wait):
			t.Fatal("stop-drain Errors() did not reach a quiet period")
		}
	}
}

// setupEC3V2Client starts the charging station and installs, BEFORE Start, a
// single availability-handler mock whose OnChangeAvailability panics with
// whatever value it receives from the returned panicValues channel. Handler
// setters have a documented before-Start contract (no synchronization once
// the asyncCallbackHandler goroutine is running), so the handler must be
// installed exactly once, before Start, rather than being swapped in later by
// each driven panic.
func (suite *OcppV2TestSuite) setupEC3V2Client() (chan []byte, chan string) {
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
	suite.chargingStation.SetAvailabilityHandler(listener)
	require.NoError(suite.T(), suite.chargingStation.Start("someUrl"))
	return writeC, panicValues
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
	_, err := suite.ocppjClient.SendRequest(availability.NewHeartbeatRequest())
	require.NoError(t, err)
	select {
	case <-writeC:
	case <-time.After(ec3Wait):
		t.Fatal("first existing error probe was not written")
	}
	require.NoError(t, suite.mockWsClient.MessageHandler([]byte(`[3,"1234",{"currentTime":"2026-01-01T00:00:00Z"}]`)))
	select {
	case <-errorsC:
	case <-time.After(ec3Wait):
		t.Fatal("first existing error did not fill Errors()")
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
	_, err = suite.ocppjClient.SendRequest(availability.NewHeartbeatRequest())
	require.NoError(t, err)
	select {
	case <-writeC:
	case <-time.After(ec3Wait):
		t.Fatal("second existing error probe was not written")
	}
	require.NoError(t, suite.mockWsClient.MessageHandler([]byte(`[3,"1234",{"currentTime":"2026-01-01T00:00:00Z"}]`)))
	time.Sleep(100 * time.Millisecond)
	suite.chargingStation.Stop()
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
	suite.chargingStation.Stop()
}
