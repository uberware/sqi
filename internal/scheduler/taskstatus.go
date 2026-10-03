// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Server-side handlers that consume worker messages from NATS
// subjects and persist state via the Store.
//
// This file implements the task-status consumer — the counterpart of the
// worker-register and worker-heartbeat consumers already in scheduler.go.
// When a worker publishes a protocol.TaskStatusMsg to task.status.<worker>.<job>,
// handleTaskStatusMessage updates the store (closing the attempt record,
// releasing usage pool slots and moving the task in one write), and drives
// step/job completion logic.
//
// Flow for each TaskStatusMsg:
//
//  1. Decode protocol.TaskStatusMsg.
//  2. Load the task attempt from the store to verify AttemptID.
//  3. For "running": update task status and record session ID on the attempt.
//  4. For terminal states (succeeded/failed/canceled):
//     a. In one store write (CompleteTaskAttempt): close the attempt (EndedAt,
//        Status, ExitCode), release any held usage pool slots, and transition
//        the task to the matching terminal status. The close and the release
//        commit even when the task transition is refused.
//     b. Check whether the enclosing step is now complete.
//     c. If the step completed successfully, call ResolveDependencies to
//        unblock downstream steps.
//     d. Check whether the enclosing job is now complete.
//
// Step and job completion are decided inside the store (FinalizeStep and
// FinalizeJob, invariant I4), computed from the child rows in the same
// statement that writes the status, so they have no page limit and cannot be
// outrun by a concurrent retry.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/ws"
)

// startTaskStatusConsumer creates the server-side JetStream push consumer for
// the SQI_TASK stream and begins delivering messages to
// [handleTaskStatusMessage].  Called once from [Scheduler.Run].
func (s *Scheduler) startTaskStatusConsumer(ctx context.Context) error {
	_, err := s.bus.ConsumeTaskStatus(ctx, s.handleTaskStatusMessage)
	return err
}

// handleTaskStatusMessage is the JetStream message handler for
// task.status.<worker>.<job> messages published by workers.
//
// The subject's worker ID is authoritative: it is compared against the
// attempt's recorded owner in [Scheduler.processTaskStatus], and a mismatch
// discards the message rather than applying it — otherwise any worker could
// report completion or failure for a task another worker holds.
//
// NOTE ON AUTH-OFF. With broker authentication disabled — the default — the
// subject's worker ID is present but NOT enforced by NATS, so a hostile
// client can publish under any ID. These checks then catch honest bugs, not
// attackers. That is why sqi-server warns at startup when the broker is
// unauthenticated and non-loopback.
func (s *Scheduler) handleTaskStatusMessage(msg jetstream.Msg) {
	ctx := s.ctx

	var m protocol.TaskStatusMsg
	if err := json.Unmarshal(msg.Data(), &m); err != nil {
		s.logger.WarnContext(
			ctx, "scheduler: malformed task.status message",
			slog.Any("error", err),
		)
		s.ackMsg(ctx, msg) // discard; re-delivery cannot fix a bad payload
		return
	}
	// This is the costly half of the version gate and the deliberate one: a
	// discarded terminal status means the server never learns the task
	// finished, so it sits until reapStaleAssignedTasks returns it to the
	// queue and it runs again somewhere else. That is preferred to the
	// alternative, because this is the message that carries the OUTCOME --
	// exit code, session, timestamps -- and acting on a payload whose shape
	// this server cannot vouch for writes a wrong result to a task that is
	// otherwise finished and correct.
	if s.discardOnVersionMismatch(ctx, msg, slog.LevelWarn, m.Version, m.WorkerID,
		"this task's reported state is not recorded; it will be reclaimed and retried") {
		return
	}
	if m.TaskID == "" || m.AttemptID == "" {
		s.logger.WarnContext(
			ctx, "scheduler: task.status missing task_id or attempt_id — discarding",
			slog.String("task_id", m.TaskID),
			slog.String("attempt_id", m.AttemptID),
		)
		s.ackMsg(ctx, msg)
		return
	}

	// The subject is the only identity NATS itself can vouch for; a message
	// on a subject that does not carry one concrete worker ID cannot be
	// attributed to anyone and is discarded rather than acted on.
	subjectWorkerID, _, ok := bus.ParseWorkerSubject(msg.Subject())
	if !ok {
		s.discardUnexpectedSubject(ctx, msg, "task.status")
		return
	}

	if err := s.processTaskStatus(ctx, subjectWorkerID, m); err != nil {
		// An illegal transition is permanent: the task has moved on (retried,
		// canceled, already terminal) and this message describes a past state.
		// Redelivering cannot make it legal, so discard it rather than Nak into
		// an infinite loop — the same reasoning as a malformed payload above.
		if errors.Is(err, store.ErrInvalidTransition) {
			s.logger.WarnContext(
				ctx, "scheduler: task status rejected by state machine — discarding",
				slog.String("task_id", m.TaskID),
				slog.String("attempt_id", m.AttemptID),
				slog.String("status", m.Status),
				slog.Any("error", err),
			)
			s.ackMsg(ctx, msg)
			return
		}
		s.logger.WarnContext(
			ctx, "scheduler: process task status failed — will redeliver",
			slog.String("task_id", m.TaskID),
			slog.String("attempt_id", m.AttemptID),
			slog.String("status", m.Status),
			slog.Any("error", err),
		)
		s.nakMsg(ctx, msg)
		return
	}

	s.ackMsg(ctx, msg)
}

