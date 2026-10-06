// SPDX-License-Identifier: AGPL-3.0-or-later

package storetest_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/store/sqlite"
	"github.com/uberware/sqi/internal/store/storetest"
)

func newStores(t *testing.T) map[string]store.Store {
	t.Helper()
	sq, err := sqlite.Open(context.Background(), t.TempDir()+"/test.db", sqlite.DefaultOptions())
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := sq.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return map[string]store.Store{"sqlite": sq, "fake": fake.New()}
}

// oneTaskJob builds a farm and queue and returns a one-step, one-task
// submission whose task has the given status.
func oneTaskJob(t *testing.T, st store.Store, status store.TaskStatus) store.JobSubmission {
	t.Helper()
	ctx := t.Context()
	farm, err := st.CreateFarm(ctx, store.Farm{ID: uuid.NewString(), Name: uuid.NewString()})
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queue, err := st.CreateQueue(ctx, store.Queue{ID: uuid.NewString(), FarmID: farm.ID, Name: uuid.NewString()})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	job := store.Job{
		ID: uuid.NewString(), FarmID: farm.ID, QueueID: queue.ID, Name: "job",
		Status: store.JobStatusPending, TemplateFormat: store.TemplateFormatJSON,
	}
	step := store.Step{ID: uuid.NewString(), JobID: job.ID, Name: "s", Status: store.StepStatusReady}
	task := store.Task{ID: uuid.NewString(), JobID: job.ID, StepID: step.ID, Name: "t", Status: status}
	return store.JobSubmission{Job: job, Steps: []store.Step{step}, Tasks: []store.Task{task}}
}

func TestSubmit_WritesStatusesAsGiven(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			sub := oneTaskJob(t, st, store.TaskStatusFailed)
			sub.Tasks[0].FailureReason = "boom"
			storetest.Submit(t, st, sub)
			got, err := st.GetTask(t.Context(), sub.Tasks[0].ID)
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			if got.Status != store.TaskStatusFailed || got.FailureReason != "boom" {
				t.Fatalf("task = %s %q, want failed \"boom\"", got.Status, got.FailureReason)
			}
		})
	}
}

func TestSubmit_StampsTimestamps(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			sub := oneTaskJob(t, st, store.TaskStatusReady)
			storetest.Submit(t, st, sub)
			got, err := st.GetTask(t.Context(), sub.Tasks[0].ID)
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
				t.Fatalf("task timestamps = %v / %v, want both set", got.CreatedAt, got.UpdatedAt)
			}
		})
	}
}

func TestLease_GivesARealAttemptAndClaims(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			sub := oneTaskJob(t, st, store.TaskStatusReady)
			storetest.Submit(t, st, sub)
			pool, err := st.CreateUsagePool(t.Context(), store.UsagePool{ID: uuid.NewString(), Name: "p", MaxConcurrent: 1})
			if err != nil {
				t.Fatalf("CreateUsagePool: %v", err)
			}
			a := storetest.Lease(t, st, store.LeaseRequest{
				TaskID: sub.Tasks[0].ID, WorkerID: "w1",
				Claims: []store.UsagePoolClaim{{ClaimID: uuid.NewString(), PoolID: pool.ID, PoolName: pool.Name}},
			})
			if a.ID == "" || a.AttemptNumber != 1 || a.Status != store.AttemptStatusRunning {
				t.Fatalf("attempt = %+v, want a running attempt numbered 1", a)
			}
			task, err := st.GetTask(t.Context(), sub.Tasks[0].ID)
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			if task.Status != store.TaskStatusAssigned || task.AssignedWorkerID != "w1" {
				t.Fatalf("task = %s on %q, want assigned on w1", task.Status, task.AssignedWorkerID)
			}
			if n, err := st.ActiveClaimCount(t.Context(), pool.ID); err != nil || n != 1 {
				t.Fatalf("ActiveClaimCount = %d, %v; want 1", n, err)
			}
		})
	}
}

