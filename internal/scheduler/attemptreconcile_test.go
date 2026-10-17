// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Tests for attemptreconcile.go: the in-memory reconciler that remembers which
// attempts each live worker reports, and the heartbeat-sweep hook that reclaims
// an open attempt its worker has stopped reporting.

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/store/storetest"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/ws"
)

func TestReconciler_Due(t *testing.T) {
	const grace, skew = 2 * time.Minute, 30 * time.Second
	t0 := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	att := func(created time.Time) store.OpenAttempt {
		return store.OpenAttempt{AttemptID: "a1", TaskID: "t1", WorkerID: "w1", CreatedAt: created}
	}
	type report struct {
		at       time.Time // both the worker's At and the receive time, unless skewed
		workerAt time.Time // zero: same as at
		ids      []string
	}
	cases := []struct {
		name    string
		reports []report
		open    store.OpenAttempt
		now     time.Time
		want    bool
	}{
		{
			"reported → kept",
			[]report{{at: t0.Add(3 * time.Minute), ids: []string{"a1"}}},
			att(t0), t0.Add(3 * time.Minute), false,
		},
		{
			"missing for less than grace → kept",
			[]report{{at: t0.Add(1 * time.Minute), ids: []string{"a1"}}, {at: t0.Add(2 * time.Minute)}},
			att(t0), t0.Add(2 * time.Minute), false,
		},
		{
			"missing for grace → reclaimed",
			[]report{{at: t0.Add(1 * time.Minute), ids: []string{"a1"}}, {at: t0.Add(3*time.Minute + time.Second)}},
			att(t0), t0.Add(3*time.Minute + time.Second), true,
		},
		{
			"never reported, created grace ago → reclaimed",
			[]report{{at: t0.Add(2*time.Minute + time.Second)}},
			att(t0), t0.Add(2*time.Minute + time.Second), true,
		},
		{
			"latest report stale → kept (worker may be gone; the offline sweep owns it)",
			[]report{{at: t0.Add(3 * time.Minute)}},
			att(t0), t0.Add(3*time.Minute + skew + time.Second), false,
		},
		{"no report from this worker → kept", nil, att(t0), t0.Add(10 * time.Minute), false},
		{
			"skewed report is ignored",
			[]report{{at: t0.Add(3 * time.Minute), workerAt: t0.Add(3*time.Minute - skew - time.Second)}},
			att(t0), t0.Add(3 * time.Minute), false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newReconciler(grace, skew)
			for _, rp := range tc.reports {
				wa := rp.workerAt
				if wa.IsZero() {
					wa = rp.at
				}
				r.record("w1", wa, rp.at, rp.ids)
			}
			got := r.due([]store.OpenAttempt{tc.open}, tc.now)
			if (len(got) == 1) != tc.want {
				t.Fatalf("due = %v, want reclaim=%v", got, tc.want)
			}
		})
	}
}

// TestReconciler_LateOlderReportCannotReplaceANewerOne pins that a heartbeat
// delivered after a newer one (a redelivery, or one held up in transit) does
// not replace it. With grace equal to the skew bound and a worker clock running
// ahead, the older report would otherwise pass the skew check, omit an attempt
// the newer one named, and be stamped late enough to reclaim it.
func TestReconciler_LateOlderReportCannotReplaceANewerOne(t *testing.T) {
	const grace, skew, ahead = 30 * time.Second, 30 * time.Second, 20 * time.Second
	t0 := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	r := newReconciler(grace, skew)
	newer := t0.Add(3 * time.Minute)
	r.record("w1", newer.Add(ahead), newer, []string{"a1"})
	late := newer.Add(35 * time.Second)
	r.record("w1", newer.Add(ahead-10*time.Second), late, nil) // sent before a1 was leased
	open := store.OpenAttempt{AttemptID: "a1", TaskID: "t1", WorkerID: "w1", CreatedAt: newer.Add(-time.Second)}
	if got := r.due([]store.OpenAttempt{open}, late); len(got) != 0 {
		t.Fatalf("due = %v; the older report replaced the newer one that holds a1", got)
	}
}

