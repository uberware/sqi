// SPDX-License-Identifier: AGPL-3.0-only

package openjd_test

import (
	"context"
	"testing"

	"github.com/uberware/sqi/internal/worker/openjd"
)

// ── No-op test doubles for the fuzz target ────────────────────────────────────

// noopOutputHandler discards all forwarded lines.
type noopOutputHandler struct{}

func (noopOutputHandler) HandleLine(_ context.Context, _, _, _, _, _ string) {}

// noopNATSPublisher discards all published messages.
type noopNATSPublisher struct{}

func (noopNATSPublisher) Publish(_ string, _ []byte) error { return nil }

// ── Fuzz target ───────────────────────────────────────────────────────────────

// FuzzHandleLine fuzzes the Interceptor's HandleLine method with arbitrary
// stdout and stderr line content.
//
// The fuzzer verifies that HandleLine never panics on any input, regardless of
// whether the line begins with an openjd_* directive or contains unusual
// Unicode, null bytes, very long content, or embedded newlines.
//
// Run with: go test -fuzz=FuzzHandleLine ./internal/worker/openjd/.
func FuzzHandleLine(f *testing.F) {
	// Seed corpus: all recognized directives, edge cases, and normal lines.
	f.Add("openjd_progress: 0", "stdout")
	f.Add("openjd_progress: 50", "stdout")
	f.Add("openjd_progress: 100", "stdout")
	f.Add("openjd_progress: -1", "stdout")
	f.Add("openjd_progress: 101", "stdout")
	f.Add("openjd_progress: abc", "stdout")
	f.Add("openjd_progress:", "stdout")
	f.Add("openjd_progress:   ", "stdout")
	f.Add("openjd_status: rendering frame 42 of 100", "stdout")
	f.Add("openjd_status:", "stdout")
	f.Add("openjd_fail: out of memory on GPU", "stdout")
	f.Add("openjd_fail:", "stdout")
	f.Add("openjd_unknown_directive: some value", "stdout")
	f.Add("openjd_", "stdout")
	f.Add("normal log line", "stdout")
	f.Add("", "stdout")
	f.Add("openjd_progress: 50", "stderr")
	f.Add("normal stderr line", "stderr")
	f.Add("\x00\x01\x02binary content", "stdout")
	f.Add("openjd_progress: 50\nopenjd_fail: escape", "stdout")
	f.Add("openjd_status: line1\nline2", "stdout")
	f.Add("openjd_status: こんにちは 🎬", "stdout")

	f.Fuzz(func(_ *testing.T, line, stream string) {
		// Build a fresh interceptor for each fuzz input to prevent state
		// from a prior openjd_fail from affecting subsequent calls.
		interceptor := openjd.New(
			noopOutputHandler{},
			noopNATSPublisher{},
			"fuzz-worker-id",
			discardLogger(), // defined in interceptor_test.go (same package)
		)

		// Register a task so the interceptor has per-attempt state to update.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		interceptor.RegisterTask("a-fuzz", "t-fuzz", "j-fuzz", "s-fuzz", cancel)
		defer interceptor.Deregister("a-fuzz")

		// Must not panic with any combination of line content and stream name.
		interceptor.HandleLine(ctx, "t-fuzz", "a-fuzz", "s-fuzz", stream, line)
	})
}
