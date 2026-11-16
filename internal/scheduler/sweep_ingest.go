// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
	"log/slog"
	"maps"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// workerSweepCutoff returns the heartbeat cutoff the worker sweep judges by:
// WorkerTimeout before the time up to which the worker consumer has applied
// its stream, not before now. A heartbeat still queued in the stream has not
// reached the store, so judging by now would take a live worker offline
// whenever the consumer falls behind. It returns false before the worker
// consumer's first marker, when nothing it has applied can be trusted yet.
// A bus interruption not yet held for is held for first. effT is the task
// consumer's effective time at now, which [Scheduler.ingestHoldsWorker] also
// needs.
func (s *Scheduler) workerSweepCutoff(ctx context.Context, now time.Time) (cutoff, effT time.Time, ok bool) {
	s.checkBusInterruption(now)
	effW := s.ingestWorker.effective(now)
	effT = s.ingestTask.effective(now)
	s.logIngestHeldBack(ctx, now, effW, effT)
	if effW.IsZero() {
		return time.Time{}, effT, false
	}
	return effW.Add(-s.cfg.WorkerTimeout).UTC(), effT, true
}

// dropIngestHeldWorkers removes from stale the workers [Scheduler.ingestHoldsWorker]
// holds back. The pending messages are read once for the whole list, after
// the list itself, so a message nak'd while the list was read is seen.
func (s *Scheduler) dropIngestHeldWorkers(ctx context.Context, stale []store.Worker, now, effT time.Time) []store.Worker {
	if len(stale) == 0 {
		return stale
	}
	pending := unionNames(s.ingestWorker.pending(now).workers, s.ingestTask.pending(now).workers)
	kept := stale[:0]
	for _, w := range stale {
		if s.ingestHoldsWorker(w, effT, pending) {
			s.logger.DebugContext(ctx, "scheduler: stale worker held back until its queued messages are applied",
				slog.String("worker_id", w.ID))
			continue
		}
		kept = append(kept, w)
	}
	return kept
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

// ingestHoldsWorker reports whether the sweep must leave the stale candidate
// w alone for now: a message naming it waits to be applied again by either
// consumer (pending, the workers such messages name), or the task consumer has
// not yet applied its stream up to the moment w's heartbeat expired, so a
// status report from before that moment may still be queued. A candidate with
// no heartbeat time is held back too.
func (s *Scheduler) ingestHoldsWorker(w store.Worker, effT time.Time, pending map[string]bool) bool {
	if w.LastHeartbeatAt == nil {
		return true
	}
	if pending[w.ID] {
		return true
	}
	return effT.Before(w.LastHeartbeatAt.Add(s.cfg.WorkerTimeout))
}

// assignedReapCutoff returns the assigned_at cutoff the stuck-assigned reap
// judges by, AssignedTaskTimeout before the time up to which the task-status
// consumer has applied its stream, and the tasks a queued status report still
// names, which the reap leaves alone. It returns false before the task
// consumer's first marker. A bus interruption not yet held for is held for
// first.
func (s *Scheduler) assignedReapCutoff(now time.Time) (cutoff time.Time, skip map[string]bool, ok bool) {
	s.checkBusInterruption(now)
	effT := s.ingestTask.effective(now)
	if effT.IsZero() {
		return time.Time{}, nil, false
	}
	return effT.Add(-s.cfg.AssignedTaskTimeout).UTC(), s.ingestTask.pending(now).tasks, true
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
