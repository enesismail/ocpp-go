package configmanager

// RED-FIRST TDD tests for the not-yet-implemented ocpp1.6/configmanager package.
//
// This file pins the public + a few unexported-helper contracts described in
// tasks/p3-config-store.md (the "Tests" section, items 1-7; item 8 is the
// facade e2e test in ocpp1.6_test/configmanager_e2e_test.go). It is
// intentionally white-box (package configmanager, not configmanager_test) so
// it can exercise the unexported dep-strip helpers (ptr/containsKey/
// findKeyIndex) per the task's Test 1.
//
// Until the production package exists, this file does not compile: Key,
// Config, Manager, ManagerV16, NewManagerV16, ptr, containsKey, findKeyIndex,
// ISO15118ProfileName, MandatoryISO15118Keys, GetMandatoryKeysForProfile,
// NewEmptyConfiguration, DefaultConfigurationFromProfiles,
// DefaultCoreConfiguration, ErrKeyNotFound, ErrReadOnly, ErrKeyCannotBeEmpty
// are all undefined. That is the expected red.
//
// Assumptions made where the spec left an implementation-internal signature
// unspecified (documented here so a reviewer can reconcile against the real
// implementation once it lands):
//   - containsKey(keys []Key, key Key) bool — mirrors manager.go's
//     SetMandatoryKeys use of lo.ContainsBy(m.mandatoryKeys, ...).
//   - findKeyIndex(keys []core.ConfigurationKey, key string) (core.ConfigurationKey, int, bool)
//     — mirrors configuration.go's lo.FindIndexOf(config.Keys, ...) return shape
//     (item, index, found).

import (
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/enesismail/ocpp-go/ocpp1.6/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Static contract pin: ManagerV16 must satisfy the ported Manager interface
// (the two facade-wiring helpers are deliberately NOT part of it, per DB8).
var _ Manager = (*ManagerV16)(nil)

// ==========================================================================
// Test 1 — dep-strip equivalence helpers (D2): ptr, containsKey, findKeyIndex.
// ==========================================================================

func TestPtrHelper(t *testing.T) {
	t.Run("string", func(t *testing.T) {
		s := "abc"
		p := ptr(s)
		require.NotNil(t, p)
		assert.Equal(t, "abc", *p)
		// Must be an independent pointer: mutating the copy of `s` inside ptr's
		// call frame must not somehow alias `p` (regression guard for a
		// naive `func ptr[T any](v *T) *T`-style mistake).
		s = "changed"
		assert.Equal(t, "abc", *p)
	})

	t.Run("int", func(t *testing.T) {
		n := ptr(42)
		require.NotNil(t, n)
		assert.Equal(t, 42, *n)
	})

	t.Run("bool", func(t *testing.T) {
		b := ptr(true)
		require.NotNil(t, b)
		assert.True(t, *b)
	})

	t.Run("each call returns a fresh pointer", func(t *testing.T) {
		p1 := ptr("x")
		p2 := ptr("x")
		assert.NotSame(t, p1, p2)
	})
}

// TestContainsKeyHelper and TestFindKeyIndexHelper pin the loop-based
// dep-strip strategy (D2, see the file-header assumptions above). They are
// coupled to that implementation choice; a future map-based refactor of the
// lookup helpers may retire these two tests.
func TestContainsKeyHelper(t *testing.T) {
	keys := []Key{Key("A"), Key("B"), Key("C")}

	var table = []struct {
		name     string
		haystack []Key
		needle   Key
		want     bool
	}{
		{"present first", keys, Key("A"), true},
		{"present last", keys, Key("C"), true},
		{"absent", keys, Key("Z"), false},
		{"empty haystack", nil, Key("A"), false},
		{"case-sensitive miss", keys, Key("a"), false},
	}
	for _, tc := range table {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, containsKey(tc.haystack, tc.needle))
		})
	}
}

func TestFindKeyIndexHelper(t *testing.T) {
	keys := []core.ConfigurationKey{
		{Key: "A", Readonly: false, Value: ptr("1")},
		{Key: "B", Readonly: true, Value: ptr("2")},
	}

	t.Run("found", func(t *testing.T) {
		item, idx, found := findKeyIndex(keys, "B")
		require.True(t, found)
		assert.Equal(t, 1, idx)
		assert.Equal(t, "B", item.Key)
		assert.True(t, item.Readonly)
	})

	t.Run("not found", func(t *testing.T) {
		_, _, found := findKeyIndex(keys, "Z")
		assert.False(t, found)
	})

	t.Run("empty slice", func(t *testing.T) {
		_, _, found := findKeyIndex(nil, "A")
		assert.False(t, found)
	})
}

