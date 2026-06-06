// SPDX-License-Identifier: AGPL-3.0-only

package scheduler

// Tests for policy.go — item 8a of the test roadmap.
//
// policyGate is unexported so these tests live in package scheduler (white-box).
// All tests use fake.New() as the store — no NATS or real SQLite needed.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// seedPolicy creates a farm, queue, and job with the given concurrency limits.
func seedPolicy(
	t *testing.T,
	st *fake.Store,
	farmMax, queueMax int,
) (farm store.Farm, queue store.Queue, job store.Job) {
	t.Helper()
	ctx := t.Context()

	farm, err := st.CreateFarm(ctx, store.Farm{
		ID:                 uuid.NewString(),
		Name:               "farm",
		MaxConcurrentTasks: farmMax,
	})
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queue, err = st.CreateQueue(ctx, store.Queue{
		ID:                 uuid.NewString(),
		FarmID:             farm.ID,
		Name:               "queue",
		MaxConcurrentTasks: queueMax,
	})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	now := time.Now()
	job, err = st.CreateJob(ctx, store.Job{
		ID:             uuid.NewString(),
		FarmID:         farm.ID,
		QueueID:        queue.ID,
		Name:           "job",
		Priority:       50,
		Status:         store.JobStatusRunning,
		TemplateFormat: store.TemplateFormatJSON,
		CreatedAt:      now,
		UpdatedAt:      now,
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	return farm, queue, job
}

// addActiveTask inserts one running task for the given job into st.
func addActiveTask(t *testing.T, st *fake.Store, job store.Job) {
	t.Helper()
	ctx := t.Context()
	now := time.Now()
	step, err := st.CreateStep(ctx, store.Step{
		ID:        uuid.NewString(),
		JobID:     job.ID,
		Name:      uuid.NewString(), // unique per call — fake store enforces (JobID,Name) uniqueness
		Status:    store.StepStatusRunning,
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}
	if _, err := st.CreateTask(ctx, store.Task{
		ID:        uuid.NewString(),
		JobID:     job.ID,
		StepID:    step.ID,
		Name:      "t",
		Status:    store.TaskStatusRunning,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
}

// ── Queue limit ───────────────────────────────────────────────────────────────

func TestPolicyGate_QueueUnlimited(t *testing.T) {
	st := fake.New()
	farm, queue, job := seedPolicy(t, st, 0, 0)
	for range 5 {
		addActiveTask(t, st, job)
	}
	if err := policyGate(t.Context(), st, job, queue, farm); err != nil {
		t.Fatalf("expected nil for unlimited queue, got %v", err)
	}
}

func TestPolicyGate_QueueUnderLimit(t *testing.T) {
	st := fake.New()
	farm, queue, job := seedPolicy(t, st, 0, 2) // limit=2
	addActiveTask(t, st, job)                   // 1 active
	if err := policyGate(t.Context(), st, job, queue, farm); err != nil {
		t.Fatalf("expected nil (1 active, limit 2), got %v", err)
	}
}

func TestPolicyGate_QueueAtCapacity(t *testing.T) {
	st := fake.New()
	farm, queue, job := seedPolicy(t, st, 0, 2)
	addActiveTask(t, st, job) // 1
	addActiveTask(t, st, job) // 2 — at limit
	err := policyGate(t.Context(), st, job, queue, farm)
	if err == nil {
		t.Fatal("expected errPolicyBlocked, got nil")
	}
	if !errors.Is(err, errPolicyBlocked) {
		t.Errorf("expected errPolicyBlocked, got %v", err)
	}
}

// ── Farm limit ────────────────────────────────────────────────────────────────

func TestPolicyGate_FarmUnlimited(t *testing.T) {
	st := fake.New()
	farm, queue, job := seedPolicy(t, st, 0, 0)
	for range 5 {
		addActiveTask(t, st, job)
	}
	if err := policyGate(t.Context(), st, job, queue, farm); err != nil {
		t.Fatalf("expected nil for unlimited farm, got %v", err)
	}
}

func TestPolicyGate_FarmAtCapacity(t *testing.T) {
	st := fake.New()
	farm, queue, job := seedPolicy(t, st, 1, 0) // farm limit=1
	addActiveTask(t, st, job)                   // 1 — farm at limit
	err := policyGate(t.Context(), st, job, queue, farm)
	if err == nil {
		t.Fatal("expected errPolicyBlocked for farm at capacity, got nil")
	}
	if !errors.Is(err, errPolicyBlocked) {
		t.Errorf("expected errPolicyBlocked, got %v", err)
	}
}

func TestPolicyGate_QueuePassesFarmBlocks(t *testing.T) {
	st := fake.New()
	farm, queue, job := seedPolicy(t, st, 1, 10) // farm=1, queue=10
	addActiveTask(t, st, job)                    // 1 — farm full
	err := policyGate(t.Context(), st, job, queue, farm)
	if err == nil {
		t.Fatal("expected errPolicyBlocked when farm at capacity")
	}
	if !errors.Is(err, errPolicyBlocked) {
		t.Errorf("expected errPolicyBlocked, got %v", err)
	}
}

// ── Store error paths ──────────────────────────────────────────────────────────

func TestPolicyGate_QueueCountError(t *testing.T) {
	st := fake.New()
	farm, queue, job := seedPolicy(t, st, 0, 5) // non-zero limit triggers the count
	est := &policyErrSt{Store: st, queueErr: errors.New("db error")}
	err := policyGate(t.Context(), est, job, queue, farm)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if errors.Is(err, errPolicyBlocked) {
		t.Error("store error must not be wrapped as errPolicyBlocked")
	}
}

func TestPolicyGate_FarmCountError(t *testing.T) {
	st := fake.New()
	// queue limit=0 (unlimited) so we skip queue check and hit farm check
	farm, queue, job := seedPolicy(t, st, 5, 0)
	est := &policyErrSt{Store: st, farmErr: errors.New("db error")}
	err := policyGate(t.Context(), est, job, queue, farm)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if errors.Is(err, errPolicyBlocked) {
		t.Error("store error must not be wrapped as errPolicyBlocked")
	}
}

// ── policyErrSt wraps fake.Store to inject count errors ──────────────────────

type policyErrSt struct {
	store.Store

	queueErr error
	farmErr  error
}

func (e *policyErrSt) CountActiveTasksInQueue(_ context.Context, _ string) (int, error) {
	if e.queueErr != nil {
		return 0, e.queueErr
	}
	return 0, nil
}

func (e *policyErrSt) CountActiveTasksInFarm(_ context.Context, _ string) (int, error) {
	if e.farmErr != nil {
		return 0, e.farmErr
	}
	return 0, nil
}
