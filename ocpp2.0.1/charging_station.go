package ocpp2

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"

	"github.com/enesismail/ocpp-go/internal/callbackqueue"
	"github.com/enesismail/ocpp-go/ocpp"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/authorization"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/availability"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/data"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/diagnostics"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/display"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/firmware"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/iso15118"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/localauth"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/meter"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/provisioning"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/remotecontrol"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/reservation"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/security"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/smartcharging"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/tariffcost"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/transactions"
	"github.com/enesismail/ocpp-go/ocpp2.0.1/types"
	"github.com/enesismail/ocpp-go/ocppj"
)

// incomingKind discriminates the three kinds of inbound event that flow
// through chargingStation.incoming - mirrors ocpp1.6/charge_point.go's
// incomingKind exactly (S3b, tasks/s3b-2.0.1-ordering-unification.md).
type incomingKind int

const (
	incomingResponse incomingKind = iota
	incomingError
	incomingRequest
)

// incomingMessage is the single envelope carried on chargingStation.incoming
// for all three inbound event kinds - mirrors ocpp1.6/charge_point.go's
// incomingMessage, except the response field is named "response" (not
// "confirmation") to match this package's existing naming.
type incomingMessage struct {
	kind      incomingKind
	response  ocpp.Response
	err       error
	request   ocpp.Request
	requestID string
	action    string
}

type chargingStation struct {
	client               *ocppj.Client
	securityHandler      security.ChargingStationHandler
	provisioningHandler  provisioning.ChargingStationHandler
	authorizationHandler authorization.ChargingStationHandler
	localAuthListHandler localauth.ChargingStationHandler
	transactionsHandler  transactions.ChargingStationHandler
	remoteControlHandler remotecontrol.ChargingStationHandler
	availabilityHandler  availability.ChargingStationHandler
	reservationHandler   reservation.ChargingStationHandler
	tariffCostHandler    tariffcost.ChargingStationHandler
	meterHandler         meter.ChargingStationHandler
	smartChargingHandler smartcharging.ChargingStationHandler
	firmwareHandler      firmware.ChargingStationHandler
	iso15118Handler      iso15118.ChargingStationHandler
	diagnosticsHandler   diagnostics.ChargingStationHandler
	displayHandler       display.ChargingStationHandler
	dataHandler          data.ChargingStationHandler
	// incoming carries all inbound events - responses, errors and requests -
	// on ONE ordered channel drained by the single asyncCallbackHandler, so an
	// inbound CALL cannot overtake a CALL_RESULT/CALL_ERROR that arrived
	// earlier on the wire (S3b; mirrors ocpp1.6's cp.incoming).
	//
	// Cap 1 deliberately, matching 1.6. Two honest consequences of the merge,
	// neither a new hazard class:
	//   - Cross-kind buffering drops from 2 slots to 1. The previous separate
	//     cap-1 responseHandler/errorHandler could hold a response AND an
	//     error at once; now a second inbound frame of any kind blocks the ws
	//     readPump until the drainer frees the slot. Two frames of the SAME
	//     kind already blocked it before, so this widens an existing stall
	//     rather than introducing one.
	//   - Producers contending for that one slot rise from 3 across two
	//     channels (responseHandler had 1: the response closure; errorHandler
	//     had 2: the error closure plus onRequestTimeout, which runs on the
	//     ocppj dispatcher's messagePump) to 4 on one channel - the same three
	//     readPump forwarding closures, now including requests, plus
	//     onRequestTimeout. So a slow user request handler occupying the
	//     drainer can now also delay outbound dispatch via a blocked
	//     onRequestTimeout, where before it only stalled the readPump.
	// Every send is preemptible against stopC, so none of this can outlive
	// Stop/StopCtx. Do NOT raise the capacity: the merged shutdown leak tests'
	// fill arithmetic depends on cap 1, and a larger buffer weakens leak
	// detection (cap N needs N+1 deliveries before one blocks).
	incoming  chan incomingMessage
	callbacks callbackqueue.CallbackQueue
	stopC     atomic.Value // holds chan struct{}; see loadStopC/storeStopC
	stopOnce  *sync.Once
	errC      chan error // external error channel
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
	// on its way out. Pre-closed in NewChargingStation - NEVER nil - and
	// replaced with a fresh, open channel by Start/StartWithRetries alongside
	// a freshly spawned handler (see Start, retirePreviousGeneration,
	// joinHandler).
	handlerDone chan struct{}
}

// asyncResponse wraps an asynchronous response for delivery via a channel
// from the callback to the sync-send select.
type asyncResponse struct {
	r ocpp.Response
	e error
}

// loadStopC returns the current generation's stop signal channel, or nil if
// Start/StartWithRetries has never been called. atomic.Value (not a plain
// field) because independent goroutine families read stopC - the ocppj
// dispatcher pump (onRequestTimeout), the ws readPump (the forwarding
// closures wired in NewChargingStation), and SendRequestCtx callers - while
// Start/StartWithRetries reassign it on every call; a plain field
// read/write pair here is a data race (see TestL2ShutdownRestartStopCRace
// under -race). This accessor is the ONLY synchronization around stopC: per
// the HOLD-SCOPE rule (tasks/facade-lifecycle-hardening.md §PR-L2 item 1),
// no lock is ever held across it, a channel op, client.Stop(), or a join -
// holding one would recreate the exact three-party deadlock this accessor
// exists to avoid. Note asyncCallbackHandler does NOT use this accessor: it
// receives stopC by parameter at spawn (deliberately - a generation-1
// handler must exit on its OWN generation's channel, never an accessor
// read that could rebind it to a later generation's).
func (cs *chargingStation) loadStopC() chan struct{} {
	v, _ := cs.stopC.Load().(chan struct{})
	return v
}

func (cs *chargingStation) storeStopC(c chan struct{}) {
	cs.stopC.Store(c)
}

// loadHandlerDone and storeHandlerDone are the mu-guarded accessors for
// handlerDone (see the mu field doc's HOLD-SCOPE RULE): the lock is held only
// long enough to copy/assign the field, never across the channel op the
// caller goes on to perform.
func (cs *chargingStation) loadHandlerDone() chan struct{} {
	cs.mu.Lock()
	hd := cs.handlerDone
	cs.mu.Unlock()
	return hd
}

func (cs *chargingStation) storeHandlerDone(hd chan struct{}) {
	cs.mu.Lock()
	cs.handlerDone = hd
	cs.mu.Unlock()
}

