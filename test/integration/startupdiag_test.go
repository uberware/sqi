// SPDX-License-Identifier: AGPL-3.0-or-later

package integration

import (
	"bytes"
	"fmt"
	"log/slog"
	"runtime"
	"runtime/pprof"
	"strings"
	"time"
)

// A test server that never starts listening used to fail with only "HTTP server
// did not come up within 10s": the logger drops everything below WARN, and a
// server stuck in startup logs nothing at WARN. The Windows preset harness hit
// that six tests in a row, and the message could not say which startup phase
// stalled. These helpers keep the INFO trail and, on a timeout, the stacks of
// the server's own goroutines (the server runs in this process), so the next
// failure names the call it is blocked in.

// maxTrailLines bounds the startup trail kept per server.
const maxTrailLines = 2000

// trailLine formats one record as "+1.234s INFO msg k=v ...", the time being
// since the server's logger was created, so a stall shows as a gap.
func trailLine(since time.Duration, r slog.Record, pre []slog.Attr) string {
	var b strings.Builder
	fmt.Fprintf(&b, "+%.3fs %s %s", since.Seconds(), r.Level, r.Message)
	for _, a := range pre {
		fmt.Fprintf(&b, " %s=%s", a.Key, a.Value)
	}
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%s", a.Key, a.Value)
		return true
	})
	return b.String()
}

// addTrail appends line, dropping the oldest once maxTrailLines is reached.
func (c *logCapture) addTrail(line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.trail) >= maxTrailLines {
		c.trail = append(c.trail[:0], c.trail[1:]...)
	}
	c.trail = append(c.trail, line)
}

// trailText returns the startup trail, one record per line.
func (c *logCapture) trailText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.trail) == 0 {
		return "(the server logged nothing at INFO or above)"
	}
	return strings.Join(c.trail, "\n")
}

// startupDiagnostics describes a server that did not start in time: how long
// its logger has existed, every INFO-or-above record it logged, and the stacks
// of the goroutines running sqi code. The last INFO line says how far startup
// got; the stacks say what it is waiting on now.
func startupDiagnostics(logs *logCapture) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n--- server startup trail (%s since logger creation) ---\n%s\n",
		time.Since(logs.start).Round(time.Millisecond), logs.trailText())
	fmt.Fprintf(&b, "--- sqi goroutines (GOMAXPROCS=%d NumCPU=%d total goroutines=%d) ---\n%s",
		runtime.GOMAXPROCS(0), runtime.NumCPU(), runtime.NumGoroutine(), sqiGoroutines())
	return b.String()
}

// sqiGoroutines returns the stacks, from a full goroutine dump, of the
// goroutines that have sqi code on them, capped so one failure cannot flood
// the log. Goroutines of earlier tests' servers are included; their ages in
// the dump header tell them from this one's.
func sqiGoroutines() string {
	const maxBytes = 120 << 10
	var dump bytes.Buffer
	if err := pprof.Lookup("goroutine").WriteTo(&dump, 2); err != nil {
		return fmt.Sprintf("(goroutine dump failed: %v)", err)
	}
	var b strings.Builder
	for g := range strings.SplitSeq(dump.String(), "\n\n") {
		if !strings.Contains(g, "github.com/uberware/sqi/internal/") {
			continue
		}
		if b.Len()+len(g) > maxBytes {
			b.WriteString("(truncated)\n")
			break
		}
		b.WriteString(g)
		b.WriteString("\n\n")
	}
	if b.Len() == 0 {
		return "(no goroutine has sqi code on its stack)\n"
	}
	return b.String()
}
