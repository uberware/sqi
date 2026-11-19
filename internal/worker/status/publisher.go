// SPDX-License-Identifier: AGPL-3.0-or-later

// Package status implements the typed task-status publisher for sqi-worker.
//
// A [Publisher] sends the status reports the worker owes the server for each
// attempt it runs: the attempt's first "running" and its terminal status
// ("succeeded", "failed", "canceled", or a "failed" carrying
// [protocol.MessageWorkerShutdown]). Each goes to the
// task.status.<workerID>.<job> subject through a [Transport] that returns only
// once the SQI_TASK stream has stored it. Progress snapshots and any later
// "running" are not sent here: the OpenJD interceptor
// (internal/worker/openjd) publishes those best-effort with core NATS.
//
// # Acknowledgement and retry
//
// Every report carries a Nats-Msg-Id of "<attemptID>:<kind>", where kind is
// running, succeeded, failed, canceled or shutdown, so a retry of a report the
// stream already stored is dropped by the stream's de-duplication window, and
// a run's own "failed" and a shutdown "failed" for one attempt do not
// de-duplicate each other.
//
// The first try runs inline, bounded by [Config.AckTimeout], so a report the
// broker takes at once is stored before the caller moves on, and a broker
// that does not answer costs the caller at most one AckTimeout. A report that
// is not acknowledged stays pending and is retried in the background with
// exponential backoff from [Config.RetryBase] to [Config.RetryMax], with no
// limit on tries, until it is stored or [Publisher.Flush] drops it. Only a
// failure no retry can cure is logged at Error and dropped at once: a marshal
// failure, a message over the broker's maximum payload, or a closed
// connection, which never reopens. A publish the broker denies surfaces as an
// ack timeout and is retried like any other. A terminal report drops a
// still-pending "running" for the same attempt, since a terminal status is
// legal on an assigned task and a later "running" would be refused as stale.
//
// [Publisher.PendingAttemptIDs] names the attempts with a report still
// pending, and [Publisher.WithPending] adds them to the attempts a worker
// holds, so its heartbeats keep listing them. [Publisher.Flush] waits for the
// pending reports at shutdown, and [Publisher.Abandon] drops them without
// waiting.
//
// # Fields
//
// Every message carries worker_id from [Config.WorkerID]. A terminal report
// also carries ended_ago_ms, how long ago the task ended by the worker's
// monotonic clock, measured again on every try, from which the server places
// the end on its own timeline.
package status

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// ── Defaults ──────────────────────────────────────────────────────────────────

// DefaultAckTimeout is [Config.AckTimeout] when it is left zero. It bounds
// the inline try of a terminal status a task hands over after the shutdown
// drain, so it is part of the worker's shutdown budget.
const DefaultAckTimeout = 2 * time.Second

// DefaultFlushTimeout is how long a worker's graceful shutdown waits in
// [Publisher.Flush] for the reports still pending after its drain. It is
// separate from the drain's grace period, which is spent by then, and part
// of the worker's shutdown budget.
const DefaultFlushTimeout = 10 * time.Second

const (
	defaultRetryBase = 250 * time.Millisecond
	defaultRetryMax  = 5 * time.Second

	// warnEvery is the shortest spacing between two Warn records for one
	// report that keeps failing.
	warnEvery = time.Minute

	kindRunning  = "running"
	kindShutdown = "shutdown"

	msgDropped = "status: dropped a report the stream never acknowledged"
)

// ── Transport ─────────────────────────────────────────────────────────────────

// Transport sends one report and returns nil only once SQI_TASK has stored it.
type Transport interface {
	PublishAcked(ctx context.Context, subj string, data []byte, msgID string) error
}

// ── Config ────────────────────────────────────────────────────────────────────

// Config holds tunable parameters for a [Publisher].
// Zero-value durations are replaced with package defaults when [New] is called.
type Config struct {
	// WorkerID is embedded in every outgoing status message.
	WorkerID string

	// AckTimeout bounds each try, the wait for the stream's ack included.
	// Default: 2 s.
	AckTimeout time.Duration

	// RetryBase is the wait before the first retry; each later wait doubles,
	// up to RetryMax. Default: 250 ms.
	RetryBase time.Duration

	// RetryMax caps the wait between retries. Default: 5 s.
	RetryMax time.Duration

	// WireOffset is added to the At timestamp of every message, and to
	// nothing else: ended_ago_ms is still measured on the real clock. It is
	// zero on a real worker; sqi-load sets it to simulate a worker whose
	// clock is skewed.
	WireOffset time.Duration
}

