// SPDX-License-Identifier: AGPL-3.0-or-later

// Package logstreamer implements the log-chunk publisher for sqi-worker.
//
// A [Publisher] implements [executor.OutputHandler] and accumulates process
// output lines from stdout and stderr into [protocol.LogChunkMsg] batches,
// publishing them to the task.logs.<workerID>.<taskID> NATS JetStream subject.
//
// # Chunking
//
// Lines are buffered per-stream (stdout / stderr) per task attempt.
// A chunk is flushed immediately when:
//   - the buffered line count reaches [Config.MaxLinesPerChunk], or
//   - the accumulated byte count reaches [Config.MaxBytesPerChunk].
//
// A background per-attempt goroutine also flushes buffered content at
// [Config.FlushInterval] intervals so the web UI log viewer feels live even
// for slowly printing processes.
//
// # Sequence numbers
//
// Each published chunk carries a monotonic [protocol.LogChunkMsg.SeqNum]
// starting at 1 for the first chunk of an attempt.  Seq numbers increment
// across both stdout and stderr chunks so the server can replay all chunks in
// publication order regardless of NATS delivery order.
//
// # Delivery
//
// A flushed chunk is encoded and queued for delivery; it is never published
// inline, so a slow or broken broker does not hold up the process's output.
// One sender goroutine delivers the queue through a [Transport], putting up
// to 32 chunks on the wire in order before it waits for their acks, so a
// worker far from the broker is not held to one chunk per round trip. It tries
// each chunk again, with backoff, until the log stream stores it. Every try of
// a chunk carries the message ID "<attemptID>/<seq>", so the stream drops a
// retry it already stored, and the server keeps one copy per seq. The
// queue holds at most 32 MiB of encoded chunks; past that the oldest are
// dropped, with a warning at most once a minute.
//
// # Lifecycle
//
// Call [Publisher.FlushLogs] after the process exits to drain all remaining
// buffered lines and stop the background flush goroutine. FlushLogs then
// waits up to 5 s for the attempt's chunks to be stored, so in the normal
// case all log output reaches the server before the terminal status message
// is published; a chunk still unstored after the wait keeps being retried and
// lands after it. At worker shutdown [Publisher.Flush] delivers what is still
// queued, for as long as its context allows; [Publisher.Abandon] drops it.
package logstreamer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// ── Config ────────────────────────────────────────────────────────────────────

// Config holds the tunable parameters for the log-chunk [Publisher].
// Zero or negative values are replaced by the package defaults when
// [New] is called.
type Config struct {
	// MaxLinesPerChunk is the maximum number of lines batched into a single
	// NATS message.  A flush is triggered immediately when the buffer reaches
	// this count.  Default: 50.
	MaxLinesPerChunk int

	// MaxBytesPerChunk is the maximum total byte count of line content in a
	// single NATS message.  A flush is triggered when the accumulated bytes
	// would meet or exceed this limit after adding a line.  Default: 16384.
	MaxBytesPerChunk int

	// FlushInterval is the maximum time a line may sit in the buffer before
	// being flushed regardless of chunk thresholds.  Keeps the web UI log
	// viewer feeling live.  Default: 500 ms.
	FlushInterval time.Duration

	// Now stamps each chunk's At; nil is time.Now.
	Now func() time.Time

	// Delivery settings, the package defaults when zero. Unexported: they
	// are not operator configuration, and only tests change them.
	ackTimeout      time.Duration
	retryBase       time.Duration
	retryMax        time.Duration
	flushWait       time.Duration
	maxPendingBytes int
	window          int
}

// DefaultConfig returns a [Config] populated with production-sensible defaults.
func DefaultConfig() Config {
	return Config{
		MaxLinesPerChunk: 50,
		MaxBytesPerChunk: 16 * 1024,
		FlushInterval:    500 * time.Millisecond,
	}
}

