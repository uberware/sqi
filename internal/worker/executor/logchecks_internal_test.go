// SPDX-License-Identifier: AGPL-3.0-or-later

package executor

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/uberware/sqi/internal/worker/protocol"
)

func mustChecker(t *testing.T, failOn, require []string) *logChecker {
	t.Helper()
	c, err := newLogChecker(&protocol.LogChecks{FailOn: failOn, RequireLine: require})
	if err != nil {
		t.Fatalf("newLogChecker: %v", err)
	}
	if c == nil {
		t.Fatal("newLogChecker returned nil for non-nil checks")
	}
	return c
}

func TestNewLogChecker_NilChecks(t *testing.T) {
	c, err := newLogChecker(nil)
	if err != nil || c != nil {
		t.Fatalf("newLogChecker(nil) = (%v, %v), want (nil, nil)", c, err)
	}
	// A nil checker is usable: it never fails and wraps to the same handler.
	if reason, failed := c.verdict(); failed || reason != "" {
		t.Errorf("nil checker verdict = (%q, %v), want (\"\", false)", reason, failed)
	}
	c.observe("anything")
	h := &recordingHandler{}
	if got := c.wrap(h); got != OutputHandler(h) {
		t.Error("nil checker's wrap must return the handler unchanged")
	}
}

func TestNewLogChecker_BadPattern(t *testing.T) {
	_, err := newLogChecker(&protocol.LogChecks{FailOn: []string{"(unclosed"}})
	if err == nil {
		t.Fatal("expected a compile error for an invalid pattern")
	}
}

