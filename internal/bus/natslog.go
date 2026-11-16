// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// suppressWindow is how long natsLogger drops copies of a line it has just
// logged. A broker stream that can no longer store a message logs the same
// error every couple of seconds for as long as the server runs; one line a
// minute is enough, and leaves room in the diagnostics buffer for everything
// else.
const suppressWindow = time.Minute

// maxSuppressEntries bounds natsLogger's table of recently logged lines, and
// so its timers (at most one per line). Lines that name a connection (its
// address and client ID) never repeat, so without a bound the table would
// grow with every refused connection.
const maxSuppressEntries = 1024

// natsLogger is the embedded NATS server's logger. Each level it forwards is
// logged under one constant message, with nats-server's own text as an
// attribute, so a broker line is easy to find:
//
//   - Errorf: Error "bus: nats server error", attribute error
//   - Warnf: Warn "bus: nats server warning", attribute warning
//   - Noticef: Debug "bus: nats server notice", attribute notice (mostly
//     startup and shutdown messages, which sqi logs itself), except a slow
//     consumer report, which means dropped messages or a closed connection
//     and is logged as a warning
//   - Fatalf: Error "bus: nats server fatal error", attribute error
//
// Debug and trace output are dropped.
//
// A line identical to one logged at the same level less than suppressWindow
// ago is dropped. When the window ends with copies dropped, the line is logged
// once more with their number as the attribute suppressed, and a new window
// starts; a copy arriving after the window but before that happens is logged
// with the count instead. A count is therefore reported at most a window late.
// [natsLogger.close] reports the counts still pending. Fatal errors are never
// dropped.
//
// The first fatal error is also recorded, so [Embedded.Start] can fail with
// it: nats-server reports a listener or JetStream failure only through its
// logger, and otherwise just never becomes ready.
type natsLogger struct {
	log   *slog.Logger
	fatal chan error // the first fatal error; buffered, never blocks the server

	// now and afterFunc are the clock; tests replace them. afterFunc runs f
	// after d and returns a function that stops it.
	now       func() time.Time
	afterFunc func(d time.Duration, f func()) (stop func() bool)

	mu     sync.Mutex
	closed bool // set by close: lines are logged untracked from then on
	recent map[recentKey]*recentLine
}

// recentKey identifies a line natsLogger logged.
type recentKey struct {
	level slog.Level
	text  string
}

// recentLine is a tracked line: when it was last logged and how many copies
// of it have been dropped since.
type recentLine struct {
	msg, attr string // the slog message and attribute key it is logged under
	logged    time.Time
	dropped   int64

	// stop stops the timer that reports dropped at the window's end; nil when
	// none is armed. gen lets a superseded timer see that it is stale.
	stop func() bool
	gen  uint64
}

// pendingLine is a line to log, with the number of copies dropped before it.
type pendingLine struct {
	key       recentKey
	msg, attr string
	dropped   int64
}

