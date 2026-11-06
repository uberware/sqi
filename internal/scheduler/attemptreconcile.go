// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// reconciler remembers, per process, which attempts each worker's recent
// heartbeats reported, so the sweep can reclaim an attempt a live worker has
// stopped reporting: a terminal report lost with the JetStream directory, a
// restored database, or a task the worker silently dropped. A false reclaim
// (the status consumer more than grace behind) costs a re-run, never a lost or
// doubled result: the late report names a closed attempt and is discarded.
//
// The same reports drive the opposite repair: an attempt a live worker still
// reports holding after the store has closed it (canceled, reclaimed, fenced
// by a restore, superseded) is a run whose cancel was lost, and the sweep
// sends it another ([Scheduler.cancelClosedAttempts]).
type reconciler struct {
	mu       sync.Mutex
	grace    time.Duration
	skew     time.Duration // reports whose worker clock is further off are ignored
	reports  map[string]workerReport
	lastSeen map[string]time.Time // attempt ID → receive time of the last report naming it
	warned   map[string]bool      // workers warned about a delayed or skewed heartbeat, until their next in-window one
}

type workerReport struct {
	workerAt   time.Time // the worker's own timestamp, for ordering its reports
	receivedAt time.Time
	attempts   map[string]struct{}
}

func newReconciler(grace, skew time.Duration) *reconciler {
	return &reconciler{
		grace: grace, skew: skew,
		reports: map[string]workerReport{}, lastSeen: map[string]time.Time{}, warned: map[string]bool{},
	}
}

// record notes one heartbeat's attempts. It ignores a report whose worker
// timestamp is more than skew from its receipt (a heartbeat delayed in
// delivery, such as a JetStream replay or a consumer stalled behind a long
// write, or one sent from a skewed clock) and returns true the first time that
// happens in an episode, so the caller warns once per episode rather than once
// per heartbeat. The next in-window report ends the episode.
//
// A report older, by the worker's own clock, than the one already recorded is
// ignored, because a delayed heartbeat must not replace a newer one whose
// attempts it may omit. An equal timestamp (a redelivery, or a report from the
// same clock tick) is taken. After a worker's clock steps backwards, its
// reports are ignored until the recorded one is pruned; until then that report
// goes stale and due reclaims none of the worker's attempts.
func (r *reconciler) record(workerID string, workerAt, receivedAt time.Time, attemptIDs []string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d := receivedAt.Sub(workerAt); d > r.skew || d < -r.skew {
		first := !r.warned[workerID]
		r.warned[workerID] = true
		return first
	}
	delete(r.warned, workerID)
	if prev, ok := r.reports[workerID]; ok && workerAt.Before(prev.workerAt) {
		return false
	}
	set := make(map[string]struct{}, len(attemptIDs))
	for _, id := range attemptIDs {
		set[id] = struct{}{}
		r.lastSeen[id] = receivedAt
	}
	r.reports[workerID] = workerReport{workerAt: workerAt, receivedAt: receivedAt, attempts: set}
	return false
}

// due returns the attempts in open to reclaim at now: the worker's latest
// report is fresh, omits the attempt, and has been omitting it for at least
// grace (measured on the server's clock from the later of the attempt's
// creation and the last report that named it). It also forgets state old
// enough not to matter.
func (r *reconciler) due(open []store.OpenAttempt, now time.Time) []store.OpenAttempt {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []store.OpenAttempt
	for _, a := range open {
		rep, ok := r.reports[a.WorkerID]
		if !ok || now.Sub(rep.receivedAt) > r.skew {
			continue
		}
		if _, held := rep.attempts[a.AttemptID]; held {
			continue
		}
		since := a.CreatedAt
		if seen, ok := r.lastSeen[a.AttemptID]; ok && seen.After(since) {
			since = seen
		}
		if rep.receivedAt.Sub(since) >= r.grace {
			out = append(out, a)
		}
	}
	r.pruneLocked(now)
	return out
}

// held returns the attempts the fresh reports at now hold, attempt ID →
// worker ID: the same freshness bound due applies.
func (r *reconciler) held(now time.Time) map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]string{}
	for w, rep := range r.reports {
		if now.Sub(rep.receivedAt) > r.skew {
			continue
		}
		for id := range rep.attempts {
			out[id] = w
		}
	}
	return out
}

