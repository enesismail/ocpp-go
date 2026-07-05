package ocppj_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/enesismail/ocpp-go/ocpp"
	"github.com/enesismail/ocpp-go/ocppj"
)

// Test 1 (property/guard, mirrors request_timeout_test.go): the ErrLocalTransport
// marker must classify a NewLocalTransportError regardless of code, must NOT
// collide with the other sentinels, and must NOT accidentally match a plain
// (untagged) *ocpp.Error - whether a real server CALLERROR or an empty Error.
func TestLocalTransportSentinel_DiscriminatesFromCALLERROR(t *testing.T) {
	// The marker classifies independently of the error code: both the
	// write-failure's InternalError and the disconnect-drain's GenericError match.
	internalErr := ocppj.NewLocalTransportError(ocppj.InternalError, "", "m")
	if !errors.Is(internalErr, ocppj.ErrLocalTransport) {
		t.Fatal("a local-transport error with InternalError code must match ErrLocalTransport")
	}
	genericErr := ocppj.NewLocalTransportError(ocppj.GenericError, "", "m")
	if !errors.Is(genericErr, ocppj.ErrLocalTransport) {
		t.Fatal("a local-transport error with GenericError code must match ErrLocalTransport")
	}

	// The constructor must PRESERVE Code/Description/MessageId exactly - not
	// drop, overwrite, or transpose them - for both codes it is called with at
	// the tagged sites. Uses distinct, non-empty description/messageID values
	// (unlike internalErr/genericErr above, which both pass "") so a broken
	// constructor that drops or swaps a field is actually caught.
	internalConstructed := ocppj.NewLocalTransportError(ocppj.InternalError, "write failed: connection reset", "msg-internal")
	assert.Equal(t, ocppj.InternalError, internalConstructed.Code, "NewLocalTransportError must preserve the passed code")
	assert.Equal(t, "write failed: connection reset", internalConstructed.Description, "NewLocalTransportError must preserve the passed description")
	assert.Equal(t, "msg-internal", internalConstructed.MessageId, "NewLocalTransportError must preserve the passed messageID")

	genericConstructed := ocppj.NewLocalTransportError(ocppj.GenericError, "client disconnected, no response received from client", "msg-generic")
	assert.Equal(t, ocppj.GenericError, genericConstructed.Code, "NewLocalTransportError must preserve the passed code")
	assert.Equal(t, "client disconnected, no response received from client", genericConstructed.Description, "NewLocalTransportError must preserve the passed description")
	assert.Equal(t, "msg-generic", genericConstructed.MessageId, "NewLocalTransportError must preserve the passed messageID")

	// Must not collide with the other sentinels.
	if errors.Is(internalErr, ocppj.ErrRequestTimeout) {
		t.Fatal("a local-transport error must NOT match ErrRequestTimeout")
	}
	if errors.Is(internalErr, ocppj.ErrDispatcherStopped) {
		t.Fatal("a local-transport error must NOT match ErrDispatcherStopped")
	}

	// A real server CALLERROR carries a code but no marker - must not match.
	callErr := ocpp.NewError(ocppj.GenericError, "boom", "m")
	if errors.Is(callErr, ocppj.ErrLocalTransport) {
		t.Fatal("a plain server GenericError CALLERROR must NOT match the local-transport sentinel")
	}

	// An untagged Error must never accidentally match any marked target.
	empty := &ocpp.Error{}
	if errors.Is(empty, ocppj.ErrLocalTransport) {
		t.Fatal("an Error with no Marker must not match the local-transport sentinel")
	}
	if errors.Is(empty, ocppj.ErrRequestTimeout) {
		t.Fatal("an Error with no Marker must not match the request-timeout sentinel")
	}
	if errors.Is(empty, ocppj.ErrDispatcherStopped) {
		t.Fatal("an Error with no Marker must not match the dispatcher-stopped sentinel")
	}
}

// localTransportClientSuite mirrors ClientDispatcherTestSuite in dispatcher_test.go
// (mock websocketClient, SetTimeout/SetOnRequestCanceled harness) to drive B1: a
// failed network write on the client dispatcher.
type localTransportClientSuite struct {
	suite.Suite
	state           ocppj.ClientState
	queue           ocppj.RequestQueue
	dispatcher      ocppj.ClientDispatcher
	endpoint        ocppj.Client
	websocketClient MockWebsocketClient
}

