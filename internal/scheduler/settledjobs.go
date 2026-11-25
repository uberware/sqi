// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
	"errors"
	"log/slog"
)

// finalizeSettledJobs finalizes, on every sweep tick, each live job whose
// steps are all terminal. A job is left that way when its FinalizeJob never
// ran after the write that finished its last step: a server stop, or a store
// error while the server stayed up, between a cancel cascade and the
// FinalizeJob after it (a REST task cancel or retry logs that error and moves
// on, and nothing redelivers it). No task report will arrive for such a job,
// so without this it stays non-terminal for good, and so do the jobs blocked
// on it.
//
// Each job goes through [Scheduler.checkJobCompletion], the path a task
// report takes: FinalizeJob decides the outcome inside its own write (a job
// whose step reopened since the list was read is left alone), the job event
// is sent only by the call that wrote the status, and the job's dependents are
// reconciled. On a healthy farm the one query returns nothing and nothing is
// written. A job that fails is logged and skipped, and the next tick retries
// it; a fenced write stops the sweep.
func (s *Scheduler) finalizeSettledJobs(ctx context.Context) {
	ids, err := s.store.ListJobIDsWithAllStepsTerminal(ctx)
	if err != nil {
		s.warnUnlessStopped(ctx, err, "scheduler: list jobs with all steps terminal failed")
		return
	}
	for _, jobID := range ids {
		if ctx.Err() != nil || s.deposed() {
			return
		}
		if err := s.checkJobCompletion(ctx, jobID); err != nil {
			if s.stopIfFenced(ctx, err) {
				return
			}
			if !errors.Is(err, context.Canceled) {
				s.logger.WarnContext(ctx, "scheduler: finalize a job whose steps are all terminal failed",
					slog.String("job_id", jobID), slog.Any("error", err))
			}
			continue
		}
		s.logger.InfoContext(ctx, "scheduler: finalized a live job whose steps were all terminal",
			slog.String("job_id", jobID))
	}
}
