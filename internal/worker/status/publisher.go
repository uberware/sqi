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
// connection, which never reopens. A publish the broker denies is not among
// them: the broker reports the violation asynchronously, so through the
// JetStream transport it surfaces as an ack timeout and is retried like any
// other. A terminal report drops a still-pending "running" for the same
// attempt, since a terminal status is legal on an assigned task and a later
// "running" would be refused as stale.
//
// [Publisher.PendingAttemptIDs] names the attempts with a report still
// pending, so the worker keeps reporting them as held, and [Publisher.Flush]
// waits for the pending reports at shutdown.
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

const (
	defaultAckTimeout = 2 * time.Second
	defaultRetryBase  = 250 * time.Millisecond
	defaultRetryMax   = 5 * time.Second

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
	key      entryKey
	subj     string
	msg      protocol.TaskStatusMsg
	terminal bool
	endedAt  time.Time // terminal only, with its monotonic reading

	// ctx ends when the entry is acked, dropped or stopped, which aborts a
	// try in flight.
	ctx    context.Context
	cancel context.CancelFunc

	// done is closed when the entry's retry goroutine exits; nil until one
	// starts. Guarded by Publisher.mu.
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
		cfg.AckTimeout = defaultAckTimeout
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
	sm := p.base(msg, sessionID, kindRunning)
	sm.At = at.Add(p.cfg.WireOffset)
	p.send(&entry{
		key:  entryKey{attemptID: msg.AttemptID, kind: kindRunning},
		subj: bus.TaskStatusSubject(p.cfg.WorkerID, msg.JobID),
		msg:  sm,
	})
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
	sm := p.base(msg, sessionID, status)
	sm.At = endedAt.Add(p.cfg.WireOffset)
	sm.Message = failureReason
	sm.ExitCode = exitCode
	sm.LastProgress = lastProgress
	p.send(&entry{
		key:      entryKey{attemptID: msg.AttemptID, kind: kind},
		subj:     bus.TaskStatusSubject(p.cfg.WorkerID, msg.JobID),
		msg:      sm,
		terminal: true,
		endedAt:  endedAt,
	})
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

// ── Internal helpers ──────────────────────────────────────────────────────────

// tryResult is how one try ended.
type tryResult int

const (
	tryAcked   tryResult = iota // the stream stored the report
	tryRetry                    // a retryable failure
	tryDropped                  // a permanent failure, or the entry was stopped
)

// base builds the fields every report carries.
func (p *Publisher) base(msg *protocol.AssignMsg, sessionID, status string) protocol.TaskStatusMsg {
	return protocol.TaskStatusMsg{
		Version:   protocol.ProtocolVersion,
		Type:      protocol.TypeTaskStatus,
		TaskID:    msg.TaskID,
		AttemptID: msg.AttemptID,
		JobID:     msg.JobID,
		Status:    status,
		SessionID: sessionID,
		WorkerID:  p.cfg.WorkerID,
	}
}

// send makes e pending, stopping the entries it replaces, then tries it once
// inline and hands it to a retry goroutine if that try fails.
func (p *Publisher) send(e *entry) {
	e.ctx, e.cancel = context.WithCancel(context.Background())

	p.mu.Lock()
	replaced := p.takeLocked(e.key)
	if e.terminal {
		replaced = append(replaced, p.takeLocked(entryKey{attemptID: e.key.attemptID, kind: kindRunning})...)
	}
	p.pending[e.key] = e
	p.mu.Unlock()
	for _, r := range replaced {
		r.stop()
	}

	switch p.try(e) {
	case tryAcked, tryDropped:
		p.finish(e)
	case tryRetry:
		p.startRetry(e)
	}
}

// takeLocked removes the entry under k from pending and returns it, or
// nothing when there is none. p.mu must be held.
func (p *Publisher) takeLocked(k entryKey) []*stopping {
	e, ok := p.pending[k]
	if !ok {
		return nil
	}
	delete(p.pending, k)
	p.notifyLocked()
	return []*stopping{{e: e, done: e.done}}
}

// stopping is an entry taken out of pending, with its retry goroutine's done
// channel as it was when it was taken.
type stopping struct {
	e    *entry
	done chan struct{}
}

// stop aborts the entry's try in flight and waits for its retry goroutine, if
// it has one, to exit. A goroutine cannot start after the entry has left
// pending (see startRetry), so a nil done means there is none to wait for.
func (s *stopping) stop() {
	s.e.cancel()
	if s.done != nil {
		<-s.done
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
		if p.try(e) != tryRetry {
			p.finish(e)
			return
		}
		delay = min(delay*2, p.cfg.RetryMax)
	}
}

// try sends e once and classifies the outcome, logging it as the rate limit
// allows.
func (p *Publisher) try(e *entry) tryResult {
	data, err := p.encode(e)
	if err != nil {
		p.logger.ErrorContext(e.ctx, "status: marshal failed; report dropped", attrs(e, slog.Any("error", err))...)
		return tryDropped
	}
	ctx, cancel := context.WithTimeout(e.ctx, p.cfg.AckTimeout)
	err = p.t.PublishAcked(ctx, e.subj, data, e.key.attemptID+":"+e.key.kind)
	cancel()
	switch {
	case err == nil:
		if e.failures > 0 {
			p.logger.InfoContext(e.ctx, "status: report stored after retrying", attrs(e, slog.Int("failures", e.failures))...)
		}
		return tryAcked
	case e.ctx.Err() != nil:
		return tryDropped // stopped; whoever stopped it logs
	case permanent(err):
		p.logger.ErrorContext(e.ctx, "status: report cannot be sent; dropped", attrs(e, slog.Any("error", err))...)
		return tryDropped
	}
	e.failures++
	if now := time.Now(); e.failures == 1 || now.Sub(e.lastWarn) >= warnEvery {
		e.lastWarn = now
		p.logger.WarnContext(e.ctx, "status: report not acknowledged; retrying",
			attrs(e, slog.Int("failures", e.failures), slog.Any("error", err))...)
	}
	return tryRetry
}

// encode marshals e's message, measuring a terminal report's age afresh.
func (p *Publisher) encode(e *entry) ([]byte, error) {
	msg := e.msg
	if e.terminal {
		msg.EndedAgoMs = max(0, p.since(e.endedAt).Milliseconds())
	}
	return json.Marshal(msg)
}

// permanent reports whether err cannot be cured by sending the same report
// again. A closed connection never reopens, so retrying on it would only keep
// the report pending until Flush gave up. A permissions violation counts only
// when a transport returns it directly; the JetStream transport never does
// (see the package doc).
func permanent(err error) bool {
	return errors.Is(err, nats.ErrPermissionViolation) ||
		errors.Is(err, nats.ErrMaxPayload) ||
		errors.Is(err, nats.ErrConnectionClosed)
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
// returns how many there were.
func (p *Publisher) dropAll(ctx context.Context) int {
	p.mu.Lock()
	all := make([]*stopping, 0, len(p.pending))
	for k := range p.pending {
		all = append(all, p.takeLocked(k)...)
	}
	p.mu.Unlock()
	slices.SortFunc(all, func(a, b *stopping) int {
		return cmp.Or(cmp.Compare(a.e.key.attemptID, b.e.key.attemptID), cmp.Compare(a.e.key.kind, b.e.key.kind))
	})
	for _, s := range all {
		s.stop()
		p.logger.WarnContext(
			ctx, msgDropped,
			slog.String("task_id", s.e.msg.TaskID),
			slog.String("attempt_id", s.e.key.attemptID),
			slog.String("kind", s.e.key.kind),
		)
	}
	return len(all)
}
