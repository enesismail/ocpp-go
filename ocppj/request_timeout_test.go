package ocppj

import (
	"errors"
	"testing"

	"github.com/enesismail/ocpp-go/ocpp"
)

// A dispatcher request-timeout must be classifiable via errors.Is against the
// sentinel, and must NOT collide with a plain server GenericError CALLERROR.
func TestRequestTimeoutSentinel_DiscriminatesFromCALLERROR(t *testing.T) {
	timeout := newRequestTimeoutError("msg-1")
	if !errors.Is(timeout, ErrRequestTimeout) {
		t.Fatal("a dispatcher request-timeout must match ErrRequestTimeout")
	}
	// a real server CALLERROR carries GenericError but no Marker
	callErr := ocpp.NewError(GenericError, "boom", "msg-1")
	if errors.Is(callErr, ErrRequestTimeout) {
		t.Fatal("a server GenericError CALLERROR must NOT match the timeout sentinel")
	}
}

// An untagged Error must never accidentally match a marked target.
func TestErrorIs_EmptyMarkerNeverMatches(t *testing.T) {
	if errors.Is(&ocpp.Error{}, ErrRequestTimeout) {
		t.Fatal("an Error with no Marker must not match the timeout sentinel")
	}
}
