// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Tests for taskstatus.go — item 8d of the test roadmap.
//
// handleTaskStatusMessage and processTaskStatus are unexported methods on
// *Scheduler, so these are white-box tests in package scheduler.
// The same fakeJSMsg type from logingest_test.go is reused here.

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/ws"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func newStatusTestScheduler(st store.Store) *Scheduler {
	return newStatusTestSchedulerWithNotifier(st, ws.NoopNotifier{})
}

func newStatusTestSchedulerWithNotifier(st store.Store, notifier ws.Notifier) *Scheduler {
	// MaxAttempts=1 (retry disabled) so these generic terminal-cascade tests
	// keep asserting the pre-auto-retry behavior: a single genuine failure
	// goes straight to store.TaskStatusFailed. Retry/park-specific behavior is
	// covered separately in failure_test.go.
	cfg := DefaultConfig()
	cfg.DefaultMaxAttempts = 1
	return New(
		cfg,
		st,
		nil, // bus — not called by processTaskStatus
		nil, // metrics — not used (retry/park branches are disabled by MaxAttempts=1 above)
		slog.New(slog.DiscardHandler),
		notifier,
		nil, // diagBuf — diagnostics disabled
	)
}

// recordingNotifier captures NotifyTask events for assertions; other Notify*
// methods are inherited as no-ops from ws.NoopNotifier.
type recordingNotifier struct {
	ws.NoopNotifier

	tasks []ws.TaskEvent
}

func (n *recordingNotifier) NotifyTask(e ws.TaskEvent) {
	n.tasks = append(n.tasks, e)
}

// taskStatusMsgJSON marshals a TaskStatusMsg to JSON bytes for use as fakeJSMsg.data.
func taskStatusMsgJSON(t *testing.T, m protocol.TaskStatusMsg) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal TaskStatusMsg: %v", err)
	}
	return b
}

// seedStatusFixture builds a complete job/step/task/attempt in the store with
// the job already in the running state. Returns all four records.
func seedStatusFixture(t *testing.T, st *fake.Store, taskStatus store.TaskStatus) (
	job store.Job, step store.Step, task store.Task, attempt store.TaskAttempt,
) {
	t.Helper()
	return seedStatusFixtureWithJobStatus(t, st, store.JobStatusRunning, taskStatus)
}

// seedStatusFixtureWithJobStatus is like seedStatusFixture but lets the caller
// choose the initial job status (e.g. pending, to exercise promotion to running).
func seedStatusFixtureWithJobStatus(
	t *testing.T, st *fake.Store, jobStatus store.JobStatus, taskStatus store.TaskStatus,
) (
	job store.Job, step store.Step, task store.Task, attempt store.TaskAttempt,
) {
	t.Helper()
	ctx := t.Context()
	now := time.Now()

	// handleTaskFailed resolves retry policy via GetQueue/GetFarm, so a real
	// job needs real farm/queue rows behind its FarmID/QueueID below.
	if _, err := st.CreateFarm(ctx, store.Farm{ID: "farm-1", Name: "farm-1"}); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "queue-1", FarmID: "farm-1", Name: "queue-1"}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}

	job, err := st.CreateJob(ctx, store.Job{
		ID:             uuid.NewString(),
		FarmID:         "farm-1",
		QueueID:        "queue-1",
		Name:           "job",
		Status:         jobStatus,
		TemplateFormat: store.TemplateFormatJSON,
		CreatedAt:      now,
		UpdatedAt:      now,
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	step, err = st.CreateStep(ctx, store.Step{
		ID: uuid.NewString(), JobID: job.ID, Name: "Step1",
		Status: store.StepStatusRunning, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}

	task, err = st.CreateTask(ctx, store.Task{
		ID: uuid.NewString(), JobID: job.ID, StepID: step.ID,
		Name: "task-0", Status: taskStatus, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	attempt, err = st.CreateTaskAttempt(ctx, store.TaskAttempt{
		ID:            uuid.NewString(),
		TaskID:        task.ID,
		AttemptNumber: 1,
		Status:        store.AttemptStatusRunning,
		StartedAt:     now,
		CreatedAt:     now,
	})
	if err != nil {
		t.Fatalf("CreateTaskAttempt: %v", err)
	}

	return job, step, task, attempt
}

// ── handleTaskStatusMessage — routing and discard ─────────────────────────────

func TestHandleTaskStatusMessage_MalformedJSON(t *testing.T) {
	st := fake.New()
	s := newStatusTestScheduler(st)
	s.ctx = t.Context()

	msg := &fakeJSMsg{data: []byte("{bad json")}
	s.handleTaskStatusMessage(msg)

	if !msg.acked {
		t.Error("malformed message should be acked (discarded)")
	}
}

func TestHandleTaskStatusMessage_MissingTaskID(t *testing.T) {
	st := fake.New()
	s := newStatusTestScheduler(st)
	s.ctx = t.Context()

	msg := &fakeJSMsg{
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			TaskID:    "",
			AttemptID: uuid.NewString(),
			Status:    "running",
		}),
	}
	s.handleTaskStatusMessage(msg)

	if !msg.acked {
		t.Error("message with missing task_id should be acked (discarded)")
	}
}

func TestHandleTaskStatusMessage_UnknownAttemptID(t *testing.T) {
	st := fake.New()
	s := newStatusTestScheduler(st)
	s.ctx = t.Context()

	msg := &fakeJSMsg{
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			TaskID:    uuid.NewString(),
			AttemptID: "no-such-attempt",
			Status:    "running",
		}),
	}
	// Unknown attempt → acked (discarded gracefully), no panic.
	s.handleTaskStatusMessage(msg)
	if !msg.acked {
		t.Error("message with unknown attempt_id should be acked")
	}
}

