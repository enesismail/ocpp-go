package ocpp16_test

// RED-FIRST TDD facade e2e test (spec Test 8, tasks/p3-config-store.md) for
// the not-yet-implemented ocpp1.6/configmanager package.
//
// It proves the two documented one-line delegations
//
//	func (h *myHandler) OnGetConfiguration(req *core.GetConfigurationRequest) (*core.GetConfigurationConfirmation, error) {
//	    return h.cfg.OnGetConfiguration(req), nil
//	}
//	func (h *myHandler) OnChangeConfiguration(req *core.ChangeConfigurationRequest) (*core.ChangeConfigurationConfirmation, error) {
//	    return h.cfg.OnChangeConfiguration(req), nil
//	}
//
// compile and round-trip through a REAL ocpp16.ChargePoint/CentralSystem pair,
// using the existing mock-websocket harness from ocpp16_test.go (there is no
// ephemeral-port helper in this suite; the ws.Start(0)/Addr() helpers from PR
// #25 live in the ws package tests, not here — per spec F2, do not invent one
// here).
//
// Until ocpp1.6/configmanager exists, this file fails to compile (unresolved
// import + undefined configmanager.Config / configmanager.ManagerV16 /
// configmanager.NewManagerV16 / configmanager.Key). That is the expected red.

