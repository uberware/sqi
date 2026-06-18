// SPDX-License-Identifier: AGPL-3.0-or-later

package log

import (
	"context"
	"io"
	"log/slog"
)

// SinkRecord is the flattened form of a slog record delivered to a [Sink].
type SinkRecord struct {
	// Ts is the record time as a UTC RFC3339 timestamp with a fixed-width
	// 9-digit nanosecond fraction ("2006-01-02T15:04:05.000000000Z07:00"), so
	// it sorts lexically. Sink implementations that need a time.Time parse it
	// with that same layout (see internal/diag and internal/worker/diaglog).
	Ts string
	// Level is the slog level string: "DEBUG", "INFO", "WARN", "ERROR".
	Level string
	// Msg is the log message.
	Msg string
	// Attrs holds all attributes (WithAttrs-accumulated + per-record), stringified.
	Attrs map[string]string
}

// Sink receives every log record that passes the configured level, in addition
// to the normal stderr output.
//
// IMPORTANT: a Sink implementation MUST NOT emit slog records itself (directly
// or indirectly).  Doing so re-enters the fan-out handler and creates an
// infinite logging loop.  Sink errors must be dropped or written straight to a
// raw io.Writer, never routed back through slog.
type Sink interface {
	Emit(r SinkRecord)
}

// fanoutHandler wraps a base slog.Handler, delegating to it for stderr output
// and additionally forwarding each handled record to a Sink.
type fanoutHandler struct {
	base  slog.Handler
	sink  Sink
	attrs []slog.Attr
}

func (h *fanoutHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.base.Enabled(ctx, l)
}

func (h *fanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	err := h.base.Handle(ctx, r)

	attrs := make(map[string]string, len(h.attrs)+r.NumAttrs())
	for _, a := range h.attrs {
		attrs[a.Key] = a.Value.String()
	}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	h.sink.Emit(SinkRecord{
		Ts:    r.Time.UTC().Format("2006-01-02T15:04:05.000000000Z07:00"),
		Level: r.Level.String(),
		Msg:   r.Message,
		Attrs: attrs,
	})
	return err
}

func (h *fanoutHandler) WithAttrs(as []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(h.attrs)+len(as))
	merged = append(merged, h.attrs...)
	merged = append(merged, as...)
	return &fanoutHandler{base: h.base.WithAttrs(as), sink: h.sink, attrs: merged}
}

func (h *fanoutHandler) WithGroup(name string) slog.Handler {
	// Groups are preserved for stderr output but not flattened into sink attrs;
	// diagnostic records are flat key/value for the UI. This is intentional.
	return &fanoutHandler{base: h.base.WithGroup(name), sink: h.sink, attrs: h.attrs}
}

// NewWithSink behaves like [New] (same level/format/destination semantics and
// SetDefault side effect) but, when sink is non-nil, also forwards every record
// that passes the level filter to sink.  A nil sink is identical to [New].
func NewWithSink(level, format string, w io.Writer, sink Sink) (*slog.Logger, error) {
	logger, err := New(level, format, w)
	if err != nil {
		return nil, err
	}
	if sink == nil {
		return logger, nil
	}
	wrapped := slog.New(&fanoutHandler{base: logger.Handler(), sink: sink})
	slog.SetDefault(wrapped)
	return wrapped, nil
}