// processTaskStatus applies a single [protocol.TaskStatusMsg] to the store.
//
// subjectWorkerID is the worker the message's subject attributes it to — the
// only identity NATS itself can vouch for. It is compared against the
// attempt's recorded WorkerID below; m.WorkerID is not trusted for this
// decision, since it is asserted by whoever sent the message rather than
// enforced by the transport.
func (s *Scheduler) processTaskStatus(ctx context.Context, subjectWorkerID string, m protocol.TaskStatusMsg) error {
	// Verify the attempt still exists and is for the right task.
	attempt, err := s.store.GetTaskAttempt(ctx, m.AttemptID)
	if errors.Is(err, store.ErrNotFound) {
		// Attempt was deleted (e.g. in a test teardown) or the ID is wrong.
		// Ack to avoid infinite re-delivery of a permanently-invalid message.
		s.logger.WarnContext(
			ctx, "scheduler: task.status for unknown attempt — discarding",
			slog.String("attempt_id", m.AttemptID),
			slog.String("task_id", m.TaskID),
		)
		return nil
	}
	if err != nil {
		return err
	}
	if attempt.TaskID != m.TaskID {
		s.logger.WarnContext(
			ctx, "scheduler: attempt task_id mismatch — discarding",
			slog.String("attempt_id", m.AttemptID),
			slog.String("msg_task_id", m.TaskID),
			slog.String("attempt_task_id", attempt.TaskID),
		)
		return nil
	}

	// The subject's worker ID was enforced by NATS when broker auth is on;
	// the payload's was asserted by whoever sent the message. Trust the
	// subject, and treat a mismatch as permanent — redelivery cannot make a
	// forged or stale message legal, so ack it away rather than Nak into a
	// loop, the same reasoning as ErrInvalidTransition above.
	if subjectWorkerID != attempt.WorkerID {
		s.logger.WarnContext(
			ctx, "scheduler: task.status from a worker that does not hold this task — discarding",
			slog.String("task_id", m.TaskID),
			slog.String("attempt_id", m.AttemptID),
			slog.String("subject_worker_id", subjectWorkerID),
			slog.String("attempt_worker_id", attempt.WorkerID),
		)
		return nil
	}

	at := m.At
	if at.IsZero() {
		at = time.Now().UTC()
	}

	switch m.Status {
	case "running":
		return s.handleTaskRunning(ctx, attempt, m)
	case "succeeded":
		return s.handleTaskTerminal(ctx, attempt, m, store.TaskStatusSucceeded, store.AttemptStatusSucceeded, at)
	case "failed":
		return s.handleTaskFailed(ctx, attempt, m, at)
	case "canceled":
		return s.handleTaskTerminal(ctx, attempt, m, store.TaskStatusCanceled, store.AttemptStatusCanceled, at)
	default:
		s.logger.WarnContext(
			ctx, "scheduler: unknown task status value — discarding",
			slog.String("status", m.Status),
			slog.String("task_id", m.TaskID),
		)
		return nil
	}
}

