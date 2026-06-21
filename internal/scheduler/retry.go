// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Retry revives failed/canceled work so it can run again.
//
// RetryJob and RetryTask are the server-side entry points for explicit retry
// (triggered by the REST API layer). Both delegate to the store's RetryTasks
// primitive — which transitions the target failed/canceled tasks, their
// terminal steps, and the terminal job back to pending in one transaction —
// then re-run openjd.ResolveDependencies to re-gate the revived tasks in
// dependency order, and fan the resulting status changes out to WebSocket
// subscribers.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/ws"
)

// RetryJob revives every failed/canceled task in the job. It returns the number
// of tasks revived (0 when none were eligible — an idempotent no-op).
func (s *Scheduler) RetryJob(ctx context.Context, jobID string) (int, error) {
	return s.retry(ctx, jobID, nil)
}

// RetryTask revives a single failed/canceled task, looking up its job to drive
// dependency re-resolution.
func (s *Scheduler) RetryTask(ctx context.Context, taskID string) error {
	task, err := s.store.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("scheduler: get task %s: %w", taskID, err)
	}
	_, err = s.retry(ctx, task.JobID, []string{taskID})
	return err
}

// retry is the shared revival path. taskIDs == nil revives all eligible tasks in
// the job; otherwise only the given IDs.
func (s *Scheduler) retry(ctx context.Context, jobID string, taskIDs []string) (int, error) {
	now := time.Now().UTC()

	revived, err := s.store.RetryTasks(ctx, jobID, taskIDs, now)
	if err != nil {
		return 0, fmt.Errorf("scheduler: retry tasks for job %s: %w", jobID, err)
	}
	if len(revived) == 0 {
		return 0, nil
	}

	// Re-gate pending tasks: promote steps whose dependencies are satisfied.
	if _, err := openjd.ResolveDependencies(ctx, s.store, jobID); err != nil {
		return 0, fmt.Errorf("scheduler: resolve dependencies for job %s: %w", jobID, err)
	}

	// Re-cancel tasks whose step is still blocked on a failed/canceled upstream.
	// This handles the case where only a downstream task is retried but its
	// upstream step remains failed — without this the downstream strands in
	// pending forever (ListReadyTasks ignores pending, checkJobCompletion never fires).
	_, canceledTasks, err := openjd.CancelDependents(ctx, s.store, jobID)
	if err != nil {
		return 0, fmt.Errorf("scheduler: cancel dependents for job %s: %w", jobID, err)
	}

	// Finalize the job if every task has now reached a terminal state.
	if err := s.checkJobCompletion(ctx, jobID); err != nil {
		return 0, fmt.Errorf("scheduler: check job completion for job %s: %w", jobID, err)
	}

	// Build a set of revived task IDs so we can deduplicate notifications below.
	revivedIDs := make(map[string]bool, len(revived))
	for _, rt := range revived {
		revivedIDs[rt.ID] = true
	}

	// Notify each revived task with its actual post-reconciliation status.
	for _, rt := range revived {
		status := string(store.TaskStatusPending)
		if cur, err := s.store.GetTask(ctx, rt.ID); err == nil {
			status = string(cur.Status)
		} else {
			s.logger.WarnContext(ctx, "scheduler: retry: get task for notify failed",
				slog.String("task_id", rt.ID), slog.Any("error", err))
		}
		s.notifier.NotifyTask(ws.TaskEvent{
			JobID:     jobID,
			TaskID:    rt.ID,
			Name:      rt.Name,
			Status:    status,
			UpdatedAt: now,
		})
	}

	// Notify re-canceled dependents that were not already in the revived set.
	for _, ct := range canceledTasks {
		if revivedIDs[ct.ID] {
			continue
		}
		s.notifier.NotifyTask(ws.TaskEvent{
			JobID:     jobID,
			TaskID:    ct.ID,
			Name:      ct.Name,
			Status:    string(ct.Status),
			UpdatedAt: now,
		})
	}

	// Notify the job's current status (after checkJobCompletion may have finalized it).
	if job, err := s.store.GetJob(ctx, jobID); err == nil {
		s.notifier.NotifyJob(ws.JobEvent{
			JobID:     jobID,
			Name:      job.Name,
			Owner:     job.Owner,
			QueueID:   job.QueueID,
			Status:    string(job.Status),
			UpdatedAt: now,
		})
	} else {
		s.logger.WarnContext(ctx, "scheduler: retry: get job for notify failed",
			slog.String("job_id", jobID), slog.Any("error", err))
	}

	// Wake parked lease waiters: newly ready tasks may fit waiting workers.
	s.notifyQueueForJob(ctx, jobID)

	s.logger.InfoContext(ctx, "scheduler: retry complete",
		slog.String("job_id", jobID), slog.Int("tasks_revived", len(revived)))
	return len(revived), nil
}
