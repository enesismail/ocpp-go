package configmanager

import (
	"errors"

	"github.com/enesismail/ocpp-go/ocpp1.6/core"
)

// OnGetConfiguration answers a core.GetConfigurationRequest from the store: an
// empty/nil req.Key returns ALL keys; otherwise the requested keys are split
// into known ConfigurationKey entries (deep-copied, D6) and UnknownKey names.
// A nil request (a defensive guard — the facade never dispatches one) returns
// an empty confirmation, consistent with OnChangeConfiguration's nil handling.
// Matching is case-sensitive/byte-exact (see the package doc).
// Thread-safe; a one-line delegation target for a consumer's
// core.ChargePointHandler.OnGetConfiguration:
//
//	func (h *myHandler) OnGetConfiguration(req *core.GetConfigurationRequest) (*core.GetConfigurationConfirmation, error) {
//	    return h.cfg.OnGetConfiguration(req), nil
//	}
func (m *ManagerV16) OnGetConfiguration(req *core.GetConfigurationRequest) *core.GetConfigurationConfirmation {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Defensive: a nil request pointer (never dispatched by the facade) is NOT
	// treated as the permissive "return all" — that is reserved for a non-nil
	// request with an empty Key list. This keeps nil-request handling symmetric
	// with OnChangeConfiguration (MINOR-3).
	if req == nil {
		return &core.GetConfigurationConfirmation{}
	}

	// An empty/nil Key list is the OCPP "return all configuration" request.
	if len(req.Key) == 0 {
		return &core.GetConfigurationConfirmation{
			ConfigurationKey: deepCopyKeys(m.ocppConfig.Keys),
		}
	}

	var known []core.ConfigurationKey
	var unknown []string
	for _, k := range req.Key {
		item, _, found := findKeyIndex(m.ocppConfig.Keys, k)
		if !found {
			unknown = append(unknown, k)
			continue
		}
		known = append(known, deepCopyKey(item))
	}

	return &core.GetConfigurationConfirmation{
		ConfigurationKey: known,
		UnknownKey:       unknown,
	}
}

// OnChangeConfiguration applies a core.ChangeConfigurationRequest and maps
// the outcome to a ConfigurationStatus:
//
//   - key empty                                   -> Rejected (malformed)
//   - key unknown                                -> NotSupported
//   - key read-only                               -> Rejected
//   - registered custom validator returns false    -> Rejected
//   - applied, OnUpdateHandler returns an error    -> Rejected (value rolled
//     back to its pre-call value, atomically, under the lock — see D4b)
//   - applied, key is in the reboot-required set   -> RebootRequired (the
//     value IS applied)
//   - applied cleanly                              -> Accepted
//
// A PANICKING OnUpdateHandler is not mapped to a status: the store is rolled
// back and the panic propagates (see runHandlerLocked) for the charge-point
// facade's panic isolation to report — a panic is a bug, not a rejection.
//
// An empty req.Value is a legal value: it is stored as a pointer to ""
// (never left nil).
//
// Any registered OnUpdateHandler for the key runs UNDER the manager lock as
// part of one atomic classify-apply-rollback critical section (see
// updateKeyLocked and the OnUpdateHandler doc): because it therefore runs on
// the serialized inbound-dispatch goroutine, it MUST be fast/non-blocking
// and MUST NOT call back into the manager (self-deadlock).
//
// OnChangeConfiguration never returns an error — every outcome is expressed
// as a status. A one-line delegation target for a consumer's
// core.ChargePointHandler.OnChangeConfiguration:
//
//	func (h *myHandler) OnChangeConfiguration(req *core.ChangeConfigurationRequest) (*core.ChangeConfigurationConfirmation, error) {
//	    return h.cfg.OnChangeConfiguration(req), nil
//	}
//
// Note on errors vs. Rejected: a handler returning an error maps to
// Rejected, not a protocol-layer CallError — this is OCPP-conformant
// ("Rejected" means "the CP did not set the configuration") and this
// helper's signature (no error return) can't emit a CallError anyway. If a
// consumer wants a CallError for a genuine protocol-layer fault, that
// belongs in the consumer's own OnChangeConfiguration handler, which may
// still return a non-nil error independently of what this helper returns.
func (m *ManagerV16) OnChangeConfiguration(req *core.ChangeConfigurationRequest) *core.ChangeConfigurationConfirmation {
	m.mu.Lock()
	defer m.mu.Unlock()

	if req == nil {
		return &core.ChangeConfigurationConfirmation{Status: core.ConfigurationStatusRejected}
	}

	key := Key(req.Key)
	if err := m.updateKeyLocked(key, ptr(req.Value)); err != nil {
		if errors.Is(err, ErrKeyNotFound) {
			return &core.ChangeConfigurationConfirmation{Status: core.ConfigurationStatusNotSupported}
		}
		// Read-only, custom-validator failure, or a rolled-back handler
		// error all surface to the OCPP layer as Rejected (D4b).
		return &core.ChangeConfigurationConfirmation{Status: core.ConfigurationStatusRejected}
	}

	if containsKey(m.rebootRequiredKeys, key) {
		return &core.ChangeConfigurationConfirmation{Status: core.ConfigurationStatusRebootRequired}
	}

	return &core.ChangeConfigurationConfirmation{Status: core.ConfigurationStatusAccepted}
}
