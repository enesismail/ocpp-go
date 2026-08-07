package ocpp16

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"

	"github.com/enesismail/ocpp-go/internal/callbackqueue"
	"github.com/enesismail/ocpp-go/ocpp"
	"github.com/enesismail/ocpp-go/ocpp1.6/certificates"
	"github.com/enesismail/ocpp-go/ocpp1.6/core"
	"github.com/enesismail/ocpp-go/ocpp1.6/extendedtriggermessage"
	"github.com/enesismail/ocpp-go/ocpp1.6/firmware"
	"github.com/enesismail/ocpp-go/ocpp1.6/localauth"
	"github.com/enesismail/ocpp-go/ocpp1.6/logging"
	"github.com/enesismail/ocpp-go/ocpp1.6/remotetrigger"
	"github.com/enesismail/ocpp-go/ocpp1.6/reservation"
	"github.com/enesismail/ocpp-go/ocpp1.6/securefirmware"
	"github.com/enesismail/ocpp-go/ocpp1.6/security"
	"github.com/enesismail/ocpp-go/ocpp1.6/smartcharging"
	"github.com/enesismail/ocpp-go/ocpp1.6/types"
	"github.com/enesismail/ocpp-go/ocppj"
)

type incomingKind int

const (
	incomingResponse incomingKind = iota
	incomingError
	incomingRequest
)

type incomingMessage struct {
	kind         incomingKind
	confirmation ocpp.Response
	err          error
	request      ocpp.Request
	requestID    string
	action       string
}

// asyncResponse wraps an asynchronous response for delivery via a channel
// from the callback to the sync-send select.
type asyncResponse struct {
	r ocpp.Response
	e error
}

type chargePoint struct {
	client                        *ocppj.Client
	coreHandler                   core.ChargePointHandler
	localAuthListHandler          localauth.ChargePointHandler
	firmwareHandler               firmware.ChargePointHandler
	reservationHandler            reservation.ChargePointHandler
	remoteTriggerHandler          remotetrigger.ChargePointHandler
	smartChargingHandler          smartcharging.ChargePointHandler
	securityHandler               security.ChargePointHandler
	logHandler                    logging.ChargePointHandler
	extendedTriggerMessageHandler extendedtriggermessage.ChargePointHandler
	secureFirmwareHandler         securefirmware.ChargePointHandler
	certificateHandler            certificates.ChargePointHandler
	onHandlerPanic                func(ocppj.HandlerPanic)
	incoming                      chan incomingMessage
	callbacks                     callbackqueue.CallbackQueue
	stopC                         atomic.Value // holds chan struct{}; see loadStopC/storeStopC
	stopOnce                      *sync.Once
	errC                          chan error // external error channel
	// mu is the PR-L1 lifecycle mutex. Scope is deliberately narrow: it
	// guards ONLY errC's lazy creation (Errors()/error()) and handlerDone
	// bookkeeping (loadHandlerDone/storeHandlerDone) - stopC stays lock-free
	// atomic.Value (loadStopC/storeStopC), unchanged from PR-L2.
	//
	// HOLD-SCOPE RULE (mandatory): mu must NEVER be held across
	// client.Stop(), the handlerDone join, or any channel send/receive -
	// every critical section below is a field copy only, with the lock
	// released before the channel op it enables. Violating this recreates a
	// three-party deadlock: the ocppj dispatcher pump -> onRequestTimeout (or
	// error(), called from the pump/handler) blocks trying to take mu;
	// dispatcher.Stop() waits on the pump to finish; facade Stop() holds mu
	// waiting on dispatcher.Stop(). See
	// tasks/facade-lifecycle-hardening.md §5 for the full derivation.
	mu sync.Mutex
	// handlerDone is the CURRENT generation's "asyncCallbackHandler has
	// exited" signal, closed by the handler itself (never by Stop/StopCtx)
	// on its way out. Pre-closed in NewChargePoint - NEVER nil - and
	// replaced with a fresh, open channel by Start alongside a freshly
	// spawned handler (see Start, retirePreviousGeneration, joinHandler).
	handlerDone chan struct{}
}

// loadStopC returns the current generation's stop signal channel, or nil if
// Start has never been called. atomic.Value (not a plain field) because
// three independent goroutine families read stopC - the ocppj dispatcher
// pump (onRequestTimeout), the ws readPump (the forwarding closures wired in
// NewChargePoint), and SendRequestCtx callers - while Start reassigns it on
// every call; a plain field read/write pair here is a data race (see
// TestL2ShutdownRestartStopCRace under -race). This accessor is the ONLY
// synchronization around stopC: per the HOLD-SCOPE rule
// (tasks/facade-lifecycle-hardening.md §PR-L2 item 1), no lock is ever held
// across it, a channel op, client.Stop(), or a join - holding one would
// recreate the exact three-party deadlock (pump -> hook -> accessor blocks;
// dispatcher.Stop() waits on the pump; facade Stop() holds the lock waiting
// on dispatcher.Stop()) this accessor exists to avoid.
func (cp *chargePoint) loadStopC() chan struct{} {
	v, _ := cp.stopC.Load().(chan struct{})
	return v
}

func (cp *chargePoint) storeStopC(c chan struct{}) {
	cp.stopC.Store(c)
}

// loadHandlerDone and storeHandlerDone are the mu-guarded accessors for
// handlerDone (see the mu field doc's HOLD-SCOPE RULE): the lock is held only
// long enough to copy/assign the field, never across the channel op the
// caller goes on to perform.
func (cp *chargePoint) loadHandlerDone() chan struct{} {
	cp.mu.Lock()
	hd := cp.handlerDone
	cp.mu.Unlock()
	return hd
}

func (cp *chargePoint) storeHandlerDone(hd chan struct{}) {
	cp.mu.Lock()
	cp.handlerDone = hd
	cp.mu.Unlock()
}

