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

// TestPGRace_G7_DeleteJobHoldsOffALogThatRaced: a log line appended while a
// delete is mid-cascade (its logs cleared, its attempts not yet) waits for the
// delete, which locked the job's logged tasks, and then fails on its foreign
// key. The delete commits on its first run: no log row landed mid-cascade, so
// the cascade's retry backstop is not needed.
func TestPGRace_G7_DeleteJobHoldsOffALogThatRaced(t *testing.T) {
	st, p := pgRace(t)
	w := seedWorld(t, st, 1, "w1")
	a := storetest.Lease(t, st, leaseReq(w.Tasks[0], "w1"))
	p.atStatement(sqlstore.SQLDeleteJobAttempts) // logs cleared, attempts not yet
	before := st.TxRetries()
	derr, lerr, waited := interleave(t, p,
		func() error { return st.DeleteJob(t.Context(), w.JobID) },
		func() error { return appendLateLog(t, st, w.Tasks[0], a.ID) })
	assertPausedAt(t, p, sqlstore.SQLDeleteJobAttempts)
	if derr != nil {
		t.Fatalf("DeleteJob failed on a log line that raced it: %v", derr)
	}
	assertLogHeldOff(t, st, before, waited, lerr, a.ID)
	if _, err := st.GetJob(t.Context(), w.JobID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("job still present after DeleteJob: %v", err)
	}
}

// TestPGRace_G7_RetentionHoldsOffALogThatRaced: the retention sweep's cascade
// locks the job's logged tasks as DeleteJob's does. A log line appended after
// it cleared the logs waits for the sweep and fails on its foreign key, and
// the sweep purges the job on its first run.
func TestPGRace_G7_RetentionHoldsOffALogThatRaced(t *testing.T) {
	st, p := pgRace(t)
	w := seedWorld(t, st, 1, "w1")
	a := storetest.Lease(t, st, leaseReq(w.Tasks[0], "w1"))
	if _, err := st.CancelJobExecution(t.Context(), w.JobID, "retire", time.Now()); err != nil {
		t.Fatalf("CancelJobExecution: %v", err)
	}
	p.atStatement(sqlstore.SQLDeleteJobAttempts) // logs cleared, attempts not yet
	before := st.TxRetries()
	var deleted []store.DeletedJob
	serr, lerr, waited := interleave(t, p,
		func() (err error) {
			deleted, err = st.DeleteTerminalJobsBefore(t.Context(), time.Now().Add(time.Hour), true)
			return
		},
		func() error { return appendLateLog(t, st, w.Tasks[0], a.ID) })
	assertPausedAt(t, p, sqlstore.SQLDeleteJobAttempts)
	if serr != nil {
		t.Fatalf("the retention sweep failed on a log line that raced it: %v", serr)
	}
	assertLogHeldOff(t, st, before, waited, lerr, a.ID)
	if len(deleted) != 1 || deleted[0].ID != w.JobID {
		t.Fatalf("retention purged %v, want exactly %s", deleted, w.JobID)
	}
	if _, err := st.GetJob(t.Context(), w.JobID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("job still present after the sweep: %v", err)
	}
}

// appendLateLog appends one log line for attempt of task, as the log ingest
// consumer does.
func appendLateLog(t *testing.T, st *postgres.Store, task, attempt string) error {
	_, err := st.CreateTaskLog(t.Context(), store.TaskLog{
		ID: uuid.NewString(), TaskID: task, AttemptID: attempt, SeqNum: 1, NATSSeq: 1,
		At: time.Now(), Stream: store.LogStreamStdout, Data: "late line",
	})
	return err
}

