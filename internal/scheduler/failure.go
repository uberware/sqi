// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// failure.go implements the auto-retry fork for worker-reported task failures.
//
// A "failed" TaskStatusMsg no longer routes straight to handleTaskTerminal.
// Instead handleTaskFailed resolves the effective retry policy (job -> queue
// -> farm -> server default, via resolveRetryPolicy/retryDefaults) and records
// the genuine failure, then picks one of three outcomes:
//
//   - RETRY: the task's failed_attempts is still below its policy ceiling and
//     the job has not hit its failure limit — the attempt is closed as failed,
//     usage-pool claims are released, and the task is re-queued to
//     [store.TaskStatusReady] with a backoff RetryAfter. It does NOT go
//     through handleTaskTerminal / checkStepCompletion, since the step is not
//     actually done — it must stay eligible for re-lease.
//   - PARKED: the job's cumulative failure count reached its FailureLimit —
//     the job is parked (paused) first, then the tripping task still goes
//     terminal-failed so the step/job completion cascade runs and the job
//     settles instead of hanging half-running.
//   - EXHAUSTED: failed_attempts reached MaxAttempts with no failure limit in
//     play — the task goes terminal-failed via the existing handleTaskTerminal
//     path.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/ws"
)

// handleTaskFailed applies retry policy to a worker-reported task failure.
// It increments the genuine-failure counters, resolves the effective policy,
// and either re-queues the task (backoff) or lets it go terminal-failed —
// parking the job first if its failure limit is reached.
func (s *Scheduler) handleTaskFailed(ctx context.Context, attempt store.TaskAttempt, m protocol.TaskStatusMsg, at time.Time) error {
	task, err := s.store.GetTask(ctx, m.TaskID)
	if err != nil {
		return err
	}
	job, err := s.store.GetJob(ctx, task.JobID)
	if err != nil {
		return err
	}
	queue, err := s.store.GetQueue(ctx, job.QueueID)
	if err != nil {
		return err
	}
	farm, err := s.store.GetFarm(ctx, job.FarmID)
	if err != nil {
		return err
	}
	policy := resolveRetryPolicy(job, queue, farm, s.retryDefaults())

	taskFailed, jobFailed, err := s.store.RecordTaskFailure(ctx, m.TaskID, at)
	if err != nil {
		return err
	}

	parked := policy.FailureLimit > 0 && jobFailed >= policy.FailureLimit

	if !parked && taskFailed < policy.MaxAttempts {
		return s.retryTaskAfterFailure(ctx, task, job, attempt, m, policy, taskFailed, at)
	}

	if parked {
		if err := s.parkJobAtFailureLimit(ctx, task.JobID, job.QueueID, policy.FailureLimit, at); err != nil {
			return err
		}
	}

	// EXHAUSTED or PARKED: finish as a terminal failure (cascades to step/job).
	return s.handleTaskTerminal(ctx, attempt, m, store.TaskStatusFailed, store.AttemptStatusFailed, at)
}

// retryTaskAfterFailure closes the attempt as failed and re-queues the task
// with backoff. Split out of handleTaskFailed to keep cyclomatic complexity in
// check.
func (s *Scheduler) retryTaskAfterFailure(
	ctx context.Context,
	task store.Task,
	job store.Job,
	attempt store.TaskAttempt,
	m protocol.TaskStatusMsg,
	policy RetryPolicy,
	taskFailed int,
	at time.Time,
) error {
	// RETRY: close the attempt, release usage, re-queue with backoff.
	if err := s.closeAttemptFailed(ctx, attempt, m, at); err != nil {
		return err
	}
	retryAfter := at.Add(policy.RetryDelay)
	if err := s.store.RequeueTaskForRetry(ctx, m.TaskID, retryAfter, at); err != nil {
		return err
	}
	s.metrics.TaskRetriesTotal.WithLabelValues(job.QueueID).Inc()
	s.logger.InfoContext(ctx, "scheduler: task auto-retry scheduled",
		slog.String("task_id", m.TaskID),
		slog.Int("failed_attempts", taskFailed),
		slog.Int("max_attempts", policy.MaxAttempts),
		slog.Duration("retry_delay", policy.RetryDelay))
	s.notifier.NotifyTask(ws.TaskEvent{
		JobID: task.JobID, TaskID: task.ID, Name: task.Name,
		Status: string(store.TaskStatusReady), UpdatedAt: at,
	})
	s.scheduleRetryWake(job.QueueID, policy.RetryDelay)
	return nil
}

// parkJobAtFailureLimit transitions job to paused because its cumulative
// failure count reached its resolved FailureLimit.
func (s *Scheduler) parkJobAtFailureLimit(ctx context.Context, jobID, queueID string, failureLimit int, at time.Time) error {
	reason := fmt.Sprintf("failure limit reached (%d)", failureLimit)
	if err := s.store.ParkJob(ctx, jobID, reason, at); err != nil {
		return err
	}
	s.metrics.JobsAutoParkedTotal.WithLabelValues(queueID).Inc()
	s.logger.WarnContext(ctx, "scheduler: job auto-parked at failure limit",
		slog.String("job_id", jobID), slog.Int("failure_limit", failureLimit))
	s.notifier.NotifyJob(ws.JobEvent{
		JobID: jobID, Status: string(store.JobStatusPaused), UpdatedAt: at,
	})
	return nil
}

// closeAttemptFailed closes attempt as failed and releases its usage-pool
// claims, WITHOUT transitioning the task — used by the retry path where the
// task returns to ready rather than a terminal state.
func (s *Scheduler) closeAttemptFailed(ctx context.Context, attempt store.TaskAttempt, m protocol.TaskStatusMsg, at time.Time) error {
	updated := attempt
	updated.Status = store.AttemptStatusFailed
	updated.SessionID = m.SessionID
	updated.EndedAt = &at
	if m.ExitCode != nil {
		code := *m.ExitCode
		updated.ExitCode = &code
	}
	if _, err := s.store.UpdateTaskAttempt(ctx, updated); err != nil {
		return err
	}
	if err := s.ReleaseTaskUsage(ctx, attempt.ID); err != nil {
		s.logger.WarnContext(ctx, "scheduler: retry: release usage failed",
			slog.String("attempt_id", attempt.ID), slog.Any("error", err))
	}
	return nil
}

// scheduleRetryWake wakes the task's queue once its backoff delay elapses so a
// parked worker re-leases promptly. The heartbeat sweep is the authoritative
// (restart-safe) wake; this AfterFunc is a latency optimization.
func (s *Scheduler) scheduleRetryWake(queueID string, delay time.Duration) {
	if delay <= 0 {
		s.WakeQueue(queueID)
		return
	}
	time.AfterFunc(delay, func() { s.WakeQueue(queueID) })
}
