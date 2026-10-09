// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

// Tests for the previously uncovered fake store methods.
//
// Covered here:
//   task.go       — ListReadyTasks, CountActiveTasksInQueue,
//                   CountActiveTasksInFarm, CountReadyTasksByQueue,
//                   CountTasksByJob, ListTasks (sort fields), filterTask edge
//                   cases
//   worker.go     — UpdateWorker, UpdateWorkerHeartbeat,
//                   ListStaleWorkers, CountIdleWorkers, ListWorkers (sort/filter)
//   job.go        — UpdateJob, CancelJobStatus, ListJobs (sort/filter)
//   task_attempt.go — GetTaskAttempt, LatestTaskAttempt, ListTaskAttempts
//   queue.go      — UpdateQueue (conflict/not-found), DeleteQueue, ListQueues
//                   (sort/filter/paused)
//   usage.go      — ActiveClaimCount, UpdateUsagePool, DeleteUsagePool
//
// Fixtures reach their state through the production write path: a job's whole
// graph in one CreateJobSubmission (jobSeed), work in flight through
// LeaseTask. The one state production cannot reach that a test here needs, a
// duplicate active claim, comes from the fake's injector.

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/storetest"
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

// jobSeed is one job's whole graph — the job, its steps and its tasks — which
// submit writes with a single CreateJobSubmission. A job starts running at
// priority 50 with no steps and no tasks; the chained methods add to it, and a
// row is written in whatever status it is given, exactly as CreateJobSubmission
// writes it.
//
// A task that must be in flight is not seeded assigned or running: seed it
// ready, then lease it with leaseTask or runTask, so it carries the attempt and
// the worker a real lease makes.
type jobSeed struct{ sub store.JobSubmission }

// newJob starts a running job at priority 50, named after its ID.
func newJob(id, farmID, queueID string) *jobSeed {
	return &jobSeed{sub: store.JobSubmission{Job: store.Job{
		ID: id, FarmID: farmID, QueueID: queueID, Name: id,
		Status: store.JobStatusRunning, Priority: 50,
	}}}
}

// as sets the job's status, stamping the timestamp a job in that status
// carries: started_at once it has run, completed_at once it is terminal.
func (j *jobSeed) as(status store.JobStatus) *jobSeed {
	now := time.Now().UTC()
	j.sub.Job.Status = status
	switch status {
	case store.JobStatusRunning:
		j.sub.Job.StartedAt = &now
	case store.JobStatusCompleted, store.JobStatusFailed, store.JobStatusCanceled:
		j.sub.Job.CompletedAt = &now
	}
	return j
}

// step adds a step in the given status, or sets the status of the step the job
// already holds under that ID. Its name is its ID.
func (j *jobSeed) step(id string, status store.StepStatus) *jobSeed {
	for i := range j.sub.Steps {
		if j.sub.Steps[i].ID == id {
			j.sub.Steps[i].Status = status
			return j
		}
	}
	j.sub.Steps = append(j.sub.Steps, store.Step{
		ID: id, JobID: j.sub.Job.ID, Name: id, StepOrder: len(j.sub.Steps),
		Status: status, DependsOn: []string{},
	})
	return j
}

// task adds a task of stepID in the given status. A task and its step are one
// submission, so a pending step is added when the job holds none by that ID;
// call step first to give the step another status. Step IDs are unique across
// jobs, as in the store, so two jobs in one test name different steps.
func (j *jobSeed) task(id, stepID string, status store.TaskStatus) *jobSeed {
	return j.taskRow(store.Task{ID: id, StepID: stepID, Status: status})
}

// taskRow adds the given task, filling in its job, its name (its ID) and its
// step, as task does.
func (j *jobSeed) taskRow(task store.Task) *jobSeed {
	task.JobID = j.sub.Job.ID
	if task.Name == "" {
		task.Name = task.ID
	}
	if !slices.ContainsFunc(j.sub.Steps, func(s store.Step) bool { return s.ID == task.StepID }) {
		j.step(task.StepID, store.StepStatusPending)
	}
	j.sub.Tasks = append(j.sub.Tasks, task)
	return j
}

// submit writes the whole graph and returns what the store wrote.
func (j *jobSeed) submit(t *testing.T, s *Store) store.JobSubmission {
	t.Helper()
	return storetest.Submit(t, s, j.sub)
}

