// SPDX-License-Identifier: AGPL-3.0-or-later

package logstreamer_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/worker/logstreamer"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// ── Mock NATS ─────────────────────────────────────────────────────────────────

// mockNATS captures published messages for assertion in tests.
type mockNATS struct {
	mu     sync.Mutex
	msgs   []capturedMsg
	pubErr error // if non-nil, all Publish calls return this error
}

type capturedMsg struct {
	subject string
	data    []byte
}

func (m *mockNATS) Publish(subj string, data []byte) error {
	if m.pubErr != nil {
		return m.pubErr
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	m.mu.Lock()
	m.msgs = append(m.msgs, capturedMsg{subject: subj, data: cp})
	m.mu.Unlock()
	return nil
}

func (m *mockNATS) snapshot() []capturedMsg {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]capturedMsg, len(m.msgs))
	copy(out, m.msgs)
	return out
}

func (m *mockNATS) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.msgs)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// discard returns a no-op slog.Logger for use in tests.
func discard() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// decodeChunk unmarshals a captured message into a LogChunkMsg.
func decodeChunk(t *testing.T, cm capturedMsg) protocol.LogChunkMsg {
	t.Helper()
	var c protocol.LogChunkMsg
	if err := json.Unmarshal(cm.data, &c); err != nil {
		t.Fatalf("decodeChunk: %v", err)
	}
	return c
}

// decodeAll decodes every captured message.
func decodeAll(t *testing.T, msgs []capturedMsg) []protocol.LogChunkMsg {
	t.Helper()
	chunks := make([]protocol.LogChunkMsg, len(msgs))
	for i, m := range msgs {
		chunks[i] = decodeChunk(t, m)
	}
	return chunks
}

