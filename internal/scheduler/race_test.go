// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Deterministic reproductions of the store races that are live even on
// SQLite: operations that read in one call and write in another, so another
// writer can land in between. Each test wraps the store, runs a callback at
// the interleaving point, and asserts the user-visible outcome. A wrapper
// overrides both the method a read-then-write implementation would call at
// that point and the guarded store operation the scheduler calls there, so the
// test fails against the race and pins the guarded operation.
//
// Every test runs over both backends, the fake and a real SQLite database, so
// the fake cannot drift from the store it stands in for.

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/sqlite"
	"github.com/uberware/sqi/internal/store/storetest"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/ws"
)

// raceBackends returns a fresh store for each backend the race tests run over,
// keyed by the subtest name. The SQLite store lives in a temp directory
// and is closed when the test ends.
func raceBackends(t *testing.T) map[string]store.Store {
	t.Helper()
	sq, err := sqlite.Open(t.Context(), t.TempDir()+"/test.db", sqlite.DefaultOptions())
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := sq.Close(); err != nil {
			t.Errorf("close sqlite store: %v", err)
		}
	})
	checkClaimsAtEnd(t, sq)
	return map[string]store.Store{"fake": newCheckedFake(t), "sqlite": sq}
}

// once runs fn the first time it is called and never again.
type once struct {
	o  sync.Once
	fn func()
}

func (o *once) fire() {
	if o != nil && o.fn != nil {
		o.o.Do(o.fn)
	}
}

func mustStep(t *testing.T, st store.Store, id string) store.Step {
	t.Helper()
	step, err := st.GetStep(t.Context(), id)
	if err != nil {
		t.Fatalf("GetStep %s: %v", id, err)
	}
	return step
}

// ── A step with more than MaxLimit tasks completes ──────────────────────────

func TestStepOverMaxLimitCompletes(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			g := seedStatusJob(t, st, statusJob{steps: []statusStep{{
				name: "Step1", status: store.StepStatusRunning,
				tasks: slices.Repeat([]store.TaskStatus{store.TaskStatusSucceeded}, store.MaxLimit+1),
			}}})
			job, step := g.job, g.steps[0]
			s := newStatusTestScheduler(st)
			if err := s.checkStepCompletion(t.Context(), step.ID, job.ID); err != nil {
				t.Fatalf("checkStepCompletion over %d tasks: %v", store.MaxLimit+1, err)
			}
			if got := mustStep(t, st, step.ID); got.Status != store.StepStatusCompleted {
				t.Fatalf("step = %q, want completed", got.Status)
			}
			if got := mustJob(t, st, job.ID); got.Status != store.JobStatusCompleted {
				t.Fatalf("job = %q, want completed", got.Status)
			}
		})
	}
}

// ── Completion must not overwrite a concurrent retry ────────────────────────

// retryDuringCompletionStore fires its hook just before the completion
// decision: ListTasks (a read-then-write completion check) or FinalizeStep
// (the guarded decision).
type retryDuringCompletionStore struct {
	store.Store

	hook *once
}

func (s *retryDuringCompletionStore) ListTasks(ctx context.Context, o store.ListTasksOptions) (store.Page[store.Task], error) {
	page, err := s.Store.ListTasks(ctx, o)
	s.hook.fire()
	return page, err
}

func (s *retryDuringCompletionStore) FinalizeStep(ctx context.Context, id string, now time.Time) (store.StepStatus, bool, error) {
	s.hook.fire()
	return s.Store.FinalizeStep(ctx, id, now)
}

func TestCompletionDoesNotOverwriteRetry(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			g := seedStatusJob(t, st, statusJob{steps: []statusStep{{
				name: "Step1", status: store.StepStatusRunning,
				tasks: []store.TaskStatus{store.TaskStatusSucceeded, store.TaskStatusFailed},
			}}})
			job, step, failed := g.job, g.steps[0], g.tasks[0][1]
			wrapped := &retryDuringCompletionStore{Store: st, hook: &once{fn: func() {
				if _, err := st.RetryTasks(context.Background(), job.ID, []string{failed.ID}, time.Now()); err != nil {
					t.Errorf("RetryTasks in hook: %v", err)
				}
			}}}
			s := newStatusTestScheduler(wrapped)
			if err := s.checkStepCompletion(t.Context(), step.ID, job.ID); err != nil {
				t.Fatalf("checkStepCompletion: %v", err)
			}
			if got := mustStep(t, st, step.ID); got.Status == store.StepStatusFailed {
				t.Fatal("step marked failed although a retry revived its failed task first")
			}
			if got := mustJob(t, st, job.ID); got.Status.IsTerminal() {
				t.Fatalf("job = %q although it holds a pending task", got.Status)
			}
		})
	}
}

// ── Redelivery after finalize still propagates ──────────────────────────────

func TestRedeliveredCompletionStillPropagates(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			g := seedStatusJob(t, st, statusJob{steps: []statusStep{
				{name: "Step1", status: store.StepStatusRunning, tasks: []store.TaskStatus{store.TaskStatusSucceeded}},
				{name: "Step2", status: store.StepStatusPending, dependsOn: []string{"Step1"}, tasks: []store.TaskStatus{store.TaskStatusPending}},
			}})
			job, step, dep := g.job, g.steps[0], g.steps[1]
			now := time.Now()
			// The first delivery finalized the step and then died before propagating.
			if _, _, err := st.FinalizeStep(t.Context(), step.ID, now); err != nil {
				t.Fatalf("FinalizeStep: %v", err)
			}
			s := newStatusTestScheduler(st)
			if err := s.checkStepCompletion(t.Context(), step.ID, job.ID); err != nil {
				t.Fatalf("checkStepCompletion redelivery: %v", err)
			}
			if got := mustStep(t, st, dep.ID); got.Status != store.StepStatusReady {
				t.Fatalf("dependent step = %q, want ready (propagation must run on redelivery)", got.Status)
			}
		})
	}
}

// ── Dependency reconcile must not undo a user cancel ────────────────────────

// cancelDuringReconcileStore fires its hook after the reconcile has read the
// dependent's upstream list, which is the window between its "still blocked"
// read and its release write.
type cancelDuringReconcileStore struct {
	store.Store

	hook *once
}