// TestReconciler_RecordWarnsOncePerSkewedEpisode pins that a delayed or
// skewed heartbeat warns once per episode: the first out-of-window report
// warns, later ones in the same episode do not, and an in-window report ends
// the episode so the next out-of-window one warns again (a single delayed
// heartbeat must not use up the worker's only warning).
func TestReconciler_RecordWarnsOncePerSkewedEpisode(t *testing.T) {
	r := newReconciler(2*time.Minute, 30*time.Second)
	now := time.Now().UTC()
	if !r.record("w1", now.Add(-time.Hour), now, nil) {
		t.Fatal("first skewed report should ask the caller to warn")
	}
	if r.record("w1", now.Add(-time.Hour), now.Add(time.Second), nil) {
		t.Fatal("a second skewed report from the same worker should not warn again")
	}
	if _, ok := r.reports["w1"]; ok {
		t.Fatal("a skewed report must not be recorded")
	}
	in := now.Add(2 * time.Second)
	if r.record("w1", in, in, nil) {
		t.Fatal("an in-window report must not warn")
	}
	if !r.record("w1", now.Add(-time.Hour), now.Add(3*time.Second), nil) {
		t.Fatal("a skewed report after an in-window one starts a new episode and should warn again")
	}
	if !r.record("w2", now.Add(-time.Hour), now, nil) {
		t.Fatal("another worker's first skewed report should warn")
	}
}

// TestNew_RaisesReconcileGraceToWorkerTimeout pins that New keeps the
// reconciler's grace at least WorkerTimeout (its skew bound) for a caller that
// builds Config directly, and leaves 0 (disabled) alone.
func TestNew_RaisesReconcileGraceToWorkerTimeout(t *testing.T) {
	cases := []struct {
		name          string
		grace, worker time.Duration
		want          time.Duration
	}{
		{"below WorkerTimeout is raised", time.Minute, 5 * time.Minute, 5 * time.Minute},
		{"equal is kept", 5 * time.Minute, 5 * time.Minute, 5 * time.Minute},
		{"above is kept", 10 * time.Minute, 5 * time.Minute, 10 * time.Minute},
		{"zero stays disabled", 0, 5 * time.Minute, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Config{ReconcileGrace: tc.grace, WorkerTimeout: tc.worker}, newCheckedFake(t), &recordBus{},
				metrics.New(), slog.New(slog.DiscardHandler), ws.NoopNotifier{}, nil)
			if s.cfg.ReconcileGrace != tc.want {
				t.Fatalf("cfg.ReconcileGrace = %s, want %s", s.cfg.ReconcileGrace, tc.want)
			}
			if tc.want == 0 {
				if s.reconciler != nil {
					t.Fatal("ReconcileGrace 0 must leave reconciliation disabled")
				}
				return
			}
			if s.reconciler == nil || s.reconciler.grace != tc.want || s.reconciler.skew != tc.worker {
				t.Fatalf("reconciler = %+v, want grace %s skew %s", s.reconciler, tc.want, tc.worker)
			}
		})
	}
}

// ── scheduler-level ──────────────────────────────────────────────────────────

// reconcileWorker is the worker every scheduler-level test here leases to.
const reconcileWorker = "w1"

// seedReconcileAttempt seeds farm f1, queue q1, an online worker w1 and job j1
// whose one task t1 is running on w1, leased and started through production
// writes, and returns the store and that task's open attempt.
func seedReconcileAttempt(t *testing.T) (*fake.Store, store.TaskAttempt) {
	t.Helper()
	st := newCheckedFake(t)
	ctx := t.Context()
	if _, err := st.CreateFarm(ctx, store.Farm{ID: "f1", Name: "f1"}); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "q1", FarmID: "f1", Name: "q1"}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	now := time.Now().UTC()
	if _, _, err := st.RegisterWorker(ctx, store.Worker{
		ID: reconcileWorker, FarmID: "f1", Hostname: reconcileWorker,
		Status: store.WorkerStatusOnline, CPUCount: 4, LastHeartbeatAt: &now,
	}); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	_, attempts := storetest.NewJob("j1", "f1", "q1").
		Step("s1", store.StepStatusReady).
		Task("t1", "s1", store.TaskStatusRunning).
		SubmitLeasing(t, st, reconcileWorker)
	return st, attempts["t1"]
}