// applyDefaults replaces any zero or negative fields with the values from
// [DefaultConfig], keeping a single source of truth for default values.
func (c *Config) applyDefaults() {
	d := DefaultConfig()
	if c.MaxLinesPerChunk <= 0 {
		c.MaxLinesPerChunk = d.MaxLinesPerChunk
	}
	if c.MaxBytesPerChunk <= 0 {
		c.MaxBytesPerChunk = d.MaxBytesPerChunk
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = d.FlushInterval
	}
	if c.ackTimeout <= 0 {
		c.ackTimeout = defaultAckTimeout
	}
	if c.retryBase <= 0 {
		c.retryBase = defaultRetryBase
	}
	if c.retryMax <= 0 {
		c.retryMax = defaultRetryMax
	}
	if c.flushWait <= 0 {
		c.flushWait = defaultFlushWait
	}
	if c.maxPendingBytes <= 0 {
		c.maxPendingBytes = defaultMaxPendingBytes
	}
	if c.window <= 0 {
		c.window = defaultWindow
	}
}

// ── bufState ──────────────────────────────────────────────────────────────────

// bufState holds the accumulated lines for a single output stream (stdout or
// stderr) within one task attempt.
type bufState struct {
	lines []string
	bytes int // sum of len(line)+1 for each buffered line (the +1 accounts for the newline separator)
}

// reset clears the buffer and returns the accumulated lines for publication.
// The returned slice is safe to use after the call — a new backing array will
// be allocated on the next append.
func (b *bufState) reset() []string {
	lines := b.lines
	b.lines = nil
	b.bytes = 0
	return lines
}

// ── attemptBuf ────────────────────────────────────────────────────────────────

// attemptBuf holds the complete per-attempt runtime state for one task
// execution's log streaming.
type attemptBuf struct {
	taskID    string
	attemptID string

	// seqNum is the monotonic chunk sequence counter for this attempt.
	// It is incremented atomically before each publish so that stdout and
	// stderr chunks share a single, strictly increasing counter per attempt.
	seqNum atomic.Int64

	mu     sync.Mutex
	stdout bufState
	stderr bufState

	// done is closed by [Publisher.FlushLogs] to signal the background flush
	// goroutine to stop after completing any in-progress periodic flush.
	done chan struct{}

	// wg tracks the background flush goroutine.  FlushLogs waits on it before
	// performing the final synchronous drain.
	wg sync.WaitGroup
}

// streamBuf returns a pointer to the [bufState] for the given stream name.
// Any unrecognized stream name falls through to stdout to avoid silent data
// loss.  Must be called with attemptBuf.mu held.
func (b *attemptBuf) streamBuf(stream string) *bufState {
	if stream == "stderr" {
		return &b.stderr
	}
	return &b.stdout
}

// ── Publisher ─────────────────────────────────────────────────────────────────

// Publisher implements executor.OutputHandler, accumulating process output
// lines from concurrent task goroutines into batched [protocol.LogChunkMsg]
// messages delivered to NATS JetStream.
//
// Publisher is safe for concurrent use from multiple goroutines.
type Publisher struct {
	out    *outbox
	logger *slog.Logger
	cfg    Config

	// workerID is this worker's stable identity. It is a subject token on
	// every chunk published, which is how the server attributes log output to
	// the worker that produced it.
	workerID string

	// mu protects the attempts map.
	mu       sync.Mutex
	attempts map[string]*attemptBuf // keyed by attemptID
}

// New returns a ready-to-use Publisher publishing as workerID through t.
// Any zero or negative fields in cfg are replaced with defaults before use.
func New(t Transport, workerID string, cfg Config, logger *slog.Logger) *Publisher {
	cfg.applyDefaults()
	return &Publisher{
		out:      newOutbox(t, cfg, logger),
		logger:   logger,
		cfg:      cfg,
		workerID: workerID,
		attempts: make(map[string]*attemptBuf),
	}
}