func (s *cancelDuringReconcileStore) ListJobDependencyIDs(ctx context.Context, id string) ([]string, error) {
	ids, err := s.Store.ListJobDependencyIDs(ctx, id)
	s.hook.fire()
	return ids, err
}

func TestReconcileDoesNotUndoUserCancel(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			up, _, _, _ := seedStatusFixtureWithJobStatus(t, st, store.JobStatusCompleted, store.TaskStatusSucceeded)
			dep := storetest.Submit(t, st, store.JobSubmission{
				Job: store.Job{
					ID: uuid.NewString(), FarmID: "farm-1", QueueID: "queue-1", Name: "dep",
					Status: store.JobStatusBlocked, TemplateFormat: store.TemplateFormatJSON,
				},
				DependsOn: []string{up.ID},
			}).Job
			wrapped := &cancelDuringReconcileStore{Store: st, hook: &once{fn: func() {
				if err := st.CancelJobStatus(context.Background(), dep.ID); err != nil {
					t.Errorf("CancelJobStatus in hook: %v", err)
				}
			}}}
			s := newStatusTestScheduler(wrapped)
			if err := s.reconcileBlockedJob(t.Context(), dep.ID); err != nil {
				t.Fatalf("reconcileBlockedJob: %v", err)
			}
			if got := mustJob(t, st, dep.ID); got.Status != store.JobStatusCanceled {
				t.Fatalf("job = %q, want canceled (the user's cancel must survive the reconcile)", got.Status)
			}
		})
	}
}

// ── A late running report must not overwrite a pause ────────────────────────

// pauseDuringPromoteStore fires its hook just before the promotion decision
// lands: after GetJob (a promotion that reads the status and then writes
// blind) or before PromoteJobRunning (the guarded write).
type pauseDuringPromoteStore struct {
	store.Store

	hook *once
}

func (s *pauseDuringPromoteStore) GetJob(ctx context.Context, id string) (store.Job, error) {
	j, err := s.Store.GetJob(ctx, id)
	s.hook.fire()
	return j, err
}

func (s *pauseDuringPromoteStore) PromoteJobRunning(ctx context.Context, id string, now time.Time) (bool, error) {
	s.hook.fire()
	return s.Store.PromoteJobRunning(ctx, id, now)
}

func TestPromoteDoesNotOverwritePause(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			job, _, _, _ := seedStatusFixtureWithJobStatus(t, st, store.JobStatusPending, store.TaskStatusRunning)
			wrapped := &pauseDuringPromoteStore{Store: st, hook: &once{fn: func() {
				if err := st.PauseJob(context.Background(), job.ID, time.Now()); err != nil {
					t.Errorf("PauseJob in hook: %v", err)
				}
			}}}
			s := newStatusTestScheduler(wrapped)
			s.maybePromoteJobRunning(t.Context(), job.ID)
			if got := mustJob(t, st, job.ID); got.Status != store.JobStatusPaused {
				t.Fatalf("job = %q, want paused (the pause must survive a late running report)", got.Status)
			}
		})
	}
}

// ── A canceled task's terminal report must release its claims ───────────────

// seedPoolClaim creates a one-slot usage pool and an active claim on attempt,
// and returns the pool. The claim is injected, so it is only for an attempt no
// lease wrote (see [injectOpenAttempt]); a claim held by in-flight work comes
// from its lease, through [statusJob]'s pool.
func seedPoolClaim(t *testing.T, st store.Store, attemptID string) store.UsagePool {
	t.Helper()
	pool := newPool(t, st, 1)
	storetest.InjectClaim(t, st, store.UsageClaim{PoolID: pool.ID, TaskAttemptID: attemptID})
	return *pool
}

// newPool creates a usage pool of maxConcurrent slots under a unique name.
func newPool(t *testing.T, st store.Store, maxConcurrent int) *store.UsagePool {
	t.Helper()
	pool, err := st.CreateUsagePool(t.Context(), store.UsagePool{ID: uuid.NewString(), Name: uuid.NewString(), MaxConcurrent: maxConcurrent})
	if err != nil {
		t.Fatalf("CreateUsagePool: %v", err)
	}
	return &pool
}

// injectOpenAttempt injects a running attempt on task for statusTestWorkerID.
// The tests that use it put it on a canceled task, a state production cannot
// reach: CancelTaskExecution and CancelJobExecution close the attempt and
// release its claims in the cancel's own transaction. They pin that a worker
// report meeting that state still closes the attempt and frees its slots.
func injectOpenAttempt(t *testing.T, st store.Store, task store.Task) store.TaskAttempt {
	t.Helper()
	return storetest.InjectAttempt(t, st, store.TaskAttempt{
		TaskID: task.ID, WorkerID: statusTestWorkerID, AttemptNumber: 1, Status: store.AttemptStatusRunning,
	})
}

// seedClaimedFixture is seedStatusFixture's job with its task leased to
// statusTestWorkerID (and, for running, started) by a lease that claims the
// one slot of a fresh usage pool, which it returns.
func seedClaimedFixture(t *testing.T, st store.Store, taskStatus store.TaskStatus) (
	job store.Job, task store.Task, attempt store.TaskAttempt, pool store.UsagePool,
) {
	t.Helper()
	p := newPool(t, st, 1)
	g := seedStatusJob(t, st, statusJob{pool: p, steps: []statusStep{
		{name: "Step1", status: store.StepStatusRunning, tasks: []store.TaskStatus{taskStatus}},
	}})
	task = g.tasks[0][0]
	return g.job, task, g.attempts[task.ID], *p
}

// seedStaleWorkerWithClaim is seedStaleWorkerWithTask with a usage claim: the
// worker's running task is leased with a claim on the one slot of a fresh
// usage pool, as a lease of a step that requires the pool writes it. Returns
// the worker, task and attempt IDs and the pool.
func seedStaleWorkerWithClaim(t *testing.T, st store.Store, age time.Duration) (
	workerID, taskID, attemptID string, pool store.UsagePool,
) {
	t.Helper()
	p := newPool(t, st, 1)
	workerID, taskID, attemptID = seedStaleWorker(t, st, age, p)
	return workerID, taskID, attemptID, *p
}

