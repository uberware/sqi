// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Server-side handlers that consume worker messages from NATS
// subjects and persist state via the Store.
//
// This file implements the task-status consumer — the counterpart of the
// worker-register and worker-heartbeat consumers already in scheduler.go.
// When a worker publishes a protocol.TaskStatusMsg to task.status.<worker>.<job>,
// handleTaskStatusMessage updates the store, closes the attempt record,
// releases usage pool slots, and drives step/job completion logic.
//
// Flow for each TaskStatusMsg:
//
//  1. Decode protocol.TaskStatusMsg.
//  2. Load the task attempt from the store to verify AttemptID.
//  3. For "running": update task status and record session ID on the attempt.
//  4. For terminal states (succeeded/failed/canceled):
//     a. Close the attempt (EndedAt, Status, ExitCode).
//     b. Transition the task to the matching terminal status.
//     c. Release any held usage pool slots.
//     d. Check whether the enclosing step is now complete.
//     e. If the step completed successfully, call ResolveDependencies to
//        unblock downstream steps.
//     f. Check whether the enclosing job is now complete.
//
// Step and job completion checks use simple list scans over the step's tasks
// and the job's steps.  This is acceptable for Phase 1 workloads; large jobs
// with many thousands of tasks per step would benefit from a counter-based
// approach added as a schema migration.

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
		// stamps the job's StartedAt via the store's COALESCE-on-running logic;
		// without it the job would stay pending and never record a start time.
		s.maybePromoteJobRunning(ctx, task.JobID)
	}
	return nil
}

// maybePromoteJobRunning transitions a job from pending to running, which the
// store uses to stamp StartedAt. It is a no-op for any non-pending status so a
// late task report cannot un-pause a paused job or revive a terminal one.
// Best-effort: failures are logged, not propagated, since the task itself has
// already been recorded running and a subsequent running report will retry.
func (s *Scheduler) maybePromoteJobRunning(ctx context.Context, jobID string) {
	job, err := s.store.GetJob(ctx, jobID)
	if err != nil {
		s.logger.WarnContext(
			ctx, "scheduler: promote job running: get job failed",
			slog.String("job_id", jobID),
			slog.Any("error", err),
		)
		return
	}
	if job.Status != store.JobStatusPending {
		return
	}
	if err := s.store.UpdateJobStatus(ctx, jobID, store.JobStatusRunning); err != nil {
		s.logger.WarnContext(
			ctx, "scheduler: promote job running: update status failed",
			slog.String("job_id", jobID),
			slog.Any("error", err),
		)
		return
	}
	s.notifier.NotifyJob(ws.JobEvent{
		JobID:     jobID,
		Status:    string(store.JobStatusRunning),
		UpdatedAt: time.Now().UTC(),
	})
}

