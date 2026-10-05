// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// failure.go implements the auto-retry fork for worker-reported task failures.
//
// A "failed" TaskStatusMsg does not route straight to handleTaskTerminal.
// handleTaskFailed resolves the effective retry policy (job -> queue
// -> farm -> server default, via ResolveRetryPolicy/RetryDefaults) and records
// the genuine failure, then picks one of the first three outcomes below; a
// shutdown report takes the fourth and records no failure:
//
//   - RETRY: the task's failed_attempts is still below its policy ceiling and
//     the job has not hit its failure limit — the attempt is closed as failed
//     (usage-pool claims are released inside RecordTaskFailure's transaction),
//     and the task is re-queued to [store.TaskStatusReady] with a backoff
//     RetryAfter. It does NOT go through handleTaskTerminal /
//     checkStepCompletion, since the step is not actually done — it must stay
//     eligible for re-lease.
//   - PARKED: the job's cumulative failure count reached its FailureLimit —
//     the job is parked (paused) first, then the tripping task still goes
//     terminal-failed so the step/job completion cascade runs and the job
//     settles instead of hanging half-running.
//   - EXHAUSTED: failed_attempts reached MaxAttempts with no failure limit in
//     play — the task goes terminal-failed via the existing handleTaskTerminal
//     path.
//   - SHUTDOWN: a report whose Message is protocol.MessageWorkerShutdown is a
//     reclaim, not a failure (handleTaskShutdown).

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
	if m.Message == protocol.MessageWorkerShutdown {
		return s.handleTaskShutdown(ctx, attempt, m)
	}
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
	policy := ResolveRetryPolicy(job, queue, farm, s.RetryDefaults())

	taskFailed, jobFailed, firstClose, err := s.store.RecordTaskFailure(ctx, attempt.ID, m.TaskID, m.ExitCode, m.SessionID, m.Message, at)
	if err != nil {
		return err
	}
	// RecordTaskFailure closes the attempt as failed whether or not this is
	// the message that first closed it, so it can never receive another log
	// chunk from here on — evict unconditionally, same as handleTaskTerminal.
	s.attemptCache.evict(attempt.ID)

	if !firstClose {
		// The attempt was already terminal when this message arrived. That is
		// either a crash-recovery redelivery — the counters committed but the
		// requeue/park action below was lost — or a STALE report: the attempt
		// was closed by a user cancel (canceled) or superseded by a newer
		// attempt (worker reclaim + re-lease). Acting on a stale report would
		// resurrect a canceled/succeeded task or yank a re-leased assignment
		// from its worker, so proceed only when the attempt was genuinely
		// closed as failed AND is still the task's latest.
		proceed, err := s.failureReportStillCurrent(ctx, m.TaskID, attempt.ID)
		if err != nil {
			return err
		}
		if !proceed {
			s.logger.InfoContext(ctx, "scheduler: stale failure report — discarding",
				slog.String("task_id", m.TaskID),
				slog.String("attempt_id", attempt.ID))
			return nil
		}
	}

	parked := policy.FailureLimit > 0 && jobFailed >= policy.FailureLimit

	if !parked && taskFailed < policy.MaxAttempts {
		return s.retryTaskAfterFailure(ctx, task, job, m, policy, taskFailed, at)
	}

	if parked {
		if err := s.parkJobAtFailureLimit(ctx, task.JobID, job.QueueID, policy.FailureLimit, at); err != nil {
			return err
		}
	}

	// EXHAUSTED or PARKED: finish as a terminal failure (cascades to step/job).
	return s.handleTaskTerminal(ctx, attempt, m, store.TaskStatusFailed, store.AttemptStatusFailed, at)
}

