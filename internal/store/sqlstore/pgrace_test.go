// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/sqlstore"
	"github.com/uberware/sqi/internal/store/storetest"
)

// world is one farm, queue, pending job, ready step and n ready tasks, with
// the named workers registered online.
type world struct {
	FarmID, QueueID, JobID, StepID string
	Tasks, Workers                 []string
}

func seedWorld(t *testing.T, st store.Store, nTasks int, workers ...string) world {
	t.Helper()
	ctx := t.Context()
	farm, err := st.CreateFarm(ctx, store.Farm{ID: "farm-" + uuid.NewString()[:8], Name: "f-" + uuid.NewString()[:8]})
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queue, err := st.CreateQueue(ctx, store.Queue{ID: "queue-" + uuid.NewString()[:8], FarmID: farm.ID, Name: "q"})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	w := world{FarmID: farm.ID, QueueID: queue.ID, JobID: "job-" + uuid.NewString()[:8], StepID: "step-" + uuid.NewString()[:8]}
	seed := storetest.NewJob(w.JobID, farm.ID, queue.ID).Step(w.StepID, store.StepStatusReady)
	for i := range nTasks {
		id := fmt.Sprintf("%s-t%d", w.JobID, i)
		seed.Task(id, w.StepID, store.TaskStatusReady)
		w.Tasks = append(w.Tasks, id)
	}
	seed.Submit(t, st)
	now := time.Now().UTC()
	for _, id := range workers {
		if _, _, err := st.RegisterWorker(ctx, store.Worker{
			ID: id, FarmID: farm.ID, Hostname: id, Status: store.WorkerStatusOnline, LastHeartbeatAt: &now,
		}); err != nil {
			t.Fatalf("RegisterWorker %s: %v", id, err)
		}
		w.Workers = append(w.Workers, id)
	}
	return w
}

func leaseReq(taskID, workerID string) store.LeaseRequest {
	return store.LeaseRequest{TaskID: taskID, WorkerID: workerID, AttemptID: uuid.NewString(), Now: time.Now().UTC()}
}

// assertPausedAt fails unless the pause fired at a statement containing want.
func assertPausedAt(t *testing.T, p *pause, want string) {
	t.Helper()
	if got := p.matched(); !strings.Contains(got, want) {
		t.Fatalf("paused at %q, want the statement %q", got, want)
	}
}

// TestPGRace_G4_CancelReportsALeaseThatRaced: a lease that lands while a job
// cancel is between reading the tasks it will report and canceling them must
// either be refused or be reported, so its worker is told to stop.
func TestPGRace_G4_CancelReportsALeaseThatRaced(t *testing.T) {
	st, p := pgRace(t)
	w := seedWorld(t, st, 1, "w1")
	var reported []store.Task
	var lease store.LeaseResult
	p.atStatement(sqlstore.SQLCancelJobTasks)
	cerr, lerr, waited := interleave(t, p,
		func() (err error) {
			reported, err = st.CancelJobExecution(t.Context(), w.JobID, "user", time.Now())
			return
		},
		func() (err error) { lease, err = st.LeaseTask(t.Context(), leaseReq(w.Tasks[0], "w1")); return })
	assertPausedAt(t, p, sqlstore.SQLCancelJobTasks)
	if cerr != nil || lerr != nil {
		t.Fatalf("cancel err %v, lease err %v", cerr, lerr)
	}
	if !waited {
		t.Errorf("the lease did not wait for the cancel's job-row lock")
	}
	if lease.Outcome == store.LeaseLeased && !slices.ContainsFunc(reported, func(x store.Task) bool { return x.ID == w.Tasks[0] }) {
		t.Fatalf("the lease landed, the cancel canceled it, and the cancel did not report it: worker w1 is never told to stop")
	}
	if got := mustTask(t, st, w.Tasks[0]); got.Status != store.TaskStatusCanceled {
		t.Fatalf("task status = %s, want canceled", got.Status)
	}
}

// TestPGRace_G9_NoTaskIsLeftWithAnOfflineWorker races a lease against the
// heartbeat sweep taking its worker offline, in both orders.
func TestPGRace_G9_NoTaskIsLeftWithAnOfflineWorker(t *testing.T) {
	offline := func(st store.Store) func() error {
		return func() error {
			_, _, err := st.OfflineStaleWorker(t.Context(), "w1", time.Now().Add(time.Hour), time.Now())
			return err
		}
	}
	t.Run("lease first", func(t *testing.T) {
		st, p := pgRace(t)
		w := seedWorld(t, st, 1, "w1")
		p.atStatement(sqlstore.SQLNextAttemptNumber) // the lease has moved the task, not committed
		lerr, oerr, waited := interleave(t, p,
			func() error { _, err := st.LeaseTask(t.Context(), leaseReq(w.Tasks[0], "w1")); return err },
			offline(st))
		assertPausedAt(t, p, sqlstore.SQLNextAttemptNumber)
		if lerr != nil || oerr != nil {
			t.Fatalf("lease err %v, offline err %v", lerr, oerr)
		}
		if !waited {
			t.Errorf("the offline sweep did not wait for the lease's worker-row lock")
		}
		assertNoTaskWithOfflineWorker(t, st, w)
	})
	t.Run("offline first", func(t *testing.T) {
		st, p := pgRace(t)
		w := seedWorld(t, st, 1, "w1")
		p.atStatement(sqlstore.SQLWorkerInFlightJobIDs) // the worker is marked offline, not committed
		var lease store.LeaseResult
		oerr, lerr, waited := interleave(t, p,
			offline(st),
			func() (err error) { lease, err = st.LeaseTask(t.Context(), leaseReq(w.Tasks[0], "w1")); return })
		assertPausedAt(t, p, sqlstore.SQLWorkerInFlightJobIDs)
		if lerr != nil || oerr != nil {
			t.Fatalf("lease err %v, offline err %v", lerr, oerr)
		}
		if !waited {
			t.Errorf("the lease did not wait for the offline sweep's worker-row lock")
		}
		if lease.Outcome == store.LeaseLeased {
			t.Errorf("a lease to a worker the sweep had just taken offline landed")
		}
		assertNoTaskWithOfflineWorker(t, st, w)
	})
}

