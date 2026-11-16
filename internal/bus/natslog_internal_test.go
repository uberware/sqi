// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
)

// recordHandler is a slog.Handler that keeps every record it is given. With
// floor set it is enabled only for that level and above.
type recordHandler struct {
	floor slog.Leveler

	mu   sync.Mutex
	recs []slog.Record
}

func (h *recordHandler) Enabled(_ context.Context, level slog.Level) bool {
	return h.floor == nil || level >= h.floor.Level()
}

func (h *recordHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, r.Clone())
	return nil
}

func (h *recordHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *recordHandler) WithGroup(string) slog.Handler { return h }

func (h *recordHandler) records() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]slog.Record(nil), h.recs...)
}

// recordAttr returns the string value of r's attribute key, or "" if r has none.
func recordAttr(r slog.Record, key string) string {
	var v string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			v = a.Value.String()
			return false
		}
		return true
	})
	return v
}

// TestNATSLogger_LevelMapping checks which of nats-server's log levels reach
// slog, at which level, and under which constant message: errors as Error,
// warnings as Warn, notices as Debug, and debug and trace output not at all.
func TestNATSLogger_LevelMapping(t *testing.T) {
	tests := []struct {
		name    string
		log     func(l *natsLogger)
		want    bool
		level   slog.Level
		msg     string
		attrKey string
		text    string
	}{
		{
			name:  "error",
			log:   func(l *natsLogger) { l.Errorf("Temporary %s Accept Error(%v)", "Client", "too many open files") },
			want:  true,
			level: slog.LevelError, msg: "bus: nats server error",
			attrKey: "error", text: "Temporary Client Accept Error(too many open files)",
		},
		{
			name:  "warning",
			log:   func(l *natsLogger) { l.Warnf("Readloop processing time: %v", "2s") },
			want:  true,
			level: slog.LevelWarn, msg: "bus: nats server warning",
			attrKey: "warning", text: "Readloop processing time: 2s",
		},
		{
			name:  "notice",
			log:   func(l *natsLogger) { l.Noticef("Entering lame duck mode, stop accepting %s", "new clients") },
			want:  true,
			level: slog.LevelDebug, msg: "bus: nats server notice",
			attrKey: "notice", text: "Entering lame duck mode, stop accepting new clients",
		},
		{
			// nats-server prefixes a client's notices with the client.
			name: "slow consumer detected",
			log: func(l *natsLogger) {
				l.Noticef("127.0.0.1:5000 - cid:7 - Slow Consumer Detected: MaxPending of %d Exceeded", 67108864)
			},
			want:  true,
			level: slog.LevelWarn, msg: "bus: nats server warning",
			attrKey: "warning", text: "127.0.0.1:5000 - cid:7 - Slow Consumer Detected: MaxPending of 67108864 Exceeded",
		},
		{
			name: "slow consumer write deadline",
			log: func(l *natsLogger) {
				l.Noticef("127.0.0.1:5000 - cid:7 - Slow Consumer %s: WriteDeadline of %v exceeded", "State", "10s")
			},
			want:  true,
			level: slog.LevelWarn, msg: "bus: nats server warning",
			attrKey: "warning", text: "127.0.0.1:5000 - cid:7 - Slow Consumer State: WriteDeadline of 10s exceeded",
		},
		{
			name: "slow consumer recovered",
			log: func(l *natsLogger) {
				l.Noticef("127.0.0.1:5000 - cid:7 - Slow Consumer Recovered: Flush took %.3fs", 0.5)
			},
			want:  true,
			level: slog.LevelDebug, msg: "bus: nats server notice",
			attrKey: "notice", text: "127.0.0.1:5000 - cid:7 - Slow Consumer Recovered: Flush took 0.500s",
		},
		{name: "debug", log: func(l *natsLogger) { l.Debugf("Client connection closed: %s", "EOF") }},
		{name: "trace", log: func(l *natsLogger) { l.Tracef("<<- [PUB %s]", "x") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &recordHandler{}
			tt.log(newNATSLogger(slog.New(h)))
			recs := h.records()
			if !tt.want {
				if len(recs) != 0 {
					t.Fatalf("got %d records, want none: %q", len(recs), recs[0].Message)
				}
				return
			}
			if len(recs) != 1 {
				t.Fatalf("got %d records, want 1", len(recs))
			}
			r := recs[0]
			if r.Level != tt.level {
				t.Errorf("level = %v, want %v", r.Level, tt.level)
			}
			if r.Message != tt.msg {
				t.Errorf("message = %q, want %q", r.Message, tt.msg)
			}
			if got := recordAttr(r, tt.attrKey); got != tt.text {
				t.Errorf("attribute %q = %q, want %q", tt.attrKey, got, tt.text)
			}
		})
	}
}