// HandleLine implements executor.OutputHandler.
//
// It appends line to the in-memory buffer for (taskID, attemptID, stream).
// When the buffer meets the MaxLinesPerChunk or MaxBytesPerChunk threshold, a
// chunk is flushed immediately: HandleLine queues it for delivery before it
// returns, without waiting on the broker. The per-attempt background goroutine
// handles the periodic FlushInterval drain between threshold events.
//
// Concurrency note: threshold-triggered flushes happen synchronously inside
// HandleLine, so when all HandleLine callers return (i.e., when the executor's
// readWg.Wait() completes after process exit), no threshold flush is in flight.
// This is why [FlushLogs] can safely drain the remaining buffer without a
// concurrent write racing against it.
func (p *Publisher) HandleLine(_ context.Context, taskID, attemptID, _ /* sessionID */, stream, line string) {
	buf := p.getOrCreate(taskID, attemptID)

	buf.mu.Lock()
	s := buf.streamBuf(stream)
	s.lines = append(s.lines, line)
	s.bytes += len(line) + 1 // +1 for the joining newline

	shouldFlush := len(s.lines) >= p.cfg.MaxLinesPerChunk || s.bytes >= p.cfg.MaxBytesPerChunk
	var toFlush []string
	if shouldFlush {
		toFlush = s.reset()
	}
	buf.mu.Unlock()

	if shouldFlush {
		// Use a background context so threshold-triggered flushes always
		// complete even if the worker context is being canceled during shutdown.
		p.publishChunk(context.Background(), buf, stream, toFlush)
	}
}

// FlushLogs implements the executor.LogFlusher interface.
//
// It signals the background flush goroutine to stop, waits for it to exit,
// then performs a final synchronous drain of any remaining buffered lines for
// both stdout and stderr.  FlushLogs MUST be called after the task process has
// exited and before the terminal status message is published. It waits up to
// the flush wait (5 s) for the attempt's chunks to be stored, so in the normal
// case all log output reaches the server first.
//
// Safety: the executor calls FlushLogs only after readWg.Wait() completes
// (all scanOutput goroutines — and thus all HandleLine calls — have returned).
// Because threshold-triggered flushes are synchronous inside HandleLine,
// there are no in-flight flushes for this attempt when FlushLogs runs.
// The only concurrent publisher is the background flushLoop goroutine, which
// is stopped via close(buf.done) + wg.Wait() before the final drain.
//
// FlushLogs is idempotent: if no lines were ever received for the given
// attempt, or if FlushLogs has already been called, it returns nil immediately.
func (p *Publisher) FlushLogs(_ context.Context, taskID, attemptID string) error {
	_ = taskID // looked up by attemptID; taskID is already embedded in the buf
	p.mu.Lock()
	buf, ok := p.attempts[attemptID]
	if ok {
		delete(p.attempts, attemptID)
	}
	p.mu.Unlock()

	if !ok {
		// No lines were received for this attempt (e.g., a task with no output).
		return nil
	}

	// Signal the background goroutine to stop.  It exits after any in-progress
	// periodic flush completes; it does NOT perform a final flush itself.
	close(buf.done)
	buf.wg.Wait() // wait for the goroutine to exit before the final drain

	// Final synchronous drain — consume any lines that arrived between the
	// goroutine's last periodic flush and now.  Use context.Background() so
	// the drain completes even if the caller's context is already canceled
	// (e.g., during worker shutdown).
	p.drainBuf(context.Background(), buf, "stdout")
	p.drainBuf(context.Background(), buf, "stderr")

	// Wait, for a bounded time, for the stream to store this attempt's
	// chunks, so they reach the server before its terminal status. A broken
	// log stream must not hold the status for long: what is still unstored
	// after the wait keeps being retried and lands after it.
	p.out.waitAttempt(buf.attemptID, p.cfg.flushWait)
	return nil
}

// Flush delivers the chunks still queued until none is left or ctx ends, then
// stops delivery for good and returns how many chunks it dropped, logging them.
// It is the worker's shutdown call: it must run while the connection is open.
func (p *Publisher) Flush(ctx context.Context) (dropped int) {
	dropped = p.out.flush(ctx)
	if dropped > 0 {
		p.logger.WarnContext(ctx, "logstreamer: shutdown left log chunks unsent", slog.Int("dropped", dropped))
	}
	return dropped
}