// ==========================================================================
// Config-level API surface (UpdateKey/UpdateKeyReadability/GetConfigurationValue/
// GetConfig/GetVersion/SetVersion/Validate + the two exported Config-path
// errors). Not one of the spec's numbered Tests, but pins the "Config struct +
// methods" surface directly, incl. MINOR-11's UpdateKeyReadability(key, readonly)
// param rename (readonly=true must mean "make read-only", not the inverted
// xBlaz `readable` meaning).
// ==========================================================================

func TestConfigDirectMethods(t *testing.T) {
	cfg := NewEmptyConfiguration()
	assert.Equal(t, 1, cfg.GetVersion())
	cfg.SetVersion(2)
	assert.Equal(t, 2, cfg.GetVersion())

	require.NoError(t, cfg.Validate(nil))

	cfg.Keys = []core.ConfigurationKey{{Key: "DirectKey", Readonly: false, Value: ptr("v0")}}

	err := cfg.Validate([]Key{Key("Missing")})
	assert.Error(t, err)

	val, err := cfg.GetConfigurationValue("DirectKey")
	require.NoError(t, err)
	require.NotNil(t, val)
	assert.Equal(t, "v0", *val)

	_, err = cfg.GetConfigurationValue("Nope")
	assert.ErrorIs(t, err, ErrKeyNotFound)

	require.NoError(t, cfg.UpdateKey("DirectKey", ptr("v1")))
	val, err = cfg.GetConfigurationValue("DirectKey")
	require.NoError(t, err)
	assert.Equal(t, "v1", *val)

	err = cfg.UpdateKey("Nope", ptr("x"))
	assert.ErrorIs(t, err, ErrKeyNotFound)

	// MINOR-11: readonly=true must mark the key read-only (not "readable").
	require.NoError(t, cfg.UpdateKeyReadability("DirectKey", true))
	err = cfg.UpdateKey("DirectKey", ptr("v2"))
	assert.ErrorIs(t, err, ErrReadOnly)

	// readonly=false must clear it again.
	require.NoError(t, cfg.UpdateKeyReadability("DirectKey", false))
	require.NoError(t, cfg.UpdateKey("DirectKey", ptr("v3")))

	got := cfg.GetConfig()
	require.Len(t, got, 1)
	assert.Equal(t, "DirectKey", got[0].Key)
	assert.Equal(t, "v3", *got[0].Value)
}

// ==========================================================================
// Test 2 — Race (MAJOR-1): concurrent RegisterCustomKeyValidator/OnUpdateKey/
// GetMandatoryKeys vs UpdateKey/OnChangeConfiguration must be -race clean.
// Bounded by a watchdog select/time.After, never a bare sleep or an unbounded
// wait, so a deadlock fails the test instead of hanging the suite.
// ==========================================================================

func TestConcurrentAccessRace(t *testing.T) {
	cfg := Config{
		Version: 1,
		Keys: []core.ConfigurationKey{
			{Key: "RaceKey", Readonly: false, Value: ptr("0")},
		},
	}
	mgr, err := NewManagerV16(cfg)
	require.NoError(t, err)

	const iterations = 200
	var wg sync.WaitGroup
	wg.Add(5)

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			mgr.RegisterCustomKeyValidator(func(k Key, v *string) bool { return true })
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_ = mgr.OnUpdateKey(Key("RaceKey"), func(v *string) error { return nil })
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_ = mgr.GetMandatoryKeys()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_ = mgr.UpdateKey(Key("RaceKey"), ptr(strconv.Itoa(i)))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			mgr.OnChangeConfiguration(&core.ChangeConfigurationRequest{Key: "RaceKey", Value: strconv.Itoa(i)})
		}
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent access did not complete within 30s — possible deadlock (e.g. OnUpdateHandler-under-lock self-deadlock, or a non-reentrant mu re-lock)")
	}
}

// ==========================================================================
// Test 3 — ISO15118 (D4a): sentinel wired into GetMandatoryKeysForProfile,
// and DefaultConfigurationFromProfiles(ISO15118ProfileName) validates.
// ==========================================================================

