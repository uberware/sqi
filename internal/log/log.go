// SPDX-License-Identifier: AGPL-3.0-or-later

// Package log provides slog-based structured logging for the sqi binaries.
//
// New constructs a [*slog.Logger] from a level string and format string,
// registers it as the process-wide default, and returns it for injection into
// components that need an explicit logger.
//
// Supported formats:
//   - "json" (default) — machine-readable JSON, one object per line
//   - "text"           — human-readable key=value pairs (logfmt-style)
//
// Supported levels: "debug", "info" (default), "warn", "error".
package log

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// New constructs a [*slog.Logger] from a level string and format string, sets
// it as the process-wide [slog] default, and returns it.
//
// level accepts "debug", "info", "warn", or "error" (case-insensitive).
// An empty string defaults to "info".
//
// format accepts "json" or "text" (case-insensitive).
// An empty string or any unrecognized value defaults to "json".
//
// w is the output destination; pass [os.Stderr] for normal use or a
// [*bytes.Buffer] in tests.
func New(level, format string, w io.Writer) (*slog.Logger, error) {
	lvl, err := ParseLevel(level)
	if err != nil {
		return nil, err
	}

	opts := &slog.HandlerOptions{Level: lvl}

	var handler slog.Handler
	if strings.EqualFold(format, "text") {
		handler = slog.NewTextHandler(w, opts)
	} else {
		handler = slog.NewJSONHandler(w, opts)
	}

	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger, nil
}

// ParseLevel converts a level string to a [slog.Level].
// Accepted values are "debug", "info", "warn", and "error"
// (case-insensitive). An empty string returns [slog.LevelInfo].
// Any other value returns an error.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("unknown log level %q: want debug, info, warn, or error", s)
	}
}
