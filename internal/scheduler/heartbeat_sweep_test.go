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
	"github.com/uberware/sqi/internal/ws"
)

// jobRecordingNotifier captures NotifyJob events; other Notify* methods are
// inherited as no-ops from ws.NoopNotifier.
type jobRecordingNotifier struct {
	ws.NoopNotifier

	jobs []ws.JobEvent
}

func (n *jobRecordingNotifier) NotifyJob(e ws.JobEvent) { n.jobs = append(n.jobs, e) }

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

// seedAssignedTask seeds a job/step/task in 'assigned' on a (live) worker with
// the given assigned_at age, plus an open running attempt. Returns task/attempt IDs.
func seedAssignedTask(t *testing.T, st *fake.Store, age time.Duration) (taskID, attemptID string) {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC()
	assignedAt := now.Add(-age)

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
		Status: store.TaskStatusAssigned, AssignedWorkerID: "w-live", AssignedAt: &assignedAt,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	attempt, err := st.CreateTaskAttempt(ctx, store.TaskAttempt{
		ID: uuid.NewString(), TaskID: task.ID, WorkerID: "w-live", AttemptNumber: 1,
		Status: store.AttemptStatusRunning, StartedAt: assignedAt, CreatedAt: assignedAt,
	})
	if err != nil {
		t.Fatalf("CreateTaskAttempt: %v", err)
	}
	return task.ID, attempt.ID
}

// TestReapStaleAssignedTasks_ReclaimsStuckTask verifies that a task stranded in
// 'assigned' longer than AssignedTaskTimeout is returned to the ready queue and
// its provisional attempt is closed — independent of any worker's liveness.
func TestReapStaleAssignedTasks_ReclaimsStuckTask(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "farm-1")
	// AssignedTaskTimeout default is 10m; age the assignment well beyond it.
	taskID, attemptID := seedAssignedTask(t, st, time.Hour)

	s.reapStaleAssignedTasks(t.Context())

	tk, err := st.GetTask(t.Context(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if tk.Status != store.TaskStatusReady {
		t.Errorf("task status = %q, want ready (reaped)", tk.Status)
	}
	if tk.AssignedWorkerID != "" {
		t.Errorf("task still assigned to %q, want cleared", tk.AssignedWorkerID)
	}
	att, err := st.GetTaskAttempt(t.Context(), attemptID)
	if err != nil {
		t.Fatalf("GetTaskAttempt: %v", err)
	}
	if att.Status != store.AttemptStatusFailed {
		t.Errorf("attempt status = %q, want failed", att.Status)
	}
	if att.EndedAt == nil {
		t.Error("expected reaped attempt EndedAt set")
	}
}

// TestReapStaleAssignedTasks_LeavesFreshAssignment verifies that a recently
// assigned task (within AssignedTaskTimeout) is not disturbed by the reaper.
func TestReapStaleAssignedTasks_LeavesFreshAssignment(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "farm-1")
	taskID, _ := seedAssignedTask(t, st, 5*time.Second)

	s.reapStaleAssignedTasks(t.Context())

	tk, err := st.GetTask(t.Context(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if tk.Status != store.TaskStatusAssigned {
		t.Errorf("task status = %q, want assigned (still within timeout)", tk.Status)
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

// TestDefaultConfig_AssignedTaskTimeoutTightened verifies that AssignedTaskTimeout
// is now <= 60 s, since "assigned" is a brief leased->running window and no longer
// needs to accommodate the removed SQI_WORK stream's MaxAge.
func TestDefaultConfig_AssignedTaskTimeoutTightened(t *testing.T) {
	if got := DefaultConfig().AssignedTaskTimeout; got > 60*time.Second {
		t.Errorf("AssignedTaskTimeout = %v, want <= 60s after lease cutover", got)
	}
}

func TestSweepStaleWorkers_ListError_ReturnsQuietly(t *testing.T) {
	st := &listStaleErrSt{Store: fake.New()}
	s := newMetricsScheduler(st, &recordBus{}, "farm-1")

	// Should not panic and should simply return on the store error.
	s.sweepStaleWorkers(t.Context())
}

// TestSweepThenDemote_StalledJobReturnsToPending reproduces the reported issue:
// a worker dies while running a job's only in-flight task, the sweep returns the
// task to ready, and — with no worker left to pick it up — the job must not stay
// marked 'running'. The heartbeat-sweep tick runs sweep then demote, so the job
// is reconciled back to 'pending' and a JobEvent is emitted.
func TestSweepThenDemote_StalledJobReturnsToPending(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "farm-1")
	notifier := &jobRecordingNotifier{}
	s.notifier = notifier

	_, taskID, _ := seedStaleWorkerWithTask(t, st, time.Hour)
	tk, err := st.GetTask(t.Context(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	jobID := tk.JobID

	// Heartbeat-sweep tick body.
	s.sweepStaleWorkers(t.Context())
	s.reapStaleAssignedTasks(t.Context())
	s.demoteStalledJobs(t.Context())

	// Task was reclaimed to ready by the sweep …
	tk, err = st.GetTask(t.Context(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if tk.Status != store.TaskStatusReady {
		t.Fatalf("task status = %q, want ready", tk.Status)
	}

	// … and the now-idle job was demoted from running back to pending.
	job, err := st.GetJob(t.Context(), jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Status != store.JobStatusPending {
		t.Errorf("job status = %q, want pending (no in-flight tasks)", job.Status)
	}

	// A JobEvent must have been fanned out so the UI reflects the change.
	var found bool
	for _, e := range notifier.jobs {
		if e.JobID == jobID && e.Status == string(store.JobStatusPending) {
			found = true
		}
	}
	if !found {
		t.Errorf("no pending JobEvent emitted for %q; events = %+v", jobID, notifier.jobs)
	}
}
