// SPDX-License-Identifier: AGPL-3.0-or-later

package executor

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// captureHandler records every forwarded line for assertion.
type captureHandler struct {
	mu    sync.Mutex
	lines []string
}

func (c *captureHandler) HandleLine(_ context.Context, _, _, _, _, line string) {
	c.mu.Lock()
	c.lines = append(c.lines, line)
	c.mu.Unlock()
}

// TestScanOutput_ScrubsTaskOutput verifies that scanOutput applies the scrub
// function to every line before forwarding it. This is the guard against a step
// command echoing an openjd_redacted_env value into its own stdout/stderr and
// leaking the cleartext secret to the log stream.
func TestScanOutput_ScrubsTaskOutput(t *testing.T) {
	h := &captureHandler{}
	scrub := func(line string) string { return strings.ReplaceAll(line, "shh", "<redacted>") }
	r := strings.NewReader("the secret is shh\nplain line\n")

	scanOutput(context.Background(), r, "stdout", "task-1", "attempt-1", "sess-1",
		h, scrub, slog.New(slog.DiscardHandler))

	if len(h.lines) != 2 {
		t.Fatalf("captured %d lines, want 2: %q", len(h.lines), h.lines)
	}
	if h.lines[0] != "the secret is <redacted>" {
		t.Errorf("line 0 = %q; want scrubbed", h.lines[0])
	}
	if h.lines[1] != "plain line" {
		t.Errorf("line 1 = %q; want unchanged", h.lines[1])
	}
	for _, l := range h.lines {
		if strings.Contains(l, "shh") {
			t.Errorf("redacted value leaked into forwarded line: %q", l)
		}
	}
}

// TestScanOutput_NilScrubForwardsVerbatim verifies a nil scrub func is a safe
// no-op (lines pass through unchanged).
func TestScanOutput_NilScrubForwardsVerbatim(t *testing.T) {
	h := &captureHandler{}
	r := strings.NewReader("line one\nline two\n")

	scanOutput(context.Background(), r, "stderr", "t", "a", "s",
		h, nil, slog.New(slog.DiscardHandler))

	if len(h.lines) != 2 || h.lines[0] != "line one" || h.lines[1] != "line two" {
		t.Fatalf("lines = %q; want [line one line two]", h.lines)
	}
}