// ── Publisher ─────────────────────────────────────────────────────────────────

// entryKey names one pending report.
type entryKey struct {
	attemptID string
	kind      string
}

// entry is one report waiting for the stream's ack.
type entry struct {
	key   entryKey
	msgID string // the Nats-Msg-Id, "<attemptID>:<kind>"
	subj  string
	msg   protocol.TaskStatusMsg

	// endedAt is when a terminal report's task ended, with its monotonic
	// reading. It is zero exactly for a "running": Terminal replaces a zero
	// end with the time of the call.
	endedAt time.Time

	// ctx ends when the entry is acked, dropped or stopped, which aborts a
	// try in flight.
	ctx    context.Context
	cancel context.CancelFunc

	// done is closed when the entry's retry goroutine exits; nil until one
	// starts. It is written under Publisher.mu and only while the entry is
	// pending ([Publisher.startRetry]), so once the entry has left pending it
	// no longer changes.
	done chan struct{}

	// Owned by whichever goroutine is sending the entry: the caller for the
	// first try, the retry goroutine after it.
	failures int
	lastWarn time.Time
}

// Publisher sends task-status reports and retries each until the SQI_TASK
// stream acknowledges it.
//
// Publisher is safe for concurrent use from multiple goroutines.
type Publisher struct {
	t      Transport
	cfg    Config
	logger *slog.Logger
	since  func(time.Time) time.Duration

	mu      sync.Mutex
	pending map[entryKey]*entry
	changed chan struct{} // closed and replaced whenever an entry leaves pending
}

// New returns a ready-to-use Publisher.
// Any zero or negative durations in cfg are replaced with defaults before use.
func New(t Transport, cfg Config, logger *slog.Logger) *Publisher {
	if cfg.AckTimeout <= 0 {
		cfg.AckTimeout = DefaultAckTimeout
	}
	if cfg.RetryBase <= 0 {
		cfg.RetryBase = defaultRetryBase
	}
	if cfg.RetryMax <= 0 {
		cfg.RetryMax = defaultRetryMax
	}
	return &Publisher{
		t:       t,
		cfg:     cfg,
		logger:  logger,
		since:   time.Since,
		pending: map[entryKey]*entry{},
		changed: make(chan struct{}),
	}
}

// ── Public methods ────────────────────────────────────────────────────────────

// Running reports an attempt's first "running", at the time the task was
// started. It returns once the stream has stored the report or its first try
// has failed; a failed report is retried in the background.
func (p *Publisher) Running(msg *protocol.AssignMsg, sessionID string, at time.Time) {
	p.send(p.newEntry(msg, sessionID, kindRunning, kindRunning, at))
}

// Terminal reports an attempt's terminal status: "succeeded", "failed", or
// "canceled". It returns once the stream has stored the report or its first
// try has failed; a failed report is retried in the background. A "running"
// for the same attempt that is still pending is dropped.
//
// exitCode should be nil where no exit code is meaningful (a canceled or
// never-started process). failureReason becomes the message's Message; a
// "failed" whose reason is [protocol.MessageWorkerShutdown] is the worker
// giving the attempt back. lastProgress is the most-recently seen
// openjd_progress value, or nil. endedAt is when the task ended; it must keep
// its monotonic clock reading (do not pass it through UTC, Round or JSON),
// since ended_ago_ms is measured from it on every try. A zero endedAt is
// taken as the time of the call.
func (p *Publisher) Terminal(
	msg *protocol.AssignMsg,
	sessionID, status string,
	exitCode *int,
	failureReason string,
	lastProgress *int,
	endedAt time.Time,
) {
	if endedAt.IsZero() {
		endedAt = time.Now() // no end recorded: report it as now, not ~292 years ago
	}
	kind := status
	if status == "failed" && failureReason == protocol.MessageWorkerShutdown {
		kind = kindShutdown
	}
	e := p.newEntry(msg, sessionID, status, kind, endedAt)
	e.msg.Message = failureReason
	e.msg.ExitCode = exitCode
	e.msg.LastProgress = lastProgress
	e.endedAt = endedAt
	p.send(e)
}

