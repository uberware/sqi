// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// suppressWindow is how long natsLogger drops copies of a line it has just
// logged. A broker stream that can no longer store a message logs the same
// error every couple of seconds for as long as the server runs; one line a
// minute says as much, and leaves room in the diagnostics buffer for
// everything else.
const suppressWindow = time.Minute

// maxSuppressEntries bounds natsLogger's table of recently logged lines.
// Lines that name a connection (its address and client ID) never repeat, so
// without a bound the table would grow with every refused connection.
const maxSuppressEntries = 1024

// natsLogger is the embedded NATS server's logger. Each level it forwards is
// logged under one constant message, with nats-server's own text as an
// attribute, so a broker line is easy to find:
//
//   - Errorf: Error "bus: nats server error", attribute error
//   - Warnf: Warn "bus: nats server warning", attribute warning
//   - Noticef: Debug "bus: nats server notice", attribute notice (mostly
//     startup and shutdown chatter; the broker logs its own lifecycle events)
//   - Fatalf: Error "bus: nats server fatal error", attribute error
//
// Debug and trace output are dropped.
//
// A line identical to one logged at the same level less than suppressWindow
// ago is dropped. The first copy logged after the window carries the number
// of copies dropped since the previous one as the attribute suppressed. A
// line that stops repeating inside a window leaves its last count unreported.
// Fatal errors are never dropped.
//
// The first fatal error is also recorded, so [Embedded.Start] can fail with
// it: nats-server reports a listener or JetStream failure only through its
// logger, and otherwise just never becomes ready.
type natsLogger struct {
	log   *slog.Logger
	fatal chan error // the first fatal error; buffered, never blocks the server
	now   func() time.Time

	mu     sync.Mutex
	recent map[recentKey]*recentLine
}

// recentKey identifies a line natsLogger logged.
type recentKey struct {
	level slog.Level
	text  string
}

// recentLine is when a line was last logged and how many copies of it have
// been dropped since.
type recentLine struct {
	logged  time.Time
	dropped int64
}

func newNATSLogger(log *slog.Logger) *natsLogger {
	return &natsLogger{
		log:    log,
		fatal:  make(chan error, 1),
		now:    time.Now,
		recent: make(map[recentKey]*recentLine),
	}
}

func (l *natsLogger) Fatalf(format string, v ...any) {
	err := fmt.Errorf(format, v...) //nolint:err113 // nats-server's own message
	l.log.ErrorContext(context.Background(), "bus: nats server fatal error", slog.Any("error", err))
	select {
	case l.fatal <- err:
	default:
	}
}

func (l *natsLogger) Noticef(format string, v ...any) {
	l.emitf(slog.LevelDebug, "bus: nats server notice", "notice", format, v...)
}

func (l *natsLogger) Warnf(format string, v ...any) {
	l.emitf(slog.LevelWarn, "bus: nats server warning", "warning", format, v...)
}

func (l *natsLogger) Errorf(format string, v ...any) {
	l.emitf(slog.LevelError, "bus: nats server error", "error", format, v...)
}

func (*natsLogger) Debugf(string, ...any) {}

func (*natsLogger) Tracef(string, ...any) {}

// emitf logs nats-server's line at level under msg, with the text as attribute
// key, unless it is a repeat inside the suppression window.
func (l *natsLogger) emitf(level slog.Level, msg, key, format string, v ...any) {
	ctx := context.Background()
	if !l.log.Enabled(ctx, level) {
		return
	}
	text := fmt.Sprintf(format, v...)
	dropped, ok := l.admit(recentKey{level: level, text: text})
	if !ok {
		return
	}
	attrs := []slog.Attr{slog.String(key, text)}
	if dropped > 0 {
		attrs = append(attrs, slog.Int64("suppressed", dropped))
	}
	l.log.LogAttrs(ctx, level, msg, attrs...)
}

// admit reports whether the line k is to be logged now and, if it is, how
// many copies of it were dropped since it was last logged.
func (l *natsLogger) admit(k recentKey) (dropped int64, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if r, seen := l.recent[k]; seen {
		if now.Sub(r.logged) < suppressWindow {
			r.dropped++
			return 0, false
		}
		dropped = r.dropped
		r.logged, r.dropped = now, 0
		return dropped, true
	}
	if len(l.recent) >= maxSuppressEntries {
		l.prune(now)
	}
	l.recent[k] = &recentLine{logged: now}
	return 0, true
}

// prune drops the lines whose window is over, and every line if that leaves
// the table full. A dropped line's unreported count is lost.
func (l *natsLogger) prune(now time.Time) {
	for k, r := range l.recent {
		if now.Sub(r.logged) >= suppressWindow {
			delete(l.recent, k)
		}
	}
	if len(l.recent) >= maxSuppressEntries {
		clear(l.recent)
	}
}
