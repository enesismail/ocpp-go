// Package configmanager implements a charge-point-side OCPP 1.6 configuration
// key store and manager: a typed, in-memory Config (key -> ConfigurationKey)
// plus a ManagerV16 that adds mandatory-key validation, custom key
// validators, per-key update handlers, and two facade-wiring helpers
// (OnGetConfiguration / OnChangeConfiguration) that turn an inbound
// core.GetConfigurationRequest / core.ChangeConfigurationRequest into a
// confirmation — a one-line delegation from the consumer's
// core.ChargePointHandler implementation.
//
// # Provenance
//
// This package is a PORT of xBlaz3kx/ocpp-go's ocpp1.6/config_manager
// package (MIT licensed). That original repository now 404s; the package
// lives on at github.com/ChargePi/ocpp-manager (package ocpp_v16), which is
// what this port was fetched from. It is not fork-original code — see
// PATCHES.md for the list of behavioral fixes made during the port
// (ISO15118 profile sentinel + defaults, UpdateKey error surfacing with
// atomic rollback, locking added to four previously-unguarded methods, and
// deep-copy boundaries on every shared slice/pointer). The dependencies
// (samber/lo, go-commons-lang) were stripped to stdlib-only equivalents. The
// two facade-wiring helpers (OnGetConfiguration/OnChangeConfiguration) are
// net new: upstream never wired the store to an inbound OCPP handler.
//
// # Case sensitivity
//
// Key matching throughout this package (Config and ManagerV16 lookups, and
// both wiring helpers) is case-sensitive and byte-exact.
// "heartbeatinterval" does NOT match "HeartbeatInterval" — a conformant CSMS
// sends the standard CamelCase key names, so this is intentional and
// matches the ported behavior (plain string comparison, no normalization or
// case-folding anywhere in the package).
//
// # OnUpdateHandler constraints
//
// A registered OnUpdateHandler (see OnUpdateKey) runs SYNCHRONOUSLY, UNDER
// the manager's internal lock, as part of one atomic
// apply -> handler -> rollback-on-error critical section. Because inbound
// OCPP dispatch on the charge-point facade is serialized onto a single
// goroutine, a slow handler stalls ALL inbound OCPP traffic (heartbeats,
// transactions, and every other feature), and a handler that calls back
// into the ManagerV16 (UpdateKey, OnChangeConfiguration, OnUpdateKey, ...)
// will self-deadlock on the non-reentrant lock. Handlers MUST be fast,
// non-blocking, and MUST NOT call back into the manager.
//
// # Non-goals
//
// No persistence layer (in-memory, process-lifetime only — call
// GetConfiguration and serialize it yourself if you need durability); no
// CSMS-side manager (charge-point side only); no OCPP 2.0.1 variant.
package configmanager
