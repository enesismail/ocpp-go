package configmanager

import (
	"errors"
	"fmt"
	"strings"

	"github.com/enesismail/ocpp-go/ocpp1.6/core"
)

// Errors returned by Config's and ManagerV16's methods. Exported so
// consumers can errors.Is() against them (DB9).
var (
	ErrKeyNotFound = errors.New("key not found")
	ErrReadOnly    = errors.New("attribute is read-only")
)

// Config is an in-memory, versioned collection of OCPP 1.6 configuration
// keys. It has no persistence of its own: a consumer wanting durability
// reads it back out (via ManagerV16.GetConfiguration) and serializes it.
//
// Unlike the ported source, Config carries no "fig:"/"default:" struct tags
// (D3) — those were inert (no kkyr/fig loader is used anywhere in this
// package or its consumers) and the Version=1 default now lives in
// NewEmptyConfiguration / NewManagerV16 instead.
type Config struct {
	Version int
	Keys    []core.ConfigurationKey
}

// UpdateKey updates the value of key in place, unless the key is unknown
// (ErrKeyNotFound) or marked read-only (ErrReadOnly). This is a low-level,
// unlocked, non-deep-copying operation on the Config value itself; the
// concurrency-safe, deep-copying, handler-aware equivalent for a running
// ManagerV16 is ManagerV16.UpdateKey.
func (config *Config) UpdateKey(key string, value *string) error {
	item, index, isFound := findKeyIndex(config.Keys, key)
	if !isFound {
		return ErrKeyNotFound
	}

	if item.Readonly {
		return ErrReadOnly
	}

	config.Keys[index].Value = value
	return nil
}

// UpdateKeyReadability sets whether key is read-only. readonly=true marks
// the key read-only; readonly=false clears it. (MINOR-11: the source named
// this parameter "readable" while assigning it straight to Readonly — an
// inverted, misleading name for a value that was always meant to mean
// "readonly". The parameter is renamed here; the assignment itself was
// already correct.)
func (config *Config) UpdateKeyReadability(key string, readonly bool) error {
	_, index, isFound := findKeyIndex(config.Keys, key)
	if !isFound {
		return ErrKeyNotFound
	}

	config.Keys[index].Readonly = readonly
	return nil
}

// GetConfigurationValue returns the value of key, or ErrKeyNotFound if it
// doesn't exist. Like UpdateKey, this is a low-level, non-deep-copying
// accessor on the Config value itself.
func (config *Config) GetConfigurationValue(key string) (*string, error) {
	item, _, isFound := findKeyIndex(config.Keys, key)
	if !isFound {
		return nil, ErrKeyNotFound
	}

	return item.Value, nil
}

// GetConfig returns the full list of configuration keys.
func (config *Config) GetConfig() []core.ConfigurationKey {
	return config.Keys
}

// GetVersion returns the configuration's version number.
func (config *Config) GetVersion() int {
	return config.Version
}

// SetVersion sets the configuration's version number.
func (config *Config) SetVersion(version int) {
	config.Version = version
}

// Validate checks that every key in mandatoryKeys is present in the
// configuration, returning an error naming the missing keys if not.
//
// Duplicate key names within Keys are NOT rejected (matching the port source):
// lookups and updates use the first match, while GetConfiguration returns every
// entry. Duplicates cannot arise via the OCPP wire (one key per request); only
// a caller hand-building a Config can introduce them.
func (config *Config) Validate(mandatoryKeys []Key) error {
	var missing []string

	for _, key := range mandatoryKeys {
		if _, _, found := findKeyIndex(config.Keys, key.String()); !found {
			missing = append(missing, key.String())
		}
	}

	if len(missing) > 0 {
		return fmt.Errorf("missing mandatory keys: %s", strings.Join(missing, ", "))
	}

	return nil
}