func (cp *chargePoint) error(err error) {
	// errC is read as a field copy under mu, then the lock is released
	// before the channel op below - this is what makes it safe against
	// Errors()' lazy creation (spec §5; see
	// TestL1ErrorsLazyCreationRaceSameChannel) without violating the
	// HOLD-SCOPE rule on the mu field doc.
	cp.mu.Lock()
	errC := cp.errC
	cp.mu.Unlock()
	if errC == nil {
		return
	}
	// Preemptible: error() runs on whichever goroutine reports it (the async
	// handler, or - via sendResponse et al - request-handling code on that
	// same goroutine). A caller that obtains Errors() and never drains it
	// would otherwise wedge that goroutine forever inside `errC <- err`,
	// which is exactly what would block the PR-L1 generation join. See spec
	// §L2 PR-L2 item 2, "error() on both facades".
	select {
	case errC <- err:
	case <-cp.loadStopC():
	}
}

// errorNonBlocking reports err on the Errors() channel with a NON-BLOCKING
// send: if Errors() was never called, or the cap-1 buffer is full, err is
// DROPPED. It exists solely for the recovered-panic route installed in
// NewChargePoint, which runs on the ocppj client's websocket read goroutine, on
// this facade's asyncCallbackHandler goroutine (the sole consumer of cp.incoming,
// where user response/error/request handlers actually run), and on the client
// dispatcher's cancel-recover path (its messagePump goroutine) — goroutines that
// must never park on a channel the consumer has stopped draining. error() is
// preemptible only while a generation is running: before the first Start,
// loadStopC() is nil, so a blocking send on a full errC there parks forever
// rather than until Stop. Every other producer keeps error()'s blocking,
// stopC-preemptible semantics (PR #37/#38); do not swap one for the other in
// either direction.
//
// errC is read as a field copy under mu and the lock is released before the
// send, per the HOLD-SCOPE rule on the mu field doc. A nil errC needs no
// special case: a send on a nil channel never proceeds, so the select takes
// default and the report is dropped. errC creation stays LAZY on the client —
// do not add eager creation here (that is the server-side EC2 design).
func (cp *chargePoint) errorNonBlocking(err error) {
	cp.mu.Lock()
	errC := cp.errC
	cp.mu.Unlock()
	select {
	case errC <- err:
	default:
	}
}

// Callback invoked whenever a queued request is canceled, due to timeout.
// By default, the callback returns a GenericError to the caller, who sent the original request.
func (cp *chargePoint) onRequestTimeout(_ string, _ ocpp.Request, err *ocpp.Error) {
	// Preemptible: runs on the ocppj dispatcher's messagePump goroutine,
	// sequentially, for every request canceled at Stop()-time or on timeout
	// (ocppj/dispatcher.go's drain-and-cancel loop). A blocking send into
	// cp.incoming (cap 1) with no reader wedges the pump - and
	// DefaultClientDispatcher.Stop() (called from client.Stop()) waits
	// unconditionally on that pump reaching done, so a wedge here hangs
	// facade Stop() forever. See spec §L2.
	select {
	case cp.incoming <- incomingMessage{kind: incomingError, err: err}:
	case <-cp.loadStopC():
	}
}

// Errors returns a channel for error messages. If it doesn't exist it is
// created. See the ChargePoint interface godoc (v16.go) for the full
// contract - in short: errC is NEVER closed and is process-lifetime, not
// per-generation. Recovered handler panics are reported here as
// *ocppj.HandlerPanicError, best-effort: that send is non-blocking, so a panic
// reported while the buffer is full or before the first Errors() call is
// dropped. Use SetOnHandlerPanic to observe every panic the facade or the
// default dispatcher recovers; panics recovered inside a custom
// ClientDispatcher reach neither the hook nor Errors().
//
// errC's lazy creation is guarded by mu so two concurrent first-callers
// cannot each create a different channel and silently lose one (spec §5).
// The lock is held only for the field check/create/copy, never across a
// channel op (HOLD-SCOPE rule on the mu field doc).
func (cp *chargePoint) Errors() <-chan error {
	cp.mu.Lock()
	if cp.errC == nil {
		cp.errC = make(chan error, 1)
	}
	errC := cp.errC
	cp.mu.Unlock()
	return errC
}

func (cp *chargePoint) BootNotification(chargePointModel string, chargePointVendor string, props ...func(request *core.BootNotificationRequest)) (*core.BootNotificationConfirmation, error) {
	request := core.NewBootNotificationRequest(chargePointModel, chargePointVendor)
	for _, fn := range props {
		fn(request)
	}
	confirmation, err := cp.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return confirmation.(*core.BootNotificationConfirmation), err
	}
}

func (cp *chargePoint) Authorize(idTag string, props ...func(request *core.AuthorizeRequest)) (*core.AuthorizeConfirmation, error) {
	request := core.NewAuthorizationRequest(idTag)
	for _, fn := range props {
		fn(request)
	}
	confirmation, err := cp.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return confirmation.(*core.AuthorizeConfirmation), err
	}
}

func (cp *chargePoint) DataTransfer(vendorId string, props ...func(request *core.DataTransferRequest)) (*core.DataTransferConfirmation, error) {
	request := core.NewDataTransferRequest(vendorId)
	for _, fn := range props {
		fn(request)
	}
	confirmation, err := cp.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return confirmation.(*core.DataTransferConfirmation), err
	}
}

func (cp *chargePoint) Heartbeat(props ...func(request *core.HeartbeatRequest)) (*core.HeartbeatConfirmation, error) {
	request := core.NewHeartbeatRequest()
	for _, fn := range props {
		fn(request)
	}
	confirmation, err := cp.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return confirmation.(*core.HeartbeatConfirmation), err
	}
}

func (cp *chargePoint) MeterValues(connectorId int, meterValues []types.MeterValue, props ...func(request *core.MeterValuesRequest)) (*core.MeterValuesConfirmation, error) {
	request := core.NewMeterValuesRequest(connectorId, meterValues)
	for _, fn := range props {
		fn(request)
	}
	confirmation, err := cp.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return confirmation.(*core.MeterValuesConfirmation), err
	}
}

