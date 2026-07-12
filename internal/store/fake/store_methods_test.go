// SPDX-License-Identifier: AGPL-3.0-or-later

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
//   usage.go      — CreateClaim, ReleaseClaim, ActiveClaimCount,
//                   TryClaimSlots, ReleaseAttemptClaims,
//                   ReleaseJobClaims, UpdateUsagePool, DeleteUsagePool

import (
	"context"
	"errors"
	"slices"
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

func mustCreateClaim(t *testing.T, s *Store, co store.UsageClaim) store.UsageClaim {
	t.Helper()
	created, err := s.CreateClaim(ctx(), co)
	if err != nil {
		t.Fatalf("CreateClaim(%q): %v", co.ID, err)
	}
	return created
}

func mustActiveClaimCount(t *testing.T, s *Store, poolID string) int {
	t.Helper()
	n, err := s.ActiveClaimCount(ctx(), poolID)
	if err != nil {
		t.Fatalf("ActiveClaimCount(%q): %v", poolID, err)
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

func mustCreateStep(t *testing.T, s *Store, id, jobID string, status store.StepStatus, dependsOn ...string) store.Step {
	t.Helper()
	st, err := s.CreateStep(ctx(), store.Step{
		ID: id, JobID: jobID, Name: id, Status: status, DependsOn: dependsOn,
	})
	if err != nil {
		t.Fatalf("CreateStep(%q): %v", id, err)
	}
	return st
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

func TestSetTaskUnschedulableReason(t *testing.T) {
	s := New()
	defer s.Close()

	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	mustCreateJob(t, s, "j1", "farm-f1", "q1")
	mustCreateTask(t, s, "t1", "j1", "s1", store.TaskStatusReady)

	if err := s.SetTaskUnschedulableReason(ctx(), "t1", "no eligible online worker: attribute requirement not met"); err != nil {
		t.Fatalf("SetTaskUnschedulableReason: %v", err)
	}
	tk := mustGetTask(t, s, "t1")
	if tk.UnschedulableReason != "no eligible online worker: attribute requirement not met" {
		t.Errorf("UnschedulableReason: got %q", tk.UnschedulableReason)
	}

	// Clearing.
	if err := s.SetTaskUnschedulableReason(ctx(), "t1", ""); err != nil {
		t.Fatalf("SetTaskUnschedulableReason clear: %v", err)
	}
	tk = mustGetTask(t, s, "t1")
	if tk.UnschedulableReason != "" {
		t.Errorf("UnschedulableReason after clear: got %q, want empty", tk.UnschedulableReason)
	}

	// Unknown id.
	if err := s.SetTaskUnschedulableReason(ctx(), "nope", "x"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound for unknown task id, got %v", err)
	}
}

// TestUnschedulableReason_ClearedOnAssign verifies that AssignTask clears a
// stale unschedulable_reason left over from the scheduler's sweep — the
// reason is only meaningful while a task is ready, and AssignTask moves it to
// assigned.
func TestUnschedulableReason_ClearedOnAssign(t *testing.T) {
	s := New()
	defer s.Close()

	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	mustCreateJob(t, s, "j1", "farm-f1", "q1")
	mustCreateTask(t, s, "t1", "j1", "s1", store.TaskStatusReady)

	if err := s.SetTaskUnschedulableReason(ctx(), "t1", "no eligible online worker"); err != nil {
		t.Fatalf("SetTaskUnschedulableReason: %v", err)
	}

	if err := s.AssignTask(ctx(), "t1", "worker-1", time.Now()); err != nil {
		t.Fatalf("AssignTask: %v", err)
	}

	tk := mustGetTask(t, s, "t1")
	if tk.UnschedulableReason != "" {
		t.Errorf("UnschedulableReason after AssignTask: got %q, want empty", tk.UnschedulableReason)
	}
}

// TestUnschedulableReason_ClearedOnCancel verifies that CancelJobTasks clears
// a stale unschedulable_reason on the tasks it cancels.
func TestUnschedulableReason_ClearedOnCancel(t *testing.T) {
	s := New()
	defer s.Close()

	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	mustCreateJob(t, s, "j1", "farm-f1", "q1")
	mustCreateTask(t, s, "t1", "j1", "s1", store.TaskStatusReady)

	if err := s.SetTaskUnschedulableReason(ctx(), "t1", "no eligible online worker"); err != nil {
		t.Fatalf("SetTaskUnschedulableReason: %v", err)
	}

	if _, err := s.CancelJobTasks(ctx(), "j1", time.Now(), ""); err != nil {
		t.Fatalf("CancelJobTasks: %v", err)
	}

	tk := mustGetTask(t, s, "t1")
	if tk.UnschedulableReason != "" {
		t.Errorf("UnschedulableReason after CancelJobTasks: got %q, want empty", tk.UnschedulableReason)
	}
}

// TestUnschedulableReason_ClearedOnUpdateStatus verifies that UpdateTaskStatus
// clears a stale unschedulable_reason on any status transition out of ready.
func TestUnschedulableReason_ClearedOnUpdateStatus(t *testing.T) {
	s := New()
	defer s.Close()

	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	mustCreateJob(t, s, "j1", "farm-f1", "q1")
	mustCreateTask(t, s, "t1", "j1", "s1", store.TaskStatusReady)

	if err := s.SetTaskUnschedulableReason(ctx(), "t1", "no eligible online worker"); err != nil {
		t.Fatalf("SetTaskUnschedulableReason: %v", err)
	}

	if err := s.UpdateTaskStatus(ctx(), "t1", store.TaskStatusRunning); err != nil {
		t.Fatalf("UpdateTaskStatus: %v", err)
	}

	tk := mustGetTask(t, s, "t1")
	if tk.UnschedulableReason != "" {
		t.Errorf("UnschedulableReason after UpdateTaskStatus: got %q, want empty", tk.UnschedulableReason)
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

	tasks, err := s.ListReadyTasks(ctx(), "farm-f1", time.Now(), 10)
	if err != nil {
		t.Fatalf("ListReadyTasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "t-ready" {
		t.Errorf("got %v, want [t-ready]", tasks)
	}
}

func TestListReadyTasks_SkipsBackoffAndPausedJobs(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	mustCreateJob(t, s, "j1", "farm-f1", "q1")
	mustCreateTask(t, s, "t-ready", "j1", "s1", store.TaskStatusReady)

	now := time.Now()
	future := now.Add(time.Minute)
	backoff := mustCreateTask(t, s, "t-backoff", "j1", "s1", store.TaskStatusReady)
	backoff.RetryAfter = &future
	s.mu.Lock()
	s.tasks[backoff.ID] = backoff
	s.mu.Unlock()

	j2, err := s.CreateJob(ctx(), store.Job{
		ID: "j2", FarmID: "farm-f1", QueueID: "q1", Name: "j2",
		Status: store.JobStatusPaused, Priority: 50,
	})
	if err != nil {
		t.Fatalf("CreateJob(j2): %v", err)
	}
	mustCreateTask(t, s, "t-paused", j2.ID, "s1", store.TaskStatusReady)

	tasks, err := s.ListReadyTasks(ctx(), "farm-f1", now, 10)
	if err != nil {
		t.Fatalf("ListReadyTasks: %v", err)
	}
	var ids []string
	for _, tk := range tasks {
		ids = append(ids, tk.ID)
	}
	if !slices.Contains(ids, "t-ready") {
		t.Fatalf("want t-ready selected, got %v", ids)
	}
	if slices.Contains(ids, "t-backoff") || slices.Contains(ids, "t-paused") {
		t.Fatalf("backoff/paused tasks must be excluded, got %v", ids)
	}
}

func TestListReadyTasks_AllFarms(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	mustCreateJob(t, s, "j1", "farm-f1", "q1")
	mustCreateTask(t, s, "t1", "j1", "s1", store.TaskStatusReady)

	tasks, err := s.ListReadyTasks(ctx(), "", time.Now(), 10)
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

	active, err := s.CancelJobTasks(ctx(), "j1", time.Now(), "")
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

	counts, err := s.CountReadyTasksByQueue(ctx(), "farm-f1", time.Now().UTC())
	if err != nil {
		t.Fatalf("CountReadyTasksByQueue: %v", err)
	}
	if counts["q1"] != 2 {
		t.Errorf("q1 count = %d, want 2", counts["q1"])
	}
}

// TestCountReadyTasksByQueue_ExcludesIneligible asserts the count uses the
// same eligibility predicate as ListReadyTasks: ready tasks still backing off
// (retry_after in the future) or under a paused/parked job are not counted —
// so the heartbeat sweep does not wake lease waiters for work nothing can
// lease. Also covers the farmID = "" (all farms) convention.
func TestCountReadyTasksByQueue_ExcludesIneligible(t *testing.T) {
	s := New()
	defer s.Close()
	now := time.Now().UTC()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	mustCreateJob(t, s, "j1", "farm-f1", "q1")
	mustCreateTask(t, s, "t-ok", "j1", "s1", store.TaskStatusReady)

	// Backing off: ready but retry_after has not elapsed (requeued from
	// running through the real auto-retry path).
	mustCreateTask(t, s, "t-backoff", "j1", "s1", store.TaskStatusRunning)
	if requeued, err := s.RequeueTaskForRetry(ctx(), "t-backoff", now.Add(time.Minute), now); err != nil || !requeued {
		t.Fatalf("RequeueTaskForRetry: requeued=%v err=%v", requeued, err)
	}

	// Under an auto-parked job.
	mustCreateJob(t, s, "j-parked", "farm-f1", "q1")
	mustCreateTask(t, s, "t-parked", "j-parked", "s1", store.TaskStatusReady)
	if err := s.ParkJob(ctx(), "j-parked", "failure limit reached (2)", now); err != nil {
		t.Fatalf("ParkJob: %v", err)
	}

	counts, err := s.CountReadyTasksByQueue(ctx(), "farm-f1", now)
	if err != nil {
		t.Fatalf("CountReadyTasksByQueue: %v", err)
	}
	if counts["q1"] != 1 {
		t.Errorf("q1 count = %d, want 1 (backoff + parked-job tasks excluded)", counts["q1"])
	}

	allFarms, err := s.CountReadyTasksByQueue(ctx(), "", now)
	if err != nil {
		t.Fatalf("CountReadyTasksByQueue(all farms): %v", err)
	}
	if allFarms["q1"] != 1 {
		t.Errorf("all-farms q1 count = %d, want 1", allFarms["q1"])
	}
}

// TestResumeJob_Fake mirrors the sqlite ResumeJob contract: resuming an
// auto-parked job clears park state and resets the failure counter; a manual
// pause/resume keeps the counter; a non-paused job is a no-op; an unknown job
// is ErrNotFound.
func TestResumeJob_Fake(t *testing.T) {
	s := New()
	defer s.Close()
	now := time.Now().UTC()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")

	// Auto-parked: reset everything.
	parked := mustCreateJob(t, s, "j-parked", "farm-f1", "q1")
	parked.FailedAttempts = 3
	if _, err := s.UpdateJob(ctx(), parked); err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}
	if err := s.ParkJob(ctx(), "j-parked", "failure limit reached (3)", now); err != nil {
		t.Fatalf("ParkJob: %v", err)
	}
	if err := s.ResumeJob(ctx(), "j-parked", now); err != nil {
		t.Fatalf("ResumeJob: %v", err)
	}
	got := mustGetJob(t, s, "j-parked")
	if got.Status != store.JobStatusPending || got.ParkReason != "" || got.FailedAttempts != 0 {
		t.Errorf("auto-parked resume: %+v, want pending with cleared park state", got)
	}

	// Manual pause: counter survives.
	manual := mustCreateJob(t, s, "j-manual", "farm-f1", "q1")
	manual.FailedAttempts = 2
	if _, err := s.UpdateJob(ctx(), manual); err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}
	if err := s.UpdateJobStatus(ctx(), "j-manual", store.JobStatusPaused); err != nil {
		t.Fatalf("UpdateJobStatus: %v", err)
	}
	if err := s.ResumeJob(ctx(), "j-manual", now); err != nil {
		t.Fatalf("ResumeJob: %v", err)
	}
	got = mustGetJob(t, s, "j-manual")
	if got.Status != store.JobStatusPending || got.FailedAttempts != 2 {
		t.Errorf("manual resume: %+v, want pending with failed_attempts 2", got)
	}

	// Not paused: no-op. Unknown: ErrNotFound.
	if err := s.ResumeJob(ctx(), "j-manual", now); err != nil {
		t.Fatalf("resume of non-paused job should be a no-op, got %v", err)
	}
	if err := s.ResumeJob(ctx(), "missing", now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
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

func TestCountUnschedulableTasksByJob(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	mustCreateJob(t, s, "j1", "farm-f1", "q1")
	mustCreateTask(t, s, "t1", "j1", "s1", store.TaskStatusReady)
	mustCreateTask(t, s, "t2", "j1", "s1", store.TaskStatusReady)
	mustCreateTask(t, s, "t3", "j1", "s1", store.TaskStatusReady)

	for _, id := range []string{"t1", "t2"} {
		if err := s.SetTaskUnschedulableReason(ctx(), id, "no worker matches required capability"); err != nil {
			t.Fatalf("SetTaskUnschedulableReason(%q): %v", id, err)
		}
	}

	n, err := s.CountUnschedulableTasksByJob(ctx(), "j1")
	if err != nil {
		t.Fatalf("CountUnschedulableTasksByJob: %v", err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2", n)
	}

	if err := s.SetTaskUnschedulableReason(ctx(), "t1", ""); err != nil {
		t.Fatalf("SetTaskUnschedulableReason(clear): %v", err)
	}
	n, err = s.CountUnschedulableTasksByJob(ctx(), "j1")
	if err != nil {
		t.Fatalf("CountUnschedulableTasksByJob: %v", err)
	}
	if n != 1 {
		t.Errorf("count after clear = %d, want 1", n)
	}
}

func TestFailureReasonSummary(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	mustCreateJob(t, s, "j1", "farm-f1", "q1")

	// Mixed reasons: staging x2, timeout x1 — staging dominates.
	mustCreateTask(t, s, "t0", "j1", "s1", store.TaskStatusFailed)
	mustCreateTask(t, s, "t1", "j1", "s1", store.TaskStatusFailed)
	mustCreateTask(t, s, "t2", "j1", "s1", store.TaskStatusFailed)
	for _, tc := range []struct{ id, reason string }{
		{"t0", "staging"}, {"t1", "staging"}, {"t2", "timeout"},
	} {
		if err := s.SetTaskFailureReason(ctx(), tc.id, tc.reason); err != nil {
			t.Fatalf("SetTaskFailureReason(%q): %v", tc.id, err)
		}
	}
	// A succeeded task must never count, even with a stray reason.
	succ := mustCreateTask(t, s, "t3", "j1", "s1", store.TaskStatusSucceeded)
	if err := s.SetTaskFailureReason(ctx(), succ.ID, "staging"); err != nil {
		t.Fatalf("SetTaskFailureReason(%q): %v", succ.ID, err)
	}

	sum, err := s.FailureReasonSummary(ctx(), "j1")
	if err != nil {
		t.Fatalf("FailureReasonSummary: %v", err)
	}
	if sum.FailedCount != 3 || sum.DominantReason != "staging" || sum.DistinctReasons != 2 {
		t.Fatalf("got %+v, want {FailedCount:3 DominantReason:staging DistinctReasons:2}", sum)
	}

	// Tie case: two reasons each with count 1 — dominant is the
	// lexicographically smaller reason, deterministically.
	mustCreateJob(t, s, "j2", "farm-f1", "q1")
	mustCreateTask(t, s, "u0", "j2", "s1", store.TaskStatusFailed)
	mustCreateTask(t, s, "u1", "j2", "s1", store.TaskStatusFailed)
	if err := s.SetTaskFailureReason(ctx(), "u0", "timeout"); err != nil {
		t.Fatalf("SetTaskFailureReason(u0): %v", err)
	}
	if err := s.SetTaskFailureReason(ctx(), "u1", "staging"); err != nil {
		t.Fatalf("SetTaskFailureReason(u1): %v", err)
	}
	sum, err = s.FailureReasonSummary(ctx(), "j2")
	if err != nil {
		t.Fatalf("FailureReasonSummary(j2): %v", err)
	}
	if sum.FailedCount != 2 || sum.DominantReason != "staging" || sum.DistinctReasons != 2 {
		t.Fatalf("got %+v, want {FailedCount:2 DominantReason:staging DistinctReasons:2}", sum)
	}

	// Empty case: a job with no failed tasks carrying a reason.
	mustCreateJob(t, s, "j3", "farm-f1", "q1")
	mustCreateTask(t, s, "v0", "j3", "s1", store.TaskStatusSucceeded)
	sum, err = s.FailureReasonSummary(ctx(), "j3")
	if err != nil {
		t.Fatalf("FailureReasonSummary(j3): %v", err)
	}
	if (sum != store.FailureSummary{}) {
		t.Fatalf("got %+v, want zero value", sum)
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

func TestRetryTasks_AllInJob(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	j := mustCreateJob(t, s, "j1", "farm-f1", "q1")
	// Mark the job terminal to prove revival resets it.
	if err := s.UpdateJobStatus(ctx(), j.ID, store.JobStatusFailed); err != nil {
		t.Fatalf("UpdateJobStatus: %v", err)
	}
	mustCreateStep(t, s, "s1", "j1", store.StepStatusFailed)
	mustCreateTask(t, s, "t-failed", "j1", "s1", store.TaskStatusFailed)
	mustCreateTask(t, s, "t-canceled", "j1", "s1", store.TaskStatusCanceled)
	mustCreateTask(t, s, "t-ok", "j1", "s1", store.TaskStatusSucceeded)

	revived, err := s.RetryTasks(ctx(), "j1", nil, time.Now())
	if err != nil {
		t.Fatalf("RetryTasks: %v", err)
	}
	if len(revived) != 2 {
		t.Fatalf("revived = %d, want 2", len(revived))
	}
	if got := mustGetTask(t, s, "t-failed").Status; got != store.TaskStatusPending {
		t.Errorf("t-failed = %v, want pending", got)
	}
	if got := mustGetTask(t, s, "t-canceled").Status; got != store.TaskStatusPending {
		t.Errorf("t-canceled = %v, want pending", got)
	}
	if got := mustGetTask(t, s, "t-ok").Status; got != store.TaskStatusSucceeded {
		t.Errorf("t-ok = %v, want succeeded (untouched)", got)
	}
	if got := mustGetJob(t, s, "j1").Status; got != store.JobStatusPending {
		t.Errorf("job = %v, want pending", got)
	}
	st, err := s.GetStep(ctx(), "s1")
	if err != nil {
		t.Fatalf("GetStep: %v", err)
	}
	if st.Status != store.StepStatusPending {
		t.Errorf("step = %v, want pending", st.Status)
	}
}

func TestRetryTasks_SubsetAndNonTerminalJobUntouched(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	mustCreateJob(t, s, "j1", "farm-f1", "q1") // stays running (non-terminal)
	mustCreateStep(t, s, "s1", "j1", store.StepStatusFailed)
	mustCreateTask(t, s, "t1", "j1", "s1", store.TaskStatusFailed)
	mustCreateTask(t, s, "t2", "j1", "s1", store.TaskStatusFailed)

	revived, err := s.RetryTasks(ctx(), "j1", []string{"t1"}, time.Now())
	if err != nil {
		t.Fatalf("RetryTasks: %v", err)
	}
	if len(revived) != 1 || revived[0].ID != "t1" {
		t.Fatalf("revived = %+v, want [t1]", revived)
	}
	if got := mustGetTask(t, s, "t1").Status; got != store.TaskStatusPending {
		t.Errorf("t1 = %v, want pending", got)
	}
	if got := mustGetTask(t, s, "t2").Status; got != store.TaskStatusFailed {
		t.Errorf("t2 = %v, want failed (not in subset)", got)
	}
	if got := mustGetJob(t, s, "j1").Status; got != store.JobStatusRunning {
		t.Errorf("job = %v, want running (already non-terminal)", got)
	}
}

// TestRetryTasks_ResetsFailureCounters asserts that RetryTasks clears the
// genuine-failure state (Tasks 1-3): a revived task's FailedAttempts and
// RetryAfter, and — since this retry also resets the terminal job — the
// job's FailedAttempts and ParkReason.
func TestRetryTasks_ResetsFailureCounters(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")

	j, err := s.CreateJob(ctx(), store.Job{
		ID: "j1", FarmID: "farm-f1", QueueID: "q1", Name: "j1",
		Status: store.JobStatusFailed, Priority: 50,
		FailedAttempts: 1, ParkReason: "failure limit reached (1)",
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	mustCreateStep(t, s, "s1", j.ID, store.StepStatusFailed)
	retryAfter := time.Now().Add(time.Minute)
	if _, err := s.CreateTask(ctx(), store.Task{
		ID: "t1", JobID: j.ID, StepID: "s1", Name: "t1",
		Status: store.TaskStatusFailed, CreatedAt: time.Now(), UpdatedAt: time.Now(),
		FailedAttempts: 1, RetryAfter: &retryAfter,
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	revived, err := s.RetryTasks(ctx(), j.ID, nil, time.Now())
	if err != nil || len(revived) != 1 {
		t.Fatalf("RetryTasks: %v revived=%d", err, len(revived))
	}

	task := mustGetTask(t, s, "t1")
	if task.FailedAttempts != 0 || task.RetryAfter != nil {
		t.Fatalf("task counters not reset: %+v", task)
	}
	job := mustGetJob(t, s, j.ID)
	if job.FailedAttempts != 0 || job.ParkReason != "" {
		t.Fatalf("job counters not reset: %+v", job)
	}
}

func TestRetryTasks_NothingEligible(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	mustCreateJob(t, s, "j1", "farm-f1", "q1")
	mustCreateStep(t, s, "s1", "j1", store.StepStatusCompleted)
	mustCreateTask(t, s, "t-ok", "j1", "s1", store.TaskStatusSucceeded)

	revived, err := s.RetryTasks(ctx(), "j1", nil, time.Now())
	if err != nil {
		t.Fatalf("RetryTasks: %v", err)
	}
	if len(revived) != 0 {
		t.Errorf("revived = %d, want 0", len(revived))
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

func TestDeleteWorker(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateWorker(t, s, "w1", "f1", store.WorkerStatusOffline)

	if err := s.DeleteWorker(ctx(), "w1"); err != nil {
		t.Fatalf("DeleteWorker: %v", err)
	}
	if _, err := s.GetWorker(ctx(), "w1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetWorker after delete: want ErrNotFound, got %v", err)
	}
}

func TestDeleteWorker_NotFound(t *testing.T) {
	s := New()
	defer s.Close()
	if err := s.DeleteWorker(ctx(), "ghost"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestDeleteOfflineWorkersBefore(t *testing.T) {
	s := New()
	defer s.Close()

	old := time.Now().Add(-2 * time.Hour)
	recent := time.Now().Add(-time.Minute)

	// w1: offline + stale → removed.
	mustCreateWorker(t, s, "w1", "f1", store.WorkerStatusOnline)
	mustHeartbeat(t, s, "w1", old)
	mustStatus(t, s, "w1", store.WorkerStatusOffline)
	// w2: offline + recent → kept.
	mustCreateWorker(t, s, "w2", "f1", store.WorkerStatusOnline)
	mustHeartbeat(t, s, "w2", recent)
	mustStatus(t, s, "w2", store.WorkerStatusOffline)
	// w3: disabled + stale → kept (status filter).
	mustCreateWorker(t, s, "w3", "f1", store.WorkerStatusOnline)
	mustHeartbeat(t, s, "w3", old)
	mustStatus(t, s, "w3", store.WorkerStatusDisabled)

	removed, err := s.DeleteOfflineWorkersBefore(ctx(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("DeleteOfflineWorkersBefore: %v", err)
	}
	if len(removed) != 1 || removed[0].ID != "w1" {
		t.Fatalf("removed: got %v, want [w1]", removed)
	}
	if _, err := s.GetWorker(ctx(), "w1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("w1 should be gone, got %v", err)
	}
	for _, id := range []string{"w2", "w3"} {
		if _, err := s.GetWorker(ctx(), id); err != nil {
			t.Errorf("worker %s should survive: %v", id, err)
		}
	}
}

func mustHeartbeat(t *testing.T, s *Store, id string, at time.Time) {
	t.Helper()
	if err := s.UpdateWorkerHeartbeat(ctx(), id, at); err != nil {
		t.Fatalf("UpdateWorkerHeartbeat(%q): %v", id, err)
	}
}

func mustStatus(t *testing.T, s *Store, id string, status store.WorkerStatus) {
	t.Helper()
	if err := s.UpdateWorkerStatus(ctx(), id, status); err != nil {
		t.Fatalf("UpdateWorkerStatus(%q): %v", id, err)
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

func TestListWorkers_Search(t *testing.T) {
	st := New()
	mk := func(id, name, host, loc string) {
		if _, err := st.RegisterWorker(ctx(), store.Worker{
			ID: id, Name: name, Hostname: host, ComputeLocation: loc,
			Status: store.WorkerStatusOnline, Tags: map[string]string{},
		}); err != nil {
			t.Fatalf("RegisterWorker(%q): %v", id, err)
		}
	}
	mk("w-render01", "render-node-01", "render01.local", "us-west")
	mk("w-comp02", "comp-node-02", "comp02.local", "eu-central")

	page, err := st.ListWorkers(ctx(), store.ListWorkersOptions{Search: "EU-CENTRAL"})
	if err != nil {
		t.Fatalf("ListWorkers: %v", err)
	}
	if page.Total != 1 {
		t.Errorf("Total: got %d, want 1", page.Total)
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

func TestListJobs_Search(t *testing.T) {
	st := New()
	ctx := context.Background()
	if _, err := st.CreateFarm(ctx, store.Farm{ID: "f1", Name: "f"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "q1", FarmID: "f1", Name: "q"}); err != nil {
		t.Fatal(err)
	}
	mk := func(id, name, owner, project string) {
		if _, err := st.CreateJob(ctx, store.Job{
			ID: id, FarmID: "f1", QueueID: "q1", Name: name, Owner: owner,
			Project: project, Status: store.JobStatusPending, Priority: 50,
			TemplateFormat: store.TemplateFormatYAML,
		}); err != nil {
			t.Fatalf("CreateJob(%q): %v", id, err)
		}
	}
	mk("render-night", "Nightly Render", "alice", "moonshot")
	mk("comp-day", "Daytime Comp", "bob", "sunrise")

	page, err := st.ListJobs(ctx, store.ListJobsOptions{Search: "NIGHTLY"})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if page.Total != 1 {
		t.Errorf("Total: got %d, want 1", page.Total)
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

func TestUpdateTaskAttempt_MessageNotOverwrittenByEmpty(t *testing.T) {
	s := New()
	defer s.Close()
	a := mustCreateAttempt(t, s, "a1", "t1", 1, store.AttemptStatusRunning)
	// Set a message.
	a.Message = "boom"
	updated := mustUpdateTaskAttempt(t, s, a)
	if updated.Message != "boom" {
		t.Errorf("message = %q, want boom", updated.Message)
	}

	// Update with empty message — should not overwrite.
	a.Message = ""
	updated = mustUpdateTaskAttempt(t, s, a)
	if updated.Message != "boom" {
		t.Errorf("message = %q, want boom (not overwritten by empty)", updated.Message)
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

// ── usage.go ──────────────────────────────────────────────────────────────────

func TestCreateClaim_ConflictOnDuplicate(t *testing.T) {
	s := New()
	defer s.Close()

	co := store.UsageClaim{
		ID: "co1", PoolID: "pool1", TaskAttemptID: "attempt1",
		ClaimedAt: time.Now(),
	}
	mustCreateClaim(t, s, co)
	_, err := s.CreateClaim(ctx(), co) // duplicate
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("want ErrConflict on duplicate, got %v", err)
	}
}

// Usage-pool names are case-insensitive (OpenJD jobtemplate-2023-09), so the
// fake store must reject a case-only duplicate just as the SQLite NOCASE unique
// index does.
func TestCreateUsagePool_ConflictCaseInsensitive(t *testing.T) {
	s := New()
	defer s.Close()

	if _, err := s.CreateUsagePool(ctx(), store.UsagePool{ID: "p1", Name: "maya", MaxConcurrent: 1}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := s.CreateUsagePool(ctx(), store.UsagePool{ID: "p2", Name: "Maya", MaxConcurrent: 2})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("want ErrConflict for case-insensitive duplicate, got %v", err)
	}
}

func TestReleaseClaim(t *testing.T) {
	s := New()
	defer s.Close()

	co := store.UsageClaim{ID: "co1", PoolID: "p1", TaskAttemptID: "a1", ClaimedAt: time.Now()}
	mustCreateClaim(t, s, co)

	if err := s.ReleaseClaim(ctx(), "co1", time.Now()); err != nil {
		t.Fatalf("ReleaseClaim: %v", err)
	}
	if n := mustActiveClaimCount(t, s, "p1"); n != 0 {
		t.Errorf("active count = %d, want 0 after release", n)
	}
}

func TestReleaseClaim_NotFound(t *testing.T) {
	s := New()
	defer s.Close()
	err := s.ReleaseClaim(ctx(), "ghost", time.Now())
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestActiveClaimCount(t *testing.T) {
	s := New()
	defer s.Close()

	for i, id := range []string{"co1", "co2"} {
		co := store.UsageClaim{
			ID: id, PoolID: "p1",
			TaskAttemptID: "a" + string(rune('1'+i)),
			ClaimedAt:     time.Now(),
		}
		mustCreateClaim(t, s, co)
	}

	if n := mustActiveClaimCount(t, s, "p1"); n != 2 {
		t.Errorf("count = %d, want 2", n)
	}
}

func TestListUsagePoolUtilization(t *testing.T) {
	s := New()
	defer s.Close()

	if _, err := s.CreateUsagePool(ctx(), store.UsagePool{ID: "p-arnold", Name: "arnold", MaxConcurrent: 5}); err != nil {
		t.Fatalf("CreateUsagePool arnold: %v", err)
	}
	if _, err := s.CreateUsagePool(ctx(), store.UsagePool{ID: "p-maya", Name: "maya", MaxConcurrent: 3}); err != nil {
		t.Fatalf("CreateUsagePool maya: %v", err)
	}

	// Two active claims on arnold, one released (should not count).
	mustCreateClaim(t, s, store.UsageClaim{ID: "co1", PoolID: "p-arnold", TaskAttemptID: "a1", ClaimedAt: time.Now()})
	mustCreateClaim(t, s, store.UsageClaim{ID: "co2", PoolID: "p-arnold", TaskAttemptID: "a2", ClaimedAt: time.Now()})
	mustCreateClaim(t, s, store.UsageClaim{ID: "co3", PoolID: "p-arnold", TaskAttemptID: "a3", ClaimedAt: time.Now()})
	if err := s.ReleaseClaim(ctx(), "co3", time.Now()); err != nil {
		t.Fatalf("ReleaseClaim: %v", err)
	}

	usage, err := s.ListUsagePoolUtilization(ctx())
	if err != nil {
		t.Fatalf("ListUsagePoolUtilization: %v", err)
	}
	if len(usage) != 2 {
		t.Fatalf("usage len = %d, want 2", len(usage))
	}
	if usage[0].Name != "arnold" || usage[0].InUse != 2 {
		t.Errorf("arnold: got %q/%d, want arnold/2", usage[0].Name, usage[0].InUse)
	}
	if usage[1].Name != "maya" || usage[1].InUse != 0 {
		t.Errorf("maya: got %q/%d, want maya/0", usage[1].Name, usage[1].InUse)
	}
}

func TestTryClaimSlots_AtCapacity(t *testing.T) {
	s := New()
	defer s.Close()

	// Pool with max 1 slot; pre-claim it.
	co := store.UsageClaim{ID: "co1", PoolID: "p1", TaskAttemptID: "a1", ClaimedAt: time.Now()}
	mustCreateClaim(t, s, co)

	err := s.TryClaimSlots(ctx(), "a2", []store.UsagePoolClaim{
		{PoolID: "p1", MaxConcurrent: 1, ClaimID: "co2"},
	}, time.Now())
	if !errors.Is(err, store.ErrUsageAtCapacity) {
		t.Fatalf("want ErrUsageAtCapacity, got %v", err)
	}
}

func TestTryClaimSlots_Unlimited(t *testing.T) {
	s := New()
	defer s.Close()

	err := s.TryClaimSlots(ctx(), "a1", []store.UsagePoolClaim{
		{PoolID: "p1", MaxConcurrent: 0, ClaimID: "co1"}, // 0 = unlimited
	}, time.Now())
	if err != nil {
		t.Fatalf("TryClaimSlots (unlimited): %v", err)
	}
}

func TestReleaseAttemptClaims(t *testing.T) {
	s := New()
	defer s.Close()

	// CreateClaim enforces uniqueness on (TaskAttemptID, PoolID) — use
	// distinct pool IDs so both rows are accepted.
	for i, id := range []string{"co1", "co2"} {
		co := store.UsageClaim{
			ID:            id,
			PoolID:        "p" + string(rune('1'+i)), // p1, p2
			TaskAttemptID: "a1",
			ClaimedAt:     time.Now(),
		}
		mustCreateClaim(t, s, co)
	}

	n, err := s.ReleaseAttemptClaims(ctx(), "a1", time.Now())
	if err != nil {
		t.Fatalf("ReleaseAttemptClaims: %v", err)
	}
	if n != 2 {
		t.Errorf("released = %d, want 2", n)
	}
}

func TestReleaseJobClaims(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	mustCreateJob(t, s, "j1", "farm-f1", "q1")
	mustCreateTask(t, s, "t1", "j1", "s1", store.TaskStatusRunning)
	mustCreateAttempt(t, s, "a1", "t1", 1, store.AttemptStatusRunning)

	co := store.UsageClaim{ID: "co1", PoolID: "p1", TaskAttemptID: "a1", ClaimedAt: time.Now()}
	mustCreateClaim(t, s, co)

	n, err := s.ReleaseJobClaims(ctx(), "j1", time.Now())
	if err != nil {
		t.Fatalf("ReleaseJobClaims: %v", err)
	}
	if n != 1 {
		t.Errorf("released = %d, want 1", n)
	}
}

func TestUpdateUsagePool_NotFound(t *testing.T) {
	s := New()
	defer s.Close()
	_, err := s.UpdateUsagePool(ctx(), store.UsagePool{ID: "ghost", Name: "x"})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestUpdateUsagePool_Conflict(t *testing.T) {
	s := New()
	defer s.Close()
	p1, err := s.CreateUsagePool(ctx(), store.UsagePool{ID: "p1", Name: "alpha", MaxConcurrent: 1})
	if err != nil {
		t.Fatalf("CreateUsagePool p1: %v", err)
	}
	if _, err := s.CreateUsagePool(ctx(), store.UsagePool{ID: "p2", Name: "beta", MaxConcurrent: 1}); err != nil {
		t.Fatalf("CreateUsagePool p2: %v", err)
	}

	_, err = s.UpdateUsagePool(ctx(), store.UsagePool{ID: p1.ID, Name: "beta", MaxConcurrent: 1})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
}

func TestDeleteUsagePool_NotFound(t *testing.T) {
	s := New()
	defer s.Close()
	err := s.DeleteUsagePool(ctx(), "ghost")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// ── job.go (DeleteJob) ────────────────────────────────────────────────────────

func TestFakeStore_DeleteJob(t *testing.T) {
	ctx := context.Background()
	st := New()
	defer st.Close()

	const jobID = "j-del"

	// Seed job + children.
	mustCreateFarm(t, st, "f1")
	mustCreateQueue(t, st, "farm-f1", "q1", "q1")
	mustCreateJob(t, st, jobID, "farm-f1", "q1")

	if _, err := st.CreateStep(ctx, store.Step{
		ID: "s1", JobID: jobID, Name: "s1",
		Status: store.StepStatusPending, DependsOn: []string{},
	}); err != nil {
		t.Fatalf("CreateStep: %v", err)
	}

	mustCreateTask(t, st, "t1", jobID, "s1", store.TaskStatusPending)
	mustCreateAttempt(t, st, "a1", "t1", 1, store.AttemptStatusRunning)

	if _, err := st.CreateTaskLog(ctx, store.TaskLog{
		ID: "log1", TaskID: "t1", AttemptID: "a1",
		SeqNum: 1, NATSSeq: 1, Stream: store.LogStreamStdout,
	}); err != nil {
		t.Fatalf("CreateTaskLog: %v", err)
	}

	mustCreateClaim(t, st, store.UsageClaim{
		ID: "cl1", PoolID: "pool1", TaskAttemptID: "a1",
		ClaimedAt: time.Now(),
	})

	// Delete the job.
	if err := st.DeleteJob(ctx, jobID); err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}

	// Job row is gone.
	if _, err := st.GetJob(ctx, jobID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetJob = %v, want ErrNotFound", err)
	}

	// Child rows are gone.
	tasks, err := st.ListTasks(ctx, store.ListTasksOptions{JobID: jobID, Pagination: store.Pagination{Limit: 100}})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks.Items) != 0 {
		t.Errorf("tasks not deleted: %d remaining", len(tasks.Items))
	}

	attempts, err := st.ListTaskAttempts(ctx, "t1")
	if err != nil {
		t.Fatalf("ListTaskAttempts: %v", err)
	}
	if len(attempts) != 0 {
		t.Errorf("task_attempts not deleted: %d remaining", len(attempts))
	}

	logs, err := st.ListTaskLogs(ctx, "a1", 0, 100)
	if err != nil {
		t.Fatalf("ListTaskLogs: %v", err)
	}
	if len(logs) != 0 {
		t.Errorf("task_logs not deleted: %d remaining", len(logs))
	}

	// Active claim count for pool1 should be 0.
	n, err := st.ActiveClaimCount(ctx, "pool1")
	if err != nil {
		t.Fatalf("ActiveClaimCount: %v", err)
	}
	if n != 0 {
		t.Errorf("usage_claims not deleted: active count = %d, want 0", n)
	}

	// Deleting a missing job returns ErrNotFound.
	if err := st.DeleteJob(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DeleteJob(missing) = %v, want ErrNotFound", err)
	}
}

// deletedFakeIDs collects IDs from a []store.DeletedJob and returns them sorted.
func deletedFakeIDs(deleted []store.DeletedJob) []string {
	ids := make([]string, len(deleted))
	for i, d := range deleted {
		ids[i] = d.ID
	}
	slices.Sort(ids)
	return ids
}

func TestFakeStore_DeleteTerminalJobsBefore(t *testing.T) {
	ctx := context.Background()
	cutoff := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	old := cutoff.Add(-time.Hour)

	st := New()
	mkJob := func(id string, status store.JobStatus, completed time.Time) {
		c := completed
		if _, err := st.CreateJob(ctx, store.Job{ID: id, Status: status, CompletedAt: &c, UpdatedAt: completed}); err != nil {
			t.Fatalf("mkJob %q: %v", id, err)
		}
	}
	mkJob("c", store.JobStatusCompleted, old)
	mkJob("x", store.JobStatusCanceled, old)
	mkJob("f", store.JobStatusFailed, old)

	got, err := st.DeleteTerminalJobsBefore(ctx, cutoff, false)
	if err != nil {
		t.Fatalf("DeleteTerminalJobsBefore: %v", err)
	}
	if ids := deletedFakeIDs(got); !slices.Equal(ids, []string{"c", "x"}) {
		t.Fatalf("deleted = %v, want [c x]", ids)
	}
	if _, err := st.GetJob(ctx, "f"); err != nil {
		t.Fatalf("failed job should survive without includeFailed: %v", err)
	}
}