// leaseTask leases the ready task taskID to workerID through the production
// lease and returns the attempt the lease made. The lease needs the task's
// job, queue and farm rows to exist; it needs no registered worker.
func leaseTask(t *testing.T, s *Store, taskID, workerID string) store.TaskAttempt {
	t.Helper()
	return storetest.Lease(t, s, store.LeaseRequest{TaskID: taskID, WorkerID: workerID})
}

// runTask leases the ready task taskID to workerID and starts it, leaving it
// running, and returns its attempt.
func runTask(t *testing.T, s *Store, taskID, workerID string) store.TaskAttempt {
	t.Helper()
	return storetest.Running(t, s, store.LeaseRequest{TaskID: taskID, WorkerID: workerID})
}

// seedReadyTask seeds farm "farm-f1", queue "q1" and job "j1" holding one ready
// task, "t1", which a lease can take.
func seedReadyTask(t *testing.T, s *Store) {
	t.Helper()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	newJob("j1", "farm-f1", "q1").task("t1", "s1", store.TaskStatusReady).submit(t, s)
}

func mustCreateWorker(t *testing.T, s *Store, id, farmID string, status store.WorkerStatus) store.Worker {
	t.Helper()
	w, _, err := s.RegisterWorker(ctx(), store.Worker{
		ID: id, FarmID: farmID, Hostname: id,
		Status: status, RegisteredAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("RegisterWorker(%q): %v", id, err)
	}
	return w
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

// mustCreatePool creates the usage pool a lease's claim names: LeaseTask reads
// the pool's cap itself, and treats a pool that does not exist as full.
func mustCreatePool(t *testing.T, s *Store, id string, maxConcurrent int) store.UsagePool {
	t.Helper()
	p, err := s.CreateUsagePool(ctx(), store.UsagePool{ID: id, Name: id, MaxConcurrent: maxConcurrent})
	if err != nil {
		t.Fatalf("CreateUsagePool(%q): %v", id, err)
	}
	return p
}

// claimOn leases the ready task taskID to w1, holding one claim, claimID, on
// pool, and returns the attempt the lease made.
func claimOn(t *testing.T, s *Store, taskID, claimID string, pool store.UsagePool) store.TaskAttempt {
	t.Helper()
	return storetest.Lease(t, s, store.LeaseRequest{
		TaskID: taskID, WorkerID: "w1",
		Claims: []store.UsagePoolClaim{{ClaimID: claimID, PoolID: pool.ID, PoolName: pool.Name}},
	})
}

func mustActiveClaimCount(t *testing.T, s *Store, poolID string) int {
	t.Helper()
	n, err := s.ActiveClaimCount(ctx(), poolID)
	if err != nil {
		t.Fatalf("ActiveClaimCount(%q): %v", poolID, err)
	}
	return n
}

// ── task.go ───────────────────────────────────────────────────────────────────

func TestSetTaskUnschedulableReason(t *testing.T) {
	s := New()
	defer s.Close()

	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	newJob("j1", "farm-f1", "q1").task("t1", "s1", store.TaskStatusReady).submit(t, s)

	if _, err := s.SetTaskUnschedulableReason(ctx(), "t1", "no eligible online worker: attribute requirement not met"); err != nil {
		t.Fatalf("SetTaskUnschedulableReason: %v", err)
	}
	tk := mustGetTask(t, s, "t1")
	if tk.UnschedulableReason != "no eligible online worker: attribute requirement not met" {
		t.Errorf("UnschedulableReason: got %q", tk.UnschedulableReason)
	}

	// Clearing.
	if _, err := s.SetTaskUnschedulableReason(ctx(), "t1", ""); err != nil {
		t.Fatalf("SetTaskUnschedulableReason clear: %v", err)
	}
	tk = mustGetTask(t, s, "t1")
	if tk.UnschedulableReason != "" {
		t.Errorf("UnschedulableReason after clear: got %q, want empty", tk.UnschedulableReason)
	}

	// Unknown id.
	if _, err := s.SetTaskUnschedulableReason(ctx(), "nope", "x"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound for unknown task id, got %v", err)
	}
}

// TestUnschedulableReason_ClearedOnLease verifies that a lease clears a stale
// unschedulable_reason left over from the scheduler's sweep — the reason is
// only meaningful while a task is ready, and a lease moves the task to
// assigned. It replaces the same-named property of the per-row assignment
// write, which the lease superseded.
func TestUnschedulableReason_ClearedOnLease(t *testing.T) {
	s := New()
	defer s.Close()

	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	newJob("j1", "farm-f1", "q1").task("t1", "s1", store.TaskStatusReady).submit(t, s)

	if _, err := s.SetTaskUnschedulableReason(ctx(), "t1", "no eligible online worker"); err != nil {
		t.Fatalf("SetTaskUnschedulableReason: %v", err)
	}

	leaseTask(t, s, "t1", "worker-1")

	tk := mustGetTask(t, s, "t1")
	if tk.UnschedulableReason != "" {
		t.Errorf("UnschedulableReason after LeaseTask: got %q, want empty", tk.UnschedulableReason)
	}
}

// TestUnschedulableReason_ClearedOnCancel verifies that both cancel paths clear
// a stale unschedulable_reason on the tasks they cancel. It replaces the
// same-named property of the per-row cancel write and of the blind status
// write, which the two execution cancels superseded.
func TestUnschedulableReason_ClearedOnCancel(t *testing.T) {
	cancels := map[string]func(t *testing.T, s *Store){
		"CancelJobExecution": func(t *testing.T, s *Store) {
			t.Helper()
			if _, err := s.CancelJobExecution(ctx(), "j1", "", time.Now().UTC()); err != nil {
				t.Fatalf("CancelJobExecution: %v", err)
			}
		},
		"CancelTaskExecution": func(t *testing.T, s *Store) {
			t.Helper()
			if _, ok, err := s.CancelTaskExecution(ctx(), "t1", "", time.Now().UTC()); err != nil || !ok {
				t.Fatalf("CancelTaskExecution = (%v, %v), want canceled", ok, err)
			}
		},
	}
	for name, cancel := range cancels {
		t.Run(name, func(t *testing.T) {
			s := New()
			defer s.Close()

			mustCreateFarm(t, s, "f1")
			mustCreateQueue(t, s, "farm-f1", "q1", "q1")
			newJob("j1", "farm-f1", "q1").task("t1", "s1", store.TaskStatusReady).submit(t, s)

			if _, err := s.SetTaskUnschedulableReason(ctx(), "t1", "no eligible online worker"); err != nil {
				t.Fatalf("SetTaskUnschedulableReason: %v", err)
			}

			cancel(t, s)

			tk := mustGetTask(t, s, "t1")
			if tk.Status != store.TaskStatusCanceled {
				t.Fatalf("task status = %q, want canceled", tk.Status)
			}
			if tk.UnschedulableReason != "" {
				t.Errorf("UnschedulableReason after %s: got %q, want empty", name, tk.UnschedulableReason)
			}
		})
	}
}

func TestListReadyTasks(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	newJob("j1", "farm-f1", "q1").
		task("t-ready", "s1", store.TaskStatusReady).
		task("t-running", "s1", store.TaskStatusReady).
		submit(t, s)
	runTask(t, s, "t-running", "w1")

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
	now := time.Now()
	future := now.Add(time.Minute)
	newJob("j1", "farm-f1", "q1").
		task("t-ready", "s1", store.TaskStatusReady).
		taskRow(store.Task{ID: "t-backoff", StepID: "s1", Status: store.TaskStatusReady, RetryAfter: &future}).
		submit(t, s)
	newJob("j2", "farm-f1", "q1").as(store.JobStatusPaused).
		task("t-paused", "s2", store.TaskStatusReady).
		submit(t, s)

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
	newJob("j1", "farm-f1", "q1").task("t1", "s1", store.TaskStatusReady).submit(t, s)

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
	newJob("j1", "farm-f1", "q1").
		task("t1", "s1", store.TaskStatusReady).
		task("t2", "s1", store.TaskStatusReady).
		task("t3", "s1", store.TaskStatusSucceeded).
		submit(t, s)
	runTask(t, s, "t1", "w1")
	leaseTask(t, s, "t2", "w1")

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
	newJob("j1", "farm-f1", "q1").
		task("t1", "s1", store.TaskStatusReady).
		task("t2", "s1", store.TaskStatusReady).
		submit(t, s)
	leaseTask(t, s, "t1", "w1")

	n, err := s.CountActiveTasksInFarm(ctx(), "farm-f1")
	if err != nil {
		t.Fatalf("CountActiveTasksInFarm: %v", err)
	}
	if n != 1 {
		t.Errorf("count = %d, want 1", n)
	}
}

func TestCountReadyTasksByQueue(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	newJob("j1", "farm-f1", "q1").
		task("t1", "s1", store.TaskStatusReady).
		task("t2", "s1", store.TaskStatusReady).
		task("t3", "s1", store.TaskStatusReady).
		submit(t, s)
	runTask(t, s, "t3", "w1")

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
	newJob("j1", "farm-f1", "q1").
		task("t-ok", "s1", store.TaskStatusReady).
		task("t-backoff", "s1", store.TaskStatusReady).
		submit(t, s)

	// Backing off: ready but retry_after has not elapsed (requeued from
	// running, as the auto-retry path does). The requeue is guarded on the
	// reporting attempt being the task's latest, so the task is leased and its
	// attempt failed first, as a worker's failure report does.
	storetest.FailAndRequeue(t, s, store.LeaseRequest{TaskID: "t-backoff", WorkerID: "w1"}, now.Add(time.Minute))

	// Under an auto-parked job.
	newJob("j-parked", "farm-f1", "q1").task("t-parked", "s2", store.TaskStatusReady).submit(t, s)
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
	parked := newJob("j-parked", "farm-f1", "q1").submit(t, s).Job
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
	manual := newJob("j-manual", "farm-f1", "q1").submit(t, s).Job
	manual.FailedAttempts = 2
	if _, err := s.UpdateJob(ctx(), manual); err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}
	if err := s.PauseJob(ctx(), "j-manual", now); err != nil {
		t.Fatalf("PauseJob: %v", err)
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
	newJob("j1", "farm-f1", "q1").
		task("t1", "s1", store.TaskStatusReady).
		task("t2", "s1", store.TaskStatusReady).
		task("t3", "s1", store.TaskStatusSucceeded).
		submit(t, s)

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
	newJob("j1", "farm-f1", "q1").
		task("t1", "s1", store.TaskStatusReady).
		task("t2", "s1", store.TaskStatusReady).
		task("t3", "s1", store.TaskStatusReady).
		submit(t, s)

	for _, id := range []string{"t1", "t2"} {
		if _, err := s.SetTaskUnschedulableReason(ctx(), id, "no worker matches required capability"); err != nil {
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

	if _, err := s.SetTaskUnschedulableReason(ctx(), "t1", ""); err != nil {
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
	// Mixed reasons: staging x2, timeout x1 — staging dominates.
	// A succeeded task must never count, even with a stray reason. No production
	// write leaves a reason on a succeeded task, so t3 is seeded with one at
	// create, which is the only way to build that state.
	newJob("j1", "farm-f1", "q1").
		taskRow(store.Task{ID: "t0", StepID: "s1", Status: store.TaskStatusFailed, FailureReason: "staging"}).
		taskRow(store.Task{ID: "t1", StepID: "s1", Status: store.TaskStatusFailed, FailureReason: "staging"}).
		taskRow(store.Task{ID: "t2", StepID: "s1", Status: store.TaskStatusFailed, FailureReason: "timeout"}).
		taskRow(store.Task{ID: "t3", StepID: "s1", Status: store.TaskStatusSucceeded, FailureReason: "staging"}).
		submit(t, s)

	sum, err := s.FailureReasonSummary(ctx(), "j1")
	if err != nil {
		t.Fatalf("FailureReasonSummary: %v", err)
	}
	if sum.FailedCount != 3 || sum.DominantReason != "staging" || sum.DistinctReasons != 2 {
		t.Fatalf("got %+v, want {FailedCount:3 DominantReason:staging DistinctReasons:2}", sum)
	}

	// Tie case: two reasons each with count 1 — dominant is the
	// lexicographically smaller reason, deterministically.
	newJob("j2", "farm-f1", "q1").
		taskRow(store.Task{ID: "u0", StepID: "s2", Status: store.TaskStatusFailed, FailureReason: "timeout"}).
		taskRow(store.Task{ID: "u1", StepID: "s2", Status: store.TaskStatusFailed, FailureReason: "staging"}).
		submit(t, s)
	sum, err = s.FailureReasonSummary(ctx(), "j2")
	if err != nil {
		t.Fatalf("FailureReasonSummary(j2): %v", err)
	}
	if sum.FailedCount != 2 || sum.DominantReason != "staging" || sum.DistinctReasons != 2 {
		t.Fatalf("got %+v, want {FailedCount:2 DominantReason:staging DistinctReasons:2}", sum)
	}

	// Empty case: a job with no failed tasks carrying a reason.
	newJob("j3", "farm-f1", "q1").task("v0", "s3", store.TaskStatusSucceeded).submit(t, s)
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
	newJob("j1", "farm-f1", "q1").
		task("b-task", "s1", store.TaskStatusReady).
		task("a-task", "s1", store.TaskStatusFailed).
		submit(t, s)
	runTask(t, s, "b-task", "w1")

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
	newJob("j1", "farm-f1", "q1").task("t1", "s1", store.TaskStatusReady).submit(t, s)
	leaseTask(t, s, "t1", "w1")

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
	newJob("j1", "farm-f1", "q1").
		task("t-failed", "s1", store.TaskStatusFailed).
		task("t-canceled", "s1", store.TaskStatusCanceled).
		task("t-ready", "s1", store.TaskStatusReady).
		submit(t, s)

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
	// The job is terminal, to prove revival resets it.
	newJob("j1", "farm-f1", "q1").as(store.JobStatusFailed).
		step("s1", store.StepStatusFailed).
		task("t-failed", "s1", store.TaskStatusFailed).
		task("t-canceled", "s1", store.TaskStatusCanceled).
		task("t-ok", "s1", store.TaskStatusSucceeded).
		submit(t, s)

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
	// The job stays running (non-terminal).
	newJob("j1", "farm-f1", "q1").
		step("s1", store.StepStatusFailed).
		task("t1", "s1", store.TaskStatusFailed).
		task("t2", "s1", store.TaskStatusFailed).
		submit(t, s)

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

	seed := newJob("j1", "farm-f1", "q1").as(store.JobStatusFailed)
	seed.sub.Job.FailedAttempts, seed.sub.Job.ParkReason = 1, "failure limit reached (1)"
	retryAfter := time.Now().Add(time.Minute)
	j := seed.step("s1", store.StepStatusFailed).
		taskRow(store.Task{
			ID: "t1", StepID: "s1", Status: store.TaskStatusFailed,
			FailedAttempts: 1, RetryAfter: &retryAfter,
		}).
		submit(t, s).Job

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
	newJob("j1", "farm-f1", "q1").
		step("s1", store.StepStatusCompleted).
		task("t-ok", "s1", store.TaskStatusSucceeded).
		submit(t, s)

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

func TestDeleteOfflineWorkersBefore(t *testing.T) {
	s := New()
	defer s.Close()

	old := time.Now().Add(-2 * time.Hour)
	recent := time.Now().Add(-time.Minute)

	// w1: offline + stale → removed.
	mustCreateWorker(t, s, "w1", "f1", store.WorkerStatusOnline)
	mustHeartbeat(t, s, "w1", old)
	mustOffline(t, s, "w1")
	// w2: offline + recent → kept.
	mustCreateWorker(t, s, "w2", "f1", store.WorkerStatusOnline)
	mustHeartbeat(t, s, "w2", recent)
	mustOffline(t, s, "w2")
	// w3: offline + stale but disabled → kept (an operator removes it).
	mustCreateWorker(t, s, "w3", "f1", store.WorkerStatusOnline)
	mustHeartbeat(t, s, "w3", old)
	mustOffline(t, s, "w3")
	if _, err := s.SetWorkerDisabled(ctx(), "w3", true); err != nil {
		t.Fatalf("SetWorkerDisabled: %v", err)
	}

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

// mustOffline takes the worker offline as a graceful deregister does; it keeps
// the heartbeat already recorded.
func mustOffline(t *testing.T, s *Store, id string) {
	t.Helper()
	if _, _, err := s.OfflineWorker(ctx(), id, "", time.Now()); err != nil {
		t.Fatalf("OfflineWorker(%q): %v", id, err)
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
	newJob("j1", "farm-f1", "q1").task("t1", "s1", store.TaskStatusReady).submit(t, s)
	mustCreateWorker(t, s, "w-idle", "farm-f1", store.WorkerStatusOnline)
	mustCreateWorker(t, s, "w-busy", "farm-f1", store.WorkerStatusOnline)

	// w-busy has a running task.
	runTask(t, s, "t1", "w-busy")

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
		if _, _, err := st.RegisterWorker(ctx(), store.Worker{
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
	newJob("j1", "farm-f1", "q1").submit(t, s)

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

func TestCancelJobStatus(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	newJob("j1", "farm-f1", "q1").submit(t, s)

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
	newJob("j1", "farm-f1", "q1").as(store.JobStatusCompleted).submit(t, s)

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
	newJob("j1", "farm-f1", "q1").submit(t, s)
	newJob("j2", "farm-f1", "q1").submit(t, s)

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
		storetest.Submit(t, st, store.JobSubmission{Job: store.Job{
			ID: id, FarmID: "f1", QueueID: "q1", Name: name, Owner: owner,
			Project: project, Status: store.JobStatusPending, Priority: 50,
			TemplateFormat: store.TemplateFormatYAML,
		}})
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
	seedReadyTask(t, s)
	leased := leaseTask(t, s, "t1", "w1")

	a, err := s.GetTaskAttempt(ctx(), leased.ID)
	if err != nil {
		t.Fatalf("GetTaskAttempt: %v", err)
	}
	if a.ID != leased.ID {
		t.Errorf("id = %q, want %q", a.ID, leased.ID)
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
	seedReadyTask(t, s)
	storetest.FailAndRequeue(t, s, store.LeaseRequest{TaskID: "t1", WorkerID: "w1"}, time.Now().UTC().Add(-time.Minute))
	second := leaseTask(t, s, "t1", "w1")

	a, err := s.LatestTaskAttempt(ctx(), "t1")
	if err != nil {
		t.Fatalf("LatestTaskAttempt: %v", err)
	}
	if a.ID != second.ID || a.AttemptNumber != 2 {
		t.Errorf("latest = %q (attempt %d), want %q (attempt 2)", a.ID, a.AttemptNumber, second.ID)
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
	seedReadyTask(t, s)
	storetest.FailAndRequeue(t, s, store.LeaseRequest{TaskID: "t1", WorkerID: "w1"}, time.Now().UTC().Add(-time.Minute))
	leaseTask(t, s, "t1", "w1")

	attempts, err := s.ListTaskAttempts(ctx(), "t1")
	if err != nil {
		t.Fatalf("ListTaskAttempts: %v", err)
	}
	if len(attempts) != 2 || attempts[0].AttemptNumber != 1 {
		t.Errorf("expected 2 attempts ordered asc; got %v", attempts)
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

// TestInjectClaim_ConflictOnDuplicateActiveClaim pins that the fake's injector
// keeps the unique-active-claim check SQLite's (attempt, pool) index enforces,
// so a fixture cannot build a state SQLite would refuse. It replaces the same
// property of the per-row claim write, which the injector superseded. (The
// lease's own duplicate-pool conflict is TestLeaseTask_SamePoolTwice.)
func TestInjectClaim_ConflictOnDuplicateActiveClaim(t *testing.T) {
	s := New()
	ctx := t.Context()
	c := store.UsageClaim{ID: "c1", PoolID: "p1", TaskAttemptID: "a1"}
	if _, err := s.InjectClaim(ctx, c); err != nil {
		t.Fatalf("first InjectClaim: %v", err)
	}
	c.ID = "c2"
	if _, err := s.InjectClaim(ctx, c); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second InjectClaim error = %v, want ErrConflict", err)
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

func TestActiveClaimCount(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	pool := mustCreatePool(t, s, "p1", 0)
	newJob("j1", "farm-f1", "q1").
		task("t1", "s1", store.TaskStatusReady).
		task("t2", "s1", store.TaskStatusReady).
		submit(t, s)

	claimOn(t, s, "t1", "co1", pool)
	claimOn(t, s, "t2", "co2", pool)

	if n := mustActiveClaimCount(t, s, "p1"); n != 2 {
		t.Errorf("count = %d, want 2", n)
	}
}

func TestListUsagePoolUtilization(t *testing.T) {
	s := New()
	defer s.Close()
	mustCreateFarm(t, s, "f1")
	mustCreateQueue(t, s, "farm-f1", "q1", "q1")
	newJob("j1", "farm-f1", "q1").
		task("t1", "s1", store.TaskStatusReady).
		task("t2", "s1", store.TaskStatusReady).
		task("t3", "s1", store.TaskStatusReady).
		submit(t, s)

	arnold, err := s.CreateUsagePool(ctx(), store.UsagePool{ID: "p-arnold", Name: "arnold", MaxConcurrent: 5})
	if err != nil {
		t.Fatalf("CreateUsagePool arnold: %v", err)
	}
	if _, err := s.CreateUsagePool(ctx(), store.UsagePool{ID: "p-maya", Name: "maya", MaxConcurrent: 3}); err != nil {
		t.Fatalf("CreateUsagePool maya: %v", err)
	}

	// Two active claims on arnold, one released (should not count). The third
	// attempt completes, as a worker's success report does, which releases its
	// claim.
	claimOn(t, s, "t1", "co1", arnold)
	claimOn(t, s, "t2", "co2", arnold)
	done := claimOn(t, s, "t3", "co3", arnold)
	storetest.Complete(t, s, done, store.TaskStatusSucceeded)

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

	// Seed job + children. The task is leased holding a claim, so it has an
	// attempt and an active claim for the delete to remove.
	mustCreateFarm(t, st, "f1")
	mustCreateQueue(t, st, "farm-f1", "q1", "q1")
	mustCreatePool(t, st, "pool1", 0)
	newJob(jobID, "farm-f1", "q1").step("s1", store.StepStatusPending).task("t1", "s1", store.TaskStatusReady).submit(t, st)
	storetest.Lease(t, st, store.LeaseRequest{
		TaskID: "t1", WorkerID: "w1", AttemptID: "a1",
		Claims: []store.UsagePoolClaim{{ClaimID: "cl1", PoolID: "pool1", PoolName: "pool1"}},
	})
	if n := mustActiveClaimCount(t, st, "pool1"); n != 1 {
		t.Fatalf("seeded active claims = %d, want 1, or the delete below proves nothing", n)
	}

	if _, err := st.CreateTaskLog(ctx, store.TaskLog{
		ID: "log1", TaskID: "t1", AttemptID: "a1",
		SeqNum: 1, NATSSeq: 1, Stream: store.LogStreamStdout,
	}); err != nil {
		t.Fatalf("CreateTaskLog: %v", err)
	}

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
		storetest.Submit(t, st, store.JobSubmission{Job: store.Job{ID: id, Status: status, CompletedAt: &c}})
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

// TestFakeStore_DeleteTerminalJobsBefore_KeepsUpstreamNeededByBlockedDependent
// mirrors the sqlite retention-purge fix test: a completed upstream older
// than the cutoff must not be purged while a dependent still waits on it
// (blocked), since purging it would make the reconciler read a missing
// upstream and wrongly cancel the dependent despite the upstream having
// succeeded. Once the dependent reaches a terminal status, the upstream
// becomes eligible for purge again.
func TestFakeStore_DeleteTerminalJobsBefore_KeepsUpstreamNeededByBlockedDependent(t *testing.T) {
	ctx := context.Background()
	cutoff := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	old := cutoff.Add(-time.Hour)

	st := New()
	c := old
	storetest.Submit(t, st, store.JobSubmission{Job: store.Job{ID: "upstream-old", Status: store.JobStatusCompleted, CompletedAt: &c}})
	storetest.Submit(t, st, store.JobSubmission{
		Job:       store.Job{ID: "dependent-blocked", Status: store.JobStatusBlocked},
		DependsOn: []string{"upstream-old"},
	})

	got, err := st.DeleteTerminalJobsBefore(ctx, cutoff, false)
	if err != nil {
		t.Fatalf("DeleteTerminalJobsBefore: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("deleted = %v, want none (upstream still needed by blocked dependent)", got)
	}
	if _, err := st.GetJob(ctx, "upstream-old"); err != nil {
		t.Fatalf("GetJob(upstream-old) after guarded sweep: %v", err)
	}

	// Once the dependent is terminal, the upstream is no longer protected.
	if canceled, _, err := st.CancelBlockedJob(ctx, "dependent-blocked", "test", time.Now().UTC()); err != nil || !canceled {
		t.Fatalf("CancelBlockedJob = (%v, %v), want canceled", canceled, err)
	}
	got, err = st.DeleteTerminalJobsBefore(ctx, cutoff, false)
	if err != nil {
		t.Fatalf("DeleteTerminalJobsBefore (2nd sweep): %v", err)
	}
	if ids := deletedFakeIDs(got); !slices.Equal(ids, []string{"upstream-old"}) {
		t.Fatalf("deleted = %v, want [upstream-old]", ids)
	}
	if _, err := st.GetJob(ctx, "upstream-old"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetJob(upstream-old) after unblock sweep = %v, want ErrNotFound", err)
	}
}