func (c *localTransportClientSuite) SetupTest() {
	c.endpoint = ocppj.Client{Id: "client1"}
	mockProfile := ocpp.NewProfile("mock", &MockFeature{})
	c.endpoint.AddProfile(mockProfile)
	c.queue = ocppj.NewFIFOClientQueue(10)
	c.dispatcher = ocppj.NewDefaultClientDispatcher(c.queue)
	c.state = ocppj.NewClientState()
	c.dispatcher.SetPendingRequestState(c.state)
	c.websocketClient = MockWebsocketClient{}
	c.dispatcher.SetNetworkClient(&c.websocketClient)
}

func (c *localTransportClientSuite) TearDownTest() {
	if c.dispatcher.IsRunning() {
		c.dispatcher.Stop()
	}
}

// Test 2 - B1 client write-failure e2e: a mock ws whose Write errors must
// surface at SetOnRequestCanceled as an ErrLocalTransport, not ErrRequestTimeout,
// and distinguishable from an empty-marker "genuine server" error.
func (c *localTransportClientSuite) TestClientWriteFailureClassifiedAsLocalTransport() {
	t := c.T()
	errMsg := "mockWriteError"
	c.websocketClient.On("Write", mock.Anything).Return(fmt.Errorf(errMsg))

	// The callback fires on the dispatcher's pump goroutine - never call
	// require/assert or t.Fatal from inside it. Capture into a buffered
	// channel and assert on the test goroutine instead.
	canceled := make(chan *ocpp.Error, 1)
	c.dispatcher.SetOnRequestCanceled(func(rID string, request ocpp.Request, err *ocpp.Error) {
		canceled <- err
	})

	c.dispatcher.Start()
	require.True(t, c.dispatcher.IsRunning())

	req := newMockRequest("somevalue")
	call, err := c.endpoint.CreateCall(req)
	require.NoError(t, err)
	data, err := call.MarshalJSON()
	require.NoError(t, err)
	bundle := ocppj.RequestBundle{Call: call, Data: data}
	err = c.dispatcher.SendRequest(bundle)
	require.NoError(t, err)

	var cancelErr *ocpp.Error
	select {
	case cancelErr = <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for OnRequestCanceled")
	}

	require.NotNil(t, cancelErr)
	assert.True(t, errors.Is(cancelErr, ocppj.ErrLocalTransport),
		"write-failure error must classify as ErrLocalTransport")
	assert.False(t, errors.Is(cancelErr, ocppj.ErrRequestTimeout),
		"write-failure error must NOT classify as ErrRequestTimeout")
	// Distinguish from a genuine (empty-marker) server CALLERROR carrying the
	// same fields: constructing one from the captured code/description/id must
	// NOT match ErrLocalTransport, proving the marker (not code/description) is
	// what discriminates.
	genuineServerErr := ocpp.NewError(cancelErr.Code, cancelErr.Description, cancelErr.MessageId)
	assert.False(t, errors.Is(genuineServerErr, ocppj.ErrLocalTransport),
		"an equivalent empty-marker error must not match ErrLocalTransport")
}

func TestLocalTransportClientSuite(t *testing.T) {
	suite.Run(t, new(localTransportClientSuite))
}

// localTransportServerSuite mirrors ServerDispatcherTestSuite in dispatcher_test.go
// (mock websocketServer, SetTimeout/SetOnRequestCanceled harness) to drive B2: a
// failed network write and a server-side request timeout.
type localTransportServerSuite struct {
	suite.Suite
	mutex           sync.RWMutex
	state           ocppj.ServerState
	websocketServer MockWebsocketServer
	endpoint        ocppj.Server
	dispatcher      ocppj.ServerDispatcher
	queueMap        ocppj.ServerQueueMap
}

func (s *localTransportServerSuite) SetupTest() {
	s.endpoint = ocppj.Server{}
	mockProfile := ocpp.NewProfile("mock", &MockFeature{})
	s.endpoint.AddProfile(mockProfile)
	s.queueMap = ocppj.NewFIFOQueueMap(10)
	s.dispatcher = ocppj.NewDefaultServerDispatcher(s.queueMap)
	s.state = ocppj.NewServerState(&s.mutex)
	s.dispatcher.SetPendingRequestState(s.state)
	s.websocketServer = MockWebsocketServer{}
	s.dispatcher.SetNetworkServer(&s.websocketServer)
}