// TestNATSLogger_FatalIsRecorded checks that a fatal error is logged at Error
// and handed to the first reader of the fatal channel, and that a second one
// does not block.
func TestNATSLogger_FatalIsRecorded(t *testing.T) {
	h := &recordHandler{}
	l := newNATSLogger(slog.New(h))
	l.Fatalf("listen on %s: address in use", ":4222")
	l.Fatalf("second")

	recs := h.records()
	if len(recs) != 2 || recs[0].Level != slog.LevelError || recs[0].Message != "bus: nats server fatal error" {
		t.Fatalf("records = %v, want two Error records named %q", recs, "bus: nats server fatal error")
	}
	select {
	case err := <-l.fatal:
		if err.Error() != "listen on :4222: address in use" {
			t.Errorf("fatal = %q", err)
		}
	default:
		t.Fatal("no fatal error recorded")
	}
}

// TestEmbedded_BrokerErrorReachesLogger checks that an error the embedded
// broker itself logs reaches sqi's logger: a connect with an unknown
// credential makes nats-server log an authentication error.
func TestEmbedded_BrokerErrorReachesLogger(t *testing.T) {
	creds := newCredentialMap()
	seed, pub := creds.enroll(t, "worker-a")
	creds.remove(pub)
	h := &recordHandler{}
	b := startEmbeddedWithLogger(t, EmbeddedConfig{
		Auth: AuthConfig{Enabled: true, UserTTL: time.Hour, Lookup: creds.lookup},
	}, slog.New(h))

	if nc, err := nats.Connect(b.ClientURL(), nats.NoReconnect(), nkeyOption(t, seed, pub)); err == nil {
		nc.Close()
		t.Fatal("a removed credential was accepted")
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, r := range h.records() {
			if r.Level == slog.LevelError && r.Message == "bus: nats server error" &&
				strings.Contains(recordAttr(r, "error"), "authentication error") {
				return
			}
		}
		if time.Now().After(deadline) {
			var msgs []string
			for _, r := range h.records() {
				msgs = append(msgs, r.Level.String()+" "+r.Message+" "+recordAttr(r, "error"))
			}
			t.Fatalf("the broker's authentication error was not logged; records:\n%s", strings.Join(msgs, "\n"))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestEmbedded_ShutdownReportsPendingCounts checks that stopping the broker
// reports the copies its logger was still holding back. Each refused connect
// logs the same callout warning, which names no client, so the second is
// dropped; shutdown, well inside the window, reports it.
func TestEmbedded_ShutdownReportsPendingCounts(t *testing.T) {
	creds := newCredentialMap()
	seed, pub := creds.enroll(t, "worker-a")
	creds.remove(pub)
	h := &recordHandler{}
	b := startEmbeddedWithLogger(t, EmbeddedConfig{
		Auth: AuthConfig{Enabled: true, UserTTL: time.Hour, Lookup: creds.lookup},
	}, slog.New(h))

	for range 2 {
		if nc, err := nats.Connect(b.ClientURL(), nats.NoReconnect(), nkeyOption(t, seed, pub)); err == nil {
			nc.Close()
			t.Fatal("a removed credential was accepted")
		}
	}
	b.Shutdown()

	const warning = "Auth callout service returned an error: not authorized"
	var counts []int64
	for _, r := range h.records() {
		if r.Message == "bus: nats server warning" && recordAttr(r, "warning") == warning {
			n, _ := recordInt(r, "suppressed")
			counts = append(counts, n)
		}
	}
	if len(counts) != 2 || counts[0] != 0 || counts[1] != 1 {
		t.Errorf("callout warnings logged with suppressed counts %v, want [0 1]", counts)
	}
}

// fakeClock is a settable clock with timers for natsLogger's repeat
// suppression. A timer runs only when the test advances the clock to or past
// its deadline, or calls fireDue.
type fakeClock struct {
	mu     sync.Mutex
	t      time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at      time.Time
	f       func()
	stopped bool
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) afterFunc(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	ft := &fakeTimer{at: c.t.Add(d), f: f}
	c.timers = append(c.timers, ft)
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		was := !ft.stopped
		ft.stopped = true
		return was
	}
}

// set moves the clock to t without running any timer.
func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// advance moves the clock on by d and runs every timer then due.
func (c *fakeClock) advance(d time.Duration) {
	c.set(c.now().Add(d))
	c.fireDue()
}

// fireDue runs every live timer whose deadline has passed, oldest first.
func (c *fakeClock) fireDue() {
	for {
		c.mu.Lock()
		var due *fakeTimer
		for _, ft := range c.timers {
			if !ft.stopped && !ft.at.After(c.t) && (due == nil || ft.at.Before(due.at)) {
				due = ft
			}
		}
		if due != nil {
			due.stopped = true
		}
		c.mu.Unlock()
		if due == nil {
			return
		}
		due.f()
	}
}

// pending is the number of timers neither run nor stopped.
func (c *fakeClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, ft := range c.timers {
		if !ft.stopped {
			n++
		}
	}
	return n
}

func newTestNATSLogger(h slog.Handler) (*natsLogger, *fakeClock) {
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	l := newNATSLogger(slog.New(h))
	l.now = clk.now
	l.afterFunc = clk.afterFunc
	return l, clk
}

// recordInt returns the int64 value of r's attribute key and whether r has it.
func recordInt(r slog.Record, key string) (int64, bool) {
	var (
		v  int64
		ok bool
	)
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			v, ok = a.Value.Int64(), true
			return false
		}
		return true
	})
	return v, ok
}