// activeClaimsOf returns the number of active claims on pool.
func activeClaimsOf(t *testing.T, st store.Store, poolID string) int {
	t.Helper()
	n, err := st.ActiveClaimCount(t.Context(), poolID)
	if err != nil {
		t.Fatalf("ActiveClaimCount: %v", err)
	}
	return n
}

// terminalReport builds the message a worker publishes when an attempt ends
// with status ("succeeded", "failed" or "canceled").
func terminalReport(t *testing.T, task store.Task, attempt store.TaskAttempt, status, message string) *fakeJSMsg {
	t.Helper()
	exit := 0
	if status == "failed" {
		exit = 1
	}
	return &fakeJSMsg{subject: statusTestSubject, data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
		Version: protocol.ProtocolVersion, TaskID: task.ID, AttemptID: attempt.ID,
		Status: status, ExitCode: &exit, Message: message, At: time.Now().UTC(),
	})}
}

func mustTaskOf(t *testing.T, st store.Store, id string) store.Task {
	t.Helper()
	task, err := st.GetTask(t.Context(), id)
	if err != nil {
		t.Fatalf("GetTask %s: %v", id, err)
	}
	return task
}

func mustAttemptOf(t *testing.T, st store.Store, id string) store.TaskAttempt {
	t.Helper()
	a, err := st.GetTaskAttempt(t.Context(), id)
	if err != nil {
		t.Fatalf("GetTaskAttempt %s: %v", id, err)
	}
	return a
}

// parkWaiter parks a lease waiter on queueID and returns a channel that
// receives true once a notify wakes it. It returns only after the waiter is
// registered, so a notify that follows cannot be missed.
func parkWaiter(t *testing.T, s *Scheduler, queueID string) <-chan bool {
	t.Helper()
	woke := make(chan bool, 1)
	go func() { woke <- s.waiters.wait(t.Context(), queueID, 30*time.Second) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.waiters.mu.Lock()
		parked := len(s.waiters.waiters[queueID])
		s.waiters.mu.Unlock()
		if parked > 0 {
			return woke
		}
		if time.Now().After(deadline) {
			t.Fatal("lease waiter never parked")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestRejectedTerminalReportReleasesClaims drives a worker's "succeeded"
// report at a task the user already canceled. The state machine refuses
// canceled -> succeeded, and the report must still free the attempt's usage
// slot: an early return on the refused transition that skipped the release
// would leak the slot for good.
func TestRejectedTerminalReportReleasesClaims(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			_, _, task, _ := seedStatusFixture(t, st, store.TaskStatusCanceled)
			attempt := injectOpenAttempt(t, st, task)
			pool := seedPoolClaim(t, st, attempt.ID)
			s := newStatusTestScheduler(st)
			s.ctx = t.Context()

			msg := terminalReport(t, task, attempt, "succeeded", "")
			s.handleTaskStatusMessage(msg)

			if msg.nacked || !msg.acked {
				t.Fatalf("a rejected terminal report must be acked, not nacked (acked=%v nacked=%v)", msg.acked, msg.nacked)
			}
			if n := activeClaimsOf(t, st, pool.ID); n != 0 {
				t.Fatalf("active claims = %d, want 0 (claim leak)", n)
			}
			if v := storetest.ClaimViolations(t, st); len(v) != 0 {
				t.Fatalf("I3 violations: %v", v)
			}
			if got := mustAttemptOf(t, st, attempt.ID); got.Status != store.AttemptStatusSucceeded || got.EndedAt == nil {
				t.Fatalf("attempt = %q (ended_at set: %v), want closed as succeeded", got.Status, got.EndedAt != nil)
			}
			if got := mustTaskOf(t, st, task.ID); got.Status != store.TaskStatusCanceled {
				t.Fatalf("task = %q, want canceled (a rejected report must not rewrite it)", got.Status)
			}
		})
	}
}

// TestRejectedTerminalReportWakesLeaseWaiters pins that the early exit on
// a refused transition still tells parked lease waiters: the report released a
// usage slot, so a waiter that was blocked on that pool may now fit.
func TestRejectedTerminalReportWakesLeaseWaiters(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			job, _, task, _ := seedStatusFixture(t, st, store.TaskStatusCanceled)
			attempt := injectOpenAttempt(t, st, task)
			s := newStatusTestScheduler(st)
			s.ctx = t.Context()
			woke := parkWaiter(t, s, job.QueueID)

			s.handleTaskStatusMessage(terminalReport(t, task, attempt, "succeeded", ""))

			select {
			case got := <-woke:
				if !got {
					t.Fatal("waiter returned without being woken")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("lease waiter was not woken by a rejected terminal report that released a slot")
			}
		})
	}
}

// ── A redelivered terminal report is a clean no-op ──────────────────────────

func TestRedeliveredTerminalReportIsNoOp(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			_, task, attempt, pool := seedClaimedFixture(t, st, store.TaskStatusRunning)
			s := newStatusTestScheduler(st)
			s.ctx = t.Context()
			payload := terminalReport(t, task, attempt, "succeeded", "").data

			var closed store.TaskAttempt
			for i := range 2 {
				msg := &fakeJSMsg{subject: statusTestSubject, data: payload}
				s.handleTaskStatusMessage(msg)
				if msg.nacked || !msg.acked {
					t.Fatalf("delivery %d: acked=%v nacked=%v, want acked", i+1, msg.acked, msg.nacked)
				}
				if got := mustTaskOf(t, st, task.ID); got.Status != store.TaskStatusSucceeded {
					t.Fatalf("delivery %d: task = %q, want succeeded", i+1, got.Status)
				}
				if n := activeClaimsOf(t, st, pool.ID); n != 0 {
					t.Fatalf("delivery %d: active claims = %d, want 0", i+1, n)
				}
				if v := storetest.ClaimViolations(t, st); len(v) != 0 {
					t.Fatalf("delivery %d: I3 violations: %v", i+1, v)
				}
				got := mustAttemptOf(t, st, attempt.ID)
				if i == 0 {
					closed = got
					continue
				}
				// The second delivery must not rewrite the attempt the first closed.
				if got.Status != closed.Status || got.EndedAt == nil || closed.EndedAt == nil ||
					!got.EndedAt.Equal(*closed.EndedAt) {
					t.Fatalf("redelivery rewrote the closed attempt: before=%+v after=%+v", closed, got)
				}
			}
		})
	}
}

