package slogadapter

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/enesismail/ocpp-go/logging"
)

// TestLevelAndMessageRouting checks that each of the six methods emits a record
// at the matching slog level with the formatted message, and (for print methods)
// that fmt.Sprint concatenates operands without a separating space.
func TestLevelAndMessageRouting(t *testing.T) {
	tests := []struct {
		name  string
		level string
		msg   string
		log   func(logging.Logger)
	}{
		{"Debug", "DEBUG", "ab", func(l logging.Logger) { l.Debug("a", "b") }},
		{"Debugf", "DEBUG", "x=7", func(l logging.Logger) { l.Debugf("x=%d", 7) }},
		{"Info", "INFO", "ab", func(l logging.Logger) { l.Info("a", "b") }},
		{"Infof", "INFO", "x=7", func(l logging.Logger) { l.Infof("x=%d", 7) }},
		{"Error", "ERROR", "ab", func(l logging.Logger) { l.Error("a", "b") }},
		{"Errorf", "ERROR", "x=7", func(l logging.Logger) { l.Errorf("x=%d", 7) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			tt.log(New(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))))

			var record map[string]any
			if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
				t.Fatalf("unmarshal log record: %v", err)
			}
			if got := record["level"]; got != tt.level {
				t.Errorf("level = %v, want %q", got, tt.level)
			}
			if got := record["msg"]; got != tt.msg {
				t.Errorf("msg = %v, want %q", got, tt.msg)
			}
		})
	}
}

// TestNilLogger verifies New(nil) actually routes through slog.Default() (not
// merely returns a non-nil no-op): it swaps the process default to a capturing
// handler, logs through New(nil), and asserts the record landed there.
func TestNilLogger(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	New(nil).Info("hello")

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("New(nil) did not log through slog.Default() (no record): %v", err)
	}
	if got := record["msg"]; got != "hello" {
		t.Errorf("msg = %v, want %q", got, "hello")
	}
}

// TestPrintMethodDoesNotLeakAttributes proves the print-style args are formatted
// into the message, NOT forwarded as slog key/value attributes.
func TestPrintMethodDoesNotLeakAttributes(t *testing.T) {
	var buf bytes.Buffer
	New(slog.New(slog.NewJSONHandler(&buf, nil))).Info("k", "v")

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("unmarshal log record: %v", err)
	}
	if got := record["msg"]; got != "kv" {
		t.Errorf("msg = %v, want %q", got, "kv")
	}
	if len(record) != 3 {
		t.Fatalf("record has unexpected attributes (want only time/level/msg): %v", record)
	}
	for _, key := range []string{"time", "level", "msg"} {
		if _, ok := record[key]; !ok {
			t.Errorf("record is missing %q: %v", key, record)
		}
	}
}
