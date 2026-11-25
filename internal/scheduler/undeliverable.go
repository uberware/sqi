// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// undeliverableReasonPrefix begins the failure reason of an attempt whose
// assignment the server could not build.
const undeliverableReasonPrefix = "server could not build the task's assignment: "

// errAssignStoreRead marks a [buildAssignPayload] failure caused by a store
// read, not by the job or the farm's configuration: retrying it later can
// succeed with nothing changed, so it never counts against the task.
var errAssignStoreRead = errors.New("store read failed")

// assignFailureUncounted reports whether a failure to build an assignment says
// nothing about the task: a store read failed, or the server is stopping. Any
// other failure comes from the job or the farm's configuration (the template
// no longer parses, the step is gone, a storage location is missing or has no
// root for the worker) and counts as a failed attempt.
func assignFailureUncounted(err error) bool {
	return errors.Is(err, errAssignStoreRead) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

// handleUndeliverableLease settles an attempt [Scheduler.tryLeaseTask] leased
// but could not deliver, because building its assignment failed after the
// lease committed. Left alone, the task would stay assigned, holding the
// worker's cores and its usage claims, until the stale-assignment reaper
// returned it to ready, and the next lease would fail on it again, forever.
//
// A failure [assignFailureUncounted] reports hands the task back to ready with
// no retry used, and is returned so the lease stops as on any store error. Any
// other failure is applied as the failed report a worker would send, through
// [Scheduler.handleTaskFailed], so the task's retry policy decides what
// follows: a retry after the backoff, or a failed task (and its step and job)
// whose reason names the error. It returns nil then, and the lease goes on to
// the next task. Only a fenced write is returned from that path; any other
// error applying it is logged, and the stale-assignment reaper is the backstop.
func (s *Scheduler) handleUndeliverableLease(
	ctx context.Context, attempt store.TaskAttempt, task store.Task, buildErr error,
) error {
	s.logger.WarnContext(ctx, "scheduler: could not build a leased task's assignment",
		slog.String("task_id", task.ID), slog.String("job_id", task.JobID),
		slog.String("worker_id", attempt.WorkerID), slog.Any("error", buildErr))

	if assignFailureUncounted(buildErr) {
		if _, err := s.reclaimAttempt(ctx, attempt.ID, task.ID, time.Now().UTC()); err != nil {
			return errors.Join(buildErr, err)
		}
		return buildErr
	}

	msg := protocol.TaskStatusMsg{
		Version:   protocol.ProtocolVersion,
		Type:      protocol.TypeTaskStatus,
		TaskID:    task.ID,
		AttemptID: attempt.ID,
		JobID:     task.JobID,
		Status:    "failed",
		Message:   undeliverableReasonPrefix + strings.TrimPrefix(buildErr.Error(), "build assign payload: "),
	}
	err := s.handleTaskFailed(ctx, attempt, msg, time.Now().UTC())
	if errors.Is(err, store.ErrFenced) {
		return err
	}
	if err != nil {
		s.logger.WarnContext(ctx, "scheduler: could not fail an undeliverable attempt; the stale-assignment reaper will return it",
			slog.String("task_id", task.ID), slog.String("attempt_id", attempt.ID), slog.Any("error", err))
	}
	return nil
}
