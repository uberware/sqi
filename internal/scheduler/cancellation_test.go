// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Tests for cancellation.go.
//
// CancelJob and CancelTask are methods on *Scheduler, so these are white-box
// tests in package scheduler. A stubBus satisfies the busClient interface so
// NATS publish calls can be recorded or failed without a real broker.

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/store/storetest"
	"github.com/uberware/sqi/internal/ws"
)

// ── stubBus: minimal busClient for cancellation tests ────────────────────────

// stubBus records PublishTaskCancel calls and can be configured to return an
// error.  All Consume*/Subscribe* methods are no-ops.
type stubBus struct {
	cancelErr   error
	cancelCalls []string // taskIDs published
}

func (*stubBus) ConsumeWorker(_ context.Context, _ jetstream.MessageHandler) (jetstream.ConsumeContext, error) {
	return nil, nil
}

func (*stubBus) ConsumeTaskStatus(_ context.Context, _ jetstream.MessageHandler) (jetstream.ConsumeContext, error) {
	return nil, nil
}

func (*stubBus) ConsumeTaskLogs(_ context.Context, _ jetstream.MessageHandler) (jetstream.ConsumeContext, error) {
	return nil, nil
}

func (*stubBus) SubscribeWorkerDiag(_ func(subject string, data []byte)) (*nats.Subscription, error) {
	return nil, nil
}

func (*stubBus) SubscribeLease(_ func(string, string, []byte) []byte) (*nats.Subscription, error) {
	return nil, nil
}

func (b *stubBus) PublishTaskCancel(_ context.Context, taskID string, _ []byte) error {
	b.cancelCalls = append(b.cancelCalls, taskID)
	return b.cancelErr
}

// newTestScheduler returns a Scheduler wired with the given store and bus stub.
// It is safe for calling CancelJob/CancelTask without ever calling Run.
func newTestScheduler(st store.Store, bus busClient) *Scheduler {
	return New(
		DefaultConfig(),
		st,
		bus,
		nil, // metrics — not used by CancelJob/CancelTask
		slog.New(slog.DiscardHandler),
		ws.NoopNotifier{},
		nil, // diagBuf — diagnostics disabled
	)
}

// ── seed helpers ──────────────────────────────────────────────────────────────

// cancelTask is one task of a job [seedCancelJob] builds.
type cancelTask struct {
	status store.TaskStatus
	// worker is the worker an assigned or running task is leased to.
	worker string
	// reason is the FailureReason the task is submitted with.
	reason string
}

// seedCancelJob builds a running job in a farm and queue of its own, with one
// step per task, through [storetest.SubmitLeasing], so each assigned or
// running task is leased to its worker and holds the attempt a real lease
// writes.
// It returns the job, and the tasks and their attempts in tasks order; the
// attempt is the zero value for a task that was not leased.
func seedCancelJob(t *testing.T, st *fake.Store, tasks ...cancelTask) (store.Job, []store.Task, []store.TaskAttempt) {
	t.Helper()
	ctx := t.Context()
	farm, err := st.CreateFarm(ctx, store.Farm{ID: uuid.NewString(), Name: "f"})
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queue, err := st.CreateQueue(ctx, store.Queue{ID: uuid.NewString(), FarmID: farm.ID, Name: "q"})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	sub := store.JobSubmission{Job: store.Job{
		ID:             uuid.NewString(),
		FarmID:         farm.ID,
		QueueID:        queue.ID,
		Name:           "test-job",
		Status:         store.JobStatusRunning,
		TemplateFormat: store.TemplateFormatJSON,
	}}
	workers := make(map[string]string, len(tasks)) // task ID -> worker
	for _, ct := range tasks {
		step := store.Step{ID: uuid.NewString(), JobID: sub.Job.ID, Name: uuid.NewString(), Status: store.StepStatusRunning}
		task := store.Task{ID: uuid.NewString(), JobID: sub.Job.ID, StepID: step.ID, Name: "t", Status: ct.status, FailureReason: ct.reason}
		workers[task.ID] = ct.worker
		sub.Steps = append(sub.Steps, step)
		sub.Tasks = append(sub.Tasks, task)
	}
	out, leased := storetest.SubmitLeasing(t, st, sub, func(task store.Task) store.LeaseRequest {
		return store.LeaseRequest{WorkerID: workers[task.ID]}
	})

	attempts := make([]store.TaskAttempt, len(out.Tasks))
	for i, task := range out.Tasks {
		attempts[i] = leased[task.ID]
	}
	return out.Job, out.Tasks, attempts
}

