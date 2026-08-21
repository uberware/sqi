// SPDX-License-Identifier: AGPL-3.0-or-later

// Package status implements the typed task-status publisher for sqi-worker.
//
// A [Publisher] publishes task state-transition messages to the
// task.status.<workerID>.<job> NATS subject.  It covers the four transitions the worker
// is responsible for: "running", "succeeded", "failed", and "canceled".
//
// # Fields
//
// Every message includes worker_id (injected from [Config.WorkerID]) and
// last_progress (the most-recently seen openjd_progress value, passed
// explicitly by the caller so the publisher remains stateless).
//
// # Retry logic
//
// [Publisher.publishWithRetry] retries transient NATS publish failures up to
// [Config.MaxRetries] times with exponential backoff starting at
// [Config.RetryDelay].  If the caller's context is canceled during a retry
// wait the retries are aborted immediately.  After all retries are exhausted
// the failure is logged at warning level and the function returns — status
// loss is preferable to deadlocking the shutdown sequence.
//
// # Shutdown flush
//
// [Publisher.ShutdownFailed] publishes a "failed" status with message
// "worker_shutdown" for every task in a provided list.  It must be called
// before draining the NATS connection so that the server learns about the
// task failures immediately rather than waiting for the heartbeat-timeout
// sweep.
package status

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// ── Defaults ──────────────────────────────────────────────────────────────────

const (
	defaultMaxRetries = 3
	defaultRetryDelay = 100 * time.Millisecond
)

// ── natsPublisher ─────────────────────────────────────────────────────────────

// natsPublisher is the minimal subset of [*nats.Conn] used for publishing.
// Defined as an interface so unit tests can inject a stub without a live NATS
// server.
type natsPublisher interface {
	Publish(subj string, data []byte) error
}

// ── Config ────────────────────────────────────────────────────────────────────

// Config holds tunable parameters for a [Publisher].
// Zero-value fields are replaced with package defaults when [New] is called.
type Config struct {
	// WorkerID is embedded in every outgoing status message.
	WorkerID string

	// MaxRetries is the number of retry attempts after an initial publish
	// failure. Default: 3.
	MaxRetries int

	// RetryDelay is the base backoff duration before the first retry.
	// Each subsequent retry doubles the delay (exponential backoff).
	// Default: 100 ms.
	RetryDelay time.Duration
}

// ── Publisher ─────────────────────────────────────────────────────────────────

// Publisher publishes typed task-status messages to task.status.<workerID>.<job>.
//
// It injects worker_id and last_progress into every message and
// retries transient NATS publish failures with exponential backoff.
//
// Publisher is safe for concurrent use from multiple goroutines.
type Publisher struct {
	nc     natsPublisher
	cfg    Config
	logger *slog.Logger
}

// New returns a ready-to-use Publisher.
// Any zero or negative fields in cfg are replaced with defaults before use.
func New(nc natsPublisher, cfg Config, logger *slog.Logger) *Publisher {
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = defaultMaxRetries
	}
	if cfg.RetryDelay <= 0 {
		cfg.RetryDelay = defaultRetryDelay
	}
	return &Publisher{nc: nc, cfg: cfg, logger: logger}
}

// ── Public methods ────────────────────────────────────────────────────────────

// Running publishes a "running" status message immediately after the task
// process is successfully launched.
//
// lastProgress is the most-recently seen openjd_progress value for this
// attempt, or nil if none has been emitted yet.  For the initial "running"
// publish (before any process output is received) this is always nil.
func (p *Publisher) Running(
	ctx context.Context,
	msg *protocol.AssignMsg,
	sessionID string,
	lastProgress *int,
	at time.Time,
) {
	sm := protocol.TaskStatusMsg{
		Version:      protocol.ProtocolVersion,
		Type:         protocol.TypeTaskStatus,
		TaskID:       msg.TaskID,
		AttemptID:    msg.AttemptID,
		JobID:        msg.JobID,
		Status:       "running",
		SessionID:    sessionID,
		WorkerID:     p.cfg.WorkerID,
		LastProgress: lastProgress,
		At:           at,
	}
	p.publishWithRetry(ctx, sm)
}