// Abandon stops delivery for good without waiting on the broker and returns
// how many queued chunks it dropped. It is the crash path: a crashed worker's
// unsent lines are lost.
func (p *Publisher) Abandon() (dropped int) {
	return p.out.close()
}

// ── Internal helpers ──────────────────────────────────────────────────────────

// now is the wire clock: cfg.Now, or time.Now when unset.
func (p *Publisher) now() time.Time {
	if p.cfg.Now != nil {
		return p.cfg.Now()
	}
	return time.Now()
}

// getOrCreate returns the existing [attemptBuf] for attemptID, or creates and
// registers a new one and starts its background flush goroutine.
func (p *Publisher) getOrCreate(taskID, attemptID string) *attemptBuf {
	p.mu.Lock()
	buf, ok := p.attempts[attemptID]
	if !ok {
		buf = &attemptBuf{
			taskID:    taskID,
			attemptID: attemptID,
			done:      make(chan struct{}),
		}
		p.attempts[attemptID] = buf
		buf.wg.Add(1)
		go p.flushLoop(buf)
	}
	p.mu.Unlock()
	return buf
}

// flushLoop is the per-attempt background goroutine that periodically drains
// buffered output lines. It exits when buf.done is closed.
func (p *Publisher) flushLoop(buf *attemptBuf) {
	defer buf.wg.Done()

	ticker := time.NewTicker(p.cfg.FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			p.drainBuf(context.Background(), buf, "stdout")
			p.drainBuf(context.Background(), buf, "stderr")
		case <-buf.done:
			// FlushLogs performs the final synchronous drain after we return.
			return
		}
	}
}

// drainBuf takes a snapshot of one stream's buffer under the lock, releases
// the lock, and publishes the captured lines as a single chunk.  It is a no-op
// if the buffer is empty.
func (p *Publisher) drainBuf(ctx context.Context, buf *attemptBuf, stream string) {
	buf.mu.Lock()
	s := buf.streamBuf(stream)
	if len(s.lines) == 0 {
		buf.mu.Unlock()
		return
	}
	lines := s.reset()
	buf.mu.Unlock()

	p.publishChunk(ctx, buf, stream, lines)
}

// publishChunk assigns the next sequence number, encodes a [protocol.LogChunkMsg]
// and queues it for the task.logs.<workerID>.<taskID> NATS subject.
// It is a no-op if lines is empty.
//
// SeqNum assignment and queueing are not atomic. When the flushLoop
// goroutine and a threshold-triggered path from HandleLine run concurrently,
// a chunk with a higher SeqNum may be queued before one with a lower SeqNum,
// and a chunk dropped by the bound leaves a gap. Both are expected: the server
// stores SeqNum for ordered replay (the NATS JetStream sequence assigned by
// the server is the persistence-order number; SeqNum is the logical counter).
// See protocol.LogChunkMsg.SeqNum documentation.
func (p *Publisher) publishChunk(ctx context.Context, buf *attemptBuf, stream string, lines []string) {
	if len(lines) == 0 {
		return
	}

	seq := buf.seqNum.Add(1) // monotonic, starting at 1

	msg := protocol.LogChunkMsg{
		Version:   protocol.ProtocolVersion,
		Type:      protocol.TypeLogChunk,
		TaskID:    buf.taskID,
		AttemptID: buf.attemptID,
		SeqNum:    seq,
		At:        p.now(),
		Stream:    stream,
		Data:      strings.Join(lines, "\n"),
	}

	data, err := json.Marshal(msg)
	if err != nil {
		p.logger.WarnContext(
			ctx, "logstreamer: marshal log chunk failed",
			slog.String("task_id", buf.taskID),
			slog.String("attempt_id", buf.attemptID),
			slog.String("stream", stream),
			slog.Int64("seq_num", seq),
			slog.Any("error", err),
		)
		return
	}

	p.out.enqueue(&pendingChunk{
		subj:      bus.TaskLogsSubject(p.workerID, buf.taskID),
		msgID:     fmt.Sprintf("%s/%d", buf.attemptID, seq),
		attemptID: buf.attemptID,
		data:      data,
	})
}
