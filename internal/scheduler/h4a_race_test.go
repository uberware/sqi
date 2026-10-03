// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Deterministic reproductions of the H4a group-2 races (spec §3.1). Each test
// wraps the store, runs a callback at the interleaving point, and asserts the
// user-visible outcome. A wrapper overrides BOTH the method the old code
// called at that point and the method the new code calls there, so one test
// demonstrates the race before its fix and pins the fix after.
//
// Every test runs over both backends (spec §8.1): the fake and a real SQLite
// database, so the fake cannot drift from the store it stands in for.

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/store/sqlite"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/ws"
)

// raceBackends returns a fresh store for each backend the H4a race tests run
// over, keyed by the subtest name. The SQLite store lives in a temp directory
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
	return map[string]store.Store{"fake": fake.New(), "sqlite": sq}
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

// ── F6: a step with more than MaxLimit tasks completes ──────────────────────

func TestH4a_F6_StepOverMaxLimitCompletes(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			job, step, _, _ := seedStatusFixture(t, st, store.TaskStatusSucceeded)
			now := time.Now()
			for range store.MaxLimit {
				if _, err := st.CreateTask(t.Context(), store.Task{
					ID: uuid.NewString(), JobID: job.ID, StepID: step.ID, Name: "t",
					Status: store.TaskStatusSucceeded, CreatedAt: now, UpdatedAt: now,
				}); err != nil {
					t.Fatalf("CreateTask: %v", err)
				}
			}
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

// ── F8: completion must not overwrite a concurrent retry ────────────────────

// retryDuringCompletionStore fires its hook just before the completion
// decision: ListTasks (old checkStepCompletion) or FinalizeStep (new).
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

func TestH4a_F8_CompletionDoesNotOverwriteRetry(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			job, step, _, _ := seedStatusFixture(t, st, store.TaskStatusSucceeded)
			now := time.Now()
			failed, err := st.CreateTask(t.Context(), store.Task{
				ID: uuid.NewString(), JobID: job.ID, StepID: step.ID, Name: "t1",
				Status: store.TaskStatusFailed, CreatedAt: now, UpdatedAt: now,
			})
			if err != nil {
				t.Fatalf("CreateTask: %v", err)
			}
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

// ── Review Focus #2: redelivery after finalize still propagates ─────────────

func TestH4a_RedeliveredCompletionStillPropagates(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			job, step, _, _ := seedStatusFixture(t, st, store.TaskStatusSucceeded)
			now := time.Now()
			dep, err := st.CreateStep(t.Context(), store.Step{
				ID: uuid.NewString(), JobID: job.ID, Name: "Step2", DependsOn: []string{step.Name},
				StepOrder: 1, Status: store.StepStatusPending, CreatedAt: now, UpdatedAt: now,
			})
			if err != nil {
				t.Fatalf("CreateStep: %v", err)
			}
			if _, err := st.CreateTask(t.Context(), store.Task{
				ID: uuid.NewString(), JobID: job.ID, StepID: dep.ID, Name: "d",
				Status: store.TaskStatusPending, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("CreateTask: %v", err)
			}
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

// ── F9: dependency reconcile must not undo a user cancel ────────────────────

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

func TestH4a_F9_ReconcileDoesNotUndoUserCancel(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			up, _, _, _ := seedStatusFixtureWithJobStatus(t, st, store.JobStatusCompleted, store.TaskStatusSucceeded)
			now := time.Now()
			dep, err := st.CreateJob(t.Context(), store.Job{
				ID: uuid.NewString(), FarmID: "farm-1", QueueID: "queue-1", Name: "dep",
				Status: store.JobStatusBlocked, TemplateFormat: store.TemplateFormatJSON, CreatedAt: now, UpdatedAt: now,
			})
			if err != nil {
				t.Fatalf("CreateJob: %v", err)
			}
			if err := st.CreateJobDependencies(t.Context(), dep.ID, []string{up.ID}); err != nil {
				t.Fatalf("CreateJobDependencies: %v", err)
			}
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

// ── F7: a late running report must not overwrite a pause ────────────────────

// pauseDuringPromoteStore fires its hook just before the promotion decision
// lands: after GetJob (old maybePromoteJobRunning, which read the status and
// then wrote blind) or before PromoteJobRunning (new, guarded).
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

func TestH4a_F7_PromoteDoesNotOverwritePause(t *testing.T) {
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

// ── F3: a canceled task's terminal report must release its claims ───────────

// claimViolations runs the backend's I3 diagnostic (an active claim on a closed
// attempt or on a task that is no longer in flight).
func claimViolations(t *testing.T, st store.Store) []string {
	t.Helper()
	switch s := st.(type) {
	case *fake.Store:
		return s.ClaimInvariantViolations()
	case *sqlite.Store:
		v, err := s.ClaimInvariantViolations(t.Context())
		if err != nil {
			t.Fatalf("ClaimInvariantViolations: %v", err)
		}
		return v
	default:
		t.Fatalf("claimViolations: unsupported store %T", st)
		return nil
	}
}

// seedPoolClaim creates a one-slot usage pool and an active claim on attempt,
// and returns the pool.
func seedPoolClaim(t *testing.T, st store.Store, attemptID string) store.UsagePool {
	t.Helper()
	pool, err := st.CreateUsagePool(t.Context(), store.UsagePool{ID: uuid.NewString(), Name: uuid.NewString(), MaxConcurrent: 1})
	if err != nil {
		t.Fatalf("CreateUsagePool: %v", err)
	}
	if _, err := st.CreateClaim(t.Context(), store.UsageClaim{ID: uuid.NewString(), PoolID: pool.ID, TaskAttemptID: attemptID}); err != nil {
		t.Fatalf("CreateClaim: %v", err)
	}
	return pool
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

// TestH4a_F3_RejectedTerminalReportReleasesClaims drives a worker's "succeeded"
// report at a task the user already canceled. The state machine refuses
// canceled -> succeeded, and the report must still free the attempt's usage
// slot: before the fix the early return on the refused transition skipped the
// release and the slot leaked for good.
func TestH4a_F3_RejectedTerminalReportReleasesClaims(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			_, _, task, attempt := seedStatusFixture(t, st, store.TaskStatusCanceled)
			pool := seedPoolClaim(t, st, attempt.ID)
			s := newStatusTestScheduler(st)
			s.ctx = t.Context()

			msg := terminalReport(t, task, attempt, "succeeded", "")
			s.handleTaskStatusMessage(msg)

			if msg.nacked || !msg.acked {
				t.Fatalf("a rejected terminal report must be acked, not nacked (acked=%v nacked=%v)", msg.acked, msg.nacked)
			}
			if n := activeClaimsOf(t, st, pool.ID); n != 0 {
				t.Fatalf("active claims = %d, want 0 (F3 leak)", n)
			}
			if v := claimViolations(t, st); len(v) != 0 {
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

// TestH4a_F3_RejectedTerminalReportWakesLeaseWaiters pins that the early exit on
// a refused transition still tells parked lease waiters: the report released a
// usage slot, so a waiter that was blocked on that pool may now fit.
func TestH4a_F3_RejectedTerminalReportWakesLeaseWaiters(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			job, _, task, attempt := seedStatusFixture(t, st, store.TaskStatusCanceled)
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

// ── Review Focus #1: a redelivered terminal report is a clean no-op ─────────

func TestH4a_RedeliveredTerminalReportIsNoOp(t *testing.T) {
	for name, st := range raceBackends(t) {
		t.Run(name, func(t *testing.T) {
			_, _, task, attempt := seedStatusFixture(t, st, store.TaskStatusRunning)
			pool := seedPoolClaim(t, st, attempt.ID)
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
				if v := claimViolations(t, st); len(v) != 0 {
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

// TestH4a_FailedReportReleasesClaims covers both outcomes of a "failed" report.
// The retry path no longer releases the claims itself (RecordTaskFailure does
// it inside its own transaction), and the exhausted path reaches
// CompleteTaskAttempt on an attempt RecordTaskFailure already closed, which
// must be a clean no-op rather than an error.
func TestH4a_FailedReportReleasesClaims(t *testing.T) {
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
					_, _, task, attempt := seedStatusFixture(t, st, store.TaskStatusRunning)
					pool := seedPoolClaim(t, st, attempt.ID)
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
					if v := claimViolations(t, st); len(v) != 0 {
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

// ── F12: queue and farm caps must hold under parallel leases ─────────────────

// leaseDuringPolicyStore fires its hook right after policyGate reads a cap
// count, which is the window between the cap check and the lease. policyGate
// is the pre-filter both before and after the fix, so the hook fires at the
// same point in both.
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

// TestH4a_F12_CapsHoldUnderParallelLease leases task A while a parallel lease
// wins task B between A's policy check and A's lease. With a cap of one, the
// scheduler used to count zero active tasks, pass the gate, and then assign A
// as well, so two tasks ran under a cap of one. The lease transaction now
// re-checks the cap with the task already counted.
func TestH4a_F12_CapsHoldUnderParallelLease(t *testing.T) {
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
						if err := st.AssignTask(context.Background(), ids[1], "w-other", time.Now()); err != nil {
							t.Errorf("AssignTask in hook: %v", err)
						}
					}}}
					s := newMetricsScheduler(wrapped, &recordBus{}, "f1")

					_, _, leased, err := s.tryLeaseTask(t.Context(), taskA, worker, worker.CPUCount, "")
					if err != nil {
						t.Fatalf("tryLeaseTask: %v", err)
					}

					if n := tc.active(t, st); n > 1 {
						t.Fatalf("active tasks = %d, want at most the cap of 1 (F12)", n)
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

// ── F4: a cancel racing a lease must not leave a running attempt behind ──────

// racingLeaseStore fires its hook at the point where a lease has decided to go
// ahead but has not yet written its attempt: CreateTaskAttempt (the old
// three-call lease, after LeaseReadyTask had already committed the
// assignment) and LeaseTask (the one-transaction lease).
type racingLeaseStore struct {
	store.Store

	hook *once
}

func (s *racingLeaseStore) CreateTaskAttempt(ctx context.Context, a store.TaskAttempt) (store.TaskAttempt, error) {
	s.hook.fire()
	return s.Store.CreateTaskAttempt(ctx, a)
}

func (s *racingLeaseStore) LeaseTask(ctx context.Context, req store.LeaseRequest) (store.LeaseResult, error) {
	s.hook.fire()
	return s.Store.LeaseTask(ctx, req)
}

// TestH4a_F4_CancelRacingLease cancels the job just before the lease writes its
// attempt. The old lease had already moved the task to assigned by then, so the
// cancel closed nothing and canceled the task, and the lease then created a
// running attempt (and an active usage claim) on a canceled task, which nothing
// ever closed. The lease transaction now sees the canceled task and writes
// nothing.
func TestH4a_F4_CancelRacingLease(t *testing.T) {
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
					t.Errorf("attempt %s is running on a canceled task (F4)", a.ID)
				}
			}
			if v := claimViolations(t, st); len(v) != 0 {
				t.Errorf("I3 violations: %v", v)
			}
			if n := activeClaimsOf(t, st, pool.ID); n != 0 {
				t.Errorf("active claims = %d on a canceled task, want 0 (F4)", n)
			}
		})
	}
}

// ── Lease outcomes other than Leased leave the task ready and write nothing ──

// TestTryLeaseTask_NonLeasedOutcomesWriteNothing drives the two outcomes a
// racing lease can produce after the scheduler's own checks passed: the task is
// taken by another lease (Lost), and the usage pool fills (PoolFull). Neither
// leaves an attempt or a claim behind. The old three-call lease left an
// orphaned attempt row after a pool refusal, because it created the attempt
// before it claimed.
func TestTryLeaseTask_NonLeasedOutcomesWriteNothing(t *testing.T) {
	cases := []struct {
		name string
		// race runs between the scheduler's checks and its lease. It may use
		// ids[1], a second ready task in the same step.
		race       func(t *testing.T, st store.Store, worker store.Worker, ids []string, pool store.UsagePool)
		wantStatus store.TaskStatus
		wantClaims int
	}{
		{
			name: "lost to another lease",
			race: func(t *testing.T, st store.Store, _ store.Worker, ids []string, _ store.UsagePool) {
				t.Helper()
				if err := st.AssignTask(context.Background(), ids[0], "w-other", time.Now()); err != nil {
					t.Errorf("AssignTask in hook: %v", err)
				}
			},
			wantStatus: store.TaskStatusAssigned,
			wantClaims: 0,
		},
		{
			name: "pool filled by another lease",
			race: func(t *testing.T, st store.Store, worker store.Worker, ids []string, pool store.UsagePool) {
				t.Helper()
				res, err := st.LeaseTask(context.Background(), store.LeaseRequest{
					TaskID: ids[1], WorkerID: worker.ID, AttemptID: uuid.NewString(), Now: time.Now().UTC(),
					Claims: []store.UsagePoolClaim{{
						ClaimID: uuid.NewString(), PoolID: pool.ID, PoolName: pool.Name, MaxConcurrent: pool.MaxConcurrent,
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
					if len(attempts) != 0 {
						t.Fatalf("task has %d attempt rows after a refused lease, want 0", len(attempts))
					}
					if n := activeClaimsOf(t, st, pool.ID); n != tc.wantClaims {
						t.Fatalf("active claims = %d, want %d", n, tc.wantClaims)
					}
					if v := claimViolations(t, st); len(v) != 0 {
						t.Fatalf("I3 violations: %v", v)
					}
				})
			}
		})
	}
}