// PendingAttemptIDs returns the attempts with a report not yet acknowledged,
// sorted and de-duplicated.
func (p *Publisher) PendingAttemptIDs() []string {
	p.mu.Lock()
	ids := make([]string, 0, len(p.pending))
	for k := range p.pending {
		ids = append(ids, k.attemptID)
	}
	p.mu.Unlock()
	slices.Sort(ids)
	return slices.Compact(ids)
}

// WithPending returns held together with the attempts that have a report not
// yet acknowledged, sorted and de-duplicated: the attempts a worker's
// heartbeat lists. The server reclaims an open attempt once heartbeats stop
// listing it, so one whose terminal report is still being retried must stay
// listed after the worker has released it. WithPending may reuse held's
// backing array.
func (p *Publisher) WithPending(held []string) []string {
	held = append(held, p.PendingAttemptIDs()...)
	slices.Sort(held)
	return slices.Compact(held)
}

// Flush waits until no report is pending or ctx ends. If ctx ends first, it
// stops and drops every report still pending, logging one Warn for each, and
// returns how many it dropped. Reports handed over after Flush returns are
// sent as usual.
func (p *Publisher) Flush(ctx context.Context) (dropped int) {
	for {
		p.mu.Lock()
		if len(p.pending) == 0 {
			p.mu.Unlock()
			return 0
		}
		changed := p.changed
		p.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return p.dropAll(ctx)
		}
	}
}

// Abandon stops and drops every report still pending without waiting for
// any, logging one Warn for each, and returns how many it dropped. It is for
// a worker that ends without finishing its sends. Reports handed over after
// Abandon returns are sent as usual.
func (p *Publisher) Abandon() (dropped int) {
	return p.dropAll(context.Background())
}

// ── Internal helpers ──────────────────────────────────────────────────────────

// newEntry builds the report of kind on msg's attempt, with the fields every
// report carries and its At set to at, moved by [Config.WireOffset].
func (p *Publisher) newEntry(msg *protocol.AssignMsg, sessionID, status, kind string, at time.Time) *entry {
	return &entry{
		key:   entryKey{attemptID: msg.AttemptID, kind: kind},
		msgID: msg.AttemptID + ":" + kind,
		subj:  bus.TaskStatusSubject(p.cfg.WorkerID, msg.JobID),
		msg: protocol.TaskStatusMsg{
			Version:   protocol.ProtocolVersion,
			Type:      protocol.TypeTaskStatus,
			TaskID:    msg.TaskID,
			AttemptID: msg.AttemptID,
			JobID:     msg.JobID,
			Status:    status,
			SessionID: sessionID,
			WorkerID:  p.cfg.WorkerID,
			At:        at.Add(p.cfg.WireOffset),
		},
	}
}

// terminal reports whether e is a terminal report rather than a "running".
func (e *entry) terminal() bool { return !e.endedAt.IsZero() }

// send makes e pending, stopping the entries it replaces, then tries it once
// inline and hands it to a retry goroutine if that try fails.
func (p *Publisher) send(e *entry) {
	e.ctx, e.cancel = context.WithCancel(context.Background())

	keys := []entryKey{e.key}
	if e.terminal() {
		keys = append(keys, entryKey{attemptID: e.key.attemptID, kind: kindRunning})
	}
	var replaced []*entry
	p.mu.Lock()
	for _, k := range keys {
		if r := p.takeLocked(k); r != nil {
			replaced = append(replaced, r)
		}
	}
	p.pending[e.key] = e
	p.mu.Unlock()
	for _, r := range replaced {
		r.stop()
	}

	if p.try(e) {
		p.startRetry(e)
	} else {
		p.finish(e)
	}
}

// takeLocked removes the entry under k from pending and returns it, or nil
// when there is none. p.mu must be held.
func (p *Publisher) takeLocked(k entryKey) *entry {
	e, ok := p.pending[k]
	if !ok {
		return nil
	}
	delete(p.pending, k)
	p.notifyLocked()
	return e
}

// stop aborts e's try in flight and waits for its retry goroutine, if it has
// one, to exit. Call it only once e has left pending: from then on e.done
// does not change (startRetry starts no goroutine for an entry that is no
// longer pending), so reading it here needs no lock, and a nil done means
// there is no goroutine to wait for.
func (e *entry) stop() {
	e.cancel()
	if e.done != nil {
		<-e.done
	}
}

// notifyLocked wakes every Flush waiting for pending to change. p.mu must be
// held.
func (p *Publisher) notifyLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

