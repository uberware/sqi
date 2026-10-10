// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// Tests for policy.go.
//
// policyGate is unexported so these tests live in package scheduler (white-box).
// All tests use the fake store (newCheckedFake) — no NATS or real SQLite needed.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/store/storetest"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// seedPolicy creates a farm, queue, and job with the given concurrency limits,
// and active running tasks of that job, each leased and started through
// production writes in the job's one step. The leases count against the caps,
// so active must not exceed them.
func seedPolicy(
	t *testing.T,
	st *fake.Store,
	farmMax, queueMax, active int,
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
	sub := store.JobSubmission{Job: store.Job{
		ID:             uuid.NewString(),
		FarmID:         farm.ID,
		QueueID:        queue.ID,
		Name:           "job",
		Priority:       50,
		Status:         store.JobStatusRunning,
		TemplateFormat: store.TemplateFormatJSON,
	}}
	sub.Steps = []store.Step{{ID: uuid.NewString(), JobID: sub.Job.ID, Name: "step", Status: store.StepStatusRunning}}
	for i := range active {
		sub.Tasks = append(sub.Tasks, store.Task{
			ID: uuid.NewString(), JobID: sub.Job.ID, StepID: sub.Steps[0].ID,
			Name: fmt.Sprintf("t%d", i), Status: store.TaskStatusReady,
		})
	}
	job = storetest.Submit(t, st, sub).Job
	for _, task := range sub.Tasks {
		storetest.Running(t, st, store.LeaseRequest{TaskID: task.ID, WorkerID: "w-policy"})
	}
	return farm, queue, job
}

// ── Queue limit ───────────────────────────────────────────────────────────────

func TestPolicyGate_QueueUnlimited(t *testing.T) {
	st := newCheckedFake(t)
	farm, queue, job := seedPolicy(t, st, 0, 0, 5)
	if err := policyGate(t.Context(), st, job, queue, farm); err != nil {
		t.Fatalf("expected nil for unlimited queue, got %v", err)
	}
}

func TestPolicyGate_QueueUnderLimit(t *testing.T) {
	st := newCheckedFake(t)
	farm, queue, job := seedPolicy(t, st, 0, 2, 1) // limit=2, 1 active
	if err := policyGate(t.Context(), st, job, queue, farm); err != nil {
		t.Fatalf("expected nil (1 active, limit 2), got %v", err)
	}
}

func TestPolicyGate_QueueAtCapacity(t *testing.T) {
	st := newCheckedFake(t)
	farm, queue, job := seedPolicy(t, st, 0, 2, 2) // 2 active — at limit
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
	st := newCheckedFake(t)
	farm, queue, job := seedPolicy(t, st, 0, 0, 5)
	if err := policyGate(t.Context(), st, job, queue, farm); err != nil {
		t.Fatalf("expected nil for unlimited farm, got %v", err)
	}
}

func TestPolicyGate_FarmAtCapacity(t *testing.T) {
	st := newCheckedFake(t)
	farm, queue, job := seedPolicy(t, st, 1, 0, 1) // farm limit=1, 1 active — farm at limit
	err := policyGate(t.Context(), st, job, queue, farm)
	if err == nil {
		t.Fatal("expected errPolicyBlocked for farm at capacity, got nil")
	}
	if !errors.Is(err, errPolicyBlocked) {
		t.Errorf("expected errPolicyBlocked, got %v", err)
	}
}

func TestPolicyGate_QueuePassesFarmBlocks(t *testing.T) {
	st := newCheckedFake(t)
	farm, queue, job := seedPolicy(t, st, 1, 10, 1) // farm=1, queue=10, 1 active — farm full
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
	st := newCheckedFake(t)
	farm, queue, job := seedPolicy(t, st, 0, 5, 0) // non-zero limit triggers the count
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
	st := newCheckedFake(t)
	// queue limit=0 (unlimited) so we skip queue check and hit farm check
	farm, queue, job := seedPolicy(t, st, 5, 0, 0)
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
