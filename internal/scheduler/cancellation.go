// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Cancellation propagation.
//
// CancelJob and CancelTask are the server-side entry points for explicit
// cancellation (triggered by the REST API layer). Each is one store operation
// followed by the worker signals:
//
//  1. [store.TaskStore.CancelJobExecution] / [store.TaskStore.CancelTaskExecution]
//     cancels the non-terminal tasks, closes their running attempts and
//     releases those attempts' usage claims in ONE transaction (invariant I3),
//     and returns the tasks that were assigned or running together with the
//     worker each held.
//  2. A task.cancel.<taskID> NATS signal is published to each of those workers
//     ([bus.Client.PublishTaskCancel]) so the worker can interrupt the running
//     process without waiting for the next heartbeat timeout.
//
//  3. CancelTask then drives step and job completion
//     ([Scheduler.checkStepCompletion]), as a terminal worker report does,
//     because canceling a job's last open task finishes its step. CancelJob
//     needs no such call: the store finalizes the job's steps, and cancels the
//     job row itself, inside the same transaction that cancels its tasks.
//
// Because the claims are released in the same transaction that closes the
// attempts, there is no window in which a canceled task still holds a usage
// slot, and nothing that has to be cleaned up afterwards.
//
// NATS publish failures are non-fatal: the scheduler logs a warning and
// continues.  Workers that miss the cancel signal will eventually be reclaimed
// by the heartbeat sweep once they go past the WorkerTimeout threshold.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// cancelPayload is the JSON body published to task.cancel.<taskID> when the
// server signals an assigned worker to stop executing a task.
type cancelPayload struct {
	TaskID     string    `json:"task_id"`
	JobID      string    `json:"job_id"`
	CanceledAt time.Time `json:"canceled_at"`
}

// CancelJob cancels every non-terminal task of the given job, closes their
// running attempts and releases their usage claims in one store transaction
// (invariant I3), then publishes a task.cancel.<taskID> signal to each worker
// that was actively executing one of the tasks.
//
// The durable "canceled by user" reason is stamped in the same transaction,
// only on tasks with no reason yet, so a more specific cause already recorded
// (e.g. a cascade-cancel's "canceled: upstream step failed") is never
// clobbered.
//
// The same transaction also finalizes the job's steps and moves the job itself
// to canceled, unless it is already completed, failed or canceled, so a job
// cancel is one write. A caller's later [store.JobStore.CancelJobStatus] (the
// REST handler makes one) is an idempotent confirmation that reports a job that
// had already completed or failed.
//
// The method is idempotent: if all tasks are already in terminal states the
// store operation changes nothing and no NATS messages are published.
func (s *Scheduler) CancelJob(ctx context.Context, jobID string) error {
	now := time.Now().UTC()

	activeTasks, err := s.store.CancelJobExecution(ctx, jobID, store.FailureReasonCanceledByUser, now)
	if err != nil {
		return fmt.Errorf("scheduler: cancel job %s: %w", jobID, err)
	}

	s.publishCancelSignals(ctx, activeTasks, now)

	s.logger.InfoContext(
		ctx, "scheduler: job cancellation complete",
		slog.String("job_id", jobID),
		slog.Int("active_tasks_canceled", len(activeTasks)),
	)
	return nil
}

// CancelTask cancels a single task, closes its running attempt and releases
// its usage claims in one store transaction (invariant I3), then publishes a
// task.cancel.<taskID> signal to the worker that held the task at that moment.
//
// If the task is already in a terminal state (succeeded, failed, canceled),
// CancelTask returns nil without modifying any state. That holds whether the
// terminal state was visible up front or the task reached it mid-cancel: the
// store decides inside its transaction and reports a terminal task as "not
// canceled", so losing that race is reported the same way as never having had
// it. Any other store failure propagates. It then drives step and job
// completion, as a terminal report does.
func (s *Scheduler) CancelTask(ctx context.Context, taskID string) error {
	now := time.Now().UTC()

	prior, canceled, err := s.store.CancelTaskExecution(ctx, taskID, store.FailureReasonCanceledByUser, now)
	if err != nil {
		return fmt.Errorf("scheduler: cancel task %s: %w", taskID, err)
	}
	if !canceled {
		s.logger.DebugContext(
			ctx, "scheduler: cancel task — already terminal",
			slog.String("task_id", taskID),
			slog.String("status", string(prior.Status)),
		)
		return nil
	}

	// Publish a cancel signal to the worker that held the task (if any).
	s.publishCancelSignals(ctx, []store.Task{prior}, now)

	// Finish the step and job as a terminal worker report would: otherwise a
	// cancel of a job's last open task leaves both open until the next start.
	// The cancel has committed, so a failure here is logged, not returned; the
	// start-up reconcile (reconcileStuckSteps) is the backstop.
	if err := s.checkStepCompletion(ctx, prior.StepID, prior.JobID); err != nil {
		s.logger.WarnContext(
			ctx, "scheduler: cancel task: step completion failed",
			slog.String("task_id", taskID),
			slog.String("step_id", prior.StepID),
			slog.Any("error", err),
		)
	}

	s.logger.InfoContext(
		ctx, "scheduler: task canceled",
		slog.String("task_id", taskID),
		slog.String("job_id", prior.JobID),
		slog.String("worker_id", prior.AssignedWorkerID),
	)
	return nil
}

// publishCancelSignals publishes a task.cancel.<taskID> NATS message for each
// task in activeTasks that has an assigned worker.  NATS publish failures are
// non-fatal: a warning is logged and iteration continues so all other workers
// still receive their signals.
func (s *Scheduler) publishCancelSignals(ctx context.Context, activeTasks []store.Task, canceledAt time.Time) {
	for _, t := range activeTasks {
		if t.AssignedWorkerID == "" {
			continue
		}
		payload, err := json.Marshal(cancelPayload{
			TaskID:     t.ID,
			JobID:      t.JobID,
			CanceledAt: canceledAt,
		})
		if err != nil {
			// json.Marshal of a struct with no interface{} fields never fails in
			// practice; log and skip rather than aborting remaining signals.
			s.logger.WarnContext(
				ctx, "scheduler: marshal cancel payload failed",
				slog.String("task_id", t.ID),
				slog.Any("error", err),
			)
			continue
		}

		if err = s.bus.PublishTaskCancel(ctx, t.ID, payload); err != nil {
			s.logger.WarnContext(
				ctx, "scheduler: publish cancel signal failed — worker will time out via heartbeat sweep",
				slog.String("task_id", t.ID),
				slog.String("worker_id", t.AssignedWorkerID),
				slog.Any("error", err),
			)
			continue
		}

		s.logger.InfoContext(
			ctx, "scheduler: cancel signal published to worker",
			slog.String("task_id", t.ID),
			slog.String("worker_id", t.AssignedWorkerID),
		)
	}
}