// pruneLocked forgets reports and sightings older than 2×grace. Pruning cannot
// change a decision: an entry older than 2×grace yields a gap of at least grace
// whether it is kept or replaced by CreatedAt, and a report that old is far
// past the skew freshness bound due requires. Caller holds r.mu.
func (r *reconciler) pruneLocked(now time.Time) {
	for id, seen := range r.lastSeen {
		if now.Sub(seen) > 2*r.grace {
			delete(r.lastSeen, id)
		}
	}
	for w, rep := range r.reports {
		if now.Sub(rep.receivedAt) > 2*r.grace {
			delete(r.reports, w)
		}
	}
}

// allOpen is how far past the sweep's clock the open-attempt list reaches, so
// that it holds every attempt open when it is read, whatever the clock: the
// list is what tells a reported attempt that is open from one that is closed.
const allOpen = 24 * time.Hour

// reconcileAttempts reclaims open attempts their workers no longer report, and
// cancels closed attempts their workers still report. Runs on the
// heartbeat-sweep tick; does nothing while disabled or during the first grace
// after the scheduler started (state is rebuilt from live heartbeats and any
// JetStream replay drains before anything acts on it).
//
// The held reports are taken before the open attempts are listed: every
// attempt they name was leased before the list was read, so one missing from
// the list is closed, not leased since.
func (s *Scheduler) reconcileAttempts(ctx context.Context, now time.Time) {
	if s.reconciler == nil || now.Sub(s.startedAt) < s.cfg.ReconcileGrace {
		return
	}
	held := s.reconciler.held(now)
	open, err := s.store.ListOpenAttempts(ctx, now.Add(allOpen))
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.logger.WarnContext(ctx, "scheduler: list open attempts failed", slog.Any("error", err))
		}
		return
	}
	for _, a := range s.reconciler.due(open, now) {
		if s.deposed() {
			return
		}
		s.reclaimUnreportedAttempt(ctx, a, now)
	}
	for _, a := range open {
		delete(held, a.AttemptID)
	}
	s.cancelClosedAttempts(ctx, held, now)
}

// cancelClosedAttempts sends a cancel naming each attempt in held (attempt ID
// → the worker reporting it), all of which the store has closed. It is sent
// on every sweep while the worker reports the attempt, because a core-NATS
// cancel reaches only a connected worker already subscribed. An attempt the
// store does not know (a database restored from before its lease) has no task
// to address it to and is skipped.
func (s *Scheduler) cancelClosedAttempts(ctx context.Context, held map[string]string, now time.Time) {
	for attemptID, workerID := range held {
		if s.deposed() {
			return
		}
		a, err := s.store.GetTaskAttempt(ctx, attemptID)
		if errors.Is(err, store.ErrNotFound) {
			s.logger.DebugContext(ctx, "scheduler: worker reports an attempt the store does not know",
				slog.String("worker_id", workerID), slog.String("attempt_id", attemptID))
			continue
		}
		if err != nil {
			s.warnUnlessStopped(ctx, err, "scheduler: read reported closed attempt failed",
				slog.String("attempt_id", attemptID))
			continue
		}
		task, err := s.store.GetTask(ctx, a.TaskID)
		if err != nil {
			s.warnUnlessStopped(ctx, err, "scheduler: read reported closed attempt's task failed",
				slog.String("task_id", a.TaskID), slog.String("attempt_id", attemptID))
			continue
		}
		s.publishCancel(ctx, protocol.TaskCancelMsg{
			TaskID: a.TaskID, AttemptID: attemptID, JobID: task.JobID, CanceledAt: now,
		}, workerID, "closed attempt still reported")
	}
}

// reclaimUnreportedAttempt hands one unreported attempt's task back through
// the same reclaim as a worker-shutdown report (handleTaskShutdown): no retry
// consumed, no failure counted.
func (s *Scheduler) reclaimUnreportedAttempt(ctx context.Context, a store.OpenAttempt, now time.Time) {
	reclaimed, err := s.reclaimAttempt(ctx, a.AttemptID, a.TaskID, now)
	if reclaimed {
		if s.metrics != nil {
			s.metrics.SchedulerTasksReconciled.Inc()
		}
		s.logger.WarnContext(ctx, "scheduler: reclaimed an attempt its worker no longer reports",
			slog.String("worker_id", a.WorkerID), slog.String("task_id", a.TaskID),
			slog.String("attempt_id", a.AttemptID), slog.Duration("open_for", now.Sub(a.CreatedAt)))
	}
	s.warnUnlessStopped(ctx, err, "scheduler: reclaim unreported attempt failed",
		slog.String("task_id", a.TaskID), slog.String("attempt_id", a.AttemptID))
}