// Terminal publishes a terminal status message: "succeeded", "failed", or
// "canceled".
//
// exitCode must be non-nil for "succeeded" and "failed" states; it should be
// nil for "canceled" where a meaningful exit code is unavailable (the process
// was killed by a signal, leaving no application exit code).
// failureReason is attached as the Message field for "failed" messages; it is
// ignored for other states.
// lastProgress is the most-recently seen openjd_progress value, or nil.
func (p *Publisher) Terminal(
	ctx context.Context,
	msg *protocol.AssignMsg,
	sessionID, status string,
	exitCode *int,
	failureReason string,
	lastProgress *int,
	at time.Time,
) {
	sm := protocol.TaskStatusMsg{
		Version:      protocol.ProtocolVersion,
		Type:         protocol.TypeTaskStatus,
		TaskID:       msg.TaskID,
		AttemptID:    msg.AttemptID,
		JobID:        msg.JobID,
		Status:       status,
		SessionID:    sessionID,
		WorkerID:     p.cfg.WorkerID,
		LastProgress: lastProgress,
		At:           at,
		Message:      failureReason,
		ExitCode:     exitCode,
	}
	p.publishWithRetry(ctx, sm)
}

// ── ShutdownTask ──────────────────────────────────────────────────────────────

// ShutdownTask carries the minimum information needed to publish a
// "worker_shutdown" failed status for an in-flight task.
type ShutdownTask struct {
	TaskID    string
	AttemptID string
	JobID     string
	SessionID string
}

// ShutdownFailed publishes a "failed" status with message "worker_shutdown"
// for every task in tasks.
//
// This MUST be called before draining the NATS connection during worker
// shutdown so the server learns about task failures immediately rather than
// waiting for the heartbeat-timeout sweep to fire.
//
// ctx should carry a deadline so the total flush time is bounded even if all
// retries are exhausted.  Each publish uses publishWithRetry; if the context
// deadline is reached or all retries are exhausted the error is logged and the
// function proceeds to the next task — all tasks are attempted regardless of
// individual publish failures.
func (p *Publisher) ShutdownFailed(ctx context.Context, tasks []ShutdownTask) {
	for _, t := range tasks {
		sm := protocol.TaskStatusMsg{
			Version:   protocol.ProtocolVersion,
			Type:      protocol.TypeTaskStatus,
			TaskID:    t.TaskID,
			AttemptID: t.AttemptID,
			JobID:     t.JobID,
			Status:    "failed",
			SessionID: t.SessionID,
			WorkerID:  p.cfg.WorkerID,
			At:        time.Now(),
			Message:   "worker_shutdown",
		}
		p.publishWithRetry(ctx, sm)
	}
}

// ── Internal helpers ──────────────────────────────────────────────────────────

// publishWithRetry marshals msg and publishes it to task.status.<workerID>.<job> with up
// to cfg.MaxRetries retries on failure, using exponential backoff.
//
// On a transient NATS failure the method waits cfg.RetryDelay (doubling each
// attempt) before retrying.  If ctx is canceled during a retry wait the
// backoff is interrupted and the failure is logged immediately.  After all
// retries are exhausted the error is logged at warning level and the function
// returns — status loss is preferable to deadlocking the shutdown sequence.
func (p *Publisher) publishWithRetry(ctx context.Context, msg protocol.TaskStatusMsg) {
	data, err := json.Marshal(msg)
	if err != nil {
		p.logger.WarnContext(
			ctx, "status: marshal failed",
			slog.String("task_id", msg.TaskID),
			slog.String("attempt_id", msg.AttemptID),
			slog.String("status", msg.Status),
			slog.Any("error", err),
		)
		return
	}

	subj := bus.TaskStatusSubject(p.cfg.WorkerID, msg.JobID)
	delay := p.cfg.RetryDelay

	for attempt := 0; attempt <= p.cfg.MaxRetries; attempt++ {
		if pubErr := p.nc.Publish(subj, data); pubErr == nil {
			return
		} else if attempt == p.cfg.MaxRetries {
			p.logger.WarnContext(
				ctx, "status: publish failed after retries",
				slog.String("task_id", msg.TaskID),
				slog.String("attempt_id", msg.AttemptID),
				slog.String("status", msg.Status),
				slog.String("subject", subj),
				slog.Int("max_retries", p.cfg.MaxRetries),
				slog.Any("error", pubErr),
			)
			return
		} else {
			p.logger.DebugContext(
				ctx, "status: publish failed, retrying",
				slog.String("task_id", msg.TaskID),
				slog.String("attempt_id", msg.AttemptID),
				slog.String("status", msg.Status),
				slog.Int("attempt", attempt+1),
				slog.Duration("delay", delay),
				slog.Any("error", pubErr),
			)
		}

		// Wait before retrying, but abort early if ctx is canceled: status loss
		// is better than deadlocking the shutdown sequence.
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			p.logger.WarnContext(
				ctx, "status: publish aborted (context canceled)",
				slog.String("task_id", msg.TaskID),
				slog.String("attempt_id", msg.AttemptID),
				slog.String("status", msg.Status),
				slog.Int("attempt", attempt+1),
			)
			return
		case <-timer.C:
			delay *= 2
		}
	}
}
