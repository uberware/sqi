// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Tests for failure.go — the auto-retry failure fork (handleTaskFailed).
//
// White-box tests in package scheduler driven by a fake store, mirroring the
// scaffolding in taskstatus_test.go (seed job/step/task/attempt, drive a
// protocol.TaskStatusMsg through processTaskStatus, assert on the resulting
// store state). newFailureHarness additionally seeds a farm/queue (required
// by handleTaskFailed's policy resolution) and lets each test configure its
// own retry policy via the scheduler's Config, since that's what actually
// drives the fork.

import (
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/ws"
)

// ── harness ──────────────────────────────────────────────────────────────────

// failureHarness wires a *Scheduler with a fake store and the given retry
// policy (applied as the server-level Config defaults, since none of the
// fixtures in this file set job/queue/farm-level overrides).
type failureHarness struct {
	t  *testing.T
	st *fake.Store
	s  *Scheduler

	// current holds each task's most recently created attempt, so reportFailed
	// can look up the right AttemptID without the caller threading it through.
	current map[string]store.TaskAttempt
}

func newFailureHarness(t *testing.T, policy RetryPolicy) *failureHarness {
	t.Helper()
	st := fake.New()

	cfg := DefaultConfig()
	cfg.DefaultMaxAttempts = policy.MaxAttempts
	cfg.RetryDelay = policy.RetryDelay
	cfg.DefaultFailureLimit = policy.FailureLimit

	s := New(
		cfg,
		st,
		nil, // bus — not called by processTaskStatus
		metrics.New(),
		slog.New(slog.DiscardHandler),
		ws.NoopNotifier{},
		nil, // diagBuf — diagnostics disabled
	)
	s.ctx = t.Context()

	h := &failureHarness{t: t, st: st, s: s, current: map[string]store.TaskAttempt{}}

	if _, err := st.CreateFarm(t.Context(), store.Farm{ID: "farm-1", Name: "farm-1"}); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	if _, err := st.CreateQueue(t.Context(), store.Queue{ID: "queue-1", FarmID: "farm-1", Name: "queue-1"}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}

	return h
}