// ── The failure fork releases claims through RecordTaskFailure ──────────────

// TestFailedReportReleasesClaims covers both outcomes of a "failed" report.
// The retry path does not release the claims itself (RecordTaskFailure does
// it inside its own transaction), and the exhausted path reaches
// CompleteTaskAttempt on an attempt RecordTaskFailure already closed, which
// must be a clean no-op rather than an error.
func TestFailedReportReleasesClaims(t *testing.T) {
	cases := []struct {
		name        string
		maxAttempts int
		wantTask    store.TaskStatus
		wantReason  string
	}{
		{"retry", 3, store.TaskStatusReady, ""},
		{"exhausted", 1, store.TaskStatusFailed, "boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for name, st := range raceBackends(t) {
				t.Run(name, func(t *testing.T) {
					_, task, attempt, pool := seedClaimedFixture(t, st, store.TaskStatusRunning)
					cfg := DefaultConfig()
					cfg.DefaultMaxAttempts = tc.maxAttempts
					cfg.RetryDelay = 0
					s := New(cfg, st, nil, metrics.New(), slog.New(slog.DiscardHandler), ws.NoopNotifier{}, nil)
					s.ctx = t.Context()

					msg := terminalReport(t, task, attempt, "failed", "boom")
					s.handleTaskStatusMessage(msg)

					if msg.nacked || !msg.acked {
						t.Fatalf("acked=%v nacked=%v, want acked", msg.acked, msg.nacked)
					}
					if n := activeClaimsOf(t, st, pool.ID); n != 0 {
						t.Fatalf("active claims = %d, want 0", n)
					}
					if v := storetest.ClaimViolations(t, st); len(v) != 0 {
						t.Fatalf("I3 violations: %v", v)
					}
					got := mustTaskOf(t, st, task.ID)
					if got.Status != tc.wantTask {
						t.Fatalf("task = %q, want %q", got.Status, tc.wantTask)
					}
					if got.FailedAttempts != 1 {
						t.Fatalf("failed_attempts = %d, want 1", got.FailedAttempts)
					}
					if got.FailureReason != tc.wantReason {
						t.Fatalf("failure_reason = %q, want %q", got.FailureReason, tc.wantReason)
					}
					if a := mustAttemptOf(t, st, attempt.ID); a.Status != store.AttemptStatusFailed {
						t.Fatalf("attempt = %q, want failed", a.Status)
					}
				})
			}
		})
	}
}

// ── Queue and farm caps must hold under parallel leases ─────────────────────

// leaseDuringPolicyStore fires its hook right after policyGate reads a cap
// count, which is the window between the cap check and the lease. policyGate is
// the pre-filter, so the hook fires before the lease transaction.
type leaseDuringPolicyStore struct {
	store.Store

	hook *once
}

func (s *leaseDuringPolicyStore) CountActiveTasksInQueue(ctx context.Context, id string) (int, error) {
	n, err := s.Store.CountActiveTasksInQueue(ctx, id)
	s.hook.fire()
	return n, err
}

func (s *leaseDuringPolicyStore) CountActiveTasksInFarm(ctx context.Context, id string) (int, error) {
	n, err := s.Store.CountActiveTasksInFarm(ctx, id)
	s.hook.fire()
	return n, err
}

// TestCapsHoldUnderParallelLease leases task A while a parallel lease
// wins task B between A's policy check and A's lease. With a cap of one, the
// policy gate counts zero active tasks and passes; assigning A as well would
// run two tasks under a cap of one. The lease transaction re-checks the cap
// with the task already counted.
func TestCapsHoldUnderParallelLease(t *testing.T) {
	one := 1
	cases := []struct {
		name   string
		setCap func(t *testing.T, st store.Store)
		active func(t *testing.T, st store.Store) int
	}{
		{
			name: "queue",
			setCap: func(t *testing.T, st store.Store) {
				t.Helper()
				q, err := st.GetQueue(t.Context(), "q1")
				if err != nil {
					t.Fatalf("GetQueue: %v", err)
				}
				q.MaxConcurrentTasks = 1
				if _, err := st.UpdateQueue(t.Context(), q); err != nil {
					t.Fatalf("UpdateQueue: %v", err)
				}
			},
			active: func(t *testing.T, st store.Store) int {
				t.Helper()
				n, err := st.CountActiveTasksInQueue(t.Context(), "q1")
				if err != nil {
					t.Fatalf("CountActiveTasksInQueue: %v", err)
				}
				return n
			},
		},
		{
			name: "farm",
			setCap: func(t *testing.T, st store.Store) {
				t.Helper()
				f, err := st.GetFarm(t.Context(), "f1")
				if err != nil {
					t.Fatalf("GetFarm: %v", err)
				}
				f.MaxConcurrentTasks = 1
				if _, err := st.UpdateFarm(t.Context(), f); err != nil {
					t.Fatalf("UpdateFarm: %v", err)
				}
			},
			active: func(t *testing.T, st store.Store) int {
				t.Helper()
				n, err := st.CountActiveTasksInFarm(t.Context(), "f1")
				if err != nil {
					t.Fatalf("CountActiveTasksInFarm: %v", err)
				}
				return n
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for name, st := range raceBackends(t) {
				t.Run(name, func(t *testing.T) {
					worker, ids := seedLeaseFixture(t, st, []*int{&one, &one})
					tc.setCap(t, st)
					taskA := mustTaskOf(t, st, ids[0])
					wrapped := &leaseDuringPolicyStore{Store: st, hook: &once{fn: func() {
						// Another lease wins task B between the policy count and this lease.
						storetest.Lease(t, st, store.LeaseRequest{TaskID: ids[1], WorkerID: "w-other"})
					}}}
					s := newMetricsScheduler(wrapped, &recordBus{}, "f1")

					_, _, leased, err := s.tryLeaseTask(t.Context(), taskA, worker, worker.CPUCount, "")
					if err != nil {
						t.Fatalf("tryLeaseTask: %v", err)
					}

					if n := tc.active(t, st); n > 1 {
						t.Fatalf("active tasks = %d, want at most the cap of 1", n)
					}
					if leased {
						t.Fatal("tryLeaseTask leased task A although the cap was already taken")
					}
					if got := mustTaskOf(t, st, ids[0]); got.Status != store.TaskStatusReady {
						t.Fatalf("task A = %q, want ready (a refused lease writes nothing)", got.Status)
					}
				})
			}
		})
	}
}