// handleTaskTerminal handles a terminal TaskStatusMsg (succeeded/failed/canceled):
// closes the attempt, updates the task, releases usage slots, and checks step/job
// completion.
func (s *Scheduler) handleTaskTerminal(
	ctx context.Context,
	attempt store.TaskAttempt,
	m protocol.TaskStatusMsg,
	taskStatus store.TaskStatus,
	attemptStatus store.AttemptStatus,
	at time.Time,
) error {
	// ── Close the attempt record ──────────────────────────────────────────
	// Skipped for failed: that path only arrives here via handleTaskFailed,
	// whose RecordTaskFailure already closed the attempt (same status, end
	// time, exit code, session, and message) inside its transaction.
	if attemptStatus != store.AttemptStatusFailed {
		updated := attempt
		updated.Status = attemptStatus
		updated.SessionID = m.SessionID // empty = no-change via COALESCE in SQL
		updated.EndedAt = &at
		updated.Message = m.Message
		if m.ExitCode != nil {
			code := *m.ExitCode
			updated.ExitCode = &code
		}
		if _, err := s.store.UpdateTaskAttempt(ctx, updated); err != nil {
			return err
		}
	}
	// The attempt is now terminal (closed above, or already closed by
	// RecordTaskFailure for the failed path), so no further log chunks will
	// be produced for it and its cached ownership entry is no longer needed.
	// SQI_LOGS and SQI_STATUS are separate streams, so a chunk published just
	// before this status can still be consumed after it — that is harmless,
	// since a cache miss falls back to the store and re-reads correctly.
	s.attemptCache.evict(attempt.ID)

	// ── Transition the task ───────────────────────────────────────────────
	if err := s.store.UpdateTaskStatus(ctx, m.TaskID, taskStatus); err != nil {
		return err
	}

	// ── Durable per-task failure reason for terminal non-success ─────────
	// An empty synthesized reason (worker "canceled" echoes always carry an
	// empty Message — see internal/worker/executor/run.go) must never
	// overwrite an existing, server-set reason (e.g. CancelTask/CancelJob's
	// "canceled by user" or the cascade's "canceled: upstream step failed").
	if taskStatus == store.TaskStatusFailed || taskStatus == store.TaskStatusCanceled {
		if reason := failureReasonOrFallback(m.Message, m.ExitCode, taskStatus); reason != "" {
			if err := s.store.SetTaskFailureReason(ctx, m.TaskID, reason); err != nil {
				return err
			}
		}
	}

	// ── Release usage pool slots ──────────────────────────────────────────
	if err := s.ReleaseTaskUsage(ctx, attempt.ID); err != nil {
		// Non-fatal: a leaked slot is recovered by the next usage count
		// check, and the job is not blocked.
		s.logger.WarnContext(
			ctx, "scheduler: release task usage claims failed",
			slog.String("attempt_id", attempt.ID),
			slog.Any("error", err),
		)
	}

	s.logger.InfoContext(
		ctx, "scheduler: task terminal",
		slog.String("task_id", m.TaskID),
		slog.String("status", m.Status),
		slog.String("attempt_id", m.AttemptID),
	)

	// ── Step / job completion ─────────────────────────────────────────────
	// Fetch the task to get its StepID and JobID.
	task, err := s.store.GetTask(ctx, m.TaskID)
	if err != nil {
		return err
	}

	// Wake any parked lease waiters: a completed task frees the worker's cores
	// so pending tasks may now fit.
	s.notifyQueueForJob(ctx, task.JobID)

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
// terminal non-success is never blank. Canceled with no message stays blank
// (server-originated cancels set their own reason via SetTaskFailureReason).
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

// checkStepCompletion inspects the task statuses of all tasks in the step.
// If every task has reached a terminal state it transitions the step and then
// calls [checkJobCompletion].  If all tasks succeeded it also calls
// [openjd.ResolveDependencies] to unblock any downstream steps.
//
// For Phase 1 workloads (steps with up to a few thousand tasks) a single
// [store.MaxLimit] page is sufficient. Steps with more than [store.MaxLimit]
// tasks would need multiple pages — that path is left as a TODO since it is
// not expected to be exercised in Phase 1.
func (s *Scheduler) checkStepCompletion(ctx context.Context, stepID, jobID string) error {
	page, err := s.store.ListTasks(ctx, store.ListTasksOptions{
		StepID: stepID,
		Pagination: store.Pagination{
			Limit: store.MaxLimit,
		},
	})
	if err != nil {
		return err
	}

	// Guard against silent truncation: if the step has more tasks than a single
	// MaxLimit page can return, the completion check would only see the first
	// MaxLimit tasks and might incorrectly mark a step complete while others are
	// still pending.  Return an error rather than silently computing a wrong result.
	if page.Total > store.MaxLimit {
		s.logger.WarnContext(
			ctx, "scheduler: checkStepCompletion: step exceeds MaxLimit — skipping to avoid incorrect completion",
			slog.String("step_id", stepID),
			slog.Int("total", page.Total),
			slog.Int("max_limit", store.MaxLimit),
		)
		return fmt.Errorf("step %q has %d tasks, exceeding MaxLimit (%d): pagination required for correct completion check",
			stepID, page.Total, store.MaxLimit)
	}

	allDone := true
	anyFailed := false
	anyCanceled := false

	for _, t := range page.Items {
		if !isTerminalTaskStatus(t.Status) {
			allDone = false
			break
		}
		switch t.Status {
		case store.TaskStatusFailed:
			anyFailed = true
		case store.TaskStatusCanceled:
			anyCanceled = true
		}
	}

	if !allDone {
		// Step is still running; nothing to do.
		return nil
	}

	var newStepStatus store.StepStatus
	switch {
	case anyFailed:
		newStepStatus = store.StepStatusFailed
	case anyCanceled:
		newStepStatus = store.StepStatusCanceled
	default:
		newStepStatus = store.StepStatusCompleted
	}

	if err := s.store.UpdateStepStatus(ctx, stepID, newStepStatus); err != nil {
		return err
	}

	s.logger.InfoContext(
		ctx, "scheduler: step complete",
		slog.String("step_id", stepID),
		slog.String("status", string(newStepStatus)),
	)

	if err := s.propagateStepDependencies(ctx, jobID, newStepStatus); err != nil {
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

// checkJobCompletion inspects all steps in the job.  If every step has reached
// a terminal state it transitions the job to the matching terminal status.
func (s *Scheduler) checkJobCompletion(ctx context.Context, jobID string) error {
	steps, err := s.store.ListSteps(ctx, jobID)
	if err != nil {
		return err
	}

	allDone := true
	anyFailed := false
	anyCanceled := false

	for _, st := range steps {
		if !isTerminalStepStatus(st.Status) {
			allDone = false
			break
		}
		switch st.Status {
		case store.StepStatusFailed:
			anyFailed = true
		case store.StepStatusCanceled:
			anyCanceled = true
		}
	}

	if !allDone {
		return nil
	}

	var newJobStatus store.JobStatus
	switch {
	case anyFailed:
		newJobStatus = store.JobStatusFailed
	case anyCanceled:
		newJobStatus = store.JobStatusCanceled
	default:
		newJobStatus = store.JobStatusCompleted
	}

	if err := s.store.UpdateJobStatus(ctx, jobID, newJobStatus); err != nil {
		return err
	}

	s.logger.InfoContext(
		ctx, "scheduler: job complete",
		slog.String("job_id", jobID),
		slog.String("status", string(newJobStatus)),
	)

	// Notify WebSocket hub of the job completion event.
	s.notifier.NotifyJob(ws.JobEvent{
		JobID:     jobID,
		Status:    string(newJobStatus),
		UpdatedAt: time.Now().UTC(),
	})

	// Release or cancel any jobs blocked on this one now that it is terminal.
	// Non-fatal: the periodic sweep is the backstop, so a failure here must not
	// block completion of the upstream job itself.
	if err := s.ReconcileDependents(ctx, jobID); err != nil {
		s.logger.ErrorContext(ctx, "scheduler: reconcile dependents after completion failed",
			slog.String("job_id", jobID), slog.Any("error", err))
	}
	return nil
}

// ── Terminal-state helpers ────────────────────────────────────────────────────

// isTerminalTaskStatus reports whether s is a terminal task state.
func isTerminalTaskStatus(s store.TaskStatus) bool {
	switch s {
	case store.TaskStatusSucceeded, store.TaskStatusFailed, store.TaskStatusCanceled:
		return true
	}
	return false
}

// isTerminalStepStatus reports whether s is a terminal step state.
func isTerminalStepStatus(s store.StepStatus) bool {
	switch s {
	case store.StepStatusCompleted, store.StepStatusFailed, store.StepStatusCanceled:
		return true
	}
	return false
}