// TestPGRace_G11_LateReportCannotEndATaskReLeasedMeanwhile: a superseded
// attempt's terminal report, held between its latest-attempt check and its
// task write, must not end the task under the attempt a concurrent lease
// creates.
func TestPGRace_G11_LateReportCannotEndATaskReLeasedMeanwhile(t *testing.T) {
	st, p := pgRace(t)
	w := seedWorld(t, st, 1, "w1", "w2")
	a1 := storetest.Lease(t, st, leaseReq(w.Tasks[0], "w1"))
	if _, err := st.ReclaimStaleAssignedTasks(t.Context(), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("reap: %v", err) // a1 closed failed, task ready
	}
	p.atStatement(sqlstore.SQLSelectTaskStatus)
	var res store.CompletionResult
	var lease store.LeaseResult
	cerr, lerr, waited := interleave(t, p,
		func() (err error) {
			res, err = st.CompleteTaskAttempt(t.Context(), store.AttemptCompletion{
				AttemptID: a1.ID, TaskID: w.Tasks[0], TaskStatus: store.TaskStatusSucceeded,
				AttemptStatus: store.AttemptStatusSucceeded, EndedAt: time.Now(),
			})
			return
		},
		func() (err error) { lease, err = st.LeaseTask(t.Context(), leaseReq(w.Tasks[0], "w2")); return })
	assertPausedAt(t, p, sqlstore.SQLSelectTaskStatus)
	if cerr != nil || lerr != nil {
		t.Fatalf("completion err %v, lease err %v", cerr, lerr)
	}
	if !waited {
		t.Errorf("the lease did not wait for the report's job-row lock")
	}
	task := mustTask(t, st, w.Tasks[0])
	if lease.Outcome == store.LeaseLeased && task.Status != store.TaskStatusAssigned {
		t.Fatalf("task re-leased to w2 but ended %s by w1's superseded report (completion %+v)", task.Status, res)
	}
	if v := storetest.ClaimViolations(t, st); len(v) != 0 {
		t.Fatalf("I3 violated: %v", v)
	}
}

// TestPGRace_LeaseEnforcesTheCapUnderTheLock: a queue cap lowered between the
// lease's scope read and its queue lock is the cap the lease enforces.
func TestPGRace_LeaseEnforcesTheCapUnderTheLock(t *testing.T) {
	st, p := pgRace(t)
	w := seedWorld(t, st, 2, "w1")
	q, err := st.GetQueue(t.Context(), w.QueueID)
	if err != nil {
		t.Fatal(err)
	}
	q.MaxConcurrentTasks = 5
	if _, err := st.UpdateQueue(t.Context(), q); err != nil {
		t.Fatal(err)
	}
	storetest.Lease(t, st, leaseReq(w.Tasks[0], "w1")) // one active task in the queue
	p.atAnchor("queues", w.QueueID)                    // after the scope read (cap 5), before the lock
	var lease store.LeaseResult
	lerr, uerr, _ := interleave(t, p,
		func() (err error) { lease, err = st.LeaseTask(t.Context(), leaseReq(w.Tasks[1], "w1")); return },
		func() error {
			q.MaxConcurrentTasks = 1
			_, err := st.UpdateQueue(t.Context(), q)
			return err
		})
	if got, want := p.matched(), "anchor queues/"+w.QueueID; got != want {
		t.Fatalf("paused at %q, want %q", got, want)
	}
	if lerr != nil || uerr != nil {
		t.Fatalf("lease err %v, queue update err %v", lerr, uerr)
	}
	if lease.Outcome != store.LeaseQueueFull {
		t.Fatalf("lease outcome = %s, want QueueFull under the cap of 1 set before the lease locked the queue", lease.Outcome)
	}
}

func mustTask(t *testing.T, st store.Store, id string) store.Task {
	t.Helper()
	task, err := st.GetTask(t.Context(), id)
	if err != nil {
		t.Fatalf("GetTask %s: %v", id, err)
	}
	return task
}

// assertNoTaskWithOfflineWorker fails if any task is assigned or running on a
// worker whose row says offline.
func assertNoTaskWithOfflineWorker(t *testing.T, st store.Store, w world) {
	t.Helper()
	for _, id := range w.Tasks {
		task := mustTask(t, st, id)
		if task.Status != store.TaskStatusAssigned && task.Status != store.TaskStatusRunning {
			continue
		}
		wk, err := st.GetWorker(t.Context(), task.AssignedWorkerID)
		if err != nil {
			t.Fatalf("GetWorker: %v", err)
		}
		if wk.Status == store.WorkerStatusOffline {
			t.Fatalf("task %s is %s on worker %s, which is offline", id, task.Status, wk.ID)
		}
	}
}
