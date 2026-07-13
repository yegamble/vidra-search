// Package telemetry builds the vidra-search process logger and Prometheus
// metrics. It mirrors vidra-core's observability conventions: a slog logger
// (JSON in production) and a PRIVATE Prometheus registry exporting bounded-
// cardinality instruments.
package telemetry

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// NewLogger constructs the process logger. level is debug|info|warn|error
// (case-insensitive; empty = info); format is "json" (default) or "text". An
// unrecognised level or format is an error so misconfiguration fails fast.
func NewLogger(w io.Writer, level, format string) (*slog.Logger, error) {
	lvl, err := ParseLevel(level)
	if err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{Level: lvl}

	var h slog.Handler
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "json":
		h = slog.NewJSONHandler(w, opts)
	case "text":
		h = slog.NewTextHandler(w, opts)
	default:
		return nil, fmt.Errorf("telemetry: unknown log format %q (want json|text)", format)
	}
	return slog.New(h), nil
}

// ParseLevel maps a level name to an slog.Level (debug|info|warn|error,
// case-insensitive; "warning" aliases warn; empty = info).
func ParseLevel(level string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("telemetry: unknown log level %q (want debug|info|warn|error)", level)
	}
}
