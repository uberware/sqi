// SPDX-License-Identifier: AGPL-3.0-or-later

// Package sqlite_test provides table-driven integration tests for every
// [Store] method against an in-memory SQLite instance.
package sqlite_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/sqlite"
)

// ── test helpers ──────────────────────────────────────────────────────────────

// openTestStore creates a new in-memory SQLite store with auto-migration
// enabled. It registers t.Cleanup to close the store when the test ends.
func openTestStore(t *testing.T) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(context.Background(), ":memory:", sqlite.DefaultOptions())
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := s.Close(); closeErr != nil {
			t.Errorf("store.Close: %v", closeErr)
		}
	})
	return s
}

// ── fixtures ──────────────────────────────────────────────────────────────────

// insertFarm inserts a minimal farm and returns the created record.
func insertFarm(t *testing.T, s *sqlite.Store, id, name string) store.Farm {
	t.Helper()
	f, err := s.CreateFarm(context.Background(), store.Farm{
		ID:   id,
		Name: name,
	})
	if err != nil {
		t.Fatalf("CreateFarm(%q): %v", id, err)
	}
	return f
}

// insertQueue inserts a minimal queue and returns the created record.
func insertQueue(t *testing.T, s *sqlite.Store, id, farmID, name string) store.Queue {
	t.Helper()
	q, err := s.CreateQueue(context.Background(), store.Queue{
		ID:     id,
		FarmID: farmID,
		Name:   name,
	})
	if err != nil {
		t.Fatalf("CreateQueue(%q): %v", id, err)
	}
	return q
}

// insertWorker inserts a minimal worker and returns the created record.
func insertWorker(t *testing.T, s *sqlite.Store, id, farmID string) store.Worker {
	t.Helper()
	w, err := s.RegisterWorker(context.Background(), store.Worker{
		ID:       id,
		FarmID:   farmID,
		Hostname: "host-" + id,
		Status:   store.WorkerStatusOnline,
		Tags:     map[string]string{},
	})
	if err != nil {
		t.Fatalf("RegisterWorker(%q): %v", id, err)
	}
	return w
}

// insertJob inserts a minimal job and returns the created record.
func insertJob(t *testing.T, s *sqlite.Store, id, farmID, queueID string) store.Job {
	t.Helper()
	j, err := s.CreateJob(context.Background(), store.Job{
		ID:             id,
		FarmID:         farmID,
		QueueID:        queueID,
		Name:           "job-" + id,
		Status:         store.JobStatusPending,
		Priority:       50,
		TemplateFormat: store.TemplateFormatYAML,
	})
	if err != nil {
		t.Fatalf("CreateJob(%q): %v", id, err)
	}
	return j
}

// insertStep inserts a minimal step and returns the created record.
func insertStep(t *testing.T, s *sqlite.Store, id, jobID, name string, order int) store.Step {
	t.Helper()
	step, err := s.CreateStep(context.Background(), store.Step{
		ID:        id,
		JobID:     jobID,
		Name:      name,
		StepOrder: order,
		Status:    store.StepStatusPending,
		DependsOn: []string{},
	})
	if err != nil {
		t.Fatalf("CreateStep(%q): %v", id, err)
	}
	return step
}

// insertTask inserts a minimal task and returns the created record.
func insertTask(t *testing.T, s *sqlite.Store, id, jobID, stepID string) store.Task {
	t.Helper()
	task, err := s.CreateTask(context.Background(), store.Task{
		ID:         id,
		JobID:      jobID,
		StepID:     stepID,
		Name:       "task-" + id,
		Status:     store.TaskStatusPending,
		Parameters: map[string]string{},
	})
	if err != nil {
		t.Fatalf("CreateTask(%q): %v", id, err)
	}
	return task
}

// ── Farm CRUD ─────────────────────────────────────────────────────────────────

func TestFarm_CreateAndGet(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	f := store.Farm{ID: "f1", Name: "Barn", Description: "test", MaxConcurrentTasks: 10}
	got, err := s.CreateFarm(ctx, f)
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	if got.Name != f.Name {
		t.Errorf("Name: got %q, want %q", got.Name, f.Name)
	}
	if got.MaxConcurrentTasks != 10 {
		t.Errorf("MaxConcurrentTasks: got %d", got.MaxConcurrentTasks)
	}

	fetched, err := s.GetFarm(ctx, "f1")
	if err != nil {
		t.Fatalf("GetFarm: %v", err)
	}
	if fetched.ID != "f1" {
		t.Errorf("ID: got %q", fetched.ID)
	}
}

func TestFarm_GetNotFound(t *testing.T) {
	s := openTestStore(t)
	_, err := s.GetFarm(context.Background(), "nope")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestFarm_Conflict(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "Farm1")
	_, err := s.CreateFarm(ctx, store.Farm{ID: "f2", Name: "Farm1"})
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("expected ErrConflict for duplicate name, got %v", err)
	}
}

func TestFarm_List(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "Alpha")
	insertFarm(t, s, "f2", "Beta")

	farms, err := s.ListFarms(ctx)
	if err != nil {
		t.Fatalf("ListFarms: %v", err)
	}
	if len(farms) != 2 {
		t.Fatalf("want 2 farms, got %d", len(farms))
	}
	// ListFarms orders by name.
	if farms[0].Name != "Alpha" {
		t.Errorf("farms[0].Name: got %q", farms[0].Name)
	}
}

func TestFarm_Update(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "OldName")

	updated, err := s.UpdateFarm(ctx, store.Farm{ID: "f1", Name: "NewName", Description: "updated"})
	if err != nil {
		t.Fatalf("UpdateFarm: %v", err)
	}
	if updated.Name != "NewName" {
		t.Errorf("Name: got %q", updated.Name)
	}
}