// wantSuppressed fails t unless r carries the suppressed count want.
func wantSuppressed(t *testing.T, r slog.Record, want int64) {
	t.Helper()
	if n, ok := recordInt(r, "suppressed"); !ok || n != want {
		t.Errorf("suppressed = %d (present %v), want %d", n, ok, want)
	}
}

// TestNATSLogger_SuppressesRepeats checks that an error repeated within the
// suppression window is logged once, and that the window's end logs it again
// with the number of copies dropped. A stream that can no longer store a
// message logs the same error every couple of seconds for as long as the
// broker runs; that becomes one line a minute.
func TestNATSLogger_SuppressesRepeats(t *testing.T) {
	h := &recordHandler{}
	l, clk := newTestNATSLogger(h)
	const text = "JetStream failed to store a msg on stream 'SQI > SQI_TASK': lmb missing"

	for i := range 30 {
		l.Errorf("%s", text)
		if i < 29 {
			clk.advance(2 * time.Second)
		}
	}
	// 30 calls over 58s: the first is logged, the next 29 fall inside its window.
	recs := h.records()
	if len(recs) != 1 {
		t.Fatalf("got %d records inside the window, want 1", len(recs))
	}
	if _, ok := recordInt(recs[0], "suppressed"); ok {
		t.Errorf("the first line carries a suppressed count")
	}

	clk.advance(2 * time.Second) // 60s: the window ends
	recs = h.records()
	if len(recs) != 2 {
		t.Fatalf("got %d records at the end of the window, want 2", len(recs))
	}
	if got := recordAttr(recs[1], "error"); got != text {
		t.Errorf("error = %q, want %q", got, text)
	}
	wantSuppressed(t, recs[1], 29)

	// That line starts a new window; its copies are counted afresh.
	l.Errorf("%s", text)
	clk.advance(suppressWindow)
	recs = h.records()
	if len(recs) != 3 {
		t.Fatalf("got %d records, want 3", len(recs))
	}
	wantSuppressed(t, recs[2], 1)

	// With no copy in a window, its end logs nothing.
	clk.advance(3 * suppressWindow)
	if got := len(h.records()); got != 3 {
		t.Errorf("got %d records after a quiet window, want 3", got)
	}
}

