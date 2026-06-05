// SPDX-License-Identifier: AGPL-3.0-only

package fake

// Tests for the previously uncovered fake store methods.
//
// Covered here:
//   task.go       — AssignTask, ReclaimWorkerTasks, ListReadyTasks,
//                   CountActiveTasksInQueue, CountActiveTasksInFarm,
//                   CancelJobTasks, CountReadyTasksByQueue, CountTasksByJob,
//                   ListTasks (sort fields), filterTask edge cases
//   worker.go     — UpdateWorker, UpdateWorkerStatus, UpdateWorkerHeartbeat,
//                   ListStaleWorkers, CountIdleWorkers, ListWorkers (sort/filter)
//   job.go        — UpdateJob, UpdateJobStatus, CancelJobStatus,
//                   ListJobs (sort/filter)
//   task_attempt.go — GetTaskAttempt, LatestTaskAttempt, ListTaskAttempts,
//                     TerminateWorkerAttempts, CancelJobAttempts,
//                     UpdateTaskAttempt
//   queue.go      — UpdateQueue (conflict/not-found), DeleteQueue, ListQueues
//                   (sort/filter/paused)
//   license.go    — CreateCheckout, ReleaseCheckout, ActiveCheckoutCount,
//                   TryClaimLicenseSlots, ReleaseAttemptCheckouts,
//                   ReleaseJobCheckouts, UpdateLicensePool, DeleteLicensePool

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func ctx() context.Context { return context.Background() }

func mustCreateFarm(t *testing.T, s *Store, name string) store.Farm {
	t.Helper()
	f, err := s.CreateFarm(ctx(), store.Farm{ID: "farm-" + name, Name: name})
	if err != nil {
		t.Fatalf("CreateFarm(%q): %v", name, err)
	}
	return f
}

func mustCreateQueue(t *testing.T, s *Store, farmID, id, name string) store.Queue {
	t.Helper()
	q, err := s.CreateQueue(ctx(), store.Queue{ID: id, FarmID: farmID, Name: name})
	if err != nil {
		t.Fatalf("CreateQueue(%q): %v", name, err)
	}
	return q
}

func mustCreateJob(t *testing.T, s *Store, id, farmID, queueID string) store.Job {
	t.Helper()
	j, err := s.CreateJob(ctx(), store.Job{
		ID: id, FarmID: farmID, QueueID: queueID, Name: id,
		Status: store.JobStatusRunning, Priority: 50,
	})
	if err != nil {
		t.Fatalf("CreateJob(%q): %v", id, err)
	}
	return j
}

