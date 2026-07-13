package ocpp2

import (
	"context"
	"fmt"
	"reflect"
	"sync"

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
	responseHandler      chan ocpp.Response
	errorHandler         chan error
	callbacks            callbackqueue.CallbackQueue
	stopC                chan struct{}
	stopOnce             *sync.Once
	errC                 chan error // external error channel
}

// asyncResponse wraps an asynchronous response for delivery via a channel
// from the callback to the sync-send select.
type asyncResponse struct {
	r ocpp.Response
	e error
}

func (cs *chargingStation) error(err error) {
	if cs.errC != nil {
		cs.errC <- err
	}
}

// Errors returns a channel for error messages. If it doesn't exist it es created.
func (cs *chargingStation) Errors() <-chan error {
	if cs.errC == nil {
		cs.errC = make(chan error, 1)
	}
	return cs.errC
}

// Callback invoked whenever a queued request is canceled, due to timeout.
// By default, the callback returns a GenericError to the caller, who sent the original request.
func (cs *chargingStation) onRequestTimeout(_ string, _ ocpp.Request, err *ocpp.Error) {
	cs.errorHandler <- err
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
	send := func() error {
		return cs.client.SendRequestCtx(ctx, request)
	}
	err := cs.callbacks.TryQueue("main", callbackqueue.RequestType(request.GetFeatureName()), send, func(confirmation ocpp.Response, err error) {
		asyncResponseC <- asyncResponse{r: confirmation, e: err}
	})
	if err != nil {
		return nil, err
	}
	stopC := cs.stopC
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
		return nil, fmt.Errorf("charging station stopped while awaiting response to %v request", featureName)
	case <-ctx.Done():
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
	send := func() error {
		return cs.client.SendRequestCtx(ctx, request)
	}
	err := cs.callbacks.TryQueue("main", callbackqueue.RequestType(request.GetFeatureName()), send, callback)
	return err
}

func (cs *chargingStation) asyncCallbackHandler(stopC chan struct{}) {
	for {
		select {
		case confirmation := <-cs.responseHandler:
			// Get and invoke callback
			if callback, ok := cs.callbacks.Dequeue("main", callbackqueue.RequestType(confirmation.GetFeatureName())); ok {
				func() {
					defer cs.client.RecoverPanicGoroutine(ocppj.ResponseHandlerKind, confirmation.GetFeatureName(), "", false)
					callback(confirmation, nil)
				}()
			} else {
				cs.error(fmt.Errorf("no callback available for incoming response %v", confirmation.GetFeatureName()))
			}
		case protoError := <-cs.errorHandler:
			// Get and invoke callback
			if callback, ok := cs.callbacks.Dequeue("main", ""); ok {
				requestID := ""
				if ocppErr, ok := protoError.(*ocpp.Error); ok {
					requestID = ocppErr.MessageId
				}
				func() {
					defer cs.client.RecoverPanicGoroutine(ocppj.ErrorHandlerKind, "", requestID, false)
					callback(nil, protoError)
				}()
			} else {
				cs.error(fmt.Errorf("no callback available for incoming error %w", protoError))
			}
		case <-stopC:
			cs.clearCallbacks()
			return
		}
	}
}

func (cs *chargingStation) clearCallbacks() {
	for _, ok := cs.callbacks.Dequeue("main", ""); ok; _, ok = cs.callbacks.Dequeue("main", "") {
	}
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

func (cs *chargingStation) Start(csmsUrl string) error {
	// Start client
	cs.stopC = make(chan struct{}, 1)
	cs.stopOnce = &sync.Once{}
	err := cs.client.Start(csmsUrl)
	// Async response handler receives incoming responses/errors and triggers callbacks
	if err == nil {
		go cs.asyncCallbackHandler(cs.stopC)
	}
	return err
}

func (cs *chargingStation) StartWithRetries(csmsUrl string) {
	// Start client
	cs.stopC = make(chan struct{}, 1)
	cs.stopOnce = &sync.Once{}
	cs.client.StartWithRetries(csmsUrl)
	// Async response handler receives incoming responses/errors and triggers callbacks
	go cs.asyncCallbackHandler(cs.stopC)
}

func (cs *chargingStation) Stop() {
	cs.client.Stop()
	if cs.stopOnce != nil {
		cs.stopOnce.Do(func() {
			close(cs.stopC)
		})
	}
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
