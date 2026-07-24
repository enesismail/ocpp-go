package configmanager

import (
	"errors"
	"fmt"
	"sync"

	"github.com/enesismail/ocpp-go/ocpp1.6/core"
)

// ErrKeyCannotBeEmpty is returned when a mutation is attempted with an empty
// Key (via OnUpdateKey, UpdateKey, or OnChangeConfiguration).
var ErrKeyCannotBeEmpty = errors.New("key cannot be empty")

// ErrNilHandler is returned by OnUpdateKey when given a nil handler: a nil
// OnUpdateHandler stored in the map would panic when later invoked under the
// lock, so it is rejected at registration time (MAJOR-1).
var ErrNilHandler = errors.New("update handler cannot be nil")

type (
	// KeyValidator is a custom, consumer-supplied validation hook: given a
	// key and its proposed new value, it returns whether the change is
	// acceptable. See RegisterCustomKeyValidator.
	KeyValidator func(key Key, value *string) bool

	// OnUpdateHandler is called after a key's value has been applied, so a
	// consumer can react to a configuration change (e.g. push the new value
	// into a live subsystem).
	//
	// It runs SYNCHRONOUSLY, UNDER the manager's internal lock, as part of
	// one atomic apply -> handler -> rollback-on-error critical section: if
	// it returns a non-nil error, the just-applied value is rolled back to
	// what it was before the call, and that error is surfaced to the
	// caller (UpdateKey) or mapped to ConfigurationStatusRejected (from
	// OnChangeConfiguration).
	//
	// Because it runs under a non-reentrant lock on what is, in the
	// charge-point facade, a single serialized inbound-dispatch goroutine,
	// an OnUpdateHandler MUST be fast and non-blocking (a slow handler
	// stalls ALL inbound OCPP traffic) and MUST NOT call back into the
	// manager (UpdateKey, OnChangeConfiguration, OnUpdateKey, ...) — doing
	// so self-deadlocks.
	//
	// If it PANICS, the just-applied value is rolled back (the store stays
	// consistent) and the panic is re-raised rather than swallowed — a panic
	// is treated as a bug, not a business rejection.
	OnUpdateHandler func(value *string) error

	// Manager is the ported configuration-manager surface (9 methods,
	// unchanged from the source). The two facade-wiring helpers
	// (OnGetConfiguration/OnChangeConfiguration) are deliberately NOT part
	// of this interface — they are concrete *ManagerV16 methods (DB8).
	Manager interface {
		SetMandatoryKeys(mandatoryKeys []Key) error
		GetMandatoryKeys() []Key
		RegisterCustomKeyValidator(KeyValidator)
		ValidateKey(key Key, value *string) error
		UpdateKey(key Key, value *string) error
		OnUpdateKey(key Key, handler OnUpdateHandler) error
		GetConfigurationValue(key Key) (*string, error)
		SetConfiguration(configuration Config) error
		GetConfiguration() ([]core.ConfigurationKey, error)
	}

	// ManagerV16 is the concrete, concurrency-safe OCPP 1.6 configuration
	// manager. Every exported method takes/releases the internal mutex
	// itself; internal *Locked helper methods assume the caller already
	// holds it (the mutex is a plain, NON-REENTRANT sync.Mutex — never call
	// a public, locking method from inside another critical section).
	ManagerV16 struct {
		mu sync.Mutex

		ocppConfig Config

		mandatoryKeys      []Key
		rebootRequiredKeys []Key

		keyValidator     KeyValidator
		onUpdateHandlers map[Key]OnUpdateHandler
	}
)

// NewManagerV16 creates a ManagerV16 seeded with defaultConfiguration,
// requiring the mandatory keys of the given profiles (see
// GetMandatoryKeysForProfile) to be present in it. defaultConfiguration is
// deep-copied in (D6): later caller-side mutation of it (or of any *string
// Value within it) never leaks into the manager's store.
func NewManagerV16(defaultConfiguration Config, profiles ...string) (*ManagerV16, error) {
	mandatoryKeys := GetMandatoryKeysForProfile(profiles...)

	if err := defaultConfiguration.Validate(mandatoryKeys); err != nil {
		return nil, err
	}

	return &ManagerV16{
		ocppConfig:       deepCopyConfig(defaultConfiguration),
		mandatoryKeys:    mandatoryKeys,
		onUpdateHandlers: make(map[Key]OnUpdateHandler),
	}, nil
}

// SetConfiguration validates configuration against the manager's mandatory
// keys and, if valid, replaces the stored configuration with a deep copy of
// it (D6).
func (m *ManagerV16) SetConfiguration(configuration Config) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := configuration.Validate(m.mandatoryKeys); err != nil {
		return err
	}

	m.ocppConfig = deepCopyConfig(configuration)
	return nil
}

// RegisterCustomKeyValidator registers validator as the custom key
// validator consulted by ValidateKey/UpdateKey/OnChangeConfiguration.
// Passing nil clears it, reverting to the default accept-everything
// behavior.
func (m *ManagerV16) RegisterCustomKeyValidator(validator KeyValidator) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.keyValidator = validator
}

// GetMandatoryKeys returns a copy of the manager's mandatory keys (D6).
func (m *ManagerV16) GetMandatoryKeys() []Key {
	m.mu.Lock()
	defer m.mu.Unlock()

	return copyKeys(m.mandatoryKeys)
}