func TestISO15118SentinelMandatoryKeys(t *testing.T) {
	got := GetMandatoryKeysForProfile(ISO15118ProfileName)
	assert.ElementsMatch(t, MandatoryISO15118Keys, got)
}

func TestISO15118UnknownProfileIgnored(t *testing.T) {
	// GetMandatoryKeysForProfile silently ignores unknown profiles (D1/MINOR-11
	// preamble; contrast with DefaultConfigurationFromProfiles, which errors).
	got := GetMandatoryKeysForProfile("NotARealProfile")
	assert.Empty(t, got)
}

func TestISO15118DefaultsValidate(t *testing.T) {
	cfg, err := DefaultConfigurationFromProfiles(ISO15118ProfileName)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.NotEmpty(t, cfg.Keys, "DefaultConfigurationFromProfiles(ISO15118ProfileName) must include ISO15118 defaults")

	mgr, err := NewManagerV16(*cfg, ISO15118ProfileName)
	require.NoError(t, err)
	require.NotNil(t, mgr)

	// Read one known ISO15118 mandatory key back from the MANAGER (not just
	// the pre-construction cfg): proves the ISO15118 defaults actually
	// survived construction, since a bug that drops the profile's defaults
	// after validation would otherwise slip past this test.
	require.NotEmpty(t, MandatoryISO15118Keys)
	val, err := mgr.GetConfigurationValue(MandatoryISO15118Keys[0])
	require.NoError(t, err, "an ISO15118 mandatory key must be present in the constructed manager's store")
	assert.NotNil(t, val)
}

// ==========================================================================
// Test 4 — UpdateKey/handler error (D4b): a failing OnUpdateHandler is
// surfaced by UpdateKey AND the stored value is rolled back to the original.
// ==========================================================================

func TestUpdateKeyHandlerErrorRollsBack(t *testing.T) {
	const original = "original-value"
	cfg := Config{Version: 1, Keys: []core.ConfigurationKey{{Key: "RollbackKey", Readonly: false, Value: ptr(original)}}}
	mgr, err := NewManagerV16(cfg)
	require.NoError(t, err)

	handlerErr := errors.New("handler boom")
	var seen *string
	require.NoError(t, mgr.OnUpdateKey(Key("RollbackKey"), func(v *string) error {
		seen = v
		return handlerErr
	}))

	err = mgr.UpdateKey(Key("RollbackKey"), ptr("new-value"))
	require.Error(t, err, "a failing OnUpdateHandler must be surfaced by UpdateKey, not silently swallowed")
	assert.ErrorIs(t, err, handlerErr)

	// The handler must be invoked with the POST-APPLY (new) value before it
	// returns its error — proving it sees "new-value", not the pre-apply
	// original — even though the store is rolled back afterward.
	require.NotNil(t, seen, "OnUpdateHandler must be invoked with a non-nil value")
	assert.Equal(t, "new-value", *seen, "OnUpdateHandler must see the NEW value before rollback")

	stored, getErr := mgr.GetConfigurationValue(Key("RollbackKey"))
	require.NoError(t, getErr)
	require.NotNil(t, stored)
	assert.Equal(t, original, *stored, "on handler error the value must be rolled back to the ORIGINAL")
}

func TestUpdateKeySuccessNoRollback(t *testing.T) {
	cfg := Config{Version: 1, Keys: []core.ConfigurationKey{{Key: "OkKey", Readonly: false, Value: ptr("v0")}}}
	mgr, err := NewManagerV16(cfg)
	require.NoError(t, err)

	require.NoError(t, mgr.OnUpdateKey(Key("OkKey"), func(v *string) error { return nil }))
	require.NoError(t, mgr.UpdateKey(Key("OkKey"), ptr("v1")))

	stored, getErr := mgr.GetConfigurationValue(Key("OkKey"))
	require.NoError(t, getErr)
	require.NotNil(t, stored)
	assert.Equal(t, "v1", *stored)
}

