package ws

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

// ---------------------- SERVER ----------------------

type CheckClientHandler func(id string, r *http.Request) bool

// DuplicateConnectionBehavior controls what the server does when a charge point
// opens a new connection while one with the same ID is already connected.
type DuplicateConnectionBehavior uint8

const (
	// DuplicateConnectionBehaviorKeepCurrent rejects the new connection and keeps
	// the existing one. This is the default.
	DuplicateConnectionBehaviorKeepCurrent DuplicateConnectionBehavior = iota
	// DuplicateConnectionBehaviorKeepNew closes the existing connection and accepts
	// the new one.
	DuplicateConnectionBehaviorKeepNew
)

// Server defines a websocket server, which passively listens for incoming connections on ws or wss protocol.
// The offered API are of asynchronous nature, and each incoming connection/message is handled using callbacks.
//
// To create a new ws server, use:
//
//	server := NewServer()
//
// If you need a server with TLS support, pass the following option:
//
//	server := NewServer(WithServerTLSConfig("cert.pem", "privateKey.pem", nil))
//
// To support client basic authentication, use:
//
//	server.SetBasicAuthHandler(func (user, pass) bool {
//		ok := authenticate(user, pass) // ... check for user and pass correctness
//		return ok
//	})
//
// To specify supported sub-protocols, use:
//
//	server.AddSupportedSubprotocol("ocpp1.6")
//
// If you need to set a specific timeout configuration, refer to the SetTimeoutConfig method.
//
// Using Start and Stop you can respectively start and stop listening for incoming client websocket connections.
//
// To be notified of new and terminated connections,
// refer to SetNewClientHandler and SetDisconnectedClientHandler functions.
//
// To receive incoming messages, you will need to set your own handler using SetMessageHandler.
// To write data on the open socket, simply call the Write function.
type Server interface {
	// Starts and runs the websocket server on a specific port and URL.
	// After start, incoming connections and messages are handled automatically, so no explicit read operation is required.
	//
	// The functions blocks forever, hence it is suggested to invoke it in a goroutine, if the caller thread needs to perform other work, e.g.:
	//	go server.Start(8887, "/ws/{id}")
	//	doStuffOnMainThread()
	//	...
	//
	// To stop a running server, call the Stop function.
	Start(port int, listenPath string)
	// Shuts down a running websocket server.
	// All open channels will be forcefully closed, and the previously called Start function will return.
	Stop()
	// Closes a specific websocket connection.
	StopConnection(id string, closeError websocket.CloseError) error
	// Errors returns a buffered channel for asynchronous error messages.
	// The channel is created when the server is constructed and is closed by the
	// server when stopped. Delivery is best-effort and lossy: the channel has a
	// small buffer and errors are dropped (not blocked on) when it is full, so a
	// slow or absent consumer never blocks the server. Consume it promptly if you
	// rely on error notifications.
	Errors() <-chan error
	// Sets a callback function for all incoming messages.
	// The callbacks accept a Channel and the received data.
	// It is up to the callback receiver, to check the identifier of the channel, to determine the source of the message.
	SetMessageHandler(handler MessageHandler)
	// SetNewClientHandler sets a callback function for all new incoming client connections.
	// It is recommended to store a reference to the Channel in the received entity, so that the Channel may be recognized later on.
	//
	// The callback is invoked after a connection was established and upgraded successfully.
	// If custom checks need to be run beforehand, refer to SetCheckClientHandler.
	SetNewClientHandler(handler ConnectedHandler)
	// Sets a callback function for all client disconnection events.
	// Once a client is disconnected, it is not possible to read/write on the respective Channel any longer.
	SetDisconnectedClientHandler(handler func(ws Channel))
	// Set custom timeout configuration parameters. If not passed, a default ServerTimeoutConfig struct will be used.
	//
	// This function must be called before starting the server, otherwise it may lead to unexpected behavior.
	SetTimeoutConfig(config ServerTimeoutConfig)
	// Write sends a message on a specific Channel, identifier by the webSocketId parameter.
	// If the passed ID is invalid, an error is returned.
	//
	// The data is queued and will be sent asynchronously in the background.
	Write(webSocketId string, data []byte) error
	// AddSupportedSubprotocol adds support for a specified subprotocol.
	// This is recommended in order to communicate the capabilities to the client during the handshake.
	// If left empty, any subprotocol will be accepted.
	//
	// Duplicates will be removed automatically.
	AddSupportedSubprotocol(subProto string)
	// SetChargePointIdResolver sets the callback function to use for resolving the charge point ID of a charger connecting to
	// the websocket server. By default, this will just be the path in the URL used by the client.
	SetChargePointIdResolver(resolver func(r *http.Request) (string, error))
	// SetBasicAuthHandler enables HTTP Basic Authentication and requires clients to pass credentials.
	// The handler function is called whenever a new client attempts to connect, to check for credentials correctness.
	// The handler must return true if the credentials were correct, false otherwise.
	SetBasicAuthHandler(handler func(username string, password string) bool)
	// SetCheckOriginHandler sets a handler for incoming websocket connections, allowing to perform
	// custom cross-origin checks.
	//
	// By default, if the Origin header is present in the request, and the Origin host is not equal
	// to the Host request header, the websocket handshake fails.
	SetCheckOriginHandler(handler func(r *http.Request) bool)
	// SetCheckClientHandler sets a handler for validate incoming websocket connections, allowing to perform
	// custom client connection checks.
	// The handler is executed before any connection upgrade and allows optionally returning a custom
	// configuration for the web socket that will be created.
	//
	// Changes to the http request at runtime may lead to undefined behavior.
	SetCheckClientHandler(handler CheckClientHandler)
	// SetDuplicateConnectionBehavior controls how a new connection from a charge
	// point ID that is already connected is handled. The default keeps the current
	// connection and rejects the new one; it can be set to instead close the current
	// connection and accept the new one. Note the security implication: allowing the
	// new connection lets a party with a valid (or guessable) ID forcibly disconnect
	// an active charger, so use it only with adequate authentication.
	SetDuplicateConnectionBehavior(behavior DuplicateConnectionBehavior)
	// Addr gives the address on which the server is listening, useful if, for
	// example, the port is system-defined (set to 0).
	Addr() *net.TCPAddr
	// GetChannel retrieves an active Channel connection by its unique identifier.
	// If a connection with the given ID exists, it returns the corresponding webSocket instance.
	// If no connection is found with the specified ID, it returns nil and a false flag.
	GetChannel(websocketId string) (Channel, bool)
}