// ── CancelJob tests ───────────────────────────────────────────────────────────

func TestCancelJob_NoActiveTasks(t *testing.T) {
	st := newCheckedFake(t)
	bus := &stubBus{}
	s := newTestScheduler(st, bus)

	job, _, _ := seedCancelJob(t, st)
	// No tasks at all → no NATS publishes.
	if err := s.CancelJob(t.Context(), job.ID); err != nil {
		t.Fatalf("CancelJob: %v", err)
	}
	if len(bus.cancelCalls) != 0 {
		t.Errorf("expected 0 NATS publishes, got %d", len(bus.cancelCalls))
	}
}

func TestCancelJob_WithAssignedWorkers(t *testing.T) {
	st := newCheckedFake(t)
	bus := &stubBus{}
	s := newTestScheduler(st, bus)

	// Two tasks with different workers.
	job, _, _ := seedCancelJob(
		t, st,
		cancelTask{status: store.TaskStatusRunning, worker: "worker-1"},
		cancelTask{status: store.TaskStatusAssigned, worker: "worker-2"},
	)

	if err := s.CancelJob(t.Context(), job.ID); err != nil {
		t.Fatalf("CancelJob: %v", err)
	}
	// Both active tasks had workers → 2 cancel signals.
	if len(bus.cancelCalls) != 2 {
		t.Errorf("expected 2 NATS publishes, got %d", len(bus.cancelCalls))
	}
}

func TestCancelJob_NATSPublishFailure_NonFatal(t *testing.T) {
	// NATS publish failures must be non-fatal: CancelJob should still return nil.
	st := newCheckedFake(t)
	bus := &stubBus{cancelErr: errors.New("nats: unavailable")}
	s := newTestScheduler(st, bus)

	job, _, _ := seedCancelJob(t, st, cancelTask{status: store.TaskStatusRunning, worker: "worker-1"})

	if err := s.CancelJob(t.Context(), job.ID); err != nil {
		t.Fatalf("expected nil despite NATS failure, got %v", err)
	}
}

