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
// UnschedulableGrace <= 0, which is the "off" setting and is not replaced by a
// default (see [Config.UnschedulableGrace]).
func (s *Scheduler) sweepUnschedulable(ctx context.Context) {
	if s.cfg.UnschedulableGrace <= 0 {
		return
	}
	cutoff := time.Now().UTC().Add(-s.cfg.UnschedulableGrace)

	// store.MaxLimit, not 0: ListReadyTasks treats a limit of 0 as "return zero
	// rows", not "unlimited". store.MaxLimit is this package's "fetch
	// effectively all" convention (e.g. checkStepCompletion).
	ready, err := s.store.ListReadyTasks(ctx, s.cfg.FarmID, time.Now().UTC(), store.MaxLimit)
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
		if s.deposed() {
			return
		}
		// The grace window delays only the first flag on a freshly-ready task
		// (UnschedulableReason == ""), so newly-submitted work is not flagged
		// before a worker has had a chance to pick it up. A task already
		// flagged is re-evaluated on every tick regardless of age, so the flag
		// clears as soon as a matching worker appears. Gating the clear on the
		// same cutoff would re-arm the grace window each time
		// SetTaskUnschedulableReason bumps UpdatedAt.
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
	written, err := s.store.SetTaskUnschedulableReason(ctx, task.ID, reason)
	if s.stopIfFenced(ctx, err) {
		return
	}
	if err != nil {
		s.logger.WarnContext(
			ctx, "scheduler: unschedulable sweep: set reason failed",
			slog.String("task_id", task.ID),
			slog.Any("error", err),
		)
		return
	}
	if !written {
		return // the task left ready since the sweep read it: nothing to report
	}
	s.notifier.NotifyTask(ws.TaskEvent{
		JobID:               task.JobID,
		TaskID:              task.ID,
		Name:                task.Name,
		Status:              string(task.Status),
		WorkerID:            task.AssignedWorkerID,
		UnschedulableReason: reason,
		UpdatedAt:           time.Now().UTC(),
	})
}

// evaluateSchedulability returns "" if at least one online worker in workers
// is eligible to run task, otherwise a human-readable reason suitable for
// [store.Task.UnschedulableReason]. A store lookup failure returns ""
// (schedulable), so a likely transient error does not flag the task.
//
// Where no worker is eligible, the reason is that the task waits for a
// capacity group when one that acts could bring a machine that runs it
// ([Scheduler.capacityWaitReason]). That view is in memory, so a farm with no
// such group makes the same store calls as before.
func (s *Scheduler) evaluateSchedulability(ctx context.Context, task store.Task, workers []store.Worker) string {
	if len(workers) == 0 && !s.hasWaitable() {
		return "no online workers"
	}

	// A read that fails reports the task schedulable, which leaves it alone,
	// except with no worker online: that reason needs no read, and reporting
	// it keeps a passing store error from clearing it.
	unread := ""
	if len(workers) == 0 {
		unread = "no online workers"
	}
	job, err := s.store.GetJob(ctx, task.JobID)
	if err != nil {
		return unread
	}
	step, err := s.store.GetStep(ctx, task.StepID)
	if err != nil {
		return unread
	}
	pools, activeCounts, err := s.buildUsageContext(ctx, step)
	if err != nil {
		return unread
	}

	requiredCores := 0
	if task.RequiredCores != nil {
		requiredCores = *task.RequiredCores
	}
	wait := func(reason string) string {
		if r := s.capacityWaitReason(job, step, requiredCores, pools); r != "" {
			return r
		}
		return reason
	}
	if len(workers) == 0 {
		return wait("no online workers")
	}

	var lastReason string
	for _, w := range workers {
		// Both tests must run: the lease gate refuses to dispatch an EXPR job
		// to a worker whose advertised EXPR caps are below this server's, so
		// the sweep must apply the same test or the task would sit ready while
		// reported schedulable.
		//
		// Ordinary eligibility is tested first. A worker that is ineligible for
		// a plain reason (wrong queue, wrong compute location, a tag it does
		// not carry) is ineligible for that reason, and that is the reason an
		// operator can act on. Only a worker that would otherwise be eligible
		// is reported as blocked by EXPR limits. exprcaps.go's reason names
		// both config keys and both numbers.
		reason, ok := WorkerEligibleWithReason(w, job, step, pools, activeCounts)
		if !ok {
			lastReason = reason
			continue
		}
		if compatReason := workerCompatBlock(s.workerExprShortfall(w), w, job); compatReason != "" {
			lastReason = compatReason
			continue
		}
		return "" // at least one eligible worker — schedulable
	}
	return wait("no eligible online worker: " + lastReason)
}

// onlineWorkers returns every online worker the scheduler's farm can dispatch
// to, including unaffiliated workers (empty FarmID); the assignment path uses
// the same options (see [store.ListWorkersOptions.IncludeUnaffiliated]).
// It reads page after page of [store.MaxLimit], so a farm of any size is read
// whole. The pages are separate reads, so a worker that changes status between
// them can move across a page boundary: one seen twice is kept once, and one
// skipped is seen on the next sweep.
func (s *Scheduler) onlineWorkers(ctx context.Context) ([]store.Worker, error) {
	var out []store.Worker
	seen := map[string]bool{}
	for offset := 0; ; offset += store.MaxLimit {
		page, err := s.store.ListWorkers(ctx, store.ListWorkersOptions{
			Status:              store.WorkerStatusOnline,
			FarmID:              s.cfg.FarmID,
			IncludeUnaffiliated: true,
			Pagination:          store.Pagination{Limit: store.MaxLimit, Offset: offset},
		})
		if err != nil {
			return nil, err
		}
		for _, w := range page.Items {
			if !seen[w.ID] {
				seen[w.ID] = true
				out = append(out, w)
			}
		}
		if len(page.Items) < store.MaxLimit {
			return out, nil
		}
	}
}
