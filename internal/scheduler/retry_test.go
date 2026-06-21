// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Tests for retry.go — RetryJob and RetryTask.
//
// White-box tests in package scheduler, using the fake store and a local
// capturing notifier. The retryNotifier records both NotifyJob and NotifyTask
// calls; other Notify* methods are inherited as no-ops from ws.NoopNotifier.

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/uberware/sqi/internal/store"
	fakestore "github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/ws"
)

// ── capturing notifier ────────────────────────────────────────────────────────

// retryNotifier records NotifyJob and NotifyTask calls for retry test assertions.
// All other Notify* methods are discarded via the embedded ws.NoopNotifier.
type retryNotifier struct {
	ws.NoopNotifier

	mu         sync.Mutex
	jobEvents  []ws.JobEvent
	taskEvents []ws.TaskEvent
}

func (n *retryNotifier) NotifyJob(e ws.JobEvent) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.jobEvents = append(n.jobEvents, e)
}

func (n *retryNotifier) NotifyTask(e ws.TaskEvent) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.taskEvents = append(n.taskEvents, e)
}

func (n *retryNotifier) JobEventCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.jobEvents)
}

func (n *retryNotifier) TaskEventCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.taskEvents)
}

// newTestSchedulerWithNotifier returns a *Scheduler wired with the given store
// and a *retryNotifier that captures job and task events.
// It is safe to call RetryJob/RetryTask without ever calling Run.
func newTestSchedulerWithNotifier(t *testing.T, st store.Store) (*Scheduler, *retryNotifier) {
	t.Helper()
	n := &retryNotifier{}
	s := New(
		DefaultConfig(),
		st,
		nil, // bus — not required for RetryJob/RetryTask
		nil, // metrics — not used
		slog.New(slog.DiscardHandler),
		n,
		nil, // diagBuf — diagnostics disabled
	)
	return s, n
}

// ── seed helpers ──────────────────────────────────────────────────────────────

// seedRetryFixture builds j1 with s1 (failed, no deps) → t1 (failed) and
// s2 (canceled, depends on s1) → t2 (canceled), job status failed.
func seedRetryFixture(t *testing.T, st *fakestore.Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.CreateFarm(ctx, store.Farm{ID: "f1", Name: "f1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "q1", FarmID: "f1", Name: "q1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateJob(ctx, store.Job{ID: "j1", FarmID: "f1", QueueID: "q1", Name: "j1", Status: store.JobStatusFailed}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateStep(ctx, store.Step{ID: "s1", JobID: "j1", Name: "s1", Status: store.StepStatusFailed}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateStep(ctx, store.Step{ID: "s2", JobID: "j1", Name: "s2", Status: store.StepStatusCanceled, DependsOn: []string{"s1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTask(ctx, store.Task{ID: "t1", JobID: "j1", StepID: "s1", Name: "t1", Status: store.TaskStatusFailed}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTask(ctx, store.Task{ID: "t2", JobID: "j1", StepID: "s2", Name: "t2", Status: store.TaskStatusCanceled}); err != nil {
		t.Fatal(err)
	}
}

func seedCompletedJob(t *testing.T, st *fakestore.Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.CreateFarm(ctx, store.Farm{ID: "f1", Name: "f1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "q1", FarmID: "f1", Name: "q1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateJob(ctx, store.Job{ID: "j1", FarmID: "f1", QueueID: "q1", Name: "j1", Status: store.JobStatusCompleted}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateStep(ctx, store.Step{ID: "s1", JobID: "j1", Name: "s1", Status: store.StepStatusCompleted}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTask(ctx, store.Task{ID: "t1", JobID: "j1", StepID: "s1", Name: "t1", Status: store.TaskStatusSucceeded}); err != nil {
		t.Fatal(err)
	}
}

// ── RetryJob tests ────────────────────────────────────────────────────────────

func TestRetryJob_RevivesAndResolves(t *testing.T) {
	st := fakestore.New()
	defer st.Close()
	ctx := context.Background()

	// A two-step job: s1 (no deps, failed) -> s2 (depends on s1, cascade-canceled).
	seedRetryFixture(t, st)

	sched, notifier := newTestSchedulerWithNotifier(t, st)

	n, err := sched.RetryJob(ctx, "j1")
	if err != nil {
		t.Fatalf("RetryJob: %v", err)
	}
	if n != 2 {
		t.Fatalf("revived = %d, want 2", n)
	}

	// s1 had no deps: its task should be promoted to ready by ResolveDependencies.
	t1, err := st.GetTask(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTask(t1): %v", err)
	}
	if t1.Status != store.TaskStatusReady {
		t.Errorf("t1 = %v, want ready", t1.Status)
	}
	// s2 depends on s1 (not yet completed again): its task stays pending.
	t2, err := st.GetTask(ctx, "t2")
	if err != nil {
		t.Fatalf("GetTask(t2): %v", err)
	}
	if t2.Status != store.TaskStatusPending {
		t.Errorf("t2 = %v, want pending", t2.Status)
	}
	// Job revived out of its terminal status.
	job, err := st.GetJob(ctx, "j1")
	if err != nil {
		t.Fatalf("GetJob(j1): %v", err)
	}
	if job.Status != store.JobStatusPending {
		t.Errorf("job = %v, want pending", job.Status)
	}
	// A job event was emitted.
	if notifier.JobEventCount() == 0 {
		t.Error("expected at least one job event")
	}
	if notifier.TaskEventCount() < 2 {
		t.Errorf("task events = %d, want >= 2", notifier.TaskEventCount())
	}
}

func TestRetryJob_NoEligibleTasks(t *testing.T) {
	st := fakestore.New()
	defer st.Close()
	ctx := context.Background()
	seedCompletedJob(t, st) // all tasks succeeded
	sched, _ := newTestSchedulerWithNotifier(t, st)

	n, err := sched.RetryJob(ctx, "j1")
	if err != nil {
		t.Fatalf("RetryJob: %v", err)
	}
	if n != 0 {
		t.Errorf("revived = %d, want 0", n)
	}
}