// armReconciler sets s up to reconcile with a one-minute grace, started an
// hour before the attempt was created, so the startup hold is long over.
func armReconciler(s *Scheduler, created time.Time) {
	s.cfg.ReconcileGrace = time.Minute
	s.reconciler = newReconciler(time.Minute, 30*time.Second)
	s.startedAt = created.Add(-time.Hour)
}

// reconciledTotal scrapes s's metrics endpoint and returns the value of
// sqi_scheduler_tasks_reconciled_total, so the test pins the exported name too.
func reconciledTotal(t *testing.T, s *Scheduler) float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	s.metrics.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for line := range strings.Lines(body) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "sqi_scheduler_tasks_reconciled_total "); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				t.Fatalf("parse %q: %v", line, err)
			}
			return f
		}
	}
	t.Fatalf("exposition has no sqi_scheduler_tasks_reconciled_total:\n%s", body)
	return 0
}

// heartbeatMsg builds w1's heartbeat as a JetStream message.
func heartbeatMsg(t *testing.T, hb protocol.HeartbeatMsg) *fakeJSMsg {
	t.Helper()
	hb.Version, hb.WorkerID = protocol.ProtocolVersion, reconcileWorker
	return &fakeJSMsg{subject: bus.WorkerHeartbeatSubject(reconcileWorker), data: workerMsgJSON(t, hb)}
}

// TestHandleWorkerHeartbeat_RecordsReportedAttempts pins that the heartbeat
// handler records a report keyed on AttemptsReported, not on the attempt list:
// a report naming attempts is recorded, a worker that predates the field
// (AttemptsReported false) leaves the last report as it was, and a report of
// no attempts at all replaces it with an empty set, which is how the
// reconciler learns that a worker no longer holds an attempt.
func TestHandleWorkerHeartbeat_RecordsReportedAttempts(t *testing.T) {
	st := newCheckedFake(t)
	s := newMetricsScheduler(st, &recordBus{}, "")
	now := time.Now().UTC()
	if _, _, err := st.RegisterWorker(t.Context(), store.Worker{
		ID: reconcileWorker, FarmID: "f1", Status: store.WorkerStatusOnline, LastHeartbeatAt: &now,
	}); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	if s.reconciler == nil {
		t.Fatal("the default config should enable reconciliation")
	}
	held := func() (map[string]struct{}, bool) {
		rep, ok := s.reconciler.reports[reconcileWorker]
		return rep.attempts, ok
	}

	msg := heartbeatMsg(t, protocol.HeartbeatMsg{
		At: time.Now().UTC(), AttemptsReported: true, ActiveAttemptIDs: []string{"a1"},
	})
	s.handleWorkerMessage(msg)
	if !msg.acked {
		t.Fatal("heartbeat should be acked")
	}
	attempts, ok := held()
	if _, has := attempts["a1"]; !ok || !has {
		t.Fatalf("report = %v (recorded %v), want it to hold a1", attempts, ok)
	}

	s.handleWorkerMessage(heartbeatMsg(t, protocol.HeartbeatMsg{At: time.Now().UTC()}))
	if attempts, _ := held(); len(attempts) != 1 {
		t.Fatalf("a heartbeat without attempts_reported changed the report to %v", attempts)
	}

	s.handleWorkerMessage(heartbeatMsg(t, protocol.HeartbeatMsg{At: time.Now().UTC(), AttemptsReported: true}))
	if attempts, ok := held(); !ok || len(attempts) != 0 {
		t.Fatalf("report = %v (recorded %v), want an empty set: the worker holds nothing", attempts, ok)
	}
}

func TestReconcileAttempts_ReclaimsAnUnreportedAttempt(t *testing.T) {
	st, attempt := seedReconcileAttempt(t)
	s := newMetricsScheduler(st, &recordBus{}, "f1")
	s.ctx = t.Context()
	created := attempt.CreatedAt
	armReconciler(s, created)
	s.attemptCache.put(attempt.ID, attempt.WorkerID, attempt.TaskID)

	at := created.Add(2 * time.Minute)
	s.reconciler.record(reconcileWorker, at, at, nil)
	s.reconcileAttempts(t.Context(), at)

	task := mustTaskOf(t, st, "t1")
	if task.Status != store.TaskStatusReady || task.FailedAttempts != 0 {
		t.Fatalf("task = %s with failed_attempts %d, want ready with 0", task.Status, task.FailedAttempts)
	}
	if got := reconciledTotal(t, s); got != 1 {
		t.Fatalf("sqi_scheduler_tasks_reconciled_total = %v, want 1", got)
	}
	if _, ok := s.attemptCache.get(attempt.ID); ok {
		t.Fatal("the reclaimed attempt is still in the attempt-owner cache")
	}
}