func newNATSLogger(log *slog.Logger) *natsLogger {
	return &natsLogger{
		log:   log,
		fatal: make(chan error, 1),
		now:   time.Now,
		afterFunc: func(d time.Duration, f func()) func() bool {
			return time.AfterFunc(d, f).Stop
		},
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
	if isSlowConsumer(format) {
		l.emitf(slog.LevelWarn, "bus: nats server warning", "warning", format, v...)
		return
	}
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

// isSlowConsumer reports whether a notice is a slow consumer report other
// than a recovery: "Slow Consumer Detected: MaxPending ... Exceeded" (the
// client is closed) or "Slow Consumer Detected: WriteDeadline ... exceeded"
// ("State" replaces "Detected" for a client already reported slow).
// nats-server puts the client before the text, so it is searched for rather
// than matched as a prefix.
func isSlowConsumer(format string) bool {
	return strings.Contains(format, "Slow Consumer ") && !strings.Contains(format, "Slow Consumer Recovered")
}

// emitf logs nats-server's line at level under msg, with the text as attribute
// key, unless it is a repeat inside the suppression window.
func (l *natsLogger) emitf(level slog.Level, msg, key, format string, v ...any) {
	if !l.log.Enabled(context.Background(), level) {
		return
	}
	p := pendingLine{key: recentKey{level: level, text: fmt.Sprintf(format, v...)}, msg: msg, attr: key}
	var ok bool
	if p.dropped, ok = l.admit(p); ok {
		l.write(p)
	}
}

// write logs p, with its dropped count when there is one.
func (l *natsLogger) write(p pendingLine) {
	attrs := []slog.Attr{slog.String(p.attr, p.key.text)}
	if p.dropped > 0 {
		attrs = append(attrs, slog.Int64("suppressed", p.dropped))
	}
	l.log.LogAttrs(context.Background(), p.key.level, p.msg, attrs...)
}

// admit reports whether the line p is to be logged now and, if it is, how
// many copies of it were dropped since it was last logged.
func (l *natsLogger) admit(p pendingLine) (dropped int64, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return 0, true
	}
	now := l.now()
	if r, seen := l.recent[p.key]; seen {
		if now.Sub(r.logged) < suppressWindow {
			r.dropped++
			if r.stop == nil {
				l.arm(p.key, r, suppressWindow-now.Sub(r.logged))
			}
			return 0, false
		}
		dropped = r.dropped
		disarm(r)
		r.logged, r.dropped = now, 0
		return dropped, true
	}
	if len(l.recent) >= maxSuppressEntries && !l.prune(now) {
		// Every tracked line is still in its window: log this one untracked
		// rather than lose a tracked line's count.
		return 0, true
	}
	l.recent[p.key] = &recentLine{msg: p.msg, attr: p.attr, logged: now}
	return 0, true
}

// arm starts r's timer to report its dropped count after d. l.mu is held.
func (l *natsLogger) arm(k recentKey, r *recentLine, d time.Duration) {
	r.gen++
	gen := r.gen
	r.stop = l.afterFunc(d, func() { l.flush(k, r, gen) })
}

// disarm stops r's timer, if one is armed. The caller holds the logger's mu.
func disarm(r *recentLine) {
	if r.stop != nil {
		r.stop()
		r.stop = nil
	}
}

// flush is r's timer: at the end of r's window it logs the copies dropped in
// it and starts a new window. A timer superseded since it was armed (a copy
// after the window reported the count, or close ran) does nothing.
func (l *natsLogger) flush(k recentKey, r *recentLine, gen uint64) {
	l.mu.Lock()
	if l.closed || l.recent[k] != r || r.gen != gen || r.stop == nil {
		l.mu.Unlock()
		return
	}
	r.stop = nil
	p := pendingLine{key: k, msg: r.msg, attr: r.attr, dropped: r.dropped}
	r.logged, r.dropped = l.now(), 0
	l.mu.Unlock()
	if p.dropped > 0 {
		l.write(p)
	}
}

// prune drops the lines whose window is over and that have no count pending,
// and reports whether that made room. l.mu is held.
func (l *natsLogger) prune(now time.Time) bool {
	for k, r := range l.recent {
		if r.stop == nil && now.Sub(r.logged) >= suppressWindow {
			delete(l.recent, k)
		}
	}
	return len(l.recent) < maxSuppressEntries
}

// close stops every timer and logs the counts still pending. Lines logged
// after it are logged untracked. Call it once the server has stopped.
func (l *natsLogger) close() {
	l.mu.Lock()
	l.closed = true
	var pending []pendingLine
	for k, r := range l.recent {
		disarm(r)
		if r.dropped > 0 {
			pending = append(pending, pendingLine{key: k, msg: r.msg, attr: r.attr, dropped: r.dropped})
		}
	}
	clear(l.recent)
	l.mu.Unlock()
	for _, p := range pending {
		l.write(p)
	}
}