// ── processTaskStatus — "running" path ───────────────────────────────────────

func TestProcessTaskStatus_Running(t *testing.T) {
	st := fake.New()
	s := newStatusTestScheduler(st)
	s.ctx = t.Context()

	_, _, task, attempt := seedStatusFixture(t, st, store.TaskStatusAssigned)

	sessionID := "openjd-session-abc"
	msg := &fakeJSMsg{
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			TaskID:    task.ID,
			AttemptID: attempt.ID,
			Status:    "running",
			SessionID: sessionID,
		}),
	}
	s.handleTaskStatusMessage(msg)

	if !msg.acked {
		t.Fatal("expected message to be acked")
	}
	stored, err := st.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != store.TaskStatusRunning {
		t.Errorf("task status = %q, want running", stored.Status)
	}
	// Verify session ID was persisted on the attempt.
	updatedAttempt, err := st.GetTaskAttempt(t.Context(), attempt.ID)
	if err != nil {
		t.Fatalf("GetTaskAttempt: %v", err)
	}
	if updatedAttempt.SessionID != sessionID {
		t.Errorf("attempt.SessionID = %q, want %q", updatedAttempt.SessionID, sessionID)
	}
}

// TestProcessTaskStatus_Running_PromotesPendingJob verifies that when the first
// task of a still-pending job reports "running", the enclosing job is promoted
// to running and its StartedAt is stamped.
func TestProcessTaskStatus_Running_PromotesPendingJob(t *testing.T) {
	st := fake.New()
	s := newStatusTestScheduler(st)
	s.ctx = t.Context()

	job, _, task, attempt := seedStatusFixtureWithJobStatus(
		t, st, store.JobStatusPending, store.TaskStatusAssigned,
	)

	msg := &fakeJSMsg{
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			TaskID:    task.ID,
			AttemptID: attempt.ID,
			Status:    "running",
		}),
	}
	s.handleTaskStatusMessage(msg)

	if !msg.acked {
		t.Fatal("expected message to be acked")
	}
	storedJob, err := st.GetJob(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if storedJob.Status != store.JobStatusRunning {
		t.Errorf("job status = %q, want running", storedJob.Status)
	}
	if storedJob.StartedAt == nil {
		t.Error("job StartedAt should be set once its first task is running")
	}
}

// TestProcessTaskStatus_Running_DoesNotUnpauseJob verifies that a "running"
// status for a job that is not pending (e.g. paused) does not flip it back to
// running.
func TestProcessTaskStatus_Running_DoesNotUnpauseJob(t *testing.T) {
	st := fake.New()
	s := newStatusTestScheduler(st)
	s.ctx = t.Context()

	job, _, task, attempt := seedStatusFixtureWithJobStatus(
		t, st, store.JobStatusPaused, store.TaskStatusAssigned,
	)

	msg := &fakeJSMsg{
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			TaskID:    task.ID,
			AttemptID: attempt.ID,
			Status:    "running",
		}),
	}
	s.handleTaskStatusMessage(msg)

	storedJob, err := st.GetJob(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if storedJob.Status != store.JobStatusPaused {
		t.Errorf("job status = %q, want paused (unchanged)", storedJob.Status)
	}
}

