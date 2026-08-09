package main

import (
	"bytes"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
)

// formatSource runs src through go/format, wrapping a parse/format failure
// with the offending source so a bug in this package's own rendering is
// diagnosable from the test failure alone.
func formatSource(src string) (string, error) {
	out, err := format.Source([]byte(src))
	if err != nil {
		return "", fmt.Errorf("format generated source: %w\n%s", err, src)
	}
	return string(out), nil
}

// writeIfChanged writes content to path only when the file does not
// already hold those exact bytes, so a no-op regeneration touches no
// mtime and two runs over unchanged input produce a byte-identical tree.
//
// A file that does need rewriting is replaced whole: the new bytes are
// written to a temporary file in the destination's own directory and that
// file is renamed over the destination, which is atomic because both sides
// of the rename live on one filesystem. Writing in place would truncate the
// destination first, so an I/O failure part-way through would leave a
// previously valid generated file half written — a state the next run
// cannot tell from a legitimately changed one, since the writer's only
// notion of "unchanged" is a byte comparison against whatever the file
// currently holds. Every failure path removes the temporary file, so a
// failed run leaves neither a damaged destination nor a stray file behind.
func writeIfChanged(path string, content []byte) error {
	existing, err := os.ReadFile(path)
	if err == nil && bytes.Equal(existing, content) {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create directory for %s: %w", path, err)
	}

	// The pattern is dot-prefixed and randomised after the extension, so the
	// temporary never reads as a Go file to any tool scanning the directory
	// while the write is in flight.
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	tempPath := temp.Name()
	if err := writeTempFile(temp, content); err != nil {
		os.Remove(tempPath)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		os.Remove(tempPath)
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// writeTempFile fills and closes the temporary file writeIfChanged renames
// into place, giving it the mode a generated file carries (os.CreateTemp
// makes an owner-only file). The handle is closed on every path, including
// the failing ones, so a caller that goes on to remove the temporary is
// never removing a file it still holds open.
func writeTempFile(temp *os.File, content []byte) error {
	if _, err := temp.Write(content); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Chmod(0o644); err != nil {
		temp.Close()
		return err
	}
	return temp.Close()
}
