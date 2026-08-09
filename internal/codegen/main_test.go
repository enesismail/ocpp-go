package main

import (
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// TestDumpIRCommandDispatchesThroughMarshalIR exercises the command the way a
// caller does — through run, in this process, rather than through a built
// binary — and pins where -dump-ir sends the work. A -dump-ir wired to a
// second, command-local rendering (or to nothing at all) fails here, and the
// golden dump every other test compares against would then be pinning bytes
// the command never writes.
//
// Dispatch is proved by replacing the marshalIR seam with a function of this
// test's own that reports a sentinel, and requiring run to surface exactly
// that sentinel: only a command that called the seam can produce it. That
// argument does not depend on what the real MarshalIR does, so it still holds
// the day MarshalIR is implemented and starts returning a nil error — where
// an assertion reading the error chain for MarshalIR's "not implemented"
// prefix would go quiet and let a bypassing -dump-ir through.
func TestDumpIRCommandDispatchesThroughMarshalIR(t *testing.T) {
	renderingReached := errors.New("rendering seam reached")
	realMarshalIR := marshalIR
	// Restoring on the way out as well as inline below keeps the swap from
	// escaping this test if an assertion between the two ends it early.
	t.Cleanup(func() { marshalIR = realMarshalIR })
	marshalIR = func(IR) ([]byte, error) { return nil, renderingReached }

	if err := run([]string{"-manifest", fixturePath("manifests", "happy_path.yaml"), "-dump-ir", t.TempDir()}, io.Discard); !errors.Is(err, renderingReached) {
		t.Fatalf("-dump-ir did not dispatch through the rendering seam: %+v", err)
	}
	marshalIR = realMarshalIR

	// With the real seam back in place, the command's failure today still
	// travels back through MarshalIR: the "marshal IR" prefix is that
	// function's own, the only part of the error chain that names it.
	err := run([]string{"-manifest", fixturePath("manifests", "happy_path.yaml"), "-dump-ir", t.TempDir()}, io.Discard)
	if err != nil && !strings.Contains(err.Error(), "marshal IR") {
		t.Fatalf("-dump-ir did not dispatch through MarshalIR: %+v", err)
	}
	requireImplemented(t, "-dump-ir command", err)
}

// TestUnknownFlagIsRejected covers the command's own argument handling rather
// than a stage still to be written: run parses its flags for real today, so
// an unrecognized flag is answered by a real error now and this test passes
// where the rest of this package's tests are red. It is here to keep that
// answer from being softened later into an ignored argument — a mistyped
// -dump-it would otherwise run the whole pipeline, write nothing, and exit
// zero, which reads exactly like a successful run that had nothing to do.
//
// The mutually-exclusive-flags check below belongs here for the same
// reason: -dump-ir and -overrides-report are both real argument-surface
// behavior today (see run's own dispatch, checked before either output
// mode's still-unimplemented body runs), not a stage waiting on the rest of
// the pipeline, so it is exercised alongside the unknown-flag case rather
// than through a stub. This makes it internal/codegen's second documented
// passing assertion, alongside this same test's unknown-flag assertion
// above — nothing else in this package is expected to pass today.
func TestUnknownFlagIsRejected(t *testing.T) {
	err := run([]string{"-dump-it", t.TempDir()}, io.Discard)
	requireExpectedHardFailureContains(t, "unknown flag check", err, "dump-it")

	// The correctly-spelled flag is accepted by that same parse, so the error
	// above is this one unknown flag rather than an argument surface that
	// rejects everything handed to it.
	accepted := run([]string{"-dump-ir", t.TempDir()}, io.Discard)
	if accepted != nil && strings.Contains(accepted.Error(), "parse arguments") {
		t.Fatalf("the argument surface rejected its own -dump-ir flag: %+v", accepted)
	}

	// -dump-ir and -overrides-report each name a distinct output mode; the
	// command has never supported producing both from one invocation, so
	// passing both together is rejected up front rather than silently
	// honoring one and ignoring the other.
	mutuallyExclusive := run([]string{"-dump-ir", t.TempDir(), "-overrides-report", filepath.Join(t.TempDir(), "report.txt")}, io.Discard)
	requireExpectedHardFailureContains(t, "mutually exclusive output flags", mutuallyExclusive, "-dump-ir", "-overrides-report")
}