// TestOnUpdateKeyNilHandlerRejected pins MAJOR-1: a nil handler is rejected at
// registration (it would otherwise be a legal map entry that panics when later
// invoked under the lock, bypassing the atomic rollback).
func TestOnUpdateKeyNilHandlerRejected(t *testing.T) {
	cfg := Config{Version: 1, Keys: []core.ConfigurationKey{{Key: "NilHandlerKey", Readonly: false, Value: ptr("v0")}}}
	mgr, err := NewManagerV16(cfg)
	require.NoError(t, err)

	err = mgr.OnUpdateKey(Key("NilHandlerKey"), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNilHandler)
}

// TestUpdateKeyEmptyKeyRejected pins MINOR-1: an empty key is rejected with
// ErrKeyCannotBeEmpty on the UpdateKey path too (consistent with OnUpdateKey),
// not the misleading ErrKeyNotFound.
func TestUpdateKeyEmptyKeyRejected(t *testing.T) {
	mgr, err := NewManagerV16(NewEmptyConfiguration())
	require.NoError(t, err)

	err = mgr.UpdateKey(Key(""), ptr("x"))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrKeyCannotBeEmpty)
}

// TestUpdateKeyHandlerPanicRollsBackAndRepanics pins MAJOR-2: a panicking
// OnUpdateHandler must not strand the just-applied value — the store is rolled
// back to the original AND the panic is re-raised (stays loud), rather than
// silently swallowed.
func TestUpdateKeyHandlerPanicRollsBackAndRepanics(t *testing.T) {
	const original = "original-value"
	cfg := Config{Version: 1, Keys: []core.ConfigurationKey{{Key: "PanicKey", Readonly: false, Value: ptr(original)}}}
	mgr, err := NewManagerV16(cfg)
	require.NoError(t, err)

	require.NoError(t, mgr.OnUpdateKey(Key("PanicKey"), func(v *string) error {
		panic("handler boom")
	}))

	require.Panics(t, func() {
		_ = mgr.UpdateKey(Key("PanicKey"), ptr("new-value"))
	}, "a panicking OnUpdateHandler must re-panic (stay loud), not be swallowed")

	// The lock must have been released (deferred Unlock survives the panic) and
	// the store rolled back to the original value.
	stored, getErr := mgr.GetConfigurationValue(Key("PanicKey"))
	require.NoError(t, getErr)
	require.NotNil(t, stored)
	assert.Equal(t, original, *stored, "a handler panic must roll the store back to the ORIGINAL value")
}

// TestOnGetConfigurationNilRequest pins MINOR-3: a nil request returns an empty
// confirmation (defensive), NOT the permissive all-keys reserved for a non-nil
// request with an empty Key list.
func TestOnGetConfigurationNilRequest(t *testing.T) {
	cfg := Config{Version: 1, Keys: []core.ConfigurationKey{{Key: "SomeKey", Readonly: false, Value: ptr("v0")}}}
	mgr, err := NewManagerV16(cfg)
	require.NoError(t, err)

	resp := mgr.OnGetConfiguration(nil)
	require.NotNil(t, resp)
	assert.Empty(t, resp.ConfigurationKey, "nil request must NOT return all keys")
	assert.Empty(t, resp.UnknownKey)
}

// ==========================================================================
// Test 5 — Deep copy (MAJOR-9 / DB2): GetConfiguration/OnGetConfiguration
// snapshots must not be mutated by a later OnChangeConfiguration; mutating the
// caller's input Config after SetConfiguration must not leak in; mandatoryKeys
// / rebootRequiredKeys must be copied in AND out.
// ==========================================================================

func TestDeepCopyGetConfigurationNotAliased(t *testing.T) {
	cfg := Config{Version: 1, Keys: []core.ConfigurationKey{{Key: "DeepCopyKey", Readonly: false, Value: ptr("v1")}}}
	mgr, err := NewManagerV16(cfg)
	require.NoError(t, err)

	keys, err := mgr.GetConfiguration()
	require.NoError(t, err)
	require.Len(t, keys, 1)
	require.NotNil(t, keys[0].Value)
	assert.Equal(t, "v1", *keys[0].Value)

	// Mutate THROUGH the returned pointer: this is the definitive non-aliasing
	// proof. A shallow copy that shares the *string with the store would leak
	// this mutation into the store; a deep copy would not. (Merely mutating the
	// store afterward and checking the snapshot is unaffected also passes for
	// an impl that reassigns key.Value = ptr("v2") instead of mutating in
	// place, so that alone would NOT be definitive.)
	*keys[0].Value = "tampered-via-snapshot"

	stored, getErr := mgr.GetConfigurationValue(Key("DeepCopyKey"))
	require.NoError(t, getErr)
	require.NotNil(t, stored)
	assert.Equal(t, "v1", *stored, "GetConfiguration must return deep-copied values; mutating the returned *string must not touch the store")
}

