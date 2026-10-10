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
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/store/storetest"
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
	st := newCheckedFake(t)

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
//
// siblingTaskIDs are further tasks of the same step, still ready. Because
// they are non-terminal, the step (and therefore the job) is NOT complete when
// taskID fails — which is exactly what a park must survive: holding a job that
// still has work. The whole job is one submission; taskID is then leased and
// started through production writes, so its attempt is the one that lease
// opened.
func (h *failureHarness) seedRunningTask(jobID, taskID, workerID string, siblingTaskIDs ...string) {
	h.t.Helper()

	if _, _, err := h.st.RegisterWorker(h.t.Context(), store.Worker{
		ID: workerID, FarmID: "farm-1", Hostname: workerID,
		Status: store.WorkerStatusOnline, CPUCount: 4,
	}); err != nil {
		h.t.Fatalf("RegisterWorker: %v", err)
	}

	stepID := taskID + "-step"
	sub := store.JobSubmission{
		Job: store.Job{
			ID: jobID, FarmID: "farm-1", QueueID: "queue-1", Name: jobID,
			Status: store.JobStatusRunning, TemplateFormat: store.TemplateFormatJSON,
		},
		Steps: []store.Step{{ID: stepID, JobID: jobID, Name: "Step1", Status: store.StepStatusRunning}},
	}
	for _, id := range append([]string{taskID}, siblingTaskIDs...) {
		sub.Tasks = append(sub.Tasks, store.Task{
			ID: id, JobID: jobID, StepID: stepID, Name: id, Status: store.TaskStatusReady,
		})
	}
	storetest.Submit(h.t, h.st, sub)

	h.run(taskID, workerID)
}

// run leases taskID to workerID and starts it, as a worker taking the task
// does, and records the attempt the lease opened as the "current" attempt
// reportFailed will target.
func (h *failureHarness) run(taskID, workerID string) store.TaskAttempt {
	h.t.Helper()
	attempt := storetest.Running(h.t, h.st, store.LeaseRequest{TaskID: taskID, WorkerID: workerID})
	h.current[taskID] = attempt
	return attempt
}

// reportFailed drives a "failed" TaskStatusMsg for taskID's current attempt
// through the real processTaskStatus entry point (the same path
// handleTaskStatusMessage uses for a worker-published task.status message).
func (h *failureHarness) reportFailed(taskID string) {
	h.t.Helper()
	h.reportFailedWithMessage(taskID, "")
}

// reportFailedWithMessage is like reportFailed but sets the worker-reported
// Message on the TaskStatusMsg, exercising the failure-reason persistence
// path (RecordTaskFailure's attempt message and, on the terminal branch,
// CompleteTaskAttempt's FailureReason).
func (h *failureHarness) reportFailedWithMessage(taskID, message string) {
	h.t.Helper()
	attempt, ok := h.current[taskID]
	if !ok {
		h.t.Fatalf("reportFailedWithMessage: no attempt recorded for task %s", taskID)
	}
	exitCode := 1
	msg := protocol.TaskStatusMsg{
		Version:   protocol.ProtocolVersion,
		TaskID:    taskID,
		AttemptID: attempt.ID,
		Status:    "failed",
		ExitCode:  &exitCode,
		Message:   message,
		At:        time.Now().UTC(),
	}
	if err := h.s.processTaskStatus(h.t.Context(), attempt.WorkerID, msg); err != nil {
		h.t.Fatalf("processTaskStatus(failed): %v", err)
	}
}