// seedRunningTask creates jobID (running) with a single running step/task and
// an open running attempt on workerID, plus a registered worker record (so the
// reclaim path used by TestHandleTaskFailed_LostWorkDoesNotCount has
// something to reclaim from).
func (h *failureHarness) seedRunningTask(jobID, taskID, workerID string) {
	h.t.Helper()
	ctx := h.t.Context()
	now := time.Now().UTC()

	if _, err := h.st.RegisterWorker(ctx, store.Worker{
		ID: workerID, FarmID: "farm-1", Hostname: workerID,
		Status: store.WorkerStatusOnline, CPUCount: 4,
	}); err != nil {
		h.t.Fatalf("RegisterWorker: %v", err)
	}

	if _, err := h.st.CreateJob(ctx, store.Job{
		ID: jobID, FarmID: "farm-1", QueueID: "queue-1", Name: jobID,
		Status: store.JobStatusRunning, TemplateFormat: store.TemplateFormatJSON,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		h.t.Fatalf("CreateJob: %v", err)
	}

	step, err := h.st.CreateStep(ctx, store.Step{
		ID: taskID + "-step", JobID: jobID, Name: "Step1",
		Status: store.StepStatusRunning, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		h.t.Fatalf("CreateStep: %v", err)
	}

	if _, err := h.st.CreateTask(ctx, store.Task{
		ID: taskID, JobID: jobID, StepID: step.ID, Name: taskID,
		Status: store.TaskStatusRunning, AssignedWorkerID: workerID,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		h.t.Fatalf("CreateTask: %v", err)
	}

	h.newAttempt(taskID, workerID)
}

// newAttempt creates a fresh running attempt for taskID on workerID and
// records it as the "current" attempt reportFailed will target.
func (h *failureHarness) newAttempt(taskID, workerID string) store.TaskAttempt {
	h.t.Helper()
	ctx := h.t.Context()
	now := time.Now().UTC()

	attempt, err := h.st.CreateTaskAttempt(ctx, store.TaskAttempt{
		ID: uuid.NewString(), TaskID: taskID, WorkerID: workerID,
		AttemptNumber: len(h.current) + 1, Status: store.AttemptStatusRunning,
		StartedAt: now, CreatedAt: now,
	})
	if err != nil {
		h.t.Fatalf("CreateTaskAttempt: %v", err)
	}
	h.current[taskID] = attempt
	return attempt
}

// reportFailed drives a "failed" TaskStatusMsg for taskID's current attempt
// through the real processTaskStatus entry point (the same path
// handleTaskStatusMessage uses for a worker-published task.status message).
func (h *failureHarness) reportFailed(taskID string) {
	h.t.Helper()
	attempt, ok := h.current[taskID]
	if !ok {
		h.t.Fatalf("reportFailed: no attempt recorded for task %s", taskID)
	}
	exitCode := 1
	msg := protocol.TaskStatusMsg{
		TaskID:    taskID,
		AttemptID: attempt.ID,
		Status:    "failed",
		ExitCode:  &exitCode,
		At:        time.Now().UTC(),
	}
	if err := h.s.processTaskStatus(h.t.Context(), msg); err != nil {
		h.t.Fatalf("processTaskStatus(failed): %v", err)
	}
}

// reassignAndReportFailed simulates a worker re-leasing the retried task: it
// opens a new attempt on workerID, then reports that attempt failed.
func (h *failureHarness) reassignAndReportFailed(taskID, workerID string) {
	h.t.Helper()
	h.newAttempt(taskID, workerID)
	h.reportFailed(taskID)
}

// reclaimWorker drives the existing offline-worker reclaim path directly
// (the same call sweepStaleWorkers makes once a worker's heartbeat goes
// stale) — a "lost work" event distinct from a worker-reported failure.
func (h *failureHarness) reclaimWorker(workerID string) {
	h.t.Helper()
	h.s.reclaimOfflineWorkerTasks(h.t.Context(), workerID, workerID)
}

func (h *failureHarness) taskStatus(taskID string) store.TaskStatus {
	h.t.Helper()
	task, err := h.st.GetTask(h.t.Context(), taskID)
	if err != nil {
		h.t.Fatalf("GetTask(%s): %v", taskID, err)
	}
	return task.Status
}

func (h *failureHarness) taskFailedAttempts(taskID string) int {
	h.t.Helper()
	task, err := h.st.GetTask(h.t.Context(), taskID)
	if err != nil {
		h.t.Fatalf("GetTask(%s): %v", taskID, err)
	}
	return task.FailedAttempts
}

func (h *failureHarness) jobStatus(jobID string) store.JobStatus {
	h.t.Helper()
	job, err := h.st.GetJob(h.t.Context(), jobID)
	if err != nil {
		h.t.Fatalf("GetJob(%s): %v", jobID, err)
	}
	return job.Status
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestHandleTaskFailed_RetriesUntilCeiling(t *testing.T) {
	// max_attempts = 2, retry_delay = 0 (synchronous wake, no timer flakiness).
	// First failure -> task back to ready (retry_after set), NOT failed.
	// Second genuine failure -> task terminal failed, step/job completion runs.
	h := newFailureHarness(t, RetryPolicy{MaxAttempts: 2, RetryDelay: 0, FailureLimit: 0})
	h.seedRunningTask("j1", "t1", "w1")

	h.reportFailed("t1")
	if got := h.taskStatus("t1"); got != store.TaskStatusReady {
		t.Fatalf("after 1st failure want ready, got %s", got)
	}
	if got := h.taskFailedAttempts("t1"); got != 1 {
		t.Fatalf("want failed_attempts 1, got %d", got)
	}
	if got := h.jobStatus("j1"); got != store.JobStatusRunning {
		t.Fatalf("job should remain running after a retry, got %s", got)
	}

	h.reassignAndReportFailed("t1", "w1") // second genuine failure
	if got := h.taskStatus("t1"); got != store.TaskStatusFailed {
		t.Fatalf("after ceiling want failed, got %s", got)
	}
	if got := h.taskFailedAttempts("t1"); got != 2 {
		t.Fatalf("want failed_attempts 2, got %d", got)
	}
	// Single-step job with its only task now terminal-failed: the job must
	// have cascaded to failed too (checkStepCompletion/checkJobCompletion).
	if got := h.jobStatus("j1"); got != store.JobStatusFailed {
		t.Fatalf("job should be failed once the task is exhausted, got %s", got)
	}
}

func TestHandleTaskFailed_ParksJobAtFailureLimit(t *testing.T) {
	h := newFailureHarness(t, RetryPolicy{MaxAttempts: 5, RetryDelay: 0, FailureLimit: 1})
	h.seedRunningTask("j1", "t1", "w1")

	h.reportFailed("t1")

	if got := h.jobStatus("j1"); got != store.JobStatusPaused {
		t.Fatalf("want job paused at failure limit, got %s", got)
	}
	if got := h.taskStatus("t1"); got != store.TaskStatusFailed {
		t.Fatalf("tripping task should be terminal failed, got %s", got)
	}
}

func TestHandleTaskFailed_LostWorkDoesNotCount(t *testing.T) {
	// A worker-offline reclaim must NOT increment failed_attempts — only a
	// worker-reported "failed" status is a genuine failure.
	h := newFailureHarness(t, RetryPolicy{MaxAttempts: 2, RetryDelay: 0})
	h.seedRunningTask("j1", "t1", "w1")

	h.reclaimWorker("w1")

	if got := h.taskFailedAttempts("t1"); got != 0 {
		t.Fatalf("lost work must not count as a genuine failure, got failed_attempts=%d", got)
	}
	if got := h.taskStatus("t1"); got != store.TaskStatusReady {
		t.Fatalf("reclaimed task should be back to ready, got %s", got)
	}
}