func TestDeepCopyOnGetConfigurationNotAliased(t *testing.T) {
	cfg := Config{Version: 1, Keys: []core.ConfigurationKey{{Key: "DeepCopyKey2", Readonly: false, Value: ptr("v1")}}}
	mgr, err := NewManagerV16(cfg)
	require.NoError(t, err)

	resp := mgr.OnGetConfiguration(&core.GetConfigurationRequest{})
	require.NotNil(t, resp)
	require.Len(t, resp.ConfigurationKey, 1)
	require.NotNil(t, resp.ConfigurationKey[0].Value)
	assert.Equal(t, "v1", *resp.ConfigurationKey[0].Value)

	// Mutate THROUGH the returned pointer: the definitive non-aliasing proof
	// (see TestDeepCopyGetConfigurationNotAliased for why the earlier-snapshot
	// pattern alone is not definitive).
	*resp.ConfigurationKey[0].Value = "tampered-via-snapshot"

	stored, getErr := mgr.GetConfigurationValue(Key("DeepCopyKey2"))
	require.NoError(t, getErr)
	require.NotNil(t, stored)
	assert.Equal(t, "v1", *stored, "OnGetConfiguration must return deep-copied ConfigurationKey values; mutating the returned *string must not touch the store")
}

func TestDeepCopySetConfigurationInputNotAliased(t *testing.T) {
	valuePtr := ptr("initial")
	input := Config{Version: 1, Keys: []core.ConfigurationKey{{Key: "SetCfgKey", Readonly: false, Value: valuePtr}}}
	mgr, err := NewManagerV16(NewEmptyConfiguration())
	require.NoError(t, err)

	require.NoError(t, mgr.SetConfiguration(input))

	// Mutate the caller's slice/pointer after the call returns.
	input.Keys[0].Value = ptr("mutated-via-slice")
	*valuePtr = "mutated-via-original-pointer"
	input.Keys = append(input.Keys, core.ConfigurationKey{Key: "Injected", Readonly: false, Value: ptr("x")})

	stored, getErr := mgr.GetConfigurationValue(Key("SetCfgKey"))
	require.NoError(t, getErr)
	require.NotNil(t, stored)
	assert.Equal(t, "initial", *stored, "SetConfiguration must deep-copy the incoming Config; caller mutation must not leak in")

	keys, err := mgr.GetConfiguration()
	require.NoError(t, err)
	for _, k := range keys {
		assert.NotEqual(t, "Injected", k.Key, "appending to the caller's slice after SetConfiguration must not leak a new key into the store")
	}
}

func TestDeepCopyMandatoryKeysInOut(t *testing.T) {
	mgr, err := NewManagerV16(NewEmptyConfiguration())
	require.NoError(t, err)

	input := []Key{Key("A"), Key("B")}
	require.NoError(t, mgr.SetMandatoryKeys(input))

	// Mutate the caller's slice after the call: must not affect stored state.
	input[0] = Key("MUTATED-IN")

	got := mgr.GetMandatoryKeys()
	assert.Contains(t, got, Key("A"))
	assert.NotContains(t, got, Key("MUTATED-IN"))

	// The returned slice must itself be a copy: mutating it must not affect
	// the manager's internal state on a subsequent read.
	if len(got) > 0 {
		got[0] = Key("MUTATED-OUT")
	}
	got2 := mgr.GetMandatoryKeys()
	assert.NotContains(t, got2, Key("MUTATED-OUT"))
}

func TestDeepCopyRebootRequiredKeysInOut(t *testing.T) {
	mgr, err := NewManagerV16(NewEmptyConfiguration())
	require.NoError(t, err)

	input := []Key{Key("RebootA")}
	mgr.SetRebootRequiredKeys(input)
	// Mutate the caller's slice after the call.
	input[0] = Key("MUTATED")

	cfg := Config{Version: 1, Keys: []core.ConfigurationKey{{Key: "RebootA", Readonly: false, Value: ptr("v0")}}}
	require.NoError(t, mgr.SetConfiguration(cfg))

	// Behavioral proof of copy-in: a change to the key under its ORIGINAL name
	// ("RebootA") must still be recognized as reboot-required, i.e. the later
	// mutation of the caller's slice element to "MUTATED" did not retroactively
	// rename the stored reboot-required entry.
	conf := mgr.OnChangeConfiguration(&core.ChangeConfigurationRequest{Key: "RebootA", Value: "v1"})
	require.NotNil(t, conf)
	assert.Equal(t, core.ConfigurationStatusRebootRequired, conf.Status)
}