func TestReconcileAttempts_HoldsDuringStartup(t *testing.T) {
	st, attempt := seedReconcileAttempt(t)
	s := newMetricsScheduler(st, &recordBus{}, "f1")
	s.ctx = t.Context()
	created := attempt.CreatedAt
	armReconciler(s, created)
	at := created.Add(2 * time.Minute)
	s.startedAt = at.Add(-time.Second) // started less than grace ago

	s.reconciler.record(reconcileWorker, at, at, nil)
	s.reconcileAttempts(t.Context(), at)

	if got := mustTaskOf(t, st, "t1").Status; got != store.TaskStatusRunning {
		t.Fatalf("task = %s, want running: nothing is reclaimed during the first grace", got)
	}
	if got := reconciledTotal(t, s); got != 0 {
		t.Fatalf("sqi_scheduler_tasks_reconciled_total = %v, want 0", got)
	}
}

// TestReconcileAttempts_LateTerminalReportIsDiscarded pins that when the
// reconciler reclaims an attempt whose terminal report was only delayed (the
// status consumer more than grace behind), the late report names a closed
// attempt and is discarded, so the task runs again rather than recording a
// result twice or reviving the old attempt.
func TestReconcileAttempts_LateTerminalReportIsDiscarded(t *testing.T) {
	st, attempt := seedReconcileAttempt(t)
	s := newMetricsScheduler(st, &recordBus{}, "f1")
	s.ctx = t.Context()
	created := attempt.CreatedAt
	armReconciler(s, created)
	at := created.Add(2 * time.Minute)
	s.reconciler.record(reconcileWorker, at, at, nil)
	s.reconcileAttempts(t.Context(), at)
	if got := mustTaskOf(t, st, "t1").Status; got != store.TaskStatusReady {
		t.Fatalf("fixture: task = %s, want ready after the reclaim", got)
	}

	zero := 0
	msg := &fakeJSMsg{
		subject: bus.TaskStatusSubject(reconcileWorker, "j1"),
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			Version: protocol.ProtocolVersion, Type: protocol.TypeTaskStatus,
			TaskID: "t1", AttemptID: attempt.ID, JobID: "j1", WorkerID: reconcileWorker,
			Status: "succeeded", ExitCode: &zero, At: at.Add(time.Second),
		}),
	}
	s.handleTaskStatusMessage(msg)

	if !msg.acked || msg.nacked {
		t.Fatalf("late report must be acked (acked=%v nacked=%v)", msg.acked, msg.nacked)
	}
	task := mustTaskOf(t, st, "t1")
	if task.Status != store.TaskStatusReady || task.FailedAttempts != 0 {
		t.Fatalf("task = %s with failed_attempts %d, want ready with 0: the late report was stale", task.Status, task.FailedAttempts)
	}
	if a := mustAttemptOf(t, st, attempt.ID); a.Status != store.AttemptStatusFailed {
		t.Fatalf("reclaimed attempt = %s, want it to stay closed as failed", a.Status)
	}
}

