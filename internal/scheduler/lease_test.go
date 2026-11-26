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
	"github.com/uberware/sqi/internal/store/storetest"
)

// TestHandleLeaseRequest_QueuelessWorkerWildcardToken reproduces the
// queueless-worker bug at the scheduler layer: a worker with no queue affinity
// requests on bus.WildcardQueueToken and must still be leased the farm's ready
// tasks (selection is farm-wide + eligibility, and a worker with no QueueIDs
// is eligible for any queue).
func TestHandleLeaseRequest_QueuelessWorkerWildcardToken(t *testing.T) {
	st := newCheckedFake(t)
	s := newMetricsScheduler(st, &recordBus{}, "f1")
	s.leaseHoldTimeout = 50 * time.Millisecond
	one := 1
	w, _ := seedLeaseFixture(t, st, []*int{&one, &one}) // worker has no QueueIDs

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
	s := newMetricsScheduler(newCheckedFake(t), &recordBus{}, "f1")

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

// readBeforeCommitStore makes the first ListReadyTasks see no ready tasks, as
// a selection that read the store just before a submission committed does,
// and then runs afterRead, standing in for that submission's wake.
type readBeforeCommitStore struct {
	store.Store

	once      sync.Once
	afterRead func()
}

func (s *readBeforeCommitStore) ListReadyTasks(ctx context.Context, farmID string, queueIDs []string, now time.Time, limit int) ([]store.Task, error) {
	first := false
	s.once.Do(func() { first = true })
	if !first {
		return s.Store.ListReadyTasks(ctx, farmID, queueIDs, now, limit)
	}
	s.afterRead()
	return nil, nil
}

// TestHandleLeaseRequest_WakeBetweenSelectionAndParkIsNotLost covers a
// submission whose wake fires after a lease request's selection found no work
// but before the request parked: the request must lease the task then, not
// leave the worker idle for the whole hold beside a ready task.
func TestHandleLeaseRequest_WakeBetweenSelectionAndParkIsNotLost(t *testing.T) {
	for _, queue := range []string{"q1", bus.WildcardQueueToken} {
		t.Run(queue, func(t *testing.T) {
			inner := newCheckedFake(t)
			st := &readBeforeCommitStore{Store: inner}
			s := newMetricsScheduler(st, &recordBus{}, "f1")
			s.leaseHoldTimeout = 5 * time.Second
			st.afterRead = func() { s.WakeQueue("q1") }
			one := 1
			w, _ := seedLeaseFixture(t, inner, []*int{&one})

			req, err := json.Marshal(leaseRequest{WorkerID: w.ID})
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}
			start := time.Now()
			reply := s.handleLeaseRequest(w.ID, queue, req)
			var got leaseReply
			if err := json.Unmarshal(reply, &got); err != nil {
				t.Fatalf("unmarshal reply: %v", err)
			}
			if len(got.Assignments) != 1 {
				t.Fatalf("leased %d tasks after %v, want 1 without waiting out the %v hold",
					len(got.Assignments), time.Since(start).Round(time.Millisecond), s.leaseHoldTimeout)
			}
			if d := time.Since(start); d > time.Second {
				t.Errorf("reply took %v, want well under the %v hold", d.Round(time.Millisecond), s.leaseHoldTimeout)
			}
		})
	}
}

// readBeforeEnableStore makes the first GetWorker see the worker disabled, as a
// read just before an enable committed does, and then runs afterRead, standing
// in for the enable handler's wake.
type readBeforeEnableStore struct {
	store.Store

	once      sync.Once
	afterRead func()
}

func (s *readBeforeEnableStore) GetWorker(ctx context.Context, id string) (store.Worker, error) {
	w, err := s.Store.GetWorker(ctx, id)
	first := false
	s.once.Do(func() { first = true })
	if first && err == nil {
		w.Disabled = true
		s.afterRead()
	}
	return w, err
}