// ==========================================================================
// Test 6 — OnGetConfiguration: empty/nil req -> all; subset -> known/unknown
// split; bogus -> UnknownKey; read-only -> Readonly:true; wrong-case key ->
// UnknownKey (case-sensitive/byte-exact matching, MINOR-3).
// ==========================================================================

func TestOnGetConfiguration(t *testing.T) {
	cfg := Config{
		Version: 1,
		Keys: []core.ConfigurationKey{
			{Key: "HeartbeatInterval", Readonly: false, Value: ptr("60")},
			{Key: "ConnectorPhaseRotation", Readonly: true, Value: ptr("Unknown")},
		},
	}
	mgr, err := NewManagerV16(cfg)
	require.NoError(t, err)

	t.Run("empty request Key returns all keys", func(t *testing.T) {
		resp := mgr.OnGetConfiguration(&core.GetConfigurationRequest{Key: []string{}})
		require.NotNil(t, resp)
		assert.Len(t, resp.ConfigurationKey, 2)
		assert.Empty(t, resp.UnknownKey)
	})

	t.Run("nil request Key returns all keys", func(t *testing.T) {
		resp := mgr.OnGetConfiguration(&core.GetConfigurationRequest{Key: nil})
		require.NotNil(t, resp)
		assert.Len(t, resp.ConfigurationKey, 2)
		assert.Empty(t, resp.UnknownKey)
	})

	t.Run("subset splits known and unknown", func(t *testing.T) {
		resp := mgr.OnGetConfiguration(&core.GetConfigurationRequest{Key: []string{"HeartbeatInterval", "Bogus"}})
		require.NotNil(t, resp)
		require.Len(t, resp.ConfigurationKey, 1)
		assert.Equal(t, "HeartbeatInterval", resp.ConfigurationKey[0].Key)
		require.Len(t, resp.UnknownKey, 1)
		// Order-agnostic: the spec does not mandate request-key ordering in the
		// response, and a single-element slice's only element is still "Bogus".
		assert.Contains(t, resp.UnknownKey, "Bogus")
	})

	t.Run("bogus-only request returns UnknownKey", func(t *testing.T) {
		resp := mgr.OnGetConfiguration(&core.GetConfigurationRequest{Key: []string{"TotallyBogus"}})
		require.NotNil(t, resp)
		assert.Empty(t, resp.ConfigurationKey)
		require.Len(t, resp.UnknownKey, 1)
		assert.Equal(t, "TotallyBogus", resp.UnknownKey[0])
	})

	t.Run("read-only key reports Readonly true", func(t *testing.T) {
		resp := mgr.OnGetConfiguration(&core.GetConfigurationRequest{Key: []string{"ConnectorPhaseRotation"}})
		require.NotNil(t, resp)
		require.Len(t, resp.ConfigurationKey, 1)
		assert.True(t, resp.ConfigurationKey[0].Readonly)
	})

	t.Run("wrong-case key is UnknownKey (case-sensitive, MINOR-3)", func(t *testing.T) {
		resp := mgr.OnGetConfiguration(&core.GetConfigurationRequest{Key: []string{"heartbeatinterval"}})
		require.NotNil(t, resp)
		assert.Empty(t, resp.ConfigurationKey)
		require.Len(t, resp.UnknownKey, 1)
		assert.Equal(t, "heartbeatinterval", resp.UnknownKey[0])
	})
}

// ==========================================================================
// Test 7 — OnChangeConfiguration status mapping.
// ==========================================================================

