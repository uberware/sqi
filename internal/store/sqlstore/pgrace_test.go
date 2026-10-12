// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/postgres"
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
		var startErr error
		var reaped []store.Task
		var reapErr error
		var wg sync.WaitGroup
		wg.Go(func() {
			started, startErr = st.StartTaskAttempt(t.Context(), a.ID, w.Tasks[0], "sess", time.Now())
		})
		wg.Go(func() {
			reaped, reapErr = st.ReclaimStaleAssignedTasks(t.Context(), time.Now().Add(time.Hour))
		})
		wg.Wait()
		if startErr != nil {
			t.Fatalf("iteration %d: the start failed, so the iteration checked nothing: %v", i, startErr)
		}
		if reapErr != nil {
			t.Fatalf("iteration %d: the reaper failed, so the test checked nothing: %v", i, reapErr)
		}
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
	var reapErr error
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
		reaped, reapErr = st.ReclaimStaleAssignedTasks(t.Context(), time.Now().Add(time.Hour))
	})
	wg.Wait()
	if reapErr != nil {
		t.Fatalf("the reaper failed, so the test checked nothing: %v", reapErr)
	}
	// The even-numbered tasks are never completed, so the reaper must return
	// at least those; an empty result would pass the loop below vacuously.
	if len(reaped) < len(w.Tasks)/2 {
		t.Fatalf("the reaper returned %d tasks, want at least %d (the ones nobody completed)", len(reaped), len(w.Tasks)/2)
	}
	for _, r := range reaped {
		if got := mustTask(t, st, r.ID); got.Status != store.TaskStatusReady {
			t.Errorf("reaper returned %s, which is %s now; it returned a task it did not reset", r.ID, got.Status)
		}
		at, err := st.GetTaskAttempt(t.Context(), attempts[r.ID].ID)
		if err != nil {
			t.Fatalf("GetTaskAttempt for %s: %v", r.ID, err)
		}
		if at.Status != store.AttemptStatusFailed {
			t.Errorf("reaper returned %s, but its attempt is %s, not failed: the task was reset without failing its attempt", r.ID, at.Status)
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

// TestPGRace_G6_RetentionSparesAJobRetriedMeanwhile: a job revived between the
// retention sweep's candidate read and its lock is not purged.
func TestPGRace_G6_RetentionSparesAJobRetriedMeanwhile(t *testing.T) {
	st, p := pgRace(t)
	farm, queue := seedFarmQueueRace(t, st)
	storetest.NewJob("job-g6", farm, queue).As(store.JobStatusCanceled).
		Step("step-g6", store.StepStatusCanceled).
		Task("job-g6-t", "step-g6", store.TaskStatusCanceled).
		Submit(t, st)
	p.atAnchor("jobs", "job-g6")
	var deleted []store.DeletedJob
	var revived []store.Task
	sweepErr, retryErr, waited := interleave(t, p,
		func() (err error) {
			deleted, err = st.DeleteTerminalJobsBefore(t.Context(), time.Now().Add(time.Hour), true)
			return
		},
		func() (err error) { revived, err = st.RetryTasks(t.Context(), "job-g6", nil, time.Now()); return })
	assertPausedAt(t, p, "anchor jobs/job-g6")
	if sweepErr != nil || retryErr != nil {
		t.Fatalf("errors: sweep %v, retry %v", sweepErr, retryErr)
	}
	if waited {
		t.Fatal("the retry waited: the sweep held the job before its anchor, so nothing raced it")
	}
	if len(revived) != 1 {
		t.Fatalf("the retry revived %d tasks, want 1: the race did not happen", len(revived))
	}
	if len(deleted) != 0 {
		t.Fatalf("retention purged %v, a job retried before the sweep locked it", deleted)
	}
	job, err := st.GetJob(t.Context(), "job-g6")
	if err != nil {
		t.Fatalf("job gone: %v", err)
	}
	if job.Status == store.JobStatusCanceled {
		t.Fatalf("job status = %s, want it revived by the retry", job.Status)
	}
	if got := mustTask(t, st, "job-g6-t").Status; got == store.TaskStatusCanceled {
		t.Fatalf("task status = %s, want it revived by the retry", got)
	}
}

// TestPGRace_G7_DeleteJobRetriesALogThatRaced: a log line appended after the
// delete cleared the logs must not fail the delete; the cascade runs again and
// removes it.
func TestPGRace_G7_DeleteJobRetriesALogThatRaced(t *testing.T) {
	st, p := pgRace(t)
	w := seedWorld(t, st, 1, "w1")
	a := storetest.Lease(t, st, leaseReq(w.Tasks[0], "w1"))
	p.atStatement(sqlstore.SQLDeleteJobAttempts) // logs cleared, attempts not yet
	derr, lerr, _ := interleave(t, p,
		func() error { return st.DeleteJob(t.Context(), w.JobID) },
		func() error {
			_, err := st.CreateTaskLog(t.Context(), store.TaskLog{
				ID: uuid.NewString(), TaskID: w.Tasks[0], AttemptID: a.ID, SeqNum: 1, NATSSeq: 1,
				At: time.Now(), Stream: store.LogStreamStdout, Data: "late line",
			})
			return err
		})
	assertPausedAt(t, p, sqlstore.SQLDeleteJobAttempts)
	if lerr != nil {
		t.Fatalf("the log append failed, so no log line raced the delete: %v", lerr)
	}
	if derr != nil {
		t.Fatalf("DeleteJob failed on a log line that raced it: %v", derr)
	}
	if _, err := st.GetJob(t.Context(), w.JobID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("job still present after DeleteJob: %v", err)
	}
	logs, err := st.ListTaskLogs(t.Context(), a.ID, 0, 10)
	if err != nil {
		t.Fatalf("ListTaskLogs: %v", err)
	}
	if len(logs) != 0 {
		t.Fatalf("%d log lines survive the deleted job", len(logs))
	}
}

// TestPGRace_G7_RetentionRetriesALogThatRaced: the retention sweep's cascade
// has the same hole as DeleteJob's. A log line appended after it cleared the
// logs must not fail the sweep; the sweep runs again and purges the job.
func TestPGRace_G7_RetentionRetriesALogThatRaced(t *testing.T) {
	st, p := pgRace(t)
	w := seedWorld(t, st, 1, "w1")
	a := storetest.Lease(t, st, leaseReq(w.Tasks[0], "w1"))
	if _, err := st.CancelJobExecution(t.Context(), w.JobID, "retire", time.Now()); err != nil {
		t.Fatalf("CancelJobExecution: %v", err)
	}
	p.atStatement(sqlstore.SQLDeleteJobAttempts) // logs cleared, attempts not yet
	var deleted []store.DeletedJob
	serr, lerr, _ := interleave(t, p,
		func() (err error) {
			deleted, err = st.DeleteTerminalJobsBefore(t.Context(), time.Now().Add(time.Hour), true)
			return
		},
		func() error {
			_, err := st.CreateTaskLog(t.Context(), store.TaskLog{
				ID: uuid.NewString(), TaskID: w.Tasks[0], AttemptID: a.ID, SeqNum: 1, NATSSeq: 1,
				At: time.Now(), Stream: store.LogStreamStdout, Data: "late line",
			})
			return err
		})
	assertPausedAt(t, p, sqlstore.SQLDeleteJobAttempts)
	if lerr != nil {
		t.Fatalf("the log append failed, so no log line raced the sweep: %v", lerr)
	}
	if serr != nil {
		t.Fatalf("the retention sweep failed on a log line that raced it: %v", serr)
	}
	if len(deleted) != 1 || deleted[0].ID != w.JobID {
		t.Fatalf("retention purged %v, want exactly %s", deleted, w.JobID)
	}
	if _, err := st.GetJob(t.Context(), w.JobID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("job still present after the sweep: %v", err)
	}
}

// anchorRecorder is the PostgreSQL dialect with the job anchors a store takes
// recorded in order.
type anchorRecorder struct {
	sqlstore.Dialect

	mu   *sync.Mutex
	jobs *[]string
}

func (d anchorRecorder) LockAnchors(ctx context.Context, tx *sql.Tx, anchors ...sqlstore.Anchor) error {
	d.mu.Lock()
	for _, a := range anchors {
		if a.Table == "jobs" {
			*d.jobs = append(*d.jobs, a.ID)
		}
	}
	d.mu.Unlock()
	return d.Dialect.LockAnchors(ctx, tx, anchors...)
}

// TestPGRace_RetentionLocksItsCandidatesInIDOrder: the sweep holds every lock
// until its one commit, so it must take the job anchors in id order whatever
// order its SELECT returned them in (insertion order here, with no ORDER BY).
func TestPGRace_RetentionLocksItsCandidatesInIDOrder(t *testing.T) {
	var mu sync.Mutex
	var locked []string
	opts := postgres.DefaultOptions()
	opts.WrapDialect = func(d sqlstore.Dialect) sqlstore.Dialect { return anchorRecorder{Dialect: d, mu: &mu, jobs: &locked} }
	st := storetest.PostgresWith(t, opts)
	farm, queue := seedFarmQueueRace(t, st)
	ids := []string{"job-c", "job-a", "job-d", "job-b"}
	for _, id := range ids {
		storetest.NewJob(id, farm, queue).As(store.JobStatusCompleted).
			Step("step-"+id, store.StepStatusCompleted).
			Task(id+"-t", "step-"+id, store.TaskStatusSucceeded).
			Submit(t, st)
	}
	mu.Lock()
	locked = nil
	mu.Unlock()
	deleted, err := st.DeleteTerminalJobsBefore(t.Context(), time.Now().Add(time.Hour), true)
	if err != nil {
		t.Fatalf("DeleteTerminalJobsBefore: %v", err)
	}
	if len(deleted) != len(ids) {
		t.Fatalf("purged %d jobs, want %d: %v", len(deleted), len(ids), deleted)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"job-a", "job-b", "job-c", "job-d"}; !slices.Equal(locked, want) {
		t.Fatalf("job anchors taken in the order %v, want %v", locked, want)
	}
	if got := idsOf(deleted); !slices.IsSorted(got) {
		t.Fatalf("purged jobs reported in the order %v, want id order", got)
	}
}

func idsOf(ds []store.DeletedJob) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.ID
	}
	return out
}