func (cs *chargingStation) error(err error) {
	// errC is read as a field copy under mu, then the lock is released
	// before the channel op below - this is what makes it safe against
	// Errors()' lazy creation (spec §5; see
	// TestL1ErrorsLazyCreationRaceSameChannel) without violating the
	// HOLD-SCOPE rule on the mu field doc.
	cs.mu.Lock()
	errC := cs.errC
	cs.mu.Unlock()
	if errC == nil {
		return
	}
	// Preemptible: error() runs on the async handler goroutine, or (via
	// sendResponse et al, invoked from the unjoined ws readPump through
	// handleIncomingRequest) directly on the readPump. A caller that obtains
	// Errors() and never drains it would otherwise wedge whichever goroutine
	// called error() forever inside `errC <- err`. See spec §L2 PR-L2
	// item 2, "error() on both facades".
	select {
	case errC <- err:
	case <-cs.loadStopC():
	}
}

// Errors returns a channel for error messages. If it doesn't exist it is
// created. See the ChargingStation interface godoc (v2.go) for the full
// contract - in short: errC is NEVER closed and is process-lifetime, not
// per-generation.
//
// errC's lazy creation is guarded by mu so two concurrent first-callers
// cannot each create a different channel and silently lose one (spec §5).
// The lock is held only for the field check/create/copy, never across a
// channel op (HOLD-SCOPE rule on the mu field doc).
func (cs *chargingStation) Errors() <-chan error {
	cs.mu.Lock()
	if cs.errC == nil {
		cs.errC = make(chan error, 1)
	}
	errC := cs.errC
	cs.mu.Unlock()
	return errC
}

// Callback invoked whenever a queued request is canceled, due to timeout.
// By default, the callback returns a GenericError to the caller, who sent the original request.
func (cs *chargingStation) onRequestTimeout(_ string, _ ocpp.Request, err *ocpp.Error) {
	// Preemptible: runs on the ocppj dispatcher's messagePump goroutine,
	// sequentially, for every request canceled at Stop()-time or on timeout.
	// A blocking send into cs.incoming (cap 1) with no reader wedges the
	// pump, which DefaultClientDispatcher.Stop() (called from client.Stop())
	// waits on unconditionally - hanging facade Stop() forever. See spec §L2.
	select {
	case cs.incoming <- incomingMessage{kind: incomingError, err: err}:
	case <-cs.loadStopC():
	}
}

func (cs *chargingStation) BootNotification(reason provisioning.BootReason, model string, vendor string, props ...func(request *provisioning.BootNotificationRequest)) (*provisioning.BootNotificationResponse, error) {
	request := provisioning.NewBootNotificationRequest(reason, model, vendor)
	for _, fn := range props {
		fn(request)
	}
	response, err := cs.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return response.(*provisioning.BootNotificationResponse), err
	}
}

func (cs *chargingStation) Authorize(idToken string, tokenType types.IdTokenType, props ...func(request *authorization.AuthorizeRequest)) (*authorization.AuthorizeResponse, error) {
	request := authorization.NewAuthorizationRequest(idToken, tokenType)
	for _, fn := range props {
		fn(request)
	}
	response, err := cs.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return response.(*authorization.AuthorizeResponse), err
	}
}

func (cs *chargingStation) ClearedChargingLimit(chargingLimitSource types.ChargingLimitSourceType, props ...func(request *smartcharging.ClearedChargingLimitRequest)) (*smartcharging.ClearedChargingLimitResponse, error) {
	request := smartcharging.NewClearedChargingLimitRequest(chargingLimitSource)
	for _, fn := range props {
		fn(request)
	}
	response, err := cs.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return response.(*smartcharging.ClearedChargingLimitResponse), err
	}
}

func (cs *chargingStation) DataTransfer(vendorId string, props ...func(request *data.DataTransferRequest)) (*data.DataTransferResponse, error) {
	request := data.NewDataTransferRequest(vendorId)
	for _, fn := range props {
		fn(request)
	}
	response, err := cs.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return response.(*data.DataTransferResponse), err
	}
}

func (cs *chargingStation) FirmwareStatusNotification(status firmware.FirmwareStatus, props ...func(request *firmware.FirmwareStatusNotificationRequest)) (*firmware.FirmwareStatusNotificationResponse, error) {
	request := firmware.NewFirmwareStatusNotificationRequest(status)
	for _, fn := range props {
		fn(request)
	}
	response, err := cs.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return response.(*firmware.FirmwareStatusNotificationResponse), err
	}
}

func (cs *chargingStation) Get15118EVCertificate(schemaVersion string, action iso15118.CertificateAction, exiRequest string, props ...func(request *iso15118.Get15118EVCertificateRequest)) (*iso15118.Get15118EVCertificateResponse, error) {
	request := iso15118.NewGet15118EVCertificateRequest(schemaVersion, action, exiRequest)
	for _, fn := range props {
		fn(request)
	}
	response, err := cs.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return response.(*iso15118.Get15118EVCertificateResponse), err
	}
}

func (cs *chargingStation) GetCertificateStatus(ocspRequestData types.OCSPRequestDataType, props ...func(request *iso15118.GetCertificateStatusRequest)) (*iso15118.GetCertificateStatusResponse, error) {
	request := iso15118.NewGetCertificateStatusRequest(ocspRequestData)
	for _, fn := range props {
		fn(request)
	}
	response, err := cs.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return response.(*iso15118.GetCertificateStatusResponse), err
	}
}

func (cs *chargingStation) Heartbeat(props ...func(request *availability.HeartbeatRequest)) (*availability.HeartbeatResponse, error) {
	request := availability.NewHeartbeatRequest()
	for _, fn := range props {
		fn(request)
	}
	response, err := cs.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return response.(*availability.HeartbeatResponse), err
	}
}

func (cs *chargingStation) LogStatusNotification(status diagnostics.UploadLogStatus, requestID int, props ...func(request *diagnostics.LogStatusNotificationRequest)) (*diagnostics.LogStatusNotificationResponse, error) {
	request := diagnostics.NewLogStatusNotificationRequest(status, requestID)
	for _, fn := range props {
		fn(request)
	}
	response, err := cs.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return response.(*diagnostics.LogStatusNotificationResponse), err
	}
}

func (cs *chargingStation) MeterValues(evseID int, meterValues []types.MeterValue, props ...func(request *meter.MeterValuesRequest)) (*meter.MeterValuesResponse, error) {
	request := meter.NewMeterValuesRequest(evseID, meterValues)
	for _, fn := range props {
		fn(request)
	}
	response, err := cs.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return response.(*meter.MeterValuesResponse), err
	}
}