func TestOnChangeConfigurationStatusMapping(t *testing.T) {
	t.Run("unknown key -> NotSupported", func(t *testing.T) {
		mgr, err := NewManagerV16(NewEmptyConfiguration())
		require.NoError(t, err)
		conf := mgr.OnChangeConfiguration(&core.ChangeConfigurationRequest{Key: "DoesNotExist", Value: "x"})
		require.NotNil(t, conf)
		assert.Equal(t, core.ConfigurationStatusNotSupported, conf.Status)
	})

	t.Run("wrong-case key -> NotSupported (case-sensitive, MINOR-3)", func(t *testing.T) {
		cfg := Config{Version: 1, Keys: []core.ConfigurationKey{{Key: "HeartbeatInterval", Readonly: false, Value: ptr("60")}}}
		mgr, err := NewManagerV16(cfg)
		require.NoError(t, err)
		conf := mgr.OnChangeConfiguration(&core.ChangeConfigurationRequest{Key: "heartbeatinterval", Value: "x"})
		require.NotNil(t, conf)
		assert.Equal(t, core.ConfigurationStatusNotSupported, conf.Status)
	})

	t.Run("empty key -> Rejected (MINOR-1)", func(t *testing.T) {
		// An empty key is malformed, not "unsupported": OnChangeConfiguration
		// maps ErrKeyCannotBeEmpty to Rejected (not NotSupported). Not reachable
		// via the wire — ChangeConfigurationRequest.Key is `validate:"required"`
		// — but pinned here to lock the deliberate mapping.
		mgr, err := NewManagerV16(NewEmptyConfiguration())
		require.NoError(t, err)
		conf := mgr.OnChangeConfiguration(&core.ChangeConfigurationRequest{Key: "", Value: "x"})
		require.NotNil(t, conf)
		assert.Equal(t, core.ConfigurationStatusRejected, conf.Status)
	})

	t.Run("read-only key -> Rejected, value unchanged", func(t *testing.T) {
		cfg := Config{Version: 1, Keys: []core.ConfigurationKey{{Key: "ROKey", Readonly: true, Value: ptr("v0")}}}
		mgr, err := NewManagerV16(cfg)
		require.NoError(t, err)
		conf := mgr.OnChangeConfiguration(&core.ChangeConfigurationRequest{Key: "ROKey", Value: "v1"})
		require.NotNil(t, conf)
		assert.Equal(t, core.ConfigurationStatusRejected, conf.Status)

		stored, getErr := mgr.GetConfigurationValue(Key("ROKey"))
		require.NoError(t, getErr)
		require.NotNil(t, stored)
		assert.Equal(t, "v0", *stored)
	})

	t.Run("custom validator returns false -> Rejected, value unchanged", func(t *testing.T) {
		cfg := Config{Version: 1, Keys: []core.ConfigurationKey{{Key: "ValidatedKey", Readonly: false, Value: ptr("v0")}}}
		mgr, err := NewManagerV16(cfg)
		require.NoError(t, err)
		mgr.RegisterCustomKeyValidator(func(k Key, v *string) bool { return false })
		conf := mgr.OnChangeConfiguration(&core.ChangeConfigurationRequest{Key: "ValidatedKey", Value: "v1"})
		require.NotNil(t, conf)
		assert.Equal(t, core.ConfigurationStatusRejected, conf.Status)

		stored, getErr := mgr.GetConfigurationValue(Key("ValidatedKey"))
		require.NoError(t, getErr)
		require.NotNil(t, stored)
		assert.Equal(t, "v0", *stored)
	})

	t.Run("reboot-required key -> RebootRequired, value IS applied", func(t *testing.T) {
		cfg := Config{Version: 1, Keys: []core.ConfigurationKey{{Key: "RebootKey", Readonly: false, Value: ptr("v0")}}}
		mgr, err := NewManagerV16(cfg)
		require.NoError(t, err)
		mgr.SetRebootRequiredKeys([]Key{Key("RebootKey")})
		conf := mgr.OnChangeConfiguration(&core.ChangeConfigurationRequest{Key: "RebootKey", Value: "v1"})
		require.NotNil(t, conf)
		assert.Equal(t, core.ConfigurationStatusRebootRequired, conf.Status)

		stored, getErr := mgr.GetConfigurationValue(Key("RebootKey"))
		require.NoError(t, getErr)
		require.NotNil(t, stored)
		assert.Equal(t, "v1", *stored, "RebootRequired still applies the value")
	})

	t.Run("handler error -> Rejected, value rolled back to ORIGINAL", func(t *testing.T) {
		cfg := Config{Version: 1, Keys: []core.ConfigurationKey{{Key: "HandlerKey", Readonly: false, Value: ptr("orig")}}}
		mgr, err := NewManagerV16(cfg)
		require.NoError(t, err)
		require.NoError(t, mgr.OnUpdateKey(Key("HandlerKey"), func(v *string) error {
			return errors.New("handler boom")
		}))
		conf := mgr.OnChangeConfiguration(&core.ChangeConfigurationRequest{Key: "HandlerKey", Value: "new"})
		require.NotNil(t, conf)
		assert.Equal(t, core.ConfigurationStatusRejected, conf.Status)

		stored, getErr := mgr.GetConfigurationValue(Key("HandlerKey"))
		require.NoError(t, getErr)
		require.NotNil(t, stored)
		assert.Equal(t, "orig", *stored, "a failing handler must roll back to the ORIGINAL value")
	})

	t.Run("success -> Accepted, handler fired, value stored", func(t *testing.T) {
		cfg := Config{Version: 1, Keys: []core.ConfigurationKey{{Key: "SuccessKey", Readonly: false, Value: ptr("orig")}}}
		mgr, err := NewManagerV16(cfg)
		require.NoError(t, err)
		var handlerCalled bool
		var handlerValue *string
		require.NoError(t, mgr.OnUpdateKey(Key("SuccessKey"), func(v *string) error {
			handlerCalled = true
			handlerValue = v
			return nil
		}))
		conf := mgr.OnChangeConfiguration(&core.ChangeConfigurationRequest{Key: "SuccessKey", Value: "updated"})
		require.NotNil(t, conf)
		assert.Equal(t, core.ConfigurationStatusAccepted, conf.Status)
		assert.True(t, handlerCalled, "OnUpdateHandler must fire on a successful change")
		require.NotNil(t, handlerValue)
		assert.Equal(t, "updated", *handlerValue)

		stored, getErr := mgr.GetConfigurationValue(Key("SuccessKey"))
		require.NoError(t, getErr)
		require.NotNil(t, stored)
		assert.Equal(t, "updated", *stored)
	})

	t.Run(`Value:"" stores ptr("") (S5)`, func(t *testing.T) {
		cfg := Config{Version: 1, Keys: []core.ConfigurationKey{{Key: "EmptyValKey", Readonly: false, Value: ptr("orig")}}}
		mgr, err := NewManagerV16(cfg)
		require.NoError(t, err)
		conf := mgr.OnChangeConfiguration(&core.ChangeConfigurationRequest{Key: "EmptyValKey", Value: ""})
		require.NotNil(t, conf)
		assert.Equal(t, core.ConfigurationStatusAccepted, conf.Status)

		stored, getErr := mgr.GetConfigurationValue(Key("EmptyValKey"))
		require.NoError(t, getErr)
		require.NotNil(t, stored, `an empty Value must be stored as ptr(""), not left nil`)
		assert.Equal(t, "", *stored)
	})
}

