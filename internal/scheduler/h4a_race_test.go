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
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/store/sqlite"
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