func (cp *chargePoint) StartTransaction(connectorId int, idTag string, meterStart int, timestamp *types.DateTime, props ...func(request *core.StartTransactionRequest)) (*core.StartTransactionConfirmation, error) {
	request := core.NewStartTransactionRequest(connectorId, idTag, meterStart, timestamp)
	for _, fn := range props {
		fn(request)
	}
	confirmation, err := cp.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return confirmation.(*core.StartTransactionConfirmation), err
	}
}

func (cp *chargePoint) StopTransaction(meterStop int, timestamp *types.DateTime, transactionId int, props ...func(request *core.StopTransactionRequest)) (*core.StopTransactionConfirmation, error) {
	request := core.NewStopTransactionRequest(meterStop, timestamp, transactionId)
	for _, fn := range props {
		fn(request)
	}
	confirmation, err := cp.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return confirmation.(*core.StopTransactionConfirmation), err
	}
}

func (cp *chargePoint) StatusNotification(connectorId int, errorCode core.ChargePointErrorCode, status core.ChargePointStatus, props ...func(request *core.StatusNotificationRequest)) (*core.StatusNotificationConfirmation, error) {
	request := core.NewStatusNotificationRequest(connectorId, errorCode, status)
	for _, fn := range props {
		fn(request)
	}
	confirmation, err := cp.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return confirmation.(*core.StatusNotificationConfirmation), err
	}
}

func (cp *chargePoint) DiagnosticsStatusNotification(status firmware.DiagnosticsStatus, props ...func(request *firmware.DiagnosticsStatusNotificationRequest)) (*firmware.DiagnosticsStatusNotificationConfirmation, error) {
	request := firmware.NewDiagnosticsStatusNotificationRequest(status)
	for _, fn := range props {
		fn(request)
	}
	confirmation, err := cp.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return confirmation.(*firmware.DiagnosticsStatusNotificationConfirmation), err
	}
}

func (cp *chargePoint) FirmwareStatusNotification(status firmware.FirmwareStatus, props ...func(request *firmware.FirmwareStatusNotificationRequest)) (*firmware.FirmwareStatusNotificationConfirmation, error) {
	request := firmware.NewFirmwareStatusNotificationRequest(status)
	for _, fn := range props {
		fn(request)
	}
	confirmation, err := cp.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return confirmation.(*firmware.FirmwareStatusNotificationConfirmation), err
	}
}

func (cp *chargePoint) SecurityEventNotification(typ string, timestamp *types.DateTime, props ...func(request *security.SecurityEventNotificationRequest)) (*security.SecurityEventNotificationResponse, error) {
	request := security.NewSecurityEventNotificationRequest(typ, timestamp)
	for _, fn := range props {
		fn(request)
	}
	confirmation, err := cp.SendRequest(request)
	if err != nil {
		return nil, err
	}
	return confirmation.(*security.SecurityEventNotificationResponse), err
}

func (cp *chargePoint) SignCertificate(CSR string, props ...func(request *security.SignCertificateRequest)) (*security.SignCertificateResponse, error) {
	request := security.NewSignCertificateRequest(CSR)
	for _, fn := range props {
		fn(request)
	}
	confirmation, err := cp.SendRequest(request)
	if err != nil {
		return nil, err
	}
	return confirmation.(*security.SignCertificateResponse), err
}

func (cp *chargePoint) SignedUpdateFirmwareStatusNotification(status securefirmware.FirmwareStatus, props ...func(request *securefirmware.SignedFirmwareStatusNotificationRequest)) (*securefirmware.SignedFirmwareStatusNotificationResponse, error) {
	request := securefirmware.NewFirmwareStatusNotificationRequest(status)
	for _, fn := range props {
		fn(request)
	}
	confirmation, err := cp.SendRequest(request)
	if err != nil {
		return nil, err
	}
	return confirmation.(*securefirmware.SignedFirmwareStatusNotificationResponse), err
}

func (cp *chargePoint) LogStatusNotification(status logging.UploadLogStatus, requestId int, props ...func(request *logging.LogStatusNotificationRequest)) (*logging.LogStatusNotificationResponse, error) {
	request := logging.NewLogStatusNotificationRequest(status, requestId)
	for _, fn := range props {
		fn(request)
	}
	confirmation, err := cp.SendRequest(request)
	if err != nil {
		return nil, err
	}
	return confirmation.(*logging.LogStatusNotificationResponse), err
}

func (cp *chargePoint) SetCoreHandler(handler core.ChargePointHandler) {
	cp.coreHandler = handler
}

func (cp *chargePoint) SetLocalAuthListHandler(handler localauth.ChargePointHandler) {
	cp.localAuthListHandler = handler
}

func (cp *chargePoint) SetFirmwareManagementHandler(handler firmware.ChargePointHandler) {
	cp.firmwareHandler = handler
}

func (cp *chargePoint) SetReservationHandler(handler reservation.ChargePointHandler) {
	cp.reservationHandler = handler
}

func (cp *chargePoint) SetRemoteTriggerHandler(handler remotetrigger.ChargePointHandler) {
	cp.remoteTriggerHandler = handler
}

func (cp *chargePoint) SetSmartChargingHandler(handler smartcharging.ChargePointHandler) {
	cp.smartChargingHandler = handler
}

func (cp *chargePoint) SetSecurityHandler(handler security.ChargePointHandler) {
	cp.securityHandler = handler
}

func (cp *chargePoint) SetLogHandler(handler logging.ChargePointHandler) {
	cp.logHandler = handler
}

func (cp *chargePoint) SetExtendedTriggerMessageHandler(handler extendedtriggermessage.ChargePointHandler) {
	cp.extendedTriggerMessageHandler = handler
}