func (cs *chargingStation) NotifyChargingLimit(chargingLimit smartcharging.ChargingLimit, props ...func(request *smartcharging.NotifyChargingLimitRequest)) (*smartcharging.NotifyChargingLimitResponse, error) {
	request := smartcharging.NewNotifyChargingLimitRequest(chargingLimit)
	for _, fn := range props {
		fn(request)
	}
	response, err := cs.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return response.(*smartcharging.NotifyChargingLimitResponse), err
	}
}

func (cs *chargingStation) NotifyCustomerInformation(data string, seqNo int, generatedAt types.DateTime, requestID int, props ...func(request *diagnostics.NotifyCustomerInformationRequest)) (*diagnostics.NotifyCustomerInformationResponse, error) {
	request := diagnostics.NewNotifyCustomerInformationRequest(data, seqNo, generatedAt, requestID)
	for _, fn := range props {
		fn(request)
	}
	response, err := cs.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return response.(*diagnostics.NotifyCustomerInformationResponse), err
	}
}

func (cs *chargingStation) NotifyDisplayMessages(requestID int, props ...func(request *display.NotifyDisplayMessagesRequest)) (*display.NotifyDisplayMessagesResponse, error) {
	request := display.NewNotifyDisplayMessagesRequest(requestID)
	for _, fn := range props {
		fn(request)
	}
	response, err := cs.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return response.(*display.NotifyDisplayMessagesResponse), err
	}
}

func (cs *chargingStation) NotifyEVChargingNeeds(evseID int, chargingNeeds smartcharging.ChargingNeeds, props ...func(request *smartcharging.NotifyEVChargingNeedsRequest)) (*smartcharging.NotifyEVChargingNeedsResponse, error) {
	request := smartcharging.NewNotifyEVChargingNeedsRequest(evseID, chargingNeeds)
	for _, fn := range props {
		fn(request)
	}
	response, err := cs.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return response.(*smartcharging.NotifyEVChargingNeedsResponse), err
	}
}

func (cs *chargingStation) NotifyEVChargingSchedule(timeBase *types.DateTime, evseID int, schedule types.ChargingSchedule, props ...func(request *smartcharging.NotifyEVChargingScheduleRequest)) (*smartcharging.NotifyEVChargingScheduleResponse, error) {
	request := smartcharging.NewNotifyEVChargingScheduleRequest(timeBase, evseID, schedule)
	for _, fn := range props {
		fn(request)
	}
	response, err := cs.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return response.(*smartcharging.NotifyEVChargingScheduleResponse), err
	}
}

func (cs *chargingStation) NotifyEvent(generatedAt *types.DateTime, seqNo int, eventData []diagnostics.EventData, props ...func(request *diagnostics.NotifyEventRequest)) (*diagnostics.NotifyEventResponse, error) {
	request := diagnostics.NewNotifyEventRequest(generatedAt, seqNo, eventData)
	for _, fn := range props {
		fn(request)
	}
	response, err := cs.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return response.(*diagnostics.NotifyEventResponse), err
	}
}

func (cs *chargingStation) NotifyMonitoringReport(requestID int, seqNo int, generatedAt *types.DateTime, monitorData []diagnostics.MonitoringData, props ...func(request *diagnostics.NotifyMonitoringReportRequest)) (*diagnostics.NotifyMonitoringReportResponse, error) {
	request := diagnostics.NewNotifyMonitoringReportRequest(requestID, seqNo, generatedAt, monitorData)
	for _, fn := range props {
		fn(request)
	}
	response, err := cs.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return response.(*diagnostics.NotifyMonitoringReportResponse), err
	}
}

func (cs *chargingStation) NotifyReport(requestID int, generatedAt *types.DateTime, seqNo int, props ...func(request *provisioning.NotifyReportRequest)) (*provisioning.NotifyReportResponse, error) {
	request := provisioning.NewNotifyReportRequest(requestID, generatedAt, seqNo)
	for _, fn := range props {
		fn(request)
	}
	response, err := cs.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return response.(*provisioning.NotifyReportResponse), err
	}
}

func (cs *chargingStation) PublishFirmwareStatusNotification(status firmware.PublishFirmwareStatus, props ...func(request *firmware.PublishFirmwareStatusNotificationRequest)) (*firmware.PublishFirmwareStatusNotificationResponse, error) {
	request := firmware.NewPublishFirmwareStatusNotificationRequest(status)
	for _, fn := range props {
		fn(request)
	}
	response, err := cs.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return response.(*firmware.PublishFirmwareStatusNotificationResponse), err
	}
}

func (cs *chargingStation) ReportChargingProfiles(requestID int, chargingLimitSource types.ChargingLimitSourceType, evseID int, chargingProfile []types.ChargingProfile, props ...func(request *smartcharging.ReportChargingProfilesRequest)) (*smartcharging.ReportChargingProfilesResponse, error) {
	request := smartcharging.NewReportChargingProfilesRequest(requestID, chargingLimitSource, evseID, chargingProfile)
	for _, fn := range props {
		fn(request)
	}
	response, err := cs.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return response.(*smartcharging.ReportChargingProfilesResponse), err
	}
}

func (cs *chargingStation) ReservationStatusUpdate(reservationID int, status reservation.ReservationUpdateStatus, props ...func(request *reservation.ReservationStatusUpdateRequest)) (*reservation.ReservationStatusUpdateResponse, error) {
	request := reservation.NewReservationStatusUpdateRequest(reservationID, status)
	for _, fn := range props {
		fn(request)
	}
	response, err := cs.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return response.(*reservation.ReservationStatusUpdateResponse), err
	}
}

func (cs *chargingStation) SecurityEventNotification(typ string, timestamp *types.DateTime, props ...func(request *security.SecurityEventNotificationRequest)) (*security.SecurityEventNotificationResponse, error) {
	request := security.NewSecurityEventNotificationRequest(typ, timestamp)
	for _, fn := range props {
		fn(request)
	}
	response, err := cs.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return response.(*security.SecurityEventNotificationResponse), err
	}
}

func (cs *chargingStation) SignCertificate(csr string, props ...func(request *security.SignCertificateRequest)) (*security.SignCertificateResponse, error) {
	request := security.NewSignCertificateRequest(csr)
	for _, fn := range props {
		fn(request)
	}
	response, err := cs.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return response.(*security.SignCertificateResponse), err
	}
}

func (cs *chargingStation) StatusNotification(timestamp *types.DateTime, status availability.ConnectorStatus, evseID int, connectorID int, props ...func(request *availability.StatusNotificationRequest)) (*availability.StatusNotificationResponse, error) {
	request := availability.NewStatusNotificationRequest(timestamp, status, evseID, connectorID)
	for _, fn := range props {
		fn(request)
	}
	response, err := cs.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return response.(*availability.StatusNotificationResponse), err
	}
}