// OnChangeConfiguration's signature deliberately returns no error (see spec):
// this is a compile-time pin on that fact.
func TestOnChangeConfigurationNeverReturnsError(t *testing.T) {
	mgr, err := NewManagerV16(NewEmptyConfiguration())
	require.NoError(t, err)
	var conf *core.ChangeConfigurationConfirmation = mgr.OnChangeConfiguration(&core.ChangeConfigurationRequest{Key: "x", Value: "y"})
	require.NotNil(t, conf)
}

// ==========================================================================
// Exported errors (DeepSeek DB9): part of the public API surface so consumers
// can errors.Is() against them.
// ==========================================================================

func TestExportedErrorsAreDistinct(t *testing.T) {
	assert.NotNil(t, ErrKeyNotFound)
	assert.NotNil(t, ErrReadOnly)
	assert.NotNil(t, ErrKeyCannotBeEmpty)
	assert.NotErrorIs(t, ErrKeyNotFound, ErrReadOnly)
	assert.NotErrorIs(t, ErrReadOnly, ErrKeyCannotBeEmpty)
}

func TestOnUpdateKeyEmptyKeyRejected(t *testing.T) {
	mgr, err := NewManagerV16(NewEmptyConfiguration())
	require.NoError(t, err)
	err = mgr.OnUpdateKey(Key(""), func(v *string) error { return nil })
	assert.ErrorIs(t, err, ErrKeyCannotBeEmpty)
}