func (cp *chargePoint) SetSecureFirmwareHandler(handler securefirmware.ChargePointHandler) {
	cp.secureFirmwareHandler = handler
}

func (cp *chargePoint) SetCertificateHandler(handler certificates.ChargePointHandler) {
	cp.certificateHandler = handler
}

// SetOnHandlerPanic registers a callback invoked when a user-provided OCPP-J
// handler panics. The recovered panic is also reported on Errors() as an
// *ocppj.HandlerPanicError, best-effort: that send is non-blocking on a cap-1
// channel, so a report is dropped when Errors() has never been called, when
// the buffer is full, or when the consumer has stopped draining. To observe
// every panic this charge point or the default dispatcher recovers — including
// ones racing shutdown — use this callback; Errors() is a monitoring stream,
// not an inventory. Set it before Start.
//
// The chain order is observable: on a panic this charge point attempts the
// Errors() report FIRST, then invokes any hook that was registered on the
// underlying *ocppj.Client BEFORE construction, then this callback. All three
// run inside one recover, so a panicking ENDPOINT hook suppresses this callback
// (and a panicking callback here suppresses nothing). No hook can suppress the
// Errors() report, which is attempted before either of them runs - it remains
// best-effort for the buffer reasons above, never because a hook panicked.
//
// A panic recovered inside the dispatcher (a raw CanceledRequestHandler, kind
// ocppj.CancelHandlerKind) reaches this callback only on a
// *ocppj.DefaultClientDispatcher: a custom ClientDispatcher never receives
// this hook, so panic reporting is unsupported with one. Panics recovered by
// this charge point itself are unaffected by the dispatcher in use.
func (cp *chargePoint) SetOnHandlerPanic(handler func(ocppj.HandlerPanic)) {
	cp.onHandlerPanic = handler
}

// SetOnDisconnectedHandler registers a callback invoked when the charge point
// loses its connection to the central system unexpectedly (not on a graceful
// Stop). The callback runs on the client's connection goroutine and blocks the
// automatic reconnect from starting until it returns, so keep it fast; hand off
// slow work to a goroutine. Set it before Start.
func (cp *chargePoint) SetOnDisconnectedHandler(handler func(err error)) {
	cp.client.SetOnDisconnectedHandler(handler)
}

// SetOnReconnectedHandler registers a callback invoked after the charge point
// has automatically re-established a dropped connection. The callback runs while
// the message dispatcher is still paused, so it MUST NOT perform a synchronous
// facade send (BootNotification, SendRequest, and similar): those block until
// the dispatcher resumes, which only happens after this callback returns; a
// deadlock. To re-run post-connect logic, dispatch it to a goroutine or use
// SendRequestAsync. Set it before Start.
func (cp *chargePoint) SetOnReconnectedHandler(handler func()) {
	cp.client.SetOnReconnectedHandler(handler)
}

func (cp *chargePoint) SendRequest(request ocpp.Request) (ocpp.Response, error) {
	return cp.SendRequestCtx(context.Background(), request)
}

// SendRequestCtx sends a synchronous OCPP request carrying a per-request
// context for cancellation and deadline propagation. A nil ctx is treated as
// context.Background(). The ctx-first parameter order follows Go convention
// and deliberately diverges from the upstream #105 proposal.
func (cp *chargePoint) SendRequestCtx(ctx context.Context, request ocpp.Request) (ocpp.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	featureName := request.GetFeatureName()
	if _, found := cp.client.GetProfileForFeature(featureName); !found {
		return nil, fmt.Errorf("feature %v is unsupported on charge point (missing profile), cannot send request", featureName)
	}

	// Create channel and pass it to a callback function, for retrieving asynchronous response
	asyncResponseC := make(chan asyncResponse, 1)
	send := func() (string, error) {
		return cp.client.SendRequestCtx(ctx, request)
	}
	err := cp.callbacks.TryQueue("main", send, func(confirmation ocpp.Response, err error) {
		asyncResponseC <- asyncResponse{r: confirmation, e: err}
	})
	if err != nil {
		return nil, err
	}
	return cp.awaitCtxResult(ctx, featureName, asyncResponseC, cp.loadStopC())
}

// awaitCtxResult is the prefer-response-fast-path helper: a non-blocking
// pre-check returns an already-delivered response even if ctx is canceled,
// then a blocking select races response against stop and ctx.Done().
// featureName only annotates the internal/stop error strings (kept identical
// to the pre-E1c messages); it does not affect control flow.
func (cp *chargePoint) awaitCtxResult(ctx context.Context, featureName string, asyncResponseC <-chan asyncResponse, stopC <-chan struct{}) (ocpp.Response, error) {
	// Prefer a ready response (non-blocking pre-check).
	select {
	case ar, ok := <-asyncResponseC:
		if !ok {
			return nil, fmt.Errorf("internal error while receiving result for %v request", featureName)
		}
		return ar.r, ar.e
	default:
	}

	select {
	case ar, ok := <-asyncResponseC:
		if !ok {
			return nil, fmt.Errorf("internal error while receiving result for %v request", featureName)
		}
		return ar.r, ar.e
	case <-stopC:
		// L3 residual guard (spec §4): Go's select has no priority among
		// simultaneously-ready cases, so if the callback delivers into
		// asyncResponseC in the same instant stopC is closed, this arm can
		// "win" the tie even though a real response is already sitting in
		// the channel one line away. Re-check it, non-blocking, before
		// conceding to the stopped error - if it is genuinely empty, stop
		// legitimately won and the error below is correct.
		select {
		case ar, ok := <-asyncResponseC:
			if !ok {
				return nil, fmt.Errorf("internal error while receiving result for %v request", featureName)
			}
			return ar.r, ar.e
		default:
		}
		return nil, fmt.Errorf("client stopped while waiting for response to %v", featureName)
	case <-ctx.Done():
		// Deliberately NO re-check here - asymmetric with the stopC arm
		// above, and intentionally so. The caller explicitly asked to cancel
		// via ctx, so honoring that by returning ctx.Err() is standard Go
		// practice and is pre-existing E1c behavior. stopC differs because
		// nobody asked for THAT outcome - it is an internal shutdown signal
		// racing a response that may have already arrived, which is exactly
		// what the re-check above exists to catch.
		return nil, ocppj.NewRequestCanceledError("", ctx.Err())
	}
}

