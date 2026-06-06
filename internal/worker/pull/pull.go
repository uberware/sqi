// SPDX-License-Identifier: AGPL-3.0-only

// Package pull implements the JetStream pull loop that fetches task-assignment
// messages from the SQI_WORK stream and dispatches them for execution.
//
// # Consumer strategy
//
// The worker creates (or ensures) durable pull consumers on the SQI_WORK
// JetStream stream. When specific queue IDs are configured, one consumer is
// created per queue using the same durable name as [bus.EnsureWorkConsumer]
// (sqi-work-<queue>), so all workers serving the same queue compete for the
// same message set — whichever worker calls Fetch first gets the assignment.
//
// When no queue IDs are configured (queue_ids is empty), a single wildcard
// consumer covering work.assign.> is created under the name "sqi-work-all".
// This is the default for homogeneous farms where every worker handles every
// queue. Note: wildcard and per-queue consumers must not coexist on the same
// SQI_WORK stream, as they would race on overlapping messages.
//
// # Pull loop (tasks 38–39)
//
// Each consumer runs its own pull goroutine. On each iteration the loop
// computes available concurrency slots (MaxConcurrentTasks − activeCount) and
// fetches up to that many messages at once. If at capacity, the loop polls
// every 200 ms until a slot opens.
//
// # Ack / nack semantics (tasks 40–41)
//
// After validating the [protocol.AssignMsg] payload, the loop calls
// [TaskDispatcher.Dispatch]. On success the message is acked immediately so
// JetStream removes it from the stream and does not redeliver it. On
// validation failure (bad JSON, protocol version mismatch, compute-location
// mismatch, or dispatcher rejection), the message is nacked with a configurable
// delay so another worker can attempt it.
//
// # Idle backoff (task 42)
//
// When a fetch returns no messages, the loop waits [Config.IdleBackoff]
// (default 2 s) before trying again, avoiding a tight polling loop on empty
// queues. The backoff resets to zero as soon as a message is received.
package pull

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// ── Constants ─────────────────────────────────────────────────────────────────

// consumerNameAll is the durable pull-consumer name used when no queue IDs are
// configured. It binds to work.assign.> and covers every queue.
//
// Workers using this consumer must not coexist with per-queue consumers on the
// same SQI_WORK stream; the two filter types would race on overlapping messages.
const consumerNameAll = "sqi-work-all"

// ackWait is how long NATS waits for a worker to acknowledge a fetched message
// before considering it unacknowledged and making it eligible for redelivery.
// Must match [bus.EnsureWorkConsumer]'s AckWait so that the server and worker
// agree on the same redelivery window.
const ackWait = 30 * time.Second

// maxDeliver is the maximum number of delivery attempts for a single message
// before NATS stops retrying. After exhaustion the message is discarded;
// the scheduler's heartbeat sweep reclaims the task and re-publishes a fresh
// assignment. Must match [bus.EnsureWorkConsumer]'s MaxDeliver.
const maxDeliver = 5

// fetchWait is the maximum time [jetstream.Consumer.Fetch] blocks waiting for
// messages before returning an empty batch. A short value keeps the pull loop
// responsive to context cancellation without spinning.
const fetchWait = 1 * time.Second

// atCapacityPollInterval is how long the pull loop sleeps when active task
// count equals MaxConcurrentTasks before checking again. Short enough to react
// quickly when a task finishes, long enough to avoid busy-waiting.
const atCapacityPollInterval = 200 * time.Millisecond

// ── Interfaces ────────────────────────────────────────────────────────────────

// TaskDispatcher is implemented by the executor component (tasks 49+). The
// Puller calls Dispatch after successfully validating an [protocol.AssignMsg].
//
// A nil return means the task was accepted and the NATS message is acked. A
// non-nil error means the task was rejected (e.g., pre-execution validation
// failed) and the message is nacked with [Config.NackDelay] so another worker
// can retry it.
type TaskDispatcher interface {
	Dispatch(ctx context.Context, msg *protocol.AssignMsg) error
}

// NoopDispatcher is a [TaskDispatcher] that accepts every assignment without
// executing it. Use it until the real executor is wired in (tasks 49+).
type NoopDispatcher struct{}