// handleTaskRunning handles a "running" TaskStatusMsg: updates the task row to
// [store.TaskStatusRunning] and records the OpenJD session ID on the attempt.
func (s *Scheduler) handleTaskRunning(ctx context.Context, attempt store.TaskAttempt, m protocol.TaskStatusMsg) error {
	// Update task status to running.
	if err := s.store.UpdateTaskStatus(ctx, m.TaskID, store.TaskStatusRunning); err != nil {
		return err
	}

	// Record the session ID on the attempt (COALESCE-safe: ignored if empty).
	if m.SessionID != "" {
		updated := attempt
		updated.SessionID = m.SessionID
		if _, err := s.store.UpdateTaskAttempt(ctx, updated); err != nil {
			// Non-fatal: the task is running; losing the session ID is a minor
			// attribution issue, not a correctness problem.
			s.logger.WarnContext(
				ctx, "scheduler: update attempt session_id failed",
				slog.String("attempt_id", attempt.ID),
				slog.Any("error", err),
			)
		}
	}

	s.logger.InfoContext(
		ctx, "scheduler: task running",
		slog.String("task_id", m.TaskID),
		slog.String("attempt_id", m.AttemptID),
		slog.String("session_id", m.SessionID),
	)

	// Notify WebSocket hub. GetTask is a cheap primary-key lookup; if it fails
	// we skip the notification rather than aborting the status update.
	if task, err := s.store.GetTask(ctx, m.TaskID); err == nil {
		s.notifier.NotifyTask(ws.TaskEvent{
			JobID:     task.JobID,
			TaskID:    task.ID,
			Name:      task.Name,
			Status:    string(store.TaskStatusRunning),
			WorkerID:  attempt.WorkerID,
			UpdatedAt: time.Now().UTC(),
		})

		// Promote the enclosing job to running on its first running task. This
		// stamps the job's StartedAt in the store's guarded promotion; without it
		// the job would stay pending and never record a start time.
		s.maybePromoteJobRunning(ctx, task.JobID)
	}
	return nil
}

// maybePromoteJobRunning moves a pending job to running on its first running
// task, which stamps StartedAt. store.PromoteJobRunning is guarded on pending
// (invariant I1), so a late report can never un-pause or revive a job.
// Best-effort: a failure is logged, not propagated, since the task itself has
// already been recorded running and a subsequent running report will retry.
func (s *Scheduler) maybePromoteJobRunning(ctx context.Context, jobID string) {
	now := time.Now().UTC()
	promoted, err := s.store.PromoteJobRunning(ctx, jobID, now)
	if err != nil {
		s.logger.WarnContext(
			ctx, "scheduler: promote job running failed",
			slog.String("job_id", jobID),
			slog.Any("error", err),
		)
		return
	}
	if !promoted {
		return
	}
	s.notifier.NotifyJob(ws.JobEvent{
		JobID:     jobID,
		Status:    string(store.JobStatusRunning),
		UpdatedAt: now,
	})
}

