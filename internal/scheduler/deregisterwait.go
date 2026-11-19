// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

const (
	// deregisterRecheckDelay is how long a deferred deregister waits before
	// it is delivered again and re-checked.
	deregisterRecheckDelay = 2 * time.Second

	// deregisterApplyWait bounds how long after its broker stamp a deregister
	// waits for the task-status consumer. Past it the deregister is applied
	// anyway, so a stalled task consumer cannot keep a departed worker's
	// attempts open. It must leave the deferral inside the worker consumer's
	// MaxDeliver budget (TestDeregisterWait_FitsWorkerConsumerDeliveries).
	deregisterApplyWait = 15 * time.Second
)

// deferDeregister reports whether msg, worker workerID's deregister, was
// handed back for a later re-check, in which case the handler returns without
// acknowledging it.
//
// A worker publishes its last task statuses to SQI_TASK and then deregisters
// on SQI_WORKER, and the two streams have separate consumers. Applying the
// deregister closes the worker's open attempts and returns their tasks to the
// queue, so a terminal status stored before the deregister but not yet applied
// would find its attempt already closed, and finished work would run again.
// The deregister is therefore applied only once the task tracker has applied
// SQI_TASK up to the deregister's broker stamp. Until then it is nak'd with
// [deregisterRecheckDelay], which leaves the worker consumer free to handle
// other workers' messages; the worker stays online meanwhile, which is safe
// because a deregistering worker has already stopped asking for work.
//
// The deferral is bounded by [deregisterApplyWait] from the stamp. A message
// with no broker stamp is applied at once, as it cannot be compared. The nak
// does not go through [Scheduler.nakMsg]: a deferral is not an ingest failure,
// and recording it as pending would hold back the liveness sweep.
func (s *Scheduler) deferDeregister(ctx context.Context, msg jetstream.Msg, workerID string) bool {
	stamp := s.brokerAt(ctx, msg, "worker.deregister", workerID)
	if stamp == nil {
		return false
	}
	now := s.now()
	// A zero effective time (no marker processed yet) is before any stamp.
	if !s.ingestTask.effective(now).Before(*stamp) {
		return false
	}
	if lag := now.Sub(*stamp); lag >= deregisterApplyWait {
		s.logger.WarnContext(
			ctx, "scheduler: deregister applied before the task-status consumer caught up",
			slog.String("worker_id", workerID),
			slog.Duration("lag", lag),
		)
		return false
	}
	if s.leftForNewLeader(ctx, msg) {
		return true
	}
	if err := msg.NakWithDelay(deregisterRecheckDelay); err != nil {
		s.logger.WarnContext(
			ctx, "scheduler: nak deferred deregister failed",
			slog.String("worker_id", workerID),
			slog.Any("error", err),
		)
	}
	s.logger.DebugContext(
		ctx, "scheduler: deregister deferred until the task-status consumer catches up",
		slog.String("worker_id", workerID),
		slog.Duration("recheck_in", deregisterRecheckDelay),
	)
	return true
}
