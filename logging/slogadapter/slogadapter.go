// Package slogadapter bridges the library's logging.Logger interface to the
// standard library's log/slog. It is a leaf package: log/slog is imported here
// only, keeping it out of the core (ocppj/ws) import graph.
package slogadapter

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/enesismail/ocpp-go/logging"
)

// New returns a logging.Logger that forwards the library's internal logs to the
// given *slog.Logger, so ocppj.SetLogger / ws.SetLogger can route the library's
// logs into an slog setup instead of the silent VoidLogger default. A nil logger
// falls back to slog.Default() (captured at construction time; a later
// slog.SetDefault does not affect an already-constructed adapter).
//
// The library's Debug/Info/Error map to the same slog levels; it has no Warn
// level. Print-style arguments are formatted with fmt.Sprint, so a multi-argument
// call such as Info("attempt", n) has no separating space, matching logrus print
// semantics. A disabled level is checked before formatting, so no fmt work is done
// for a filtered-out record. Because this is a wrapper, slog source location
// (HandlerOptions.AddSource) points at the adapter, not the library call site.
func New(logger *slog.Logger) logging.Logger {
	if logger == nil {
		logger = slog.Default()
	}
	return &slogLogger{logger: logger}
}

type slogLogger struct{ logger *slog.Logger }

// emit formats and logs at level only if that level is enabled, so a filtered-out
// record pays no formatting cost.
func (l *slogLogger) emit(level slog.Level, format func() string) {
	ctx := context.Background()
	if l.logger.Enabled(ctx, level) {
		l.logger.Log(ctx, level, format())
	}
}

func (l *slogLogger) Debug(args ...interface{}) {
	l.emit(slog.LevelDebug, func() string { return fmt.Sprint(args...) })
}

func (l *slogLogger) Debugf(format string, args ...interface{}) {
	l.emit(slog.LevelDebug, func() string { return fmt.Sprintf(format, args...) })
}

func (l *slogLogger) Info(args ...interface{}) {
	l.emit(slog.LevelInfo, func() string { return fmt.Sprint(args...) })
}

func (l *slogLogger) Infof(format string, args ...interface{}) {
	l.emit(slog.LevelInfo, func() string { return fmt.Sprintf(format, args...) })
}

func (l *slogLogger) Error(args ...interface{}) {
	l.emit(slog.LevelError, func() string { return fmt.Sprint(args...) })
}

func (l *slogLogger) Errorf(format string, args ...interface{}) {
	l.emit(slog.LevelError, func() string { return fmt.Sprintf(format, args...) })
}

var _ logging.Logger = (*slogLogger)(nil)