func TestRunning_StartsTheTask(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			sub := oneTaskJob(t, st, store.TaskStatusReady)
			storetest.Submit(t, st, sub)
			storetest.Running(t, st, store.LeaseRequest{TaskID: sub.Tasks[0].ID, WorkerID: "w1", Now: time.Now().UTC()})
			task, err := st.GetTask(t.Context(), sub.Tasks[0].ID)
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			if task.Status != store.TaskStatusRunning {
				t.Fatalf("task status = %s, want running", task.Status)
			}
		})
	}
}

func TestInjectTaskAttempt_WritesAnUnreachableState(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			// A running attempt carrying a message on a canceled task: no
			// production write produces it.
			sub := oneTaskJob(t, st, store.TaskStatusCanceled)
			storetest.Submit(t, st, sub)
			now := time.Now().UTC()
			in := store.TaskAttempt{
				ID: uuid.NewString(), TaskID: sub.Tasks[0].ID, WorkerID: "w1", AttemptNumber: 1,
				Status: store.AttemptStatusRunning, StartedAt: now, CreatedAt: now, Message: "stale",
			}
			if _, err := storetest.InjectorFor(t, st).InjectTaskAttempt(t.Context(), in); err != nil {
				t.Fatalf("InjectTaskAttempt: %v", err)
			}
			got, err := st.GetTaskAttempt(t.Context(), in.ID)
			if err != nil {
				t.Fatalf("GetTaskAttempt: %v", err)
			}
			if got.Status != store.AttemptStatusRunning || got.Message != "stale" {
				t.Fatalf("attempt = %s %q, want running \"stale\"", got.Status, got.Message)
			}
		})
	}
}

func TestInjectClaim_OnAClosedAttempt(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			sub := oneTaskJob(t, st, store.TaskStatusSucceeded)
			storetest.Submit(t, st, sub)
			now := time.Now().UTC()
			inj := storetest.InjectorFor(t, st)
			a, err := inj.InjectTaskAttempt(t.Context(), store.TaskAttempt{
				ID: uuid.NewString(), TaskID: sub.Tasks[0].ID, WorkerID: "w1", AttemptNumber: 1,
				Status: store.AttemptStatusSucceeded, StartedAt: now, EndedAt: &now, CreatedAt: now,
			})
			if err != nil {
				t.Fatalf("InjectTaskAttempt: %v", err)
			}
			pool, err := st.CreateUsagePool(t.Context(), store.UsagePool{ID: uuid.NewString(), Name: "p", MaxConcurrent: 1})
			if err != nil {
				t.Fatalf("CreateUsagePool: %v", err)
			}
			if _, err := inj.InjectClaim(t.Context(), store.UsageClaim{ID: uuid.NewString(), PoolID: pool.ID, TaskAttemptID: a.ID}); err != nil {
				t.Fatalf("InjectClaim: %v", err)
			}
			if n, err := st.ActiveClaimCount(t.Context(), pool.ID); err != nil || n != 1 {
				t.Fatalf("ActiveClaimCount = %d, %v; want the leaked claim counted", n, err)
			}
		})
	}
}

func TestInjectClaim_SQLiteKeepsForeignKeys(t *testing.T) {
	st := newStores(t)["sqlite"]
	pool, err := st.CreateUsagePool(t.Context(), store.UsagePool{ID: uuid.NewString(), Name: "p", MaxConcurrent: 1})
	if err != nil {
		t.Fatalf("CreateUsagePool: %v", err)
	}
	_, err = storetest.InjectorFor(t, st).InjectClaim(t.Context(),
		store.UsageClaim{ID: uuid.NewString(), PoolID: pool.ID, TaskAttemptID: "no-such-attempt"})
	if err == nil {
		t.Fatal("InjectClaim with a missing attempt succeeded on SQLite; want a foreign-key error")
	}
}

// wrapper embeds store.Store the way the scheduler's checked and racing stores
// do, so it satisfies store.Store but not Injector.
type wrapper struct{ store.Store }

func TestAsInjector_WrapperIsNotAnInjector(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			if _, ok := storetest.AsInjector(st); !ok {
				t.Fatalf("%T is not an Injector", st)
			}
			if _, ok := storetest.AsInjector(wrapper{st}); ok {
				t.Fatal("a wrapper embedding store.Store must not be an Injector")
			}
		})
	}
}
