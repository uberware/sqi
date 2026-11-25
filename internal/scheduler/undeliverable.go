// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// undeliverableReasonPrefix begins the failure reason of an attempt whose
// assignment the server could not build.
const undeliverableReasonPrefix = "server could not build the task's assignment: "

// errAssignStoreRead marks a [buildAssignMsg] failure caused by a store read,
// not by the job, the farm's configuration or the worker: retrying it later can
// succeed with nothing changed.
var errAssignStoreRead = errors.New("store read failed")

// assignFailure says what [Scheduler.tryLeaseTask] does when building a task's
// assignment for a worker fails.
type assignFailure int

const (
	// assignStoreFailure: a store read failed. Lease nothing and stop the
	// walk, as on any store error; the task is not charged.
	assignStoreFailure assignFailure = iota
	// assignSkipWorker: the failure is this worker's (a storage location has no
	// root for its compute location). Leave the task ready for another worker;
	// it is not charged, so one misconfigured worker cannot fail a job others
	// could run.
	assignSkipWorker
	// assignFailsTask: the failure is the job's or the farm's (the stored
	// template does not parse, the step is missing, a storage location does not exist), so
	// every worker would hit it. Lease the task and charge the attempt through
	// its retry policy ([Scheduler.handleUndeliverableLease]).
	assignFailsTask
)

// classifyAssignFailure sorts a non-nil [buildAssignMsg] error. Its only store
// read, and so its only use of the context, is the storage-location list, whose
// failure wraps errAssignStoreRead.
func classifyAssignFailure(err error) assignFailure {
	var noRoot *openjd.NoRootError
	switch {
	case errors.Is(err, errAssignStoreRead):
		return assignStoreFailure
	case errors.As(err, &noRoot):
		return assignSkipWorker
	default:
		return assignFailsTask
	}
}

// handleUndeliverableLease settles an attempt [Scheduler.tryLeaseTask] leased
// although its assignment could not be built, because the failure is the job's
// (assignFailsTask). An unsettled attempt would hold the task assigned, with the
// worker's cores and its usage claims, until the stale-assignment reaper
// returned it to ready, and every later lease would fail the same way.
//
// The failure is applied as the failed report a worker would send, through
// [Scheduler.handleTaskFailed], so the task's retry policy decides what
// follows: a retry after the backoff, or a failed task (and its step and job)
// whose reason names the error. Only a fenced write is returned. Any other
// error is logged: if the attempt was not closed, the stale-assignment reaper
// returns the task to ready; if the task was already failed, its step is
// finalized by the next start's stuck-step pass (see "Known gaps" in
// docs/architecture.md).
func (s *Scheduler) handleUndeliverableLease(
	ctx context.Context, attempt store.TaskAttempt, task store.Task, buildErr error,
) error {
	s.logger.WarnContext(ctx, "scheduler: could not build a leased task's assignment — failing the attempt",
		slog.String("task_id", task.ID), slog.String("job_id", task.JobID),
		slog.String("worker_id", attempt.WorkerID), slog.Any("error", buildErr))

	msg := protocol.TaskStatusMsg{
		Version:   protocol.ProtocolVersion,
		Type:      protocol.TypeTaskStatus,
		TaskID:    task.ID,
		AttemptID: attempt.ID,
		JobID:     task.JobID,
		Status:    "failed",
		Message:   undeliverableReasonPrefix + buildErr.Error(),
	}
	err := s.handleTaskFailed(ctx, attempt, msg, time.Now().UTC())
	if errors.Is(err, store.ErrFenced) {
		return err
	}
	if err != nil {
		s.logger.WarnContext(ctx, "scheduler: applying an undeliverable attempt's failure did not complete",
			slog.String("task_id", task.ID), slog.String("attempt_id", attempt.ID), slog.Any("error", err))
	}
	return nil
}