// ── A cancel racing a lease must not leave a running attempt behind ─────────

// racingLeaseStore fires its hook at the point where a lease has decided to go
// ahead but has not yet written its attempt: before LeaseTask (the
// one-transaction lease; a three-call lease would already have committed the
// assignment at that point).
type racingLeaseStore struct {
	store.Store

	hook *once
}

func (s *racingLeaseStore) LeaseTask(ctx context.Context, req store.LeaseRequest) (store.LeaseResult, error) {
	s.hook.fire()
	return s.Store.LeaseTask(ctx, req)
}

// TestCancelRacingLease cancels the job just before the lease writes its
// attempt. A three-call lease has already moved the task to assigned by then,
// so the cancel closes nothing and cancels the task, and the lease then creates
// a running attempt (and an active usage claim) on a canceled task, which
// nothing ever closes. The lease transaction sees the canceled task and writes
// nothing.
func TestCancelRacingLease(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			pool, err := st.CreateUsagePool(t.Context(), store.UsagePool{ID: uuid.NewString(), Name: "maya", MaxConcurrent: 1})
			if err != nil {
				t.Fatalf("CreateUsagePool: %v", err)
			}
			one := 1
			worker, ids := seedLeaseFixtureWith(t, st, []*int{&one}, &store.StepHostRequirements{UsagePools: []string{"maya"}})
			taskA := mustTaskOf(t, st, ids[0])
			canceller := newMetricsScheduler(st, &recordBus{}, "f1")
			wrapped := &racingLeaseStore{Store: st, hook: &once{fn: func() {
				if err := canceller.CancelJob(context.Background(), taskA.JobID); err != nil {
					t.Errorf("CancelJob in hook: %v", err)
				}
			}}}
			s := newMetricsScheduler(wrapped, &recordBus{}, "f1")

			if _, _, _, err := s.tryLeaseTask(t.Context(), taskA, worker, worker.CPUCount, ""); err != nil {
				t.Fatalf("tryLeaseTask: %v", err)
			}

			if got := mustTaskOf(t, st, ids[0]); got.Status != store.TaskStatusCanceled {
				t.Fatalf("task = %q, want canceled (the hook did not run, or the lease overwrote the cancel)", got.Status)
			}
			attempts, err := st.ListTaskAttempts(t.Context(), ids[0])
			if err != nil {
				t.Fatalf("ListTaskAttempts: %v", err)
			}
			for _, a := range attempts {
				if a.Status == store.AttemptStatusRunning {
					t.Errorf("attempt %s is running on a canceled task", a.ID)
				}
			}
			if v := storetest.ClaimViolations(t, st); len(v) != 0 {
				t.Errorf("I3 violations: %v", v)
			}
			if n := activeClaimsOf(t, st, pool.ID); n != 0 {
				t.Errorf("active claims = %d on a canceled task, want 0", n)
			}
		})
	}
}

// cancelAfterLeaseStore fires its hook right after a lease has committed: after
// LeaseTask (the one-transaction lease; for a three-call lease, the point was
// after its attempt write).
type cancelAfterLeaseStore struct {
	store.Store

	hook *once
}

func (s *cancelAfterLeaseStore) LeaseTask(ctx context.Context, req store.LeaseRequest) (store.LeaseResult, error) {
	out, err := s.Store.LeaseTask(ctx, req)
	s.hook.fire()
	return out, err
}

// TestLeaseDuringCancelLeaksNothing is the other side of
// [TestCancelRacingLease]: the cancel lands immediately after the lease
// commits, so the attempt and the claim it just wrote already exist and the
// cancel must find, close and release them. A cancel that closed attempts
// without releasing their claims, or released claims first and closed attempts
// afterwards, would leave an active claim on a canceled task.
//
// A three-call cancel passes this test too: the window that fails is the
// lease-side one (the cancel landing before the lease writes), which
// [TestCancelRacingLease] reproduces. This test pins that the
// single-transaction cancel closes the same ground.
func TestLeaseDuringCancelLeaksNothing(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			pool, err := st.CreateUsagePool(t.Context(), store.UsagePool{ID: uuid.NewString(), Name: "lic", MaxConcurrent: 1})
			if err != nil {
				t.Fatalf("CreateUsagePool: %v", err)
			}
			one := 1
			worker, ids := seedLeaseFixtureWith(t, st, []*int{&one}, &store.StepHostRequirements{UsagePools: []string{"lic"}})
			task := mustTaskOf(t, st, ids[0])
			canceller := newMetricsScheduler(st, &recordBus{}, "f1")
			wrapped := &cancelAfterLeaseStore{Store: st, hook: &once{fn: func() {
				if err := canceller.CancelJob(context.Background(), task.JobID); err != nil {
					t.Errorf("CancelJob in hook: %v", err)
				}
			}}}
			s := newMetricsScheduler(wrapped, &recordBus{}, "f1")

			if _, _, _, err := s.tryLeaseTask(t.Context(), task, worker, worker.CPUCount, ""); err != nil {
				t.Fatalf("tryLeaseTask: %v", err)
			}

			if got := mustTaskOf(t, st, task.ID); got.Status != store.TaskStatusCanceled {
				t.Fatalf("task = %q, want canceled (the hook did not run, or the lease overwrote the cancel)", got.Status)
			}
			attempts, err := st.ListTaskAttempts(t.Context(), task.ID)
			if err != nil {
				t.Fatalf("ListTaskAttempts: %v", err)
			}
			if len(attempts) != 1 {
				t.Fatalf("attempts = %d, want the one the lease wrote (the hook ran before the lease committed)", len(attempts))
			}
			if attempts[0].Status != store.AttemptStatusCanceled {
				t.Errorf("attempt = %q, want canceled", attempts[0].Status)
			}
			if v := storetest.ClaimViolations(t, st); len(v) != 0 {
				t.Errorf("I3 violations after cancel-vs-lease: %v (claim leak)", v)
			}
			if n := activeClaimsOf(t, st, pool.ID); n != 0 {
				t.Errorf("active claims = %d on a canceled task, want 0", n)
			}
		})
	}
}