func (cp *chargePoint) SendRequestAsync(request ocpp.Request, callback func(confirmation ocpp.Response, err error)) error {
	return cp.SendRequestAsyncCtx(context.Background(), request, callback)
}

// SendRequestAsyncCtx sends an asynchronous OCPP request carrying a per-request
// context for cancellation. A nil ctx is treated as context.Background().
func (cp *chargePoint) SendRequestAsyncCtx(ctx context.Context, request ocpp.Request, callback func(confirmation ocpp.Response, err error)) error {
	if ctx == nil {
		ctx = context.Background()
	}
	featureName := request.GetFeatureName()
	if _, found := cp.client.GetProfileForFeature(featureName); !found {
		return fmt.Errorf("feature %v is unsupported on charge point (missing profile), cannot send request", featureName)
	}
	switch featureName {
	case core.AuthorizeFeatureName, core.BootNotificationFeatureName, core.DataTransferFeatureName, core.HeartbeatFeatureName, core.MeterValuesFeatureName, core.StartTransactionFeatureName, core.StopTransactionFeatureName, core.StatusNotificationFeatureName,
		firmware.DiagnosticsStatusNotificationFeatureName, firmware.FirmwareStatusNotificationFeatureName,
		logging.LogStatusNotificationFeatureName,
		securefirmware.SignedFirmwareStatusNotificationFeatureName,
		security.SecurityEventNotificationFeatureName, security.SignCertificateFeatureName:
		break
	default:
		return fmt.Errorf("unsupported action %v on charge point, cannot send request", featureName)
	}
	// Response will be retrieved asynchronously via asyncHandler
	send := func() (string, error) {
		return cp.client.SendRequestCtx(ctx, request)
	}
	err := cp.callbacks.TryQueue("main", send, callback)
	return err
}

// asyncCallbackHandler drains cp.incoming for exactly one generation, exiting
// when stopC closes. stopC and handlerDone are received as PARAMETERS - the
// exact local variables Start just created - and never re-derived via
// loadStopC()/loadHandlerDone() inside this function (spec §3, "real
// parameter-passing", mirroring 2.0.1's asyncCallbackHandler(stopC)).
//
// PR-L2 narrowed the old hazard to a one-time capture ("stopC :=
// cp.loadStopC()" right before the loop) but did not close it: a goroutine
// delayed past a full Stop+Start would still make that FIRST read observe
// generation-2's channel, letting two handlers briefly co-drain cp.incoming
// and letting generation-1's clearCallbacks() drain generation-2's freshly
// registered callbacks. Passing the parameters pins this goroutine to the
// ONE generation it was spawned for, for its entire lifetime, regardless of
// scheduling delay - closing the window completely. This is also why Start
// MUST pass its own local stopC/handlerDone (see Start): spawning via
// `go cp.asyncCallbackHandler(cp.loadStopC(), cp.loadHandlerDone())` would
// silently reopen the exact race this parameter-passing exists to close, by
// re-deriving from the (possibly already-reassigned-by-a-later-Start)
// accessors instead of the generation this goroutine belongs to.
//
// asyncCallbackHandler is the only consumer of cp.incoming. Keeping one FIFO
// drain preserves wire-order dispatch; do not add another consumer or split
// this channel.
//
// Handlers run here, not on the read goroutine. If a handler calls
// SendRequestAsync while a dispatcher request-timeout is being delivered, the
// callback queue lock, dispatcher's cap-1 request channel, and this sole
// cp.incoming consumer can form a lock cycle. This pre-existing response
// callback caveat also applies to inbound request handlers.
//
// A blocking cp.error() wedges all response, error, and request handling in
// this loop, for example when Errors() is obtained but never drained -
// error() is itself preemptible against stopC now (see error()), so this
// can no longer wedge past Stop()/StopCtx().
func (cp *chargePoint) asyncCallbackHandler(stopC chan struct{}, handlerDone chan struct{}) {
	// Closed on every exit path (today there is only one, the <-stopC arm
	// below) so Stop/StopCtx's join - and Start's implicit
	// retirePreviousGeneration join - can observe that this generation has
	// fully exited. See joinHandler.
	defer close(handlerDone)
	for {
		select {
		case incoming := <-cp.incoming:
			switch incoming.kind {
			case incomingResponse:
				// Get and invoke callback
				if callback, ok := cp.callbacks.Dequeue("main", incoming.requestID); ok {
					func() {
						defer cp.client.RecoverPanicGoroutine(ocppj.ResponseHandlerKind, incoming.confirmation.GetFeatureName(), "", false)
						callback(incoming.confirmation, nil)
					}()
				} else {
					err := fmt.Errorf("no handler available for incoming response %v", incoming.confirmation.GetFeatureName())
					cp.error(err)
				}
			case incomingError:
				// Get and invoke callback by exact request ID
				requestID := ""
				if ocppError, ok := incoming.err.(*ocpp.Error); ok {
					requestID = ocppError.MessageId
				}
				if requestID == "" {
					cp.error(fmt.Errorf("cannot route error with no message id: %v", incoming.err))
				} else if callback, ok := cp.callbacks.Dequeue("main", requestID); ok {
					func() {
						defer cp.client.RecoverPanicGoroutine(ocppj.ErrorHandlerKind, "", requestID, false)
						callback(nil, incoming.err)
					}()
				} else {
					err := fmt.Errorf("no handler available for error %v", incoming.err.Error())
					cp.error(err)
				}
			case incomingRequest:
				func() {
					defer cp.client.RecoverPanicGoroutine(ocppj.RequestHandlerKind, incoming.action, incoming.requestID, true)
					cp.handleIncomingRequest(incoming.request, incoming.requestID, incoming.action)
				}()
			}
		case <-stopC:
			// Handler stopped, cleanup callbacks.
			// No callback invocation, since the user manually stopped the client.
			// A buffered inbound CALL may be dropped without a CALLERROR.
			cp.clearCallbacks()
			return
		}
	}
}