// TestHandleLeaseRequest_EnableBetweenReadAndHoldIsNotLost covers an enable
// whose wake fires after a lease request read the worker as disabled but before
// the request was held: the request must lease the worker's work then, not
// after the whole hold.
func TestHandleLeaseRequest_EnableBetweenReadAndHoldIsNotLost(t *testing.T) {
	inner := newCheckedFake(t)
	st := &readBeforeEnableStore{Store: inner}
	s := newMetricsScheduler(st, &recordBus{}, "f1")
	s.leaseHoldTimeout = 5 * time.Second
	one := 1
	w, _ := seedLeaseFixture(t, inner, []*int{&one})
	st.afterRead = func() { s.WakeWorker(w.ID) }

	req, err := json.Marshal(leaseRequest{WorkerID: w.ID})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	start := time.Now()
	reply := s.handleLeaseRequest(w.ID, "q1", req)
	var got leaseReply
	if err := json.Unmarshal(reply, &got); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if len(got.Assignments) != 1 {
		t.Fatalf("leased %d tasks after %v, want 1 without waiting out the %v hold",
			len(got.Assignments), time.Since(start).Round(time.Millisecond), s.leaseHoldTimeout)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("reply took %v, want well under the %v hold", d.Round(time.Millisecond), s.leaseHoldTimeout)
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

// seedLeaseFixture seeds farm f1, queue q1, a 4-core worker w1, a running job
// and a ready "render" step with one ready task per entry of coresPerTask.
func seedLeaseFixture(t *testing.T, st store.Store, coresPerTask []*int) (store.Worker, []string) {
	t.Helper()
	return seedLeaseFixtureWith(t, st, coresPerTask, nil)
}

// seedLeaseFixtureWith is seedLeaseFixture with host requirements on the step,
// so a test can make the lease carry usage-pool claims. Pass nil for none.
func seedLeaseFixtureWith(
	t *testing.T, st store.Store, coresPerTask []*int, hostReqs *store.StepHostRequirements,
) (store.Worker, []string) {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC()
	if _, err := st.CreateFarm(ctx, store.Farm{ID: "f1", Name: "F1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "q1", FarmID: "f1", Name: "Q1"}); err != nil {
		t.Fatal(err)
	}
	w, _, err := st.RegisterWorker(ctx, store.Worker{
		ID: "w1", FarmID: "f1", Hostname: "h1", Status: store.WorkerStatusOnline,
		CPUCount: 4, LastHeartbeatAt: &now, Tags: map[string]string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return w, submitReadyIn(t, st, "q1", coresPerTask, hostReqs)
}

// submitReadyIn submits a running job in farm f1's queue queueID with one ready
// "render" step, which carries hostReqs (nil for none), and one ready task per
// entry of coresPerTask. It returns the task IDs in that order.
func submitReadyIn(
	t *testing.T, st store.Store, queueID string, coresPerTask []*int, hostReqs *store.StepHostRequirements,
) []string {
	t.Helper()
	return submitReadyWith(t, st, queueID, coresPerTask, hostReqs, nil)
}

// submitReadyWith is submitReadyIn with mutate (nil for none) applied to the
// submission before it is submitted.
func submitReadyWith(
	t *testing.T, st store.Store, queueID string, coresPerTask []*int, hostReqs *store.StepHostRequirements,
	mutate func(*store.JobSubmission),
) []string {
	t.Helper()
	sub := store.JobSubmission{Job: store.Job{
		ID: uuid.NewString(), FarmID: "f1", QueueID: queueID, Name: "j",
		Status: store.JobStatusRunning, TemplateFormat: store.TemplateFormatJSON,
		RawTemplate: minimalRenderJSON,
	}}
	sub.Steps = []store.Step{{
		ID: uuid.NewString(), JobID: sub.Job.ID, Name: "render",
		Status: store.StepStatusReady, HostRequirements: hostReqs,
	}}
	var ids []string
	for _, c := range coresPerTask {
		tk := store.Task{
			ID: uuid.NewString(), JobID: sub.Job.ID, StepID: sub.Steps[0].ID,
			Name: "t", Status: store.TaskStatusReady, Parameters: map[string]string{},
			RequiredCores: c,
		}
		sub.Tasks = append(sub.Tasks, tk)
		ids = append(ids, tk.ID)
	}
	if mutate != nil {
		mutate(&sub)
	}
	storetest.Submit(t, st, sub)
	return ids
}

func TestSelectLeaseBatch_FillsFreeCores(t *testing.T) {
	st := newCheckedFake(t)
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
	st := newCheckedFake(t)
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
	st := newCheckedFake(t)
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
	inner := newCheckedFake(t)
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
	st := newCheckedFake(t)
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

// TestLease_MultiQueueWorkerTakesOnlyItsQueues pins that a worker listing two
// queues is leased work only from those: with one ready task in each of three
// queues, its request leases exactly the two in its queues, and the third
// stays ready.
func TestLease_MultiQueueWorkerTakesOnlyItsQueues(t *testing.T) {
	st := newCheckedFake(t)
	s := newMetricsScheduler(st, &recordBus{}, "f1")
	s.leaseHoldTimeout = 50 * time.Millisecond
	ctx := t.Context()
	now := time.Now().UTC()
	if _, err := st.CreateFarm(ctx, store.Farm{ID: "f1", Name: "F1"}); err != nil {
		t.Fatal(err)
	}
	taskQueue := map[string]string{}
	one := 1
	for _, q := range []string{"qA", "qB", "qC"} {
		if _, err := st.CreateQueue(ctx, store.Queue{ID: q, FarmID: "f1", Name: q}); err != nil {
			t.Fatal(err)
		}
		taskQueue[submitReadyIn(t, st, q, []*int{&one}, nil)[0]] = q
	}
	w, _, err := st.RegisterWorker(ctx, store.Worker{
		ID: "w1", FarmID: "f1", Hostname: "h1", Status: store.WorkerStatusOnline,
		CPUCount: 8, LastHeartbeatAt: &now, Tags: map[string]string{}, QueueIDs: []string{"qA", "qB"},
	})
	if err != nil {
		t.Fatal(err)
	}

	req, err := json.Marshal(leaseRequest{WorkerID: w.ID})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var got leaseReply
	if err := json.Unmarshal(s.handleLeaseRequest(w.ID, "qA", req), &got); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if len(got.Assignments) != 2 {
		t.Fatalf("assignments = %d, want 2 (one each from qA and qB)", len(got.Assignments))
	}
	leased := map[string]bool{}
	for id, q := range taskQueue {
		tk, err := st.GetTask(ctx, id)
		if err != nil {
			t.Fatalf("GetTask %s: %v", id, err)
		}
		if tk.Status == store.TaskStatusAssigned {
			leased[q] = true
		}
		if q == "qC" && tk.Status != store.TaskStatusReady {
			t.Errorf("qC task status = %q, want ready", tk.Status)
		}
	}
	if !leased["qA"] || !leased["qB"] {
		t.Errorf("leased queues = %v, want qA and qB", leased)
	}
}

// TestSelectLeaseBatch_WorkerScopedBeforeBatchLimit pins that the ready list
// a lease walks is cut to the batch size only after it is narrowed to the work
// the worker can take. In each case a higher-priority job holds more ready
// tasks than the batch size in a queue or farm the worker does not serve, and
// the worker's own lower-priority task must still be leased.
func TestSelectLeaseBatch_WorkerScopedBeforeBatchLimit(t *testing.T) {
	cases := []struct {
		name          string
		schedulerFarm string
		workerQueues  []string
		blockerFarm   string // farm of the higher-priority job and its queue
	}{
		{"queue the worker does not list", "f1", []string{"qMine"}, "f1"},
		{"farm the worker is not in, scheduler managing every farm", "", nil, "f2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newCheckedFake(t)
			s := newMetricsScheduler(st, &recordBus{}, tc.schedulerFarm)
			s.cfg.AssignBatchSize = 2
			ctx := t.Context()
			now := time.Now().UTC()
			for _, f := range []string{"f1", "f2"} {
				if _, err := st.CreateFarm(ctx, store.Farm{ID: f, Name: f}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := st.CreateQueue(ctx, store.Queue{ID: "qOther", FarmID: tc.blockerFarm, Name: "qOther"}); err != nil {
				t.Fatal(err)
			}
			if _, err := st.CreateQueue(ctx, store.Queue{ID: "qMine", FarmID: "f1", Name: "qMine"}); err != nil {
				t.Fatal(err)
			}
			one := 1
			farmAndPriority := func(farm string, priority int) func(*store.JobSubmission) {
				return func(sub *store.JobSubmission) { sub.Job.FarmID, sub.Job.Priority = farm, priority }
			}
			submitReadyWith(t, st, "qOther", []*int{&one, &one, &one}, nil, farmAndPriority(tc.blockerFarm, 100))
			own := submitReadyWith(t, st, "qMine", []*int{&one}, nil, farmAndPriority("f1", 0))[0]
			w, _, err := st.RegisterWorker(ctx, store.Worker{
				ID: "w1", FarmID: "f1", Hostname: "h1", Status: store.WorkerStatusOnline,
				CPUCount: 4, LastHeartbeatAt: &now, Tags: map[string]string{}, QueueIDs: tc.workerQueues,
			})
			if err != nil {
				t.Fatal(err)
			}

			batch, err := s.selectLeaseBatch(ctx, w)
			if err != nil {
				t.Fatalf("selectLeaseBatch: %v", err)
			}
			if len(batch) != 1 {
				t.Fatalf("batch len = %d, want 1 (the worker's own task)", len(batch))
			}
			if got, err := st.GetTask(ctx, own); err != nil || got.Status != store.TaskStatusAssigned {
				t.Fatalf("own task = %+v (err %v), want assigned", got, err)
			}
		})
	}
}

// TestHandleLeaseRequest_ConcurrentSameWorkerDoesNotOverLease verifies the
// per-worker selection lock: two concurrent lease requests for the SAME worker
// on a fixture with more ready cores than the worker can hold must not both read
// the same committed-core count and each lease up to free, over-committing the
// worker. Total leased cores must stay ≤ worker.CPUCount. Run with -race.
func TestHandleLeaseRequest_ConcurrentSameWorkerDoesNotOverLease(t *testing.T) {
	st := newCheckedFake(t)
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
	st := newCheckedFake(t)
	s := newMetricsScheduler(st, &recordBus{}, "f1")
	one := 1
	w, _ := seedLeaseFixture(t, st, []*int{&one, &one})

	// Assign the worker's tasks so the offline transition has work to return.
	if _, err := s.selectLeaseBatch(t.Context(), w); err != nil {
		t.Fatalf("selectLeaseBatch: %v", err)
	}
	reclaimed, _, err := st.OfflineWorker(t.Context(), w.ID, "", time.Now().UTC())
	if err != nil || len(reclaimed) != 2 {
		t.Fatalf("OfflineWorker = (%d tasks, %v), want the worker's 2 leased tasks", len(reclaimed), err)
	}

	woke := make(chan bool, 1)
	go func() { woke <- s.waiters.wait(context.Background(), "q1", time.Second) }()
	time.Sleep(20 * time.Millisecond) // let the waiter park

	s.reclaimOfflineWorkerTasks(t.Context(), w.ID, w.Hostname, reclaimOffline, reclaimed)

	select {
	case got := <-woke:
		if !got {
			t.Error("parked waiter not woken by reclaim (want notifyAll broadcast)")
		}
	case <-time.After(time.Second):
		t.Fatal("waiter did not return after reclaim")
	}
}

// TestReclaimOfflineWorkerTasks_NothingReclaimedDoesNotWake pins the other
// branch: a worker that went offline holding nothing returns no tasks to the
// ready queue, so there is nothing for a parked waiter to re-lease and no wake.
func TestReclaimOfflineWorkerTasks_NothingReclaimedDoesNotWake(t *testing.T) {
	st := newCheckedFake(t)
	s := newMetricsScheduler(st, &recordBus{}, "f1")
	one := 1
	w, _ := seedLeaseFixture(t, st, []*int{&one})

	woke := parkWaiter(t, s, "q1")

	s.reclaimOfflineWorkerTasks(t.Context(), w.ID, w.Hostname, reclaimOffline, nil)

	select {
	case <-woke:
		t.Fatal("a waiter was woken although no task came back to ready")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestSelectLeaseBatch_OversizedTaskNotLeased verifies that a ready task whose
// RequiredCores exceeds the worker's total CPUCount is not leased (it can never
// fit on this worker regardless of current load).
func TestSelectLeaseBatch_OversizedTaskNotLeased(t *testing.T) {
	st := newCheckedFake(t)
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
	st := newCheckedFake(t)
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
	if _, _, err := st.RegisterWorker(t.Context(), store.Worker{
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

// TestAnswerLease_HoldsAStoppingReplyUntilReleased pins the shutdown order at
// the scheduler: while running, a lease reply goes out at once; once the
// scheduler is stopping, it is held until the lease subscriber has stopped
// taking requests, so the worker's next request cannot race the broker's
// removal of the interest.
func TestAnswerLease_HoldsAStoppingReplyUntilReleased(t *testing.T) {
	s := newTestScheduler(newCheckedFake(t), &recordBus{})
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	s.ctx = ctx
	s.leaseReplies, s.releaseLeaseReplies = context.WithCancel(context.Background())
	defer s.releaseLeaseReplies()
	// A request with no identity is answered empty at once, touching nothing.
	answer := func() <-chan []byte {
		got := make(chan []byte, 1)
		go func() { got <- s.answerLease("w1", "q1", []byte(`{}`)) }()
		return got
	}

	select {
	case <-answer():
	case <-time.After(5 * time.Second):
		t.Fatal("a running scheduler held its reply")
	}

	stop()
	held := answer()
	select {
	case <-held:
		t.Fatal("a stopping scheduler replied before the lease subscriber stopped taking requests")
	case <-time.After(100 * time.Millisecond):
	}
	s.releaseLeaseReplies()
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the held reply was not sent once released")
	}
}