// ── Lease outcomes other than Leased leave the task ready and write nothing ──

// TestTryLeaseTask_NonLeasedOutcomesWriteNothing drives the two outcomes a
// racing lease can produce after the scheduler's own checks passed: the task is
// taken by another lease (Lost), and the usage pool fills (PoolFull). Neither
// leaves an attempt or a claim behind. A three-call lease that created the
// attempt before it claimed would leave an orphaned attempt row after a pool
// refusal.
func TestTryLeaseTask_NonLeasedOutcomesWriteNothing(t *testing.T) {
	cases := []struct {
		name string
		// race runs between the scheduler's checks and its lease. It may use
		// ids[1], a second ready task in the same step.
		race       func(t *testing.T, st store.Store, worker store.Worker, ids []string, pool store.UsagePool)
		wantStatus store.TaskStatus
		wantClaims int
		// wantAttempts is the number of attempt rows on task A: the competing
		// lease's own when it took task A, and never one of the refused lease.
		wantAttempts int
	}{
		{
			name: "lost to another lease",
			race: func(t *testing.T, st store.Store, _ store.Worker, ids []string, pool store.UsagePool) {
				t.Helper()
				storetest.Lease(t, st, store.LeaseRequest{
					TaskID: ids[0], WorkerID: "w-other",
					Claims: []store.UsagePoolClaim{{
						ClaimID: uuid.NewString(), PoolID: pool.ID, PoolName: pool.Name,
					}},
				})
			},
			wantStatus:   store.TaskStatusAssigned,
			wantClaims:   1, // the competing lease's own claim, and only that
			wantAttempts: 1, // the competing lease's own attempt, and only that
		},
		{
			name: "pool filled by another lease",
			race: func(t *testing.T, st store.Store, worker store.Worker, ids []string, pool store.UsagePool) {
				t.Helper()
				res, err := st.LeaseTask(context.Background(), store.LeaseRequest{
					TaskID: ids[1], WorkerID: worker.ID, AttemptID: uuid.NewString(), Now: time.Now().UTC(),
					Claims: []store.UsagePoolClaim{{
						ClaimID: uuid.NewString(), PoolID: pool.ID, PoolName: pool.Name,
					}},
				})
				if err != nil || res.Outcome != store.LeaseLeased {
					t.Errorf("competing LeaseTask in hook = %q, %v; want leased", res.Outcome, err)
				}
			},
			wantStatus: store.TaskStatusReady,
			wantClaims: 1, // the competing lease's own claim, and only that
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for name, st := range raceBackends(t) {
				t.Run(name, func(t *testing.T) {
					pool, err := st.CreateUsagePool(t.Context(), store.UsagePool{ID: uuid.NewString(), Name: "maya", MaxConcurrent: 1})
					if err != nil {
						t.Fatalf("CreateUsagePool: %v", err)
					}
					one := 1
					worker, ids := seedLeaseFixtureWith(t, st, []*int{&one, &one}, &store.StepHostRequirements{UsagePools: []string{"maya"}})
					taskA := mustTaskOf(t, st, ids[0])
					wrapped := &racingLeaseStore{Store: st, hook: &once{fn: func() { tc.race(t, st, worker, ids, pool) }}}
					s := newMetricsScheduler(wrapped, &recordBus{}, "f1")

					payload, cost, leased, err := s.tryLeaseTask(t.Context(), taskA, worker, worker.CPUCount, "")
					if err != nil {
						t.Fatalf("tryLeaseTask: %v", err)
					}

					if leased || payload != nil || cost != 0 {
						t.Fatalf("tryLeaseTask = (payload %d bytes, cost %d, leased %v), want a skip", len(payload), cost, leased)
					}
					if got := mustTaskOf(t, st, ids[0]); got.Status != tc.wantStatus {
						t.Fatalf("task = %q, want %q", got.Status, tc.wantStatus)
					}
					attempts, err := st.ListTaskAttempts(t.Context(), ids[0])
					if err != nil {
						t.Fatalf("ListTaskAttempts: %v", err)
					}
					if len(attempts) != tc.wantAttempts {
						t.Fatalf("task has %d attempt rows after a refused lease, want %d", len(attempts), tc.wantAttempts)
					}
					for _, a := range attempts {
						if a.WorkerID == worker.ID {
							t.Fatalf("attempt %s on task A belongs to the refused lease's worker %s", a.ID, a.WorkerID)
						}
					}
					if n := activeClaimsOf(t, st, pool.ID); n != tc.wantClaims {
						t.Fatalf("active claims = %d, want %d", n, tc.wantClaims)
					}
					if v := storetest.ClaimViolations(t, st); len(v) != 0 {
						t.Fatalf("I3 violations: %v", v)
					}
				})
			}
		})
	}
}

// ── The reaper must never close a re-leased attempt ─────────────────────────

// leaseAfterReapStore fires its hook right after the reclaim has committed,
// which is where a reaper that looked up the task's latest attempt afterwards
// would do so.
type leaseAfterReapStore struct {
	store.Store

	hook *once
}

func (s *leaseAfterReapStore) ReclaimStaleAssignedTasks(ctx context.Context, cutoff time.Time) ([]store.Task, error) {
	out, err := s.Store.ReclaimStaleAssignedTasks(ctx, cutoff)
	s.hook.fire()
	return out, err
}

