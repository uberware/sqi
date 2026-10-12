// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore_test

import (
	"fmt"
	"slices"
	"strings"
	"sync"
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
	farmID, queueID := seedFarmQueueRace(t, st)
	w := world{FarmID: farmID, QueueID: queueID, JobID: "job-" + uuid.NewString()[:8], StepID: "step-" + uuid.NewString()[:8]}
	seed := storetest.NewJob(w.JobID, farmID, queueID).Step(w.StepID, store.StepStatusReady)
	for i := range nTasks {
		id := fmt.Sprintf("%s-t%d", w.JobID, i)
		seed.Task(id, w.StepID, store.TaskStatusReady)
		w.Tasks = append(w.Tasks, id)
	}
	seed.Submit(t, st)
	registerOnline(t, st, farmID, workers...)
	w.Workers = append(w.Workers, workers...)
	return w
}

// seedFarmQueueRace creates one farm and one queue in it, with unique names.
func seedFarmQueueRace(t *testing.T, st store.Store) (farmID, queueID string) {
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
	return farm.ID, queue.ID
}

// registerOnline registers each named worker in the farm, online with a fresh
// heartbeat.
func registerOnline(t *testing.T, st store.Store, farmID string, ids ...string) {
	t.Helper()
	now := time.Now().UTC()
	for _, id := range ids {
		if _, _, err := st.RegisterWorker(t.Context(), store.Worker{
			ID: id, FarmID: farmID, Hostname: id, Status: store.WorkerStatusOnline, LastHeartbeatAt: &now,
		}); err != nil {
			t.Fatalf("RegisterWorker %s: %v", id, err)
		}
	}
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

// TestPGRace_G9_OfflineReclaimsEveryTaskALeaseCommitted races a lease against
// the heartbeat sweep taking its worker offline, in both orders. The worker
// row's shared lock serializes the two: an offline transition never commits
// while a lease to its worker is in flight, so it reclaims every task assigned
// to the worker when it commits. A lease that waits for the transition then
// lands on the offline worker, which is the documented behavior (ruling R10:
// the lease refuses only a disabled worker; see "Known gaps" in
// docs/architecture.md).
func TestPGRace_G9_OfflineReclaimsEveryTaskALeaseCommitted(t *testing.T) {
	t.Run("lease first", func(t *testing.T) {
		st, p := pgRace(t)
		w := seedWorld(t, st, 1, "w1")
		p.atStatement(sqlstore.SQLNextAttemptNumber) // the lease has moved the task, not committed
		lerr, oerr, waited := interleave(t, p,
			func() error { _, err := st.LeaseTask(t.Context(), leaseReq(w.Tasks[0], "w1")); return err },
			func() error {
				_, _, err := st.OfflineStaleWorker(t.Context(), "w1", time.Now().Add(time.Hour), time.Now())
				return err
			})
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
		w := seedWorld(t, st, 2, "w1")
		storetest.Lease(t, st, leaseReq(w.Tasks[0], "w1")) // in flight before the sweep
		p.atStatement(sqlstore.SQLWorkerInFlightJobIDs)    // the worker is marked offline, not committed
		var (
			reclaimed []store.Task
			lease     store.LeaseResult
		)
		oerr, lerr, waited := interleave(t, p,
			func() (err error) {
				reclaimed, _, err = st.OfflineStaleWorker(t.Context(), "w1", time.Now().Add(time.Hour), time.Now())
				return
			},
			func() (err error) { lease, err = st.LeaseTask(t.Context(), leaseReq(w.Tasks[1], "w1")); return })
		assertPausedAt(t, p, sqlstore.SQLWorkerInFlightJobIDs)
		if lerr != nil || oerr != nil {
			t.Fatalf("lease err %v, offline err %v", lerr, oerr)
		}
		if !waited {
			t.Errorf("the lease did not wait for the offline sweep's worker-row lock")
		}
		if len(reclaimed) != 1 || reclaimed[0].ID != w.Tasks[0] {
			t.Fatalf("offline reclaimed %+v, want exactly task %s, the one assigned to w1 when it committed", reclaimed, w.Tasks[0])
		}
		if got := mustTask(t, st, w.Tasks[0]); got.Status != store.TaskStatusReady {
			t.Fatalf("reclaimed task status = %s, want ready", got.Status)
		}
		if lease.Outcome != store.LeaseLeased {
			t.Fatalf("lease outcome = %s, want leased: an offline worker is not refused (R10)", lease.Outcome)
		}
		if got := mustTask(t, st, w.Tasks[1]); got.Status != store.TaskStatusAssigned || got.AssignedWorkerID != "w1" {
			t.Fatalf("leased task = %s on %q, want assigned on w1", got.Status, got.AssignedWorkerID)
		}
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

// TestPGRace_LeaseRefusesAJobMovedBeforeItsLock: a job moved to another queue
// between the lease's scope read and its job lock must not be leased against
// the old queue. The move here is from an uncapped queue to one whose cap is
// already full, so a lease that kept the queue it read first would anchor and
// count nothing and land past the new queue's cap (I5).
func TestPGRace_LeaseRefusesAJobMovedBeforeItsLock(t *testing.T) {
	st, p := pgRace(t)
	w := seedWorld(t, st, 1, "w1")
	full, err := st.CreateQueue(t.Context(), store.Queue{
		ID: "queue-" + uuid.NewString()[:8], FarmID: w.FarmID, Name: "full", MaxConcurrentTasks: 1,
	})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	other := "job-" + uuid.NewString()[:8]
	storetest.NewJob(other, w.FarmID, full.ID).Step(other+"-s", store.StepStatusReady).
		Task(other+"-t0", other+"-s", store.TaskStatusReady).Submit(t, st)
	storetest.Lease(t, st, leaseReq(other+"-t0", "w1")) // the full queue's one slot
	job, err := st.GetJob(t.Context(), w.JobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	p.atAnchor("jobs", w.JobID) // after the scope read (the old queue), before the job lock
	var lease store.LeaseResult
	lerr, merr, _ := interleave(t, p,
		func() (err error) { lease, err = st.LeaseTask(t.Context(), leaseReq(w.Tasks[0], "w1")); return },
		func() error {
			job.QueueID = full.ID
			_, err := st.UpdateJob(t.Context(), job)
			return err
		})
	if got, want := p.matched(), "anchor jobs/"+w.JobID; got != want {
		t.Fatalf("paused at %q, want %q", got, want)
	}
	if lerr != nil || merr != nil {
		t.Fatalf("lease err %v, move err %v", lerr, merr)
	}
	if lease.Outcome == store.LeaseLeased {
		t.Fatalf("lease landed in queue %s past its cap of 1: it was decided against the queue read before the job moved", full.ID)
	}
	if got := mustTask(t, st, w.Tasks[0]); got.Status != store.TaskStatusReady {
		t.Fatalf("task status = %s, want ready (the refused lease writes nothing)", got.Status)
	}
}

// TestPGRace_G8_NoDemotionUnderALease: the stall sweep must not demote a
// running job while a lease of one of its tasks is in flight.
func TestPGRace_G8_NoDemotionUnderALease(t *testing.T) {
	st, p := pgRace(t)
	farm, queue := seedFarmQueueRace(t, st)
	storetest.NewJob("job-g8", farm, queue).As(store.JobStatusRunning).
		Step("step-g8", store.StepStatusReady).
		Task("job-g8-done", "step-g8", store.TaskStatusSucceeded).
		Task("job-g8-next", "step-g8", store.TaskStatusReady).
		Submit(t, st)
	registerOnline(t, st, farm, "w1")
	p.atStatement(sqlstore.SQLNextAttemptNumber) // the lease has moved the task, not committed
	var demoted []string
	lerr, derr, waited := interleave(t, p,
		func() error { _, err := st.LeaseTask(t.Context(), leaseReq("job-g8-next", "w1")); return err },
		func() (err error) { demoted, err = st.DemoteStalledJobs(t.Context(), time.Now()); return })
	assertPausedAt(t, p, sqlstore.SQLNextAttemptNumber)
	if lerr != nil || derr != nil {
		t.Fatalf("lease err %v, demote err %v", lerr, derr)
	}
	if !waited {
		t.Errorf("the sweep did not wait for the lease's job-row lock")
	}
	job, err := st.GetJob(t.Context(), "job-g8")
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != store.JobStatusRunning || len(demoted) != 0 {
		t.Fatalf("job %s demoted to %s (sweep returned %v) while its lease was in flight", job.ID, job.Status, demoted)
	}
}

// TestPGRace_G12_ReaperAndCompletionDoNotDeadlock: the reaper writes a task
// then its attempt; a completion writes the attempt then the task. Without the
// reaper taking the job row first they deadlock, and PostgreSQL aborts one,
// which the store then retries. With it, neither waits on the other's row.
func TestPGRace_G12_ReaperAndCompletionDoNotDeadlock(t *testing.T) {
	st, p := pgRace(t)
	w := seedWorld(t, st, 1, "w1")
	a1 := storetest.Lease(t, st, leaseReq(w.Tasks[0], "w1"))
	p.atStatement(sqlstore.SQLIsLatestAttempt) // the completion has closed a1, holds its row
	before := st.TxRetries()
	cerr, rerr, _ := interleave(t, p,
		func() error {
			_, err := st.CompleteTaskAttempt(t.Context(), store.AttemptCompletion{
				AttemptID: a1.ID, TaskID: w.Tasks[0], TaskStatus: store.TaskStatusSucceeded,
				AttemptStatus: store.AttemptStatusSucceeded, EndedAt: time.Now(),
			})
			return err
		},
		func() error {
			_, err := st.ReclaimStaleAssignedTasks(t.Context(), time.Now().Add(time.Hour))
			return err
		})
	assertPausedAt(t, p, sqlstore.SQLIsLatestAttempt)
	if cerr != nil || rerr != nil {
		t.Fatalf("completion err %v, reap err %v", cerr, rerr)
	}
	if n := st.TxRetries() - before; n != 0 {
		t.Fatalf("%d transaction retries: the reaper and the completion deadlocked", n)
	}
	if got := mustTask(t, st, w.Tasks[0]); got.Status != store.TaskStatusSucceeded {
		t.Fatalf("task = %s, want succeeded: the reaper must skip a task its worker completed", got.Status)
	}
}

// TestPGRace_G1_AReportedTaskIsNeverReaped hammers the reaper against running
// reports: a task its worker reported running is never returned to ready.
func TestPGRace_G1_AReportedTaskIsNeverReaped(t *testing.T) {
	st := storetest.Postgres(t)
	for i := range 50 {
		w := seedWorld(t, st, 1, fmt.Sprintf("w%d", i))
		a := storetest.Lease(t, st, leaseReq(w.Tasks[0], w.Workers[0]))
		var started bool
		var reaped []store.Task
		var wg sync.WaitGroup
		wg.Go(func() {
			started, _ = st.StartTaskAttempt(t.Context(), a.ID, w.Tasks[0], "sess", time.Now()) //nolint:errcheck // asserted through started
		})
		wg.Go(func() {
			reaped, _ = st.ReclaimStaleAssignedTasks(t.Context(), time.Now().Add(time.Hour)) //nolint:errcheck // asserted through reaped
		})
		wg.Wait()
		if started && slices.ContainsFunc(reaped, func(x store.Task) bool { return x.ID == w.Tasks[0] }) {
			t.Fatalf("iteration %d: the task was reported running and also reaped", i)
		}
	}
}

// TestPGRace_G3_ReaperReturnsExactlyWhatItReset: under concurrent completions,
// every task the reaper returns is one it reset (ready, its attempt failed),
// and none it returns was completed.
func TestPGRace_G3_ReaperReturnsExactlyWhatItReset(t *testing.T) {
	st := storetest.Postgres(t)
	w := seedWorld(t, st, 20, "w1")
	attempts := map[string]store.TaskAttempt{}
	for _, id := range w.Tasks {
		attempts[id] = storetest.Lease(t, st, leaseReq(id, "w1"))
	}
	var reaped []store.Task
	var wg sync.WaitGroup
	for i, id := range w.Tasks {
		if i%2 == 0 {
			continue
		}
		wg.Go(func() {
			_, _ = st.CompleteTaskAttempt(t.Context(), store.AttemptCompletion{ //nolint:errcheck // outcome checked below
				AttemptID: attempts[id].ID, TaskID: id, TaskStatus: store.TaskStatusSucceeded,
				AttemptStatus: store.AttemptStatusSucceeded, EndedAt: time.Now(),
			})
		})
	}
	wg.Go(func() {
		reaped, _ = st.ReclaimStaleAssignedTasks(t.Context(), time.Now().Add(time.Hour)) //nolint:errcheck // checked below
	})
	wg.Wait()
	for _, r := range reaped {
		if got := mustTask(t, st, r.ID); got.Status != store.TaskStatusReady {
			t.Errorf("reaper returned %s, which is %s now; it returned a task it did not reset", r.ID, got.Status)
		}
	}
}

// TestPGRace_FinalizeJobReportsTheStatusUnderTheLock: a FinalizeJob that waits
// for another finalize of the same job reports the job's status as the other
// left it (already terminal), not as it read it before waiting.
func TestPGRace_FinalizeJobReportsTheStatusUnderTheLock(t *testing.T) {
	st, p := pgRace(t)
	farm, queue := seedFarmQueueRace(t, st)
	storetest.NewJob("job-fin", farm, queue).As(store.JobStatusRunning).
		Step("step-fin", store.StepStatusCompleted).
		Task("job-fin-t", "step-fin", store.TaskStatusSucceeded).
		Submit(t, st)
	p.atStatement(sqlstore.SQLFinalizeJob)
	var status store.JobStatus
	var finalized bool
	pausedErr, competitorErr, waited := interleave(t, p,
		func() error { _, _, err := st.FinalizeJob(t.Context(), "job-fin", time.Now()); return err },
		func() (err error) {
			status, finalized, err = st.FinalizeJob(t.Context(), "job-fin", time.Now())
			return
		})
	assertPausedAt(t, p, sqlstore.SQLFinalizeJob)
	if pausedErr != nil || competitorErr != nil {
		t.Fatalf("finalize errors: paused %v, competitor %v", pausedErr, competitorErr)
	}
	if !waited {
		t.Error("the second finalize did not wait for the first's job-row lock")
	}
	if finalized || status != store.JobStatusCompleted {
		t.Fatalf("second FinalizeJob = (%s, %v), want (completed, false): it must report the status it found under the lock", status, finalized)
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
