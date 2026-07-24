package configmanager

import (
	"github.com/enesismail/ocpp-go/ocpp1.6/core"
)

// ptr returns a pointer to a fresh copy of v. It replaces the source's use
// of samber/lo's ToPtr (D2 dep-strip): every call allocates a new value, so
// callers never end up aliasing the pointer of some other variable.
func ptr[T any](v T) *T {
	return &v
}

// containsKey reports whether key is present in keys, using exact ==
// comparison (case-sensitive/byte-exact, see the package doc). It replaces
// the source's samber/lo.ContainsBy call sites (D2).
func containsKey(keys []Key, key Key) bool {
	for _, k := range keys {
		if k == key {
			return true
		}
	}
	return false
}

// findKeyIndex looks up key (compared case-sensitive/byte-exact) in keys and
// returns the matching core.ConfigurationKey, its index, and whether it was
// found. It replaces the source's samber/lo.FindIndexOf/lo.Find call sites
// (D2).
func findKeyIndex(keys []core.ConfigurationKey, key string) (core.ConfigurationKey, int, bool) {
	for i, k := range keys {
		if k.Key == key {
			return k, i, true
		}
	}
	return core.ConfigurationKey{}, -1, false
}

// deepCopyKey returns a core.ConfigurationKey with its own, independent
// Value pointer (D6): mutating the returned key's Value can never leak back
// into (or out of) the caller's copy.
func deepCopyKey(k core.ConfigurationKey) core.ConfigurationKey {
	out := k
	if k.Value != nil {
		out.Value = ptr(*k.Value)
	}
	return out
}

// deepCopyKeys returns a slice of deep copies of keys (D6); a nil input
// yields a nil output, otherwise a fresh backing array is always allocated.
func deepCopyKeys(keys []core.ConfigurationKey) []core.ConfigurationKey {
	if keys == nil {
		return nil
	}
	out := make([]core.ConfigurationKey, len(keys))
	for i, k := range keys {
		out[i] = deepCopyKey(k)
	}
	return out
}

// deepCopyConfig returns a Config whose Keys slice (and every Value
// pointer within it) is independent of cfg's (D6).
func deepCopyConfig(cfg Config) Config {
	return Config{
		Version: cfg.Version,
		Keys:    deepCopyKeys(cfg.Keys),
	}
}

// copyKeys returns an independent copy of a []Key slice (D6): used for the
// mandatory-keys / reboot-required-keys copy-in/copy-out boundaries, where
// the elements themselves (plain strings) need no deep copy, only the slice
// header/backing array must not be shared with the caller.
func copyKeys(keys []Key) []Key {
	if keys == nil {
		return nil
	}
	out := make([]Key, len(keys))
	copy(out, keys)
	return out
}