// handleTaskTerminal handles a terminal TaskStatusMsg (succeeded/failed/canceled):
// closes the attempt, releases its usage slots and moves the task in one store
// write ([store.TaskStore.CompleteTaskAttempt]), then checks step/job completion.
//
// A report the state machine refuses (the task already reached a different
// terminal status, or went back to ready or pending through a reap, an offline
// reclaim or a retry) still closes the attempt and frees its slots; it returns
// [store.ErrInvalidTransition] so the consumer acks it.
func (s *Scheduler) handleTaskTerminal(
	ctx context.Context,
	attempt store.TaskAttempt,
	m protocol.TaskStatusMsg,
	taskStatus store.TaskStatus,
	attemptStatus store.AttemptStatus,
	at time.Time,
) error {
	// ── Attempt, claims and task in one write (invariant I3) ──────────────
	// The attempt close is a guarded no-op on the failed path, whose
	// RecordTaskFailure already closed it; the claim release is unconditional, and
	// so is its commit: a refused task transition (below) still frees the slot.
	var reason string
	if taskStatus == store.TaskStatusFailed || taskStatus == store.TaskStatusCanceled {
		// An empty synthesized reason (worker "canceled" echoes always carry an
		// empty Message, see internal/worker/executor/run.go) is not stamped, so it
		// can never overwrite a server-set reason such as CancelTask/CancelJob's
		// "canceled by user" or the cascade's "canceled: upstream step failed".
		reason = failureReasonOrFallback(m.Message, m.ExitCode, taskStatus)
	}
	res, err := s.store.CompleteTaskAttempt(ctx, store.AttemptCompletion{
		AttemptID: attempt.ID, TaskID: m.TaskID, TaskStatus: taskStatus, AttemptStatus: attemptStatus,
		ExitCode: m.ExitCode, SessionID: m.SessionID, Message: m.Message, FailureReason: reason, EndedAt: at,
	})
	if err != nil {
		return err
	}
	// The attempt is now terminal (closed above, or already closed by
	// RecordTaskFailure for the failed path), so no further log chunks will be
	// produced for it and its cached ownership entry is no longer needed.
	// SQI_LOGS and SQI_STATUS are separate streams, so a chunk published just
	// before this status can still be consumed after it; that is harmless, since
	// a cache miss falls back to the store and re-reads correctly.
	s.attemptCache.evict(attempt.ID)

	// Fetch the task to get its StepID and JobID.
	task, err := s.store.GetTask(ctx, m.TaskID)
	if err != nil {
		return err
	}

	// Wake any parked lease waiters: the attempt's usage claims were released
	// whether or not the task moved, and a completed task frees the worker's
	// cores, so pending tasks may now fit.
	s.notifyQueueForJob(ctx, task.JobID)

	if res.Rejected {
		// The task no longer holds a status this report can move it from: it
		// reached a different terminal status (a cancel), or it went back to ready
		// or pending (a reap, an offline reclaim, or a retry that raced this
		// report). The claims were released above, so nothing leaks. Returned as
		// ErrInvalidTransition so the consumer acks the message instead of
		// redelivering a report that can never become legal.
		return fmt.Errorf("scheduler: task %s: %w", m.TaskID, store.ErrInvalidTransition)
	}

	s.logger.InfoContext(
		ctx, "scheduler: task terminal",
		slog.String("task_id", m.TaskID),
		slog.String("status", m.Status),
		slog.String("attempt_id", m.AttemptID),
	)

	// Notify WebSocket hub of the terminal status change.
	s.notifier.NotifyTask(ws.TaskEvent{
		JobID:     task.JobID,
		TaskID:    task.ID,
		Name:      task.Name,
		Status:    string(taskStatus),
		WorkerID:  attempt.WorkerID,
		UpdatedAt: time.Now().UTC(),
	})

	return s.checkStepCompletion(ctx, task.StepID, task.JobID)
}

// failureReasonOrFallback returns m.Message, or a synthesized fallback so a
// terminal non-success is never blank. Canceled with no message stays blank:
// a server-originated cancel stamps its own reason, only on a task with none
// yet, inside the store write that cancels the task (CancelJobExecution,
// CancelTaskExecution, CancelPendingStep or CancelBlockedJob).
func failureReasonOrFallback(msg string, exitCode *int, status store.TaskStatus) string {
	if msg != "" {
		return msg
	}
	if status == store.TaskStatusFailed {
		if exitCode != nil {
			return fmt.Sprintf("failed (exit %d)", *exitCode)
		}
		return "failed"
	}
	return ""
}

// ── Step and job completion ───────────────────────────────────────────────────

// checkStepCompletion finalizes the step when every task is terminal
// (store.FinalizeStep computes the outcome inside the write, invariant I4, with
// no page limit), then propagates dependencies and finalizes the job.
//
// Propagation runs whenever the step is terminal, not only when this call
// wrote it: a redelivered completion whose first delivery finalized the step
// and then died must still release or cancel dependents. Every step involved
// is idempotent.
func (s *Scheduler) checkStepCompletion(ctx context.Context, stepID, jobID string) error {
	status, changed, err := s.store.FinalizeStep(ctx, stepID, time.Now().UTC())
	if err != nil {
		return err
	}
	if status == "" {
		return nil // a task is still in flight
	}
	if changed {
		s.logger.InfoContext(
			ctx, "scheduler: step complete",
			slog.String("step_id", stepID),
			slog.String("status", string(status)),
		)
	}
	if err := s.propagateStepDependencies(ctx, jobID, status); err != nil {
		return err
	}
	return s.checkJobCompletion(ctx, jobID)
}

