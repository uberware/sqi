// SPDX-License-Identifier: AGPL-3.0-or-later

package executor

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
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

// aRun returns n bytes of "a".
func aRun(n int) string { return strings.Repeat("a", n) }

// checkPieces fails t for any piece over the 65536-byte limit and, when the
// input was valid UTF-8, for any piece that is not.
func checkPieces(t *testing.T, pieces []string, valid bool) {
	t.Helper()
	for i, p := range pieces {
		if len(p) > 65536 {
			t.Errorf("piece %d is %d bytes, over the 65536 limit", i, len(p))
		}
		if valid && !utf8.ValidString(p) {
			t.Errorf("piece %d is not valid UTF-8", i)
		}
	}
}

// TestScanOutput_LongLines verifies that a line longer than 64 KiB is delivered
// as consecutive pieces of at most maxOutputLine bytes, each valid UTF-8, that
// reading carries on past it, and that shorter lines arrive exactly as before.
func TestScanOutput_LongLines(t *testing.T) {
	a := aRun
	cases := []struct {
		name  string
		input string
		want  []string
	}{
		{"short lines unchanged", "one\ntwo\n", []string{"one", "two"}},
		{"exactly limit", a(65536) + "\nnext\n", []string{a(65536), "next"}},
		{"one over limit", a(65537) + "\nnext\n", []string{a(65536), "a", "next"}},
		{"1 MiB line then more", a(1<<20) + "\nafter\n", append(slices.Repeat([]string{a(65536)}, 16), "after")},
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
					t.Errorf("piece %d: len %d, want len %d", i, len(h.lines[i]), len(tc.want[i]))
				}
			}
			checkPieces(t, h.lines, true)
		})
	}
}

// TestScanOutput_LongLinesReassemble feeds long lines whose pieces carry held-back
// bytes into the next buffer (non-ASCII text, a rune right at a cut, invalid
// UTF-8) and checks that each line's pieces reassemble to it exactly, that no
// piece is over the limit, and that pieces of valid input are valid UTF-8.
// Each long line is followed by a short sentinel line so its pieces can be
// told apart from the next line.
func TestScanOutput_LongLinesReassemble(t *testing.T) {
	a := aRun
	cases := []struct {
		name  string
		line  string
		valid bool
	}{
		// é held back at the first cut, then a cut that must back off over the
		// continuation byte of a second é.
		{"carry then backoff", a(65535) + "é" + a(65533) + "é", true},
		{"long 3-byte text", strings.Repeat("€", 50000), true},
		{"4-byte rune across the boundary", a(65534) + "😀" + a(10), true},
		// Three bytes of the rune are held back, then the line ends in a buffer
		// filled to the last byte, so the line's tail is over the limit.
		{"held-back rune then a full final buffer", a(65533) + "😀" + a(65534), true},
		{"long 3-byte text after a held-back rune", a(65535) + strings.Repeat("€", 70000), true},
		{"invalid continuation bytes", strings.Repeat("\x80", 200000), false},
		{"lone 0xFF bytes", strings.Repeat("\xff", 70000), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &captureHandler{}
			input := tc.line + "\nEND\n"
			scanOutput(context.Background(), strings.NewReader(input), "stdout", "t", "a", "s",
				h, nil, slog.New(slog.DiscardHandler))

			if len(h.lines) < 2 || h.lines[len(h.lines)-1] != "END" {
				t.Fatalf("last line is not the END sentinel (piece lengths %v)", lengths(h.lines))
			}
			pieces := h.lines[:len(h.lines)-1]
			if got := strings.Join(pieces, ""); got != tc.line {
				t.Fatalf("pieces reassemble to %d bytes, want the original %d (piece lengths %v)",
					len(got), len(tc.line), lengths(pieces))
			}
			if slices.Contains(pieces, "") {
				t.Errorf("an empty piece was delivered (piece lengths %v)", lengths(pieces))
			}
			checkPieces(t, pieces, tc.valid)
		})
	}
}

// TestPieceLen_NeverZero pins the guard that keeps the reader moving: a
// non-empty buffer always yields a non-empty piece, even when every byte would
// otherwise be held back. scanOutput never hands pieceLen a buffer that short,
// so the guard is only reachable directly.
func TestPieceLen_NeverZero(t *testing.T) {
	for _, b := range [][]byte{{0xE2}, {0xE2, 0x82}, {'\r'}} {
		if n := pieceLen(b); n != len(b) {
			t.Errorf("pieceLen(%q) = %d; want %d", b, n, len(b))
		}
	}
}

func lengths(lines []string) []int {
	out := make([]int, len(lines))
	for i, l := range lines {
		out[i] = len(l)
	}
	return out
}

// TestScanOutput_LongLineWarnsOnce verifies that splitting lines is reported
// once per stream, not once per long line.
func TestScanOutput_LongLineWarnsOnce(t *testing.T) {
	var logs bytes.Buffer
	long := strings.Repeat("b", 70000)
	scanOutput(context.Background(), strings.NewReader(long+"\n"+long+"\n"), "stdout", "t", "a", "s",
		&captureHandler{}, nil, slog.New(slog.NewTextHandler(&logs, nil)))

	if n := strings.Count(logs.String(), "over 64 KiB"); n != 1 {
		t.Fatalf("got %d over-64-KiB warnings, want 1 (log: %s)", n, logs.String())
	}
}