// Dispatch always returns nil, causing the Puller to ack every assignment.
func (NoopDispatcher) Dispatch(_ context.Context, _ *protocol.AssignMsg) error { return nil }

// StateSource provides the Puller with the current number of active tasks so
// the pull loop never fetches more assignments than there are concurrency slots.
// The executor (tasks 49+) implements this interface; use [NoopStateSource]
// until then.
type StateSource interface {
	// ActiveTaskCount returns the number of task processes currently executing.
	ActiveTaskCount() int
}

// NoopStateSource is a [StateSource] that always reports zero active tasks.
// Use before the executor is available.
type NoopStateSource struct{}

// ActiveTaskCount always returns 0.
func (NoopStateSource) ActiveTaskCount() int { return 0 }

// ── Config ────────────────────────────────────────────────────────────────────

// Config parameterises the pull loop. Obtain values from [workerconfig.WorkerSettings].
type Config struct {
	// QueueIDs lists the queues this worker serves. An empty slice means the
	// worker covers all queues via a wildcard consumer (task 38).
	QueueIDs []string

	// MaxConcurrentTasks is the concurrency ceiling. The loop fetches at most
	// MaxConcurrentTasks − activeCount messages at a time (task 39).
	MaxConcurrentTasks int

	// ComputeLocation is compared against the ComputeLocation field in each
	// incoming [protocol.AssignMsg]. A mismatch causes an immediate nack (task 41).
	// An empty worker or assignment location disables the check.
	ComputeLocation string

	// IdleBackoff is the wait between pull attempts when the queue is empty
	// (task 42). Must be > 0; defaults to 2 s when [New] is called.
	IdleBackoff time.Duration

	// NackDelay is the redelivery delay applied when an assignment fails
	// pre-execution validation (task 40). Gives other workers a window to
	// pick up the assignment. Must be > 0; defaults to 5 s when [New] is called.
	NackDelay time.Duration
}

// ── Puller ────────────────────────────────────────────────────────────────────

// Puller fetches task-assignment messages from the SQI_WORK JetStream stream
// and dispatches them via a [TaskDispatcher]. Obtain one with [New] and call
// [Puller.Run] in a goroutine; it blocks until ctx is canceled.
type Puller struct {
	js         jetstream.JetStream
	cfg        Config
	state      StateSource
	dispatcher TaskDispatcher
	logger     *slog.Logger
}

// New creates a Puller. js must be a JetStream context obtained from the
// worker's *nats.Conn (via jetstream.New). state and dispatcher must be
// non-nil; use [NoopStateSource] and [NoopDispatcher] before the executor
// package is available.
//
// Zero or negative IdleBackoff and NackDelay are replaced with safe defaults
// (2 s and 5 s respectively).
func New(
	js jetstream.JetStream,
	cfg Config,
	state StateSource,
	dispatcher TaskDispatcher,
	logger *slog.Logger,
) *Puller {
	if cfg.IdleBackoff <= 0 {
		cfg.IdleBackoff = 2 * time.Second
	}
	if cfg.NackDelay <= 0 {
		cfg.NackDelay = 5 * time.Second
	}
	if cfg.MaxConcurrentTasks < 1 {
		cfg.MaxConcurrentTasks = 1
	}
	return &Puller{
		js:         js,
		cfg:        cfg,
		state:      state,
		dispatcher: dispatcher,
		logger:     logger,
	}
}

// Run starts the pull loop and blocks until ctx is canceled.
// Callers must invoke Run in a goroutine.
//
// On startup, Run ensures the required JetStream consumers exist (creating
// them on the remote broker if absent). If consumer setup fails, Run logs the
// error and returns immediately without entering the pull loop.
func (p *Puller) Run(ctx context.Context) {
	p.logger.InfoContext(
		ctx, "pull: starting",
		slog.Any("queue_ids", p.cfg.QueueIDs),
		slog.Int("max_concurrent_tasks", p.cfg.MaxConcurrentTasks),
		slog.String("compute_location", p.cfg.ComputeLocation),
		slog.Duration("idle_backoff", p.cfg.IdleBackoff),
		slog.Duration("nack_delay", p.cfg.NackDelay),
	)

	consumers, err := p.ensureConsumers(ctx)
	if err != nil {
		p.logger.ErrorContext(ctx, "pull: failed to ensure consumers — pull loop not started",
			slog.Any("error", err))
		return
	}

	// Run one pull goroutine per consumer and wait for all to exit.
	var wg sync.WaitGroup
	for queueLabel, consumer := range consumers {
		wg.Add(1)
		go func(label string, c jetstream.Consumer) {
			defer wg.Done()
			p.runConsumerLoop(ctx, c, label)
		}(queueLabel, consumer)
	}
	wg.Wait()

	p.logger.InfoContext(context.Background(), "pull: stopped")
}