// TestReaperDoesNotCloseReleasedAttempt re-leases a task in the window between
// the reaper's reclaim and what the reaper does next. A reaper that then looked
// up the task's latest attempt would find the one the new lease had just
// written, close it as failed and release its claims, while the task kept
// running on the new worker. The store closes exactly the attempts of the tasks
// it reclaimed, inside the reclaim, so the new lease is out of reach. The
// reclaimed assignment's own attempt and claim are released.
func TestReaperDoesNotCloseReleasedAttempt(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			pool, err := st.CreateUsagePool(t.Context(), store.UsagePool{ID: uuid.NewString(), Name: "lic", MaxConcurrent: 2})
			if err != nil {
				t.Fatalf("CreateUsagePool: %v", err)
			}
			_, task, stale := seedStaleAssignment(t, st, "w-old", &pool) // stale assigned_at
			var fresh store.TaskAttempt
			wrapped := &leaseAfterReapStore{Store: st, hook: &once{fn: func() {
				res, err := st.LeaseTask(context.Background(), store.LeaseRequest{
					TaskID: task.ID, WorkerID: "w-new", AttemptID: uuid.NewString(), Now: time.Now().UTC(),
					Claims: []store.UsagePoolClaim{{ClaimID: uuid.NewString(), PoolID: pool.ID, PoolName: pool.Name}},
				})
				if err != nil || res.Outcome != store.LeaseLeased {
					t.Errorf("re-lease in hook = (%+v, %v), want leased", res, err)
					return
				}
				fresh = res.Attempt
			}}}
			s := newMetricsScheduler(wrapped, &recordBus{}, "farm-1")
			s.cfg.AssignedTaskTimeout = time.Minute

			s.reapStaleAssignedTasks(t.Context())

			if fresh.ID == "" {
				t.Fatal("the hook did not re-lease the task")
			}
			if a := mustAttemptOf(t, st, fresh.ID); a.Status != store.AttemptStatusRunning || a.EndedAt != nil {
				t.Fatalf("re-leased attempt = %q (ended %v), want running and open", a.Status, a.EndedAt)
			}
			if a := mustAttemptOf(t, st, stale.ID); a.Status != store.AttemptStatusFailed {
				t.Fatalf("reaped attempt = %q, want failed", a.Status)
			}
			if got := mustTaskOf(t, st, task.ID); got.Status != store.TaskStatusAssigned || got.AssignedWorkerID != "w-new" {
				t.Fatalf("task = %q on %q, want assigned to w-new", got.Status, got.AssignedWorkerID)
			}
			if n := activeClaimsOf(t, st, pool.ID); n != 1 {
				t.Fatalf("active claims = %d, want 1 (the re-lease's own; the reaped attempt's is released)", n)
			}
			if v := storetest.ClaimViolations(t, st); len(v) != 0 {
				t.Fatalf("I3 violations: %v", v)
			}
		})
	}
}

// ── A superseded attempt's late report must not end the re-leased task ──────

// TestSupersededAttemptLateReportIsIgnored reaps an assignment, leases the task
// again to another worker, and then delivers the first worker's late terminal
// report. The arrow assigned/running -> succeeded (or canceled) is legal, so a
// store that checked only the arrow would complete the task while the new
// attempt was open and held its claims: an I3 violation, and for a canceled
// echo an undo of a cancel-then-retry. The store refuses a report from an
// attempt that is not the task's latest; the consumer acks it as it does any
// refused report, still wakes lease waiters, and leaves the new lease alone.
func TestSupersededAttemptLateReportIsIgnored(t *testing.T) {
	cases := []struct {
		name    string
		current store.TaskStatus // the re-leased task's status when the late report lands
		report  string
	}{
		{"succeeded on an assigned task", store.TaskStatusAssigned, "succeeded"},
		{"succeeded on a running task", store.TaskStatusRunning, "succeeded"},
		{"canceled echo on a running task", store.TaskStatusRunning, "canceled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for name, st := range raceBackends(t) {
				t.Run(name, func(t *testing.T) {
					pool := newPool(t, st, 1)
					job, task, stale := seedStaleAssignment(t, st, statusTestWorkerID, pool) // stale assigned_at
					s := newStatusTestScheduler(st)
					s.ctx = t.Context()
					s.cfg.AssignedTaskTimeout = time.Minute

					s.reapStaleAssignedTasks(t.Context())
					fresh := storetest.Lease(t, st, store.LeaseRequest{
						TaskID: task.ID, WorkerID: "w-new",
						Claims: []store.UsagePoolClaim{{ClaimID: uuid.NewString(), PoolID: pool.ID, PoolName: pool.Name}},
					})
					if tc.current == store.TaskStatusRunning {
						storetest.Start(t, st, fresh)
					}
					woke := parkWaiter(t, s, job.QueueID)

					msg := terminalReport(t, task, stale, tc.report, "")
					s.handleTaskStatusMessage(msg)

					if msg.nacked || !msg.acked {
						t.Fatalf("a superseded attempt's report must be acked, not nacked (acked=%v nacked=%v)", msg.acked, msg.nacked)
					}
					if got := mustTaskOf(t, st, task.ID); got.Status != tc.current || got.AssignedWorkerID != "w-new" {
						t.Fatalf("task = %q on %q, want %q on w-new (a superseded attempt's report must not end the new lease)",
							got.Status, got.AssignedWorkerID, tc.current)
					}
					if a := mustAttemptOf(t, st, fresh.ID); a.Status != store.AttemptStatusRunning || a.EndedAt != nil {
						t.Fatalf("new attempt = %q (ended %v), want running and open", a.Status, a.EndedAt)
					}
					if n := activeClaimsOf(t, st, pool.ID); n != 1 {
						t.Fatalf("active claims = %d, want 1 (the new attempt's)", n)
					}
					if v := storetest.ClaimViolations(t, st); len(v) != 0 {
						t.Fatalf("I3 violations: %v", v)
					}
					select {
					case got := <-woke:
						if !got {
							t.Fatal("waiter returned without being woken")
						}
					case <-time.After(5 * time.Second):
						t.Fatal("lease waiter was not woken by the refused late report")
					}
				})
			}
		})
	}
}

// ── A heartbeat that lands during the sweep keeps the worker online ─────────

