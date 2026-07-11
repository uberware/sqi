// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Cancellation propagation.
//
// CancelJob and CancelTask are the server-side entry points for explicit
// cancellation (triggered by the REST API layer). Both
// methods follow the same sequence used by the heartbeat sweep:
//
//  1. Close running task attempts ([store.TaskAttemptStore.CancelJobAttempts]).
//  2. Transition all non-terminal tasks to [store.TaskStatusCanceled]
//     ([store.TaskStore.CancelJobTasks]).
//  3. Publish a task.cancel.<taskID> NATS signal to each worker that was
//     actively executing a task ([bus.Client.PublishTaskCancel]) so the worker
//     can interrupt the running process without waiting for the next heartbeat
//     timeout.
//  4. Release all usage pool slots held by the job
//     ([store.UsageClaimStore.ReleaseJobClaims]).
//
// NATS publish failures are non-fatal: the scheduler logs a warning and
// continues.  Workers that miss the cancel signal will eventually be reclaimed
// by the heartbeat sweep once they go past the WorkerTimeout threshold.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// cancelPayload is the JSON body published to task.cancel.<taskID> when the
// server signals an assigned worker to stop executing a task.
type cancelPayload struct {
	TaskID     string    `json:"task_id"`
	JobID      string    `json:"job_id"`
	CanceledAt time.Time `json:"canceled_at"`
}

// CancelJob transitions all non-terminal tasks for the given job to canceled,
// closes any running task attempts, releases held usage pool slots, and publishes
// a task.cancel.<taskID> signal to each worker that was actively executing a
// task at the time of cancellation.
//
// CancelJob does NOT update the job's own status; that is the caller's
// responsibility (typically the REST handler that also calls
// [store.JobStore.UpdateJobStatus]).
//
// The method is idempotent: if all tasks are already in terminal states the
// store operations are no-ops and no NATS messages are published.
func (s *Scheduler) CancelJob(ctx context.Context, jobID string) error {
	now := time.Now().UTC()

	// Step 1: close running attempts before canceling the task rows so that
	// the attempt EndedAt is recorded while the tasks still carry their
	// assigned_worker_id (mirrors the heartbeat sweep pattern).
	nAttempts, err := s.store.CancelJobAttempts(ctx, jobID, now)
	if err != nil {
		return fmt.Errorf("scheduler: cancel attempts for job %s: %w", jobID, err)
	}
	if nAttempts > 0 {
		s.logger.InfoContext(
			ctx, "scheduler: closed running attempts for canceled job",
			slog.String("job_id", jobID),
			slog.Int("attempts_closed", nAttempts),
		)
	}

	// Snapshot the IDs of tasks about to be canceled by this call (all
	// non-terminal tasks in the job right now) *before* CancelJobTasks runs,
	// so the "canceled by user" annotation below lands only on tasks this
	// call actually transitioned — not on tasks that were already canceled
	// earlier for a different reason (e.g. a cascade-cancel).
	toAnnotate := s.nonTerminalTaskIDs(ctx, jobID)

	// Step 2: cancel all non-terminal tasks; capture the previously active
	// tasks so we know which workers to notify.
	activeTasks, err := s.store.CancelJobTasks(ctx, jobID, now)
	if err != nil {
		return fmt.Errorf("scheduler: cancel tasks for job %s: %w", jobID, err)
	}

	// Step 3: publish cancel signals to all workers that had active tasks.
	s.publishCancelSignals(ctx, activeTasks, now)

	// Annotate every task this call just canceled with the durable reason.
	// Guarded (IfEmpty) so a concurrent cascade-cancel that already recorded a
	// more specific cause ("canceled: upstream step failed") is never clobbered
	// — cascade always wins regardless of ordering. Best-effort: a failure here
	// does not roll back the cancellation itself, since the reason is an
	// explanatory annotation, not part of the state machine.
	for _, id := range toAnnotate {
		if err := s.store.SetTaskFailureReasonIfEmpty(ctx, id, "canceled by user"); err != nil {
			s.logger.WarnContext(
				ctx, "scheduler: set failure reason for canceled task failed",
				slog.String("task_id", id),
				slog.Any("error", err),
			)
		}
	}

	// Step 4: release all usage pool slots held by the job.
	n, err := s.store.ReleaseJobClaims(ctx, jobID, now)
	if err != nil {
		// Non-fatal: log and continue — claims will be cleaned up when the
		// worker next tries to claim a slot and finds the attempt gone.
		s.logger.WarnContext(
			ctx, "scheduler: release job usage claims failed",
			slog.String("job_id", jobID),
			slog.Any("error", err),
		)
	} else if n > 0 {
		s.logger.InfoContext(
			ctx, "scheduler: released usage claims for canceled job",
			slog.String("job_id", jobID),
			slog.Int("claims_released", n),
		)
	}

	s.logger.InfoContext(
		ctx, "scheduler: job cancellation complete",
		slog.String("job_id", jobID),
		slog.Int("active_tasks_canceled", len(activeTasks)),
	)
	return nil
}