func TestCancelJob_TasksAreCanceledInStore(t *testing.T) {
	st := newCheckedFake(t)
	s := newTestScheduler(st, &stubBus{})

	job, tasks, _ := seedCancelJob(t, st, cancelTask{status: store.TaskStatusRunning, worker: "worker-1"})
	tk := tasks[0]

	if err := s.CancelJob(t.Context(), job.ID); err != nil {
		t.Fatalf("CancelJob: %v", err)
	}
	stored, err := st.GetTask(t.Context(), tk.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != store.TaskStatusCanceled {
		t.Errorf("task status = %q, want canceled", stored.Status)
	}
	if stored.FailureReason != "canceled by user" {
		t.Errorf("task failure_reason = %q, want %q", stored.FailureReason, "canceled by user")
	}
}

// TestCancelJob_DoesNotClobberCascadeReason asserts that a task already carrying
// a cascade-cancel reason keeps it when CancelJob runs — cascade (the specific
// cause) always wins over user-cancel, regardless of ordering. The reason is
// stamped by the same UPDATE that cancels the task, and only on a task with no
// reason yet, so this holds even under real concurrency; here we pre-set the
// reason to model a cascade that landed first.
func TestCancelJob_DoesNotClobberCascadeReason(t *testing.T) {
	st := newCheckedFake(t)
	s := newTestScheduler(st, &stubBus{})

	// A cascade-cancel already recorded the specific cause.
	job, tasks, _ := seedCancelJob(t, st, cancelTask{
		status: store.TaskStatusRunning, worker: "worker-1", reason: "canceled: upstream step failed",
	})
	tk := tasks[0]

	if err := s.CancelJob(t.Context(), job.ID); err != nil {
		t.Fatalf("CancelJob: %v", err)
	}

	stored, err := st.GetTask(t.Context(), tk.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.FailureReason != "canceled: upstream step failed" {
		t.Errorf("task failure_reason = %q, want cascade reason preserved %q",
			stored.FailureReason, "canceled: upstream step failed")
	}
}

// ── CancelTask tests ──────────────────────────────────────────────────────────

func TestCancelTask_NotFound(t *testing.T) {
	st := newCheckedFake(t)
	s := newTestScheduler(st, &stubBus{})

	err := s.CancelTask(t.Context(), "no-such-task")
	if err == nil {
		t.Fatal("expected error for non-existent task, got nil")
	}
}

func TestCancelTask_AlreadyTerminal_NoOp(t *testing.T) {
	for _, status := range []store.TaskStatus{
		store.TaskStatusSucceeded,
		store.TaskStatusFailed,
		store.TaskStatusCanceled,
	} {
		t.Run(string(status), func(t *testing.T) {
			st := newCheckedFake(t)
			bus := &stubBus{}
			s := newTestScheduler(st, bus)
			_, tasks, _ := seedCancelJob(t, st, cancelTask{status: status})
			tk := tasks[0]

			if err := s.CancelTask(t.Context(), tk.ID); err != nil {
				t.Fatalf("CancelTask on terminal task: %v", err)
			}
			// Verify no state change.
			stored, err := st.GetTask(t.Context(), tk.ID)
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			if stored.Status != status {
				t.Errorf("status changed from %q to %q (should be no-op)", status, stored.Status)
			}
			if len(bus.cancelCalls) != 0 {
				t.Error("expected 0 NATS publishes for terminal task")
			}
		})
	}
}

func TestCancelTask_AssignedTask_CanceledAndSignaled(t *testing.T) {
	st := newCheckedFake(t)
	bus := &stubBus{}
	s := newTestScheduler(st, bus)

	_, tasks, _ := seedCancelJob(t, st, cancelTask{status: store.TaskStatusAssigned, worker: "worker-99"})
	tk := tasks[0]

	if err := s.CancelTask(t.Context(), tk.ID); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}

	stored, err := st.GetTask(t.Context(), tk.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != store.TaskStatusCanceled {
		t.Errorf("task status = %q, want canceled", stored.Status)
	}
	if stored.FailureReason != "canceled by user" {
		t.Errorf("task failure_reason = %q, want %q", stored.FailureReason, "canceled by user")
	}
	if len(bus.cancelCalls) != 1 {
		t.Errorf("expected 1 NATS cancel signal, got %d", len(bus.cancelCalls))
	}
}

// TestCancelTask_DoesNotClobberCascadeReason mirrors the CancelJob case: a task
// already annotated with a cascade-cancel reason keeps it through CancelTask.
func TestCancelTask_DoesNotClobberCascadeReason(t *testing.T) {
	st := newCheckedFake(t)
	s := newTestScheduler(st, &stubBus{})

	_, tasks, _ := seedCancelJob(t, st, cancelTask{
		status: store.TaskStatusRunning, worker: "worker-1", reason: "canceled: upstream step failed",
	})
	tk := tasks[0]

	if err := s.CancelTask(t.Context(), tk.ID); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}

	stored, err := st.GetTask(t.Context(), tk.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.FailureReason != "canceled: upstream step failed" {
		t.Errorf("task failure_reason = %q, want cascade reason preserved", stored.FailureReason)
	}
}

func TestCancelTask_ReadyTask_NoNATSSignal(t *testing.T) {
	// Ready tasks have no assigned worker — no NATS signal should be published.
	st := newCheckedFake(t)
	bus := &stubBus{}
	s := newTestScheduler(st, bus)

	_, tasks, _ := seedCancelJob(t, st, cancelTask{status: store.TaskStatusReady})
	tk := tasks[0]

	if err := s.CancelTask(t.Context(), tk.ID); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	if len(bus.cancelCalls) != 0 {
		t.Errorf("expected 0 NATS signals for unassigned task, got %d", len(bus.cancelCalls))
	}
	stored, err := st.GetTask(t.Context(), tk.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != store.TaskStatusCanceled {
		t.Errorf("task status = %q, want canceled", stored.Status)
	}
}
