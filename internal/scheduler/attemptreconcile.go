// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"slices"
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
	mu    sync.Mutex
	grace time.Duration
	skew  time.Duration // reports whose worker clock is further off are ignored
	// reports holds each worker's recent reports in the worker's own time
	// order, the latest last: at most maxKeptReports, none but the latest
	// received grace or more before the latest.
	reports  map[string][]workerReport
	lastSeen map[string]time.Time // attempt ID → receive time of the last report naming it
	warned   map[string]bool      // workers warned about a delayed or skewed heartbeat, until their next in-window one
}

// maxReportedAttempts bounds the attempt list one heartbeat may carry. Leasing
// is gated on cores, so a worker holds at most its CPU count of attempts plus
// the few closed ones it is still stopping; a longer list is broken or
// hostile, and acting on it would cost a store read per listed attempt on
// every sweep. Such a list is ignored whole, never truncated: an attempt left
// off a report reads as no longer held, and would be reclaimed.
const maxReportedAttempts = 4096

// maxKeptReports bounds the reports kept per worker. due decides on the newest
// one the task-status consumer has applied, which while that consumer keeps up
// is one of the last few; a consumer further behind than the kept reports
// reach holds the reclaim back, which is the safe direction.
const maxKeptReports = 32

// LogHeartbeatSkewed begins the warning the scheduler logs for a heartbeat whose
// worker timestamp is more than heartbeat_timeout from its receipt. The chaos
// checks look for it in the server's log.
const LogHeartbeatSkewed = "scheduler: worker heartbeat delayed or skewed"

type workerReport struct {
	workerAt   time.Time // the worker's own timestamp, for ordering its reports
	receivedAt time.Time
	brokerAt   time.Time // when the broker stored the heartbeat; zero without a stamp
	attempts   map[string]struct{}
}

// freshLocked reports whether rep is recent enough at now to act on. Caller
// holds r.mu.
func (r *reconciler) freshLocked(rep workerReport, now time.Time) bool {
	return now.Sub(rep.receivedAt) <= r.skew
}

func newReconciler(grace, skew time.Duration) *reconciler {
	return &reconciler{
		grace: grace, skew: skew,
		reports: map[string][]workerReport{}, lastSeen: map[string]time.Time{}, warned: map[string]bool{},
	}
}

// record notes one heartbeat's attempts. It ignores a report whose worker
// timestamp is more than skew from its receipt (a heartbeat delayed in
// delivery, such as a JetStream replay or a consumer stalled behind a long
// write, or one sent from a skewed clock) and returns true the first time that
// happens in an episode, so the caller warns once per episode rather than once
// per heartbeat. The next in-window report ends the episode.
//
// A report older, by the worker's own clock, than the latest one recorded is
// ignored, because a delayed heartbeat must not displace a newer one whose
// attempts it may omit. An equal timestamp (a redelivery, or a report from the
// same clock tick) replaces the latest. After a worker's clock steps
// backwards, its reports are ignored until the latest is pruned; until then
// that report goes stale and due reclaims none of the worker's attempts.
//
// brokerAt is the broker's timestamp on the heartbeat, zero when the message
// carried none. due holds the report back until the task-status consumer has
// handled every message the worker published before it.
func (r *reconciler) record(
	workerID string, workerAt, receivedAt, brokerAt time.Time, attemptIDs []string,
) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d := receivedAt.Sub(workerAt); d > r.skew || d < -r.skew {
		first := !r.warned[workerID]
		r.warned[workerID] = true
		return first
	}
	delete(r.warned, workerID)
	reps := r.reports[workerID]
	if n := len(reps); n > 0 {
		if workerAt.Before(reps[n-1].workerAt) {
			return false
		}
		if workerAt.Equal(reps[n-1].workerAt) {
			reps = reps[:n-1]
		}
	}
	set := make(map[string]struct{}, len(attemptIDs))
	for _, id := range attemptIDs {
		set[id] = struct{}{}
		r.lastSeen[id] = receivedAt
	}
	reps = append(reps, workerReport{workerAt: workerAt, receivedAt: receivedAt, brokerAt: brokerAt, attempts: set})
	r.reports[workerID] = r.trimLocked(reps, reps[len(reps)-1].receivedAt, maxKeptReports)
	return false
}

