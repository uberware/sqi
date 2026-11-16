// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
	"log/slog"
	"maps"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// ingestView is what one sweep tick's liveness checks read from the ingest
// trackers. It is read once per tick, so every check on the tick judges the
// same state.
type ingestView struct {
	// workerAt and taskAt are the times up to which the worker and
	// task-status consumers have applied their streams, each zero before its
	// consumer's first marker.
	workerAt, taskAt time.Time
	// pendingWorkers names the workers a message waiting to be applied again
	// names, in either consumer; workerStreamPending those the worker
	// consumer's messages name alone.
	pendingWorkers, workerStreamPending map[string]bool
	// pendingTasks and pendingAttempts name the tasks and attempts a task
	// status waiting to be applied again names.
	pendingTasks, pendingAttempts map[string]bool
}

// readIngest reads the trackers at now. Unless the liveness sweeps are
// deferred at now ([Scheduler.livenessDeferred]), a bus interruption not yet
// held for is held for first, so a check landing between an interruption and
// the marker loop's next tick is held too.
func (s *Scheduler) readIngest(now time.Time) ingestView {
	if deferred, _ := s.livenessDeferred(now); !deferred {
		s.checkBusInterruption(now)
	}
	v := ingestView{workerAt: s.ingestWorker.effective(now), taskAt: s.ingestTask.effective(now)}
	w, t := s.ingestWorker.pending(now), s.ingestTask.pending(now)
	v.workerStreamPending = w.workers
	v.pendingWorkers = unionNames(w.workers, t.workers)
	v.pendingTasks, v.pendingAttempts = t.tasks, t.attempts
	return v
}

// unionNames returns the names in a or b, nil when both are empty. It may
// return a or b itself.
func unionNames(a, b map[string]bool) map[string]bool {
	if len(b) == 0 {
		return a
	}
	if len(a) == 0 {
		return b
	}
	out := maps.Clone(a)
	maps.Copy(out, b)
	return out
}

// workerSweepCutoff returns the heartbeat cutoff the worker sweep judges by:
// WorkerTimeout before the time up to which the worker consumer has applied
// its stream, not before now. A heartbeat still queued in the stream has not
// reached the store, so judging by now would take a live worker offline
// whenever the consumer falls behind. It returns false before the worker
// consumer's first marker, when nothing it has applied can be trusted yet.
func (s *Scheduler) workerSweepCutoff(ctx context.Context, now time.Time, v ingestView) (time.Time, bool) {
	s.logIngestHeldBack(ctx, now, v.workerAt, v.taskAt)
	if v.workerAt.IsZero() {
		return time.Time{}, false
	}
	return v.workerAt.Add(-s.cfg.WorkerTimeout).UTC(), true
}

// dropIngestHeldWorkers removes from stale the workers [Scheduler.ingestHoldsWorker]
// holds back.
func (s *Scheduler) dropIngestHeldWorkers(ctx context.Context, stale []store.Worker, v ingestView) []store.Worker {
	kept := stale[:0]
	for _, w := range stale {
		if s.ingestHoldsWorker(w, v) {
			s.logger.DebugContext(ctx, "scheduler: stale worker held back until its queued messages are applied",
				slog.String("worker_id", w.ID))
			continue
		}
		kept = append(kept, w)
	}
	return kept
}

// ingestHoldsWorker reports whether the sweep must leave the stale candidate
// w alone for now: a message naming it waits to be applied again by either
// consumer, or the task consumer has not yet applied its stream up to the
// moment w's heartbeat expired, so a status report from before that moment
// may still be queued. A candidate with no heartbeat time is held back too.
func (s *Scheduler) ingestHoldsWorker(w store.Worker, v ingestView) bool {
	if w.LastHeartbeatAt == nil {
		return true
	}
	if v.pendingWorkers[w.ID] {
		return true
	}
	return v.taskAt.Before(w.LastHeartbeatAt.Add(s.cfg.WorkerTimeout))
}

// assignedReapCutoff returns the assigned_at cutoff the stuck-assigned reap
// judges by, AssignedTaskTimeout before the time up to which the task-status
// consumer has applied its stream. It returns false before the task
// consumer's first marker.
func (s *Scheduler) assignedReapCutoff(v ingestView) (time.Time, bool) {
	if v.taskAt.IsZero() {
		return time.Time{}, false
	}
	return v.taskAt.Add(-s.cfg.AssignedTaskTimeout).UTC(), true
}

// logIngestHeldBack logs once per episode that the liveness sweep is held
// back because a consumer trails now by more than a sweep interval: once when
// an episode starts, and again only after it ends and a new one starts. A
// consumer that has processed no marker yet trails by all of now.
func (s *Scheduler) logIngestHeldBack(ctx context.Context, now, effW, effT time.Time) {
	behind := now.Sub(effW) > s.cfg.HeartbeatSweepInterval || now.Sub(effT) > s.cfg.HeartbeatSweepInterval
	if was := s.heldBack.Swap(behind); !behind || was {
		return
	}
	s.logger.InfoContext(ctx, "scheduler: liveness sweep held back until the consumers catch up",
		slog.Time("worker_caught_up_to", effW), slog.Time("task_caught_up_to", effT))
}
