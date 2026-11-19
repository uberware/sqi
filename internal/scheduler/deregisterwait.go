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

	// deregisterApplyWait is the nominal time a deregister waits for the
	// task-status consumer before it is applied anyway, so a stalled task
	// consumer cannot keep a departed worker's attempts open.
	deregisterApplyWait = 15 * time.Second

	// deregisterMaxRechecks is the delivery at which a deregister is applied
	// whether or not the task-status consumer has caught up: the deliveries
	// before it are nak'd [deregisterRecheckDelay] apart, which spans
	// [deregisterApplyWait]. The bound counts deliveries rather than comparing
	// the server's clock with the broker stamp, so neither clock skew nor a
	// deregister first delivered long after it was stored (a server restart
	// or failover) can end the wait early. It must stay inside the worker
	// consumer's MaxDeliver budget
	// (TestDeregisterWait_FitsWorkerConsumerDeliveries).
	deregisterMaxRechecks = int(deregisterApplyWait/deregisterRecheckDelay) + 1
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
// SQI_TASK up to the deregister's broker stamp and none of the worker's
// statuses stored by then is waiting to be delivered again after a failed
// apply ([Scheduler.taskStatusesCaughtUp]). Until then it is nak'd with
// [deregisterRecheckDelay], which leaves the worker consumer free to handle
// other workers' messages; the worker stays online meanwhile, which is safe
// because a deregistering worker has already stopped asking for work.
//
// The deferral is bounded by the message's delivery count: its
// [deregisterMaxRechecks]th delivery is applied with a warning. A message with
// no broker metadata or stamp is applied at once, as it cannot be compared.
// The nak does not go through [Scheduler.nakMsg]: a deferral is not an ingest
// failure, and recording it as pending would hold back the liveness sweep.
func (s *Scheduler) deferDeregister(ctx context.Context, msg jetstream.Msg, workerID string) bool {
	stamp := s.brokerAt(ctx, msg, "worker.deregister", workerID)
	if stamp == nil {
		return false
	}
	if s.taskStatusesCaughtUp(workerID, *stamp) {
		return false
	}
	// brokerAt has read the metadata successfully; a tracked message caches it.
	if md, err := msg.Metadata(); err == nil && md.NumDelivered >= uint64(deregisterMaxRechecks) {
		s.logger.WarnContext(
			ctx, "scheduler: deregister applied before the task-status consumer caught up",
			slog.String("worker_id", workerID),
			slog.Uint64("deliveries", md.NumDelivered),
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

// taskStatusesCaughtUp reports whether the task-status consumer has applied
// everything worker workerID stored on SQI_TASK up to stamp: its tracker has
// applied the stream past stamp, and none of the worker's statuses stamped at
// or before it waits to be delivered again after a failed apply. A marker
// moves the tracker past a message that is waiting for redelivery, so the
// first check alone would let the deregister reclaim the attempt that status
// is about to close.
func (s *Scheduler) taskStatusesCaughtUp(workerID string, stamp time.Time) bool {
	now := s.now()
	// A zero effective time (no marker processed yet) is before any stamp.
	if s.ingestTask.effective(now).Before(stamp) {
		return false
	}
	return !s.ingestTask.pendingFromWorkerBy(workerID, stamp, now)
}