// ── Consumer setup ────────────────────────────────────────────────────────────

// ensureConsumers creates or updates the durable pull consumers required for
// this worker's configured queues. Returns a map keyed by queue label (queue
// ID, or "*" for the all-queues wildcard consumer) to the ready Consumer.
func (p *Puller) ensureConsumers(ctx context.Context) (map[string]jetstream.Consumer, error) {
	if len(p.cfg.QueueIDs) == 0 {
		consumer, err := p.ensureWildcardConsumer(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]jetstream.Consumer{"*": consumer}, nil
	}

	consumers := make(map[string]jetstream.Consumer, len(p.cfg.QueueIDs))
	for _, queueID := range p.cfg.QueueIDs {
		consumer, err := p.ensureQueueConsumer(ctx, queueID)
		if err != nil {
			return nil, fmt.Errorf("queue %q: %w", queueID, err)
		}
		consumers[queueID] = consumer
	}
	return consumers, nil
}

// ensureQueueConsumer creates or updates the durable pull consumer for a
// specific queue, using the same consumer name format as [bus.EnsureWorkConsumer]
// so that all workers serving the same queue share a single durable consumer
// and therefore compete for the same message set.
func (p *Puller) ensureQueueConsumer(ctx context.Context, queueID string) (jetstream.Consumer, error) {
	name := "sqi-work-" + sanitizeConsumerToken(queueID)
	cfg := jetstream.ConsumerConfig{
		Durable:       name,
		FilterSubject: bus.WorkAssignSubject(queueID),
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       ackWait,
		MaxDeliver:    maxDeliver,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		Description:   "Work-assignment pull consumer for queue " + queueID,
	}
	consumer, err := p.js.CreateOrUpdateConsumer(ctx, bus.StreamWork, cfg)
	if err != nil {
		return nil, fmt.Errorf("pull: ensure consumer for queue %q: %w", queueID, err)
	}
	p.logger.InfoContext(
		ctx, "pull: consumer ready",
		slog.String("consumer", name),
		slog.String("queue_id", queueID),
		slog.String("filter_subject", cfg.FilterSubject),
	)
	return consumer, nil
}

// ensureWildcardConsumer creates or updates a single durable pull consumer
// covering the work.assign.> wildcard, used when no queue IDs are configured.
func (p *Puller) ensureWildcardConsumer(ctx context.Context) (jetstream.Consumer, error) {
	filterSubject := bus.SubjectWorkAssignPrefix + ".>"
	cfg := jetstream.ConsumerConfig{
		Durable:       consumerNameAll,
		FilterSubject: filterSubject,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       ackWait,
		MaxDeliver:    maxDeliver,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		Description:   "Work-assignment pull consumer covering all queues (wildcard).",
	}
	consumer, err := p.js.CreateOrUpdateConsumer(ctx, bus.StreamWork, cfg)
	if err != nil {
		return nil, fmt.Errorf("pull: ensure wildcard consumer: %w", err)
	}
	p.logger.InfoContext(
		ctx, "pull: wildcard consumer ready",
		slog.String("consumer", consumerNameAll),
		slog.String("filter_subject", filterSubject),
	)
	return consumer, nil
}

// ── ackableMsg ────────────────────────────────────────────────────────────────

// ackableMsg is the minimal subset of [jetstream.Msg] used by [handleMessage].
// Defining it locally avoids requiring tests to implement the full
// [jetstream.Msg] interface. Values of this type are obtained via [wrapMsg],
// which adapts a [jetstream.Msg] into simple no-option method signatures —
// avoiding any reference to AckOpt, which is not exported by the jetstream
// package.
type ackableMsg interface {
	// Data returns the raw message payload.
	Data() []byte
	// Ack acknowledges the message so JetStream removes it from the stream.
	Ack() error
	// NakWithDelay negatively acknowledges the message, requesting redelivery
	// after the specified delay so another worker can attempt it.
	NakWithDelay(time.Duration) error
}