// TestNATSLogger_ReportsTheLastCount checks that copies of a line that stops
// repeating inside its window are still reported, at the window's end: a
// short descriptor shortage logs one accept error and then goes quiet.
func TestNATSLogger_ReportsTheLastCount(t *testing.T) {
	h := &recordHandler{}
	l, clk := newTestNATSLogger(h)
	const text = "Temporary Client Accept Error(too many open files), sleeping 1000ms"

	l.Errorf("%s", text)
	for range 39 {
		clk.advance(time.Second)
		l.Errorf("%s", text)
	}
	if got := len(h.records()); got != 1 {
		t.Fatalf("got %d records during the burst, want 1", got)
	}
	clk.advance(suppressWindow)
	recs := h.records()
	if len(recs) != 2 {
		t.Fatalf("got %d records after the window, want 2", len(recs))
	}
	if recs[1].Level != slog.LevelError || recs[1].Message != "bus: nats server error" ||
		recordAttr(recs[1], "error") != text {
		t.Errorf("late record = %v %q %q", recs[1].Level, recs[1].Message, recordAttr(recs[1], "error"))
	}
	wantSuppressed(t, recs[1], 39)
}

// TestNATSLogger_LateTimerDoesNotDoubleReport checks that a copy logged after
// the window, before the window's timer has run, reports the count, and the
// timer then reports nothing more.
func TestNATSLogger_LateTimerDoesNotDoubleReport(t *testing.T) {
	h := &recordHandler{}
	l, clk := newTestNATSLogger(h)

	l.Errorf("same")
	l.Errorf("same")
	l.Errorf("same")
	clk.set(clk.now().Add(suppressWindow + time.Second))
	l.Errorf("same") // the window is over; its timer has not run yet
	clk.fireDue()

	recs := h.records()
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
	wantSuppressed(t, recs[1], 2)
}

// TestNATSLogger_OneTimerPerLine checks that a flood arms at most one timer
// for its line, however many copies it drops.
func TestNATSLogger_OneTimerPerLine(t *testing.T) {
	l, clk := newTestNATSLogger(&recordHandler{})
	for range 1000 {
		l.Errorf("flood A")
		l.Warnf("flood B")
	}
	if got := clk.pending(); got != 2 {
		t.Errorf("%d timers pending, want 2 (one per line)", got)
	}
}

// TestNATSLogger_CloseReportsAndStops checks that close logs every pending
// count, stops every timer, and that a line logged after close is logged and
// not tracked.
func TestNATSLogger_CloseReportsAndStops(t *testing.T) {
	h := &recordHandler{}
	l, clk := newTestNATSLogger(h)
	l.Errorf("flood")
	l.Errorf("flood")
	l.Errorf("flood")
	l.Warnf("once")

	l.close()
	recs := h.records()
	if len(recs) != 3 {
		t.Fatalf("got %d records after close, want 3", len(recs))
	}
	wantSuppressed(t, recs[2], 2)
	if got := clk.pending(); got != 0 {
		t.Errorf("%d timers pending after close, want 0", got)
	}

	l.Errorf("flood")
	l.Errorf("flood")
	if got := len(h.records()); got != 5 {
		t.Errorf("got %d records, want every line after close logged (5)", got)
	}
	if got := l.recentLen(); got != 0 || clk.pending() != 0 {
		t.Errorf("after close: %d lines tracked, %d timers pending; want none", got, clk.pending())
	}
	clk.advance(2 * suppressWindow)
	if got := len(h.records()); got != 5 {
		t.Errorf("got %d records after the window, want 5", got)
	}
}

// TestNATSLogger_SuppressionIsPerMessageAndLevel checks that only identical
// lines at the same level suppress each other.
func TestNATSLogger_SuppressionIsPerMessageAndLevel(t *testing.T) {
	h := &recordHandler{}
	l, _ := newTestNATSLogger(h)

	l.Errorf("stream %s failed", "A")
	l.Errorf("stream %s failed", "B")
	l.Warnf("stream %s failed", "A")
	l.Noticef("stream %s failed", "A")
	l.Errorf("stream %s failed", "A")
	l.Warnf("stream %s failed", "A")

	if got := len(h.records()); got != 4 {
		t.Fatalf("got %d records, want 4 (A and B errors, A warning, A notice)", got)
	}
}

