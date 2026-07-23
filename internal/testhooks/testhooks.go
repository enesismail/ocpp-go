// Package testhooks holds optional, nil-by-default observation seams used only
// by the E2-0 cross-delivery regression tests to pin a specific goroutine
// interleaving. It lives under internal/ so it is never part of the public API,
// while remaining importable both by the production facades that read the seams
// and by their black-box test packages (which sit in a separate directory and
// therefore cannot see a facade-local test file).
//
// This package must stay a leaf: `ocpp` and `ws` (its only imports) must never
// import it back, or the production facades that read these seams would cycle.
package testhooks

import (
	"github.com/enesismail/ocpp-go/ocpp"
	"github.com/enesismail/ocpp-go/ws"
)

// ChargePointResponse, when non-nil, is invoked at the top of the OCPP 1.6
// charge-point response-handling closure — after the ocppj layer has completed
// the pending request but before the facade routes the response to a callback.
// It is nil in all normal operation and is set only by tests. Not safe for
// concurrent mutation: set it before traffic starts and clear it (nil) when done.
var ChargePointResponse func(confirmation ocpp.Response, requestId string)

// CentralSystemResponse is the server-side counterpart of ChargePointResponse,
// invoked at the top of the OCPP 1.6 central-system response-handling closure.
// Same constraints apply.
var CentralSystemResponse func(client ws.Channel, response ocpp.Response, requestId string)