// ── processTaskStatus — terminal paths ───────────────────────────────────────

func TestProcessTaskStatus_Succeeded(t *testing.T) {
	st := fake.New()
	s := newStatusTestScheduler(st)
	s.ctx = t.Context()

	_, _, task, attempt := seedStatusFixture(t, st, store.TaskStatusRunning)
	exitCode := 0

	msg := &fakeJSMsg{
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			TaskID:    task.ID,
			AttemptID: attempt.ID,
			Status:    "succeeded",
			ExitCode:  &exitCode,
			At:        time.Now().UTC(),
		}),
	}
	s.handleTaskStatusMessage(msg)

	if !msg.acked {
		t.Fatal("expected message to be acked")
	}
	stored, err := st.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != store.TaskStatusSucceeded {
		t.Errorf("task status = %q, want succeeded", stored.Status)
	}
}

func TestProcessTaskStatus_Failed(t *testing.T) {
	st := fake.New()
	s := newStatusTestScheduler(st)
	s.ctx = t.Context()

	_, _, task, attempt := seedStatusFixture(t, st, store.TaskStatusRunning)
	exitCode := 1

	msg := &fakeJSMsg{
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			TaskID:    task.ID,
			AttemptID: attempt.ID,
			Status:    "failed",
			ExitCode:  &exitCode,
			At:        time.Now().UTC(),
		}),
	}
	s.handleTaskStatusMessage(msg)

	stored, err := st.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != store.TaskStatusFailed {
		t.Errorf("task status = %q, want failed", stored.Status)
	}
}

func TestProcessTaskStatus_Canceled(t *testing.T) {
	st := fake.New()
	s := newStatusTestScheduler(st)
	s.ctx = t.Context()

	_, _, task, attempt := seedStatusFixture(t, st, store.TaskStatusRunning)

	msg := &fakeJSMsg{
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			TaskID:    task.ID,
			AttemptID: attempt.ID,
			Status:    "canceled",
			At:        time.Now().UTC(),
		}),
	}
	s.handleTaskStatusMessage(msg)

	stored, err := st.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != store.TaskStatusCanceled {
		t.Errorf("task status = %q, want canceled", stored.Status)
	}
}

// TestProcessTaskStatus_Canceled_PersistsMessageAndReason asserts that a
// worker-canceled status (whose attempt is closed directly by
// handleTaskTerminal, not RecordTaskFailure) still persists the worker's
// Message onto both the closed attempt and the task's durable FailureReason.
func TestProcessTaskStatus_Canceled_PersistsMessageAndReason(t *testing.T) {
	st := fake.New()
	s := newStatusTestScheduler(st)
	s.ctx = t.Context()

	_, _, task, attempt := seedStatusFixture(t, st, store.TaskStatusRunning)

	msg := &fakeJSMsg{
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			TaskID:    task.ID,
			AttemptID: attempt.ID,
			Status:    "canceled",
			Message:   "canceled by operator",
			At:        time.Now().UTC(),
		}),
	}
	s.handleTaskStatusMessage(msg)

	stored, err := st.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != store.TaskStatusCanceled {
		t.Errorf("task status = %q, want canceled", stored.Status)
	}
	if stored.FailureReason != "canceled by operator" {
		t.Errorf("task.failure_reason = %q, want %q", stored.FailureReason, "canceled by operator")
	}

	storedAttempt, err := st.GetTaskAttempt(t.Context(), attempt.ID)
	if err != nil {
		t.Fatalf("GetTaskAttempt: %v", err)
	}
	if storedAttempt.Message != "canceled by operator" {
		t.Errorf("attempt.message = %q, want %q", storedAttempt.Message, "canceled by operator")
	}
}