// reassignAndReportFailed simulates a worker re-leasing the retried task: it
// opens a new attempt on workerID, then reports that attempt failed.
// reassignAndReportFailed models a genuine second attempt: a task sitting in
// ready after a retry is assigned to a worker and starts running before it
// fails again. The assigned/running steps are not decoration — the store
// enforces the state machine, and ready → failed is not an arrow. Skipping them
// would have this helper exercise a transition the store rejects (and rightly:
// a "failed" landing on a ready task means a stale attempt's message was
// redelivered after a retry already revived the task, which must not re-fail
// it). The lease takes the task ready → assigned and opens the new attempt,
// and the start takes it assigned → running.
func (h *failureHarness) reassignAndReportFailed(taskID, workerID string) {
	h.t.Helper()
	task, err := h.st.GetTask(h.t.Context(), taskID)
	if err != nil {
		h.t.Fatalf("reassignAndReportFailed: GetTask: %v", err)
	}
	if task.Status != store.TaskStatusReady {
		h.t.Fatalf("reassignAndReportFailed: cannot reassign from %q", task.Status)
	}
	h.run(taskID, workerID)
	h.reportFailed(taskID)
}

// reclaimWorker takes the worker offline through the store call the graceful
// deregister makes (the same transition sweepStaleWorkers makes once a worker's
// heartbeat goes stale, minus the staleness guard), then reports the reclaim as
// the scheduler does: a "lost work" event distinct from a worker-reported
// failure.
func (h *failureHarness) reclaimWorker(workerID string) {
	h.t.Helper()
	reclaimed, _, err := h.st.OfflineWorker(h.t.Context(), workerID, "", time.Now().UTC())
	if err != nil {
		h.t.Fatalf("OfflineWorker(%s): %v", workerID, err)
	}
	h.s.reclaimOfflineWorkerTasks(h.t.Context(), workerID, workerID, reclaimOffline, reclaimed)
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

func (h *failureHarness) jobFailedAttempts(jobID string) int {
	h.t.Helper()
	job, err := h.st.GetJob(h.t.Context(), jobID)
	if err != nil {
		h.t.Fatalf("GetJob(%s): %v", jobID, err)
	}
	return job.FailedAttempts
}

func (h *failureHarness) taskFailureReason(taskID string) string {
	h.t.Helper()
	task, err := h.st.GetTask(h.t.Context(), taskID)
	if err != nil {
		h.t.Fatalf("GetTask(%s): %v", taskID, err)
	}
	return task.FailureReason
}

// latestAttemptMessage returns the Message of the most recently created
// attempt for taskID (the one reportFailed*/newAttempt targets).
func (h *failureHarness) latestAttemptMessage(taskID string) string {
	h.t.Helper()
	attempt, ok := h.current[taskID]
	if !ok {
		h.t.Fatalf("latestAttemptMessage: no attempt recorded for task %s", taskID)
	}
	stored, err := h.st.GetTaskAttempt(h.t.Context(), attempt.ID)
	if err != nil {
		h.t.Fatalf("GetTaskAttempt(%s): %v", attempt.ID, err)
	}
	return stored.Message
}

func (h *failureHarness) parkReason(jobID string) string {
	h.t.Helper()
	job, err := h.st.GetJob(h.t.Context(), jobID)
	if err != nil {
		h.t.Fatalf("GetJob(%s): %v", jobID, err)
	}
	return job.ParkReason
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

// TestHandleTaskFailed_Retry_EvictsAttemptCache proves the RETRY branch of
// handleTaskFailed evicts the attempt-owner cache entry itself. That branch
// returns from retryTaskAfterFailure without ever reaching handleTaskTerminal,
// so the terminal path's own evict call cannot cover it — deleting
// handleTaskFailed's evict would leave every other test in this file green.
func TestHandleTaskFailed_Retry_EvictsAttemptCache(t *testing.T) {
	h := newFailureHarness(t, RetryPolicy{MaxAttempts: 2, RetryDelay: 0, FailureLimit: 0})
	h.seedRunningTask("j1", "t1", "w1")

	attempt := h.current["t1"]
	h.s.attemptCache.put(attempt.ID, attempt.WorkerID, attempt.TaskID)

	h.reportFailed("t1") // first failure: RETRY, not EXHAUSTED (ceiling is 2)

	if got := h.taskStatus("t1"); got != store.TaskStatusReady {
		t.Fatalf("expected retry to requeue the task, got %s", got)
	}
	if _, ok := h.s.attemptCache.get(attempt.ID); ok {
		t.Error("expected attempt-owner cache entry to be evicted on the retry branch")
	}
}

// TestHandleTaskFailed_RedeliveryCountsOnce is the IMP-1 regression at the
// scheduler layer. The task-status JetStream consumer is at-least-once, so the
// same "failed" message can be delivered more than once (NAK, AckWait expiry,
// MaxDeliver). A redelivery of an already-processed attempt must be counted
// exactly once — it must not push the task toward its ceiling or the job toward
// its failure limit, and must not strand the task.
func TestHandleTaskFailed_RedeliveryCountsOnce(t *testing.T) {
	h := newFailureHarness(t, RetryPolicy{MaxAttempts: 3, RetryDelay: 0, FailureLimit: 0})
	h.seedRunningTask("j1", "t1", "w1")

	exit := 1
	msg := protocol.TaskStatusMsg{
		Version:   protocol.ProtocolVersion,
		TaskID:    "t1",
		AttemptID: h.current["t1"].ID,
		Status:    "failed",
		ExitCode:  &exit,
		At:        time.Now().UTC(),
	}

	// First delivery: one genuine failure → retry, task back to ready.
	if err := h.s.processTaskStatus(t.Context(), h.current["t1"].WorkerID, msg); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if got := h.taskFailedAttempts("t1"); got != 1 {
		t.Fatalf("after first delivery want task failed_attempts 1, got %d", got)
	}
	if got := h.jobFailedAttempts("j1"); got != 1 {
		t.Fatalf("after first delivery want job failed_attempts 1, got %d", got)
	}
	if got := h.taskStatus("t1"); got != store.TaskStatusReady {
		t.Fatalf("after first delivery want ready, got %s", got)
	}

	// Redelivery of the SAME message (same attempt): the attempt is already
	// failed, so RecordTaskFailure returns the current counts without
	// re-incrementing, the retry decision is re-made identically, and the
	// requeue is re-applied harmlessly.
	if err := h.s.processTaskStatus(t.Context(), h.current["t1"].WorkerID, msg); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if got := h.taskFailedAttempts("t1"); got != 1 {
		t.Fatalf("redelivery must not re-count task: want 1, got %d", got)
	}
	if got := h.jobFailedAttempts("j1"); got != 1 {
		t.Fatalf("redelivery must not re-count job: want 1, got %d", got)
	}
	if got := h.taskStatus("t1"); got != store.TaskStatusReady {
		t.Fatalf("redelivery must leave task ready (not exhausted), got %s", got)
	}
	if got := h.jobStatus("j1"); got != store.JobStatusRunning {
		t.Fatalf("job should remain running across a redelivery, got %s", got)
	}

	// A genuine SECOND attempt still counts: the exactly-once gate is per
	// attempt, not per task.
	h.reassignAndReportFailed("t1", "w1")
	if got := h.taskFailedAttempts("t1"); got != 2 {
		t.Fatalf("a genuine second attempt must count: want 2, got %d", got)
	}
}

func TestHandleTaskFailed_ParksJobAtFailureLimit(t *testing.T) {
	// The real park scenario: a job with work still remaining. The step holds
	// two tasks — t1 fails and trips FailureLimit:1, while t2 stays ready. The
	// step is therefore NOT complete when t1 fails, so checkStepCompletion /
	// checkJobCompletion naturally no-op and the park holds without any guard.
	h := newFailureHarness(t, RetryPolicy{MaxAttempts: 5, RetryDelay: 0, FailureLimit: 1})
	h.seedRunningTask("j1", "t1", "w1", "t2") // second task, still ready (non-terminal)

	h.reportFailed("t1")

	if got := h.jobStatus("j1"); got != store.JobStatusPaused {
		t.Fatalf("want job paused at failure limit, got %s", got)
	}
	if got := h.taskStatus("t1"); got != store.TaskStatusFailed {
		t.Fatalf("tripping task should be terminal failed, got %s", got)
	}
	// The sibling still has work to do — parking must not have finalized it.
	if got := h.taskStatus("t2"); got != store.TaskStatusReady {
		t.Fatalf("sibling task should remain ready under a park, got %s", got)
	}
	if reason := h.parkReason("j1"); !strings.Contains(reason, "failure limit") {
		t.Fatalf("park_reason should record the failure limit, got %q", reason)
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

// TestLeaseGatesPass_SkipsPausedJob covers the defense-in-depth skip added to
// leaseGatesPass: even if a ready task for a paused job reaches the lease gate
// (the ready-list→lease race window), the gate must refuse it so a parked job's
// leftover work is never dispatched.
func TestLeaseGatesPass_SkipsPausedJob(t *testing.T) {
	h := newFailureHarness(t, RetryPolicy{MaxAttempts: 2, RetryDelay: 0})
	ctx := t.Context()
	now := time.Now().UTC()

	worker, _, err := h.st.RegisterWorker(ctx, store.Worker{
		ID: "w1", FarmID: "farm-1", Hostname: "w1",
		Status: store.WorkerStatusOnline, CPUCount: 4,
	})
	if err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	// The job is paused by the operator's write, after submission, with its
	// task still ready.
	storetest.Submit(t, h.st, store.JobSubmission{
		Job: store.Job{
			ID: "jp", FarmID: "farm-1", QueueID: "queue-1", Name: "jp",
			Status: store.JobStatusRunning, TemplateFormat: store.TemplateFormatJSON,
		},
		Steps: []store.Step{{ID: "sp", JobID: "jp", Name: "Step1", Status: store.StepStatusRunning}},
		Tasks: []store.Task{{ID: "tp", JobID: "jp", StepID: "sp", Name: "tp", Status: store.TaskStatusReady}},
	})
	if err := h.st.PauseJob(ctx, "jp", now); err != nil {
		t.Fatalf("PauseJob: %v", err)
	}
	task, err := h.st.GetTask(ctx, "tp")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}

	// "" = no EXPR cap shortfall for this worker; the paused-job gate under
	// test is unrelated to it.
	_, pass, err := h.s.leaseGatesPass(ctx, task, worker, "")
	if err != nil {
		t.Fatalf("leaseGatesPass: unexpected error: %v", err)
	}
	if pass {
		t.Fatal("leaseGatesPass should refuse a task whose job is paused")
	}
}

// TestHandleTaskFailed_PersistsReason asserts that a worker-reported failure
// message that goes terminal (max_attempts=1, so the first failure is
// exhausted, not retried) persists on both the closed attempt's Message and
// the task's durable FailureReason.
func TestHandleTaskFailed_PersistsReason(t *testing.T) {
	// max_attempts=1 so the first failure is terminal.
	h := newFailureHarness(t, RetryPolicy{MaxAttempts: 1, RetryDelay: 0})
	h.seedRunningTask("j1", "t1", "w1")
	h.reportFailedWithMessage("t1", "worker not configured for staging")
	if got := h.taskFailureReason("t1"); got != "worker not configured for staging" {
		t.Fatalf("task.failure_reason = %q", got)
	}
	if got := h.latestAttemptMessage("t1"); got != "worker not configured for staging" {
		t.Fatalf("attempt.message = %q", got)
	}
}

// TestHandleTaskFailed_RetriedAttemptRecordsMessage_TaskReasonStaysClear
// asserts that a retried (non-terminal) failure still records the closed
// attempt's Message via RecordTaskFailure, but does NOT set the task's
// FailureReason — only handleTaskTerminal does that, and the retry path
// never calls it.
func TestHandleTaskFailed_RetriedAttemptRecordsMessage_TaskReasonStaysClear(t *testing.T) {
	// max_attempts=2: first failure retries (task -> ready, reason clear), but
	// the closed attempt still records its message.
	h := newFailureHarness(t, RetryPolicy{MaxAttempts: 2, RetryDelay: 0})
	h.seedRunningTask("j1", "t1", "w1")
	h.reportFailedWithMessage("t1", "process exited with code 1")
	if got := h.taskStatus("t1"); got != store.TaskStatusReady {
		t.Fatalf("want ready, got %s", got)
	}
	if got := h.taskFailureReason("t1"); got != "" {
		t.Fatalf("retried task must have empty failure_reason, got %q", got)
	}
	if got := h.latestAttemptMessage("t1"); got != "process exited with code 1" {
		t.Fatalf("attempt.message = %q", got)
	}
}

// ── stale / raced failure reports ────────────────────────────────────────────

// TestHandleTaskFailed_CancelRace_DoesNotResurrectTask covers the user-cancel
// vs in-flight-failure race, which needs NO redelivery: the worker publishes
// "failed", but before the server processes it the user cancels the task —
// the attempt is closed as canceled and the task goes terminal-canceled with
// its durable reason. The late failure report must be discarded: it must not
// flip the canceled task back to ready, wipe its "canceled by user" reason,
// or count a failure.
func TestHandleTaskFailed_CancelRace_DoesNotResurrectTask(t *testing.T) {
	h := newFailureHarness(t, RetryPolicy{MaxAttempts: 3, RetryDelay: 0})
	h.seedRunningTask("j1", "t1", "w1")
	ctx := t.Context()
	now := time.Now().UTC()

	// The cancel lands first: one store operation closes the attempt as
	// canceled, cancels the task and stamps the durable reason.
	if _, canceled, err := h.st.CancelTaskExecution(ctx, "t1", store.FailureReasonCanceledByUser, now); err != nil || !canceled {
		t.Fatalf("CancelTaskExecution = (%v, %v), want canceled", canceled, err)
	}

	// The worker's in-flight "failed" report is processed afterwards.
	h.reportFailed("t1")

	if got := h.taskStatus("t1"); got != store.TaskStatusCanceled {
		t.Fatalf("canceled task resurrected: status = %s, want canceled", got)
	}
	if got := h.taskFailureReason("t1"); got != store.FailureReasonCanceledByUser {
		t.Fatalf("failure_reason = %q, want %q preserved", got, store.FailureReasonCanceledByUser)
	}
	if got := h.taskFailedAttempts("t1"); got != 0 {
		t.Fatalf("canceled attempt must not count as a failure: failed_attempts = %d", got)
	}
}

// TestHandleTaskFailed_SupersededAttempt_LeavesReleasedTaskAlone covers the
// late-failure-after-reclaim race: worker A drops off, the sweep closes its
// attempt as failed and reclaims the task, and worker B re-leases it (a newer
// attempt is running). When worker A reconnects and its buffered "failed" for
// the OLD attempt is delivered, it must be discarded — acting on it would rip
// the assignment out from under worker B and let a third worker lease the
// task into duplicate concurrent execution.
func TestHandleTaskFailed_SupersededAttempt_LeavesReleasedTaskAlone(t *testing.T) {
	h := newFailureHarness(t, RetryPolicy{MaxAttempts: 5, RetryDelay: 0})
	h.seedRunningTask("j1", "t1", "wA")
	ctx := t.Context()
	now := time.Now().UTC()

	// The sweep closed worker A's attempt as failed (worker went offline)…
	// ReclaimTaskAttempt is the offline reclaim for this one task: it closes
	// the attempt as failed and returns the task to ready with no worker.
	stale := h.current["t1"]
	if reclaimed, err := h.st.ReclaimTaskAttempt(ctx, stale.ID, "t1", now); err != nil || !reclaimed {
		t.Fatalf("ReclaimTaskAttempt = (%v, %v), want reclaimed", reclaimed, err)
	}

	// …and the task was reclaimed, then re-leased to worker B (newer attempt).
	h.run("t1", "wB")

	// Worker A reconnects: its buffered "failed" for the stale attempt lands.
	exitCode := 1
	msg := protocol.TaskStatusMsg{
		Version:   protocol.ProtocolVersion,
		TaskID:    "t1",
		AttemptID: stale.ID,
		Status:    "failed",
		ExitCode:  &exitCode,
		At:        time.Now().UTC(),
	}
	if err := h.s.processTaskStatus(ctx, stale.WorkerID, msg); err != nil {
		t.Fatalf("processTaskStatus(stale failed): %v", err)
	}

	task, err := h.st.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != store.TaskStatusRunning {
		t.Fatalf("stale report disturbed the re-leased task: status = %s, want running", task.Status)
	}
	if task.AssignedWorkerID != "wB" {
		t.Fatalf("assignment yanked from worker B: assigned to %q", task.AssignedWorkerID)
	}
	if task.RetryAfter != nil {
		t.Fatal("stale report must not stamp retry_after on the re-leased task")
	}
}

// TestHandleTaskFailed_StaleReportDoesNotPark pins the stale-report gate on its
// own. The other stale-report tests land on the retry branch, where
// RequeueTaskForRetry's guard also refuses a superseded attempt, so they stay
// green with the gate removed. The park branch has no such guard: ParkJob pauses
// the job whichever attempt asked. Here the server restarts with a lower failure
// limit and JetStream redelivers the failure report of an attempt a retry has
// already superseded; without the gate, that redelivery parks a job whose task
// is running fine on another worker.
func TestHandleTaskFailed_StaleReportDoesNotPark(t *testing.T) {
	h := newFailureHarness(t, RetryPolicy{MaxAttempts: 5, RetryDelay: 0})
	h.seedRunningTask("j1", "t1", "wA")

	// A genuine failure: counted once, and the task retried.
	stale := h.current["t1"]
	h.reportFailed("t1")
	if got := h.taskStatus("t1"); got != store.TaskStatusReady {
		t.Fatalf("fixture: failed task should be requeued, got %s", got)
	}
	fresh := h.run("t1", "wB")

	// The restart lowers the limit to the job's failure count, then the old
	// attempt's report is delivered again.
	h.s.cfg.DefaultFailureLimit = 1
	exitCode := 1
	err := h.s.processTaskStatus(t.Context(), stale.WorkerID, protocol.TaskStatusMsg{
		Version: protocol.ProtocolVersion, TaskID: "t1", AttemptID: stale.ID,
		Status: "failed", ExitCode: &exitCode, At: time.Now().UTC(),
	})

	if got := h.jobStatus("j1"); got != store.JobStatusRunning {
		t.Fatalf("stale report changed the job: status = %s, want running", got)
	}
	if err != nil {
		t.Fatalf("processTaskStatus(stale failed): %v", err)
	}
	task, err := h.st.GetTask(t.Context(), "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != store.TaskStatusRunning || task.AssignedWorkerID != "wB" {
		t.Fatalf("task = %s on %q, want running on wB", task.Status, task.AssignedWorkerID)
	}
	if a := mustAttemptOf(t, h.st, fresh.ID); a.Status != store.AttemptStatusRunning {
		t.Fatalf("re-leased attempt = %s, want running", a.Status)
	}
}

// TestHandleTaskFailed_CrashRecoveryRedelivery_StillRequeues pins the
// crash-recovery contract the stale-report gate must NOT break: the server
// committed RecordTaskFailure (attempt closed, counters incremented) but died
// before the requeue, so the un-acked message is redelivered after restart.
// The attempt is closed as failed and is still the task's latest, so the
// redelivery must complete the lost retry action.
func TestHandleTaskFailed_CrashRecoveryRedelivery_StillRequeues(t *testing.T) {
	h := newFailureHarness(t, RetryPolicy{MaxAttempts: 3, RetryDelay: 0})
	h.seedRunningTask("j1", "t1", "w1")
	ctx := t.Context()

	// First half of handleTaskFailed only: the counters committed, then the
	// server crashed before RequeueTaskForRetry ran.
	att := h.current["t1"]
	if _, _, first, err := h.st.RecordTaskFailure(ctx, att.ID, "t1", nil, "", "", time.Now().UTC()); err != nil || !first {
		t.Fatalf("RecordTaskFailure: first=%v err=%v", first, err)
	}
	if got := h.taskStatus("t1"); got != store.TaskStatusRunning {
		t.Fatalf("fixture: task should still be running pre-redelivery, got %s", got)
	}

	// Redelivery after restart drives the full path.
	h.reportFailed("t1")

	if got := h.taskStatus("t1"); got != store.TaskStatusReady {
		t.Fatalf("crash-recovery redelivery must requeue: status = %s, want ready", got)
	}
	if got := h.taskFailedAttempts("t1"); got != 1 {
		t.Fatalf("redelivery must not re-count: failed_attempts = %d, want 1", got)
	}
}

// ── park recovery ────────────────────────────────────────────────────────────

// TestParkResume_RearmsFailureLimit covers the auto-park recovery loop: a job
// parks at its failure limit, the operator resumes it, and the NEXT genuine
// failure must retry normally instead of instantly re-parking. Resume must
// therefore clear park_reason and reset the job's failure counter.
func TestParkResume_RearmsFailureLimit(t *testing.T) {
	h := newFailureHarness(t, RetryPolicy{MaxAttempts: 10, RetryDelay: 0, FailureLimit: 2})
	h.seedRunningTask("j1", "t1", "w1", "t2") // t2 keeps the step/job non-terminal through the park
	ctx := t.Context()
	now := time.Now().UTC()

	// Two genuine failures trip the limit and park the job.
	h.reportFailed("t1")
	h.reassignAndReportFailed("t1", "w1")
	if got := h.jobStatus("j1"); got != store.JobStatusPaused {
		t.Fatalf("fixture: job should be parked, got %s", got)
	}
	if h.parkReason("j1") == "" {
		t.Fatal("fixture: parked job should carry a park_reason")
	}

	// Operator resumes: park state cleared, failure limit re-armed.
	if err := h.st.ResumeJob(ctx, "j1", now); err != nil {
		t.Fatalf("ResumeJob: %v", err)
	}
	if got := h.jobStatus("j1"); got != store.JobStatusPending {
		t.Fatalf("after resume want pending, got %s", got)
	}
	if got := h.parkReason("j1"); got != "" {
		t.Fatalf("resume must clear park_reason, got %q", got)
	}
	if got := h.jobFailedAttempts("j1"); got != 0 {
		t.Fatalf("resume must reset the job failure counter, got %d", got)
	}

	// The next genuine failure retries normally — no instant re-park.
	h.run("t2", "w1")
	h.reportFailed("t2")

	if got := h.jobStatus("j1"); got == store.JobStatusPaused {
		t.Fatal("job re-parked immediately after resume — failure limit was not re-armed")
	}
	if got := h.taskStatus("t2"); got != store.TaskStatusReady {
		t.Fatalf("post-resume failure should retry: task status = %s, want ready", got)
	}
}

// ── backoff-wake timer lifecycle ─────────────────────────────────────────────

// TestScheduleRetryWake_TimerLifecycle asserts the backoff-wake timers are
// tracked while pending, remove themselves once fired, are all stopped by
// shutdown, and are not scheduled at all once the scheduler context is done —
// so no fire-and-forget timers outlive Run.
func TestScheduleRetryWake_TimerLifecycle(t *testing.T) {
	h := newFailureHarness(t, RetryPolicy{MaxAttempts: 3, RetryDelay: time.Hour})

	pending := func() int {
		h.s.retryWakeMu.Lock()
		defer h.s.retryWakeMu.Unlock()
		return len(h.s.retryWakeTimers)
	}

	// Pending timers are tracked…
	h.s.scheduleRetryWake("queue-1", time.Hour)
	h.s.scheduleRetryWake("queue-1", time.Hour)
	if got := pending(); got != 2 {
		t.Fatalf("pending timers = %d, want 2", got)
	}

	// …and shutdown stops them all.
	h.s.stopRetryWakeTimers()
	if got := pending(); got != 0 {
		t.Fatalf("pending timers after stop = %d, want 0", got)
	}

	// A fired timer removes itself from the tracking set.
	h.s.scheduleRetryWake("queue-1", time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for pending() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("fired timer never removed itself from the tracking set")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// After the scheduler context is canceled, no new timers are scheduled.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.s.ctx = ctx
	h.s.scheduleRetryWake("queue-1", time.Hour)
	if got := pending(); got != 0 {
		t.Fatalf("timer scheduled after shutdown: pending = %d, want 0", got)
	}
}