// finish removes e from pending, unless something already replaced or
// dropped it, and releases its context.
func (p *Publisher) finish(e *entry) {
	p.mu.Lock()
	if p.pending[e.key] == e {
		delete(p.pending, e.key)
		p.notifyLocked()
	}
	p.mu.Unlock()
	e.cancel()
}

// startRetry starts e's retry goroutine, unless e has already left pending.
func (p *Publisher) startRetry(e *entry) {
	p.mu.Lock()
	if p.pending[e.key] != e {
		p.mu.Unlock()
		e.cancel()
		return
	}
	e.done = make(chan struct{})
	p.mu.Unlock()
	go p.retry(e)
}

// retry tries e again, with exponential backoff, until it is acked, fails
// permanently or is stopped.
func (p *Publisher) retry(e *entry) {
	defer close(e.done)
	delay := p.cfg.RetryBase
	for {
		timer := time.NewTimer(delay)
		select {
		case <-e.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if !p.try(e) {
			p.finish(e)
			return
		}
		delay = min(delay*2, p.cfg.RetryMax)
	}
}

// try sends e once, logging the outcome as the rate limit allows, and
// reports whether e should be tried again: false once it is stored, has
// failed permanently or has been stopped.
func (p *Publisher) try(e *entry) (retry bool) {
	data, err := p.encode(e)
	if err != nil {
		p.logger.ErrorContext(e.ctx, "status: marshal failed; report dropped", attrs(e, slog.Any("error", err))...)
		return false
	}
	ctx, cancel := context.WithTimeout(e.ctx, p.cfg.AckTimeout)
	err = p.t.PublishAcked(ctx, e.subj, data, e.msgID)
	cancel()
	switch {
	case err == nil:
		if e.failures > 0 {
			p.logger.InfoContext(e.ctx, "status: report stored after retrying", attrs(e, slog.Int("failures", e.failures))...)
		}
		return false
	case e.ctx.Err() != nil:
		return false // stopped; whoever stopped it logs
	case permanent(err):
		p.logger.ErrorContext(e.ctx, "status: report cannot be sent; dropped", attrs(e, slog.Any("error", err))...)
		return false
	}
	e.failures++
	if now := time.Now(); e.failures == 1 || now.Sub(e.lastWarn) >= warnEvery {
		e.lastWarn = now
		p.logger.WarnContext(e.ctx, "status: report not acknowledged; retrying",
			attrs(e, slog.Int("failures", e.failures), slog.Any("error", err))...)
	}
	return true
}

// encode marshals e's message, measuring a terminal report's age afresh.
func (p *Publisher) encode(e *entry) ([]byte, error) {
	msg := e.msg
	if e.terminal() {
		msg.EndedAgoMs = max(0, p.since(e.endedAt).Milliseconds())
	}
	return json.Marshal(msg)
}

// permanent reports whether err cannot be cured by sending the same report
// again. A closed connection never reopens, so retrying on it would only keep
// the report pending until Flush gave up.
func permanent(err error) bool {
	return errors.Is(err, nats.ErrMaxPayload) || errors.Is(err, nats.ErrConnectionClosed)
}

// attrs returns the attributes naming e, followed by extra.
func attrs(e *entry, extra ...any) []any {
	return append([]any{
		slog.String("task_id", e.msg.TaskID),
		slog.String("attempt_id", e.key.attemptID),
		slog.String("kind", e.key.kind),
		slog.String("subject", e.subj),
	}, extra...)
}

// dropAll stops and drops every pending entry, logging one Warn for each, and
// returns how many there were. Every entry is canceled before any is waited
// for, so their retry goroutines wind down together rather than one by one.
func (p *Publisher) dropAll(ctx context.Context) int {
	p.mu.Lock()
	all := make([]*entry, 0, len(p.pending))
	for k := range p.pending {
		all = append(all, p.takeLocked(k))
	}
	p.mu.Unlock()
	slices.SortFunc(all, func(a, b *entry) int {
		return cmp.Or(cmp.Compare(a.key.attemptID, b.key.attemptID), cmp.Compare(a.key.kind, b.key.kind))
	})
	for _, e := range all {
		e.cancel()
	}
	for _, e := range all {
		e.stop()
		p.logger.WarnContext(ctx, msgDropped, attrs(e)...)
	}
	return len(all)
}