// wrappedMsg adapts a [jetstream.Msg] to [ackableMsg]. Calling Ack() and
// NakWithDelay(d) with no option arguments is valid for any variadic signature,
// regardless of whether the option type is exported by the jetstream package.
type wrappedMsg struct{ msg jetstream.Msg }

// wrapMsg wraps a raw JetStream message for use as an [ackableMsg].
func wrapMsg(m jetstream.Msg) ackableMsg { return wrappedMsg{m} }

func (w wrappedMsg) Data() []byte                       { return w.msg.Data() }
func (w wrappedMsg) Ack() error                         { return w.msg.Ack() }
func (w wrappedMsg) NakWithDelay(d time.Duration) error { return w.msg.NakWithDelay(d) }

// ── Pull loop ─────────────────────────────────────────────────────────────────

// runConsumerLoop is the per-consumer pull loop. It blocks until ctx is canceled.
//
// On each iteration:
//  1. Compute available concurrency slots.
//  2. If at capacity, wait [atCapacityPollInterval] and retry.
//  3. Fetch up to slots messages with a [fetchWait] timeout.
//  4. Handle each message (validate, dispatch, ack/nack).
//  5. If the queue was empty, apply [Config.IdleBackoff] before fetching again
//     (task 42).
func (p *Puller) runConsumerLoop(ctx context.Context, consumer jetstream.Consumer, queueLabel string) {
	idling := false // suppress repeated "queue empty" debug logs

	for {
		if ctx.Err() != nil {
			return
		}

		// Task 39: gate on concurrency slots.
		slots := p.cfg.MaxConcurrentTasks - p.state.ActiveTaskCount()
		if slots <= 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(atCapacityPollInterval):
				continue
			}
		}

		// Fetch up to `slots` messages. Blocks for up to fetchWait if the
		// queue is empty, then returns an empty batch.
		batch, err := consumer.Fetch(slots, jetstream.FetchMaxWait(fetchWait))
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			p.logger.WarnContext(
				ctx, "pull: fetch error — retrying after idle backoff",
				slog.String("queue", queueLabel),
				slog.Any("error", err),
			)
			select {
			case <-ctx.Done():
				return
			case <-time.After(p.cfg.IdleBackoff):
			}
			continue
		}

		received := p.drainBatch(ctx, batch, queueLabel)
		if received > 0 {
			idling = false // task 42: reset backoff as soon as a message is received
		}

		if received == 0 {
			// Task 42: backoff on empty queue to avoid tight polling.
			if !idling {
				p.logger.DebugContext(
					ctx, "pull: queue empty — backing off",
					slog.String("queue", queueLabel),
					slog.Duration("backoff", p.cfg.IdleBackoff),
				)
				idling = true
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(p.cfg.IdleBackoff):
			}
		}
	}
}

// drainBatch iterates the messages channel returned by [jetstream.Consumer.Fetch],
// dispatching each via [handleMessage]. If ctx is already canceled when a
// message is dequeued, the message is nacked immediately and the channel
// continues to be drained so NATS can close it cleanly.
//
// Returns the total number of messages received (regardless of whether they
// were processed or nacked due to cancellation).
func (p *Puller) drainBatch(ctx context.Context, batch jetstream.MessageBatch, queueLabel string) int {
	received := 0
	for msg := range batch.Messages() {
		received++
		if ctx.Err() != nil {
			// Shutdown mid-batch: nack immediately so messages redeliver
			// quickly to another worker rather than waiting for AckWait.
			if err := wrapMsg(msg).NakWithDelay(0); err != nil {
				p.logger.WarnContext(
					context.Background(),
					"pull: shutdown nack failed — message will redeliver after ack-wait",
					slog.String("queue", queueLabel),
					slog.Any("error", err),
				)
			}
			continue // drain remaining items so NATS can close the channel
		}
		p.handleMessage(ctx, wrapMsg(msg), queueLabel)
	}
	return received
}

// ── Message handling ──────────────────────────────────────────────────────────