// propagateStepDependencies updates dependent steps after a step reaches the
// terminal status newStepStatus. When the step completed successfully it resolves
// dependencies so any step waiting on it can have its tasks promoted to ready.
// When the step terminated unsuccessfully it cascade-cancels any step that
// depended on it, since such steps can never become ready — otherwise they would
// strand in pending and the job could never reach a terminal state.
//
// The error is returned (not swallowed) so the caller can nak the triggering
// message and let JetStream redeliver: a transient store error mid-cascade would
// otherwise leave dependents stranded and re-introduce the job hang this logic
// exists to prevent. All three store operations involved are idempotent, so
// redelivery is safe.
func (s *Scheduler) propagateStepDependencies(ctx context.Context, jobID string, newStepStatus store.StepStatus) error {
	if newStepStatus == store.StepStatusCompleted {
		n, err := openjd.ResolveDependencies(ctx, s.store, jobID)
		if err != nil {
			return fmt.Errorf("scheduler: resolve dependencies for job %s: %w", jobID, err)
		}
		if n > 0 {
			s.logger.InfoContext(
				ctx, "scheduler: promoted dependent steps to ready",
				slog.String("job_id", jobID),
				slog.Int("steps_promoted", n),
			)
			// Wake parked lease waiters: newly ready tasks may fit waiting workers.
			s.notifyQueueForJob(ctx, jobID)
		}
		return nil
	}

	n, canceledTasks, err := openjd.CancelDependents(ctx, s.store, jobID)
	if err != nil {
		return fmt.Errorf("scheduler: cancel dependents for job %s: %w", jobID, err)
	}
	if n > 0 {
		s.logger.InfoContext(
			ctx, "scheduler: canceled dependent steps after upstream failure",
			slog.String("job_id", jobID),
			slog.Int("steps_canceled", n),
			slog.Int("tasks_canceled", len(canceledTasks)),
		)
	}

	// Fan the cascade-canceled tasks out to WebSocket subscribers; they were never
	// assigned, so there is no worker to attribute. The durable failure reason
	// was stamped by CancelDependents in the same UPDATE that canceled them.
	now := time.Now().UTC()
	for _, t := range canceledTasks {
		s.notifier.NotifyTask(ws.TaskEvent{
			JobID:     t.JobID,
			TaskID:    t.ID,
			Name:      t.Name,
			Status:    string(store.TaskStatusCanceled),
			UpdatedAt: now,
		})
	}
	return nil
}

// checkJobCompletion finalizes the job when every step is terminal
// (store.FinalizeJob, invariant I4). The job event is emitted only by the call
// that wrote the status; dependents are reconciled whenever the job is terminal,
// because ReconcileDependents is idempotent and a redelivery must not strand
// them.
func (s *Scheduler) checkJobCompletion(ctx context.Context, jobID string) error {
	status, changed, err := s.store.FinalizeJob(ctx, jobID, time.Now().UTC())
	if err != nil {
		return err
	}
	if status == "" {
		return nil // a step is still in flight
	}
	if changed {
		s.logger.InfoContext(
			ctx, "scheduler: job complete",
			slog.String("job_id", jobID),
			slog.String("status", string(status)),
		)

		// Notify WebSocket hub of the job completion event.
		s.notifier.NotifyJob(ws.JobEvent{
			JobID:     jobID,
			Status:    string(status),
			UpdatedAt: time.Now().UTC(),
		})
	}

	// Release or cancel any jobs blocked on this one now that it is terminal.
	// Non-fatal: the periodic sweep is the backstop, so a failure here must not
	// block completion of the upstream job itself.
	if err := s.ReconcileDependents(ctx, jobID); err != nil {
		s.logger.ErrorContext(ctx, "scheduler: reconcile dependents after completion failed",
			slog.String("job_id", jobID), slog.Any("error", err))
	}
	return nil
}