// CancelTask transitions a single task to [store.TaskStatusCanceled], closes
// its running attempt if any, releases held usage pool slots, and publishes a
// task.cancel.<taskID> signal to the assigned worker.
//
// If the task is already in a terminal state (succeeded, failed, canceled),
// CancelTask returns nil without modifying any state.
func (s *Scheduler) CancelTask(ctx context.Context, taskID string) error {
	now := time.Now().UTC()

	task, err := s.store.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("scheduler: get task %s: %w", taskID, err)
	}

	// No-op if already terminal.
	switch task.Status {
	case store.TaskStatusSucceeded, store.TaskStatusFailed, store.TaskStatusCanceled:
		s.logger.DebugContext(
			ctx, "scheduler: cancel task — already terminal",
			slog.String("task_id", taskID),
			slog.String("status", string(task.Status)),
		)
		return nil
	}

	workerID := task.AssignedWorkerID
	jobID := task.JobID

	// Close the latest running attempt before updating the task status.
	if closeErr := s.closeSingleTaskAttempt(ctx, taskID, now); closeErr != nil {
		s.logger.WarnContext(
			ctx, "scheduler: close task attempt on cancel failed",
			slog.String("task_id", taskID),
			slog.Any("error", closeErr),
		)
		// Non-fatal: continue with task cancellation.
	}

	if err = s.store.UpdateTaskStatus(ctx, taskID, store.TaskStatusCanceled); err != nil {
		return fmt.Errorf("scheduler: transition task %s to canceled: %w", taskID, err)
	}

	// Annotate the durable failure reason. Guarded (IfEmpty) so a concurrent
	// cascade-cancel's more specific reason is never clobbered. Best-effort:
	// does not roll back the cancellation itself — the reason is an annotation.
	if err = s.store.SetTaskFailureReasonIfEmpty(ctx, taskID, "canceled by user"); err != nil {
		s.logger.WarnContext(
			ctx, "scheduler: set failure reason for canceled task failed",
			slog.String("task_id", taskID),
			slog.Any("error", err),
		)
	}

	// Publish a cancel signal to the assigned worker (if any).
	if workerID != "" {
		s.publishCancelSignals(ctx, []store.Task{task}, now)
	}

	// Release any usage pool slots held by this task's latest attempt.
	attempt, err := s.store.LatestTaskAttempt(ctx, taskID)
	if err == nil {
		if releaseErr := s.ReleaseTaskUsage(ctx, attempt.ID); releaseErr != nil {
			s.logger.WarnContext(
				ctx, "scheduler: release usage claims after task cancel failed",
				slog.String("task_id", taskID),
				slog.String("attempt_id", attempt.ID),
				slog.Any("error", releaseErr),
			)
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		s.logger.WarnContext(
			ctx, "scheduler: latest attempt lookup failed during task cancel",
			slog.String("task_id", taskID),
			slog.Any("error", err),
		)
	}

	s.logger.InfoContext(
		ctx, "scheduler: task canceled",
		slog.String("task_id", taskID),
		slog.String("job_id", jobID),
		slog.String("worker_id", workerID),
	)
	return nil
}

