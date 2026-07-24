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
	incoming                      chan incomingMessage
	callbacks                     callbackqueue.CallbackQueue
	stopC                         atomic.Value // holds chan struct{}; see loadStopC/storeStopC
	stopOnce                      *sync.Once
	errC                          chan error // external error channel
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

func (cp *chargePoint) error(err error) {
	if cp.errC == nil {
		return
	}
	// Preemptible: error() runs on whichever goroutine reports it (the async
	// handler, or - via sendResponse et al - request-handling code on that
	// same goroutine). A caller that obtains Errors() and never drains it
	// would otherwise wedge that goroutine forever inside `cp.errC <- err`,
	// which is exactly what would block the PR-L1 generation join. See spec
	// §L2 PR-L2 item 2, "error() on both facades".
	select {
	case cp.errC <- err:
	case <-cp.loadStopC():
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

// Errors returns a channel for error messages. If it doesn't exist it es created.
func (cp *chargePoint) Errors() <-chan error {
	if cp.errC == nil {
		cp.errC = make(chan error, 1)
	}
	return cp.errC
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

func (cp *chargePoint) SetOnHandlerPanic(handler func(ocppj.HandlerPanic)) {
	cp.client.SetOnHandlerPanic(handler)
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
		return nil, fmt.Errorf("client stopped while waiting for response to %v", featureName)
	case <-ctx.Done():
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

func (cp *chargePoint) asyncCallbackHandler() {
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
	// can no longer wedge past Stop().
	//
	// stopC is captured ONCE here, right after Start() has stored it and
	// before the very first select - not re-read via loadStopC() on every
	// iteration. This is deliberate: a re-read on every loop turn would let a
	// generation-1 handler rebind to a generation-2 Start's fresh channel
	// mid-loop (the exact hazard PR-L1's parameter-passing exists to close);
	// capturing once pins this goroutine to whichever generation is current at
	// its FIRST execution — NOT necessarily the one it was spawned for. If the
	// scheduler delays this goroutine past a full Stop+Start, the load below
	// returns generation-2's channel and two handlers briefly co-drain
	// cp.incoming. That residual window is strictly narrower than the
	// per-iteration field read it replaces (which could rebind on ANY
	// iteration, and raced outright); closing it fully needs PR-L1's
	// parameter-passing plus the generation handshake.
	stopC := cp.loadStopC()
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

func (cp *chargePoint) Start(centralSystemUrl string) error {
	// Start client. stopC is close-only (never carries a value), so it is
	// unbuffered.
	cp.storeStopC(make(chan struct{}))
	cp.stopOnce = &sync.Once{}
	err := cp.client.Start(centralSystemUrl)
	// Async response handler receives incoming responses/errors and triggers callbacks
	if err == nil {
		go cp.asyncCallbackHandler()
	}
	return err
}

func (cp *chargePoint) Stop() {
	// Close stopC BEFORE client.Stop(). client.Stop() blocks inside
	// dispatcher.Stop() until the messagePump goroutine drains, and every
	// producer that can wedge that pump (onRequestTimeout, the forwarding
	// closures wired in NewChargePoint, error()) is now preemptible against
	// stopC. Closing it AFTER client.Stop() - today's order - makes that
	// preemption dead code: the pump would already be wedged, waiting on a
	// signal that has not fired yet. See spec §Sequencing, "Stop()'s order
	// must change."
	//
	// stopOnce/nil-guarded: Stop() before Start() (stopOnce is nil - nothing
	// to close) and a repeated Stop() (stopOnce already fired) must not
	// panic - mirrors 2.0.1's existing parity guard.
	if cp.stopOnce != nil {
		stopC := cp.loadStopC()
		cp.stopOnce.Do(func() {
			if stopC != nil {
				close(stopC)
			}
		})
	}
	cp.client.Stop()

	// PR-L2 interim: cp.errC is deliberately NEVER closed here (dropped -
	// see spec §PR-L2 item 5). Closing it here, now that error() is
	// preemptible, would race a handler goroutine parked in error()'s
	// `select { case cp.errC <- err: case <-stopC: }`: both arms could
	// become ready at once (closed stopC AND a concurrently-closed errC),
	// and Go's pseudo-random arm choice can pick the errC arm - a send on a
	// channel closing right now, which panics. A future change reinstates
	// the close, but only after a successful generation join proves the
	// handler goroutine is no longer running (see PR-L1 item 4 in the
	// spec); until then, see the amended Errors() docstring in v16.go.
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
