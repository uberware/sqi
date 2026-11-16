// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// heldBackLog is the state behind [Scheduler.logIngestHeldBack].
type heldBackLog struct {
	mu sync.Mutex
	// active is set while the consumers trail now by more than a sweep
	// interval, so an episode is logged once.
	active bool
}

// workerSweepCutoff returns the heartbeat cutoff the worker sweep judges by:
// WorkerTimeout before the time up to which the worker consumer has applied
// its stream, not before now. A heartbeat still queued in the stream has not
// reached the store, so judging by now would take a live worker offline
// whenever the consumer falls behind. It returns false before the worker
// consumer's first marker, when nothing it has applied can be trusted yet.
// effT is the task consumer's effective time at now, which
// [Scheduler.ingestHoldsWorker] also needs.
func (s *Scheduler) workerSweepCutoff(ctx context.Context, now time.Time) (cutoff, effT time.Time, ok bool) {
	effW := s.ingestWorker.effective(now)
	effT = s.ingestTask.effective(now)
	s.logIngestHeldBack(ctx, now, effW, effT)
	if effW.IsZero() {
		return time.Time{}, effT, false
	}
	return effW.Add(-s.cfg.WorkerTimeout).UTC(), effT, true
}

// dropIngestHeldWorkers removes from stale the workers [Scheduler.ingestHoldsWorker]
// holds back.
func (s *Scheduler) dropIngestHeldWorkers(ctx context.Context, stale []store.Worker, now, effT time.Time) []store.Worker {
	kept := stale[:0]
	for _, w := range stale {
		if s.ingestHoldsWorker(w, now, effT) {
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
// moment w's heartbeat expired, so a status report from before that moment may
// still be queued. A candidate with no heartbeat time is held back too.
func (s *Scheduler) ingestHoldsWorker(w store.Worker, now, effT time.Time) bool {
	if w.LastHeartbeatAt == nil {
		return true
	}
	key := workerKey(w.ID)
	if s.ingestWorker.pending(key, now) || s.ingestTask.pending(key, now) {
		return true
	}
	return effT.Before(w.LastHeartbeatAt.Add(s.cfg.WorkerTimeout))
}

// assignedReapCutoff returns the assigned_at cutoff the stuck-assigned reap
// judges by, AssignedTaskTimeout before the time up to which the task-status
// consumer has applied its stream, and the tasks a queued status report still
// names, which the reap leaves alone. It returns false before the task
// consumer's first marker.
func (s *Scheduler) assignedReapCutoff(now time.Time) (cutoff time.Time, skip map[string]bool, ok bool) {
	effT := s.ingestTask.effective(now)
	if effT.IsZero() {
		return time.Time{}, nil, false
	}
	return effT.Add(-s.cfg.AssignedTaskTimeout).UTC(), s.ingestTask.pendingIDs(keyKindTask, now), true
}

// logIngestHeldBack logs once per episode that the liveness sweep is held
// back because a consumer trails now by more than a sweep interval: once when
// an episode starts, and again only after it ends and a new one starts. A
// consumer that has processed no marker yet trails by all of now.
func (s *Scheduler) logIngestHeldBack(ctx context.Context, now, effW, effT time.Time) {
	behind := now.Sub(effW) > s.cfg.HeartbeatSweepInterval || now.Sub(effT) > s.cfg.HeartbeatSweepInterval
	h := &s.heldBack
	h.mu.Lock()
	start := behind && !h.active
	h.active = behind
	h.mu.Unlock()
	if !start {
		return
	}
	s.logger.InfoContext(ctx, "scheduler: liveness sweep held back until the consumers catch up",
		slog.Time("worker_caught_up_to", effW), slog.Time("task_caught_up_to", effT))
}