func TestReconcileAttempts_DisabledAtZero(t *testing.T) {
	st, attempt := seedReconcileAttempt(t)
	cfg := DefaultConfig()
	cfg.FarmID = "f1"
	cfg.ReconcileGrace = 0
	s := New(cfg, st, &recordBus{}, metrics.New(), slog.New(slog.DiscardHandler), ws.NoopNotifier{}, nil)
	s.ctx = context.Background()
	if s.reconciler != nil {
		t.Fatal("ReconcileGrace 0 must disable reconciliation (no reconciler)")
	}
	if s.cfg.ReconcileGrace != 0 {
		t.Fatalf("New normalised ReconcileGrace 0 to %s; 0 means disabled", s.cfg.ReconcileGrace)
	}
	s.startedAt = attempt.CreatedAt.Add(-time.Hour)

	// A heartbeat reporting nothing is not recorded anywhere, and the sweep
	// hook does nothing.
	s.handleWorkerMessage(heartbeatMsg(t, protocol.HeartbeatMsg{At: time.Now().UTC(), AttemptsReported: true}))
	s.reconcileAttempts(t.Context(), attempt.CreatedAt.Add(10*time.Minute))

	if got := mustTaskOf(t, st, "t1").Status; got != store.TaskStatusRunning {
		t.Fatalf("task = %s, want running: reconciliation is disabled", got)
	}
	if got := reconciledTotal(t, s); got != 0 {
		t.Fatalf("sqi_scheduler_tasks_reconciled_total = %v, want 0", got)
	}
	open, err := st.ListOpenAttempts(t.Context(), time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("ListOpenAttempts: %v", err)
	}
	if !slices.ContainsFunc(open, func(a store.OpenAttempt) bool { return a.AttemptID == attempt.ID }) {
		t.Fatalf("open attempts = %v, want %s still open", open, attempt.ID)
	}
}

// TestReconciler_DueBoundaries pins the two inequalities due decides on: an
// attempt unreported for exactly grace is reclaimed and one nanosecond less is
// kept, and a report exactly skew old still counts as fresh while one a
// nanosecond older does not.
func TestReconciler_DueBoundaries(t *testing.T) {
	const grace, skew = 2 * time.Minute, 30 * time.Second
	t0 := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	open := []store.OpenAttempt{{AttemptID: "a1", TaskID: "t1", WorkerID: "w1", CreatedAt: t0}}
	cases := []struct {
		name     string
		reportAt time.Time // a report from w1 omitting a1, received (and sent) then
		now      time.Time
		want     bool
	}{
		{"unreported for exactly grace → reclaimed", t0.Add(grace), t0.Add(grace), true},
		{"a nanosecond short of grace → kept", t0.Add(grace - time.Nanosecond), t0.Add(grace - time.Nanosecond), false},
		{"report exactly skew old → still fresh", t0.Add(grace), t0.Add(grace + skew), true},
		{"report a nanosecond past skew → stale, kept", t0.Add(grace), t0.Add(grace + skew + time.Nanosecond), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newReconciler(grace, skew)
			r.record("w1", tc.reportAt, tc.reportAt, nil)
			if got := r.due(open, tc.now); (len(got) == 1) != tc.want {
				t.Fatalf("due = %v, want reclaim=%v", got, tc.want)
			}
		})
	}
}

// TestReconciler_PrunesOldStateWithoutChangingADecision pins that due forgets
// reports and sightings older than 2×grace, keeps newer ones, and that the
// forgetting cannot flip a decision: an attempt last seen more than 2×grace
// ago is due whether its sighting is kept or replaced by its creation time.
func TestReconciler_PrunesOldStateWithoutChangingADecision(t *testing.T) {
	const grace, skew = time.Minute, 30 * time.Second
	t0 := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	r := newReconciler(grace, skew)
	r.record("gone", t0, t0, []string{"old"})                                          // will be 2×grace+1s old
	r.record("w1", t0, t0, []string{"a1"})                                             // a1's last sighting, likewise
	r.record("w2", t0.Add(31*time.Second), t0.Add(31*time.Second), []string{"recent"}) // 90s old: past grace, inside 2×grace

	now := t0.Add(2*grace + time.Second)
	r.record("w1", now, now, nil) // w1 fresh, no longer naming a1
	open := []store.OpenAttempt{{AttemptID: "a1", TaskID: "t1", WorkerID: "w1", CreatedAt: t0.Add(-time.Hour)}}
	if got := r.due(open, now); len(got) != 1 {
		t.Fatalf("first due = %v, want a1 reclaimed", got)
	}
	if _, ok := r.reports["gone"]; ok {
		t.Error("a report older than 2×grace was kept")
	}
	if _, ok := r.lastSeen["old"]; ok {
		t.Error("a sighting older than 2×grace was kept")
	}
	if _, ok := r.lastSeen["a1"]; ok {
		t.Error("a1's sighting, older than 2×grace, was kept")
	}
	if _, ok := r.reports["w2"]; !ok {
		t.Error("a report inside 2×grace was pruned")
	}
	if _, ok := r.lastSeen["recent"]; !ok {
		t.Error("a sighting inside 2×grace was pruned")
	}
	if got := r.due(open, now); len(got) != 1 {
		t.Fatalf("due after pruning = %v, want a1 still reclaimed: pruning must not change a decision", got)
	}
}

