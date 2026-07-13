package ocpp

// PR-E1a (tasks/e1-context-aware-send.md, "## PR-E1a — completion-ownership +
// readiness rework") RED-FIRST test: ocpp.Error.Unwrap.
//
// This test verifies that ocpp.Error gains an Unwrap() method that returns the
// Cause field, enabling errors.Is to traverse the error chain into the cause.
//
// RED-FIRST discipline: against today's codebase, ocpp.Error has no Cause field
// and no Unwrap method, so this file is EXPECTED to fail compilation.
//
// Finding 7 (code review): the original version of this file only exercised
// Unwrap() directly and never proved the receiver is a VALUE receiver
// specifically — a pointer-receiver Unwrap would also have compiled and
// passed every assertion here. Fixed by (a) a compile-time assertion against
// an Error VALUE (not &Error{}), which only compiles for a value-receiver
// Unwrap, and (b) exercising errors.Is on BOTH an Error value and a *Error
// for a wrapped context.Canceled, matching the existing value-receiver Is
// method's contract (promoted into *Error's method set either way, but only
// a value receiver satisfies interface{ Unwrap() error } = Error{}).

import (
	"context"
	"errors"
	"testing"
)

// Compile-time assertion: Unwrap must be a VALUE receiver, promoted into
// *Error's method set — matching the existing value-receiver Is method
// (ocpp.go: `func (err Error) Is(target error) bool`). A pointer-receiver
// Unwrap would satisfy `interface{ Unwrap() error } = &Error{}` but NOT
// `= Error{}`; this line pins the value-receiver contract specifically.
var _ interface{ Unwrap() error } = Error{}

// TestE1aErrorUnwrap verifies that an ocpp.Error with a Cause set returns that
// cause from its Unwrap method, so errors.Is can traverse into it.
func TestE1aErrorUnwrap(t *testing.T) {
	// PR-E1a: ocpp.Error gains a Cause field and an Unwrap method.
	// Unwrap is a value receiver (matching Is), promoted into *Error's method set.
	cause := context.Canceled
	err := Error{
		Code:        "GenericError",
		Description: "Request canceled",
		MessageId:   "test-msg",
		Cause:       cause,
	}

	unwrapped := err.Unwrap()
	if unwrapped != cause {
		t.Fatalf("Unwrap() = %v, want %v", unwrapped, cause)
	}
}

// TestE1aErrorIsUnwrapsToContextCanceledValueReceiver exercises errors.Is on
// an Error VALUE (not a pointer) wrapping context.Canceled, proving errors.Is
// can traverse into Cause via the value-receiver Unwrap without ever going
// through a pointer.
func TestE1aErrorIsUnwrapsToContextCanceledValueReceiver(t *testing.T) {
	err := Error{Code: "GenericError", Description: "canceled", MessageId: "x", Cause: context.Canceled}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("errors.Is(Error value, context.Canceled) = false, want true")
	}
}

// TestE1aErrorIsUnwrapsToContextCanceledPointerReceiver exercises errors.Is on
// a *Error wrapping context.Canceled, proving the value-receiver Unwrap is
// correctly promoted into the pointer method set too — the shape every
// sentinel in ocppj actually uses (ErrRequestTimeout, ErrDispatcherStopped,
// ErrLocalTransport, and PR-E1a's ErrRequestCanceled are all *ocpp.Error).
func TestE1aErrorIsUnwrapsToContextCanceledPointerReceiver(t *testing.T) {
	err := &Error{Code: "GenericError", Description: "canceled", MessageId: "x", Cause: context.Canceled}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("errors.Is(*Error, context.Canceled) = false, want true")
	}
}