// TestNewLogChecker_ProgramSize pins that the worker refuses a pattern whose
// compiled program exceeds logpattern.MaxProgramInsts, the same bound the
// server validates against, and accepts one at or under it.
func TestNewLogChecker_ProgramSize(t *testing.T) {
	tests := []struct {
		name    string
		checks  *protocol.LogChecks
		wantErr string
	}{
		{
			name:   "realistic patterns",
			checks: &protocol.LogChecks{FailOn: []string{`(?i)\berror\b.*failed with`}, RequireLine: []string{"SQI_RENDER: OK"}},
		},
		{
			name:   "exactly at the limit",
			checks: &protocol.LogChecks{RequireLine: []string{"a{998}a{1000}"}},
		},
		{
			name:    "one over the limit",
			checks:  &protocol.LogChecks{RequireLine: []string{"a{999}a{1000}"}},
			wantErr: `requireLine "a{999}a{1000}": compiles to 2001 instructions; at most 2000 are allowed`,
		},
		{
			name:    "905-byte concatenation of bounded repeats",
			checks:  &protocol.LogChecks{FailOn: []string{"ok", strings.Repeat(".{0,999}", 113) + "x"}},
			wantErr: "instructions; at most 2000 are allowed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := newLogChecker(tc.checks)
			if tc.wantErr == "" {
				if err != nil || c == nil {
					t.Fatalf("newLogChecker = (%v, %v), want a checker", c, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("newLogChecker error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestLogChecker(t *testing.T) {
	long := "ERROR " + strings.Repeat("x", 494) // 500 bytes

	tests := []struct {
		name       string
		failOn     []string
		require    []string
		lines      []string
		wantFailed bool
		wantReason string
	}{
		{
			name:       "failOn match",
			failOn:     []string{`ERROR +\|`},
			lines:      []string{"fine", "[ERROR python.Render] ERROR  | boom", "later"},
			wantFailed: true,
			wantReason: `log line matched failOn pattern "ERROR +\\|": "[ERROR python.Render] ERROR  | boom"`,
		},
		{
			name:       "first matching line wins",
			failOn:     []string{"bad"},
			lines:      []string{"bad one", "bad two"},
			wantFailed: true,
			wantReason: `log line matched failOn pattern "bad": "bad one"`,
		},
		{
			name:       "long line is truncated to 200 bytes plus an ellipsis",
			failOn:     []string{"^ERROR"},
			lines:      []string{long},
			wantFailed: true,
			wantReason: `log line matched failOn pattern "^ERROR": ` + quoted(long[:200]+"…"),
		},
		{
			name:       "truncation never cuts inside a multi-byte character",
			failOn:     []string{"^a"},
			lines:      []string{"a" + strings.Repeat("é", 150)}, // byte 200 falls inside a rune
			wantFailed: true,
			wantReason: `log line matched failOn pattern "^a": ` + quoted("a"+strings.Repeat("é", 99)+"…"),
		},
		{
			name:       "requireLine unmatched",
			require:    []string{"SQI_RENDER: OK"},
			lines:      []string{"working", "done"},
			wantFailed: true,
			wantReason: `the task never printed a line matching requireLine pattern "SQI_RENDER: OK"`,
		},
		{
			name:       "requireLine matched",
			require:    []string{"SQI_RENDER: OK"},
			lines:      []string{"working", "SQI_RENDER: OK"},
			wantFailed: false,
		},
		{
			name:       "two requireLine, one matched: the reason names the unmatched one",
			require:    []string{"first thing", "second thing"},
			lines:      []string{"saw the first thing"},
			wantFailed: true,
			wantReason: `the task never printed a line matching requireLine pattern "second thing"`,
		},
		{
			name:       "two requireLine, both unmatched: the first is named",
			require:    []string{"first thing", "second thing"},
			lines:      []string{"nothing"},
			wantFailed: true,
			wantReason: `the task never printed a line matching requireLine pattern "first thing"`,
		},
		{
			name:       "host log prefix does not hide the success marker",
			require:    []string{"SQI_RENDER: OK"},
			lines:      []string{"[2026.10.28-12.00.00:000][  0]LogPython: SQI_RENDER: OK"},
			wantFailed: false,
		},
		{
			name:       "failOn wins over a satisfied requireLine",
			failOn:     []string{"SQI_RENDER: FAILED:"},
			require:    []string{"SQI_RENDER: OK"},
			lines:      []string{"SQI_RENDER: OK", "SQI_RENDER: FAILED: late"},
			wantFailed: true,
			wantReason: `log line matched failOn pattern "SQI_RENDER: FAILED:": "SQI_RENDER: FAILED: late"`,
		},
		{
			name:       "a (?i) pattern folds case",
			failOn:     []string{"(?i)fatal"},
			lines:      []string{"A FATAL thing"},
			wantFailed: true,
			wantReason: `log line matched failOn pattern "(?i)fatal": "A FATAL thing"`,
		},
		{
			name:       "patterns are case sensitive without (?i)",
			failOn:     []string{"fatal"},
			lines:      []string{"A FATAL thing"},
			wantFailed: false,
		},
		{
			name:       "a redacted-env directive line is not scanned",
			failOn:     []string{"SECRET"},
			lines:      []string{"openjd_redacted_env: SECRET=hunter2"},
			wantFailed: false,
		},
		{
			name:       "an indented redacted-env directive line is not scanned",
			failOn:     []string{"SECRET"},
			lines:      []string{"  \topenjd_redacted_env: SECRET=hunter2"},
			wantFailed: false,
		},
		{
			name:       "no lines at all",
			failOn:     []string{"bad"},
			wantFailed: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := mustChecker(t, tc.failOn, tc.require)
			for _, l := range tc.lines {
				c.observe(l)
			}
			reason, failed := c.verdict()
			if failed != tc.wantFailed {
				t.Fatalf("failed = %v, want %v (reason %q)", failed, tc.wantFailed, reason)
			}
			if reason != tc.wantReason {
				t.Errorf("reason = %q\n want     %q", reason, tc.wantReason)
			}
		})
	}
}

// quoted renders s the way the reason format's %q does.
func quoted(s string) string { return `"` + s + `"` }

type recordingHandler struct {
	mu    sync.Mutex
	lines []string
}

func (r *recordingHandler) HandleLine(_ context.Context, _, _, _, _, line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
}

func TestLogChecker_WrapObservesAndForwards(t *testing.T) {
	c := mustChecker(t, []string{"boom"}, nil)
	h := &recordingHandler{}
	w := c.wrap(h)

	w.HandleLine(context.Background(), "t", "a", "s", "stdout", "all fine")
	w.HandleLine(context.Background(), "t", "a", "s", "stderr", "boom here")

	if len(h.lines) != 2 || h.lines[0] != "all fine" || h.lines[1] != "boom here" {
		t.Errorf("forwarded lines = %q, want both lines unchanged", h.lines)
	}
	if _, failed := c.verdict(); !failed {
		t.Error("wrap did not feed lines to the checker")
	}
}

// TestLogChecker_ConcurrentObserve exercises observe from two goroutines (the
// stdout and stderr readers) under the race detector.
func TestLogChecker_ConcurrentObserve(t *testing.T) {
	c := mustChecker(t, []string{"bad"}, []string{"ok"})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 200 {
				c.observe("ok line")
				c.observe("bad line")
			}
		})
	}
	wg.Wait()
	reason, failed := c.verdict()
	if !failed || reason != `log line matched failOn pattern "bad": "bad line"` {
		t.Errorf("verdict = (%q, %v)", reason, failed)
	}
}