// clearCallbacks discards every pending callback on stop (they are not invoked;
// DrainAll's non-FIFO order is irrelevant since nothing is called).
func (cp *chargePoint) clearCallbacks() {
	cp.callbacks.DrainAll("main")
}

func (cp *chargePoint) sendResponse(confirmation ocpp.Response, err error, requestId string) {
	if err != nil {
		// Send error response
		if ocppError, ok := err.(*ocpp.Error); ok {
			err = cp.client.SendError(requestId, ocppError.Code, ocppError.Description, nil)
		} else {
			err = cp.client.SendError(requestId, ocppj.InternalError, err.Error(), nil)
		}
		if err != nil {
			// Error while sending an error. Will attempt to send a default error instead
			cp.client.HandleFailedResponseError(requestId, err, "")
			// Notify client implementation
			err = fmt.Errorf("replying to request %s with 'internal error' failed: %w", requestId, err)
			cp.error(err)
		}
		return
	}

	if confirmation == nil || reflect.ValueOf(confirmation).IsNil() {
		err = fmt.Errorf("empty confirmation to request %s", requestId)
		// Sending a dummy error to server instead, then notify client implementation
		_ = cp.client.SendError(requestId, ocppj.GenericError, err.Error(), nil)
		cp.error(err)
		return
	}

	// send confirmation response
	err = cp.client.SendResponse(requestId, confirmation)
	if err != nil {
		// Error while sending an error. Will attempt to send a default error instead
		cp.client.HandleFailedResponseError(requestId, err, confirmation.GetFeatureName())
		// Notify client implementation
		err = fmt.Errorf("failed responding to request %s: %w", requestId, err)
		cp.error(err)
	}
}

// closeStopC closes the CURRENT generation's stopC exactly once (stopOnce-
// guarded, mirroring the pre-PR-L1 inline logic this replaces). A no-op if
// Start has never been called (stopOnce nil) or has already fired for this
// generation. Shared by the explicit Stop/StopCtx path and by Start's
// implicit retirement of the PREVIOUS generation (retirePreviousGeneration).
func (cp *chargePoint) closeStopC() {
	if cp.stopOnce == nil {
		return
	}
	stopC := cp.loadStopC()
	cp.stopOnce.Do(func() {
		if stopC != nil {
			close(stopC)
		}
	})
}