// TestReclaimUnreportedAttempt_LostRace pins the path where the attempt closes
// between the sweep listing it and the reclaim (its terminal report landed
// first): nothing is reclaimed or counted, the task keeps its result, and the
// attempt still leaves the attempt-owner cache, since it is closed either way.
func TestReclaimUnreportedAttempt_LostRace(t *testing.T) {
	st, attempt := seedReconcileAttempt(t)
	s := newMetricsScheduler(st, &recordBus{}, "f1")
	s.ctx = t.Context()
	armReconciler(s, attempt.CreatedAt)

	zero := 0
	done := &fakeJSMsg{
		subject: bus.TaskStatusSubject(reconcileWorker, "j1"),
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			Version: protocol.ProtocolVersion, Type: protocol.TypeTaskStatus,
			TaskID: "t1", AttemptID: attempt.ID, JobID: "j1", WorkerID: reconcileWorker,
			Status: "succeeded", ExitCode: &zero, At: time.Now().UTC(),
		}),
	}
	s.handleTaskStatusMessage(done)
	if !done.acked {
		t.Fatal("fixture: the terminal report was not acked")
	}
	s.attemptCache.put(attempt.ID, attempt.WorkerID, attempt.TaskID)

	s.reclaimUnreportedAttempt(t.Context(), store.OpenAttempt{
		AttemptID: attempt.ID, TaskID: "t1", WorkerID: reconcileWorker, CreatedAt: attempt.CreatedAt,
	}, time.Now().UTC())

	if got := mustTaskOf(t, st, "t1").Status; got != store.TaskStatusSucceeded {
		t.Fatalf("task = %s, want succeeded: the reclaim lost the race and must change nothing", got)
	}
	if a := mustAttemptOf(t, st, attempt.ID); a.Status != store.AttemptStatusSucceeded {
		t.Fatalf("attempt = %s, want succeeded", a.Status)
	}
	if got := reconciledTotal(t, s); got != 0 {
		t.Fatalf("sqi_scheduler_tasks_reconciled_total = %v, want 0", got)
	}
	if _, ok := s.attemptCache.get(attempt.ID); ok {
		t.Fatal("the closed attempt is still in the attempt-owner cache")
	}
}

// TestSweepOnce_DemotesAJobAReconcileReclaimLeftIdle pins the sweep order: the
// reconciler runs before the stalled-job demotion, so a job whose only
// in-flight task it reclaims is back to pending on the same tick, not left
// "running" with nothing running until the next one.
func TestSweepOnce_DemotesAJobAReconcileReclaimLeftIdle(t *testing.T) {
	st, attempt := seedReconcileAttempt(t)
	s := newMetricsScheduler(st, &recordBus{}, "f1")
	s.ctx = t.Context()
	// The job goes running as in production, on its first running task.
	s.maybePromoteJobRunning(t.Context(), "j1")
	if job, err := st.GetJob(t.Context(), "j1"); err != nil || job.Status != store.JobStatusRunning {
		t.Fatalf("fixture: job = %s, %v; want running", job.Status, err)
	}
	// A grace short enough that the attempt, created just now, is past it on
	// sweepOnce's own clock; startedAt long ago so the startup hold is over.
	const grace = time.Millisecond
	s.cfg.ReconcileGrace = grace
	s.reconciler = newReconciler(grace, 30*time.Second)
	s.startedAt = attempt.CreatedAt.Add(-time.Hour)
	time.Sleep(5 * grace)
	now := time.Now().UTC()
	s.reconciler.record(reconcileWorker, now, now, nil)

	s.sweepOnce(t.Context())

	if got := mustTaskOf(t, st, "t1").Status; got != store.TaskStatusReady {
		t.Fatalf("task = %s, want ready (reclaimed)", got)
	}
	job, err := st.GetJob(t.Context(), "j1")
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != store.JobStatusPending {
		t.Fatalf("job = %s after one sweep, want pending: the demotion must see the reclaim", job.Status)
	}
}
