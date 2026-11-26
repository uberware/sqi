// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/uberware/sqi/internal/openjd"
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

	// A task's answer depends only on its step (which fixes its job) and the
	// cores it needs, so each is evaluated once per sweep: a backlog of one
	// step costs one job, step and pool read, not one per task. A task whose
	// own parameters hold a loc:// reference is the exception, since its
	// storage locations are its own, and is evaluated by itself.
	verdicts := map[stepCores]string{}
	locs := newStorageLocations(s.store)
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
		key := stepCores{stepID: task.StepID, cores: requiredCores(task)}
		if paramsReferenceLocs(task.Parameters) {
			key.taskID = task.ID
		}
		reason, ok := verdicts[key]
		if !ok {
			reason = s.evaluateSchedulabilityWith(ctx, task, workers, locs)
			verdicts[key] = reason
		}
		s.applySchedulability(ctx, task, reason)
	}
}

// stepCores is what a task's schedulability depends on besides the workers:
// its step and the cores it needs, and the task itself when its parameters
// reference a storage location.
type stepCores struct {
	stepID string
	cores  int
	taskID string
}

// requiredCores is the cores task declares it needs, or 0 for none.
func requiredCores(task store.Task) int {
	if task.RequiredCores != nil {
		return *task.RequiredCores
	}
	return 0
}

// reconcileTaskSchedulability computes task's current schedulability against
// workers and applies it ([Scheduler.applySchedulability]).
func (s *Scheduler) reconcileTaskSchedulability(ctx context.Context, task store.Task, workers []store.Worker) {
	s.applySchedulability(ctx, task, s.evaluateSchedulability(ctx, task, workers))
}

// applySchedulability persists reason as task's UnschedulableReason and
// notifies WebSocket subscribers, only when it differs from the stored one.
func (s *Scheduler) applySchedulability(ctx context.Context, task store.Task, reason string) {
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
// ([Scheduler.capacityWaitReason]). That view is in memory, so it adds no
// store call.
//
// A task that references a storage location is also checked against each
// otherwise eligible worker's compute location, as the lease does
// ([storageRootBlock]), once per compute location.
func (s *Scheduler) evaluateSchedulability(ctx context.Context, task store.Task, workers []store.Worker) string {
	return s.evaluateSchedulabilityWith(ctx, task, workers, newStorageLocations(s.store))
}

// evaluateSchedulabilityWith is [Scheduler.evaluateSchedulability] reading
// storage locations through locs, which a sweep shares across its tasks.
func (s *Scheduler) evaluateSchedulabilityWith(
	ctx context.Context, task store.Task, workers []store.Worker, locs *storageLocations,
) string {
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

	cores := requiredCores(task)
	wait := func(reason string) string {
		if r := s.capacityWaitReason(job, step, cores, pools); r != "" {
			return r
		}
		return reason
	}
	if len(workers) == 0 {
		return wait("no online workers")
	}

	roots := newRootChecker(task, job, step, locs)
	var lastReason string
	for _, w := range workers {
		// Both tests must run: the lease gate withholds a job from a worker
		// that fails workerCompatBlock (EXPR caps below this server's, or a
		// worker-enforced extension it does not advertise), so the sweep must
		// apply the same test or the task would sit ready while reported
		// schedulable.
		//
		// Ordinary eligibility is tested first, so a worker ineligible for a
		// plain reason (wrong queue, wrong compute location, a missing tag)
		// reports that reason. Only a worker that would otherwise be eligible
		// is reported as blocked by the compatibility gate. exprcaps.go's
		// reason names both config keys and both numbers.
		reason, ok := WorkerEligibleWithReason(w, job, step, pools, activeCounts)
		if !ok {
			lastReason = reason
			continue
		}
		if compatReason := workerCompatBlock(s.workerExprShortfall(w), w, job); compatReason != "" {
			lastReason = compatReason
			continue
		}
		if rootReason := roots.block(ctx, w); rootReason != "" {
			lastReason = rootReason
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

// locScheme begins a storage-location reference in a template or parameter.
const locScheme = "loc://"

// paramsReferenceLocs reports whether any parameter value holds a loc://
// reference.
func paramsReferenceLocs(params map[string]string) bool {
	for _, v := range params {
		if strings.Contains(v, locScheme) {
			return true
		}
	}
	return false
}

// rootChecker answers, for one task, which compute locations lack a root for a
// storage location the task references, caching the answer per compute
// location. A task that references none is never checked.
type rootChecker struct {
	task  store.Task
	job   store.Job
	step  store.Step
	locs  *storageLocations
	check bool
	seen  map[string]string // compute location -> reason, "" when it has every root
}

func newRootChecker(task store.Task, job store.Job, step store.Step, locs *storageLocations) *rootChecker {
	return &rootChecker{
		task: task, job: job, step: step, locs: locs,
		check: strings.Contains(job.RawTemplate, locScheme) ||
			paramsReferenceLocs(job.Parameters) || paramsReferenceLocs(task.Parameters),
		seen: map[string]string{},
	}
}

// block returns why worker cannot be given the task for want of a storage
// root, or "".
func (r *rootChecker) block(ctx context.Context, worker store.Worker) string {
	if !r.check {
		return ""
	}
	reason, ok := r.seen[worker.ComputeLocation]
	if !ok {
		reason = storageRootBlock(ctx, r.task, worker, r.job, r.step, r.locs)
		r.seen[worker.ComputeLocation] = reason
	}
	return reason
}

// storageRootBlock returns the reason worker cannot be given task because a
// storage location the task references has no root for the worker's compute
// location, or "" when it can. It builds the assignment as the lease does and
// reports only that failure: any other build failure leases the task and fails
// it, so it is not a reason the task waits. The queue only sets the run-as
// identity, which cannot fail, so none is read.
func storageRootBlock(
	ctx context.Context, task store.Task, worker store.Worker, job store.Job, step store.Step, locs *storageLocations,
) string {
	_, err := buildAssignMsg(ctx, task, worker, job, step, store.Queue{}, locs)
	if noRoot, ok := errors.AsType[*openjd.NoRootError](err); ok {
		return noRoot.Error()
	}
	return ""
}
