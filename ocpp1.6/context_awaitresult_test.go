package ocpp16

// PR-E1c (tasks/e1c-context-aware-send.md) RED-FIRST white-box unit test.
//
// Test 8 — prefer-response fast-path (spec test 8, 1.6 facade).
//
// This file lives in `package ocpp16` (not `ocpp16_test`) so it can reach
// the unexported chargePoint.awaitCtxResult helper and the promoted
// asyncResponse package-level type.
//
// RED-FIRST discipline: these tests reference the not-yet-existing
// awaitCtxResult helper and the package-level asyncResponse type. Against
// today's codebase:
//   - asyncResponse is a local type inside SendRequest, not package-level
//   - awaitCtxResult does not exist
//   - The prefer-response fast-path (non-blocking pre-check) does not exist
//
// This file is EXPECTED to fail compilation — that IS the intended red
// state pinning the PR-E1c contract (C4 testability seam).
//
// Spec test implemented: 8.

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/enesismail/ocpp-go/ocppj"
)

// e1cTestResponse is a minimal ocpp.Response for white-box testing.
type e1cTestResponse struct{}

func (e1cTestResponse) GetFeatureName() string { return "E1cTest" }

// TestE1cAwaitCtxResultPreLoadedResponseWinsOverCanceledCtx verifies that
// when asyncResponseC already has a successful response, awaitCtxResult
// returns it even if the ctx is already canceled — the non-blocking
// pre-check (the "prefer-response fast-path") wins.
func TestE1cAwaitCtxResultPreLoadedResponseWinsOverCanceledCtx(t *testing.T) {
	cp := &chargePoint{
		stopC: make(chan struct{}, 1),
	}
	preloadedResp := e1cTestResponse{}

	// A plain 3-arm select with BOTH a buffered response and a closed ctx.Done()
	// picks uniformly at random, so a single shot would pass ~50% even without
	// the required non-blocking pre-check. Iterate so a missing pre-check fails
	// with probability ~1-2^-N (deterministic guard for the C4 fast-path seam).
	for i := 0; i < 100; i++ {
		asyncResponseC := make(chan asyncResponse, 1)
		asyncResponseC <- asyncResponse{r: preloadedResp, e: nil}

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // already-canceled ctx

		resp, err := cp.awaitCtxResult(ctx, "E1cTest", asyncResponseC, cp.stopC)
		require.NoErrorf(t, err, "iter %d: pre-loaded response must win over canceled ctx", i)
		require.Equalf(t, preloadedResp, resp, "iter %d: must return the pre-loaded response", i)
	}
}

// TestE1cAwaitCtxResultEmptyChannelCanceledCtxReturnsError verifies that
// with an empty asyncResponseC and an already-canceled ctx, awaitCtxResult
// returns an error matching both ErrRequestCanceled and context.Canceled.
func TestE1cAwaitCtxResultEmptyChannelCanceledCtxReturnsError(t *testing.T) {
	cp := &chargePoint{
		stopC: make(chan struct{}, 1),
	}

	asyncResponseC := make(chan asyncResponse, 1)
	// Leave empty — no response pre-loaded.

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	resp, err := cp.awaitCtxResult(ctx, "E1cTest", asyncResponseC, cp.stopC)
	assert.Error(t, err, "canceled ctx must produce an error when no response is pre-loaded")
	assert.Nil(t, resp, "no response expected when ctx is canceled and channel is empty")
	assert.True(t, errors.Is(err, context.Canceled),
		"error must match context.Canceled, got %v", err)
	assert.True(t, errors.Is(err, ocppj.ErrRequestCanceled),
		"error must match ErrRequestCanceled, got %v", err)
}