// slowFlushCfg returns a Config with a very long flush interval so
// tests that do not want the background goroutine to fire on its own can
// rely on threshold-triggered or explicit FlushLogs flushes only.
func slowFlushCfg(maxLines, maxBytes int) logstreamer.Config {
	return logstreamer.Config{
		MaxLinesPerChunk: maxLines,
		MaxBytesPerChunk: maxBytes,
		FlushInterval:    10 * time.Second,
	}
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestSequenceNumberMonotonicity verifies that chunk sequence numbers are
// strictly monotonically increasing from 1 (task 65).
func TestSequenceNumberMonotonicity(t *testing.T) {
	nc := &mockNATS{}
	// MaxLinesPerChunk=3 so every 3rd line triggers an immediate flush.
	cfg := slowFlushCfg(3, 1<<20)
	p := logstreamer.New(nc, cfg, discard())
	ctx := context.Background()

	const totalLines = 10
	for i := range totalLines {
		p.HandleLine(ctx, "task-seq", "attempt-seq", "sess", "stdout", fmt.Sprintf("line-%02d", i))
	}
	if err := p.FlushLogs(ctx, "task-seq", "attempt-seq"); err != nil {
		t.Fatalf("FlushLogs: %v", err)
	}

	msgs := nc.snapshot()
	if len(msgs) == 0 {
		t.Fatal("expected at least one published chunk, got none")
	}
	chunks := decodeAll(t, msgs)

	// Verify sequence starts at 1 and is strictly increasing.
	if chunks[0].SeqNum != 1 {
		t.Errorf("first chunk SeqNum = %d, want 1", chunks[0].SeqNum)
	}
	for i := 1; i < len(chunks); i++ {
		if chunks[i].SeqNum <= chunks[i-1].SeqNum {
			t.Errorf("seq not monotonically increasing at index %d: got %d after %d",
				i, chunks[i].SeqNum, chunks[i-1].SeqNum)
		}
	}

	// Verify all lines are present across the chunks.
	var allData strings.Builder
	for _, c := range chunks {
		allData.WriteString(c.Data)
		allData.WriteByte('\n')
	}
	for i := range totalLines {
		want := fmt.Sprintf("line-%02d", i)
		if !strings.Contains(allData.String(), want) {
			t.Errorf("published chunks missing line %q", want)
		}
	}
}

// TestChunkBoundaryLines verifies that a chunk is flushed immediately when
// MaxLinesPerChunk is reached (task 66).
func TestChunkBoundaryLines(t *testing.T) {
	nc := &mockNATS{}
	const maxLines = 3
	cfg := slowFlushCfg(maxLines, 1<<20)
	p := logstreamer.New(nc, cfg, discard())
	ctx := context.Background()

	// Write exactly maxLines lines — should trigger one immediate flush.
	for i := range maxLines {
		p.HandleLine(ctx, "task-lines", "attempt-lines", "sess", "stdout", fmt.Sprintf("L%d", i))
	}

	// The flush is synchronous inside HandleLine, so no sleep needed.
	if got := nc.count(); got != 1 {
		t.Fatalf("expected 1 chunk after %d lines, got %d", maxLines, got)
	}
	chunk := decodeChunk(t, nc.snapshot()[0])
	lines := strings.Split(chunk.Data, "\n")
	if len(lines) != maxLines {
		t.Errorf("chunk has %d lines, want %d; data = %q", len(lines), maxLines, chunk.Data)
	}

	// Write one more line — no immediate flush yet (buffer has 1 line < maxLines).
	p.HandleLine(ctx, "task-lines", "attempt-lines", "sess", "stdout", "extra")
	if got := nc.count(); got != 1 {
		t.Errorf("expected still 1 chunk before threshold, got %d", got)
	}

	// FlushLogs must publish the remaining line.
	if err := p.FlushLogs(ctx, "task-lines", "attempt-lines"); err != nil {
		t.Fatalf("FlushLogs: %v", err)
	}
	if got := nc.count(); got != 2 {
		t.Errorf("expected 2 chunks after FlushLogs, got %d", got)
	}
}

// TestChunkBoundaryBytes verifies that a chunk is flushed immediately when
// MaxBytesPerChunk is reached (task 66).
func TestChunkBoundaryBytes(t *testing.T) {
	nc := &mockNATS{}
	// Each line is 10 chars; byte accounting = 10+1=11 per line.
	// MaxBytesPerChunk=20: after line 2 (22 bytes), flush triggers.
	cfg := slowFlushCfg(1000, 20)
	p := logstreamer.New(nc, cfg, discard())
	ctx := context.Background()

	const line = "0123456789" // 10 bytes

	p.HandleLine(ctx, "task-bytes", "attempt-bytes", "sess", "stdout", line) // 11 bytes — no flush
	if got := nc.count(); got != 0 {
		t.Fatalf("expected 0 chunks after 1 line, got %d", got)
	}

	p.HandleLine(ctx, "task-bytes", "attempt-bytes", "sess", "stdout", line) // 22 bytes — flush
	if got := nc.count(); got != 1 {
		t.Fatalf("expected 1 chunk after byte threshold, got %d", got)
	}

	chunk := decodeChunk(t, nc.snapshot()[0])
	parts := strings.Split(chunk.Data, "\n")
	if len(parts) != 2 {
		t.Errorf("expected 2 lines in chunk, got %d; data=%q", len(parts), chunk.Data)
	}

	// The third line sits in the buffer until FlushLogs.
	p.HandleLine(ctx, "task-bytes", "attempt-bytes", "sess", "stdout", line)
	if got := nc.count(); got != 1 {
		t.Errorf("expected still 1 chunk before third line threshold, got %d", got)
	}
	if err := p.FlushLogs(ctx, "task-bytes", "attempt-bytes"); err != nil {
		t.Fatalf("FlushLogs: %v", err)
	}
	if got := nc.count(); got != 2 {
		t.Errorf("expected 2 total chunks after FlushLogs, got %d", got)
	}
}

// TestFlushBeforeTerminalStatus verifies the ordering guarantee: FlushLogs
// publishes all remaining buffered lines before it returns, ensuring the
// caller can safely publish a terminal status immediately after (task 68).
func TestFlushBeforeTerminalStatus(t *testing.T) {
	nc := &mockNATS{}
	// Large thresholds so no immediate flush fires during HandleLine calls.
	cfg := slowFlushCfg(1000, 1<<20)
	p := logstreamer.New(nc, cfg, discard())
	ctx := context.Background()

	const nLines = 5
	for i := range nLines {
		p.HandleLine(ctx, "task-order", "attempt-order", "sess", "stdout", fmt.Sprintf("line-%d", i))
	}

	// Before FlushLogs: nothing should have been published (chunk not full,
	// interval is 10 s so ticker won't fire during this test).
	if before := nc.count(); before != 0 {
		t.Fatalf("expected 0 messages before FlushLogs, got %d", before)
	}

	// FlushLogs must publish the buffered lines before returning.
	if err := p.FlushLogs(ctx, "task-order", "attempt-order"); err != nil {
		t.Fatalf("FlushLogs: %v", err)
	}

	// The "terminal status" is published here, after FlushLogs returns.
	// The ordering guarantee is that the lines are already published at this point.
	if after := nc.count(); after == 0 {
		t.Fatal("expected at least one chunk published by FlushLogs, got 0")
	}

	// Verify the published chunk contains all 5 lines.
	chunks := decodeAll(t, nc.snapshot())
	var allData strings.Builder
	for _, c := range chunks {
		if allData.Len() > 0 {
			allData.WriteByte('\n')
		}
		allData.WriteString(c.Data)
	}
	for i := range nLines {
		want := fmt.Sprintf("line-%d", i)
		if !strings.Contains(allData.String(), want) {
			t.Errorf("line %q missing from published chunks", want)
		}
	}
}

// TestFlushInterval verifies that the background goroutine flushes buffered
// lines within the configured interval (task 67).
func TestFlushInterval(t *testing.T) {
	nc := &mockNATS{}
	const interval = 50 * time.Millisecond
	cfg := logstreamer.Config{
		MaxLinesPerChunk: 1000,
		MaxBytesPerChunk: 1 << 20,
		FlushInterval:    interval,
	}
	p := logstreamer.New(nc, cfg, discard())
	ctx := context.Background()

	p.HandleLine(ctx, "task-tick", "attempt-tick", "sess", "stdout", "hello")

	// Nothing published immediately (well under chunk thresholds).
	if got := nc.count(); got != 0 {
		t.Fatalf("expected 0 chunks immediately after HandleLine, got %d", got)
	}

	// After waiting longer than the interval, the background ticker must have
	// flushed the line.  Use 5× the interval for tolerance on slow CI hosts.
	deadline := time.Now().Add(5 * interval)
	for time.Now().Before(deadline) {
		if nc.count() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := nc.count(); got == 0 {
		t.Fatalf("expected at least one chunk after %v, got 0", 5*interval)
	}

	// Clean up — stop the background goroutine.
	if err := p.FlushLogs(ctx, "task-tick", "attempt-tick"); err != nil {
		t.Errorf("cleanup FlushLogs: %v", err)
	}
}

// TestStdoutStderrSeparateChunks verifies that stdout and stderr lines are
// published in separate chunks with the correct Stream tag (task 64).
func TestStdoutStderrSeparateChunks(t *testing.T) {
	nc := &mockNATS{}
	cfg := slowFlushCfg(1000, 1<<20)
	p := logstreamer.New(nc, cfg, discard())
	ctx := context.Background()

	p.HandleLine(ctx, "task-streams", "attempt-streams", "sess", "stdout", "out-line")
	p.HandleLine(ctx, "task-streams", "attempt-streams", "sess", "stderr", "err-line")

	if err := p.FlushLogs(ctx, "task-streams", "attempt-streams"); err != nil {
		t.Fatalf("FlushLogs: %v", err)
	}

	msgs := nc.snapshot()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 chunks (one per stream), got %d", len(msgs))
	}

	streams := map[string]string{}
	for _, m := range msgs {
		c := decodeChunk(t, m)
		streams[c.Stream] = c.Data
	}
	if data, ok := streams["stdout"]; !ok || data != "out-line" {
		t.Errorf("stdout chunk: want data=%q, got streams=%v", "out-line", streams)
	}
	if data, ok := streams["stderr"]; !ok || data != "err-line" {
		t.Errorf("stderr chunk: want data=%q, got streams=%v", "err-line", streams)
	}
}

// TestMultipleAttemptsIndependentSequences verifies that concurrent task
// attempts have independent, non-interleaved sequence counters.
func TestMultipleAttemptsIndependentSequences(t *testing.T) {
	nc := &mockNATS{}
	cfg := slowFlushCfg(3, 1<<20)
	p := logstreamer.New(nc, cfg, discard())
	ctx := context.Background()

	// Write 6 lines for attempt-A (triggers two immediate flushes).
	for i := range 6 {
		p.HandleLine(ctx, "task-multi", "attempt-A", "sess", "stdout", fmt.Sprintf("A-%d", i))
	}
	// Write 3 lines for attempt-B (triggers one immediate flush).
	for i := range 3 {
		p.HandleLine(ctx, "task-multi", "attempt-B", "sess", "stdout", fmt.Sprintf("B-%d", i))
	}
	if err := p.FlushLogs(ctx, "task-multi", "attempt-A"); err != nil {
		t.Fatalf("FlushLogs attempt-A: %v", err)
	}
	if err := p.FlushLogs(ctx, "task-multi", "attempt-B"); err != nil {
		t.Fatalf("FlushLogs attempt-B: %v", err)
	}

	msgs := nc.snapshot()
	seqA := make([]int64, 0)
	seqB := make([]int64, 0)
	for _, m := range msgs {
		c := decodeChunk(t, m)
		switch c.AttemptID {
		case "attempt-A":
			seqA = append(seqA, c.SeqNum)
		case "attempt-B":
			seqB = append(seqB, c.SeqNum)
		}
	}
	if len(seqA) == 0 || len(seqB) == 0 {
		t.Fatalf("expected chunks for both attempts; seqA=%v seqB=%v", seqA, seqB)
	}

	// Each attempt's seq numbers must start at 1 and be strictly increasing.
	checkMono := func(name string, seqs []int64) {
		t.Helper()
		if seqs[0] != 1 {
			t.Errorf("%s: first seq = %d, want 1", name, seqs[0])
		}
		for i := 1; i < len(seqs); i++ {
			if seqs[i] <= seqs[i-1] {
				t.Errorf("%s: seq not monotonically increasing at index %d: %d <= %d",
					name, i, seqs[i], seqs[i-1])
			}
		}
	}
	checkMono("attempt-A", seqA)
	checkMono("attempt-B", seqB)
}

// TestFlushLogsIdempotent verifies that calling FlushLogs twice for the
// same attempt is safe and does not panic or publish duplicate chunks.
func TestFlushLogsIdempotent(t *testing.T) {
	nc := &mockNATS{}
	cfg := slowFlushCfg(100, 1<<20)
	p := logstreamer.New(nc, cfg, discard())
	ctx := context.Background()

	p.HandleLine(ctx, "task-idem", "attempt-idem", "sess", "stdout", "hello")

	if err := p.FlushLogs(ctx, "task-idem", "attempt-idem"); err != nil {
		t.Fatalf("first FlushLogs: %v", err)
	}
	countAfterFirst := nc.count()

	// Second call should be a no-op.
	if err := p.FlushLogs(ctx, "task-idem", "attempt-idem"); err != nil {
		t.Fatalf("second FlushLogs: %v", err)
	}
	if got := nc.count(); got != countAfterFirst {
		t.Errorf("second FlushLogs published %d extra messages, want 0", got-countAfterFirst)
	}
}

// TestFlushLogsNoLines verifies that FlushLogs on an attempt that never
// received any lines returns nil and publishes nothing.
func TestFlushLogsNoLines(t *testing.T) {
	nc := &mockNATS{}
	p := logstreamer.New(nc, logstreamer.DefaultConfig(), discard())
	ctx := context.Background()

	if err := p.FlushLogs(ctx, "task-empty", "attempt-empty"); err != nil {
		t.Fatalf("FlushLogs: %v", err)
	}
	if got := nc.count(); got != 0 {
		t.Errorf("expected 0 messages for empty attempt, got %d", got)
	}
}

// TestNATSSubject verifies that log chunks are published to the correct
// task.logs.<taskID> NATS subject.
func TestNATSSubject(t *testing.T) {
	nc := &mockNATS{}
	cfg := slowFlushCfg(1, 1<<20) // flush on the first line
	p := logstreamer.New(nc, cfg, discard())
	ctx := context.Background()

	const taskID = "my-task-id"
	p.HandleLine(ctx, taskID, "attempt-subj", "sess", "stdout", "line")

	msgs := nc.snapshot()
	if len(msgs) == 0 {
		t.Fatal("no messages published")
	}
	want := bus.TaskLogsSubject(taskID)
	for _, m := range msgs {
		if m.subject != want {
			t.Errorf("subject = %q, want %q", m.subject, want)
		}
	}
}

// TestPublishErrorDoesNotPanic verifies that a NATS publish error is logged
// as a warning and does not propagate out to HandleLine or FlushLogs.
func TestPublishErrorDoesNotPanic(t *testing.T) {
	nc := &mockNATS{pubErr: errors.New("nats: connection closed")}
	cfg := slowFlushCfg(1, 1<<20) // flush on the first line
	p := logstreamer.New(nc, cfg, discard())
	ctx := context.Background()

	// Should not panic even though NATS publish returns an error.
	p.HandleLine(ctx, "task-err", "attempt-err", "sess", "stdout", "line")
	if err := p.FlushLogs(ctx, "task-err", "attempt-err"); err != nil {
		t.Fatalf("FlushLogs: %v", err)
	}
}

// TestDefaultConfig verifies DefaultConfig returns sensible non-zero values.
func TestDefaultConfig(t *testing.T) {
	cfg := logstreamer.DefaultConfig()
	if cfg.MaxLinesPerChunk <= 0 {
		t.Errorf("MaxLinesPerChunk = %d, want > 0", cfg.MaxLinesPerChunk)
	}
	if cfg.MaxBytesPerChunk <= 0 {
		t.Errorf("MaxBytesPerChunk = %d, want > 0", cfg.MaxBytesPerChunk)
	}
	if cfg.FlushInterval <= 0 {
		t.Errorf("FlushInterval = %v, want > 0", cfg.FlushInterval)
	}
}