func (cs *chargingStation) TransactionEvent(t transactions.TransactionEvent, timestamp *types.DateTime, reason transactions.TriggerReason, seqNo int, info transactions.Transaction, props ...func(request *transactions.TransactionEventRequest)) (*transactions.TransactionEventResponse, error) {
	request := transactions.NewTransactionEventRequest(t, timestamp, reason, seqNo, info)
	for _, fn := range props {
		fn(request)
	}
	response, err := cs.SendRequest(request)
	if err != nil {
		return nil, err
	} else {
		return response.(*transactions.TransactionEventResponse), err
	}
}

func (cs *chargingStation) SetSecurityHandler(handler security.ChargingStationHandler) {
	cs.securityHandler = handler
}

func (cs *chargingStation) SetProvisioningHandler(handler provisioning.ChargingStationHandler) {
	cs.provisioningHandler = handler
}

func (cs *chargingStation) SetAuthorizationHandler(handler authorization.ChargingStationHandler) {
	cs.authorizationHandler = handler
}

func (cs *chargingStation) SetLocalAuthListHandler(handler localauth.ChargingStationHandler) {
	cs.localAuthListHandler = handler
}

func (cs *chargingStation) SetTransactionsHandler(handler transactions.ChargingStationHandler) {
	cs.transactionsHandler = handler
}

func (cs *chargingStation) SetRemoteControlHandler(handler remotecontrol.ChargingStationHandler) {
	cs.remoteControlHandler = handler
}

func (cs *chargingStation) SetAvailabilityHandler(handler availability.ChargingStationHandler) {
	cs.availabilityHandler = handler
}

func (cs *chargingStation) SetReservationHandler(handler reservation.ChargingStationHandler) {
	cs.reservationHandler = handler
}

func (cs *chargingStation) SetTariffCostHandler(handler tariffcost.ChargingStationHandler) {
	cs.tariffCostHandler = handler
}

func (cs *chargingStation) SetMeterHandler(handler meter.ChargingStationHandler) {
	cs.meterHandler = handler
}

func (cs *chargingStation) SetSmartChargingHandler(handler smartcharging.ChargingStationHandler) {
	cs.smartChargingHandler = handler
}

func (cs *chargingStation) SetFirmwareHandler(handler firmware.ChargingStationHandler) {
	cs.firmwareHandler = handler
}

func (cs *chargingStation) SetISO15118Handler(handler iso15118.ChargingStationHandler) {
	cs.iso15118Handler = handler
}

func (cs *chargingStation) SetDiagnosticsHandler(handler diagnostics.ChargingStationHandler) {
	cs.diagnosticsHandler = handler
}

func (cs *chargingStation) SetDisplayHandler(handler display.ChargingStationHandler) {
	cs.displayHandler = handler
}

func (cs *chargingStation) SetDataHandler(handler data.ChargingStationHandler) {
	cs.dataHandler = handler
}

func (cs *chargingStation) SetOnHandlerPanic(handler func(ocppj.HandlerPanic)) {
	cs.client.SetOnHandlerPanic(handler)
}

// SetOnDisconnectedHandler registers a callback invoked when the charging
// station loses its connection to the CSMS unexpectedly (not on a graceful
// Stop). The callback runs on the client's connection goroutine and blocks the
// automatic reconnect from starting until it returns, so keep it fast; hand off
// slow work to a goroutine. Set it before Start/StartWithRetries.
func (cs *chargingStation) SetOnDisconnectedHandler(handler func(err error)) {
	cs.client.SetOnDisconnectedHandler(handler)
}

// SetOnReconnectedHandler registers a callback invoked after the charging
// station has (re)established a connection. It MUST NOT perform a synchronous
// facade send (BootNotification, SendRequest, and similar): on a post-drop
// reconnect the message dispatcher is still paused and the send deadlocks until
// this callback returns; after an initial StartWithRetries success the
// dispatcher is not yet started and the send fails ("client is not started").
// Either way, dispatch post-connect work to a goroutine or use SendRequestAsync.
// Set it before Start/StartWithRetries.
func (cs *chargingStation) SetOnReconnectedHandler(handler func()) {
	cs.client.SetOnReconnectedHandler(handler)
}

func (cs *chargingStation) SendRequest(request ocpp.Request) (ocpp.Response, error) {
	return cs.SendRequestCtx(context.Background(), request)
}

// SendRequestCtx sends a synchronous OCPP request carrying a per-request
// context for cancellation and deadline propagation. A nil ctx is treated as
// context.Background(). The ctx-first parameter order follows Go convention
// and deliberately diverges from the upstream #105 proposal.
func (cs *chargingStation) SendRequestCtx(ctx context.Context, request ocpp.Request) (ocpp.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	featureName := request.GetFeatureName()
	if _, found := cs.client.GetProfileForFeature(featureName); !found {
		return nil, fmt.Errorf("feature %v is unsupported on charging station (missing profile), cannot send request", featureName)
	}

	// Create channel and pass it to a callback function, for retrieving asynchronous response
	asyncResponseC := make(chan asyncResponse, 1)
	send := func() (string, error) {
		return cs.client.SendRequestCtx(ctx, request)
	}
	err := cs.callbacks.TryQueue("main", send, func(confirmation ocpp.Response, err error) {
		asyncResponseC <- asyncResponse{r: confirmation, e: err}
	})
	if err != nil {
		return nil, err
	}
	stopC := cs.loadStopC()
	return cs.awaitCtxResult(ctx, featureName, asyncResponseC, stopC)
}