// assertLogHeldOff checks how a log append that raced a job's delete cascade
// ended: it waited on a lock, failed on its foreign key once the delete
// committed, left no line behind, and the delete ran no more than once (no
// transaction retried since before).
func assertLogHeldOff(t *testing.T, st *postgres.Store, before int64, waited bool, lerr error, attempt string) {
	t.Helper()
	if !waited {
		t.Fatalf("the log append did not wait for the delete (err %v): the delete did not lock the job's logged tasks", lerr)
	}
	if !errors.Is(lerr, store.ErrConflict) {
		t.Fatalf("log append = %v, want a foreign-key conflict once the delete committed", lerr)
	}
	if n := st.TxRetries() - before; n != 0 {
		t.Errorf("%d transaction retries: the cascade ran again, so a log row landed mid-cascade", n)
	}
	logs, err := st.ListTaskLogs(t.Context(), attempt, 0, 10)
	if err != nil {
		t.Fatalf("ListTaskLogs: %v", err)
	}
	if len(logs) != 0 {
		t.Fatalf("%d log lines survive the deleted job", len(logs))
	}
}

// anchorRecorder is the PostgreSQL dialect with the job anchors each
// transaction takes recorded in order, keyed by transaction.
type anchorRecorder struct {
	sqlstore.Dialect

	mu   *sync.Mutex
	jobs map[*sql.Tx][]string
}

func (d anchorRecorder) LockAnchors(ctx context.Context, tx *sql.Tx, anchors ...sqlstore.Anchor) error {
	d.mu.Lock()
	for _, a := range anchors {
		if a.Table == "jobs" {
			d.jobs[tx] = append(d.jobs[tx], a.ID)
		}
	}
	d.mu.Unlock()
	return d.Dialect.LockAnchors(ctx, tx, anchors...)
}

// TestPGRace_RetentionLocksItsCandidatesInIDOrder: the sweep holds every lock
// until its one commit, so it must take the job anchors in id order whatever
// order its SELECT returned them in (heap order here, with no ORDER BY).
//
// The order is checked per transaction: under SQI_TEST_PG_FORCE_TX_RETRY=1 the
// sweep runs twice, the rehearsal rolled back, and each run must be sorted.
func TestPGRace_RetentionLocksItsCandidatesInIDOrder(t *testing.T) {
	var mu sync.Mutex
	locked := map[*sql.Tx][]string{}
	opts := postgres.DefaultOptions()
	opts.WrapDialect = func(d sqlstore.Dialect) sqlstore.Dialect { return anchorRecorder{Dialect: d, mu: &mu, jobs: locked} }
	st := storetest.PostgresWith(t, opts)
	farm, queue := seedFarmQueueRace(t, st)
	ids := []string{"job-c", "job-a", "job-d", "job-b"}
	for _, id := range ids {
		storetest.NewJob(id, farm, queue).As(store.JobStatusCompleted).
			Step("step-"+id, store.StepStatusCompleted).
			Task(id+"-t", "step-"+id, store.TaskStatusSucceeded).
			Submit(t, st)
	}
	// The test proves the sort only if the sweep's SELECT, which has no ORDER
	// BY, would hand the jobs back unsorted without it.
	if heap := heapOrderOfJobs(t, st); slices.IsSorted(heap) {
		t.Fatalf("a SELECT without ORDER BY returned %v, already in id order: this test would pass without the sort", heap)
	}
	mu.Lock()
	clear(locked)
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
	if len(locked) == 0 {
		t.Fatal("no transaction took a job anchor: the recorder saw nothing")
	}
	want := []string{"job-a", "job-b", "job-c", "job-d"}
	for _, got := range locked {
		if !slices.Equal(got, want) {
			t.Errorf("a sweep transaction took the job anchors in the order %v, want %v", got, want)
		}
	}
	if got := idsOf(deleted); !slices.IsSorted(got) {
		t.Fatalf("purged jobs reported in the order %v, want id order", got)
	}
}

// heapOrderOfJobs returns the job ids of the terminal jobs as a plain SELECT
// without ORDER BY yields them, on a separate connection.
func heapOrderOfJobs(t *testing.T, st *postgres.Store) []string {
	t.Helper()
	raw, err := sql.Open("pgx", st.DSNForTest())
	if err != nil {
		t.Fatalf("open a separate connection: %v", err)
	}
	t.Cleanup(func() {
		if err := raw.Close(); err != nil {
			t.Errorf("close the separate connection: %v", err)
		}
	})
	rows, err := raw.QueryContext(t.Context(), `SELECT id FROM jobs WHERE status IN ('completed', 'canceled', 'failed')`)
	if err != nil {
		t.Fatalf("select job ids: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan job id: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read job ids: %v", err)
	}
	return ids
}

