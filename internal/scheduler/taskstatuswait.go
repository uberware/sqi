// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// The wait a worker's deregister and a restarted worker's registration take
// before reclaiming the worker's attempts: until the task-status consumer has
// applied the statuses the worker stored on SQI_TASK before the message.

import (
	"context"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/worker/protocol"
)

const (
	// taskWaitRecheckDelay is how long a deferred message waits before it is
	// delivered again and re-checked.
	taskWaitRecheckDelay = 2 * time.Second

	// taskWaitApplyWait is the nominal time a message waits for the
	// task-status consumer before it is applied anyway, so a stalled task
	// consumer cannot keep a departed process's attempts open.
	taskWaitApplyWait = 15 * time.Second

	// taskWaitMaxRechecks is the delivery at which a message is applied
	// whether or not the task-status consumer has caught up: the deliveries
	// before it are nak'd [taskWaitRecheckDelay] apart, which spans
	// [taskWaitApplyWait]. The bound counts deliveries rather than comparing
	// the server's clock with the broker stamp, so neither clock skew nor a
	// message first delivered long after it was stored (a server restart or
	// failover) can end the wait early. It must stay inside the worker
	// consumer's MaxDeliver budget
	// (TestTaskStatusWait_FitsWorkerConsumerDeliveries).
	taskWaitMaxRechecks = int(taskWaitApplyWait/taskWaitRecheckDelay) + 1
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
// apply ([Scheduler.taskStatusesCaughtUp]); until then [Scheduler.holdForTaskStatuses]
// hands it back. The worker stays online meanwhile, which is safe because a
// deregistering worker has already stopped asking for work. A message with no
// broker metadata or stamp is applied at once, as it cannot be compared.
func (s *Scheduler) deferDeregister(ctx context.Context, msg jetstream.Msg, workerID string) bool {
	stamp := s.brokerAt(ctx, msg, "worker.deregister", workerID)
	if stamp == nil || s.taskStatusesCaughtUp(workerID, *stamp) {
		return false
	}
	return s.holdForTaskStatuses(ctx, msg, workerID,
		"scheduler: deregister applied before the task-status consumer caught up")
}

// deferRestartRegistration reports whether msg, a registration m of worker
// m.WorkerID that the broker stamped at stamp (nil when it has none), was
// handed back for a later re-check, in which case the handler returns without
// acknowledging it.
//
// A registration from a new process (an instance ID other than the stored
// one) reclaims the previous process's assigned and running tasks inside the
// registration write ([store.WorkerStore.RegisterWorker]). A worker restarted
// quickly can register before the task-status consumer has applied the
// statuses its previous process stored on SQI_TASK, and the reclaim would then
// close an attempt whose terminal status is about to land, so finished work
// would run again. Such a registration waits as a deregister does
// ([Scheduler.deferDeregister]). Meanwhile the new process's lease requests
// get no work ([Scheduler.leaseFromUnregisteredInstance]).
//
// Only a registration that would reclaim something waits. One with no broker
// stamp, a first registration, a re-registration of the stored process (a
// reconnect), a stale one the write will discard ([store.StaleRegistration])
// and one whose previous process holds no assigned or running task are
// applied at once, so an ordinary restart is not delayed by the marker
// cadence. A failed store read applies the registration at once, and the
// registration write then reports the store's state.
func (s *Scheduler) deferRestartRegistration(ctx context.Context, msg jetstream.Msg, m protocol.RegisterMsg, stamp *time.Time) bool {
	if stamp == nil || m.InstanceID == "" || s.taskStatusesCaughtUp(m.WorkerID, *stamp) {
		return false
	}
	stored, err := s.store.GetWorker(ctx, m.WorkerID)
	if err != nil || stored.InstanceID == "" || stored.InstanceID == m.InstanceID ||
		store.StaleRegistration(stored, store.Worker{RegistrationBrokerAt: stamp}) {
		return false
	}
	// Every held task costs at least one core (a declared reservation is at
	// least 1, and an undeclared one is charged the 1 passed here), so a
	// positive sum means the previous process holds something to reclaim.
	if held, err := s.store.CommittedCores(ctx, m.WorkerID, 1); err != nil || held <= 0 {
		return false
	}
	return s.holdForTaskStatuses(ctx, msg, m.WorkerID,
		"scheduler: restart registration applied before the task-status consumer caught up")
}

// holdForTaskStatuses hands msg, a message of worker workerID that the
// task-status consumer has not caught up to, back for a re-check in
// [taskWaitRecheckDelay] and reports true, unless this is its
// [taskWaitMaxRechecks]th delivery, which logs exhausted at Warn and reports
// false so the caller applies it. A scheduler that has lost leadership leaves
// the message untouched for the new leader. The nak does not go through
// [Scheduler.nakMsg]: a deferral is not an ingest failure, and recording it as
// pending would hold back the liveness sweep.
func (s *Scheduler) holdForTaskStatuses(ctx context.Context, msg jetstream.Msg, workerID, exhausted string) bool {
	if n := deliveries(msg); n >= uint64(taskWaitMaxRechecks) {
		s.logger.WarnContext(
			ctx, exhausted,
			slog.String("worker_id", workerID),
			slog.Uint64("deliveries", n),
		)
		return false
	}
	if s.leftForNewLeader(ctx, msg) {
		return true
	}
	if err := msg.NakWithDelay(taskWaitRecheckDelay); err != nil {
		s.logger.WarnContext(
			ctx, "scheduler: nak of a message waiting for the task-status consumer failed",
			slog.String("worker_id", workerID),
			slog.String("subject", msg.Subject()),
			slog.Any("error", err),
		)
	}
	s.logger.DebugContext(
		ctx, "scheduler: message deferred until the task-status consumer catches up",
		slog.String("worker_id", workerID),
		slog.String("subject", msg.Subject()),
		slog.Duration("recheck_in", taskWaitRecheckDelay),
	)
	return true
}

// taskStatusesCaughtUp reports whether the task-status consumer has applied
// everything worker workerID stored on SQI_TASK up to stamp: its tracker has
// applied the stream past stamp, and none of the worker's statuses stamped at
// or before it waits to be delivered again after a failed apply. A marker
// moves the tracker past a message that is waiting for redelivery, so the
// first check alone would let a reclaim close the attempt that status is
// about to close.
func (s *Scheduler) taskStatusesCaughtUp(workerID string, stamp time.Time) bool {
	now := s.now()
	// A zero effective time (no marker processed yet) is before any stamp.
	if s.ingestTask.effective(now).Before(stamp) {
		return false
	}
	return !s.ingestTask.pendingFromWorkerBy(workerID, stamp, now)
}