// handleMessage validates a raw NATS message, dispatches it via the registered
// [TaskDispatcher], and acks or nacks the message accordingly.
//
// Ack path (task 40): validation passes and Dispatch returns nil → msg.Ack().
// Nack path (tasks 40–41): any validation failure or Dispatch error → msg.NakWithDelay.
func (p *Puller) handleMessage(ctx context.Context, msg ackableMsg, queueLabel string) {
	// ── Parse payload ──────────────────────────────────────────────────────
	var assign protocol.AssignMsg
	if err := json.Unmarshal(msg.Data(), &assign); err != nil {
		p.logger.ErrorContext(
			ctx, "pull: malformed AssignMsg payload — nacking",
			slog.String("queue", queueLabel),
			slog.Any("error", err),
		)
		p.nack(ctx, msg, queueLabel, "")
		return
	}

	// ── Protocol version check ─────────────────────────────────────────────
	if assign.Version != protocol.ProtocolVersion {
		p.logger.WarnContext(
			ctx, "pull: protocol version mismatch — nacking",
			slog.String("queue", queueLabel),
			slog.String("task_id", assign.TaskID),
			slog.String("received_version", assign.Version),
			slog.String("expected_version", protocol.ProtocolVersion),
		)
		p.nack(ctx, msg, queueLabel, assign.TaskID)
		return
	}

	// ── Compute location check (task 41) ───────────────────────────────────
	// Only validate when both the worker and the assignment specify a location.
	// An empty location on either side disables the check (e.g., no path map
	// was generated, or the worker has no location configured).
	if assign.ComputeLocation != "" && p.cfg.ComputeLocation != "" &&
		assign.ComputeLocation != p.cfg.ComputeLocation {
		p.logger.WarnContext(
			ctx, "pull: assignment targets a different compute location — nacking",
			slog.String("queue", queueLabel),
			slog.String("task_id", assign.TaskID),
			slog.String("assignment_location", assign.ComputeLocation),
			slog.String("worker_location", p.cfg.ComputeLocation),
		)
		p.nack(ctx, msg, queueLabel, assign.TaskID)
		return
	}

	p.logger.InfoContext(
		ctx, "pull: received assignment",
		slog.String("queue", queueLabel),
		slog.String("task_id", assign.TaskID),
		slog.String("job_id", assign.JobID),
		slog.String("attempt_id", assign.AttemptID),
		slog.String("task_name", assign.TaskName),
	)

	// ── Dispatch (task 40) ─────────────────────────────────────────────────
	// Dispatch starts the session. Ack immediately on success so JetStream
	// removes the message from the stream and does not redeliver it.
	if err := p.dispatcher.Dispatch(ctx, &assign); err != nil {
		p.logger.WarnContext(
			ctx, "pull: dispatcher rejected assignment — nacking",
			slog.String("queue", queueLabel),
			slog.String("task_id", assign.TaskID),
			slog.Any("error", err),
		)
		p.nack(ctx, msg, queueLabel, assign.TaskID)
		return
	}

	// Ack after the dispatcher confirms it accepted the task (task 40).
	if err := msg.Ack(); err != nil {
		// Ack failure is non-fatal: the message will be redelivered after
		// AckWait elapses. The dispatcher/executor must be idempotent with
		// respect to duplicate task IDs to handle this case gracefully.
		p.logger.WarnContext(
			ctx, "pull: ack failed — task is executing but may be redelivered",
			slog.String("queue", queueLabel),
			slog.String("task_id", assign.TaskID),
			slog.Any("error", err),
		)
	}
}

// nack sends a NakWithDelay for msg. Errors are logged as warnings since the
// AckWait timer will trigger redelivery regardless if the nack does not land.
func (p *Puller) nack(ctx context.Context, msg ackableMsg, queueLabel, taskID string) {
	if err := msg.NakWithDelay(p.cfg.NackDelay); err != nil {
		p.logger.WarnContext(
			ctx, "pull: nack failed — message will redeliver after ack-wait",
			slog.String("queue", queueLabel),
			slog.String("task_id", taskID),
			slog.Any("error", err),
		)
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// sanitizeConsumerToken replaces characters that are not allowed in JetStream
// consumer names (alphanumeric, dash, underscore, dot) with underscores.
// Mirrors the unexported bus.sanitizeConsumerToken function so the worker uses
// identical consumer names to the server's [bus.EnsureWorkConsumer].
func sanitizeConsumerToken(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, s)
}
