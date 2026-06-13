// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Tests for the heartbeat-timeout sweep in scheduler.go: sweepStaleWorkers.
// White-box tests in package scheduler driven by a fake store. A real metrics
// registry is used because sweepStaleWorkers refreshes the WorkersTotal gauge.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// seedStaleWorkerWithTask registers an online worker whose last heartbeat is
// older than the given cutoff age, plus an assigned task and a running attempt
// on that worker. Returns the worker and task IDs.
func seedStaleWorkerWithTask(t *testing.T, st *fake.Store, age time.Duration) (workerID, taskID, attemptID string) {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC()
	stale := now.Add(-age)

	workerID = "w-stale"
	if _, err := st.RegisterWorker(ctx, store.Worker{
		ID: workerID, FarmID: "farm-1", Hostname: "node-stale",
		Status: store.WorkerStatusOnline, LastHeartbeatAt: &stale,
	}); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}

	job, err := st.CreateJob(ctx, store.Job{
		ID: uuid.NewString(), FarmID: "farm-1", QueueID: "queue-1", Name: "j",
		Status: store.JobStatusRunning, TemplateFormat: store.TemplateFormatJSON,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	step, err := st.CreateStep(ctx, store.Step{
		ID: uuid.NewString(), JobID: job.ID, Name: "s",
		Status: store.StepStatusRunning, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}
	task, err := st.CreateTask(ctx, store.Task{
		ID: uuid.NewString(), JobID: job.ID, StepID: step.ID, Name: "t",
		Status: store.TaskStatusRunning, AssignedWorkerID: workerID,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	taskID = task.ID

	attempt, err := st.CreateTaskAttempt(ctx, store.TaskAttempt{
		ID: uuid.NewString(), TaskID: task.ID, WorkerID: workerID, AttemptNumber: 1,
		Status: store.AttemptStatusRunning, StartedAt: now, CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateTaskAttempt: %v", err)
	}
	attemptID = attempt.ID
	return workerID, taskID, attemptID
}

func TestSweepStaleWorkers_ReclaimsAndTerminates(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "farm-1")
	// WorkerTimeout default is 30s; age the heartbeat well beyond it.
	workerID, taskID, attemptID := seedStaleWorkerWithTask(t, st, time.Hour)

	s.sweepStaleWorkers(t.Context())

	// Worker marked offline.
	w, err := st.GetWorker(t.Context(), workerID)
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if w.Status != store.WorkerStatusOffline {
		t.Errorf("worker status = %q, want offline", w.Status)
	}

	// Running attempt closed as failed with an end time.
	att, err := st.GetTaskAttempt(t.Context(), attemptID)
	if err != nil {
		t.Fatalf("GetTaskAttempt: %v", err)
	}
	if att.Status != store.AttemptStatusFailed {
		t.Errorf("attempt status = %q, want failed", att.Status)
	}
	if att.EndedAt == nil {
		t.Error("expected attempt EndedAt set")
	}

	// Task reclaimed back to ready, worker reference cleared.
	tk, err := st.GetTask(t.Context(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if tk.Status != store.TaskStatusReady {
		t.Errorf("task status = %q, want ready (reclaimed)", tk.Status)
	}
	if tk.AssignedWorkerID != "" {
		t.Errorf("task still assigned to %q, want cleared", tk.AssignedWorkerID)
	}
}

func TestSweepStaleWorkers_NoStaleWorkers_NoOp(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "farm-1")

	// Fresh worker: heartbeat now, well within the timeout window.
	now := time.Now().UTC()
	if _, err := st.RegisterWorker(t.Context(), store.Worker{
		ID: "w-fresh", FarmID: "farm-1", Status: store.WorkerStatusOnline, LastHeartbeatAt: &now,
	}); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}

	s.sweepStaleWorkers(t.Context())

	w, err := st.GetWorker(t.Context(), "w-fresh")
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if w.Status != store.WorkerStatusOnline {
		t.Errorf("fresh worker status = %q, want online (not swept)", w.Status)
	}
}

// listStaleErrSt makes ListStaleWorkers fail to exercise the early-return path.
type listStaleErrSt struct {
	store.Store
}

func (*listStaleErrSt) ListStaleWorkers(_ context.Context, _ time.Time) ([]store.Worker, error) {
	return nil, context.DeadlineExceeded
}

func TestSweepStaleWorkers_ListError_ReturnsQuietly(t *testing.T) {
	st := &listStaleErrSt{Store: fake.New()}
	s := newMetricsScheduler(st, &recordBus{}, "farm-1")

	// Should not panic and should simply return on the store error.
	s.sweepStaleWorkers(t.Context())
}
