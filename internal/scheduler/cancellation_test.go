// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Tests for cancellation.go — item 8b of the test roadmap.
//
// CancelJob and CancelTask are methods on *Scheduler, so these are white-box
// tests in package scheduler. A stubBus satisfies the busClient interface so
// NATS publish calls can be recorded or failed without a real broker.

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	nats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/ws"
)

// ── stubBus: minimal busClient for cancellation tests ────────────────────────

// stubBus records PublishTaskCancel calls and can be configured to return an
// error.  All Consume* and PublishWorkAssign methods are no-ops.
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
func (*stubBus) PublishWorkAssign(_ context.Context, _ string, _ []byte) error { return nil }

func (*stubBus) SubscribeWorkerDiag(_ func(subject string, data []byte)) (*nats.Subscription, error) {
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
	)
}

// ── seed helpers ──────────────────────────────────────────────────────────────

func seedCancelJob(t *testing.T, st *fake.Store) store.Job {
	t.Helper()
	ctx := t.Context()
	now := time.Now()
	farm, err := st.CreateFarm(ctx, store.Farm{ID: uuid.NewString(), Name: "f"})
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queue, err := st.CreateQueue(ctx, store.Queue{ID: uuid.NewString(), FarmID: farm.ID, Name: "q"})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	job, err := st.CreateJob(ctx, store.Job{
		ID:             uuid.NewString(),
		FarmID:         farm.ID,
		QueueID:        queue.ID,
		Name:           "test-job",
		Status:         store.JobStatusRunning,
		TemplateFormat: store.TemplateFormatJSON,
		CreatedAt:      now,
		UpdatedAt:      now,
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	return job
}

func seedTaskForJob(t *testing.T, st *fake.Store, job store.Job, workerID string, status store.TaskStatus) store.Task {
	t.Helper()
	ctx := t.Context()
	now := time.Now()
	step, err := st.CreateStep(ctx, store.Step{
		ID: uuid.NewString(), JobID: job.ID, Name: uuid.NewString(),
		Status: store.StepStatusRunning, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}
	task, err := st.CreateTask(ctx, store.Task{
		ID:               uuid.NewString(),
		JobID:            job.ID,
		StepID:           step.ID,
		Name:             "t",
		Status:           status,
		AssignedWorkerID: workerID,
		CreatedAt:        now,
		UpdatedAt:        now,
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	return task
}

// ── CancelJob tests ───────────────────────────────────────────────────────────

func TestCancelJob_NoActiveTasks(t *testing.T) {
	st := fake.New()
	bus := &stubBus{}
	s := newTestScheduler(st, bus)

	job := seedCancelJob(t, st)
	// No tasks at all → no NATS publishes.
	if err := s.CancelJob(t.Context(), job.ID); err != nil {
		t.Fatalf("CancelJob: %v", err)
	}
	if len(bus.cancelCalls) != 0 {
		t.Errorf("expected 0 NATS publishes, got %d", len(bus.cancelCalls))
	}
}

func TestCancelJob_WithAssignedWorkers(t *testing.T) {
	st := fake.New()
	bus := &stubBus{}
	s := newTestScheduler(st, bus)

	job := seedCancelJob(t, st)
	// Two tasks with different workers.
	seedTaskForJob(t, st, job, "worker-1", store.TaskStatusRunning)
	seedTaskForJob(t, st, job, "worker-2", store.TaskStatusAssigned)

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
	st := fake.New()
	bus := &stubBus{cancelErr: errors.New("nats: unavailable")}
	s := newTestScheduler(st, bus)

	job := seedCancelJob(t, st)
	seedTaskForJob(t, st, job, "worker-1", store.TaskStatusRunning)

	if err := s.CancelJob(t.Context(), job.ID); err != nil {
		t.Fatalf("expected nil despite NATS failure, got %v", err)
	}
}

func TestCancelJob_TasksAreCanceledInStore(t *testing.T) {
	st := fake.New()
	s := newTestScheduler(st, &stubBus{})

	job := seedCancelJob(t, st)
	tk := seedTaskForJob(t, st, job, "", store.TaskStatusRunning)

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
}

// ── CancelTask tests ──────────────────────────────────────────────────────────

func TestCancelTask_NotFound(t *testing.T) {
	st := fake.New()
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
			st := fake.New()
			bus := &stubBus{}
			s := newTestScheduler(st, bus)
			job := seedCancelJob(t, st)
			tk := seedTaskForJob(t, st, job, "", status)

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
	st := fake.New()
	bus := &stubBus{}
	s := newTestScheduler(st, bus)

	job := seedCancelJob(t, st)
	tk := seedTaskForJob(t, st, job, "worker-99", store.TaskStatusAssigned)

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
	if len(bus.cancelCalls) != 1 {
		t.Errorf("expected 1 NATS cancel signal, got %d", len(bus.cancelCalls))
	}
}

func TestCancelTask_ReadyTask_NoNATSSignal(t *testing.T) {
	// Ready tasks have no assigned worker — no NATS signal should be published.
	st := fake.New()
	bus := &stubBus{}
	s := newTestScheduler(st, bus)

	job := seedCancelJob(t, st)
	tk := seedTaskForJob(t, st, job, "", store.TaskStatusReady)

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