// SetMandatoryKeys adds mandatoryKeys to the manager's mandatory-key set
// (skipping any already present); the input slice is copied in (D6), so a
// later caller-side mutation of it cannot affect the manager.
func (m *ManagerV16) SetMandatoryKeys(mandatoryKeys []Key) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, key := range mandatoryKeys {
		if containsKey(m.mandatoryKeys, key) {
			continue
		}
		m.mandatoryKeys = append(m.mandatoryKeys, key)
	}

	return nil
}

// SetRebootRequiredKeys sets the (opt-in, default-empty) set of keys whose
// successful change should be reported as ConfigurationStatusRebootRequired
// rather than ConfigurationStatusAccepted by OnChangeConfiguration. The
// input slice is copied in (D6).
func (m *ManagerV16) SetRebootRequiredKeys(keys []Key) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.rebootRequiredKeys = copyKeys(keys)
}

// validateKeyLocked runs the registered custom validator, if any, against
// key/value. Callers must already hold m.mu.
func (m *ManagerV16) validateKeyLocked(key Key, value *string) error {
	if m.keyValidator == nil {
		return nil
	}

	if !m.keyValidator(key, value) {
		return fmt.Errorf("key validation failed for key %s", key)
	}

	return nil
}

// ValidateKey runs the registered custom validator, if any, against
// key/value; with no validator registered, every value is accepted (the
// explicit default).
func (m *ManagerV16) ValidateKey(key Key, value *string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.validateKeyLocked(key, value)
}

// updateKeyLocked is the single atomicity primitive shared by UpdateKey and
// OnChangeConfiguration (D4b): it validates, applies value to key, and — if
// an OnUpdateHandler is registered for key — invokes it (still holding the
// lock, per the OnUpdateHandler contract) and rolls back to the
// pre-apply value if the handler errors. Callers must already hold m.mu.
func (m *ManagerV16) updateKeyLocked(key Key, value *string) error {
	// Reject an empty key consistently across every mutation path (an empty
	// key would otherwise fall through to a findKeyIndex miss and surface as
	// the misleading ErrKeyNotFound — MINOR-1).
	if key.String() == "" {
		return ErrKeyCannotBeEmpty
	}

	if err := m.validateKeyLocked(key, value); err != nil {
		return err
	}

	original, index, found := findKeyIndex(m.ocppConfig.Keys, key.String())
	if !found {
		return ErrKeyNotFound
	}
	if original.Readonly {
		return ErrReadOnly
	}

	m.ocppConfig.Keys[index].Value = value

	if handler, ok := m.onUpdateHandlers[key]; ok {
		return m.runHandlerLocked(handler, value, index, original.Value)
	}

	return nil
}

// runHandlerLocked invokes handler(value) while m.mu is held and guarantees the
// store ends exactly where it started on ANY handler failure. On a returned
// error it rolls the key at index back to origValue and surfaces the error
// (the handler saw the new value, as documented, but rejected it — D4b). On a
// PANIC it rolls back and RE-PANICS (MAJOR-2): a panicking handler must not
// strand the just-applied value, but a panic is a bug rather than a business
// rejection, so it stays loud — the charge-point facade's request-handler
// panic isolation (charge_point.go RecoverPanicGoroutine) reports it while the
// store remains consistent.
func (m *ManagerV16) runHandlerLocked(handler OnUpdateHandler, value *string, index int, origValue *string) error {
	defer func() {
		if r := recover(); r != nil {
			m.ocppConfig.Keys[index].Value = origValue
			panic(r)
		}
	}()

	if err := handler(value); err != nil {
		m.ocppConfig.Keys[index].Value = origValue
		return err
	}
	return nil
}

// UpdateKey validates and applies value to key. Any error from the
// registered OnUpdateHandler (if one is registered for key) is surfaced
// here directly, and the store is rolled back to its pre-call value (D4b) —
// unlike the source, which discarded the handler's error via a no-op defer.
func (m *ManagerV16) UpdateKey(key Key, value *string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.updateKeyLocked(key, value)
}

// GetConfiguration returns a deep copy of every stored configuration key
// (D6).
func (m *ManagerV16) GetConfiguration() ([]core.ConfigurationKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return deepCopyKeys(m.ocppConfig.Keys), nil
}

// GetConfigurationValue returns a deep copy of key's value (D6), or
// ErrKeyNotFound if key is unknown.
func (m *ManagerV16) GetConfigurationValue(key Key) (*string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	value, err := m.ocppConfig.GetConfigurationValue(key.String())
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, nil
	}
	return ptr(*value), nil
}

// OnUpdateKey registers handler to run (under the lock, see the
// OnUpdateHandler doc) whenever key's value is subsequently changed via
// UpdateKey or OnChangeConfiguration. Registering again for the same key
// replaces the previous handler (last-write-wins map assignment) — this is
// not an error.
//
// handler MUST be fast/non-blocking and MUST NOT call back into the manager
// (self-deadlock on the non-reentrant lock) — see the OnUpdateHandler doc
// for the full contract.
func (m *ManagerV16) OnUpdateKey(key Key, handler OnUpdateHandler) error {
	if key.String() == "" {
		return ErrKeyCannotBeEmpty
	}
	// Reject a nil handler at registration: a nil func stored in the map is a
	// legal map entry (ok==true on lookup) that panics when invoked under the
	// lock, which would bypass the atomic rollback (MAJOR-1).
	if handler == nil {
		return ErrNilHandler
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, err := m.ocppConfig.GetConfigurationValue(key.String()); err != nil {
		return err
	}

	m.onUpdateHandlers[key] = handler
	return nil
}
