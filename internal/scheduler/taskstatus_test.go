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
	return New(
		DefaultConfig(),
		st,
		nil, // bus — not called by processTaskStatus
		nil, // metrics — not used
		slog.New(slog.DiscardHandler),
		ws.NoopNotifier{},
	)
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

// seedStatusFixture builds a complete job/step/task/attempt in the store.
// Returns all four records.
func seedStatusFixture(t *testing.T, st *fake.Store, taskStatus store.TaskStatus) (
	job store.Job, step store.Step, task store.Task, attempt store.TaskAttempt,
) {
	t.Helper()
	ctx := t.Context()
	now := time.Now()

	job, err := st.CreateJob(ctx, store.Job{
		ID:             uuid.NewString(),
		FarmID:         "farm-1",
		QueueID:        "queue-1",
		Name:           "job",
		Status:         store.JobStatusRunning,
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
