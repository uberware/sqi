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
			storetest.Running(t, st, store.LeaseRequest{TaskID: sub.Tasks[0].ID, WorkerID: "w1"})
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
			in := storetest.InjectAttempt(t, st, store.TaskAttempt{
				TaskID: sub.Tasks[0].ID, WorkerID: "w1", AttemptNumber: 1,
				Status: store.AttemptStatusRunning, Message: "stale",
			})
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
			a := storetest.InjectAttempt(t, st, store.TaskAttempt{
				TaskID: sub.Tasks[0].ID, WorkerID: "w1", AttemptNumber: 1,
				Status: store.AttemptStatusSucceeded, EndedAt: &now,
			})
			pool, err := st.CreateUsagePool(t.Context(), store.UsagePool{ID: uuid.NewString(), Name: "p", MaxConcurrent: 1})
			if err != nil {
				t.Fatalf("CreateUsagePool: %v", err)
			}
			storetest.InjectClaim(t, st, store.UsageClaim{PoolID: pool.ID, TaskAttemptID: a.ID})
			if n, err := st.ActiveClaimCount(t.Context(), pool.ID); err != nil || n != 1 {
				t.Fatalf("ActiveClaimCount = %d, %v; want the leaked claim counted", n, err)
			}
			if v := storetest.ClaimViolations(t, st); len(v) != 1 {
				t.Fatalf("ClaimViolations = %v, want the leaked claim reported", v)
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

var (
	_ storetest.Injector         = (*sqlite.Store)(nil)
	_ storetest.Injector         = (*fake.Store)(nil)
	_ storetest.InvariantChecker = (*sqlite.Store)(nil)
	_ storetest.InvariantChecker = (*fake.Store)(nil)
	_ storetest.AuditReader      = (*sqlite.Store)(nil)
	_ storetest.AuditReader      = (*fake.Store)(nil)
)

func TestSubmitLeasing_LeasesInFlightTasks(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			sub := oneTaskJob(t, st, store.TaskStatusRunning)
			sub.Tasks = append(
				sub.Tasks,
				store.Task{ID: uuid.NewString(), JobID: sub.Job.ID, StepID: sub.Steps[0].ID, Name: "a", Status: store.TaskStatusAssigned},
				store.Task{ID: uuid.NewString(), JobID: sub.Job.ID, StepID: sub.Steps[0].ID, Name: "r", Status: store.TaskStatusReady},
			)
			out, attempts := storetest.SubmitLeasing(t, st, sub, storetest.LeaseTo("w1"))
			want := []store.TaskStatus{store.TaskStatusRunning, store.TaskStatusAssigned, store.TaskStatusReady}
			for i, task := range out.Tasks {
				if task.Status != want[i] {
					t.Errorf("task %d status = %s, want %s", i, task.Status, want[i])
				}
			}
			if len(attempts) != 2 || attempts[out.Tasks[0].ID].WorkerID != "w1" || attempts[out.Tasks[1].ID].WorkerID != "w1" {
				t.Fatalf("attempts = %+v, want one on w1 for each in-flight task", attempts)
			}
		})
	}
}

func TestComplete_EndsTaskAndAttempt(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			sub := oneTaskJob(t, st, store.TaskStatusReady)
			storetest.Submit(t, st, sub)
			a := storetest.Running(t, st, store.LeaseRequest{TaskID: sub.Tasks[0].ID, WorkerID: "w1"})
			storetest.Complete(t, st, a, store.TaskStatusSucceeded)
			got, err := st.GetTaskAttempt(t.Context(), a.ID)
			if err != nil {
				t.Fatalf("GetTaskAttempt: %v", err)
			}
			if got.Status != store.AttemptStatusSucceeded {
				t.Fatalf("attempt status = %s, want succeeded", got.Status)
			}
		})
	}
}

func TestFailAndRequeue_LeavesTheTaskReady(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			sub := oneTaskJob(t, st, store.TaskStatusReady)
			storetest.Submit(t, st, sub)
			a := storetest.FailAndRequeue(t, st, store.LeaseRequest{TaskID: sub.Tasks[0].ID, WorkerID: "w1"}, time.Now().UTC())
			task, err := st.GetTask(t.Context(), a.TaskID)
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			if task.Status != store.TaskStatusReady {
				t.Fatalf("task status = %s, want ready", task.Status)
			}
		})
	}
}

func TestInjectors_KeepGivenTimestamps(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			sub := oneTaskJob(t, st, store.TaskStatusSucceeded)
			storetest.Submit(t, st, sub)
			then := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
			a := storetest.InjectAttempt(t, st, store.TaskAttempt{
				TaskID: sub.Tasks[0].ID, WorkerID: "w1", AttemptNumber: 1,
				Status: store.AttemptStatusRunning, CreatedAt: then,
			})
			if !a.CreatedAt.Equal(then) {
				t.Errorf("attempt CreatedAt = %v, want %v", a.CreatedAt, then)
			}
			pool, err := st.CreateUsagePool(t.Context(), store.UsagePool{ID: uuid.NewString(), Name: "p", MaxConcurrent: 1})
			if err != nil {
				t.Fatalf("CreateUsagePool: %v", err)
			}
			c := storetest.InjectClaim(t, st, store.UsageClaim{PoolID: pool.ID, TaskAttemptID: a.ID, ClaimedAt: then, ReleasedAt: &then})
			if !c.ClaimedAt.Equal(then) || c.ReleasedAt != nil {
				t.Errorf("claim = claimed %v released %v, want claimed %v and active", c.ClaimedAt, c.ReleasedAt, then)
			}
			if n, err := st.ActiveClaimCount(t.Context(), pool.ID); err != nil || n != 1 {
				t.Fatalf("ActiveClaimCount = %d, %v; want the injected claim active", n, err)
			}
		})
	}
}

func TestJobSeed_BuildsAndLeasesAWholeGraph(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			farm, err := st.CreateFarm(t.Context(), store.Farm{ID: uuid.NewString(), Name: uuid.NewString()})
			if err != nil {
				t.Fatalf("CreateFarm: %v", err)
			}
			queue, err := st.CreateQueue(t.Context(), store.Queue{ID: uuid.NewString(), FarmID: farm.ID, Name: uuid.NewString()})
			if err != nil {
				t.Fatalf("CreateQueue: %v", err)
			}
			job := uuid.NewString()
			out, attempts := storetest.NewJob(job, farm.ID, queue.ID).As(store.JobStatusRunning).
				Step(job+"-a", store.StepStatusReady).
				Task(job+"-t1", job+"-a", store.TaskStatusRunning).
				Task(job+"-t2", job+"-b", store.TaskStatusReady).
				SubmitLeasing(t, st, "w1")
			if len(out.Steps) != 2 || out.Steps[1].Status != store.StepStatusPending || out.Steps[1].StepOrder != 1 {
				t.Fatalf("steps = %+v, want the task's missing step added pending, second", out.Steps)
			}
			if out.Tasks[0].Status != store.TaskStatusRunning || attempts[out.Tasks[0].ID].WorkerID != "w1" {
				t.Fatalf("task 0 = %s, attempts %+v; want running on w1", out.Tasks[0].Status, attempts)
			}
		})
	}
}