// Default implementation of a Websocket server.
//
// Use the NewServer function to create a new server.
type server struct {
	connections                 map[string]*webSocket
	httpServer                  *http.Server
	messageHandler              func(ws Channel, data []byte) error
	chargePointIdResolver       func(*http.Request) (string, error)
	checkClientHandler          CheckClientHandler
	duplicateConnectionBehavior DuplicateConnectionBehavior
	newClientHandler            func(ws Channel)
	disconnectedHandler         func(ws Channel)
	basicAuthHandler            func(username string, password string) bool
	tlsCertificatePath          string
	tlsCertificateKey           string
	timeoutConfig               ServerTimeoutConfig
	upgrader                    websocket.Upgrader
	errC                        chan error
	errClosed                   bool // true once errC has been closed, to prevent double-close and sends on a closed channel
	connMutex                   sync.RWMutex
	addr                        *net.TCPAddr
	httpHandler                 *mux.Router
}

// ServerOpt is a function that can be used to set options on a server during creation.
type ServerOpt func(s *server)

// WithServerTLSConfig sets the TLS configuration for the server.
// If the passed tlsConfig is nil, the client will not use TLS.
func WithServerTLSConfig(certificatePath string, certificateKey string, tlsConfig *tls.Config) ServerOpt {
	return func(s *server) {
		s.tlsCertificatePath = certificatePath
		s.tlsCertificateKey = certificateKey
		if tlsConfig != nil {
			s.httpServer.TLSConfig = tlsConfig
		}
	}
}