// handleTaskShutdown applies a "failed"/"worker_shutdown" report as a reclaim:
// the task did nothing wrong, so it goes back to ready
// with no retry consumed and no job failure counted, exactly as the offline
// reclaim behind a deregister treats it. Whichever of the two the server
// applies first does the work; the other finds the attempt closed and does
// nothing, so both orders end in the same state.
func (s *Scheduler) handleTaskShutdown(ctx context.Context, attempt store.TaskAttempt, m protocol.TaskStatusMsg) error {
	reclaimed, err := s.store.ReclaimTaskAttempt(ctx, attempt.ID, m.TaskID, time.Now().UTC())
	if err != nil {
		return err
	}
	s.attemptCache.evict(attempt.ID)
	if !reclaimed {
		s.logger.InfoContext(ctx, "scheduler: shutdown report for an attempt already closed — discarding",
			slog.String("task_id", m.TaskID), slog.String("attempt_id", attempt.ID))
		return nil
	}
	task, err := s.store.GetTask(ctx, m.TaskID)
	if err != nil {
		return err
	}
	s.logger.InfoContext(ctx, "scheduler: task reclaimed from a shutting-down worker",
		slog.String("task_id", m.TaskID), slog.String("worker_id", attempt.WorkerID))
	s.notifyTaskReclaimed(ctx, task, time.Now().UTC())
	return nil
}

// retryTaskAfterFailure re-queues the task with backoff. Split out of
// handleTaskFailed to keep cyclomatic complexity in check.
func (s *Scheduler) retryTaskAfterFailure(
	ctx context.Context,
	task store.Task,
	job store.Job,
	m protocol.TaskStatusMsg,
	policy RetryPolicy,
	taskFailed int,
	at time.Time,
) error {
	// RETRY: the attempt was already closed as failed, and its usage claims
	// released, inside RecordTaskFailure's transaction; here we only re-queue
	// with backoff.
	retryAfter := at.Add(policy.RetryDelay)
	requeued, err := s.store.RequeueTaskForRetry(ctx, m.TaskID, m.AttemptID, retryAfter, at)
	if err != nil {
		return err
	}
	if !requeued {
		// The task left assigned/running, or a newer lease superseded this
		// attempt (canceled, reclaimed and re-leased, or a redelivery whose
		// first delivery already requeued it) — the store guard declined the
		// transition, so skip the retry side effects too.
		s.logger.InfoContext(ctx, "scheduler: retry requeue skipped — task no longer in flight or attempt superseded",
			slog.String("task_id", m.TaskID),
			slog.String("attempt_id", m.AttemptID))
		return nil
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

// failureReportStillCurrent reports whether a "failed" message whose attempt
// was already terminal may still drive retry/park actions: true only when the
// attempt is closed as failed (RecordTaskFailure or the offline-worker sweep
// closed it — not a cancel) and is still the task's latest attempt (no newer
// lease has superseded it). This is the crash-recovery case; everything else
// is a stale report that must be discarded.
func (s *Scheduler) failureReportStillCurrent(ctx context.Context, taskID, attemptID string) (bool, error) {
	cur, err := s.store.GetTaskAttempt(ctx, attemptID)
	if err != nil {
		return false, err
	}
	if cur.Status != store.AttemptStatusFailed {
		return false, nil
	}
	latest, err := s.store.LatestTaskAttempt(ctx, taskID)
	if err != nil {
		return false, err
	}
	return latest.ID == attemptID, nil
}

// scheduleRetryWake wakes the task's queue once its backoff delay elapses so a
// parked worker re-leases promptly. The heartbeat sweep is the authoritative
// (restart-safe) wake; this AfterFunc is a latency optimization. Timers are
// tracked in retryWakeTimers (each removes itself on fire) so shutdown can
// stop the stragglers instead of leaving them alive past [Run].
func (s *Scheduler) scheduleRetryWake(queueID string, delay time.Duration) {
	if delay <= 0 {
		s.WakeQueue(queueID)
		return
	}
	if s.ctx.Err() != nil {
		return // shutting down — the wake would land in a void
	}

	s.retryWakeMu.Lock()
	defer s.retryWakeMu.Unlock()
	var t *time.Timer
	t = time.AfterFunc(delay, func() {
		s.retryWakeMu.Lock()
		delete(s.retryWakeTimers, t)
		s.retryWakeMu.Unlock()
		s.WakeQueue(queueID)
	})
	s.retryWakeTimers[t] = struct{}{}
}

// stopRetryWakeTimers stops and drops every pending backoff-wake timer.
// Called once during [Run] shutdown, after the worker goroutines drain.
func (s *Scheduler) stopRetryWakeTimers() {
	s.retryWakeMu.Lock()
	defer s.retryWakeMu.Unlock()
	for t := range s.retryWakeTimers {
		t.Stop()
	}
	clear(s.retryWakeTimers)
}
