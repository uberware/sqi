// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Cross-job (whole-job) dependency reconciliation — the job-level analog of
// the within-job step dependency logic in taskstatus.go
// (openjd.ResolveDependencies / openjd.CancelDependents).
//
// A job created with one or more upstream job IDs starts in
// store.JobStatusBlocked and sits there until every upstream reaches
// store.JobStatusCompleted (released to pending) or any upstream fails, is
// canceled, or is deleted (the dependent is canceled — its dependency can
// never be satisfied). Cancellation cascades transitively: canceling a job
// re-reconciles ITS dependents, so a chain of blocked jobs unwinds in one
// pass. ReconcileDependents is called event-driven whenever an upstream job
// reaches a terminal status; sweepBlockedJobs is the periodic backstop that
// catches any dependent missed by a dropped event or an upstream removed by
// retention.

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/ws"
)

// ReconcileDependents re-evaluates every job blocked on upstreamJobID after that
// upstream changed state (completed, failed, canceled, or deleted). Each still-
// blocked dependent is released or canceled by reconcileBlockedJob.
func (s *Scheduler) ReconcileDependents(ctx context.Context, upstreamJobID string) error {
	dependents, err := s.store.ListDependents(ctx, upstreamJobID)
	if err != nil {
		return err
	}
	for _, id := range dependents {
		if err := s.reconcileBlockedJob(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// reconcileBlockedJob evaluates one job's cross-job dependencies and either
// releases it (all upstreams completed), cancels it (any upstream failed,
// canceled, or deleted), or leaves it blocked (some upstream still running).
func (s *Scheduler) reconcileBlockedJob(ctx context.Context, jobID string) error {
	job, err := s.store.GetJob(ctx, jobID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if job.Status != store.JobStatusBlocked {
		return nil // already released/canceled by an earlier pass
	}

	upstreamIDs, err := s.store.ListJobDependencyIDs(ctx, jobID)
	if err != nil {
		return err
	}

	allCompleted := true
	for _, uid := range upstreamIDs {
		up, gerr := s.store.GetJob(ctx, uid)
		if errors.Is(gerr, store.ErrNotFound) {
			return s.cancelAndCascade(ctx, jobID) // upstream deleted → unsatisfiable
		}
		if gerr != nil {
			return gerr
		}
		switch up.Status {
		case store.JobStatusCompleted:
			// satisfied
		case store.JobStatusFailed, store.JobStatusCanceled:
			return s.cancelAndCascade(ctx, jobID)
		default:
			allCompleted = false
		}
	}
	if !allCompleted {
		return nil
	}
	return s.releaseBlockedJob(ctx, job)
}

// releaseBlockedJob transitions a satisfied blocked job to pending and promotes
// its no-dependency steps' tasks to ready.
func (s *Scheduler) releaseBlockedJob(ctx context.Context, job store.Job) error {
	if err := s.store.UpdateJobStatus(ctx, job.ID, store.JobStatusPending); err != nil {
		return err
	}
	if _, err := openjd.ResolveDependencies(ctx, s.store, job.ID); err != nil {
		return err
	}
	s.WakeQueue(job.QueueID)
	s.notifier.NotifyJob(ws.JobEvent{
		JobID:     job.ID,
		Status:    string(store.JobStatusPending),
		UpdatedAt: time.Now().UTC(),
	})
	s.logger.InfoContext(ctx, "scheduler: released blocked job", slog.String("job_id", job.ID))
	return nil
}

// cancelAndCascade cancels a blocked job whose dependency can never be satisfied,
// then reconciles ITS dependents so the cancellation cascades transitively.
func (s *Scheduler) cancelAndCascade(ctx context.Context, jobID string) error {
	if err := s.cancelBlockedJob(ctx, jobID); err != nil {
		return err
	}
	return s.ReconcileDependents(ctx, jobID)
}

// cancelBlockedJob cancels every non-terminal step and pending task of a blocked
// job and marks the job canceled, stamping the upstream-failed reason. Because a
// blocked job's tasks are always pending (never assigned), no worker is involved.
func (s *Scheduler) cancelBlockedJob(ctx context.Context, jobID string) error {
	steps, err := s.store.ListSteps(ctx, jobID)
	if err != nil {
		return err
	}
	var canceledTasks []store.Task
	for _, step := range steps {
		if isTerminalStepStatus(step.Status) {
			continue
		}
		if err := s.store.UpdateStepStatus(ctx, step.ID, store.StepStatusCanceled); err != nil {
			return err
		}
		tasks, err := s.store.TransitionStepPendingTasks(ctx, step.ID, store.TaskStatusCanceled, store.FailureReasonUpstreamFailed)
		if err != nil {
			return err
		}
		canceledTasks = append(canceledTasks, tasks...)
	}
	if err := s.store.UpdateJobStatus(ctx, jobID, store.JobStatusCanceled); err != nil {
		return err
	}

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
	s.notifier.NotifyJob(ws.JobEvent{
		JobID:     jobID,
		Status:    string(store.JobStatusCanceled),
		UpdatedAt: now,
	})
	s.logger.InfoContext(ctx, "scheduler: canceled blocked job (upstream unsatisfiable)", slog.String("job_id", jobID))
	return nil
}

// sweepBlockedJobs re-evaluates every blocked job. It is the periodic backstop for
// ReconcileDependents, catching dependents missed by an event (e.g. upstream
// removed by the retention auto-purge, or an event dropped during downtime).
func (s *Scheduler) sweepBlockedJobs(ctx context.Context) error {
	blocked, err := s.store.ListBlockedJobs(ctx)
	if err != nil {
		return err
	}
	for _, job := range blocked {
		if err := s.reconcileBlockedJob(ctx, job.ID); err != nil {
			s.logger.ErrorContext(ctx, "scheduler: sweep reconcile blocked job failed",
				slog.String("job_id", job.ID), slog.Any("error", err))
		}
	}
	return nil
}
