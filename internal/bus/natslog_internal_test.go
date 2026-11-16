// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	nats "github.com/nats-io/nats.go"
)

// recordHandler is a slog.Handler that keeps every record it is given.
type recordHandler struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (*recordHandler) Enabled(context.Context, slog.Level) bool { return true }

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

// fakeClock is a settable clock for natsLogger's repeat suppression.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func newTestNATSLogger(h slog.Handler) (*natsLogger, *fakeClock) {
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	l := newNATSLogger(slog.New(h))
	l.now = clk.now
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

// TestNATSLogger_SuppressesRepeats checks that an error repeated within the
// suppression window is logged once, and that the next one after the window
// is logged with the number of copies dropped in between. A stream that can no
// longer store a message logs the same error every couple of seconds for as
// long as the broker runs.
func TestNATSLogger_SuppressesRepeats(t *testing.T) {
	h := &recordHandler{}
	l, clk := newTestNATSLogger(h)
	const text = "JetStream failed to store a msg on stream 'SQI > SQI_TASK': lmb missing"

	for range 30 {
		l.Errorf("%s", text)
		clk.t = clk.t.Add(2 * time.Second)
	}
	// 30 calls over 60s: the first is logged, the next 29 fall inside its window.
	recs := h.records()
	if len(recs) != 1 {
		t.Fatalf("got %d records inside the window, want 1", len(recs))
	}
	if _, ok := recordInt(recs[0], "suppressed"); ok {
		t.Errorf("the first line carries a suppressed count")
	}

	l.Errorf("%s", text) // 60s after the first: the window is over
	recs = h.records()
	if len(recs) != 2 {
		t.Fatalf("got %d records after the window, want 2", len(recs))
	}
	if got := recordAttr(recs[1], "error"); got != text {
		t.Errorf("error = %q, want %q", got, text)
	}
	if n, ok := recordInt(recs[1], "suppressed"); !ok || n != 29 {
		t.Errorf("suppressed = %d (present %v), want 29", n, ok)
	}

	// A new window starts with that line; its repeats are counted afresh.
	clk.t = clk.t.Add(time.Second)
	l.Errorf("%s", text)
	clk.t = clk.t.Add(suppressWindow)
	l.Errorf("%s", text)
	recs = h.records()
	if len(recs) != 3 {
		t.Fatalf("got %d records, want 3", len(recs))
	}
	if n, _ := recordInt(recs[2], "suppressed"); n != 1 {
		t.Errorf("suppressed = %d in the second window, want 1", n)
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
// table of recent lines without limit.
func TestNATSLogger_RepeatTableIsBounded(t *testing.T) {
	h := &recordHandler{}
	l, clk := newTestNATSLogger(h)
	for i := range 3 * maxSuppressEntries {
		l.Errorf("authentication error - cid:%d", i)
		clk.t = clk.t.Add(time.Millisecond)
	}
	if got := len(h.records()); got != 3*maxSuppressEntries {
		t.Errorf("got %d records, want every distinct line (%d)", got, 3*maxSuppressEntries)
	}
	if got := l.recentLen(); got > maxSuppressEntries {
		t.Errorf("repeat table holds %d entries, want at most %d", got, maxSuppressEntries)
	}
}

// recentLen is the number of lines in l's repeat table.
func (l *natsLogger) recentLen() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent)
}
