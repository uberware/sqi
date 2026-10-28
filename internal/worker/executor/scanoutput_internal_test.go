// SPDX-License-Identifier: AGPL-3.0-or-later

package executor

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
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

// TestScanOutput_LongLines verifies that a line longer than the 64 KiB scanner
// limit is delivered as consecutive pieces of at most maxOutputLine bytes, each
// valid UTF-8, and that reading carries on past it.
func TestScanOutput_LongLines(t *testing.T) {
	a := func(n int) string { return strings.Repeat("a", n) }
	repeat := func(s string, n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = s
		}
		return out
	}

	cases := []struct {
		name  string
		input string
		want  []string
	}{
		{"short lines unchanged", "one\ntwo\n", []string{"one", "two"}},
		{"exactly limit", a(65536) + "\nnext\n", []string{a(65536), "next"}},
		{"one over limit", a(65537) + "\nnext\n", []string{a(65536), "a", "next"}},
		{"1 MiB line then more", a(1<<20) + "\nafter\n", append(repeat(a(65536), 16), "after")},
		{"utf8 boundary", a(65535) + "é" + "\nnext\n", []string{a(65535), "é", "next"}},
		{"crlf at boundary", a(65536) + "\r\nnext\r\n", []string{a(65536), "next"}},
		{"cr is the buffer's last byte", a(65535) + "\r\nnext\n", []string{a(65535), "next"}},
		{"cr inside a long line is kept", a(65535) + "\rb\n", []string{a(65535), "\rb"}},
		{"long then unterminated", a(70000) + "\ntail", []string{a(65536), a(4464), "tail"}},
		{"empty lines", "\n\nx\n", []string{"", "", "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &captureHandler{}
			scanOutput(context.Background(), strings.NewReader(tc.input), "stdout", "t", "a", "s",
				h, nil, slog.New(slog.DiscardHandler))

			if len(h.lines) != len(tc.want) {
				t.Fatalf("captured %d pieces, want %d (lengths %v)", len(h.lines), len(tc.want), lengths(h.lines))
			}
			for i := range tc.want {
				if h.lines[i] != tc.want[i] {
					t.Errorf("piece %d: len %d, want len %d (equal=%v)", i, len(h.lines[i]), len(tc.want[i]), h.lines[i] == tc.want[i])
				}
			}
			for i, l := range h.lines {
				if len(l) > 65536 {
					t.Errorf("piece %d is %d bytes, over the 65536 limit", i, len(l))
				}
				if !utf8.ValidString(l) {
					t.Errorf("piece %d is not valid UTF-8", i)
				}
			}
		})
	}
}

func lengths(lines []string) []int {
	out := make([]int, len(lines))
	for i, l := range lines {
		out[i] = len(l)
	}
	return out
}

// recordingHandler is a slog.Handler that keeps every record's message.
type recordingHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (*recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (r *recordingHandler) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	r.msgs = append(r.msgs, rec.Message)
	r.mu.Unlock()
	return nil
}

func (r *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return r }

func (r *recordingHandler) WithGroup(string) slog.Handler { return r }

// TestScanOutput_LongLineWarnsOnce verifies that splitting lines is reported
// once per stream, not once per long line.
func TestScanOutput_LongLineWarnsOnce(t *testing.T) {
	rec := &recordingHandler{}
	long := strings.Repeat("b", 70000)
	scanOutput(context.Background(), strings.NewReader(long+"\n"+long+"\n"), "stdout", "t", "a", "s",
		&captureHandler{}, nil, slog.New(rec))

	n := 0
	for _, m := range rec.msgs {
		if strings.Contains(m, "over 64 KiB") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("got %d over-64-KiB warnings, want 1 (messages: %q)", n, rec.msgs)
	}
}