func (s *localTransportServerSuite) TearDownTest() {
	if s.dispatcher.IsRunning() {
		s.dispatcher.Stop()
	}
}

// Test 3 - B2 server write-failure e2e: a mock server ws whose Write(clientID, ...)
// errors must surface at SetOnRequestCanceled as an ErrLocalTransport.
func (s *localTransportServerSuite) TestServerWriteFailureClassifiedAsLocalTransport() {
	t := s.T()
	clientID := "client1"
	errMsg := "mockServerWriteError"
	s.websocketServer.On("Write", mock.AnythingOfType("string"), mock.Anything).Return(fmt.Errorf(errMsg))

	canceled := make(chan *ocpp.Error, 1)
	s.dispatcher.SetOnRequestCanceled(func(cID string, rID string, request ocpp.Request, err *ocpp.Error) {
		canceled <- err
	})

	s.dispatcher.Start()
	require.True(t, s.dispatcher.IsRunning())
	s.dispatcher.CreateClient(clientID)

	req := newMockRequest("somevalue")
	call, err := s.endpoint.CreateCall(req)
	require.NoError(t, err)
	data, err := call.MarshalJSON()
	require.NoError(t, err)
	bundle := ocppj.RequestBundle{Call: call, Data: data}
	err = s.dispatcher.SendRequest(clientID, bundle)
	require.NoError(t, err)

	var cancelErr *ocpp.Error
	select {
	case cancelErr = <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for OnRequestCanceled")
	}

	require.NotNil(t, cancelErr)
	assert.True(t, errors.Is(cancelErr, ocppj.ErrLocalTransport),
		"server write-failure error must classify as ErrLocalTransport")
	assert.False(t, errors.Is(cancelErr, ocppj.ErrRequestTimeout),
		"a server write-failure must NOT be misclassified as a request timeout")
}

// Test 4 - B2 server timeout classification (the asymmetry fix): Write succeeds
// but no response is ever delivered, so a short SetTimeout fires the server-side
// timeout. This currently FAILS: the server timeout path (dispatcher.go ~:732)
// is untagged today, so its error does not match ErrRequestTimeout until B2b
// tags it with newRequestTimeoutError.
func (s *localTransportServerSuite) TestServerTimeoutClassifiedAsRequestTimeout() {
	t := s.T()
	clientID := "client1"
	s.websocketServer.On("Write", mock.AnythingOfType("string"), mock.Anything).Return(nil)

	canceled := make(chan *ocpp.Error, 1)
	s.dispatcher.SetOnRequestCanceled(func(cID string, rID string, request ocpp.Request, err *ocpp.Error) {
		canceled <- err
	})

	s.dispatcher.SetTimeout(200 * time.Millisecond)
	s.dispatcher.Start()
	require.True(t, s.dispatcher.IsRunning())
	s.dispatcher.CreateClient(clientID)

	req := newMockRequest("somevalue")
	call, err := s.endpoint.CreateCall(req)
	require.NoError(t, err)
	data, err := call.MarshalJSON()
	require.NoError(t, err)
	bundle := ocppj.RequestBundle{Call: call, Data: data}
	err = s.dispatcher.SendRequest(clientID, bundle)
	require.NoError(t, err)

	var cancelErr *ocpp.Error
	select {
	case cancelErr = <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for OnRequestCanceled")
	}

	require.NotNil(t, cancelErr)
	assert.True(t, errors.Is(cancelErr, ocppj.ErrRequestTimeout),
		"server request-timeout error must classify as ErrRequestTimeout (closes the client/server asymmetry)")
	// Explicitly exclude ErrLocalTransport: this forces the B2b explicit
	// newRequestTimeoutError swap at dispatcher.go:~732. An impl that skips 2b
	// and relies on the Change-3 fail-safe backstop would stamp the
	// local-transport marker on this (empty-marker) timeout error — this assert
	// catches that mislabel, since the backstop can never fabricate ErrRequestTimeout.
	assert.False(t, errors.Is(cancelErr, ocppj.ErrLocalTransport),
		"a server request-timeout must NOT be defaulted to ErrLocalTransport (the backstop must not mask a missing 2b timeout tag)")
}

func TestLocalTransportServerSuite(t *testing.T) {
	suite.Run(t, new(localTransportServerSuite))
}