func TestFarm_Delete(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")

	if err := s.DeleteFarm(ctx, "f1"); err != nil {
		t.Fatalf("DeleteFarm: %v", err)
	}
	_, err := s.GetFarm(ctx, "f1")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestFarm_DeleteNotFound(t *testing.T) {
	s := openTestStore(t)
	err := s.DeleteFarm(context.Background(), "nope")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// ── Queue CRUD ────────────────────────────────────────────────────────────────

func TestQueue_CreateAndGet(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")

	q, err := s.CreateQueue(ctx, store.Queue{
		ID:                 "q1",
		FarmID:             "f1",
		Name:               "Default",
		Priority:           50,
		MaxConcurrentTasks: 5,
	})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	if q.Name != "Default" {
		t.Errorf("Name: got %q", q.Name)
	}

	got, err := s.GetQueue(ctx, "q1")
	if err != nil {
		t.Fatalf("GetQueue: %v", err)
	}
	if got.MaxConcurrentTasks != 5 {
		t.Errorf("MaxConcurrentTasks: got %d", got.MaxConcurrentTasks)
	}
}

func TestQueue_GetNotFound(t *testing.T) {
	s := openTestStore(t)
	_, err := s.GetQueue(context.Background(), "nope")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestQueue_Update(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")

	updated, err := s.UpdateQueue(ctx, store.Queue{ID: "q1", FarmID: "f1", Name: "Q1-updated", Priority: 80, Paused: true})
	if err != nil {
		t.Fatalf("UpdateQueue: %v", err)
	}
	if !updated.Paused {
		t.Error("Paused: want true")
	}
	if updated.Priority != 80 {
		t.Errorf("Priority: got %d", updated.Priority)
	}
}

func TestQueue_Delete(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")

	if err := s.DeleteQueue(ctx, "q1"); err != nil {
		t.Fatalf("DeleteQueue: %v", err)
	}
	_, err := s.GetQueue(ctx, "q1")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound after delete, got %v", err)
	}
}

// ── Worker CRUD ───────────────────────────────────────────────────────────────

func TestWorker_RegisterAndGet(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")

	w := store.Worker{
		ID:              "w1",
		FarmID:          "f1",
		Name:            "render-node-alpha",
		Hostname:        "host1",
		IPAddress:       "10.0.0.1",
		ComputeLocation: "onprem",
		OS:              "linux",
		OSVersion:       "6.1",
		CPUCount:        16,
		RAMMb:           32768,
		GPUInfo:         store.GPUInfo{Vendor: "NVIDIA", Model: "RTX4090", VRAMMb: 24576, Count: 1},
		Tags:            map[string]string{"env": "prod"},
		Status:          store.WorkerStatusOnline,
	}
	got, err := s.RegisterWorker(ctx, w)
	if err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	if got.CPUCount != 16 {
		t.Errorf("CPUCount: got %d", got.CPUCount)
	}
	if got.Name != "render-node-alpha" {
		t.Errorf("Name: got %q, want render-node-alpha", got.Name)
	}
	if got.Tags["env"] != "prod" {
		t.Errorf("Tags[env]: got %q", got.Tags["env"])
	}

	fetched, err := s.GetWorker(ctx, "w1")
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if fetched.Name != "render-node-alpha" {
		t.Errorf("GetWorker Name: got %q, want render-node-alpha", fetched.Name)
	}
	if fetched.GPUInfo.Model != "RTX4090" {
		t.Errorf("GPUInfo.Model: got %q", fetched.GPUInfo.Model)
	}
}

func TestWorker_GetNotFound(t *testing.T) {
	s := openTestStore(t)
	_, err := s.GetWorker(context.Background(), "nope")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestWorker_Upsert(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")

	w1, err := s.RegisterWorker(ctx, store.Worker{ID: "w1", FarmID: "f1", Hostname: "a", Status: store.WorkerStatusOnline, Tags: map[string]string{}})
	if err != nil {
		t.Fatalf("first register: %v", err)
	}
	// Re-register with updated hostname — should update, not fail.
	w2, err := s.RegisterWorker(ctx, store.Worker{ID: "w1", FarmID: "f1", Hostname: "b", Status: store.WorkerStatusOnline, Tags: map[string]string{}})
	if err != nil {
		t.Fatalf("upsert register: %v", err)
	}
	if w2.Hostname != "b" {
		t.Errorf("Hostname after upsert: got %q", w2.Hostname)
	}
	// RegisteredAt must be preserved.
	if !w2.RegisteredAt.Equal(w1.RegisteredAt) {
		t.Errorf("RegisteredAt changed: was %v, now %v", w1.RegisteredAt, w2.RegisteredAt)
	}
}

func TestWorker_UpdateStatus(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertWorker(t, s, "w1", "f1")

	if err := s.UpdateWorkerStatus(ctx, "w1", store.WorkerStatusOffline); err != nil {
		t.Fatalf("UpdateWorkerStatus: %v", err)
	}
	w, err := s.GetWorker(ctx, "w1")
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if w.Status != store.WorkerStatusOffline {
		t.Errorf("Status: got %q", w.Status)
	}
}

func TestWorker_UpdateHeartbeat(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertWorker(t, s, "w1", "f1")

	ts := time.Now().Add(-5 * time.Second).UTC().Truncate(time.Second)
	if err := s.UpdateWorkerHeartbeat(ctx, "w1", ts); err != nil {
		t.Fatalf("UpdateWorkerHeartbeat: %v", err)
	}
	w, err := s.GetWorker(ctx, "w1")
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if w.LastHeartbeatAt == nil {
		t.Fatal("LastHeartbeatAt: nil")
	}
	if !w.LastHeartbeatAt.Equal(ts) {
		t.Errorf("LastHeartbeatAt: got %v, want %v", w.LastHeartbeatAt, ts)
	}
}

func TestWorker_ListStale(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertWorker(t, s, "w1", "f1")
	insertWorker(t, s, "w2", "f1")

	old := time.Now().Add(-2 * time.Minute)
	recent := time.Now().Add(-10 * time.Second)

	if err := s.UpdateWorkerHeartbeat(ctx, "w1", old); err != nil {
		t.Fatalf("UpdateWorkerHeartbeat w1: %v", err)
	}
	if err := s.UpdateWorkerHeartbeat(ctx, "w2", recent); err != nil {
		t.Fatalf("UpdateWorkerHeartbeat w2: %v", err)
	}

	// Stale = heartbeat before now-1min.
	stale, err := s.ListStaleWorkers(ctx, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("ListStaleWorkers: %v", err)
	}
	if len(stale) != 1 || stale[0].ID != "w1" {
		t.Errorf("stale workers: got %v, want [w1]", stale)
	}
}

func TestWorker_Delete(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertWorker(t, s, "w1", "f1")

	if err := s.DeleteWorker(ctx, "w1"); err != nil {
		t.Fatalf("DeleteWorker: %v", err)
	}
	if _, err := s.GetWorker(ctx, "w1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetWorker after delete: got %v, want ErrNotFound", err)
	}
}

func TestWorker_Delete_NotFound(t *testing.T) {
	s := openTestStore(t)
	if err := s.DeleteWorker(context.Background(), "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DeleteWorker(missing): got %v, want ErrNotFound", err)
	}
}

// TestWorker_Delete_WithAttemptHistory proves migration 00012 relaxed the
// foreign keys: a worker referenced by tasks (assigned_worker_id) and
// task_attempts (worker_id) can still be hard-deleted, and those rows survive.
func TestWorker_Delete_WithAttemptHistory(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertWorker(t, s, "w1", "f1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "step", 0)
	if _, err := s.CreateTask(ctx, store.Task{
		ID: "t1", JobID: "j1", StepID: "s1", Name: "task-t1",
		Status: store.TaskStatusReady, Parameters: map[string]string{},
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// Point a task (assigned_worker_id) and an attempt (worker_id) at the worker.
	if err := s.AssignTask(ctx, "t1", "w1", time.Now().UTC()); err != nil {
		t.Fatalf("AssignTask: %v", err)
	}
	if _, err := s.CreateTaskAttempt(ctx, store.TaskAttempt{
		ID:            "a1",
		TaskID:        "t1",
		WorkerID:      "w1",
		AttemptNumber: 1,
		Status:        store.AttemptStatusRunning,
		StartedAt:     time.Now().UTC(),
		CreatedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateTaskAttempt: %v", err)
	}

	if err := s.DeleteWorker(ctx, "w1"); err != nil {
		t.Fatalf("DeleteWorker with attempt history: %v", err)
	}

	// The attempt row (and its worker_id snapshot) must survive the deletion.
	att, err := s.GetTaskAttempt(ctx, "a1")
	if err != nil {
		t.Fatalf("GetTaskAttempt after worker delete: %v", err)
	}
	if att.WorkerID != "w1" {
		t.Errorf("attempt WorkerID snapshot: got %q, want w1", att.WorkerID)
	}
}

func TestWorker_DeleteOfflineWorkersBefore(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")

	old := time.Now().Add(-2 * time.Hour)
	recent := time.Now().Add(-1 * time.Minute)

	// w1: offline + stale heartbeat → removed.
	insertWorker(t, s, "w1", "f1")
	if err := s.UpdateWorkerHeartbeat(ctx, "w1", old); err != nil {
		t.Fatalf("heartbeat w1: %v", err)
	}
	if err := s.UpdateWorkerStatus(ctx, "w1", store.WorkerStatusOffline); err != nil {
		t.Fatalf("status w1: %v", err)
	}
	// w2: offline but recent heartbeat → kept.
	insertWorker(t, s, "w2", "f1")
	if err := s.UpdateWorkerHeartbeat(ctx, "w2", recent); err != nil {
		t.Fatalf("heartbeat w2: %v", err)
	}
	if err := s.UpdateWorkerStatus(ctx, "w2", store.WorkerStatusOffline); err != nil {
		t.Fatalf("status w2: %v", err)
	}
	// w3: disabled + stale heartbeat → kept (status filter protects it).
	insertWorker(t, s, "w3", "f1")
	if err := s.UpdateWorkerHeartbeat(ctx, "w3", old); err != nil {
		t.Fatalf("heartbeat w3: %v", err)
	}
	if err := s.UpdateWorkerStatus(ctx, "w3", store.WorkerStatusDisabled); err != nil {
		t.Fatalf("status w3: %v", err)
	}

	removed, err := s.DeleteOfflineWorkersBefore(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("DeleteOfflineWorkersBefore: %v", err)
	}
	if len(removed) != 1 || removed[0].ID != "w1" {
		t.Fatalf("removed: got %v, want [w1]", removed)
	}
	for _, id := range []string{"w2", "w3"} {
		if _, err := s.GetWorker(ctx, id); err != nil {
			t.Errorf("worker %s should survive: %v", id, err)
		}
	}
	if _, err := s.GetWorker(ctx, "w1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("worker w1 should be gone: got %v", err)
	}
}

func TestWorker_CountIdleWorkers(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertFarm(t, s, "f2", "F2")
	insertWorker(t, s, "w1", "f1")
	insertWorker(t, s, "w2", "f1")
	insertWorker(t, s, "w3", "f2")

	n, err := s.CountIdleWorkers(ctx, "f1")
	if err != nil {
		t.Fatalf("CountIdleWorkers: %v", err)
	}
	if n != 2 {
		t.Errorf("f1 idle: got %d, want 2", n)
	}

	all, err := s.CountIdleWorkers(ctx, "")
	if err != nil {
		t.Fatalf("CountIdleWorkers all: %v", err)
	}
	if all != 3 {
		t.Errorf("all idle: got %d, want 3", all)
	}
}

// ── Job CRUD ──────────────────────────────────────────────────────────────────

func TestJob_CreateAndGet(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")

	j, err := s.CreateJob(ctx, store.Job{
		ID:             "j1",
		FarmID:         "f1",
		QueueID:        "q1",
		Name:           "MyJob",
		Owner:          "alice",
		Priority:       75,
		Status:         store.JobStatusPending,
		TemplateFormat: store.TemplateFormatYAML,
		RawTemplate:    "specificationVersion: jobtemplate-2023-09",
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if j.Priority != 75 {
		t.Errorf("Priority: got %d", j.Priority)
	}

	got, err := s.GetJob(ctx, "j1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Owner != "alice" {
		t.Errorf("Owner: got %q", got.Owner)
	}
}

func TestJob_GetNotFound(t *testing.T) {
	s := openTestStore(t)
	_, err := s.GetJob(context.Background(), "nope")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestJob_Parameters_RoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")

	want := map[string]string{"Frame": "42", "Quality": "high"}
	j, err := s.CreateJob(ctx, store.Job{
		ID:             "j1",
		FarmID:         "f1",
		QueueID:        "q1",
		Name:           "ParamJob",
		Status:         store.JobStatusPending,
		Priority:       50,
		TemplateFormat: store.TemplateFormatYAML,
		Parameters:     want,
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if len(j.Parameters) != len(want) {
		t.Errorf("CreateJob returned Parameters len %d, want %d", len(j.Parameters), len(want))
	}

	got, err := s.GetJob(ctx, "j1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	for k, v := range want {
		if got.Parameters[k] != v {
			t.Errorf("GetJob Parameters[%q] = %q, want %q", k, got.Parameters[k], v)
		}
	}
}

func TestJob_Parameters_NilRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")

	j, err := s.CreateJob(ctx, store.Job{
		ID:             "j1",
		FarmID:         "f1",
		QueueID:        "q1",
		Name:           "NoParamJob",
		Status:         store.JobStatusPending,
		Priority:       50,
		TemplateFormat: store.TemplateFormatYAML,
		Parameters:     nil,
	})
	if err != nil {
		t.Fatalf("CreateJob with nil Parameters: %v", err)
	}

	got, err := s.GetJob(ctx, j.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	// nil or empty map both acceptable for a job with no parameters
	if len(got.Parameters) != 0 {
		t.Errorf("GetJob Parameters = %v, want nil or empty", got.Parameters)
	}
}

func TestJob_UpdateStatus(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertJob(t, s, "j1", "f1", "q1")

	cases := []struct {
		status       store.JobStatus
		wantStarted  bool
		wantFinished bool
	}{
		{store.JobStatusRunning, true, false},
		{store.JobStatusCompleted, true, true},
	}
	for _, tc := range cases {
		if err := s.UpdateJobStatus(ctx, "j1", tc.status); err != nil {
			t.Fatalf("UpdateJobStatus(%q): %v", tc.status, err)
		}
		j, err := s.GetJob(ctx, "j1")
		if err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		if j.Status != tc.status {
			t.Errorf("Status: got %q, want %q", j.Status, tc.status)
		}
		if tc.wantStarted && j.StartedAt == nil {
			t.Errorf("StartedAt: should be set for status %q", tc.status)
		}
		if tc.wantFinished && j.CompletedAt == nil {
			t.Errorf("CompletedAt: should be set for status %q", tc.status)
		}
	}
}

func TestJob_UpdateStatus_NotFound(t *testing.T) {
	s := openTestStore(t)
	err := s.UpdateJobStatus(context.Background(), "nope", store.JobStatusRunning)
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestJob_ListJobs_FilterByStatus(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertJob(t, s, "j1", "f1", "q1")
	insertJob(t, s, "j2", "f1", "q1")

	if err := s.UpdateJobStatus(ctx, "j2", store.JobStatusRunning); err != nil {
		t.Fatalf("UpdateJobStatus: %v", err)
	}

	page, err := s.ListJobs(ctx, store.ListJobsOptions{Status: store.JobStatusPending})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if page.Total != 1 {
		t.Errorf("Total: got %d, want 1", page.Total)
	}
}

func TestJob_ListJobs_Search(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	mk := func(id, name, owner, project string) {
		if _, err := s.CreateJob(ctx, store.Job{
			ID: id, FarmID: "f1", QueueID: "q1", Name: name, Owner: owner,
			Project: project, Status: store.JobStatusPending, Priority: 50,
			TemplateFormat: store.TemplateFormatYAML,
		}); err != nil {
			t.Fatalf("CreateJob(%q): %v", id, err)
		}
	}
	mk("render-night", "Nightly Render", "alice", "moonshot")
	mk("comp-day", "Daytime Comp", "bob", "sunrise")

	cases := []struct {
		name, search string
		wantTotal    int
	}{
		{"by name (case-insensitive)", "nightly", 1},
		{"by owner", "bob", 1},
		{"by project", "moonshot", 1},
		{"by id substring", "comp-", 1},
		{"no match", "zzz", 0},
		{"empty matches all", "", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page, err := s.ListJobs(ctx, store.ListJobsOptions{Search: tc.search})
			if err != nil {
				t.Fatalf("ListJobs: %v", err)
			}
			if page.Total != tc.wantTotal {
				t.Errorf("Total: got %d, want %d", page.Total, tc.wantTotal)
			}
		})
	}
}

// ── Step CRUD ─────────────────────────────────────────────────────────────────

func TestStep_CreateAndGet(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertJob(t, s, "j1", "f1", "q1")

	step, err := s.CreateStep(ctx, store.Step{
		ID:        "s1",
		JobID:     "j1",
		Name:      "Render",
		StepOrder: 0,
		Status:    store.StepStatusPending,
		DependsOn: []string{},
	})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}
	if step.Name != "Render" {
		t.Errorf("Name: got %q", step.Name)
	}

	got, err := s.GetStep(ctx, "s1")
	if err != nil {
		t.Fatalf("GetStep: %v", err)
	}
	if got.StepOrder != 0 {
		t.Errorf("StepOrder: got %d", got.StepOrder)
	}
}

func TestStep_ListSteps_OrderByStepOrder(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s2", "j1", "Composite", 1)
	insertStep(t, s, "s1", "j1", "Render", 0)

	steps, err := s.ListSteps(ctx, "j1")
	if err != nil {
		t.Fatalf("ListSteps: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("want 2 steps, got %d", len(steps))
	}
	if steps[0].Name != "Render" {
		t.Errorf("steps[0]: got %q, want Render", steps[0].Name)
	}
}

func TestStep_UpdateStatus(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)

	if err := s.UpdateStepStatus(ctx, "s1", store.StepStatusRunning); err != nil {
		t.Fatalf("UpdateStepStatus: %v", err)
	}
	step, err := s.GetStep(ctx, "s1")
	if err != nil {
		t.Fatalf("GetStep: %v", err)
	}
	if step.Status != store.StepStatusRunning {
		t.Errorf("Status: got %q", step.Status)
	}
}

// ── Task CRUD ─────────────────────────────────────────────────────────────────

func TestTask_CreateAndGet(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)

	task, err := s.CreateTask(ctx, store.Task{
		ID:         "t1",
		JobID:      "j1",
		StepID:     "s1",
		Name:       "frame-001",
		Status:     store.TaskStatusReady,
		Parameters: map[string]string{"Frame": "1"},
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if task.Parameters["Frame"] != "1" {
		t.Errorf("Parameters[Frame]: got %q", task.Parameters["Frame"])
	}

	got, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != store.TaskStatusReady {
		t.Errorf("Status: got %q", got.Status)
	}
}

func TestTask_GetNotFound(t *testing.T) {
	s := openTestStore(t)
	_, err := s.GetTask(context.Background(), "nope")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestTask_UpdateStatus(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)
	insertTask(t, s, "t1", "j1", "s1")

	if err := s.UpdateTaskStatus(ctx, "t1", store.TaskStatusRunning); err != nil {
		t.Fatalf("UpdateTaskStatus: %v", err)
	}
	task, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != store.TaskStatusRunning {
		t.Errorf("Status: got %q", task.Status)
	}
}

func TestTask_AssignTask(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertWorker(t, s, "w1", "f1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)
	insertTask(t, s, "t1", "j1", "s1")

	at := time.Now().UTC().Truncate(time.Second)
	if err := s.AssignTask(ctx, "t1", "w1", at); err != nil {
		t.Fatalf("AssignTask: %v", err)
	}
	task, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.AssignedWorkerID != "w1" {
		t.Errorf("AssignedWorkerID: got %q", task.AssignedWorkerID)
	}
	if task.Status != store.TaskStatusAssigned {
		t.Errorf("Status: got %q", task.Status)
	}
}

func TestTask_ListReadyTasks(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)

	// Insert two tasks: one ready, one pending.
	_, err := s.CreateTask(ctx, store.Task{
		ID: "t1", JobID: "j1", StepID: "s1",
		Name: "ready-task", Status: store.TaskStatusReady,
		Parameters: map[string]string{},
	})
	if err != nil {
		t.Fatalf("CreateTask t1: %v", err)
	}
	_, err = s.CreateTask(ctx, store.Task{
		ID: "t2", JobID: "j1", StepID: "s1",
		Name: "pending-task", Status: store.TaskStatusPending,
		Parameters: map[string]string{},
	})
	if err != nil {
		t.Fatalf("CreateTask t2: %v", err)
	}

	ready, err := s.ListReadyTasks(ctx, "f1", 10)
	if err != nil {
		t.Fatalf("ListReadyTasks: %v", err)
	}
	if len(ready) != 1 || ready[0].ID != "t1" {
		t.Errorf("ready tasks: got %v, want [t1]", ready)
	}
}

func TestTask_ListReadyTasks_PausedQueue(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	q := store.Queue{ID: "q1", FarmID: "f1", Name: "Q1", Paused: true}
	if _, err := s.CreateQueue(ctx, q); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)
	_, err := s.CreateTask(ctx, store.Task{
		ID: "t1", JobID: "j1", StepID: "s1",
		Name: "t", Status: store.TaskStatusReady,
		Parameters: map[string]string{},
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	ready, err := s.ListReadyTasks(ctx, "f1", 10)
	if err != nil {
		t.Fatalf("ListReadyTasks: %v", err)
	}
	if len(ready) != 0 {
		t.Errorf("paused queue should yield no ready tasks; got %d", len(ready))
	}
}

func TestTask_ReclaimWorkerTasks(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertWorker(t, s, "w1", "f1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)
	insertTask(t, s, "t1", "j1", "s1")
	insertTask(t, s, "t2", "j1", "s1")

	if err := s.AssignTask(ctx, "t1", "w1", time.Now()); err != nil {
		t.Fatalf("AssignTask t1: %v", err)
	}

	n, err := s.ReclaimWorkerTasks(ctx, "w1")
	if err != nil {
		t.Fatalf("ReclaimWorkerTasks: %v", err)
	}
	if n != 1 {
		t.Errorf("reclaimed: got %d, want 1", n)
	}
	task, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != store.TaskStatusReady {
		t.Errorf("Status after reclaim: got %q", task.Status)
	}
	if task.AssignedWorkerID != "" {
		t.Errorf("AssignedWorkerID after reclaim: got %q", task.AssignedWorkerID)
	}
}

func TestTask_ReclaimStaleAssignedTasks(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertWorker(t, s, "w1", "f1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)
	insertTask(t, s, "t1", "j1", "s1") // stale assignment
	insertTask(t, s, "t2", "j1", "s1") // fresh assignment

	if err := s.AssignTask(ctx, "t1", "w1", time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("AssignTask t1: %v", err)
	}
	if err := s.AssignTask(ctx, "t2", "w1", time.Now()); err != nil {
		t.Fatalf("AssignTask t2: %v", err)
	}

	cutoff := time.Now().Add(-30 * time.Minute)
	reclaimed, err := s.ReclaimStaleAssignedTasks(ctx, cutoff)
	if err != nil {
		t.Fatalf("ReclaimStaleAssignedTasks: %v", err)
	}
	if len(reclaimed) != 1 {
		t.Fatalf("reclaimed count: got %d, want 1", len(reclaimed))
	}
	if reclaimed[0].ID != "t1" {
		t.Errorf("reclaimed task: got %q, want t1", reclaimed[0].ID)
	}
	// The returned snapshot must retain the pre-reset worker so the caller can
	// close attempts and release usage claims.
	if reclaimed[0].AssignedWorkerID != "w1" {
		t.Errorf("reclaimed AssignedWorkerID: got %q, want w1", reclaimed[0].AssignedWorkerID)
	}

	t1, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask t1: %v", err)
	}
	if t1.Status != store.TaskStatusReady {
		t.Errorf("t1 status: got %q, want ready", t1.Status)
	}
	if t1.AssignedWorkerID != "" {
		t.Errorf("t1 AssignedWorkerID: got %q, want cleared", t1.AssignedWorkerID)
	}

	t2, err := s.GetTask(ctx, "t2")
	if err != nil {
		t.Fatalf("GetTask t2: %v", err)
	}
	if t2.Status != store.TaskStatusAssigned {
		t.Errorf("t2 status: got %q, want assigned (still fresh)", t2.Status)
	}
}

func TestTask_CountActiveTasksInQueue(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertWorker(t, s, "w1", "f1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)
	insertTask(t, s, "t1", "j1", "s1")
	insertTask(t, s, "t2", "j1", "s1")

	if err := s.AssignTask(ctx, "t1", "w1", time.Now()); err != nil {
		t.Fatalf("AssignTask: %v", err)
	}

	n, err := s.CountActiveTasksInQueue(ctx, "q1")
	if err != nil {
		t.Fatalf("CountActiveTasksInQueue: %v", err)
	}
	if n != 1 {
		t.Errorf("active in q1: got %d, want 1", n)
	}
}

func TestTask_CountActiveTasksInFarm(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertQueue(t, s, "q2", "f1", "Q2")
	insertWorker(t, s, "w1", "f1")
	insertJob(t, s, "j1", "f1", "q1")
	insertJob(t, s, "j2", "f1", "q2")
	insertStep(t, s, "s1", "j1", "S1", 0)
	insertStep(t, s, "s2", "j2", "S2", 0)
	insertTask(t, s, "t1", "j1", "s1")
	insertTask(t, s, "t2", "j2", "s2")

	if err := s.AssignTask(ctx, "t1", "w1", time.Now()); err != nil {
		t.Fatalf("AssignTask: %v", err)
	}
	if err := s.AssignTask(ctx, "t2", "w1", time.Now()); err != nil {
		t.Fatalf("AssignTask: %v", err)
	}

	n, err := s.CountActiveTasksInFarm(ctx, "f1")
	if err != nil {
		t.Fatalf("CountActiveTasksInFarm: %v", err)
	}
	if n != 2 {
		t.Errorf("active in f1: got %d, want 2", n)
	}
}

func TestTask_CountReadyTasksByQueue(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertQueue(t, s, "q2", "f1", "Q2")
	insertJob(t, s, "j1", "f1", "q1")
	insertJob(t, s, "j2", "f1", "q2")
	insertStep(t, s, "s1", "j1", "S1", 0)
	insertStep(t, s, "s2", "j2", "S2", 0)

	for i, id := range []string{"t1", "t2"} {
		_, err := s.CreateTask(ctx, store.Task{
			ID: id, JobID: "j1", StepID: "s1",
			Name: id, Status: store.TaskStatusReady,
			Parameters: map[string]string{"i": string(rune('0' + i))},
		})
		if err != nil {
			t.Fatalf("CreateTask %q: %v", id, err)
		}
	}
	_, err := s.CreateTask(ctx, store.Task{
		ID: "t3", JobID: "j2", StepID: "s2",
		Name: "t3", Status: store.TaskStatusReady,
		Parameters: map[string]string{},
	})
	if err != nil {
		t.Fatalf("CreateTask t3: %v", err)
	}

	counts, err := s.CountReadyTasksByQueue(ctx, "f1")
	if err != nil {
		t.Fatalf("CountReadyTasksByQueue: %v", err)
	}
	if counts["q1"] != 2 {
		t.Errorf("q1: got %d, want 2", counts["q1"])
	}
	if counts["q2"] != 1 {
		t.Errorf("q2: got %d, want 1", counts["q2"])
	}
}

func TestTask_CancelJobTasks(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertWorker(t, s, "w1", "f1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)
	insertTask(t, s, "t1", "j1", "s1") // will be assigned
	insertTask(t, s, "t2", "j1", "s1") // ready

	if err := s.AssignTask(ctx, "t1", "w1", time.Now()); err != nil {
		t.Fatalf("AssignTask: %v", err)
	}
	if err := s.UpdateTaskStatus(ctx, "t2", store.TaskStatusReady); err != nil {
		t.Fatalf("UpdateTaskStatus t2: %v", err)
	}

	active, err := s.CancelJobTasks(ctx, "j1", time.Now())
	if err != nil {
		t.Fatalf("CancelJobTasks: %v", err)
	}
	// Only t1 was assigned → one active task returned.
	if len(active) != 1 || active[0].ID != "t1" {
		t.Errorf("active tasks returned: got %v", active)
	}
	// Both tasks should now be canceled.
	for _, id := range []string{"t1", "t2"} {
		task, err := s.GetTask(ctx, id)
		if err != nil {
			t.Fatalf("GetTask %q: %v", id, err)
		}
		if task.Status != store.TaskStatusCanceled {
			t.Errorf("%s status: got %q, want canceled", id, task.Status)
		}
	}
}

func TestTask_TransitionStepPendingTasks(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)
	insertStep(t, s, "s2", "j1", "S2", 1)
	insertTask(t, s, "t1", "j1", "s1") // pending, step s1
	insertTask(t, s, "t2", "j1", "s1") // pending, step s1
	insertTask(t, s, "t3", "j1", "s2") // pending, but different step

	// A non-pending task in s1 must be left untouched.
	insertTask(t, s, "t4", "j1", "s1")
	if err := s.UpdateTaskStatus(ctx, "t4", store.TaskStatusReady); err != nil {
		t.Fatalf("UpdateTaskStatus t4: %v", err)
	}

	affected, err := s.TransitionStepPendingTasks(ctx, "s1", store.TaskStatusCanceled)
	if err != nil {
		t.Fatalf("TransitionStepPendingTasks: %v", err)
	}

	// Only the two pending tasks of s1 are affected and returned.
	gotIDs := make(map[string]bool)
	for _, task := range affected {
		gotIDs[task.ID] = true
		if task.Status != store.TaskStatusCanceled {
			t.Errorf("returned task %s status = %q, want canceled", task.ID, task.Status)
		}
	}
	if len(affected) != 2 || !gotIDs["t1"] || !gotIDs["t2"] {
		t.Errorf("affected = %v, want exactly t1 and t2", gotIDs)
	}

	// Persisted state matches.
	for id, want := range map[string]store.TaskStatus{
		"t1": store.TaskStatusCanceled,
		"t2": store.TaskStatusCanceled,
		"t3": store.TaskStatusPending, // other step, unchanged
		"t4": store.TaskStatusReady,   // not pending, unchanged
	} {
		task, err := s.GetTask(ctx, id)
		if err != nil {
			t.Fatalf("GetTask %q: %v", id, err)
		}
		if task.Status != want {
			t.Errorf("%s status = %q, want %q", id, task.Status, want)
		}
	}
}

// ── UsagePool CRUD ────────────────────────────────────────────────────────────

func TestUsagePool_CreateAndGet(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	pool, err := s.CreateUsagePool(ctx, store.UsagePool{
		ID:            "p1",
		Name:          "MayaRenderer",
		ServerHint:    "27000@licsrv",
		MaxConcurrent: 5,
	})
	if err != nil {
		t.Fatalf("CreateUsagePool: %v", err)
	}
	if pool.MaxConcurrent != 5 {
		t.Errorf("MaxConcurrent: got %d", pool.MaxConcurrent)
	}

	got, err := s.GetUsagePool(ctx, "p1")
	if err != nil {
		t.Fatalf("GetUsagePool: %v", err)
	}
	if got.Name != "MayaRenderer" {
		t.Errorf("Name: got %q", got.Name)
	}
	if got.ServerHint != "27000@licsrv" {
		t.Errorf("ServerHint: got %q", got.ServerHint)
	}
}

func TestUsagePool_Conflict(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_, err := s.CreateUsagePool(ctx, store.UsagePool{ID: "p1", Name: "X", MaxConcurrent: 1})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err = s.CreateUsagePool(ctx, store.UsagePool{ID: "p2", Name: "X", MaxConcurrent: 2})
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("expected ErrConflict for duplicate name, got %v", err)
	}
}

// Usage-pool names are the trailing segment of a case-insensitive capability
// name (OpenJD jobtemplate-2023-09), so two names differing only in case are the
// same capability and must conflict.
func TestUsagePool_ConflictCaseInsensitive(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateUsagePool(ctx, store.UsagePool{ID: "p1", Name: "maya", MaxConcurrent: 1}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := s.CreateUsagePool(ctx, store.UsagePool{ID: "p2", Name: "Maya", MaxConcurrent: 2})
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("expected ErrConflict for case-insensitive duplicate name, got %v", err)
	}
}

func TestUsagePool_DeleteNotFound(t *testing.T) {
	s := openTestStore(t)
	err := s.DeleteUsagePool(context.Background(), "nope")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// ── Ping ──────────────────────────────────────────────────────────────────────

func TestStore_Ping(t *testing.T) {
	s := openTestStore(t)
	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

// ── ErrNotFound sentinels ─────────────────────────────────────────────────────

func TestErrNotFound_GetWorker(t *testing.T) {
	s := openTestStore(t)
	_, err := s.GetWorker(context.Background(), "missing")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetWorker missing: expected ErrNotFound, got %v", err)
	}
}

func TestErrNotFound_UpdateWorkerStatus(t *testing.T) {
	s := openTestStore(t)
	err := s.UpdateWorkerStatus(context.Background(), "missing", store.WorkerStatusOffline)
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UpdateWorkerStatus missing: expected ErrNotFound, got %v", err)
	}
}

func TestErrNotFound_GetStep(t *testing.T) {
	s := openTestStore(t)
	_, err := s.GetStep(context.Background(), "missing")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetStep missing: expected ErrNotFound, got %v", err)
	}
}

func TestErrNotFound_UpdateTaskStatus(t *testing.T) {
	s := openTestStore(t)
	err := s.UpdateTaskStatus(context.Background(), "missing", store.TaskStatusRunning)
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UpdateTaskStatus missing: expected ErrNotFound, got %v", err)
	}
}

// ── ListJobs sort and pagination ──────────────────────────────────────────────

func TestJob_ListJobs_Pagination(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	for _, id := range []string{"j1", "j2", "j3"} {
		insertJob(t, s, id, "f1", "q1")
	}

	page, err := s.ListJobs(ctx, store.ListJobsOptions{
		Pagination: store.Pagination{Limit: 2, Offset: 0},
	})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if page.Total != 3 {
		t.Errorf("Total: got %d, want 3", page.Total)
	}
	if len(page.Items) != 2 {
		t.Errorf("Items: got %d, want 2", len(page.Items))
	}
}

// ── StorageLocation CRUD ──────────────────────────────────────────────────────

func TestStorageLocation_CRUD(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	loc := store.StorageLocation{
		ID:          "sl1",
		Name:        "nas_shows",
		Type:        store.StorageLocationTypeFilesystem,
		Description: "Main NAS shows volume",
		Roots: map[string]string{
			"default":         "/mnt/nas/shows",
			"windows_workers": `Z:\shows`,
		},
	}

	created, err := s.CreateStorageLocation(ctx, loc)
	if err != nil {
		t.Fatalf("CreateStorageLocation: %v", err)
	}
	if created.ID != "sl1" || created.Name != "nas_shows" {
		t.Errorf("created: got %+v", created)
	}
	if created.Roots["default"] != "/mnt/nas/shows" {
		t.Errorf("Roots[default]: got %q", created.Roots["default"])
	}

	// GetStorageLocation
	got, err := s.GetStorageLocation(ctx, "sl1")
	if err != nil {
		t.Fatalf("GetStorageLocation: %v", err)
	}
	if got.Name != "nas_shows" {
		t.Errorf("Name: got %q", got.Name)
	}

	// GetStorageLocationByName
	byName, err := s.GetStorageLocationByName(ctx, "nas_shows")
	if err != nil {
		t.Fatalf("GetStorageLocationByName: %v", err)
	}
	if byName.ID != "sl1" {
		t.Errorf("byName.ID: got %q", byName.ID)
	}

	// GetStorageLocation not found
	_, err = s.GetStorageLocation(ctx, "missing")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing ID: want ErrNotFound, got %v", err)
	}

	// Conflict on duplicate name
	dup := loc
	dup.ID = "sl2"
	_, err = s.CreateStorageLocation(ctx, dup)
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("duplicate name: want ErrConflict, got %v", err)
	}

	// ListStorageLocations
	loc2 := store.StorageLocation{
		ID:    "sl2",
		Name:  "s3_output",
		Type:  store.StorageLocationTypeS3,
		Roots: map[string]string{"default": "s3://bucket/output"},
	}
	if _, err := s.CreateStorageLocation(ctx, loc2); err != nil {
		t.Fatalf("CreateStorageLocation loc2: %v", err)
	}
	locs, err := s.ListStorageLocations(ctx)
	if err != nil {
		t.Fatalf("ListStorageLocations: %v", err)
	}
	if len(locs) != 2 {
		t.Fatalf("ListStorageLocations: got %d, want 2", len(locs))
	}
	// Ordered by name: nas_shows < s3_output
	if locs[0].Name != "nas_shows" || locs[1].Name != "s3_output" {
		t.Errorf("order: %v, %v", locs[0].Name, locs[1].Name)
	}

	// UpdateStorageLocation
	updated, err := s.UpdateStorageLocation(ctx, store.StorageLocation{
		ID:   "sl1",
		Name: "nas_shows",
		Type: store.StorageLocationTypeFilesystem,
		Roots: map[string]string{
			"default":         "/mnt/nas/shows",
			"windows_workers": `Z:\shows`,
			"cloud_aws":       "s3://studio-bucket/shows",
		},
	})
	if err != nil {
		t.Fatalf("UpdateStorageLocation: %v", err)
	}
	if updated.Roots["cloud_aws"] != "s3://studio-bucket/shows" {
		t.Errorf("updated cloud_aws root: got %q", updated.Roots["cloud_aws"])
	}
	if updated.UpdatedAt.Before(updated.CreatedAt) {
		t.Errorf("UpdatedAt should be >= CreatedAt")
	}

	// DeleteStorageLocation
	if err := s.DeleteStorageLocation(ctx, "sl1"); err != nil {
		t.Fatalf("DeleteStorageLocation: %v", err)
	}
	_, err = s.GetStorageLocation(ctx, "sl1")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("after delete: want ErrNotFound, got %v", err)
	}

	// Delete not found
	if err := s.DeleteStorageLocation(ctx, "sl1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete missing: want ErrNotFound, got %v", err)
	}
}

// ── ListWorkers filter ────────────────────────────────────────────────────────

func TestWorker_ListWorkers_FilterByStatus(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertWorker(t, s, "w1", "f1")
	insertWorker(t, s, "w2", "f1")

	if err := s.UpdateWorkerStatus(ctx, "w2", store.WorkerStatusOffline); err != nil {
		t.Fatalf("UpdateWorkerStatus: %v", err)
	}

	page, err := s.ListWorkers(ctx, store.ListWorkersOptions{Status: store.WorkerStatusOnline})
	if err != nil {
		t.Fatalf("ListWorkers: %v", err)
	}
	if page.Total != 1 {
		t.Errorf("online workers: got %d, want 1", page.Total)
	}
	if page.Items[0].ID != "w1" {
		t.Errorf("Items[0].ID: got %q", page.Items[0].ID)
	}
}

func TestWorker_ListWorkers_Search(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	mk := func(id, name, host, loc string) {
		if _, err := s.RegisterWorker(ctx, store.Worker{
			ID: id, FarmID: "f1", Name: name, Hostname: host,
			ComputeLocation: loc, Status: store.WorkerStatusOnline,
			Tags: map[string]string{},
		}); err != nil {
			t.Fatalf("RegisterWorker(%q): %v", id, err)
		}
	}
	mk("w-render01", "render-node-01", "render01.local", "us-west")
	mk("w-comp02", "comp-node-02", "comp02.local", "eu-central")

	cases := []struct {
		name, search string
		wantTotal    int
	}{
		{"by name", "render-node", 1},
		{"by hostname", "comp02.local", 1},
		{"by id", "w-render01", 1},
		{"by compute location", "eu-central", 1},
		{"case-insensitive", "US-WEST", 1},
		{"no match", "zzz", 0},
		{"empty matches all", "", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page, err := s.ListWorkers(ctx, store.ListWorkersOptions{Search: tc.search})
			if err != nil {
				t.Fatalf("ListWorkers: %v", err)
			}
			if page.Total != tc.wantTotal {
				t.Errorf("Total: got %d, want %d", page.Total, tc.wantTotal)
			}
		})
	}
}

// ── DemoteStalledJobs ─────────────────────────────────────────────────────────

// seedJobWithTasks creates a job (in the given status) with one step and a task
// per status in taskStatuses. Returns the job ID.
func seedJobWithTasks(t *testing.T, s *sqlite.Store, jobID string, jobStatus store.JobStatus, taskStatuses ...store.TaskStatus) string {
	t.Helper()
	ctx := context.Background()
	insertJob(t, s, jobID, "f1", "q1")
	step := insertStep(t, s, jobID+"-step", jobID, "render", 0)
	for i, ts := range taskStatuses {
		if _, err := s.CreateTask(ctx, store.Task{
			ID:         fmt.Sprintf("%s-t%d", jobID, i),
			JobID:      jobID,
			StepID:     step.ID,
			Name:       fmt.Sprintf("task-%d", i),
			Status:     ts,
			Parameters: map[string]string{},
		}); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
	}
	if err := s.UpdateJobStatus(ctx, jobID, jobStatus); err != nil {
		t.Fatalf("UpdateJobStatus(%q): %v", jobStatus, err)
	}
	return jobID
}

func TestDemoteStalledJobs(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")

	const (
		R = store.TaskStatusReady
		A = store.TaskStatusAssigned
		U = store.TaskStatusRunning
		S = store.TaskStatusSucceeded
	)

	// stalled: running job whose only non-terminal work is ready (the recovered
	// frames 41-50 case: 2 succeeded + 1 ready, no in-flight task).
	seedJobWithTasks(t, s, "stalled", store.JobStatusRunning, S, S, R)
	// inflight-running: a running task means the job is genuinely running.
	seedJobWithTasks(t, s, "inflight-running", store.JobStatusRunning, U, R)
	// inflight-assigned: an assigned task also counts as in flight.
	seedJobWithTasks(t, s, "inflight-assigned", store.JobStatusRunning, A, R)
	// all-terminal: no schedulable work left — leave it for completion logic.
	seedJobWithTasks(t, s, "all-terminal", store.JobStatusRunning, S, S)
	// pending: not running, so never a demotion candidate even when idle.
	seedJobWithTasks(t, s, "pending", store.JobStatusPending, R)

	now := time.Now().UTC()
	demoted, err := s.DemoteStalledJobs(ctx, now)
	if err != nil {
		t.Fatalf("DemoteStalledJobs: %v", err)
	}

	if len(demoted) != 1 || demoted[0] != "stalled" {
		t.Fatalf("demoted = %v, want [stalled]", demoted)
	}

	wantStatus := map[string]store.JobStatus{
		"stalled":           store.JobStatusPending,
		"inflight-running":  store.JobStatusRunning,
		"inflight-assigned": store.JobStatusRunning,
		"all-terminal":      store.JobStatusRunning,
		"pending":           store.JobStatusPending,
	}
	for id, want := range wantStatus {
		j, err := s.GetJob(ctx, id)
		if err != nil {
			t.Fatalf("GetJob(%q): %v", id, err)
		}
		if j.Status != want {
			t.Errorf("job %q status = %q, want %q", id, j.Status, want)
		}
	}

	// The demoted job keeps its StartedAt (it did start) and stamps updated_at.
	j, err := s.GetJob(ctx, "stalled")
	if err != nil {
		t.Fatalf("GetJob(stalled): %v", err)
	}
	if j.StartedAt == nil {
		t.Error("demoted job lost StartedAt, want preserved")
	}
	if !j.UpdatedAt.Equal(now) {
		t.Errorf("demoted job UpdatedAt = %v, want %v", j.UpdatedAt, now)
	}

	// Idempotent: a second sweep with the stalled job now pending demotes nothing.
	again, err := s.DemoteStalledJobs(ctx, now)
	if err != nil {
		t.Fatalf("DemoteStalledJobs (2nd): %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second sweep demoted %v, want none", again)
	}
}

func TestCommittedCores(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertJob(t, s, "j1", "f1", "q1")
	step := insertStep(t, s, "s1", "j1", "render", 0)

	mk := func(id string, status store.TaskStatus, cores *int, worker string) {
		tk, err := s.CreateTask(ctx, store.Task{
			ID: id, JobID: "j1", StepID: step.ID, Name: id,
			Status: store.TaskStatusPending, Parameters: map[string]string{},
			RequiredCores: cores,
		})
		if err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		if worker != "" {
			if err := s.AssignTask(ctx, tk.ID, worker, time.Now().UTC()); err != nil {
				t.Fatalf("AssignTask %s: %v", id, err)
			}
			if err := s.UpdateTaskStatus(ctx, tk.ID, status); err != nil {
				t.Fatalf("UpdateTaskStatus %s: %v", id, err)
			}
		}
	}
	insertWorker(t, s, "w1", "f1")
	insertWorker(t, s, "w2", "f1")

	one, two := 1, 2
	mk("a", store.TaskStatusRunning, &two, "w1")   // 2 cores
	mk("b", store.TaskStatusAssigned, &one, "w1")  // 1 core
	mk("c", store.TaskStatusRunning, nil, "w1")    // undeclared -> fullMachineCost (4)
	mk("d", store.TaskStatusSucceeded, &two, "w1") // terminal -> not counted
	mk("e", store.TaskStatusRunning, &one, "w2")   // other worker -> not counted

	got, err := s.CommittedCores(ctx, "w1", 4)
	if err != nil {
		t.Fatalf("CommittedCores: %v", err)
	}
	if got != 2+1+4 { // 7
		t.Errorf("CommittedCores(w1, full=4) = %d, want 7", got)
	}

	zero, err := s.CommittedCores(ctx, "idle", 4)
	if err != nil || zero != 0 {
		t.Errorf("CommittedCores(idle) = %d, %v, want 0, nil", zero, err)
	}
}

func TestTask_RequiredCoresRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertJob(t, s, "j1", "f1", "q1")
	step := insertStep(t, s, "s1", "j1", "render", 0)

	four := 4
	created, err := s.CreateTask(ctx, store.Task{
		ID: "t1", JobID: "j1", StepID: step.ID, Name: "t1",
		Status: store.TaskStatusPending, Parameters: map[string]string{},
		RequiredCores: &four,
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if created.RequiredCores == nil || *created.RequiredCores != 4 {
		t.Fatalf("created RequiredCores = %v, want 4", created.RequiredCores)
	}

	got, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.RequiredCores == nil || *got.RequiredCores != 4 {
		t.Errorf("GetTask RequiredCores = %v, want 4", got.RequiredCores)
	}

	// Undeclared stays nil.
	if _, err := s.CreateTask(ctx, store.Task{
		ID: "t2", JobID: "j1", StepID: step.ID, Name: "t2",
		Status: store.TaskStatusPending, Parameters: map[string]string{},
	}); err != nil {
		t.Fatalf("CreateTask t2: %v", err)
	}
	got2, err := s.GetTask(ctx, "t2")
	if err != nil {
		t.Fatalf("GetTask t2: %v", err)
	}
	if got2.RequiredCores != nil {
		t.Errorf("undeclared RequiredCores = %v, want nil", got2.RequiredCores)
	}
}

func TestLeaseReadyTask(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertJob(t, s, "j1", "f1", "q1")
	step := insertStep(t, s, "s1", "j1", "render", 0)
	insertWorker(t, s, "w1", "f1")
	insertWorker(t, s, "w2", "f1")
	tk, err := s.CreateTask(ctx, store.Task{
		ID: "t1", JobID: "j1", StepID: step.ID, Name: "t1",
		Status: store.TaskStatusReady, Parameters: map[string]string{},
	})
	if err != nil {
		t.Fatalf("CreateTask t1: %v", err)
	}

	now := time.Now().UTC()
	ok, err := s.LeaseReadyTask(ctx, tk.ID, "w1", now)
	if err != nil || !ok {
		t.Fatalf("first lease = %v, %v, want true, nil", ok, err)
	}
	got, err := s.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask t1: %v", err)
	}
	if got.Status != store.TaskStatusAssigned || got.AssignedWorkerID != "w1" || got.AssignedAt == nil {
		t.Fatalf("after lease: status=%q worker=%q at=%v", got.Status, got.AssignedWorkerID, got.AssignedAt)
	}

	// Second lease loses the race (no longer ready).
	ok2, err := s.LeaseReadyTask(ctx, tk.ID, "w2", now)
	if err != nil || ok2 {
		t.Errorf("second lease = %v, %v, want false, nil", ok2, err)
	}
}
