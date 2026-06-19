// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// minimalRenderJSON is a minimal OpenJD template with a step named "render".
const minimalRenderJSON = `{
  "specificationVersion": "jobtemplate-2023-09",
  "name": "j",
  "steps": [
    {
      "name": "render",
      "script": {
        "actions": {
          "onRun": { "command": "render" }
        }
      }
    }
  ]
}`

func seedLeaseFixture(t *testing.T, st *fake.Store, coresPerTask []*int) (store.Worker, []string) {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC()
	if _, err := st.CreateFarm(ctx, store.Farm{ID: "f1", Name: "F1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "q1", FarmID: "f1", Name: "Q1"}); err != nil {
		t.Fatal(err)
	}
	w, err := st.RegisterWorker(ctx, store.Worker{
		ID: "w1", FarmID: "f1", Hostname: "h1", Status: store.WorkerStatusOnline,
		CPUCount: 4, LastHeartbeatAt: &now, Tags: map[string]string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := st.CreateJob(ctx, store.Job{
		ID: uuid.NewString(), FarmID: "f1", QueueID: "q1", Name: "j",
		Status: store.JobStatusRunning, TemplateFormat: store.TemplateFormatJSON,
		RawTemplate: minimalRenderJSON,
		CreatedAt:   now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	step, err := st.CreateStep(ctx, store.Step{
		ID: uuid.NewString(), JobID: job.ID, Name: "render",
		Status: store.StepStatusReady, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i, c := range coresPerTask {
		tk, err := st.CreateTask(ctx, store.Task{
			ID: uuid.NewString(), JobID: job.ID, StepID: step.ID,
			Name: "t", Status: store.TaskStatusReady, Parameters: map[string]string{},
			RequiredCores: c, CreatedAt: now.Add(time.Duration(i) * time.Millisecond), UpdatedAt: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, tk.ID)
	}
	return w, ids
}

func TestSelectLeaseBatch_FillsFreeCores(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "f1")
	one := 1
	// four 1-core tasks on a 4-core worker -> all four fit in one batch.
	w, ids := seedLeaseFixture(t, st, []*int{&one, &one, &one, &one})

	batch, err := s.selectLeaseBatch(t.Context(), w)
	if err != nil {
		t.Fatalf("selectLeaseBatch: %v", err)
	}
	if len(batch) != 4 {
		t.Fatalf("batch len = %d, want 4", len(batch))
	}
	for _, id := range ids {
		got, err := st.GetTask(t.Context(), id)
		if err != nil {
			t.Fatalf("GetTask %s: %v", id, err)
		}
		if got.Status != store.TaskStatusAssigned || got.AssignedWorkerID != "w1" {
			t.Errorf("task %s status=%q worker=%q, want assigned/w1", id, got.Status, got.AssignedWorkerID)
		}
	}
}

func TestSelectLeaseBatch_RespectsCapacity(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "f1")
	three, two := 3, 2
	// 3-core + 2-core ready on a 4-core worker: only the 3-core fits (2-core would total 5).
	w, _ := seedLeaseFixture(t, st, []*int{&three, &two})

	batch, err := s.selectLeaseBatch(t.Context(), w)
	if err != nil {
		t.Fatalf("selectLeaseBatch: %v", err)
	}
	if len(batch) != 1 {
		t.Fatalf("batch len = %d, want 1 (only 3-core fits)", len(batch))
	}
}

func TestSelectLeaseBatch_UndeclaredIsFullMachine(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "f1")
	// one undeclared task + one 1-core task on a 4-core worker: undeclared needs
	// the whole machine, so it is taken alone (1-core would not also fit).
	one := 1
	w, _ := seedLeaseFixture(t, st, []*int{nil, &one})

	batch, err := s.selectLeaseBatch(t.Context(), w)
	if err != nil {
		t.Fatalf("selectLeaseBatch: %v", err)
	}
	if len(batch) != 1 {
		t.Fatalf("batch len = %d, want 1 (undeclared takes whole machine)", len(batch))
	}
}

// TestSelectLeaseBatch_PolicyStoreErrorPropagates verifies that a genuine DB
// error from CountActiveTasksInQueue (inside policyGate) is not swallowed as a
// silent skip but is instead returned as an error from selectLeaseBatch.
func TestSelectLeaseBatch_PolicyStoreErrorPropagates(t *testing.T) {
	inner := fake.New()
	one := 1
	w, _ := seedLeaseFixture(t, inner, []*int{&one})

	// Seed a queue-level concurrency limit so policyGate calls CountActiveTasksInQueue.
	q, err := inner.GetQueue(t.Context(), "q1")
	if err != nil {
		t.Fatal(err)
	}
	q.MaxConcurrentTasks = 10
	if _, err := inner.UpdateQueue(t.Context(), q); err != nil {
		t.Fatal(err)
	}

	errDB := errors.New("connection reset by peer")
	st := &policyErrStore{Store: inner, errCountQueue: errDB}
	s := newMetricsScheduler(st, &recordBus{}, "f1")

	_, batchErr := s.selectLeaseBatch(t.Context(), w)
	if batchErr == nil {
		t.Fatal("selectLeaseBatch: want error, got nil")
	}
	if !errors.Is(batchErr, errDB) {
		t.Errorf("selectLeaseBatch err = %v, want to wrap errDB", batchErr)
	}
}

// policyErrStore wraps a [store.Store] and injects errCountQueue on
// CountActiveTasksInQueue calls to simulate a DB failure inside policyGate.
type policyErrStore struct {
	store.Store

	errCountQueue error
}

func (s *policyErrStore) CountActiveTasksInQueue(_ context.Context, _ string) (int, error) {
	return 0, s.errCountQueue
}
