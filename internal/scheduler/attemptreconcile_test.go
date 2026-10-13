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

// TestReconcileAttempts_LateTerminalReportIsDiscarded pins spec §6.5: when the
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