// TestProcessTaskStatus_Canceled_EmptyWorkerEchoPreservesServerReason is the
// regression for the bug where a user-canceled RUNNING task lost its
// "canceled by user" reason. CancelTask/CancelJob set the task's
// failure_reason up front, then kill the worker; the worker always echoes
// back "canceled" with an empty Message (internal/worker/executor/run.go).
// Before the fix, handleTaskTerminal unconditionally called
// SetTaskFailureReason with the synthesized (empty, for canceled) reason,
// clobbering the server-set one. The fix guards that write so an empty
// synthesized reason never overwrites an existing reason.
func TestProcessTaskStatus_Canceled_EmptyWorkerEchoPreservesServerReason(t *testing.T) {
	st := fake.New()
	s := newStatusTestScheduler(st)
	s.ctx = t.Context()

	_, _, task, attempt := seedStatusFixture(t, st, store.TaskStatusRunning)

	// Simulate what CancelTask/CancelJob did before killing the worker: stamp
	// the authoritative reason directly via the store.
	if err := st.SetTaskFailureReason(t.Context(), task.ID, "canceled by user"); err != nil {
		t.Fatalf("SetTaskFailureReason: %v", err)
	}

	// The killed worker's terminal echo always carries an empty Message.
	msg := &fakeJSMsg{
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			TaskID:    task.ID,
			AttemptID: attempt.ID,
			Status:    "canceled",
			Message:   "",
			At:        time.Now().UTC(),
		}),
	}
	s.handleTaskStatusMessage(msg)

	if !msg.acked {
		t.Fatal("expected message to be acked")
	}

	stored, err := st.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != store.TaskStatusCanceled {
		t.Errorf("task status = %q, want canceled", stored.Status)
	}
	if stored.FailureReason != "canceled by user" {
		t.Errorf("task.failure_reason = %q, want %q (must not be clobbered by the empty worker echo)",
			stored.FailureReason, "canceled by user")
	}
}

// ── Step and job completion ───────────────────────────────────────────────────

func TestProcessTaskStatus_AllTasksSucceeded_StepAndJobComplete(t *testing.T) {
	// Single-step job: when the only task succeeds, the step and job should
	// both transition to their terminal states.
	st := fake.New()
	s := newStatusTestScheduler(st)
	s.ctx = t.Context()

	job, step, task, attempt := seedStatusFixture(t, st, store.TaskStatusRunning)

	exitCode := 0
	msg := &fakeJSMsg{
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			TaskID:    task.ID,
			AttemptID: attempt.ID,
			Status:    "succeeded",
			ExitCode:  &exitCode,
			At:        time.Now().UTC(),
		}),
	}
	s.handleTaskStatusMessage(msg)

	// Step should be completed.
	steps, err := st.ListSteps(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("ListSteps: %v", err)
	}
	for _, st2 := range steps {
		if st2.ID == step.ID && st2.Status != store.StepStatusCompleted {
			t.Errorf("step status = %q, want completed", st2.Status)
		}
	}

	// Job should be completed.
	storedJob, err := st.GetJob(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if storedJob.Status != store.JobStatusCompleted {
		t.Errorf("job status = %q, want completed", storedJob.Status)
	}
}

func TestProcessTaskStatus_TaskFailed_JobFails(t *testing.T) {
	// Single-step job: failed task → step failed → job failed.
	st := fake.New()
	s := newStatusTestScheduler(st)
	s.ctx = t.Context()

	job, _, task, attempt := seedStatusFixture(t, st, store.TaskStatusRunning)

	exitCode := 1
	msg := &fakeJSMsg{
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			TaskID:    task.ID,
			AttemptID: attempt.ID,
			Status:    "failed",
			ExitCode:  &exitCode,
			At:        time.Now().UTC(),
		}),
	}
	s.handleTaskStatusMessage(msg)

	storedJob, err := st.GetJob(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if storedJob.Status != store.JobStatusFailed {
		t.Errorf("job status = %q, want failed", storedJob.Status)
	}
}