// awaitCtxResult is the prefer-response-fast-path helper: a non-blocking
// pre-check returns an already-delivered response even if ctx is canceled,
// then a blocking select races response against stop and ctx.Done().
// featureName only annotates the internal/stop error strings (kept identical
// to the pre-E1c messages); it does not affect control flow.
func (cs *chargingStation) awaitCtxResult(ctx context.Context, featureName string, asyncResponseC <-chan asyncResponse, stopC <-chan struct{}) (ocpp.Response, error) {
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
		return nil, fmt.Errorf("charging station stopped while awaiting response to %v request", featureName)
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

func (cs *chargingStation) SendRequestAsync(request ocpp.Request, callback func(response ocpp.Response, err error)) error {
	return cs.SendRequestAsyncCtx(context.Background(), request, callback)
}

// SendRequestAsyncCtx sends an asynchronous OCPP request carrying a per-request
// context for cancellation. A nil ctx is treated as context.Background().
func (cs *chargingStation) SendRequestAsyncCtx(ctx context.Context, request ocpp.Request, callback func(response ocpp.Response, err error)) error {
	if ctx == nil {
		ctx = context.Background()
	}
	featureName := request.GetFeatureName()
	if _, found := cs.client.GetProfileForFeature(featureName); !found {
		return fmt.Errorf("feature %v is unsupported on charging station (missing profile), cannot send request", featureName)
	}
	switch featureName {
	case authorization.AuthorizeFeatureName,
		provisioning.BootNotificationFeatureName,
		smartcharging.ClearedChargingLimitFeatureName,
		data.DataTransferFeatureName,
		firmware.FirmwareStatusNotificationFeatureName,
		iso15118.Get15118EVCertificateFeatureName,
		iso15118.GetCertificateStatusFeatureName,
		availability.HeartbeatFeatureName,
		diagnostics.LogStatusNotificationFeatureName,
		meter.MeterValuesFeatureName,
		smartcharging.NotifyChargingLimitFeatureName,
		diagnostics.NotifyCustomerInformationFeatureName,
		display.NotifyDisplayMessagesFeatureName,
		smartcharging.NotifyEVChargingNeedsFeatureName,
		smartcharging.NotifyEVChargingScheduleFeatureName,
		diagnostics.NotifyEventFeatureName,
		diagnostics.NotifyMonitoringReportFeatureName,
		provisioning.NotifyReportFeatureName,
		firmware.PublishFirmwareStatusNotificationFeatureName,
		smartcharging.ReportChargingProfilesFeatureName,
		reservation.ReservationStatusUpdateFeatureName,
		security.SecurityEventNotificationFeatureName,
		security.SignCertificateFeatureName,
		availability.StatusNotificationFeatureName,
		transactions.TransactionEventFeatureName:
		break
	default:
		return fmt.Errorf("unsupported action %v on charging station, cannot send request", featureName)
	}
	// Response will be retrieved asynchronously via asyncHandler
	send := func() (string, error) {
		return cs.client.SendRequestCtx(ctx, request)
	}
	err := cs.callbacks.TryQueue("main", send, callback)
	return err
}

// asyncCallbackHandler drains cs.incoming for exactly one generation,
// exiting when stopC closes. stopC and handlerDone are received as
// PARAMETERS - the exact local variables Start/StartWithRetries just
// created - never re-derived via loadStopC()/loadHandlerDone() inside this
// function (spec §3's "real parameter-passing"; this facade already did this
// for stopC pre-PR-L1, handlerDone is the PR-L1 addition). Passing the
// parameters (rather than reading a shared field/accessor) pins this
// goroutine to the ONE generation it was spawned for, for its entire
// lifetime, regardless of scheduling delay - a delayed spawn can never rebind
// to a later generation's channels mid-loop, which is what would let two
// handlers briefly co-drain, or let generation-1's clearCallbacks() drain
// generation-2's freshly registered callbacks (cs.callbacks is shared across
// generations).
func (cs *chargingStation) asyncCallbackHandler(stopC chan struct{}, handlerDone chan struct{}) {
	// Closed on every exit path (today there is only one, the <-stopC arm
	// below) so Stop/StopCtx's join - and Start/StartWithRetries' implicit
	// retirePreviousGeneration join - can observe that this generation has
	// fully exited. See joinHandler.
	defer close(handlerDone)
	for {
		select {
		case incoming := <-cs.incoming:
			switch incoming.kind {
			case incomingResponse:
				// Get and invoke callback
				if callback, ok := cs.callbacks.Dequeue("main", incoming.requestID); ok {
					func() {
						defer cs.client.RecoverPanicGoroutine(ocppj.ResponseHandlerKind, incoming.response.GetFeatureName(), "", false)
						callback(incoming.response, nil)
					}()
				} else {
					cs.error(fmt.Errorf("no callback available for incoming response %v", incoming.response.GetFeatureName()))
				}
			case incomingError:
				// Get and invoke callback by exact request ID
				requestID := ""
				if ocppError, ok := incoming.err.(*ocpp.Error); ok {
					requestID = ocppError.MessageId
				}
				if requestID == "" {
					cs.error(fmt.Errorf("cannot route error with no message id: %v", incoming.err))
				} else if callback, ok := cs.callbacks.Dequeue("main", requestID); ok {
					func() {
						defer cs.client.RecoverPanicGoroutine(ocppj.ErrorHandlerKind, "", requestID, false)
						callback(nil, incoming.err)
					}()
				} else {
					cs.error(fmt.Errorf("no callback available for incoming error %w", incoming.err))
				}
			case incomingRequest:
				// Moving request handling off ocppj's own goroutine (see the
				// forwarding closure in v2.go) silently drops ocppj's own
				// panic-isolation guard around SetRequestHandler
				// (ocppj/client.go's recoverHandler covers only the trivial
				// forwarding closure now). RecoverPanicGoroutine here
				// reinstates the SAME guard - same kind, action, requestID,
				// onHandlerPanic sink, and sendCallError=true so the peer
				// still gets a CALLERROR instead of hanging. See spec's
				// "THE CRITICAL HAZARD" - omitting this reintroduces exactly
				// the crash class S6 was created to fix.
				func() {
					defer cs.client.RecoverPanicGoroutine(ocppj.RequestHandlerKind, incoming.action, incoming.requestID, true)
					cs.handleIncomingRequest(incoming.request, incoming.requestID, incoming.action)
				}()
			}
		case <-stopC:
			// Handler stopped, cleanup callbacks.
			// No callback invocation, since the user manually stopped the client.
			// A buffered inbound CALL may be dropped without a CALLERROR: the
			// peer that sent it simply times out waiting for a response.
			// Deliberate - see spec §"The change" item 3: draining-and-replying
			// at stop time would re-introduce a blocking send to a possibly-
			// unreachable peer inside Stop().
			cs.clearCallbacks()
			return
		}
	}
}

// clearCallbacks discards every pending callback on stop (they are not invoked;
// DrainAll's non-FIFO order is irrelevant since nothing is called).
func (cs *chargingStation) clearCallbacks() {
	cs.callbacks.DrainAll("main")
}

func (cs *chargingStation) sendResponse(response ocpp.Response, err error, requestId string) {
	if err != nil {
		// Send error response
		if ocppError, ok := err.(*ocpp.Error); ok {
			err = cs.client.SendError(requestId, ocppError.Code, ocppError.Description, nil)
		} else {
			err = cs.client.SendError(requestId, ocppj.InternalError, err.Error(), nil)
		}
		if err != nil {
			// Error while sending an error. Will attempt to send a default error instead
			cs.client.HandleFailedResponseError(requestId, err, "")
			// Notify client implementation
			err = fmt.Errorf("replying to request %s with 'internal error' failed: %w", requestId, err)
			cs.error(err)
		}
		return
	}

	if response == nil || reflect.ValueOf(response).IsNil() {
		err = fmt.Errorf("empty response to request %s", requestId)
		// Sending a dummy error to server instead, then notify client implementation
		_ = cs.client.SendError(requestId, ocppj.GenericError, err.Error(), nil)
		cs.error(err)
		return
	}

	// send confirmation response
	err = cs.client.SendResponse(requestId, response)
	if err != nil {
		// Error while sending an error. Will attempt to send a default error instead
		cs.client.HandleFailedResponseError(requestId, err, response.GetFeatureName())
		// Notify client implementation
		err = fmt.Errorf("failed responding to request %s: %w", requestId, err)
		cs.error(err)
	}
}

// closeStopC closes the CURRENT generation's stopC exactly once (stopOnce-
// guarded, mirroring the pre-PR-L1 inline logic this replaces). A no-op if
// Start/StartWithRetries has never been called (stopOnce nil) or has already
// fired for this generation. Shared by the explicit Stop/StopCtx path and by
// Start/StartWithRetries' implicit retirement of the PREVIOUS generation
// (retirePreviousGeneration).
func (cs *chargingStation) closeStopC() {
	if cs.stopOnce == nil {
		return
	}
	stopC := cs.loadStopC()
	cs.stopOnce.Do(func() {
		if stopC != nil {
			close(stopC)
		}
	})
}

// joinHandler blocks until the CURRENT generation's handlerDone closes (i.e.
// asyncCallbackHandler has exited), bounded by ctx. handlerDone is
// pre-closed in NewChargingStation and is left at its last-closed value
// whenever no handler is spawned (a failed Start - see Start), so this
// returns immediately (nil) whenever there is no live handler to wait for.
// That matters concretely: Stop()-before-Start() and
// Stop()-after-failed-Start() both rely on handlerDone being non-nil here -
// `select { case <-handlerDone: case <-ctx.Done(): }` with a NIL
// handlerDone selects on two nil channels and hangs forever, which is
// exactly the hang this function (and the constructor's pre-closed
// initialization) exists to prevent.
func (cs *chargingStation) joinHandler(ctx context.Context) error {
	select {
	case <-cs.loadHandlerDone():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// retirePreviousGeneration closes and joins the CURRENT generation before a
// new one is installed - the generation-orphan remedy spec §2 decides on for
// ALL THREE Start* paths (this facade's Start AND StartWithRetries, plus
// ocpp1.6's Start). Without it, a second Start*/StartWithRetries leaves the
// prior generation's handler parked forever on a stopC no future Stop() can
// reach (Stop() only ever closes loadStopC()'s CURRENT value via the
// accessor), and lets that orphaned handler's clearCallbacks() drain
// callbacks the new generation just registered - cs.callbacks is shared
// across generations.
//
// Reuses StopCtx verbatim (spec §2: "reuse the §2 join for the implicit
// stop... the guarantee is the same one Stop() gives") rather than only
// closing stopC and joining the handler. Calling cs.client.Stop() here is
// NOT optional: cs.client is one long-lived *ocppj.Client shared across
// every generation, and its dispatcher cannot safely have two overlapping
// Start()/messagePump lifetimes - ocppj/dispatcher.go's Start() reassigns
// d.timer (and d.requestChannel/d.doneC) unguarded against a still-running
// PREVIOUS messagePump goroutine reading them, which a plain facade-level
// retirement (close+join only, no client.Stop()) leaves running whenever the
// previous generation's handler was pinned. Confirmed empirically: an
// earlier version of this function omitted client.Stop() and
// go test -race caught the exact d.timer read/write race on the sibling
// 1.6 facade's TestL1GenerationOrphanSecondStartJoinsPreviousGeneration.
// Calling StopCtx's full sequence (close stopC, client.Stop(), join)
// guarantees the previous generation's dispatcher has FULLY exited
// (client.Stop() blocks on dispatcher.Stop()'s <-done) before
// Start/StartWithRetries proceeds to call client.Start()/StartWithRetries()
// again below.
//
// Guarded on stopOnce == nil (Start*/StartWithRetries has never been
// called): with no previous generation there is nothing to retire, and -
// unlike closeStopC and joinHandler, which both degrade to safe no-ops on a
// fresh facade - StopCtx unconditionally calls cs.client.Stop() too, which
// is NOT a no-op against a real (or mocked) network client. Skipping the
// whole retirement here, rather than relying on StopCtx's inner no-ops,
// avoids an extra, surprising client.Stop()/IsConnected() call on every very
// first Start*() - which most callers (and most of this repo's existing
// Start()-only tests, which do not mock Stop()/IsConnected() at all) do not
// expect.
func (cs *chargingStation) retirePreviousGeneration() {
	if cs.stopOnce == nil {
		return
	}
	_ = cs.StopCtx(context.Background())
}

func (cs *chargingStation) Start(csmsUrl string) error {
	// Generation-orphan remedy (spec §2): retire the PREVIOUS generation, if
	// any, before installing a new one. See retirePreviousGeneration.
	cs.retirePreviousGeneration()

	// stopC/handlerDone are close-only (never carry a value), so both are
	// unbuffered. Created here as LOCAL variables and passed directly to the
	// spawned handler below (spec §3's real parameter-passing) - never
	// re-derived via the accessors, which is what would reopen the
	// delayed-spawn rebind window (see asyncCallbackHandler's doc comment).
	stopC := make(chan struct{})
	handlerDone := make(chan struct{})
	cs.storeStopC(stopC)
	cs.stopOnce = &sync.Once{}
	err := cs.client.Start(csmsUrl)
	// Async response handler receives incoming responses/errors and triggers
	// callbacks - wired up, and handlerDone replaced, ONLY on success. On
	// failure there is no handler to join, so handlerDone must be left at
	// its previous (already-closed) value; unconditionally storing a fresh,
	// OPEN handlerDone here would make a subsequent Stop()/StopCtx() join
	// forever on a handler that was never spawned (see
	// TestL1StopAfterFailedStartReturns).
	if err == nil {
		cs.storeHandlerDone(handlerDone)
		go cs.asyncCallbackHandler(stopC, handlerDone)
	}
	return err
}

func (cs *chargingStation) StartWithRetries(csmsUrl string) {
	// Generation-orphan remedy (spec §2): retire the PREVIOUS generation, if
	// any, before installing a new one. See retirePreviousGeneration. This is
	// the path spec §2 names explicitly as the reason the decided remedy is
	// "close-previous" rather than "reject-second-start": StartWithRetries
	// returns void, so a rejection could not be reported through it.
	cs.retirePreviousGeneration()

	// stopC/handlerDone: see the identical comment in Start.
	stopC := make(chan struct{})
	handlerDone := make(chan struct{})
	cs.storeStopC(stopC)
	cs.stopOnce = &sync.Once{}
	cs.client.StartWithRetries(csmsUrl)
	// Async response handler receives incoming responses/errors and triggers
	// callbacks. Unlike Start, StartWithRetries has no failure path to guard
	// against - the underlying client retries in the background and always
	// "succeeds" from the facade's perspective - so handlerDone/the handler
	// are unconditionally installed, matching the pre-PR-L1 behavior.
	cs.storeHandlerDone(handlerDone)
	go cs.asyncCallbackHandler(stopC, handlerDone)
}

// Stop is StopCtx bounded by context.Background(): it always waits for the
// in-flight handler generation to fully exit, however long that takes. See
// the ChargingStation interface's Stop/StopCtx godocs (v2.go) for the
// hazards this join introduces.
func (cs *chargingStation) Stop() {
	_ = cs.StopCtx(context.Background())
}

// StopCtx is the context-bounded variant of Stop. See the ChargingStation
// interface godoc (v2.go) for the full consumer-facing contract.
func (cs *chargingStation) StopCtx(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// Close stopC BEFORE client.Stop() (via closeStopC) - see the sibling
	// 1.6 chargePoint.StopCtx comment (charge_point.go) for the full
	// rationale: client.Stop() blocks until the dispatcher pump drains, and
	// every producer that can wedge that pump (onRequestTimeout, the
	// forwarding closures, error()) is preemptible against stopC. Closing it
	// AFTER client.Stop() would make that preemption dead code. See spec
	// §Sequencing.
	//
	// closeStopC is stopOnce/nil-guarded: Stop() before Start() (stopOnce is
	// nil - nothing to close) and a repeated Stop()/StopCtx() (stopOnce
	// already fired) must not panic.
	cs.closeStopC()
	cs.client.Stop()

	// The join (spec §2): wait for the in-flight handler generation to fully
	// exit, bounded by ctx. This is the ONLY part of Stop/StopCtx that can
	// legitimately run long - client.Stop() above is already fast regardless
	// of the handler's state (every producer it drains through is
	// preemptible against the now-closed stopC); the join instead waits on
	// USER code (an in-flight SendRequestAsync callback, or - since S3b - an
	// inbound request handler, which now runs on this same joined handler
	// goroutine rather than the unjoined readPump) that may run arbitrarily
	// long. On
	// ctx expiry this returns ctx.Err() without waiting further - stopC is
	// already closed regardless, so a RETRY StopCtx (even with a fresh
	// context) is the supported way to reach a clean stop afterwards: the
	// handler will have exited (or will shortly) and the retry's join
	// succeeds immediately.
	//
	// cs.errC is deliberately NEVER closed here, or anywhere (spec §1, user
	// decision - supersedes an earlier "close after a successful join" plan
	// that turned out to be unsafe even with the join in place: error()
	// guards on errC == nil, not closedness, so a LATER generation's
	// error() would send on a closed channel and panic on an internally-
	// spawned goroutine the consumer cannot recover; and a RETRY StopCtx
	// joins a handlerDone that is already closed (closed stays closed), so
	// it would double-close errC too). See the Errors() godoc (v2.go) for
	// the full, permanent contract.
	return cs.joinHandler(ctx)
}

func (cs *chargingStation) IsConnected() bool {
	return cs.client.IsConnected()
}

func (cs *chargingStation) notImplementedError(requestId string, action string) {
	err := cs.client.SendError(requestId, ocppj.NotImplemented, fmt.Sprintf("no handler for action %v implemented", action), nil)
	if err != nil {
		cs.error(fmt.Errorf("replying csms to request %v with error: %w", requestId, err))
	}
}

func (cs *chargingStation) notSupportedError(requestId string, action string) {
	err := cs.client.SendError(requestId, ocppj.NotSupported, fmt.Sprintf("unsupported action %v on charging station", action), nil)
	if err != nil {
		cs.error(fmt.Errorf("replying csms to request %s with 'not supported': %w", requestId, err))
	}
}

func (cs *chargingStation) handleIncomingRequest(request ocpp.Request, requestId string, action string) {
	profile, found := cs.client.GetProfileForFeature(action)
	// Check whether action is supported and a listener for it exists
	if !found {
		cs.notImplementedError(requestId, action)
		return
	} else {
		supported := true
		switch profile.Name {
		case authorization.ProfileName:
			if cs.authorizationHandler == nil {
				supported = false
			}
		case availability.ProfileName:
			if cs.availabilityHandler == nil {
				supported = false
			}
		case data.ProfileName:
			if cs.dataHandler == nil {
				supported = false
			}
		case diagnostics.ProfileName:
			if cs.diagnosticsHandler == nil {
				supported = false
			}
		case display.ProfileName:
			if cs.displayHandler == nil {
				supported = false
			}
		case firmware.ProfileName:
			if cs.firmwareHandler == nil {
				supported = false
			}
		case iso15118.ProfileName:
			if cs.iso15118Handler == nil {
				supported = false
			}
		case localauth.ProfileName:
			if cs.localAuthListHandler == nil {
				supported = false
			}
		case meter.ProfileName:
			if cs.meterHandler == nil {
				supported = false
			}
		case provisioning.ProfileName:
			if cs.provisioningHandler == nil {
				supported = false
			}
		case remotecontrol.ProfileName:
			if cs.remoteControlHandler == nil {
				supported = false
			}
		case reservation.ProfileName:
			if cs.reservationHandler == nil {
				supported = false
			}
		case security.ProfileName:
			if cs.securityHandler == nil {
				supported = false
			}
		case smartcharging.ProfileName:
			if cs.smartChargingHandler == nil {
				supported = false
			}
		case tariffcost.ProfileName:
			if cs.tariffCostHandler == nil {
				supported = false
			}
		case transactions.ProfileName:
			if cs.transactionsHandler == nil {
				supported = false
			}
		}
		if !supported {
			cs.notSupportedError(requestId, action)
			return
		}
	}
	// Process request
	var response ocpp.Response
	var err error
	switch action {
	case reservation.CancelReservationFeatureName:
		response, err = cs.reservationHandler.OnCancelReservation(request.(*reservation.CancelReservationRequest))
	case security.CertificateSignedFeatureName:
		response, err = cs.securityHandler.OnCertificateSigned(request.(*security.CertificateSignedRequest))
	case availability.ChangeAvailabilityFeatureName:
		response, err = cs.availabilityHandler.OnChangeAvailability(request.(*availability.ChangeAvailabilityRequest))
	case authorization.ClearCacheFeatureName:
		response, err = cs.authorizationHandler.OnClearCache(request.(*authorization.ClearCacheRequest))
	case smartcharging.ClearChargingProfileFeatureName:
		response, err = cs.smartChargingHandler.OnClearChargingProfile(request.(*smartcharging.ClearChargingProfileRequest))
	case display.ClearDisplayMessageFeatureName:
		response, err = cs.displayHandler.OnClearDisplay(request.(*display.ClearDisplayRequest))
	case diagnostics.ClearVariableMonitoringFeatureName:
		response, err = cs.diagnosticsHandler.OnClearVariableMonitoring(request.(*diagnostics.ClearVariableMonitoringRequest))
	case tariffcost.CostUpdatedFeatureName:
		response, err = cs.tariffCostHandler.OnCostUpdated(request.(*tariffcost.CostUpdatedRequest))
	case diagnostics.CustomerInformationFeatureName:
		response, err = cs.diagnosticsHandler.OnCustomerInformation(request.(*diagnostics.CustomerInformationRequest))
	case data.DataTransferFeatureName:
		response, err = cs.dataHandler.OnDataTransfer(request.(*data.DataTransferRequest))
	case iso15118.DeleteCertificateFeatureName:
		response, err = cs.iso15118Handler.OnDeleteCertificate(request.(*iso15118.DeleteCertificateRequest))
	case provisioning.GetBaseReportFeatureName:
		response, err = cs.provisioningHandler.OnGetBaseReport(request.(*provisioning.GetBaseReportRequest))
	case smartcharging.GetChargingProfilesFeatureName:
		response, err = cs.smartChargingHandler.OnGetChargingProfiles(request.(*smartcharging.GetChargingProfilesRequest))
	case smartcharging.GetCompositeScheduleFeatureName:
		response, err = cs.smartChargingHandler.OnGetCompositeSchedule(request.(*smartcharging.GetCompositeScheduleRequest))
	case display.GetDisplayMessagesFeatureName:
		response, err = cs.displayHandler.OnGetDisplayMessages(request.(*display.GetDisplayMessagesRequest))
	case iso15118.GetInstalledCertificateIdsFeatureName:
		response, err = cs.iso15118Handler.OnGetInstalledCertificateIds(request.(*iso15118.GetInstalledCertificateIdsRequest))
	case localauth.GetLocalListVersionFeatureName:
		response, err = cs.localAuthListHandler.OnGetLocalListVersion(request.(*localauth.GetLocalListVersionRequest))
	case diagnostics.GetLogFeatureName:
		response, err = cs.diagnosticsHandler.OnGetLog(request.(*diagnostics.GetLogRequest))
	case diagnostics.GetMonitoringReportFeatureName:
		response, err = cs.diagnosticsHandler.OnGetMonitoringReport(request.(*diagnostics.GetMonitoringReportRequest))
	case provisioning.GetReportFeatureName:
		response, err = cs.provisioningHandler.OnGetReport(request.(*provisioning.GetReportRequest))
	case transactions.GetTransactionStatusFeatureName:
		response, err = cs.transactionsHandler.OnGetTransactionStatus(request.(*transactions.GetTransactionStatusRequest))
	case provisioning.GetVariablesFeatureName:
		response, err = cs.provisioningHandler.OnGetVariables(request.(*provisioning.GetVariablesRequest))
	case iso15118.InstallCertificateFeatureName:
		response, err = cs.iso15118Handler.OnInstallCertificate(request.(*iso15118.InstallCertificateRequest))
	case firmware.PublishFirmwareFeatureName:
		response, err = cs.firmwareHandler.OnPublishFirmware(request.(*firmware.PublishFirmwareRequest))
	case remotecontrol.RequestStartTransactionFeatureName:
		response, err = cs.remoteControlHandler.OnRequestStartTransaction(request.(*remotecontrol.RequestStartTransactionRequest))
	case remotecontrol.RequestStopTransactionFeatureName:
		response, err = cs.remoteControlHandler.OnRequestStopTransaction(request.(*remotecontrol.RequestStopTransactionRequest))
	case reservation.ReserveNowFeatureName:
		response, err = cs.reservationHandler.OnReserveNow(request.(*reservation.ReserveNowRequest))
	case provisioning.ResetFeatureName:
		response, err = cs.provisioningHandler.OnReset(request.(*provisioning.ResetRequest))
	case localauth.SendLocalListFeatureName:
		response, err = cs.localAuthListHandler.OnSendLocalList(request.(*localauth.SendLocalListRequest))
	case smartcharging.SetChargingProfileFeatureName:
		response, err = cs.smartChargingHandler.OnSetChargingProfile(request.(*smartcharging.SetChargingProfileRequest))
	case display.SetDisplayMessageFeatureName:
		response, err = cs.displayHandler.OnSetDisplayMessage(request.(*display.SetDisplayMessageRequest))
	case diagnostics.SetMonitoringBaseFeatureName:
		response, err = cs.diagnosticsHandler.OnSetMonitoringBase(request.(*diagnostics.SetMonitoringBaseRequest))
	case diagnostics.SetMonitoringLevelFeatureName:
		response, err = cs.diagnosticsHandler.OnSetMonitoringLevel(request.(*diagnostics.SetMonitoringLevelRequest))
	case provisioning.SetNetworkProfileFeatureName:
		response, err = cs.provisioningHandler.OnSetNetworkProfile(request.(*provisioning.SetNetworkProfileRequest))
	case diagnostics.SetVariableMonitoringFeatureName:
		response, err = cs.diagnosticsHandler.OnSetVariableMonitoring(request.(*diagnostics.SetVariableMonitoringRequest))
	case provisioning.SetVariablesFeatureName:
		response, err = cs.provisioningHandler.OnSetVariables(request.(*provisioning.SetVariablesRequest))
	case remotecontrol.TriggerMessageFeatureName:
		response, err = cs.remoteControlHandler.OnTriggerMessage(request.(*remotecontrol.TriggerMessageRequest))
	case remotecontrol.UnlockConnectorFeatureName:
		response, err = cs.remoteControlHandler.OnUnlockConnector(request.(*remotecontrol.UnlockConnectorRequest))
	case firmware.UnpublishFirmwareFeatureName:
		response, err = cs.firmwareHandler.OnUnpublishFirmware(request.(*firmware.UnpublishFirmwareRequest))
	case firmware.UpdateFirmwareFeatureName:
		response, err = cs.firmwareHandler.OnUpdateFirmware(request.(*firmware.UpdateFirmwareRequest))
	default:
		cs.notSupportedError(requestId, action)
		return
	}
	cs.sendResponse(response, err, requestId)
}