// publishCancelSignals publishes a task.cancel.<taskID> NATS message for each
// task in activeTasks that has an assigned worker.  NATS publish failures are
// non-fatal: a warning is logged and iteration continues so all other workers
// still receive their signals.
func (s *Scheduler) publishCancelSignals(ctx context.Context, activeTasks []store.Task, canceledAt time.Time) {
	for _, t := range activeTasks {
		if t.AssignedWorkerID == "" {
			continue
		}
		payload, err := json.Marshal(cancelPayload{
			TaskID:     t.ID,
			JobID:      t.JobID,
			CanceledAt: canceledAt,
		})
		if err != nil {
			// json.Marshal of a struct with no interface{} fields never fails in
			// practice; log and skip rather than aborting remaining signals.
			s.logger.WarnContext(
				ctx, "scheduler: marshal cancel payload failed",
				slog.String("task_id", t.ID),
				slog.Any("error", err),
			)
			continue
		}

		if err = s.bus.PublishTaskCancel(ctx, t.ID, payload); err != nil {
			s.logger.WarnContext(
				ctx, "scheduler: publish cancel signal failed — worker will time out via heartbeat sweep",
				slog.String("task_id", t.ID),
				slog.String("worker_id", t.AssignedWorkerID),
				slog.Any("error", err),
			)
			continue
		}

		s.logger.InfoContext(
			ctx, "scheduler: cancel signal published to worker",
			slog.String("task_id", t.ID),
			slog.String("worker_id", t.AssignedWorkerID),
		)
	}
}

// nonTerminalTaskIDs returns the IDs of every task in jobID that is not yet in
// a terminal state. Used by CancelJob to enumerate the tasks it is about to
// cancel (including pending/ready ones, which CancelJobTasks cancels but does
// not return), so the "canceled by user" annotation can be applied to all of
// them. It is snapshotted before CancelJobTasks flips the rows; correctness of
// the annotation itself does not depend on this timing — the guarded
// SetTaskFailureReasonIfEmpty ensures a more specific reason (e.g. a
// cascade-cancel) is never overwritten regardless of ordering. Best-effort: a
// lookup failure is logged and yields an empty (not partial) result.
func (s *Scheduler) nonTerminalTaskIDs(ctx context.Context, jobID string) []string {
	page, err := s.store.ListTasks(ctx, store.ListTasksOptions{
		JobID: jobID,
		Statuses: []store.TaskStatus{
			store.TaskStatusPending, store.TaskStatusReady,
			store.TaskStatusAssigned, store.TaskStatusRunning,
		},
		Pagination: store.Pagination{Limit: store.MaxLimit},
	})
	if err != nil {
		s.logger.WarnContext(
			ctx, "scheduler: list non-terminal tasks for cancel-reason annotation failed",
			slog.String("job_id", jobID),
			slog.Any("error", err),
		)
		return nil
	}
	if page.Total > store.MaxLimit {
		// Same truncation guard as checkStepCompletion: rather than
		// mis-annotate a subset, skip annotation for this outsized job.
		s.logger.WarnContext(
			ctx, "scheduler: job exceeds MaxLimit — skipping cancel-reason annotation",
			slog.String("job_id", jobID),
			slog.Int("total", page.Total),
		)
		return nil
	}
	ids := make([]string, len(page.Items))
	for i, t := range page.Items {
		ids[i] = t.ID
	}
	return ids
}

// closeSingleTaskAttempt marks the latest running attempt for taskID as
// canceled.  It is a no-op when no running attempt exists.
func (s *Scheduler) closeSingleTaskAttempt(ctx context.Context, taskID string, now time.Time) error {
	attempt, err := s.store.LatestTaskAttempt(ctx, taskID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("latest attempt for task %s: %w", taskID, err)
	}
	if attempt.Status != store.AttemptStatusRunning {
		return nil
	}
	attempt.Status = store.AttemptStatusCanceled
	attempt.EndedAt = &now
	if _, err = s.store.UpdateTaskAttempt(ctx, attempt); err != nil {
		return fmt.Errorf("close attempt %s: %w", attempt.ID, err)
	}
	return nil
}