// heartbeatDuringSweepStore fires its hook right after the sweep has listed its
// stale candidates, which is the window between the list and the write that
// takes a worker offline.
type heartbeatDuringSweepStore struct {
	store.Store

	hook *once
	// listed holds the candidates the sweep was handed, so a test can tell that
	// the worker was one of them when the hook ran.
	listed []store.Worker
}

// ListStaleWorkers is the sweep's candidate list.
func (s *heartbeatDuringSweepStore) ListStaleWorkers(ctx context.Context, before time.Time) ([]store.Worker, error) {
	out, err := s.Store.ListStaleWorkers(ctx, before)
	s.listed = out
	s.hook.fire()
	return out, err
}

// TestHeartbeatDuringSweepKeepsWorkerOnline lands a heartbeat in the window
// between the sweep's stale list and its offline write. An unconditional
// offline write would mark the live worker offline and send its running task
// back to ready, to be leased and run a second time. The offline write
// re-checks the heartbeat itself, so the worker stays online, its task stays
// running and no offline event is sent.
func TestHeartbeatDuringSweepKeepsWorkerOnline(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			workerID, taskID, attemptID, pool := seedStaleWorkerWithClaim(t, st, 10*time.Minute)
			// fired proves the heartbeat landed inside the race window: every
			// assertion below also holds if the worker was never a candidate.
			fired := false
			wrapped := &heartbeatDuringSweepStore{Store: st, hook: &once{fn: func() {
				fired = true
				if err := st.UpdateWorkerHeartbeat(context.Background(), workerID, time.Now().UTC()); err != nil {
					t.Errorf("heartbeat in hook: %v", err)
				}
			}}}
			rec := &workerRecordingNotifier{}
			s := newRetentionScheduler(wrapped, time.Hour, rec)

			s.sweepStaleWorkers(t.Context())

			if !fired {
				t.Fatal("the heartbeat hook never ran: the sweep did not list stale workers, so this test proves nothing")
			}
			if len(wrapped.listed) != 1 || wrapped.listed[0].ID != workerID {
				t.Fatalf("sweep candidates = %+v, want the stale worker %q (the heartbeat must land after it was listed)", wrapped.listed, workerID)
			}
			w, err := st.GetWorker(t.Context(), workerID)
			if err != nil {
				t.Fatalf("GetWorker: %v", err)
			}
			if w.Status != store.WorkerStatusOnline {
				t.Fatalf("worker = %q, want online (its heartbeat arrived during the sweep)", w.Status)
			}
			if got := mustTaskOf(t, st, taskID); got.Status != store.TaskStatusRunning || got.AssignedWorkerID != workerID {
				t.Fatalf("task = %q on %q, want it still running on %q: a live worker's task was reclaimed and will run twice",
					got.Status, got.AssignedWorkerID, workerID)
			}
			if a := mustAttemptOf(t, st, attemptID); a.Status != store.AttemptStatusRunning || a.EndedAt != nil {
				t.Fatalf("attempt = %q (ended %v), want running and open", a.Status, a.EndedAt)
			}
			if n := activeClaimsOf(t, st, pool.ID); n != 1 {
				t.Fatalf("active claims = %d, want the live attempt's 1", n)
			}
			for _, e := range rec.workers {
				if e.WorkerID == workerID && e.Status == string(store.WorkerStatusOffline) {
					t.Fatalf("an offline event was sent for a worker that stayed online: %+v", e)
				}
			}
		})
	}
}

// ── Offline reclaim releases the usage claims of the attempts it closes ─────

// TestOfflineReclaimReleasesClaims pins that the offline sweep, which closes a
// dead worker's attempts and returns its tasks to ready, also releases the
// attempts' usage claims; otherwise a license slot stays held until the job is
// deleted.
func TestOfflineReclaimReleasesClaims(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			_, taskID, attemptID, pool := seedStaleWorkerWithClaim(t, st, 10*time.Minute)
			s := newRetentionScheduler(st, time.Hour, ws.NoopNotifier{})

			s.sweepStaleWorkers(t.Context())

			if got := mustTaskOf(t, st, taskID); got.Status != store.TaskStatusReady {
				t.Fatalf("task = %q, want ready (reclaimed)", got.Status)
			}
			if a := mustAttemptOf(t, st, attemptID); a.Status != store.AttemptStatusFailed {
				t.Fatalf("attempt = %q, want failed", a.Status)
			}
			if n := activeClaimsOf(t, st, pool.ID); n != 0 {
				t.Fatalf("active claims = %d, want 0: the dead worker's license slot is still held", n)
			}
			if v := storetest.ClaimViolations(t, st); len(v) != 0 {
				t.Fatalf("I3 violations: %v", v)
			}
		})
	}
}

// TestDeregisterReleasesClaims is the graceful-shutdown twin: a worker
// that deregisters mid-render must free its license slot as well.
func TestDeregisterReleasesClaims(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			workerID, taskID, _, pool := seedStaleWorkerWithClaim(t, st, 0) // a live worker
			s := newRetentionScheduler(st, time.Hour, ws.NoopNotifier{})

			msg := &fakeJSMsg{
				subject: bus.WorkerDeregisterSubject(workerID),
				data:    workerMsgJSON(t, map[string]string{"worker_id": workerID, "reason": "shutdown"}),
			}
			s.handleWorkerMessage(msg)

			if !msg.acked {
				t.Fatal("deregister was not acked")
			}
			if w, err := st.GetWorker(t.Context(), workerID); err != nil || w.Status != store.WorkerStatusOffline {
				t.Fatalf("worker = (%+v, %v), want offline", w, err)
			}
			if got := mustTaskOf(t, st, taskID); got.Status != store.TaskStatusReady {
				t.Fatalf("task = %q, want ready (reclaimed on deregister)", got.Status)
			}
			if n := activeClaimsOf(t, st, pool.ID); n != 0 {
				t.Fatalf("active claims = %d, want 0: a deregistered worker's license slot is still held", n)
			}
			if v := storetest.ClaimViolations(t, st); len(v) != 0 {
				t.Fatalf("I3 violations: %v", v)
			}
		})
	}
}