// NewServer Creates a new websocket server.
//
// Additional options may be added using the AddOption function.
//
// By default, the websockets are not secure, and the server will not perform any client certificate verification.
//
// To add TLS support to the server, a valid server certificate path and key must be passed.
// To also add support for client certificate verification, a valid TLSConfig needs to be configured.
// For example:
//
//		tlsConfig := &tls.Config{
//			ClientAuth: tls.RequireAndVerifyClientCert,
//			ClientCAs: clientCAs,
//		}
//	 server := ws.NewServer(ws.WithServerTLSConfig("cert.pem", "privateKey.pem", tlsConfig))
//
// When TLS is correctly configured, the server will automatically use it for all created websocket channels.
func NewServer(opts ...ServerOpt) Server {
	router := mux.NewRouter()
	s := &server{
		httpServer:                  &http.Server{},
		timeoutConfig:               NewServerTimeoutConfig(),
		upgrader:                    websocket.Upgrader{Subprotocols: []string{}},
		errC:                        make(chan error, 1),
		httpHandler:                 router,
		duplicateConnectionBehavior: DuplicateConnectionBehaviorKeepCurrent,
		chargePointIdResolver: func(r *http.Request) (string, error) {
			url := r.URL
			return path.Base(url.Path), nil
		},
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

func (s *server) SetMessageHandler(handler MessageHandler) {
	s.messageHandler = handler
}

func (s *server) SetCheckClientHandler(handler CheckClientHandler) {
	s.checkClientHandler = handler
}

func (s *server) SetDuplicateConnectionBehavior(behavior DuplicateConnectionBehavior) {
	s.duplicateConnectionBehavior = behavior
}

func (s *server) SetNewClientHandler(handler ConnectedHandler) {
	s.newClientHandler = handler
}

func (s *server) SetDisconnectedClientHandler(handler func(ws Channel)) {
	s.disconnectedHandler = handler
}

func (s *server) SetTimeoutConfig(config ServerTimeoutConfig) {
	s.timeoutConfig = config
}

func (s *server) AddSupportedSubprotocol(subProto string) {
	for _, sub := range s.upgrader.Subprotocols {
		if sub == subProto {
			// Don't add duplicates
			return
		}
	}
	s.upgrader.Subprotocols = append(s.upgrader.Subprotocols, subProto)
}

func (s *server) SetChargePointIdResolver(resolver func(r *http.Request) (string, error)) {
	s.chargePointIdResolver = resolver
}

func (s *server) SetBasicAuthHandler(handler func(username string, password string) bool) {
	s.basicAuthHandler = handler
}

func (s *server) SetCheckOriginHandler(handler func(r *http.Request) bool) {
	s.upgrader.CheckOrigin = handler
}

func (s *server) error(err error) {
	log.Error(err)
	// Guard the send with the mutex so it cannot race with Stop() closing the
	// channel. The send is non-blocking (buffered channel with a default case),
	// so the mutex is never held across a blocking channel operation.
	s.connMutex.RLock()
	defer s.connMutex.RUnlock()
	if s.errClosed {
		return
	}
	select {
	case s.errC <- err:
	default:
	}
}

func (s *server) Errors() <-chan error {
	s.connMutex.RLock()
	defer s.connMutex.RUnlock()
	return s.errC
}

func (s *server) Addr() *net.TCPAddr {
	s.connMutex.RLock()
	defer s.connMutex.RUnlock()
	return s.addr
}

func (s *server) AddHttpHandler(listenPath string, handler func(w http.ResponseWriter, r *http.Request)) {
	// The mux router is not safe for concurrent registration. Guard it with the
	// shared mutex so concurrent AddHttpHandler/Start calls don't race.
	s.connMutex.Lock()
	defer s.connMutex.Unlock()
	s.httpHandler.HandleFunc(listenPath, handler)
}

func (s *server) Start(port int, listenPath string) {
	addr := fmt.Sprintf(":%v", port)

	// Configure lifecycle fields under the lock so they don't race with Stop(),
	// Addr() and concurrent AddHttpHandler calls. The lock is released before
	// the blocking Serve call.
	s.connMutex.Lock()
	s.connections = make(map[string]*webSocket)
	if s.httpServer == nil {
		s.httpServer = &http.Server{}
	}
	s.httpServer.Addr = addr
	// Register the websocket handler directly on the router (we already hold the
	// lock, so we must not call AddHttpHandler, which re-acquires it).
	s.httpHandler.HandleFunc(listenPath, func(w http.ResponseWriter, r *http.Request) {
		s.wsHandler(w, r)
	})
	s.httpServer.Handler = s.httpHandler
	// Snapshot the server so we can call Serve without holding the lock.
	httpServer := s.httpServer
	s.connMutex.Unlock()

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		s.error(fmt.Errorf("failed to listen: %w", err))
		return
	}

	s.connMutex.Lock()
	s.addr = ln.Addr().(*net.TCPAddr)
	s.connMutex.Unlock()

	defer ln.Close()

	log.Infof("listening on tcp network %v", addr)
	httpServer.RegisterOnShutdown(s.stopConnections)
	if s.tlsCertificatePath != "" && s.tlsCertificateKey != "" {
		err = httpServer.ServeTLS(ln, s.tlsCertificatePath, s.tlsCertificateKey)
	} else {
		err = httpServer.Serve(ln)
	}

	if !errors.Is(err, http.ErrServerClosed) {
		s.error(fmt.Errorf("failed to listen: %w", err))
	}
}

func (s *server) Stop() {
	log.Info("stopping websocket server")
	// Snapshot the http server under the lock, then shut it down outside the
	// lock (Shutdown blocks until active connections drain).
	s.connMutex.RLock()
	httpServer := s.httpServer
	s.connMutex.RUnlock()
	if httpServer != nil {
		err := httpServer.Shutdown(context.TODO())
		if err != nil {
			s.error(fmt.Errorf("shutdown failed: %w", err))
		}
	}

	// Close the error channel (close-once, guarded by the mutex to avoid racing
	// with concurrent error() senders). The channel is not niled out, so a
	// concurrent sender never observes a nil channel.
	s.connMutex.Lock()
	if !s.errClosed {
		s.errClosed = true
		close(s.errC)
	}
	s.connMutex.Unlock()
}

func (s *server) StopConnection(id string, closeError websocket.CloseError) error {
	s.connMutex.RLock()
	w, ok := s.connections[id]
	s.connMutex.RUnlock()

	if !ok {
		return fmt.Errorf("couldn't stop websocket connection. No connection with id %s is open", id)
	}
	log.Debugf("sending stop signal for websocket %s", w.ID())
	return w.Close(closeError)
}

func (s *server) GetChannel(websocketId string) (Channel, bool) {
	s.connMutex.RLock()
	defer s.connMutex.RUnlock()
	c, ok := s.connections[websocketId]
	return c, ok
}

func (s *server) stopConnections() {
	s.connMutex.RLock()
	defer s.connMutex.RUnlock()
	for _, conn := range s.connections {
		_ = conn.Close(websocket.CloseError{Code: websocket.CloseNormalClosure, Text: ""})
	}
}

func (s *server) Write(webSocketId string, data []byte) error {
	s.connMutex.RLock()
	w, ok := s.connections[webSocketId]
	s.connMutex.RUnlock()
	if !ok {
		return fmt.Errorf("couldn't write to websocket. No socket with id %v is open", webSocketId)
	}
	log.Debugf("queuing data for websocket %s", webSocketId)
	return w.Write(data)
}

func (s *server) wsHandler(w http.ResponseWriter, r *http.Request) {
	responseHeader := http.Header{}
	id, err := s.chargePointIdResolver(r)
	if err != nil {
		s.error(fmt.Errorf("failed to resolve charge point id"))
		http.Error(w, "NotFound", http.StatusNotFound)
		return
	}
	log.Debugf("handling new connection for %s from %s", id, r.RemoteAddr)
	// Negotiate sub-protocol
	clientSubProtocols := websocket.Subprotocols(r)
	negotiatedSubProtocol := ""
out:
	for _, requestedProto := range clientSubProtocols {
		if len(s.upgrader.Subprotocols) == 0 {
			// All subProtocols are accepted, pick first
			negotiatedSubProtocol = requestedProto
			break
		}
		// Check if requested suprotocol is supported by server
		for _, supportedProto := range s.upgrader.Subprotocols {
			if requestedProto == supportedProto {
				negotiatedSubProtocol = requestedProto
				break out
			}
		}
	}
	if negotiatedSubProtocol != "" {
		responseHeader.Add("Sec-WebSocket-Protocol", negotiatedSubProtocol)
	}
	// Handle client authentication
	if s.basicAuthHandler != nil {
		username, password, ok := r.BasicAuth()
		if ok {
			ok = s.basicAuthHandler(username, password)
		}
		if !ok {
			s.error(fmt.Errorf("basic auth failed: credentials invalid"))
			w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}
	// Custom client checks
	if s.checkClientHandler != nil {
		ok := s.checkClientHandler(id, r)
		if !ok {
			s.error(fmt.Errorf("client validation: invalid client"))
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}

	// Upgrade websocket
	conn, err := s.upgrader.Upgrade(w, r, responseHeader)
	if err != nil {
		s.error(fmt.Errorf("upgrade failed: %w", err))
		return
	}

	log.Debugf("upgraded websocket connection for %s from %s", id, conn.RemoteAddr().String())
	// If unsupported sub-protocol, terminate the connection immediately
	if negotiatedSubProtocol == "" {
		s.error(fmt.Errorf("unsupported subprotocols %v for new client %v (%v)", clientSubProtocols, id, r.RemoteAddr))
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseProtocolError, "invalid or unsupported subprotocol"),
			time.Now().Add(s.timeoutConfig.WriteWait))
		_ = conn.Close()
		return
	}
	// Check whether a client with this ID is already connected.
	var staleConn *webSocket
	s.connMutex.Lock()
	switch s.duplicateConnectionBehavior {
	case DuplicateConnectionBehaviorKeepNew:
		// Accept the new connection and close the existing one. We capture the
		// existing connection here but Close it AFTER releasing connMutex (below),
		// using the webSocket's own concurrency-safe Close — never holding connMutex
		// across the close, and never touching the raw connection (which would race
		// the existing connection's read/write pumps). handleDisconnect deletes map
		// entries by identity, so the stale connection's later disconnect won't evict
		// the new entry registered below. (See SetDuplicateConnectionBehavior for the
		// security implications.) NOTE: do not call s.error() here — it takes
		// connMutex.RLock(), which would self-deadlock against this write lock.
		if currentConn, exists := s.connections[id]; exists {
			staleConn = currentConn
		}
	default:
		// Keep the current connection: reject the new one immediately with a PolicyViolation.
		if _, exists := s.connections[id]; exists {
			s.connMutex.Unlock()
			s.error(fmt.Errorf("client %s already exists, closing duplicate client", id))
			_ = conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "a connection with this ID already exists"),
				time.Now().Add(s.timeoutConfig.WriteWait))
			_ = conn.Close()
			return
		}
	}
	// Create web socket for client, state is automatically set to connected
	ws := newWebSocket(
		id,
		conn,
		r.TLS,
		NewDefaultWebSocketConfig(
			s.timeoutConfig.WriteWait,
			s.timeoutConfig.PingWait,
			s.timeoutConfig.PingPeriod,
			s.timeoutConfig.PongWait),
		s.handleMessage,
		s.handleDisconnect,
		func(_ Channel, err error) {
			s.error(err)
		},
	)
	// Add new client
	s.connections[ws.id] = ws
	s.connMutex.Unlock()
	// Close any displaced connection (KeepNew) outside the lock, so neither a
	// blocking close nor s.error() (which takes connMutex.RLock) can stall
	// connMutex for other handlers.
	if staleConn != nil {
		s.error(fmt.Errorf("client %s already exists, closing existing client", id))
		_ = staleConn.Close(websocket.CloseError{Code: websocket.ClosePolicyViolation, Text: "a connection with this ID has reconnected"})
	}
	// Start reader and write routine
	ws.run()
	if s.newClientHandler != nil {
		var channel Channel = ws
		s.newClientHandler(channel)
	}
}

// --------- Internal callbacks webSocket -> server ---------
func (s *server) handleMessage(w Channel, data []byte) error {
	if s.messageHandler != nil {
		return s.messageHandler(w, data)
	}
	return fmt.Errorf("no message handler set")
}

func (s *server) handleDisconnect(w Channel, _ error) {
	// server never attempts to auto-reconnect to client. Resources are simply freed up
	s.connMutex.Lock()
	// Only act if the map still points to THIS connection. With
	// DuplicateConnectionBehaviorKeepNew a replacement may already be registered
	// under the same ID; a stale disconnect must neither evict the replacement nor
	// notify the upper layer (which would tear down dispatcher/pending state for an
	// ID that is currently connected via the new socket).
	current := false
	if existing, ok := s.connections[w.ID()]; ok && Channel(existing) == w {
		delete(s.connections, w.ID())
		current = true
	}
	s.connMutex.Unlock()
	log.Infof("closed connection to %s", w.ID())
	if current && s.disconnectedHandler != nil {
		s.disconnectedHandler(w)
	}
}