// trimLocked drops from reps, oldest first, all but the newest keep reports
// and every report but the latest received grace or more before at. Caller
// holds r.mu.
func (r *reconciler) trimLocked(reps []workerReport, at time.Time, keep int) []workerReport {
	drop := max(len(reps)-keep, 0)
	for drop < len(reps)-1 && at.Sub(reps[drop].receivedAt) >= r.grace {
		drop++
	}
	return slices.Delete(reps, 0, drop)
}

// due returns the attempts in open to reclaim at now. It decides on the
// newest of the worker's kept reports that the task-status consumer has
// applied up to: taskEffective (zero before the consumer's first marker) has
// reached its broker stamp. That report must omit the attempt, and must have
// been received at least grace (on the server's clock) after the later of the
// attempt's creation and the last report that named it. The worker's latest
// report must also be fresh and omit the attempt, and pending (nil when
// nothing is pending) must not name it, because a queued status message may
// be about to close it. It also forgets state old enough not to matter.
//
// This is safe because of what the deciding report shows: the worker did not
// hold the attempt when it sent it, and every status message the worker
// published before it has been applied, so a terminal report the worker sent
// for the attempt is already in the store. An attempt still open after that is
// one whose terminal report was lost. Deciding on the latest report alone
// would be no safer, only later: a worker heartbeating faster than the markers
// always has a report newer than the newest applied marker.
func (r *reconciler) due(
	open []store.OpenAttempt, now, taskEffective time.Time, pending map[string]bool,
) []store.OpenAttempt {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []store.OpenAttempt
	for _, a := range open {
		reps := r.reports[a.WorkerID]
		if len(reps) == 0 || pending[a.AttemptID] {
			continue
		}
		latest := reps[len(reps)-1]
		if !r.freshLocked(latest, now) {
			continue
		}
		if _, held := latest.attempts[a.AttemptID]; held {
			continue
		}
		rep, ok := newestApplied(reps, taskEffective)
		if !ok {
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

// newestApplied returns the newest of reps the task-status consumer, applied
// up to taskEffective, has caught up to, and false when it has caught up to
// none.
func newestApplied(reps []workerReport, taskEffective time.Time) (workerReport, bool) {
	for _, rep := range slices.Backward(reps) {
		if consumerCaughtUp(rep, taskEffective) {
			return rep, true
		}
	}
	return workerReport{}, false
}

// consumerCaughtUp reports whether the task-status consumer, applied up to
// taskEffective, has handled every message published before rep.
func consumerCaughtUp(rep workerReport, taskEffective time.Time) bool {
	return !taskEffective.IsZero() && (rep.brokerAt.IsZero() || !rep.brokerAt.After(taskEffective))
}

// heldAttempt is a worker's report of an attempt it holds.
type heldAttempt struct {
	workerID   string
	receivedAt time.Time // when the server received the report
}

// held returns the attempts each worker's latest report holds, by attempt ID,
// when that report is fresh at now: the same freshness bound due applies. The
// latest report alone says what the worker holds now, which is what a cancel
// resend needs; an older report naming an attempt says nothing of the run
// since.
func (r *reconciler) held(now time.Time) map[string]heldAttempt {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]heldAttempt{}
	for w, reps := range r.reports {
		rep := reps[len(reps)-1]
		if !r.freshLocked(rep, now) {
			continue
		}
		for id := range rep.attempts {
			out[id] = heldAttempt{workerID: w, receivedAt: rep.receivedAt}
		}
	}
	return out
}

// pruneLocked forgets sightings older than 2×grace, each worker's earlier
// reports received grace or more before now, and a worker whose latest report
// is older than 2×grace. None of it can cause a reclaim: a sighting older
// than 2×grace yields a gap of at least grace whether it is kept or replaced
// by CreatedAt, a latest report that old is far past the skew freshness bound
// due requires, and dropping an earlier report can only leave due with no
// applied report to decide on, which holds the reclaim back. Caller holds
// r.mu.
func (r *reconciler) pruneLocked(now time.Time) {
	for id, seen := range r.lastSeen {
		if now.Sub(seen) > 2*r.grace {
			delete(r.lastSeen, id)
		}
	}
	for w, reps := range r.reports {
		if now.Sub(reps[len(reps)-1].receivedAt) > 2*r.grace {
			delete(r.reports, w)
			continue
		}
		r.reports[w] = r.trimLocked(reps, now, len(reps))
	}
}

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
	open, err := s.store.ListOpenAttempts(ctx)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.logger.WarnContext(ctx, "scheduler: list open attempts failed", slog.Any("error", err))
		}
		return
	}
	taskEffective := s.ingestTask.effective(now)
	// due still runs with nothing open: it also forgets old reports.
	var pending map[string]bool
	if len(open) > 0 {
		pending = s.ingestTask.pending(now).attempts
	}
	for _, a := range s.reconciler.due(open, now, taskEffective, pending) {
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

// cancelClosedAttempts sends a cancel naming each attempt in held, none of
// which is open in the store. It is sent on every sweep while the worker
// reports the attempt, because a core-NATS cancel reaches only a connected
// worker already subscribed. A report received before the store closed the
// attempt is skipped: it says nothing about the run since.
//
// An attempt the store does not know (a database restored from before its
// lease, or a job deleted while it ran) is canceled by attempt ID alone, with
// no task. The reports were read before the open attempts were listed, so
// every attempt they name was leased before this read; one the store does not
// have is gone, not yet to be written.
func (s *Scheduler) cancelClosedAttempts(ctx context.Context, held map[string]heldAttempt, now time.Time) {
	for attemptID, h := range held {
		if s.deposed() {
			return
		}
		a, err := s.store.GetTaskAttempt(ctx, attemptID)
		if errors.Is(err, store.ErrNotFound) {
			s.countCancelResent("unknown")
			s.publishCancel(ctx, protocol.TaskCancelMsg{AttemptID: attemptID, CanceledAt: now},
				h.workerID, "unknown attempt still reported")
			continue
		}
		if err != nil {
			s.warnUnlessStopped(ctx, err, "scheduler: read reported closed attempt failed",
				slog.String("attempt_id", attemptID))
			continue
		}
		if a.EndedAt != nil && !h.receivedAt.After(*a.EndedAt) {
			continue
		}
		task, err := s.store.GetTask(ctx, a.TaskID)
		if err != nil {
			s.warnUnlessStopped(ctx, err, "scheduler: read reported closed attempt's task failed",
				slog.String("task_id", a.TaskID), slog.String("attempt_id", attemptID))
			continue
		}
		s.countCancelResent("closed")
		s.publishCancel(ctx, protocol.TaskCancelMsg{
			TaskID: a.TaskID, AttemptID: attemptID, JobID: task.JobID, CanceledAt: now,
		}, h.workerID, "closed attempt still reported")
	}
}

// countCancelResent counts one cancel cancelClosedAttempts sends, by cause.
func (s *Scheduler) countCancelResent(cause string) {
	s.metrics.SchedulerCancelsResent.WithLabelValues(cause).Inc()
}

// reclaimUnreportedAttempt hands one unreported attempt's task back through
// the same reclaim as a worker-shutdown report (handleTaskShutdown): no retry
// consumed, no failure counted.
func (s *Scheduler) reclaimUnreportedAttempt(ctx context.Context, a store.OpenAttempt, now time.Time) {
	reclaimed, err := s.reclaimAttempt(ctx, a.AttemptID, a.TaskID, now)
	if reclaimed {
		s.metrics.SchedulerTasksReconciled.Inc()
		s.logger.WarnContext(ctx, "scheduler: reclaimed an attempt its worker no longer reports",
			slog.String("worker_id", a.WorkerID), slog.String("task_id", a.TaskID),
			slog.String("attempt_id", a.AttemptID), slog.Duration("open_for", now.Sub(a.CreatedAt)))
	}
	s.warnUnlessStopped(ctx, err, "scheduler: reclaim unreported attempt failed",
		slog.String("task_id", a.TaskID), slog.String("attempt_id", a.AttemptID))
}
