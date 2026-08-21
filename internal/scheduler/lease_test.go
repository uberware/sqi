// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/bus"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// TestHandleLeaseRequest_QueuelessWorkerWildcardToken reproduces the
// queueless-worker bug at the scheduler layer: a worker with no queue affinity
// requests on bus.WildcardQueueToken and must still be leased the farm's ready
// tasks (selection is farm-wide + eligibility, and an empty-QueueID worker is
// eligible for any queue).
func TestHandleLeaseRequest_QueuelessWorkerWildcardToken(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "f1")
	s.leaseHoldTimeout = 50 * time.Millisecond
	one := 1
	w, _ := seedLeaseFixture(t, st, []*int{&one, &one}) // worker has empty QueueID

	req, err := json.Marshal(leaseRequest{WorkerID: w.ID})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	reply := s.handleLeaseRequest(w.ID, bus.WildcardQueueToken, req)

	var got leaseReply
	if err := json.Unmarshal(reply, &got); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if len(got.Assignments) != 2 {
		t.Fatalf("queueless worker on wildcard token leased %d tasks, want 2", len(got.Assignments))
	}
}

// TestWakeQueue_WakesWildcardParkedWorkers verifies a per-queue wake also wakes
// queue-unaffiliated workers parked under the wildcard token, so a queueless
// worker is leased newly-submitted work promptly rather than after a full hold.
func TestWakeQueue_WakesWildcardParkedWorkers(t *testing.T) {
	s := newMetricsScheduler(fake.New(), &recordBus{}, "f1")

	woke := make(chan bool, 1)
	go func() { woke <- s.waiters.wait(context.Background(), bus.WildcardQueueToken, time.Second) }()
	time.Sleep(20 * time.Millisecond) // let the wildcard waiter park

	s.WakeQueue("some-real-queue-id") // wake a specific queue

	select {
	case got := <-woke:
		if !got {
			t.Error("wildcard-parked waiter returned false; want woken by per-queue WakeQueue")
		}
	case <-time.After(time.Second):
		t.Fatal("wildcard-parked waiter was not woken by WakeQueue")
	}
}

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

func TestHandleLeaseRequest_ReturnsBatch(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "f1")
	s.leaseHoldTimeout = 50 * time.Millisecond
	one := 1
	w, _ := seedLeaseFixture(t, st, []*int{&one, &one})

	req, err := json.Marshal(leaseRequest{WorkerID: w.ID})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	reply := s.handleLeaseRequest(w.ID, "q1", req)

	var got leaseReply
	if err := json.Unmarshal(reply, &got); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if len(got.Assignments) != 2 {
		t.Fatalf("assignments = %d, want 2", len(got.Assignments))
	}
}

// TestHandleLeaseRequest_ConcurrentSameWorkerDoesNotOverLease verifies the
// per-worker selection lock: two concurrent lease requests for the SAME worker
// on a fixture with more ready cores than the worker can hold must not both read
// the same committed-core count and each lease up to free, over-committing the
// worker. Total leased cores must stay ≤ worker.CPUCount. Run with -race.
func TestHandleLeaseRequest_ConcurrentSameWorkerDoesNotOverLease(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "f1")
	s.leaseHoldTimeout = 50 * time.Millisecond
	// 4-core worker, six 1-core ready tasks (6 cores of demand > 4 capacity).
	one := 1
	w, _ := seedLeaseFixture(t, st, []*int{&one, &one, &one, &one, &one, &one})

	req, err := json.Marshal(leaseRequest{WorkerID: w.ID})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	for range 2 {
		go func() {
			defer wg.Done()
			_ = s.handleLeaseRequest(w.ID, "q1", req)
		}()
	}
	wg.Wait()

	// Sum cores across every task now committed to the worker.
	committed, err := st.CommittedCores(t.Context(), w.ID, w.CPUCount)
	if err != nil {
		t.Fatalf("CommittedCores: %v", err)
	}
	if committed > w.CPUCount {
		t.Fatalf("committed cores = %d, want ≤ %d (over-lease)", committed, w.CPUCount)
	}
}

// TestReclaimOfflineWorkerTasks_WakesParkedWaiters verifies Fix 2b: a bulk
// reclaim that returns tasks to ready broadcasts a wake so parked workers
// re-lease without waiting out leaseHoldTimeout.
func TestReclaimOfflineWorkerTasks_WakesParkedWaiters(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "f1")
	one := 1
	w, _ := seedLeaseFixture(t, st, []*int{&one, &one})

	// Assign the worker's tasks so reclaim has work to return (n > 0).
	if _, err := s.selectLeaseBatch(t.Context(), w); err != nil {
		t.Fatalf("selectLeaseBatch: %v", err)
	}

	woke := make(chan bool, 1)
	go func() { woke <- s.waiters.wait(context.Background(), "q1", time.Second) }()
	time.Sleep(20 * time.Millisecond) // let the waiter park

	s.reclaimOfflineWorkerTasks(t.Context(), w.ID, w.Hostname)

	select {
	case got := <-woke:
		if !got {
			t.Error("parked waiter not woken by reclaim (want notifyAll broadcast)")
		}
	case <-time.After(time.Second):
		t.Fatal("waiter did not return after reclaim")
	}
}

// TestSelectLeaseBatch_OversizedTaskNotLeased verifies that a ready task whose
// RequiredCores exceeds the worker's total CPUCount is not leased (it can never
// fit on this worker regardless of current load).
func TestSelectLeaseBatch_OversizedTaskNotLeased(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "f1")
	// Worker has 4 cores; task requires 8 — permanently unschedulable here.
	eight := 8
	_, ids := seedLeaseFixture(t, st, []*int{&eight})

	batch, err := s.selectLeaseBatch(t.Context(), store.Worker{
		ID: "w1", FarmID: "f1", Hostname: "h1", Status: store.WorkerStatusOnline,
		CPUCount: 4,
	})
	if err != nil {
		t.Fatalf("selectLeaseBatch: %v", err)
	}
	if len(batch) != 0 {
		t.Fatalf("batch len = %d, want 0 (task requires more cores than worker has)", len(batch))
	}
	// Task must still be ready — not consumed.
	tk, err := st.GetTask(t.Context(), ids[0])
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if tk.Status != store.TaskStatusReady {
		t.Errorf("task status = %q, want ready (oversized, not leased)", tk.Status)
	}
}

func TestHandleLeaseRequest_EmptyTimesOut(t *testing.T) {
	st := fake.New()
	s := newMetricsScheduler(st, &recordBus{}, "f1")
	s.leaseHoldTimeout = 40 * time.Millisecond
	// Register a worker but seed no ready tasks.
	now := time.Now().UTC()
	if _, err := st.CreateFarm(t.Context(), store.Farm{ID: "f1", Name: "F1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateQueue(t.Context(), store.Queue{ID: "q1", FarmID: "f1", Name: "Q1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterWorker(t.Context(), store.Worker{
		ID: "w1", FarmID: "f1", Status: store.WorkerStatusOnline, CPUCount: 4,
		LastHeartbeatAt: &now, Tags: map[string]string{},
	}); err != nil {
		t.Fatal(err)
	}

	req, err := json.Marshal(leaseRequest{WorkerID: "w1"})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	start := time.Now()
	reply := s.handleLeaseRequest("w1", "q1", req)
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Errorf("returned too fast (%v); expected to park until timeout", elapsed)
	}
	var got leaseReply
	if err := json.Unmarshal(reply, &got); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if len(got.Assignments) != 0 {
		t.Errorf("assignments = %d, want 0", len(got.Assignments))
	}
}