func idsOf(ds []store.DeletedJob) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.ID
	}
	return out
}

// TestPGRace_G2_PoolCapHoldsUnderConcurrentLeases: a lease held between its
// claim count and its claim insert keeps a second lease from taking the last
// slot too.
func TestPGRace_G2_PoolCapHoldsUnderConcurrentLeases(t *testing.T) {
	st, p := pgRace(t)
	w := seedWorld(t, st, 2, "w1")
	pool, err := st.CreateUsagePool(t.Context(), store.UsagePool{ID: "pool-g2", Name: "lic-g2", MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	claim := func(task string) store.LeaseRequest {
		r := leaseReq(task, "w1")
		r.Claims = []store.UsagePoolClaim{{ClaimID: uuid.NewString(), PoolID: pool.ID, PoolName: pool.Name}}
		return r
	}
	p.atStatement(sqlstore.SQLInsertClaim) // the first lease has counted 0 claims, not inserted its own
	var first, second store.LeaseResult
	ferr, serr, waited := interleave(t, p,
		func() (err error) { first, err = st.LeaseTask(t.Context(), claim(w.Tasks[0])); return },
		func() (err error) { second, err = st.LeaseTask(t.Context(), claim(w.Tasks[1])); return })
	assertPausedAt(t, p, sqlstore.SQLInsertClaim)
	if ferr != nil || serr != nil {
		t.Fatalf("first lease err %v, second lease err %v", ferr, serr)
	}
	if n, err := st.ActiveClaimCount(t.Context(), pool.ID); err != nil || n != 1 {
		t.Fatalf("active claims = %d (err %v), want exactly 1 under the cap of 1", n, err)
	}
	if !waited {
		t.Error("the second lease did not wait for the first's pool-row lock")
	}
	if first.Outcome != store.LeaseLeased || second.Outcome != store.LeasePoolFull || second.FullPool != pool.Name {
		t.Fatalf("outcomes: first %s, second %s (full pool %q); want leased and pool_full on %q",
			first.Outcome, second.Outcome, second.FullPool, pool.Name)
	}
}

// TestPGRace_G5_RetryAndCancelSerialize: a single-task cancel that arrives
// while a retry is reviving the task waits for the retry and then cancels
// the revived task.
func TestPGRace_G5_RetryAndCancelSerialize(t *testing.T) {
	st, p := pgRace(t)
	farm, queue := seedFarmQueueRace(t, st)
	storetest.NewJob("job-g5", farm, queue).As(store.JobStatusFailed).
		Step("step-g5", store.StepStatusFailed).
		Task("job-g5-t", "step-g5", store.TaskStatusFailed).
		Submit(t, st)
	p.atStatement(sqlstore.SQLRetryTasksPrefix) // the retry holds the job row, has not revived the task
	var (
		revived  []store.Task
		prior    store.Task
		canceled bool
	)
	rerr, cerr, waited := interleave(t, p,
		func() (err error) { revived, err = st.RetryTasks(t.Context(), "job-g5", nil, time.Now()); return },
		func() (err error) {
			prior, canceled, err = st.CancelTaskExecution(t.Context(), "job-g5-t", "user", time.Now())
			return
		})
	assertPausedAt(t, p, sqlstore.SQLRetryTasksPrefix)
	if rerr != nil || cerr != nil {
		t.Fatalf("retry err %v, cancel err %v", rerr, cerr)
	}
	if len(revived) != 1 {
		t.Fatalf("the retry revived %d tasks, want 1", len(revived))
	}
	if !canceled || prior.Status != revived[0].Status {
		t.Fatalf("cancel = (canceled %v, found the task %s); want it to cancel the task as the retry left it (%s), not read it %s before the retry",
			canceled, prior.Status, revived[0].Status, store.TaskStatusFailed)
	}
	if !waited {
		t.Error("the cancel did not wait for the retry's job-row lock")
	}
	if got := mustTask(t, st, "job-g5-t"); got.Status != store.TaskStatusCanceled {
		t.Fatalf("task status = %s, want canceled", got.Status)
	}
}

// TestPGRace_G10_TwoAdminDeletionsLeaveOneAdmin: deleting the only two
// enabled admins at once refuses one of them.
//
// The guard counts the other enabled admins inside the DELETE, and under READ
// COMMITTED a statement reads its snapshot when it starts. Parking one delete
// before its statement therefore proves nothing: the other delete would finish
// first and the parked one would count a fresh snapshot, so it would refuse
// even with no anchors. The race needs both snapshots taken before either
// commit, so the test makes the second delete hold its snapshot while the
// first one runs: admin-b has a session, whose row a separate connection
// holds, and the cascade that removes it stalls the second delete after its
// guard has counted two admins. Without the admin anchors the second delete
// has counted (and removed admin-b) when the first one, parked at its DELETE,
// resumes, counts admin-b still alive and removes admin-a: no admin is left.
// With them the second delete waits for the first one's locks and, when it
// finally runs, counts one admin and is refused.
func TestPGRace_G10_TwoAdminDeletionsLeaveOneAdmin(t *testing.T) {
	st, p := pgRace(t)
	for _, id := range []string{"admin-a", "admin-b"} {
		if _, err := st.CreateUser(t.Context(), store.User{ID: id, Username: id, PasswordHash: "x", Role: "admin"}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	if _, err := st.CreateSession(t.Context(), store.Session{
		ID: "sess-b", TokenHash: "tok-b", UserID: "admin-b", ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("pgx", st.DSNForTest())
	if err != nil {
		t.Fatalf("open a separate connection: %v", err)
	}
	t.Cleanup(func() {
		if err := raw.Close(); err != nil {
			t.Errorf("close the separate connection: %v", err)
		}
	})
	hold, err := raw.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin the holding transaction: %v", err)
	}
	t.Cleanup(func() {
		if err := hold.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Errorf("roll back the holding transaction: %v", err)
		}
	})
	var one int
	if err := hold.QueryRowContext(t.Context(), `SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE`, "sess-b").Scan(&one); err != nil {
		t.Fatalf("hold admin-b's session row: %v", err)
	}

	p.atStatement(sqlstore.SQLDeleteUserKeepingAdmin) // admin-a's delete, before its statement reads a snapshot
	errA, errB, waited := interleave(t, p,
		func() error {
			err := st.DeleteUser(t.Context(), "admin-a")
			// Let admin-b's delete finish its cascade once admin-a's has committed.
			if rerr := hold.Rollback(); rerr != nil {
				t.Errorf("release admin-b's session row: %v", rerr)
			}
			return err
		},
		func() error { return st.DeleteUser(t.Context(), "admin-b") })
	assertPausedAt(t, p, sqlstore.SQLDeleteUserKeepingAdmin)
	users, err := st.ListUsers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 {
		t.Fatalf("users left = %d (errs %v, %v); the last enabled admin must never be deleted", len(users), errA, errB)
	}
	if errA != nil || !errors.Is(errB, store.ErrLastAdmin) {
		t.Fatalf("delete admin-a err %v, delete admin-b err %v; want nil and ErrLastAdmin", errA, errB)
	}
	if !waited {
		t.Error("the second delete did not wait for the first's admin-row locks")
	}
}

// TestPGRace_SubmissionDoesNotHoldItsUpstreamWhileInserting: a submission with
// depends_on takes its upstream job rows FOR SHARE only for the dependency
// re-check at the end of its transaction. While it is still inserting its
// tasks, a writer that locks an upstream job exclusively (here a worker's
// completion of an upstream task) must not wait for it: a 1M-task submission
// takes tens of seconds to insert.
func TestPGRace_SubmissionDoesNotHoldItsUpstreamWhileInserting(t *testing.T) {
	st, p := pgRace(t)
	up := seedWorld(t, st, 1, "w1")
	attempt := storetest.Running(t, st, leaseReq(up.Tasks[0], "w1"))

	downID, downStep := "job-"+uuid.NewString()[:8], "step-"+uuid.NewString()[:8]
	seed := storetest.NewJob(downID, up.FarmID, up.QueueID).As(store.JobStatusBlocked).Step(downStep, store.StepStatusPending)
	for i := range 10 {
		seed.Task(fmt.Sprintf("%s-t%d", downID, i), downStep, store.TaskStatusPending)
	}
	seed.Sub.DependsOn = []string{up.JobID}

	var done store.CompletionResult
	p.atStatement(sqlstore.SQLInsertTasksHead)
	serr, cerr, waited := interleave(t, p,
		func() error { _, err := st.CreateJobSubmission(t.Context(), seed.Sub); return err },
		func() (err error) {
			done, err = st.CompleteTaskAttempt(t.Context(), store.AttemptCompletion{
				AttemptID: attempt.ID, TaskID: attempt.TaskID, TaskStatus: store.TaskStatusSucceeded,
				AttemptStatus: store.AttemptStatusSucceeded, EndedAt: time.Now().UTC(),
			})
			return err
		})
	assertPausedAt(t, p, sqlstore.SQLInsertTasksHead)
	if serr != nil || cerr != nil {
		t.Fatalf("submission err %v, completion err %v", serr, cerr)
	}
	if !done.Applied {
		t.Fatalf("completion not applied: %+v", done)
	}
	if waited {
		t.Error("a completion on the upstream job waited for a submission still inserting its tasks: " +
			"the submission locked its upstream before its inserts, not just for the re-check before commit")
	}
	job, err := st.GetJob(t.Context(), downID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Status != store.JobStatusBlocked {
		t.Fatalf("submitted job status = %s, want blocked", job.Status)
	}
	if got := mustTask(t, st, downID+"-t9"); got.Status != store.TaskStatusPending {
		t.Fatalf("last submitted task status = %s, want pending", got.Status)
	}
}

// TestPGRace_SubmissionDoesNotBlockLeasesOnItsQueue: a submission's new job row
// references its queue and farm, so PostgreSQL holds FOR KEY SHARE on both rows
// from the job's insert to the submission's commit. A lease anchors a capped
// queue and farm; it must use a lock that does not conflict with that, or every
// lease on a capped queue or farm waits for the whole of a large submission
// into it.
func TestPGRace_SubmissionDoesNotBlockLeasesOnItsQueue(t *testing.T) {
	st, p := pgRace(t)
	w := seedWorld(t, st, 1, "w1")
	ctx := t.Context()
	q, err := st.GetQueue(ctx, w.QueueID)
	if err != nil {
		t.Fatalf("GetQueue: %v", err)
	}
	q.MaxConcurrentTasks = 5
	if _, err := st.UpdateQueue(ctx, q); err != nil {
		t.Fatalf("UpdateQueue: %v", err)
	}
	f, err := st.GetFarm(ctx, w.FarmID)
	if err != nil {
		t.Fatalf("GetFarm: %v", err)
	}
	f.MaxConcurrentTasks = 5
	if _, err := st.UpdateFarm(ctx, f); err != nil {
		t.Fatalf("UpdateFarm: %v", err)
	}

	subID, subStep := "job-"+uuid.NewString()[:8], "step-"+uuid.NewString()[:8]
	seed := storetest.NewJob(subID, w.FarmID, w.QueueID).Step(subStep, store.StepStatusReady)
	for i := range 10 {
		seed.Task(fmt.Sprintf("%s-t%d", subID, i), subStep, store.TaskStatusReady)
	}

	var lease store.LeaseResult
	p.atStatement(sqlstore.SQLInsertTasksHead)
	serr, lerr, waited := interleave(t, p,
		func() error { _, err := st.CreateJobSubmission(ctx, seed.Sub); return err },
		func() (err error) { lease, err = st.LeaseTask(ctx, leaseReq(w.Tasks[0], "w1")); return err })
	assertPausedAt(t, p, sqlstore.SQLInsertTasksHead)
	if serr != nil || lerr != nil {
		t.Fatalf("submission err %v, lease err %v", serr, lerr)
	}
	if lease.Outcome != store.LeaseLeased {
		t.Fatalf("lease outcome = %s, want leased", lease.Outcome)
	}
	if waited {
		t.Error("a lease on a capped queue and farm waited for a submission into them: " +
			"its queue or farm anchor conflicts with the submission's foreign-key locks")
	}
}
