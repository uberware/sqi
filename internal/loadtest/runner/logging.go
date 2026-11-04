// SPDX-License-Identifier: AGPL-3.0-or-later

package runner

import (
	"context"
	"log/slog"
)

// fleetLogLevel is the least severe record the simulated workers log: each
// worker logs its connects, heartbeats and disconnects at info, which for a
// fleet of thousands drowns the run's own progress.
const fleetLogLevel = slog.LevelWarn

// levelFloor is a handler that drops records below min and passes the rest
// to h.
type levelFloor struct {
	h   slog.Handler
	min slog.Level
}

// withLevelFloor returns l with every record below min dropped.
func withLevelFloor(l *slog.Logger, minLevel slog.Level) *slog.Logger {
	return slog.New(levelFloor{h: l.Handler(), min: minLevel})
}

func (f levelFloor) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= f.min && f.h.Enabled(ctx, l)
}

func (f levelFloor) Handle(ctx context.Context, r slog.Record) error { return f.h.Handle(ctx, r) }

func (f levelFloor) WithAttrs(attrs []slog.Attr) slog.Handler {
	return levelFloor{h: f.h.WithAttrs(attrs), min: f.min}
}

func (f levelFloor) WithGroup(name string) slog.Handler {
	return levelFloor{h: f.h.WithGroup(name), min: f.min}
}
