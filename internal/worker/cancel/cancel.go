// SPDX-License-Identifier: AGPL-3.0-or-later

// Package cancel implements per-task cancel-signal subscriptions for sqi-worker.
//
// When a job is canceled on sqi-server the scheduler publishes a
// task.cancel.<taskID> NATS message for each task that was actively executing.
// [Handler] subscribes to those subjects and routes the signal to the executor
// via [TaskCanceler.Cancel].
//
// # Subject scheme
//
// The server publishes to task.cancel.<taskID>.  A per-task NATS core
// subscription is created in [Handler.Register] when the executor begins
// processing each task and torn down in [Handler.Deregister] when the task
// completes.  Using task-specific subscriptions avoids the routing ambiguity
// that would arise if multiple workers shared a single consumer on the
// SQI_CANCEL JetStream stream with WorkQueuePolicy.
//
// # Durability
//
// Core subscriptions miss messages published before [Handler.Register] or
// while the worker is disconnected, and nothing on the worker reads the
// SQI_CANCEL stream's copy. The server covers the first case once the
// worker's "running" report arrives: the executor registers before it
// publishes "running", and the server answers a "running" report for an
// attempt it has already closed with a cancel naming that attempt
// ([protocol.TaskCancelMsg.AttemptID]), which this subscription then receives.
// A run that misses that cancel too (its "running" dropped, or the worker
// disconnected) runs to the end.
//
// # Usage
//
//	h := cancel.New(nc, exec, logger)
//	exec.SetCancelRegistrar(h)
package cancel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	nats "github.com/nats-io/nats.go"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// ── Interfaces ────────────────────────────────────────────────────────────────

// TaskCanceler is implemented by the executor to cancel a specific in-progress
// task.  [*executor.Executor] implements this interface.
type TaskCanceler interface {
	// Cancel requests cancellation of the task identified by taskID. A
	// non-empty attemptID limits it to that attempt: a task running another
	// attempt is left alone. Returns true if a matching run was found and
	// cancellation was initiated, false otherwise (already finished, never
	// dispatched, or another attempt).
	Cancel(taskID, attemptID string) bool
}

// natsSubscriber is the minimal subset of [*nats.Conn] used for subscribing.
// Defined as an interface so unit tests can inject a stub without a live NATS
// server.
type natsSubscriber interface {
	Subscribe(subj string, cb nats.MsgHandler) (*nats.Subscription, error)
}

// ── Handler ───────────────────────────────────────────────────────────────────

// Handler manages per-task NATS cancel subscriptions.
//
// Create one with [New] and pass it to [executor.Executor.SetCancelRegistrar]
// before starting the pull loop.  The executor calls [Register] before each
// task process starts and [Deregister] when the task goroutine exits.
//
// Handler is safe for concurrent use from multiple goroutines.
type Handler struct {
	nc       natsSubscriber
	canceler TaskCanceler
	logger   *slog.Logger

	mu   sync.Mutex
	subs map[string]*nats.Subscription
}

// New creates a ready-to-use Handler.
//
// nc is the worker's NATS connection (implements [natsSubscriber]).
// canceler is typically the worker's Executor.
func New(nc natsSubscriber, canceler TaskCanceler, logger *slog.Logger) *Handler {
	return &Handler{
		nc:       nc,
		canceler: canceler,
		logger:   logger,
		subs:     make(map[string]*nats.Subscription),
	}
}

// Register subscribes to task.cancel.<taskID> so that cancel signals published
// by the server are delivered to [TaskCanceler.Cancel].
//
// Calling Register for the same taskID more than once replaces the existing
// subscription.  Errors are returned so the executor can log them; they do not
// abort task execution.
func (h *Handler) Register(taskID string) error {
	subj := bus.TaskCancelSubject(taskID)
	sub, err := h.nc.Subscribe(subj, func(m *nats.Msg) {
		h.handleCancel(m, taskID)
	})
	if err != nil {
		return fmt.Errorf("cancel: subscribe %q: %w", subj, err)
	}

	h.mu.Lock()
	old := h.subs[taskID]
	h.subs[taskID] = sub
	h.mu.Unlock()

	// Drain any previous subscription for this task ID (idempotent guard).
	if old != nil {
		if err := old.Unsubscribe(); err != nil && !errors.Is(err, nats.ErrConnectionClosed) {
			h.logger.WarnContext(
				context.Background(),
				"cancel: could not unsubscribe replaced subscription",
				slog.String("task_id", taskID),
				slog.Any("error", err),
			)
		}
	}
	return nil
}

// Deregister unsubscribes from task.cancel.<taskID>.
//
// Safe to call multiple times; a missing subscription is a no-op.
func (h *Handler) Deregister(taskID string) {
	h.mu.Lock()
	sub, ok := h.subs[taskID]
	if ok {
		delete(h.subs, taskID)
	}
	h.mu.Unlock()

	if sub == nil {
		return
	}
	if err := sub.Unsubscribe(); err != nil && !errors.Is(err, nats.ErrConnectionClosed) {
		h.logger.WarnContext(
			context.Background(),
			"cancel: unsubscribe failed",
			slog.String("task_id", taskID),
			slog.Any("error", err),
		)
	}
}

// ── Internal ──────────────────────────────────────────────────────────────────

// handleCancel is the NATS message callback for task.cancel.<taskID>.
//
// It unmarshals the cancel payload, calls the registered TaskCanceler, and
// logs the outcome.  Messages that cannot be parsed are logged and ignored;
// unknown tasks (already finished) are acked silently.
func (h *Handler) handleCancel(m *nats.Msg, taskID string) {
	bg := context.Background()
	var msg protocol.TaskCancelMsg
	if err := json.Unmarshal(m.Data, &msg); err != nil {
		h.logger.WarnContext(
			bg,
			"cancel: malformed cancel message — ignoring",
			slog.String("task_id", taskID),
			slog.String("subject", m.Subject),
			slog.Any("error", err),
		)
		return
	}

	found := h.canceler.Cancel(taskID, msg.AttemptID)
	if found {
		h.logger.InfoContext(
			bg,
			"cancel: task cancellation initiated",
			slog.String("task_id", taskID),
			slog.String("attempt_id", msg.AttemptID),
			slog.String("job_id", msg.JobID),
		)
	} else {
		// Task already finished, never assigned to this worker, or running
		// an attempt other than the one named.
		h.logger.DebugContext(
			bg,
			"cancel: no matching run (finished, or another attempt) — ignoring cancel signal",
			slog.String("task_id", taskID),
			slog.String("attempt_id", msg.AttemptID),
			slog.String("job_id", msg.JobID),
		)
	}
}