func TestProcessTaskStatus_SucceededStep_UnblocksDependentStep(t *testing.T) {
	// Two-step job: Step1 → Step2 (depends on Step1).
	// When Step1's task succeeds, Step2's tasks should move from pending→ready.
	st := fake.New()
	ctx := t.Context()
	now := time.Now()

	job, err := st.CreateJob(ctx, store.Job{
		ID:             uuid.NewString(),
		FarmID:         "farm-1",
		QueueID:        "queue-1",
		Name:           "two-step-job",
		Status:         store.JobStatusRunning,
		TemplateFormat: store.TemplateFormatJSON,
		CreatedAt:      now,
		UpdatedAt:      now,
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	step1, err := st.CreateStep(ctx, store.Step{
		ID: uuid.NewString(), JobID: job.ID, Name: "Step1",
		Status: store.StepStatusRunning, StepOrder: 0,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateStep1: %v", err)
	}

	step2, err := st.CreateStep(ctx, store.Step{
		ID: uuid.NewString(), JobID: job.ID, Name: "Step2",
		Status: store.StepStatusPending, StepOrder: 1,
		DependsOn: []string{"Step1"},
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateStep2: %v", err)
	}

	task1, err := st.CreateTask(ctx, store.Task{
		ID: uuid.NewString(), JobID: job.ID, StepID: step1.ID,
		Name: "t1", Status: store.TaskStatusRunning,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateTask1: %v", err)
	}

	// Step2 task starts pending.
	task2, err := st.CreateTask(ctx, store.Task{
		ID: uuid.NewString(), JobID: job.ID, StepID: step2.ID,
		Name: "t2", Status: store.TaskStatusPending,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateTask2: %v", err)
	}

	attempt1, err := st.CreateTaskAttempt(ctx, store.TaskAttempt{
		ID:            uuid.NewString(),
		TaskID:        task1.ID,
		AttemptNumber: 1,
		Status:        store.AttemptStatusRunning,
		StartedAt:     now,
		CreatedAt:     now,
	})
	if err != nil {
		t.Fatalf("CreateTaskAttempt: %v", err)
	}

	s := newStatusTestScheduler(st)
	s.ctx = ctx

	exitCode := 0
	msg := &fakeJSMsg{
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			TaskID:    task1.ID,
			AttemptID: attempt1.ID,
			Status:    "succeeded",
			ExitCode:  &exitCode,
			At:        now,
		}),
	}
	s.handleTaskStatusMessage(msg)

	// task2 should now be ready (dependency on Step1 resolved).
	stored2, err := st.GetTask(ctx, task2.ID)
	if err != nil {
		t.Fatalf("GetTask(task2): %v", err)
	}
	if stored2.Status != store.TaskStatusReady {
		t.Errorf("task2 status = %q after Step1 completion, want ready", stored2.Status)
	}
}

// seedDependentStep adds a pending Step2 (depending on the seedStatusFixture's
// "Step1") plus a pending task to an existing job, returning both records.
func seedDependentStep(t *testing.T, st *fake.Store, jobID string) (store.Step, store.Task) {
	t.Helper()
	ctx := t.Context()
	now := time.Now()

	step2, err := st.CreateStep(ctx, store.Step{
		ID: uuid.NewString(), JobID: jobID, Name: "Step2",
		Status: store.StepStatusPending, StepOrder: 1,
		DependsOn: []string{"Step1"},
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateStep2: %v", err)
	}
	task2, err := st.CreateTask(ctx, store.Task{
		ID: uuid.NewString(), JobID: jobID, StepID: step2.ID,
		Name: "t2", Status: store.TaskStatusPending,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateTask2: %v", err)
	}
	return step2, task2
}

func TestProcessTaskStatus_FailedStep_CascadeCancelsDependentAndCompletesJob(t *testing.T) {
	// Two-step job: Step1 → Step2 (depends on Step1).
	// When Step1's task fails, Step2 can never run, so it must be canceled
	// (along with its pending tasks) and the job must reach a terminal state
	// rather than hanging in running forever.
	st := fake.New()
	ctx := t.Context()

	job, _, task1, attempt1 := seedStatusFixture(t, st, store.TaskStatusRunning)
	step2, task2 := seedDependentStep(t, st, job.ID)

	s := newStatusTestScheduler(st)
	s.ctx = ctx

	exitCode := 1
	msg := &fakeJSMsg{
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			TaskID:    task1.ID,
			AttemptID: attempt1.ID,
			Status:    "failed",
			ExitCode:  &exitCode,
			At:        time.Now().UTC(),
		}),
	}
	s.handleTaskStatusMessage(msg)

	if !msg.acked {
		t.Error("successful cascade should ack the message")
	}

	// Step2 must be canceled (its only dependency failed).
	stored2Step, err := st.GetStep(ctx, step2.ID)
	if err != nil {
		t.Fatalf("GetStep(step2): %v", err)
	}
	if stored2Step.Status != store.StepStatusCanceled {
		t.Errorf("step2 status = %q, want canceled", stored2Step.Status)
	}

	// task2 (pending) must be canceled too.
	stored2, err := st.GetTask(ctx, task2.ID)
	if err != nil {
		t.Fatalf("GetTask(task2): %v", err)
	}
	if stored2.Status != store.TaskStatusCanceled {
		t.Errorf("task2 status = %q, want canceled", stored2.Status)
	}
	if stored2.FailureReason != "canceled: upstream step failed" {
		t.Errorf("task2 failure_reason = %q, want %q", stored2.FailureReason, "canceled: upstream step failed")
	}

	// The job must reach a terminal state, not hang in running.
	storedJob, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if storedJob.Status != store.JobStatusFailed {
		t.Errorf("job status = %q, want failed", storedJob.Status)
	}
}

func TestProcessTaskStatus_CascadeCancel_NotifiesCanceledTasks(t *testing.T) {
	// A cascade-canceled dependent task must be fanned out to WebSocket clients,
	// otherwise the live UI shows it frozen as pending.
	st := fake.New()
	ctx := t.Context()

	job, _, task1, attempt1 := seedStatusFixture(t, st, store.TaskStatusRunning)
	_, task2 := seedDependentStep(t, st, job.ID)

	notifier := &recordingNotifier{}
	s := newStatusTestSchedulerWithNotifier(st, notifier)
	s.ctx = ctx

	exitCode := 1
	msg := &fakeJSMsg{
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			TaskID:    task1.ID,
			AttemptID: attempt1.ID,
			Status:    "failed",
			ExitCode:  &exitCode,
			At:        time.Now().UTC(),
		}),
	}
	s.handleTaskStatusMessage(msg)

	// Expect a canceled-task event for task2 (task1's own failed event also fires).
	var got *ws.TaskEvent
	for i := range notifier.tasks {
		if notifier.tasks[i].TaskID == task2.ID {
			got = &notifier.tasks[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("no TaskEvent emitted for cascade-canceled task2; events = %+v", notifier.tasks)
	}
	if got.Status != string(store.TaskStatusCanceled) {
		t.Errorf("task2 event status = %q, want canceled", got.Status)
	}
}

func TestProcessTaskStatus_CascadeCancel_StoreError_Nacked(t *testing.T) {
	// If the cascade hits a transient store error, the message must be nacked so
	// JetStream redelivers — otherwise dependents strand and the job hangs, the
	// exact failure this cascade exists to prevent.
	inner := fake.New()
	ctx := t.Context()

	job, _, task1, attempt1 := seedStatusFixture(t, inner, store.TaskStatusRunning)
	seedDependentStep(t, inner, job.ID)

	est := &cancelTasksErrSt{Store: inner}
	s := newStatusTestScheduler(est)
	s.ctx = ctx

	exitCode := 1
	msg := &fakeJSMsg{
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			TaskID:    task1.ID,
			AttemptID: attempt1.ID,
			Status:    "failed",
			ExitCode:  &exitCode,
			At:        time.Now().UTC(),
		}),
	}
	s.handleTaskStatusMessage(msg)

	if !msg.nacked {
		t.Error("store error during cascade should cause the message to be nacked")
	}
}

// cancelTasksErrSt makes the dependent-task cancellation fail mid-cascade.
type cancelTasksErrSt struct {
	store.Store
}

func (*cancelTasksErrSt) TransitionStepPendingTasks(_ context.Context, _ string, _ store.TaskStatus, _ string) ([]store.Task, error) {
	return nil, errInjectedLog
}

// ── Store error on UpdateTaskAttempt → message nacked ────────────────────────

func TestProcessTaskStatus_UpdateAttemptError_Nacked(t *testing.T) {
	inner := fake.New()
	_, _, task, attempt := seedStatusFixture(t, inner, store.TaskStatusRunning)
	est := &updateAttemptErrSt{Store: inner}

	s := newStatusTestScheduler(est)
	s.ctx = t.Context()

	exitCode := 0
	msg := &fakeJSMsg{
		data: taskStatusMsgJSON(t, protocol.TaskStatusMsg{
			TaskID:    task.ID,
			AttemptID: attempt.ID,
			Status:    "succeeded",
			ExitCode:  &exitCode,
			At:        time.Now().UTC(),
		}),
	}
	s.handleTaskStatusMessage(msg)

	if !msg.nacked {
		t.Error("store error should cause message to be nacked")
	}
}

// updateAttemptErrSt makes UpdateTaskAttempt fail.
type updateAttemptErrSt struct {
	store.Store
}

func (*updateAttemptErrSt) UpdateTaskAttempt(_ context.Context, _ store.TaskAttempt) (store.TaskAttempt, error) {
	return store.TaskAttempt{}, errInjectedLog
}