import (
	"fmt"
	"time"

	"github.com/enesismail/ocpp-go/ocpp1.6/configmanager"
	"github.com/enesismail/ocpp-go/ocpp1.6/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// configManagerCoreListener is a real core.ChargePointHandler whose two
// config-related methods are one-line delegations to a
// configmanager.ManagerV16, exactly as documented in the spec's "Consumer
// usage" snippet. The other 7 core.ChargePointHandler methods are inherited,
// unset, from the embedded mock (unused/untriggered in these scenarios,
// mirroring how other facade e2e tests in this package only .On() the
// methods they exercise).
type configManagerCoreListener struct {
	*MockChargePointCoreListener
	cfgMgr *configmanager.ManagerV16
}

func (h *configManagerCoreListener) OnGetConfiguration(req *core.GetConfigurationRequest) (*core.GetConfigurationConfirmation, error) {
	return h.cfgMgr.OnGetConfiguration(req), nil
}

func (h *configManagerCoreListener) OnChangeConfiguration(req *core.ChangeConfigurationRequest) (*core.ChangeConfigurationConfirmation, error) {
	return h.cfgMgr.OnChangeConfiguration(req), nil
}

func strp(s string) *string { return &s }

func (suite *OcppV16TestSuite) TestConfigManagerFacadeE2EGetConfiguration() {
	t := suite.T()
	wsId := "test_id"
	messageId := defaultMessageId
	wsUrl := "someUrl"

	cfg := configmanager.Config{
		Version: 1,
		Keys: []core.ConfigurationKey{
			{Key: "HeartbeatInterval", Readonly: false, Value: strp("60")},
		},
	}
	cfgMgr, err := configmanager.NewManagerV16(cfg)
	require.NoError(t, err)
	handler := &configManagerCoreListener{MockChargePointCoreListener: &MockChargePointCoreListener{}, cfgMgr: cfgMgr}

	requestJson := fmt.Sprintf(`[2,"%v","%v",{}]`, messageId, core.GetConfigurationFeatureName)
	channel := NewMockWebSocket(wsId)

	setupDefaultCentralSystemHandlers(suite, nil, expectedCentralSystemOptions{clientId: wsId, rawWrittenMessage: []byte(requestJson), forwardWrittenMessage: true})
	setupDefaultChargePointHandlers(suite, handler, expectedChargePointOptions{serverUrl: wsUrl, clientId: wsId, createChannelOnStart: true, channel: channel, forwardWrittenMessage: true})

	// Run Test
	suite.centralSystem.Start(8887, "somePath")
	err = suite.chargePoint.Start(wsUrl)
	require.NoError(t, err)

	resultChannel := make(chan bool, 1)
	err = suite.centralSystem.GetConfiguration(wsId, func(confirmation *core.GetConfigurationConfirmation, err error) {
		require.NoError(t, err)
		require.NotNil(t, confirmation)
		require.Len(t, confirmation.ConfigurationKey, 1)
		assert.Equal(t, "HeartbeatInterval", confirmation.ConfigurationKey[0].Key)
		require.NotNil(t, confirmation.ConfigurationKey[0].Value)
		assert.Equal(t, "60", *confirmation.ConfigurationKey[0].Value)
		assert.Empty(t, confirmation.UnknownKey)
		resultChannel <- true
	}, nil)
	require.NoError(t, err)

	select {
	case result := <-resultChannel:
		assert.True(t, result)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for GetConfiguration round-trip through the facade")
	}
}

func (suite *OcppV16TestSuite) TestConfigManagerFacadeE2EChangeConfiguration() {
	t := suite.T()
	wsId := "test_id"
	messageId := defaultMessageId
	wsUrl := "someUrl"
	key := "HeartbeatInterval"

	cfg := configmanager.Config{
		Version: 1,
		Keys: []core.ConfigurationKey{
			{Key: key, Readonly: false, Value: strp("60")},
		},
	}
	cfgMgr, err := configmanager.NewManagerV16(cfg)
	require.NoError(t, err)
	handler := &configManagerCoreListener{MockChargePointCoreListener: &MockChargePointCoreListener{}, cfgMgr: cfgMgr}

	requestJson := fmt.Sprintf(`[2,"%v","%v",{"key":"%v","value":"120"}]`, messageId, core.ChangeConfigurationFeatureName, key)
	channel := NewMockWebSocket(wsId)

	setupDefaultCentralSystemHandlers(suite, nil, expectedCentralSystemOptions{clientId: wsId, rawWrittenMessage: []byte(requestJson), forwardWrittenMessage: true})
	setupDefaultChargePointHandlers(suite, handler, expectedChargePointOptions{serverUrl: wsUrl, clientId: wsId, createChannelOnStart: true, channel: channel, forwardWrittenMessage: true})

	// Run Test
	suite.centralSystem.Start(8887, "somePath")
	err = suite.chargePoint.Start(wsUrl)
	require.NoError(t, err)

	resultChannel := make(chan bool, 1)
	err = suite.centralSystem.ChangeConfiguration(wsId, func(confirmation *core.ChangeConfigurationConfirmation, err error) {
		require.NoError(t, err)
		require.NotNil(t, confirmation)
		assert.Equal(t, core.ConfigurationStatusAccepted, confirmation.Status)
		resultChannel <- true
	}, key, "120")
	require.NoError(t, err)

	select {
	case result := <-resultChannel:
		assert.True(t, result)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for ChangeConfiguration round-trip through the facade")
	}

	// Proves the delegation actually reached the ManagerV16's store, not just
	// that a status came back.
	stored, getErr := cfgMgr.GetConfigurationValue(configmanager.Key(key))
	require.NoError(t, getErr)
	require.NotNil(t, stored)
	assert.Equal(t, "120", *stored)
}

func (suite *OcppV16TestSuite) TestConfigManagerFacadeE2EChangeConfigurationUnknownKeyNotSupported() {
	t := suite.T()
	wsId := "test_id"
	messageId := defaultMessageId
	wsUrl := "someUrl"

	cfgMgr, err := configmanager.NewManagerV16(configmanager.NewEmptyConfiguration())
	require.NoError(t, err)
	handler := &configManagerCoreListener{MockChargePointCoreListener: &MockChargePointCoreListener{}, cfgMgr: cfgMgr}

	requestJson := fmt.Sprintf(`[2,"%v","%v",{"key":"%v","value":"%v"}]`, messageId, core.ChangeConfigurationFeatureName, "NoSuchKey", "x")
	channel := NewMockWebSocket(wsId)

	setupDefaultCentralSystemHandlers(suite, nil, expectedCentralSystemOptions{clientId: wsId, rawWrittenMessage: []byte(requestJson), forwardWrittenMessage: true})
	setupDefaultChargePointHandlers(suite, handler, expectedChargePointOptions{serverUrl: wsUrl, clientId: wsId, createChannelOnStart: true, channel: channel, forwardWrittenMessage: true})

	suite.centralSystem.Start(8887, "somePath")
	err = suite.chargePoint.Start(wsUrl)
	require.NoError(t, err)

	resultChannel := make(chan bool, 1)
	err = suite.centralSystem.ChangeConfiguration(wsId, func(confirmation *core.ChangeConfigurationConfirmation, err error) {
		require.NoError(t, err)
		require.NotNil(t, confirmation)
		assert.Equal(t, core.ConfigurationStatusNotSupported, confirmation.Status)
		resultChannel <- true
	}, "NoSuchKey", "x")
	require.NoError(t, err)

	select {
	case result := <-resultChannel:
		assert.True(t, result)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for ChangeConfiguration round-trip through the facade")
	}
}