// TestNATSLogger_DisabledLevelIsSkipped checks that a level the logger does
// not log costs no tracking: at Info, notices are neither logged nor kept.
func TestNATSLogger_DisabledLevelIsSkipped(t *testing.T) {
	h := &recordHandler{floor: slog.LevelInfo}
	l, clk := newTestNATSLogger(h)
	l.Noticef("Server is ready")
	l.Noticef("Server is ready")
	if got := len(h.records()); got != 0 {
		t.Errorf("got %d records, want none", got)
	}
	if got := l.recentLen(); got != 0 || clk.pending() != 0 {
		t.Errorf("%d lines tracked, %d timers pending; want none", got, clk.pending())
	}
}

// TestNATSLogger_FatalIsNeverSuppressed checks that every fatal error is
// logged, repeated or not.
func TestNATSLogger_FatalIsNeverSuppressed(t *testing.T) {
	h := &recordHandler{}
	l, _ := newTestNATSLogger(h)
	l.Fatalf("same")
	l.Fatalf("same")
	if got := len(h.records()); got != 2 {
		t.Fatalf("got %d records, want 2", got)
	}
}

// TestNATSLogger_RepeatTableIsBounded checks that distinct lines, such as the
// authentication errors that name each connection's address, cannot grow the
// table of recent lines without limit, and that a full table keeps the lines
// it already tracks: a flood's pending count is still reported.
func TestNATSLogger_RepeatTableIsBounded(t *testing.T) {
	h := &recordHandler{}
	l, clk := newTestNATSLogger(h)
	l.Errorf("flood")
	l.Errorf("flood")
	for i := range 3 * maxSuppressEntries {
		l.Errorf("authentication error - cid:%d", i)
		clk.set(clk.now().Add(time.Millisecond))
	}
	if got := len(h.records()); got != 1+3*maxSuppressEntries {
		t.Errorf("got %d records, want every distinct line (%d)", got, 1+3*maxSuppressEntries)
	}
	if got := l.recentLen(); got > maxSuppressEntries {
		t.Errorf("repeat table holds %d entries, want at most %d", got, maxSuppressEntries)
	}

	clk.advance(suppressWindow)
	recs := h.records()
	last := recs[len(recs)-1]
	if recordAttr(last, "error") != "flood" {
		t.Fatalf("last record = %q, want the flood's count", recordAttr(last, "error"))
	}
	wantSuppressed(t, last, 1)
}

// TestNATSLogger_Concurrent drives the logger from many goroutines with the
// real clock and timers, for the race detector, and checks that every call is
// accounted for. The run takes far less than a window, so each line is logged
// once and close reports the rest: per line, the first record plus the count
// close reports equal the calls made.
func TestNATSLogger_Concurrent(t *testing.T) {
	h := &recordHandler{}
	l := newNATSLogger(slog.New(h))
	const goroutines, calls = 8, 200
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			for i := range calls {
				l.Errorf("line %d", i%4)
				l.Warnf("worker %d call %d", g, i)
			}
		})
	}
	wg.Wait()
	l.close()

	seen := map[string]int64{}
	for _, r := range h.records() {
		text := recordAttr(r, "error") + recordAttr(r, "warning")
		if n, ok := recordInt(r, "suppressed"); ok {
			seen[text] += n // close's report: the copies only
		} else {
			seen[text]++
		}
	}
	for i := range 4 {
		text := "line " + strconv.Itoa(i)
		if got, want := seen[text], int64(goroutines*calls/4); got != want {
			t.Errorf("%q: %d accounted for, want %d", text, got, want)
		}
	}
	if got := len(seen); got != 4+goroutines*calls {
		t.Errorf("%d distinct lines logged, want %d", got, 4+goroutines*calls)
	}
}

// recentLen is the number of lines in l's repeat table.
func (l *natsLogger) recentLen() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent)
}
