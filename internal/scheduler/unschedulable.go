// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/ws"
)

// sweepUnschedulable flags each ready task that has waited longer than
// UnschedulableGrace and that no online worker can satisfy, and clears the
// flag on tasks that have become schedulable (e.g. a matching worker joined,
// or the job/step's requirements no longer apply). A no-op when
// UnschedulableGrace <= 0 — that is the "off" setting, not coerced to a
// default (see [Config.UnschedulableGrace]).
func (s *Scheduler) sweepUnschedulable(ctx context.Context) {
	if s.cfg.UnschedulableGrace <= 0 {
		return
	}
	cutoff := time.Now().UTC().Add(-s.cfg.UnschedulableGrace)

	// store.MaxLimit, not 0: ListReadyTasks/[fake and sqlite] both treat a
	// limit of 0 as "return zero rows", not "unlimited" — confirmed against
	// sqlListReadyTasks's `LIMIT ?` (SQLite LIMIT 0 = no rows) and the fake
	// store's `readyTasks[:limit]` slicing. store.MaxLimit mirrors the
	// "fetch effectively all" convention used elsewhere in this package
	// (e.g. checkStepCompletion, workers.go's active-task lookup).
	ready, err := s.store.ListReadyTasks(ctx, s.cfg.FarmID, store.MaxLimit)
	if err != nil {
		s.logger.WarnContext(ctx, "scheduler: unschedulable sweep: list ready tasks failed", slog.Any("error", err))
		return
	}
	if len(ready) == 0 {
		return
	}

	workers, err := s.onlineWorkers(ctx)
	if err != nil {
		s.logger.WarnContext(ctx, "scheduler: unschedulable sweep: list workers failed", slog.Any("error", err))
		return
	}

	for _, task := range ready {
		// The grace window only delays the *first* flag on a freshly-ready
		// task (UnschedulableReason == "") so newly-submitted work isn't
		// flagged before a worker has had a chance to pick it up. A task
		// already flagged is re-evaluated on every tick regardless of age so
		// it clears the moment a matching worker appears — gating the clear
		// on the same cutoff would re-arm the grace window every time
		// SetTaskUnschedulableReason bumps UpdatedAt, needlessly prolonging a
		// stale "unschedulable" annotation well past the point it stopped
		// being true.
		if task.UnschedulableReason == "" && task.UpdatedAt.After(cutoff) {
			continue // still within grace, not yet flagged
		}
		s.reconcileTaskSchedulability(ctx, task, workers)
	}
}

// reconcileTaskSchedulability computes task's current schedulability against
// workers and, only when the result differs from the task's stored
// UnschedulableReason, persists the change and notifies WebSocket subscribers.
func (s *Scheduler) reconcileTaskSchedulability(ctx context.Context, task store.Task, workers []store.Worker) {
	reason := s.evaluateSchedulability(ctx, task, workers)
	if reason == task.UnschedulableReason {
		return // no change — avoid notification churn
	}
	if err := s.store.SetTaskUnschedulableReason(ctx, task.ID, reason); err != nil {
		s.logger.WarnContext(
			ctx, "scheduler: unschedulable sweep: set reason failed",
			slog.String("task_id", task.ID),
			slog.Any("error", err),
		)
		return
	}
	// TODO(P2-T4): carry unschedulable reason on the event once
	// ws.TaskEvent grows an UnschedulableReason field; for now this just
	// prompts subscribed clients to refetch the task.
	s.notifier.NotifyTask(ws.TaskEvent{
		JobID:     task.JobID,
		TaskID:    task.ID,
		Name:      task.Name,
		Status:    string(task.Status),
		WorkerID:  task.AssignedWorkerID,
		UpdatedAt: time.Now().UTC(),
	})
}

// evaluateSchedulability returns "" if at least one online worker in workers
// is eligible to run task, otherwise a human-readable reason suitable for
// [store.Task.UnschedulableReason]. Store lookup failures resolve to ""
// (schedulable) rather than flagging the task on what is likely a transient
// error.
func (s *Scheduler) evaluateSchedulability(ctx context.Context, task store.Task, workers []store.Worker) string {
	if len(workers) == 0 {
		return "no online workers"
	}

	job, err := s.store.GetJob(ctx, task.JobID)
	if err != nil {
		return ""
	}
	step, err := s.store.GetStep(ctx, task.StepID)
	if err != nil {
		return ""
	}
	pools, activeCounts, err := s.buildUsageContext(ctx, step)
	if err != nil {
		return ""
	}

	var lastReason string
	for _, w := range workers {
		reason, ok := WorkerEligibleWithReason(w, job, step, pools, activeCounts)
		if ok {
			return "" // at least one eligible worker — schedulable
		}
		lastReason = reason
	}
	return "no eligible online worker: " + lastReason
}

// onlineWorkers returns every online worker the scheduler's farm can dispatch
// to, including unaffiliated workers (empty FarmID) — the same options used
// by the assignment path (see [store.ListWorkersOptions.IncludeUnaffiliated])
// and the "fetch effectively all rows" pagination convention used elsewhere
// in this package (e.g. checkStepCompletion, workers.go active-task lookup).
func (s *Scheduler) onlineWorkers(ctx context.Context) ([]store.Worker, error) {
	page, err := s.store.ListWorkers(ctx, store.ListWorkersOptions{
		Status:              store.WorkerStatusOnline,
		FarmID:              s.cfg.FarmID,
		IncludeUnaffiliated: true,
		Pagination:          store.Pagination{Limit: store.MaxLimit, Offset: 0},
	})
	if err != nil {
		return nil, err
	}
	return page.Items, nil
}