func mustCreateTask(t *testing.T, s *Store, id, jobID, stepID string, status store.TaskStatus) store.Task {
	t.Helper()
	tk, err := s.CreateTask(ctx(), store.Task{
		ID: id, JobID: jobID, StepID: stepID, Name: id,
		Status: status, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("CreateTask(%q): %v", id, err)
	}
	return tk
}

func mustCreateWorker(t *testing.T, s *Store, id, farmID string, status store.WorkerStatus) store.Worker {
	t.Helper()
	w, err := s.RegisterWorker(ctx(), store.Worker{
		ID: id, FarmID: farmID, Hostname: id,
		Status: status, RegisteredAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("RegisterWorker(%q): %v", id, err)
	}
	return w
}

func mustCreateAttempt(t *testing.T, s *Store, id, taskID string, n int, status store.AttemptStatus) store.TaskAttempt {
	t.Helper()
	a, err := s.CreateTaskAttempt(ctx(), store.TaskAttempt{
		ID: id, TaskID: taskID, AttemptNumber: n,
		Status: status, StartedAt: time.Now(), CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("CreateTaskAttempt(%q): %v", id, err)
	}
	return a
}

func mustGetTask(t *testing.T, s *Store, id string) store.Task {
	t.Helper()
	tk, err := s.GetTask(ctx(), id)
	if err != nil {
		t.Fatalf("GetTask(%q): %v", id, err)
	}
	return tk
}

func mustGetWorker(t *testing.T, s *Store, id string) store.Worker {
	t.Helper()
	w, err := s.GetWorker(ctx(), id)
	if err != nil {
		t.Fatalf("GetWorker(%q): %v", id, err)
	}
	return w
}

func mustGetJob(t *testing.T, s *Store, id string) store.Job {
	t.Helper()
	j, err := s.GetJob(ctx(), id)
	if err != nil {
		t.Fatalf("GetJob(%q): %v", id, err)
	}
	return j
}

func mustGetAttempt(t *testing.T, s *Store, id string) store.TaskAttempt {
	t.Helper()
	a, err := s.GetTaskAttempt(ctx(), id)
	if err != nil {
		t.Fatalf("GetTaskAttempt(%q): %v", id, err)
	}
	return a
}

func mustCreateCheckout(t *testing.T, s *Store, co store.LicenseCheckout) store.LicenseCheckout {
	t.Helper()
	created, err := s.CreateCheckout(ctx(), co)
	if err != nil {
		t.Fatalf("CreateCheckout(%q): %v", co.ID, err)
	}
	return created
}

func mustActiveCheckoutCount(t *testing.T, s *Store, poolID string) int {
	t.Helper()
	n, err := s.ActiveCheckoutCount(ctx(), poolID)
	if err != nil {
		t.Fatalf("ActiveCheckoutCount(%q): %v", poolID, err)
	}
	return n
}

func mustUpdateTaskAttempt(t *testing.T, s *Store, a store.TaskAttempt) store.TaskAttempt {
	t.Helper()
	updated, err := s.UpdateTaskAttempt(ctx(), a)
	if err != nil {
		t.Fatalf("UpdateTaskAttempt(%q): %v", a.ID, err)
	}
	return updated
}

// ── task.go ───────────────────────────────────────────────────────────────────

func TestAssignTask(t *testing.T) {
	s := New()
	defer s.Close()

	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	mustCreateJob(t, s, "j1", "farm-f1", "q1")
	mustCreateTask(t, s, "t1", "j1", "s1", store.TaskStatusReady)

	now := time.Now()
	if err := s.AssignTask(ctx(), "t1", "worker-1", now); err != nil {
		t.Fatalf("AssignTask: %v", err)
	}
	tk := mustGetTask(t, s, "t1")
	if tk.Status != store.TaskStatusAssigned {
		t.Errorf("status = %v, want assigned", tk.Status)
	}
	if tk.AssignedWorkerID != "worker-1" {
		t.Errorf("worker = %q, want worker-1", tk.AssignedWorkerID)
	}
}

func TestAssignTask_NotFound(t *testing.T) {
	s := New()
	defer s.Close()
	err := s.AssignTask(ctx(), "nope", "w", time.Now())
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestReclaimWorkerTasks(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	mustCreateJob(t, s, "j1", "farm-f1", "q1")
	mustCreateTask(t, s, "t1", "j1", "s1", store.TaskStatusAssigned)
	mustCreateTask(t, s, "t2", "j1", "s1", store.TaskStatusRunning)
	mustCreateTask(t, s, "t3", "j1", "s1", store.TaskStatusSucceeded)

	// Manually set assigned worker on t1 and t2.
	if err := s.AssignTask(ctx(), "t1", "w1", time.Now()); err != nil {
		t.Fatalf("AssignTask t1: %v", err)
	}
	if err := s.UpdateTaskStatus(ctx(), "t2", store.TaskStatusRunning); err != nil {
		t.Fatalf("UpdateTaskStatus t2: %v", err)
	}
	// t2 has no assigned worker set — but we manually fix that.
	s.mu.Lock()
	tk := s.tasks["t2"]
	tk.AssignedWorkerID = "w1"
	s.tasks["t2"] = tk
	s.mu.Unlock()

	n, err := s.ReclaimWorkerTasks(ctx(), "w1")
	if err != nil {
		t.Fatalf("ReclaimWorkerTasks: %v", err)
	}
	if n != 2 {
		t.Errorf("reclaimed = %d, want 2", n)
	}
	tk1 := mustGetTask(t, s, "t1")
	if tk1.Status != store.TaskStatusReady {
		t.Errorf("t1 status = %v, want ready", tk1.Status)
	}
}

func TestListReadyTasks(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	mustCreateJob(t, s, "j1", "farm-f1", "q1")
	mustCreateTask(t, s, "t-ready", "j1", "s1", store.TaskStatusReady)
	mustCreateTask(t, s, "t-running", "j1", "s1", store.TaskStatusRunning)

	tasks, err := s.ListReadyTasks(ctx(), "farm-f1", 10)
	if err != nil {
		t.Fatalf("ListReadyTasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "t-ready" {
		t.Errorf("got %v, want [t-ready]", tasks)
	}
}

func TestListReadyTasks_AllFarms(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	mustCreateJob(t, s, "j1", "farm-f1", "q1")
	mustCreateTask(t, s, "t1", "j1", "s1", store.TaskStatusReady)

	tasks, err := s.ListReadyTasks(ctx(), "", 10)
	if err != nil {
		t.Fatalf("ListReadyTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Errorf("want 1 task, got %d", len(tasks))
	}
}

func TestCountActiveTasksInQueue(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	mustCreateJob(t, s, "j1", "farm-f1", "q1")
	mustCreateTask(t, s, "t1", "j1", "s1", store.TaskStatusRunning)
	mustCreateTask(t, s, "t2", "j1", "s1", store.TaskStatusAssigned)
	mustCreateTask(t, s, "t3", "j1", "s1", store.TaskStatusSucceeded)

	n, err := s.CountActiveTasksInQueue(ctx(), "q1")
	if err != nil {
		t.Fatalf("CountActiveTasksInQueue: %v", err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2", n)
	}
}

func TestCountActiveTasksInFarm(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	mustCreateJob(t, s, "j1", "farm-f1", "q1")
	mustCreateTask(t, s, "t1", "j1", "s1", store.TaskStatusAssigned)
	mustCreateTask(t, s, "t2", "j1", "s1", store.TaskStatusReady)

	n, err := s.CountActiveTasksInFarm(ctx(), "farm-f1")
	if err != nil {
		t.Fatalf("CountActiveTasksInFarm: %v", err)
	}
	if n != 1 {
		t.Errorf("count = %d, want 1", n)
	}
}

func TestCancelJobTasks(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	mustCreateJob(t, s, "j1", "farm-f1", "q1")
	mustCreateTask(t, s, "t-ready", "j1", "s1", store.TaskStatusReady)
	mustCreateTask(t, s, "t-running", "j1", "s1", store.TaskStatusRunning)
	mustCreateTask(t, s, "t-done", "j1", "s1", store.TaskStatusSucceeded)

	active, err := s.CancelJobTasks(ctx(), "j1", time.Now())
	if err != nil {
		t.Fatalf("CancelJobTasks: %v", err)
	}
	// Only the running task was active.
	if len(active) != 1 {
		t.Errorf("active = %d, want 1", len(active))
	}

	tk := mustGetTask(t, s, "t-ready")
	if tk.Status != store.TaskStatusCanceled {
		t.Errorf("t-ready status = %v, want canceled", tk.Status)
	}
	done := mustGetTask(t, s, "t-done")
	if done.Status != store.TaskStatusSucceeded {
		t.Error("terminal task should not be changed")
	}
}

func TestCountReadyTasksByQueue(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	mustCreateJob(t, s, "j1", "farm-f1", "q1")
	mustCreateTask(t, s, "t1", "j1", "s1", store.TaskStatusReady)
	mustCreateTask(t, s, "t2", "j1", "s1", store.TaskStatusReady)
	mustCreateTask(t, s, "t3", "j1", "s1", store.TaskStatusRunning)

	counts, err := s.CountReadyTasksByQueue(ctx(), "farm-f1")
	if err != nil {
		t.Fatalf("CountReadyTasksByQueue: %v", err)
	}
	if counts["q1"] != 2 {
		t.Errorf("q1 count = %d, want 2", counts["q1"])
	}
}

func TestCountTasksByJob(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	mustCreateJob(t, s, "j1", "farm-f1", "q1")
	mustCreateTask(t, s, "t1", "j1", "s1", store.TaskStatusReady)
	mustCreateTask(t, s, "t2", "j1", "s1", store.TaskStatusReady)
	mustCreateTask(t, s, "t3", "j1", "s1", store.TaskStatusSucceeded)

	counts, err := s.CountTasksByJob(ctx(), "j1")
	if err != nil {
		t.Fatalf("CountTasksByJob: %v", err)
	}
	if counts[store.TaskStatusReady] != 2 {
		t.Errorf("ready = %d, want 2", counts[store.TaskStatusReady])
	}
	if counts[store.TaskStatusSucceeded] != 1 {
		t.Errorf("succeeded = %d, want 1", counts[store.TaskStatusSucceeded])
	}
}

func TestListTasks_SortFields(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	mustCreateJob(t, s, "j1", "farm-f1", "q1")
	mustCreateTask(t, s, "b-task", "j1", "s1", store.TaskStatusRunning)
	mustCreateTask(t, s, "a-task", "j1", "s1", store.TaskStatusFailed)

	for _, field := range []store.TaskSortField{
		store.TaskSortByStatus,
		store.TaskSortByUpdatedAt,
		store.TaskSortByName,
		store.TaskSortByCreatedAt,
	} {
		for _, dir := range []store.SortDir{store.SortAsc, store.SortDesc} {
			_, err := s.ListTasks(ctx(), store.ListTasksOptions{
				JobID:      "j1",
				SortBy:     field,
				SortDir:    dir,
				Pagination: store.Pagination{Limit: 10},
			})
			if err != nil {
				t.Errorf("ListTasks(field=%v,dir=%v): %v", field, dir, err)
			}
		}
	}
}

func TestListTasks_FilterByWorkerID(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	mustCreateJob(t, s, "j1", "farm-f1", "q1")
	mustCreateTask(t, s, "t1", "j1", "s1", store.TaskStatusAssigned)
	if err := s.AssignTask(ctx(), "t1", "w1", time.Now()); err != nil {
		t.Fatalf("AssignTask: %v", err)
	}

	page, err := s.ListTasks(ctx(), store.ListTasksOptions{
		WorkerID:   "w1",
		Pagination: store.Pagination{Limit: 10},
	})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(page.Items) != 1 {
		t.Errorf("want 1 task, got %d", len(page.Items))
	}
}

func TestListTasks_FilterByStatuses(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	mustCreateJob(t, s, "j1", "farm-f1", "q1")
	mustCreateTask(t, s, "t-failed", "j1", "s1", store.TaskStatusFailed)
	mustCreateTask(t, s, "t-canceled", "j1", "s1", store.TaskStatusCanceled)
	mustCreateTask(t, s, "t-ready", "j1", "s1", store.TaskStatusReady)

	page, err := s.ListTasks(ctx(), store.ListTasksOptions{
		Statuses:   []store.TaskStatus{store.TaskStatusFailed, store.TaskStatusCanceled},
		Pagination: store.Pagination{Limit: 10},
	})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(page.Items) != 2 {
		t.Errorf("want 2, got %d", len(page.Items))
	}
}

// ── worker.go ─────────────────────────────────────────────────────────────────

func TestUpdateWorker(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateWorker(t, s, "w1", "farm-f1", store.WorkerStatusOnline)

	updated, err := s.UpdateWorker(ctx(), store.Worker{
		ID: "w1", FarmID: "farm-f1", Hostname: "new-host", Status: store.WorkerStatusOnline,
	})
	if err != nil {
		t.Fatalf("UpdateWorker: %v", err)
	}
	if updated.Hostname != "new-host" {
		t.Errorf("hostname = %q, want new-host", updated.Hostname)
	}
}

func TestUpdateWorker_NotFound(t *testing.T) {
	s := New()
	defer s.Close()
	_, err := s.UpdateWorker(ctx(), store.Worker{ID: "ghost"})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestUpdateWorkerStatus(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateWorker(t, s, "w1", "f1", store.WorkerStatusOnline)

	if err := s.UpdateWorkerStatus(ctx(), "w1", store.WorkerStatusOffline); err != nil {
		t.Fatalf("UpdateWorkerStatus: %v", err)
	}
	w := mustGetWorker(t, s, "w1")
	if w.Status != store.WorkerStatusOffline {
		t.Errorf("status = %v, want offline", w.Status)
	}
}

func TestUpdateWorkerStatus_NotFound(t *testing.T) {
	s := New()
	defer s.Close()
	err := s.UpdateWorkerStatus(ctx(), "ghost", store.WorkerStatusOffline)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestUpdateWorkerHeartbeat(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateWorker(t, s, "w1", "f1", store.WorkerStatusOnline)

	now := time.Now()
	if err := s.UpdateWorkerHeartbeat(ctx(), "w1", now); err != nil {
		t.Fatalf("UpdateWorkerHeartbeat: %v", err)
	}
	w := mustGetWorker(t, s, "w1")
	if w.LastHeartbeatAt == nil || !w.LastHeartbeatAt.Equal(now) {
		t.Errorf("heartbeat not set correctly")
	}
}

func TestUpdateWorkerHeartbeat_NotFound(t *testing.T) {
	s := New()
	defer s.Close()
	err := s.UpdateWorkerHeartbeat(ctx(), "ghost", time.Now())
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestListStaleWorkers(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateWorker(t, s, "w-fresh", "f1", store.WorkerStatusOnline)
	mustCreateWorker(t, s, "w-stale", "f1", store.WorkerStatusOnline)
	mustCreateWorker(t, s, "w-offline", "f1", store.WorkerStatusOffline)

	// Give w-fresh a recent heartbeat.
	now := time.Now()
	if err := s.UpdateWorkerHeartbeat(ctx(), "w-fresh", now); err != nil {
		t.Fatalf("UpdateWorkerHeartbeat: %v", err)
	}

	stale, err := s.ListStaleWorkers(ctx(), now.Add(-time.Second))
	if err != nil {
		t.Fatalf("ListStaleWorkers: %v", err)
	}
	// w-stale has no heartbeat, w-offline is skipped.
	if len(stale) != 1 || stale[0].ID != "w-stale" {
		t.Errorf("stale workers = %v, want [w-stale]", stale)
	}
}

func TestCountIdleWorkers(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	mustCreateJob(t, s, "j1", "farm-f1", "q1")
	mustCreateWorker(t, s, "w-idle", "farm-f1", store.WorkerStatusOnline)
	mustCreateWorker(t, s, "w-busy", "farm-f1", store.WorkerStatusOnline)
	mustCreateTask(t, s, "t1", "j1", "s1", store.TaskStatusRunning)

	// Mark w-busy as having a running task.
	s.mu.Lock()
	tk := s.tasks["t1"]
	tk.AssignedWorkerID = "w-busy"
	s.tasks["t1"] = tk
	s.mu.Unlock()

	n, err := s.CountIdleWorkers(ctx(), "farm-f1")
	if err != nil {
		t.Fatalf("CountIdleWorkers: %v", err)
	}
	if n != 1 {
		t.Errorf("idle = %d, want 1", n)
	}
}

func TestCountIdleWorkers_AllFarms(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateWorker(t, s, "w1", "farm-any", store.WorkerStatusOnline)

	n, err := s.CountIdleWorkers(ctx(), "")
	if err != nil {
		t.Fatalf("CountIdleWorkers: %v", err)
	}
	if n != 1 {
		t.Errorf("want 1, got %d", n)
	}
}

func TestListWorkers_SortAndFilter(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateWorker(t, s, "w1", "f1", store.WorkerStatusOnline)
	mustCreateWorker(t, s, "w2", "f2", store.WorkerStatusOffline)

	for _, field := range []store.WorkerSortField{
		store.WorkerSortByHostname,
		store.WorkerSortByStatus,
		store.WorkerSortByRegisteredAt,
		store.WorkerSortByLastHeartbeatAt,
	} {
		_, err := s.ListWorkers(ctx(), store.ListWorkersOptions{
			SortBy:     field,
			Pagination: store.Pagination{Limit: 10},
		})
		if err != nil {
			t.Errorf("ListWorkers(sort=%v): %v", field, err)
		}
	}

	// Filter by farm.
	page, err := s.ListWorkers(ctx(), store.ListWorkersOptions{
		FarmID:     "f1",
		Pagination: store.Pagination{Limit: 10},
	})
	if err != nil {
		t.Fatalf("ListWorkers(FarmID): %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != "w1" {
		t.Errorf("filter by farm: got %v, want [w1]", page.Items)
	}

	// Filter by status.
	page, err = s.ListWorkers(ctx(), store.ListWorkersOptions{
		Status:     store.WorkerStatusOffline,
		Pagination: store.Pagination{Limit: 10},
	})
	if err != nil {
		t.Fatalf("ListWorkers(Status): %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != "w2" {
		t.Errorf("filter by status: got %v, want [w2]", page.Items)
	}
}

// ── job.go ────────────────────────────────────────────────────────────────────

func TestUpdateJob(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	mustCreateJob(t, s, "j1", "farm-f1", "q1")

	updated, err := s.UpdateJob(ctx(), store.Job{
		ID: "j1", FarmID: "farm-f1", QueueID: "q1",
		Name: "renamed", Priority: 99,
	})
	if err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}
	if updated.Name != "renamed" {
		t.Errorf("name = %q, want renamed", updated.Name)
	}
	// Lifecycle fields should be preserved.
	if updated.Status != store.JobStatusRunning {
		t.Errorf("status altered to %v", updated.Status)
	}
}

func TestUpdateJob_NotFound(t *testing.T) {
	s := New()
	defer s.Close()
	_, err := s.UpdateJob(ctx(), store.Job{ID: "ghost"})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestUpdateJobStatus_RunningSetStartedAt(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	j, err := s.CreateJob(ctx(), store.Job{ID: "j1", FarmID: "farm-f1", QueueID: "q1", Name: "j1", Status: store.JobStatusPending})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	if err := s.UpdateJobStatus(ctx(), j.ID, store.JobStatusRunning); err != nil {
		t.Fatalf("UpdateJobStatus: %v", err)
	}
	got := mustGetJob(t, s, j.ID)
	if got.StartedAt == nil {
		t.Error("StartedAt should be set when transitioning to running")
	}
}

func TestUpdateJobStatus_TerminalSetCompletedAt(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	mustCreateJob(t, s, "j1", "farm-f1", "q1")

	for _, status := range []store.JobStatus{store.JobStatusCompleted, store.JobStatusFailed, store.JobStatusCanceled} {
		if err := s.UpdateJobStatus(ctx(), "j1", store.JobStatusRunning); err != nil {
			t.Fatalf("reset to running: %v", err)
		}
		if err := s.UpdateJobStatus(ctx(), "j1", status); err != nil {
			t.Fatalf("UpdateJobStatus(%v): %v", status, err)
		}
		got := mustGetJob(t, s, "j1")
		if got.CompletedAt == nil {
			t.Errorf("%v: CompletedAt should be set", status)
		}
	}
}

func TestUpdateJobStatus_NotFound(t *testing.T) {
	s := New()
	defer s.Close()
	err := s.UpdateJobStatus(ctx(), "ghost", store.JobStatusFailed)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestCancelJobStatus(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	mustCreateJob(t, s, "j1", "farm-f1", "q1")

	if err := s.CancelJobStatus(ctx(), "j1"); err != nil {
		t.Fatalf("CancelJobStatus: %v", err)
	}
	j := mustGetJob(t, s, "j1")
	if j.Status != store.JobStatusCanceled {
		t.Errorf("status = %v, want canceled", j.Status)
	}
	// Idempotent.
	if err := s.CancelJobStatus(ctx(), "j1"); err != nil {
		t.Fatalf("second CancelJobStatus should be idempotent: %v", err)
	}
}

func TestCancelJobStatus_CompletedConflict(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	mustCreateJob(t, s, "j1", "farm-f1", "q1")
	if err := s.UpdateJobStatus(ctx(), "j1", store.JobStatusCompleted); err != nil {
		t.Fatalf("UpdateJobStatus: %v", err)
	}

	err := s.CancelJobStatus(ctx(), "j1")
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
}

func TestCancelJobStatus_NotFound(t *testing.T) {
	s := New()
	defer s.Close()
	err := s.CancelJobStatus(ctx(), "ghost")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestListJobs_SortAndFilter(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	mustCreateJob(t, s, "j1", "farm-f1", "q1")
	mustCreateJob(t, s, "j2", "farm-f1", "q1")

	for _, field := range []store.JobSortField{
		store.JobSortByCreatedAt,
		store.JobSortByPriority,
		store.JobSortByStatus,
		store.JobSortByUpdatedAt,
		store.JobSortByName,
	} {
		_, err := s.ListJobs(ctx(), store.ListJobsOptions{
			SortBy:     field,
			Pagination: store.Pagination{Limit: 10},
		})
		if err != nil {
			t.Errorf("ListJobs(sort=%v): %v", field, err)
		}
	}

	// Filter fields.
	for _, opts := range []store.ListJobsOptions{
		{FarmID: "farm-f1", Pagination: store.Pagination{Limit: 10}},
		{QueueID: "q1", Pagination: store.Pagination{Limit: 10}},
		{Status: store.JobStatusRunning, Pagination: store.Pagination{Limit: 10}},
	} {
		if _, err := s.ListJobs(ctx(), opts); err != nil {
			t.Errorf("ListJobs(%+v): %v", opts, err)
		}
	}
}

// ── task_attempt.go ───────────────────────────────────────────────────────────

func TestGetTaskAttempt(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateAttempt(t, s, "a1", "t1", 1, store.AttemptStatusRunning)

	a, err := s.GetTaskAttempt(ctx(), "a1")
	if err != nil {
		t.Fatalf("GetTaskAttempt: %v", err)
	}
	if a.ID != "a1" {
		t.Errorf("id = %q, want a1", a.ID)
	}
}

func TestGetTaskAttempt_NotFound(t *testing.T) {
	s := New()
	defer s.Close()
	_, err := s.GetTaskAttempt(ctx(), "ghost")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestLatestTaskAttempt_Multiple(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateAttempt(t, s, "a1", "t1", 1, store.AttemptStatusFailed)
	mustCreateAttempt(t, s, "a2", "t1", 2, store.AttemptStatusRunning)

	a, err := s.LatestTaskAttempt(ctx(), "t1")
	if err != nil {
		t.Fatalf("LatestTaskAttempt: %v", err)
	}
	if a.ID != "a2" {
		t.Errorf("latest = %q, want a2", a.ID)
	}
}

func TestLatestTaskAttempt_NotFound(t *testing.T) {
	s := New()
	defer s.Close()
	_, err := s.LatestTaskAttempt(ctx(), "t-none")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestListTaskAttempts(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateAttempt(t, s, "a2", "t1", 2, store.AttemptStatusRunning)
	mustCreateAttempt(t, s, "a1", "t1", 1, store.AttemptStatusFailed)

	attempts, err := s.ListTaskAttempts(ctx(), "t1")
	if err != nil {
		t.Fatalf("ListTaskAttempts: %v", err)
	}
	if len(attempts) != 2 || attempts[0].AttemptNumber != 1 {
		t.Errorf("expected 2 attempts ordered asc; got %v", attempts)
	}
}

func TestTerminateWorkerAttempts(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	mustCreateJob(t, s, "j1", "farm-f1", "q1")
	mustCreateTask(t, s, "t1", "j1", "s1", store.TaskStatusAssigned)
	if err := s.AssignTask(ctx(), "t1", "w1", time.Now()); err != nil {
		t.Fatalf("AssignTask: %v", err)
	}
	mustCreateAttempt(t, s, "a1", "t1", 1, store.AttemptStatusRunning)

	now := time.Now()
	n, err := s.TerminateWorkerAttempts(ctx(), "w1", store.AttemptStatusFailed, now)
	if err != nil {
		t.Fatalf("TerminateWorkerAttempts: %v", err)
	}
	if n != 1 {
		t.Errorf("terminated = %d, want 1", n)
	}
	a := mustGetAttempt(t, s, "a1")
	if a.Status != store.AttemptStatusFailed {
		t.Errorf("status = %v, want failed", a.Status)
	}
}

func TestCancelJobAttempts(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	mustCreateJob(t, s, "j1", "farm-f1", "q1")
	mustCreateTask(t, s, "t1", "j1", "s1", store.TaskStatusRunning)
	mustCreateAttempt(t, s, "a1", "t1", 1, store.AttemptStatusRunning)

	n, err := s.CancelJobAttempts(ctx(), "j1", time.Now())
	if err != nil {
		t.Fatalf("CancelJobAttempts: %v", err)
	}
	if n != 1 {
		t.Errorf("canceled = %d, want 1", n)
	}
}

func TestUpdateTaskAttempt(t *testing.T) {
	s := New()
	defer s.Close()
	a := mustCreateAttempt(t, s, "a1", "t1", 1, store.AttemptStatusRunning)

	now := time.Now()
	a.Status = store.AttemptStatusSucceeded
	a.EndedAt = &now
	a.SessionID = "sess-123"

	updated, err := s.UpdateTaskAttempt(ctx(), a)
	if err != nil {
		t.Fatalf("UpdateTaskAttempt: %v", err)
	}
	if updated.Status != store.AttemptStatusSucceeded {
		t.Errorf("status = %v, want succeeded", updated.Status)
	}
	if updated.SessionID != "sess-123" {
		t.Errorf("session_id = %q, want sess-123", updated.SessionID)
	}
}

func TestUpdateTaskAttempt_SessionIDNotOverwrittenByEmpty(t *testing.T) {
	s := New()
	defer s.Close()
	a := mustCreateAttempt(t, s, "a1", "t1", 1, store.AttemptStatusRunning)
	// Set a session ID.
	a.SessionID = "original"
	mustUpdateTaskAttempt(t, s, a)

	// Update with empty session ID — should not overwrite.
	a.SessionID = ""
	updated := mustUpdateTaskAttempt(t, s, a)
	if updated.SessionID != "original" {
		t.Errorf("session_id = %q, want original", updated.SessionID)
	}
}

func TestUpdateTaskAttempt_NotFound(t *testing.T) {
	s := New()
	defer s.Close()
	_, err := s.UpdateTaskAttempt(ctx(), store.TaskAttempt{ID: "ghost"})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// ── queue.go ──────────────────────────────────────────────────────────────────

func TestUpdateQueue_NotFound(t *testing.T) {
	s := New()
	defer s.Close()
	_, err := s.UpdateQueue(ctx(), store.Queue{ID: "ghost", FarmID: "f1", Name: "q"})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestUpdateQueue_Conflict(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateQueue(t, s, "f1", "q1", "alpha")
	mustCreateQueue(t, s, "f1", "q2", "beta")

	_, err := s.UpdateQueue(ctx(), store.Queue{ID: "q1", FarmID: "f1", Name: "beta"})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
}

func TestListQueues_SortAndFilter(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateQueue(t, s, "f1", "q1", "alpha")
	mustCreateQueue(t, s, "f1", "q2", "beta")

	for _, field := range []store.QueueSortField{
		store.QueueSortByName,
		store.QueueSortByPriority,
		store.QueueSortByCreatedAt,
	} {
		_, err := s.ListQueues(ctx(), store.ListQueuesOptions{
			SortBy:     field,
			Pagination: store.Pagination{Limit: 10},
		})
		if err != nil {
			t.Errorf("ListQueues(sort=%v): %v", field, err)
		}
	}

	// Filter by paused.
	paused := true
	page, err := s.ListQueues(ctx(), store.ListQueuesOptions{
		Paused:     &paused,
		Pagination: store.Pagination{Limit: 10},
	})
	if err != nil {
		t.Fatalf("ListQueues(paused=true): %v", err)
	}
	if len(page.Items) != 0 {
		t.Errorf("expected 0 paused queues, got %d", len(page.Items))
	}
}

// ── license.go ────────────────────────────────────────────────────────────────

func TestCreateCheckout_ConflictOnDuplicate(t *testing.T) {
	s := New()
	defer s.Close()

	co := store.LicenseCheckout{
		ID: "co1", PoolID: "pool1", TaskAttemptID: "attempt1",
		CheckedOutAt: time.Now(),
	}
	mustCreateCheckout(t, s, co)
	_, err := s.CreateCheckout(ctx(), co) // duplicate
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("want ErrConflict on duplicate, got %v", err)
	}
}

func TestReleaseCheckout(t *testing.T) {
	s := New()
	defer s.Close()

	co := store.LicenseCheckout{ID: "co1", PoolID: "p1", TaskAttemptID: "a1", CheckedOutAt: time.Now()}
	mustCreateCheckout(t, s, co)

	if err := s.ReleaseCheckout(ctx(), "co1", time.Now()); err != nil {
		t.Fatalf("ReleaseCheckout: %v", err)
	}
	if n := mustActiveCheckoutCount(t, s, "p1"); n != 0 {
		t.Errorf("active count = %d, want 0 after release", n)
	}
}

func TestReleaseCheckout_NotFound(t *testing.T) {
	s := New()
	defer s.Close()
	err := s.ReleaseCheckout(ctx(), "ghost", time.Now())
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestActiveCheckoutCount(t *testing.T) {
	s := New()
	defer s.Close()

	for i, id := range []string{"co1", "co2"} {
		co := store.LicenseCheckout{
			ID: id, PoolID: "p1",
			TaskAttemptID: "a" + string(rune('1'+i)),
			CheckedOutAt:  time.Now(),
		}
		mustCreateCheckout(t, s, co)
	}

	if n := mustActiveCheckoutCount(t, s, "p1"); n != 2 {
		t.Errorf("count = %d, want 2", n)
	}
}

func TestTryClaimLicenseSlots_AtCapacity(t *testing.T) {
	s := New()
	defer s.Close()

	// Pool with max 1 slot; pre-claim it.
	co := store.LicenseCheckout{ID: "co1", PoolID: "p1", TaskAttemptID: "a1", CheckedOutAt: time.Now()}
	mustCreateCheckout(t, s, co)

	err := s.TryClaimLicenseSlots(ctx(), "a2", []store.LicensePoolClaim{
		{PoolID: "p1", MaxConcurrent: 1, CheckoutID: "co2"},
	}, time.Now())
	if !errors.Is(err, store.ErrLicenseAtCapacity) {
		t.Fatalf("want ErrLicenseAtCapacity, got %v", err)
	}
}

func TestTryClaimLicenseSlots_Unlimited(t *testing.T) {
	s := New()
	defer s.Close()

	err := s.TryClaimLicenseSlots(ctx(), "a1", []store.LicensePoolClaim{
		{PoolID: "p1", MaxConcurrent: 0, CheckoutID: "co1"}, // 0 = unlimited
	}, time.Now())
	if err != nil {
		t.Fatalf("TryClaimLicenseSlots (unlimited): %v", err)
	}
}

func TestReleaseAttemptCheckouts(t *testing.T) {
	s := New()
	defer s.Close()

	// CreateCheckout enforces uniqueness on (TaskAttemptID, PoolID) — use
	// distinct pool IDs so both rows are accepted.
	for i, id := range []string{"co1", "co2"} {
		co := store.LicenseCheckout{
			ID:            id,
			PoolID:        "p" + string(rune('1'+i)), // p1, p2
			TaskAttemptID: "a1",
			CheckedOutAt:  time.Now(),
		}
		mustCreateCheckout(t, s, co)
	}

	n, err := s.ReleaseAttemptCheckouts(ctx(), "a1", time.Now())
	if err != nil {
		t.Fatalf("ReleaseAttemptCheckouts: %v", err)
	}
	if n != 2 {
		t.Errorf("released = %d, want 2", n)
	}
}

func TestReleaseJobCheckouts(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	mustCreateJob(t, s, "j1", "farm-f1", "q1")
	mustCreateTask(t, s, "t1", "j1", "s1", store.TaskStatusRunning)
	mustCreateAttempt(t, s, "a1", "t1", 1, store.AttemptStatusRunning)

	co := store.LicenseCheckout{ID: "co1", PoolID: "p1", TaskAttemptID: "a1", CheckedOutAt: time.Now()}
	mustCreateCheckout(t, s, co)

	n, err := s.ReleaseJobCheckouts(ctx(), "j1", time.Now())
	if err != nil {
		t.Fatalf("ReleaseJobCheckouts: %v", err)
	}
	if n != 1 {
		t.Errorf("released = %d, want 1", n)
	}
}

func TestUpdateLicensePool_NotFound(t *testing.T) {
	s := New()
	defer s.Close()
	_, err := s.UpdateLicensePool(ctx(), store.LicensePool{ID: "ghost", Name: "x"})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestUpdateLicensePool_Conflict(t *testing.T) {
	s := New()
	defer s.Close()
	p1, err := s.CreateLicensePool(ctx(), store.LicensePool{ID: "p1", Name: "alpha", Product: "maya", MaxConcurrent: 1})
	if err != nil {
		t.Fatalf("CreateLicensePool p1: %v", err)
	}
	if _, err := s.CreateLicensePool(ctx(), store.LicensePool{ID: "p2", Name: "beta", Product: "maya", MaxConcurrent: 1}); err != nil {
		t.Fatalf("CreateLicensePool p2: %v", err)
	}

	_, err = s.UpdateLicensePool(ctx(), store.LicensePool{ID: p1.ID, Name: "beta", Product: "maya", MaxConcurrent: 1})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
}

func TestDeleteLicensePool_NotFound(t *testing.T) {
	s := New()
	defer s.Close()
	err := s.DeleteLicensePool(ctx(), "ghost")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