// joinHandler blocks until the CURRENT generation's handlerDone closes (i.e.
// asyncCallbackHandler has exited), bounded by ctx. handlerDone is
// pre-closed in NewChargePoint and is left at its last-closed value whenever
// no handler is spawned (a failed Start - see Start), so this returns
// immediately (nil) whenever there is no live handler to wait for. That
// matters concretely: Stop()-before-Start() and Stop()-after-failed-Start()
// both rely on handlerDone being non-nil here - `select { case
// <-handlerDone: case <-ctx.Done(): }` with a NIL handlerDone selects on two
// nil channels and hangs forever, which is exactly the hang this function
// (and the constructor's pre-closed initialization) exists to prevent.
func (cp *chargePoint) joinHandler(ctx context.Context) error {
	select {
	case <-cp.loadHandlerDone():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// retirePreviousGeneration closes and joins the CURRENT generation before a
// new one is installed - the generation-orphan remedy spec §2 decides on for
// ALL THREE Start* paths (this one, and ocpp2.0.1's Start/StartWithRetries).
// Without it, a second Start* leaves the prior generation's handler parked
// forever on a stopC no future Stop() can reach (Stop() only ever closes
// loadStopC()'s CURRENT value via the accessor), and lets that orphaned
// handler's clearCallbacks() drain callbacks the new generation just
// registered - cp.callbacks is shared across generations.
//
// Reuses StopCtx verbatim (spec §2: "reuse the §2 join for the implicit
// stop... the guarantee is the same one Stop() gives") rather than only
// closing stopC and joining the handler. Calling cp.client.Stop() here is
// NOT optional: cp.client is one long-lived *ocppj.Client shared across
// every generation, and its dispatcher cannot safely have two overlapping
// Start()/messagePump lifetimes - ocppj/dispatcher.go's Start() reassigns
// d.timer (and d.requestChannel/d.doneC) unguarded against a still-running
// PREVIOUS messagePump goroutine reading them, which a plain facade-level
// retirement (close+join only, no client.Stop()) leaves running whenever the
// previous generation's handler was pinned. Confirmed empirically: an
// earlier version of this function omitted client.Stop() and
// go test -race caught the exact d.timer read/write race on
// TestL1GenerationOrphanSecondStartJoinsPreviousGeneration. Calling
// StopCtx's full sequence (close stopC, client.Stop(), join) guarantees the
// previous generation's dispatcher has FULLY exited (client.Stop() blocks on
// dispatcher.Stop()'s <-done) before Start proceeds to call client.Start()
// again below.
//
// Guarded on stopOnce == nil (Start has never been called): with no
// previous generation there is nothing to retire, and - unlike closeStopC
// and joinHandler, which both degrade to safe no-ops on a fresh facade -
// StopCtx unconditionally calls cp.client.Stop() too, which is NOT a no-op
// against a real (or mocked) network client. Skipping the whole retirement
// here, rather than relying on StopCtx's inner no-ops, avoids an extra,
// surprising client.Stop()/IsConnected() call on every very first Start() -
// which most callers (and most of this repo's existing Start()-only tests,
// which do not mock Stop()/IsConnected() at all) do not expect.
func (cp *chargePoint) retirePreviousGeneration() {
	if cp.stopOnce == nil {
		return
	}
	_ = cp.StopCtx(context.Background())
}

func (cp *chargePoint) Start(centralSystemUrl string) error {
	// Generation-orphan remedy (spec §2): retire the PREVIOUS generation, if
	// any, before installing a new one. See retirePreviousGeneration.
	cp.retirePreviousGeneration()

	// stopC/handlerDone are close-only (never carry a value), so both are
	// unbuffered. Created here as LOCAL variables and passed directly to the
	// spawned handler below (spec §3's real parameter-passing) - never
	// re-derived via the accessors, which is what would reopen the
	// delayed-spawn rebind window (see asyncCallbackHandler's doc comment).
	stopC := make(chan struct{})
	handlerDone := make(chan struct{})
	cp.storeStopC(stopC)
	cp.stopOnce = &sync.Once{}
	err := cp.client.Start(centralSystemUrl)
	// Async response handler receives incoming responses/errors and triggers
	// callbacks - wired up, and handlerDone replaced, ONLY on success. On
	// failure there is no handler to join, so handlerDone must be left at
	// its previous (already-closed) value; unconditionally storing a fresh,
	// OPEN handlerDone here would make a subsequent Stop()/StopCtx() join
	// forever on a handler that was never spawned (see
	// TestL1StopAfterFailedStartReturns).
	if err == nil {
		cp.storeHandlerDone(handlerDone)
		go cp.asyncCallbackHandler(stopC, handlerDone)
	}
	return err
}

// Stop is StopCtx bounded by context.Background(): it always waits for the
// in-flight handler generation to fully exit, however long that takes. See
// the ChargePoint interface's Stop/StopCtx godocs (v16.go) for the hazards
// this join introduces.
func (cp *chargePoint) Stop() {
	_ = cp.StopCtx(context.Background())
}

// StopCtx is the context-bounded variant of Stop. See the ChargePoint
// interface godoc (v16.go) for the full consumer-facing contract.
func (cp *chargePoint) StopCtx(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// Close stopC BEFORE client.Stop() (via closeStopC). client.Stop()
	// blocks inside dispatcher.Stop() until the messagePump goroutine
	// drains, and every producer that can wedge that pump (onRequestTimeout,
	// the forwarding closures wired in NewChargePoint, error()) is
	// preemptible against stopC. Closing it AFTER client.Stop() would make
	// that preemption dead code: the pump would already be wedged, waiting
	// on a signal that has not fired yet. See spec §Sequencing, "Stop()'s
	// order must change."
	//
	// closeStopC is stopOnce/nil-guarded: Stop() before Start() (stopOnce is
	// nil - nothing to close) and a repeated Stop()/StopCtx() (stopOnce
	// already fired) must not panic - mirrors 2.0.1's existing parity guard.
	cp.closeStopC()
	cp.client.Stop()

	// The join (spec §2): wait for the in-flight handler generation to fully
	// exit, bounded by ctx. This is the ONLY part of Stop/StopCtx that can
	// legitimately run long - client.Stop() above is already fast regardless
	// of the handler's state (every producer it drains through is
	// preemptible against the now-closed stopC); the join instead waits on
	// USER code (an in-flight SendRequestAsync callback, or an inbound
	// request handler) that may run arbitrarily long. On ctx expiry this
	// returns ctx.Err() without waiting further - stopC is already closed
	// regardless, so a RETRY StopCtx (even with a fresh context) is the
	// supported way to reach a clean stop afterwards: the handler will have
	// exited (or will shortly, once its in-flight callback returns) and the
	// retry's join succeeds immediately.
	//
	// cp.errC is deliberately NEVER closed here, or anywhere (spec §1, user
	// decision - supersedes an earlier "close after a successful join" plan
	// that turned out to be unsafe even with the join in place: error()
	// guards on errC == nil, not closedness, so a LATER generation's
	// error() would send on a closed channel and panic on an internally-
	// spawned goroutine the consumer cannot recover; and a RETRY StopCtx
	// joins a handlerDone that is already closed (closed stays closed), so
	// it would double-close errC too - stopOnce cannot guard that, since it
	// fires before the join, not after). See the Errors() godoc (v16.go)
	// for the full, permanent contract.
	return cp.joinHandler(ctx)
}

func (cp *chargePoint) IsConnected() bool {
	return cp.client.IsConnected()
}

func (cp *chargePoint) notImplementedError(requestId string, action string) {
	err := cp.client.SendError(requestId, ocppj.NotImplemented, fmt.Sprintf("no handler for action %v implemented", action), nil)
	if err != nil {
		err = fmt.Errorf("replying cs to request %s with 'not implemented': %w", requestId, err)
		cp.error(err)
	}
}

func (cp *chargePoint) notSupportedError(requestId string, action string) {
	err := cp.client.SendError(requestId, ocppj.NotSupported, fmt.Sprintf("unsupported action %v on charge point", action), nil)
	if err != nil {
		err = fmt.Errorf("replying cs to request %s with 'not supported': %w", requestId, err)
		cp.error(err)
	}
}

func (cp *chargePoint) handleIncomingRequest(request ocpp.Request, requestId string, action string) {
	profile, found := cp.client.GetProfileForFeature(action)
	// Check whether action is supported and a handler for it exists
	if !found {
		cp.notImplementedError(requestId, action)
		return
	} else {
		switch profile.Name {
		case core.ProfileName:
			if cp.coreHandler == nil {
				cp.notSupportedError(requestId, action)
				return
			}
		case localauth.ProfileName:
			if cp.localAuthListHandler == nil {
				cp.notSupportedError(requestId, action)
				return
			}
		case firmware.ProfileName:
			if cp.firmwareHandler == nil {
				cp.notSupportedError(requestId, action)
				return
			}
		case reservation.ProfileName:
			if cp.reservationHandler == nil {
				cp.notSupportedError(requestId, action)
				return
			}
		case remotetrigger.ProfileName:
			if cp.remoteTriggerHandler == nil {
				cp.notSupportedError(requestId, action)
				return
			}
		case smartcharging.ProfileName:
			if cp.smartChargingHandler == nil {
				cp.notSupportedError(requestId, action)
				return
			}
		case security.ProfileName:
			if cp.securityHandler == nil {
				cp.notSupportedError(requestId, action)
				return
			}
		case logging.ProfileName:
			if cp.logHandler == nil {
				cp.notSupportedError(requestId, action)
				return
			}
		case extendedtriggermessage.ProfileName:
			if cp.extendedTriggerMessageHandler == nil {
				cp.notSupportedError(requestId, action)
				return
			}
		case securefirmware.ProfileName:
			if cp.secureFirmwareHandler == nil {
				cp.notSupportedError(requestId, action)
				return
			}
		case certificates.ProfileName:
			if cp.certificateHandler == nil {
				cp.notSupportedError(requestId, action)
				return
			}
		}
	}

	// Process request
	var confirmation ocpp.Response
	var err error
	switch action {
	case core.ChangeAvailabilityFeatureName:
		confirmation, err = cp.coreHandler.OnChangeAvailability(request.(*core.ChangeAvailabilityRequest))
	case core.ChangeConfigurationFeatureName:
		confirmation, err = cp.coreHandler.OnChangeConfiguration(request.(*core.ChangeConfigurationRequest))
	case core.ClearCacheFeatureName:
		confirmation, err = cp.coreHandler.OnClearCache(request.(*core.ClearCacheRequest))
	case core.DataTransferFeatureName:
		confirmation, err = cp.coreHandler.OnDataTransfer(request.(*core.DataTransferRequest))
	case core.GetConfigurationFeatureName:
		confirmation, err = cp.coreHandler.OnGetConfiguration(request.(*core.GetConfigurationRequest))
	case core.RemoteStartTransactionFeatureName:
		confirmation, err = cp.coreHandler.OnRemoteStartTransaction(request.(*core.RemoteStartTransactionRequest))
	case core.RemoteStopTransactionFeatureName:
		confirmation, err = cp.coreHandler.OnRemoteStopTransaction(request.(*core.RemoteStopTransactionRequest))
	case core.ResetFeatureName:
		confirmation, err = cp.coreHandler.OnReset(request.(*core.ResetRequest))
	case core.UnlockConnectorFeatureName:
		confirmation, err = cp.coreHandler.OnUnlockConnector(request.(*core.UnlockConnectorRequest))
	case localauth.GetLocalListVersionFeatureName:
		confirmation, err = cp.localAuthListHandler.OnGetLocalListVersion(request.(*localauth.GetLocalListVersionRequest))
	case localauth.SendLocalListFeatureName:
		confirmation, err = cp.localAuthListHandler.OnSendLocalList(request.(*localauth.SendLocalListRequest))
	case firmware.GetDiagnosticsFeatureName:
		confirmation, err = cp.firmwareHandler.OnGetDiagnostics(request.(*firmware.GetDiagnosticsRequest))
	case firmware.UpdateFirmwareFeatureName:
		confirmation, err = cp.firmwareHandler.OnUpdateFirmware(request.(*firmware.UpdateFirmwareRequest))
	case reservation.ReserveNowFeatureName:
		confirmation, err = cp.reservationHandler.OnReserveNow(request.(*reservation.ReserveNowRequest))
	case reservation.CancelReservationFeatureName:
		confirmation, err = cp.reservationHandler.OnCancelReservation(request.(*reservation.CancelReservationRequest))
	case remotetrigger.TriggerMessageFeatureName:
		confirmation, err = cp.remoteTriggerHandler.OnTriggerMessage(request.(*remotetrigger.TriggerMessageRequest))
	case smartcharging.SetChargingProfileFeatureName:
		confirmation, err = cp.smartChargingHandler.OnSetChargingProfile(request.(*smartcharging.SetChargingProfileRequest))
	case smartcharging.ClearChargingProfileFeatureName:
		confirmation, err = cp.smartChargingHandler.OnClearChargingProfile(request.(*smartcharging.ClearChargingProfileRequest))
	case smartcharging.GetCompositeScheduleFeatureName:
		confirmation, err = cp.smartChargingHandler.OnGetCompositeSchedule(request.(*smartcharging.GetCompositeScheduleRequest))
	case security.CertificateSignedFeatureName:
		confirmation, err = cp.securityHandler.OnCertificateSigned(request.(*security.CertificateSignedRequest))
	case logging.GetLogFeatureName:
		confirmation, err = cp.logHandler.OnGetLog(request.(*logging.GetLogRequest))
	case securefirmware.SignedUpdateFirmwareFeatureName:
		confirmation, err = cp.secureFirmwareHandler.OnSignedUpdateFirmware(request.(*securefirmware.SignedUpdateFirmwareRequest))
	case certificates.GetInstalledCertificateIdsFeatureName:
		confirmation, err = cp.certificateHandler.OnGetInstalledCertificateIds(request.(*certificates.GetInstalledCertificateIdsRequest))
	case certificates.DeleteCertificateFeatureName:
		confirmation, err = cp.certificateHandler.OnDeleteCertificate(request.(*certificates.DeleteCertificateRequest))
	case certificates.InstallCertificateFeatureName:
		confirmation, err = cp.certificateHandler.OnInstallCertificate(request.(*certificates.InstallCertificateRequest))
	case extendedtriggermessage.ExtendedTriggerMessageFeatureName:
		confirmation, err = cp.extendedTriggerMessageHandler.OnExtendedTriggerMessage(request.(*extendedtriggermessage.ExtendedTriggerMessageRequest))
	default:
		cp.notSupportedError(requestId, action)
		return
	}
	cp.sendResponse(confirmation, err, requestId)
}
