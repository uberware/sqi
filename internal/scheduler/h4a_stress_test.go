// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// The H4a concurrent stress test (spec §8.4). Racers run every operation H4a
// made atomic, concurrently, against one job whose tasks claim capped usage
// pools, on the real SQLite store, and a monitor racing alongside them checks
// the invariants on the snapshots it reads. SQLite serializes writers, so this
// cannot prove the group-1 races closed (spec §3.2); what it catches,
// non-deterministically, is a group-2 regression: an operation whose own
// transaction leaves the database in a state the invariants forbid, or two
// operations whose composition does.
//
// The run is a sequence of rounds. In each round every racer due that round
// makes one call, all of them started together so they race, and the next round
// starts when the last of them returns. How much a run does, and what each
// racer is offered to work on, is therefore a function of the round count and
// not of how fast the host is: a run on a fast host does the same work, faster.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/sqlite"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/ws"
)

const (
	// stressTasks is the number of tasks in the stressed job.
	stressTasks = 24
	// stressSilentEvery makes every stressSilentEvery-th task silent: no
	// reporter ever reports on it (see [stressRun.silent]).
	stressSilentEvery = 6
	// stressMinRounds is how many rounds every run makes. It sets the run's
	// volume, not its duration: about 8 s under -race on a slow Windows host,
	// under a second without -race.
	stressMinRounds = 200
	// stressMaxRounds bounds a run in which some racer has not yet taken effect
	// by stressMinRounds (see [stressRun.missing]). Reaching it fails the test.
	stressMaxRounds = 3000
)

// stressPoolCaps are the caps of the two usage pools. Every lease claims the
// first; leases of odd-numbered tasks claim the second as well, so a lease
// takes one or two pools in one transaction.
var stressPoolCaps = []int{3, 2}

// stressHistoryDDL records every committed change of a task's status, in
// commit order, so the test can replay each task's whole history afterwards.
// It is test-local: the triggers are installed on the test's own database and
// only append to their own table.
var stressHistoryDDL = []string{
	`CREATE TABLE h4a_stress_task_history (
		seq         INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id     TEXT NOT NULL,
		from_status TEXT,
		to_status   TEXT NOT NULL)`,
	`CREATE TRIGGER h4a_stress_task_inserted AFTER INSERT ON tasks BEGIN
		INSERT INTO h4a_stress_task_history (task_id, from_status, to_status) VALUES (NEW.id, NULL, NEW.status);
	END`,
	`CREATE TRIGGER h4a_stress_task_moved AFTER UPDATE OF status ON tasks
	WHEN OLD.status IS NOT NEW.status BEGIN
		INSERT INTO h4a_stress_task_history (task_id, from_status, to_status) VALUES (NEW.id, OLD.status, NEW.status);
	END`,
}

// stressRetryArrows are the task arrows the history may contain beyond
// [store.ValidateTaskTransition]'s table. The table is the arrow set of
// UpdateTaskStatus, which never revives a terminal task ("terminal states have
// no outgoing transitions"). RetryTasks is the one bulk path that does, by
// design: it moves failed and canceled tasks back to pending
// ([store.TaskStore.RetryTasks]). Every other bulk path (reclaim, offline
// reclaim, requeue, cancel, step release) writes an arrow the table lists.
var stressRetryArrows = map[[2]store.TaskStatus]bool{
	{store.TaskStatusFailed, store.TaskStatusPending}:   true,
	{store.TaskStatusCanceled, store.TaskStatusPending}: true,
}

// stressFixture is the seeded world the stress test races over.
type stressFixture struct {
	jobID   string
	stepID  string
	farmID  string
	taskIDs []string
	pools   []store.UsagePool
	workers []string
}

// stressCounts tallies what each racer achieved, so the test can show it
// actually raced rather than passing on an idle database.
type stressCounts struct {
	leased, leaseLost, leasePoolFull, leaseOther atomic.Int64
	reportsRejected                              atomic.Int64
	taskCancels, jobCancels                      atomic.Int64
	reaped, offlined, offlineReclaimed           atomic.Int64
	retries, revived                             atomic.Int64
	snapshots                                    atomic.Int64

	// reports counts applied reports by status; its keys are fixed up front,
	// so concurrent readers of the map itself need no lock.
	reports map[string]*atomic.Int64
}

// stressRacer is one entry of the round schedule: fn runs, with the round
// number, in every round where round%every == offset.
type stressRacer struct {
	every, offset int
	fn            func(round int)
}

// stressRun is one run of the stress test.
type stressRun struct {
	t  *testing.T
	st *sqlite.Store
	db *sql.DB // a second connection to the same file: history and diagnostics
	s  *Scheduler
	fx stressFixture
	// reportedBy[g] is reporter g's scheduler. Two schedulers share the store as
	// two server processes on one database would (handlers inside one server
	// share one *Scheduler): reporter 0's lets every failure go terminal and
	// reporter 1's requeues a task until its third failure, so both arms of the
	// failure fork race (a cancel and the retry after it reset a task's failure
	// count too often for one policy to reach both).
	reportedBy [2]*Scheduler
	counts     stressCounts

	// failed is set by the first failure, which alone is reported (what fails
	// after it is usually its consequence); the run stops after that round.
	failed atomic.Bool
}

// TestH4a_ConcurrentStress_SQLite races leases, CancelJob, CancelTask, the
// reaper, OfflineStaleWorker, RetryTasks (through RetryJob) and worker reports
// against one job with capped usage pools, on the real SQLite store. While they
// run, a monitor racing alongside asserts on every snapshot it reads that
// invariant I3 holds, no pool is over its cap, no task has two open attempts
// and no task out of flight has an open one; afterwards the test asserts the
// same again and that every task's whole status history is made of legal
// arrows, and fails a run in which any racer never took effect. It cannot
// prove group 1 (spec §3.2) because SQLite serializes writes; it catches
// group-2 regressions non-deterministically. Runs in make test, with or
// without -race.
func TestH4a_ConcurrentStress_SQLite(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test: races every H4a store operation on SQLite")
	}
	path := t.TempDir() + "/test.db"
	st, err := sqlite.Open(t.Context(), path, sqlite.DefaultOptions())
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close sqlite store: %v", err)
		}
	})
	r := &stressRun{
		t:  t,
		st: st,
		db: openStressHistory(t, path),
		s:  newStressScheduler(t, st, 1),
		fx: seedStressJob(t, st, stressTasks),
	}
	r.reportedBy = [2]*Scheduler{r.s, newStressScheduler(t, st, 3)}
	r.counts.reports = map[string]*atomic.Int64{}
	for _, s := range []string{"running", "succeeded", "failed", "canceled"} {
		r.counts.reports[s] = &atomic.Int64{}
	}

	start := time.Now()
	rounds := r.run()
	r.logCounts(rounds, time.Since(start))
	if t.Failed() {
		return
	}
	r.checkSnapshot("after the run")
	assertAtMostOneOpenAttempt(t, st, r.fx.taskIDs)
	r.assertLegalHistory()
}

// ── the round schedule ───────────────────────────────────────────────────────

// schedule is who races in which rounds. Leases, reports and the monitor run
// every round; the rest run often enough to give every other racer work and
// rarely enough that work stays in flight (a job cancel cancels every task).
//
// CancelJob runs in an even round, where no retry runs, and never in round 0.
// Once it commits nothing in its round can revive a task (only a retry does), so
// every task is terminal when the next round's retry finalizes the step, and
// that retry returns them all to ready (see [stressRun.retry]). In the same
// round as a retry, the retry could revive the tasks before the cancel and
// leave them pending in a step the cancel never finalizes, run after run.
func (r *stressRun) schedule() []stressRacer {
	return []stressRacer{
		{1, 0, func(n int) { r.leaseOne(0, n) }},
		{1, 0, func(n int) { r.leaseOne(1, n) }},
		{1, 0, func(n int) { r.reportOne(0, n) }},
		{1, 0, func(n int) { r.reportOne(1, n) }},
		{1, 0, func(int) { r.checkSnapshot("during the run") }},
		{3, 0, r.cancelTask},
		{30, 28, r.cancelJob},
		{3, 1, r.reap},
		{2, 0, r.offline},
		{2, 1, r.retry},
	}
}

// run makes the rounds and returns how many it made. It stops after
// stressMinRounds once every racer has taken effect, and fails the test if one
// still has not by stressMaxRounds.
func (r *stressRun) run() int {
	racers := r.schedule()
	for round := range stressMaxRounds {
		var wg sync.WaitGroup
		for _, rc := range racers {
			if round%rc.every == rc.offset {
				wg.Go(func() { rc.fn(round) })
			}
		}
		wg.Wait()
		if r.failed.Load() {
			return round + 1
		}
		if round+1 >= stressMinRounds && len(r.missing()) == 0 {
			return round + 1
		}
	}
	r.failf("after %d rounds the stress run had made no %s: it did not race what it claims to",
		stressMaxRounds, strings.Join(r.missing(), ", "))
	return stressMaxRounds
}

// missing names the racers that have not taken effect yet. Each is offered
// work by construction every few rounds, whatever the host's speed, so a run
// reaching stressMaxRounds with one missing means it is broken, not slow:
//   - leases: the job starts with every task ready and both pools empty, and
//     retry returns canceled and failed tasks to ready all through the run;
//   - pool-full refusals: the silent tasks are all odd, so each claims both
//     pools, and they stay assigned until something reclaims them (the reaper
//     runs every third round); two of them fill the second pool;
//   - running and terminal reports: two reporters every round, each looking
//     for a task in flight that is not silent;
//   - reaper reclaims: a silent task's assignment is never reported, so it
//     stays assigned until the reaper (every third round, cutoff "now", so
//     every assignment committed before its call is eligible with no clock
//     aging), an offline sweep of its worker or a cancel takes it;
//   - offline reclaims: every second round one worker, in turn, is swept, and
//     leases go to the workers in turn, silent tasks included;
//   - retry revivals: every third round CancelTask cancels a task in turn, and
//     reporter 0 lets a failure go terminal.
//
// Not listed, because their counts are fixed by the schedule and so prove
// nothing: the CancelTask, CancelJob and RetryJob calls (their effect is the
// revivals above and the arrows in the history), and the offline sweeps
// marked, since each sweep re-arms its worker as stale immediately before.
// Rejected reports are not listed either: they need a racer to move a task
// between the reporter's read and its write, which is a timing, not a schedule.
func (r *stressRun) missing() []string {
	c := &r.counts
	var out []string
	for _, k := range []struct {
		name string
		n    int64
	}{
		{"leases", c.leased.Load()},
		{"pool-full lease refusals", c.leasePoolFull.Load()},
		{"running reports applied", c.reports["running"].Load()},
		{"terminal reports applied", c.reports["succeeded"].Load() + c.reports["failed"].Load() + c.reports["canceled"].Load()},
		{"tasks reclaimed by the reaper", c.reaped.Load()},
		{"tasks reclaimed by an offline sweep", c.offlineReclaimed.Load()},
		{"tasks revived by retry", c.revived.Load()},
	} {
		if k.n == 0 {
			out = append(out, k.name)
		}
	}
	return out
}

// ── racers ───────────────────────────────────────────────────────────────────

// silent reports whether task idx is one no reporter ever reports on, as if
// its worker died without a word: only the reaper, an offline sweep or a
// cancel can take its assignment back.
func silent(idx int) bool { return idx%stressSilentEvery == stressSilentEvery-1 }

// leaseOne is leaser g's lease in round n: it leases a task it last saw ready
// to the fixture's workers in turn, claiming one or two pools. Aiming at a
// ready task makes most leases contend for real work rather than bounce off a
// task in flight; the task may still be gone by the time the lease runs.
func (r *stressRun) leaseOne(g, n int) {
	if idx, ok := r.find((n*5+g*11)%stressTasks, false, store.TaskStatusReady); ok {
		r.lease(idx, r.fx.workers[(n+g)%len(r.fx.workers)])
	}
}

// find returns the first task, scanning from start and passing over the silent
// tasks when skipSilent, whose status is one of want when read. The read is
// unlocked and only a hint.
func (r *stressRun) find(start int, skipSilent bool, want ...store.TaskStatus) (int, bool) {
	for k := range stressTasks {
		idx := (start + k) % stressTasks
		if skipSilent && silent(idx) {
			continue
		}
		task, err := r.st.GetTask(r.t.Context(), r.fx.taskIDs[idx])
		if !r.expect("GetTask", err) {
			return 0, false
		}
		if slices.Contains(want, task.Status) {
			return idx, true
		}
	}
	return 0, false
}

// lease leases task idx to workerID. Every lease outcome is legitimate under
// contention and is counted; only an error fails the test.
func (r *stressRun) lease(idx int, workerID string) {
	claims := []store.UsagePoolClaim{{ClaimID: uuid.NewString(), PoolID: r.fx.pools[0].ID, PoolName: r.fx.pools[0].Name}}
	if idx%2 == 1 {
		claims = append(claims, store.UsagePoolClaim{ClaimID: uuid.NewString(), PoolID: r.fx.pools[1].ID, PoolName: r.fx.pools[1].Name})
	}
	res, err := r.st.LeaseTask(r.t.Context(), store.LeaseRequest{
		TaskID: r.fx.taskIDs[idx], WorkerID: workerID, AttemptID: uuid.NewString(), Now: time.Now().UTC(), Claims: claims,
	})
	if !r.expect("LeaseTask", err) {
		return
	}
	switch res.Outcome {
	case store.LeaseLeased:
		r.counts.leased.Add(1)
	case store.LeaseLost:
		r.counts.leaseLost.Add(1)
	case store.LeasePoolFull:
		r.counts.leasePoolFull.Add(1)
	default:
		r.counts.leaseOther.Add(1)
	}
}

// reportOne is reporter g's report in round n. The reporters play the workers:
// for a task in flight that is not silent they report on the task's latest
// attempt, "running" for half the assigned ones and otherwise a terminal
// status, through the scheduler's own report handler (so a failure runs the
// auto-retry fork and a terminal report finalizes the step and job).
func (r *stressRun) reportOne(g, n int) {
	if idx, ok := r.find((n*7+g*13)%stressTasks, true, store.TaskStatusAssigned, store.TaskStatusRunning); ok {
		r.report(g, idx, n)
	}
}

// report has reporter g report on task idx's latest attempt; i picks the status
// (see [stressReport]). Nothing serializes it against a lease or another
// report, as nothing does between real workers: between the read of the latest
// attempt and the report, the reaper or an offline sweep may close that attempt
// and a lease may replace it, so the report arrives late from a superseded
// attempt, as an old worker's would. A terminal report from it is refused by
// the store (CompleteTaskAttempt checks that the attempt is the task's latest),
// and a failure report whose attempt was already closed is discarded by the
// failure fork (failureReportStillCurrent). One pre-existing window stays open:
// a failure report that closes the attempt itself (RecordTaskFailure), then a
// reclaim and a new lease, then the report's RequeueTaskForRetry, which is
// guarded on the task's status and not on its attempt, so it returns the new
// lease to ready with its attempt open. The snapshot checks catch that shape
// if a run hits it; closing it is H4b's work, not this test's.
func (r *stressRun) report(g, idx, i int) {
	ctx := r.t.Context()
	task, err := r.st.GetTask(ctx, r.fx.taskIDs[idx])
	if !r.expect("GetTask", err) {
		return
	}
	if task.Status != store.TaskStatusAssigned && task.Status != store.TaskStatusRunning {
		return
	}
	// A task in flight always has an attempt, so ErrNotFound is a failure here.
	attempt, err := r.st.LatestTaskAttempt(ctx, task.ID)
	if !r.expect("LatestTaskAttempt", err) || attempt.Status != store.AttemptStatusRunning {
		return
	}
	m := stressReport(task, attempt, i)
	err = r.reportedBy[g].processTaskStatus(ctx, attempt.WorkerID, m)
	if errors.Is(err, store.ErrInvalidTransition) {
		// The task moved on first (reclaimed, canceled, or already terminal):
		// the scheduler refuses the report and the consumer would ack it.
		r.counts.reportsRejected.Add(1)
		return
	}
	if r.expect("processTaskStatus "+m.Status, err) {
		r.counts.reports[m.Status].Add(1)
	}
}

// stressReport builds report number i on attempt: "running" for every other
// assigned task, otherwise succeeded, failed, failed or canceled in turn
// (failures weighted up so the auto-retry fork runs, and succeeded kept rare
// because it is the one status nothing revives).
func stressReport(task store.Task, attempt store.TaskAttempt, i int) protocol.TaskStatusMsg {
	m := protocol.TaskStatusMsg{
		Version: protocol.ProtocolVersion, Type: protocol.TypeTaskStatus,
		TaskID: task.ID, AttemptID: attempt.ID, JobID: task.JobID, At: time.Now().UTC(),
	}
	if task.Status == store.TaskStatusAssigned && i%2 == 0 {
		m.Status = "running"
		return m
	}
	m.Status = []string{"succeeded", "failed", "failed", "canceled"}[(i/2)%4]
	switch m.Status {
	case "succeeded":
		zero := 0
		m.ExitCode = &zero
	case "failed":
		one := 1
		m.ExitCode = &one
		m.Message = "stress failure"
	}
	return m
}

func (r *stressRun) cancelTask(n int) {
	r.counts.taskCancels.Add(1)
	r.expect("CancelTask", r.s.CancelTask(r.t.Context(), r.fx.taskIDs[(n*3)%stressTasks]))
}

func (r *stressRun) cancelJob(int) {
	r.counts.jobCancels.Add(1)
	r.expect("CancelJob", r.s.CancelJob(r.t.Context(), r.fx.jobID))
}

// reap runs the reaper with a cutoff of now, so every assignment committed
// before the call is stale: what it is offered depends on the schedule, not on
// how long an assignment has been waiting by the clock.
func (r *stressRun) reap(int) {
	reclaimed, err := r.st.ReclaimStaleAssignedTasks(r.t.Context(), time.Now().UTC())
	if r.expect("ReclaimStaleAssignedTasks", err) {
		r.counts.reaped.Add(int64(len(reclaimed)))
	}
}

// offline re-arms one worker as online with a heartbeat a minute old, then
// sweeps it, so the offline reclaim races leases and reports on that worker.
func (r *stressRun) offline(n int) {
	ctx := r.t.Context()
	id := r.fx.workers[n%len(r.fx.workers)]
	old := time.Now().UTC().Add(-time.Minute)
	if _, err := r.st.RegisterWorker(ctx, store.Worker{
		ID: id, FarmID: r.fx.farmID, Hostname: id, Status: store.WorkerStatusOnline, LastHeartbeatAt: &old,
	}); !r.expect("RegisterWorker", err) {
		return
	}
	now := time.Now().UTC()
	reclaimed, marked, err := r.st.OfflineStaleWorker(ctx, id, now, now)
	if !r.expect("OfflineStaleWorker", err) {
		return
	}
	if marked {
		r.counts.offlined.Add(1)
	}
	r.counts.offlineReclaimed.Add(int64(len(reclaimed)))
}

// retry finalizes the step when every task is terminal, then retries the job
// (RetryJob, which calls the store's RetryTasks). The finalize is a WORKAROUND
// for a pre-existing bug, not something production does at this point: a job
// cancel never finalizes the job's steps, and RetryTasks resets only a failed or
// canceled step, so cancel-then-RetryJob leaves every revived task pending
// forever in a step still marked ready. When nothing was in flight at the
// cancel no worker echo finalizes the step, and ListStuckSteps skips the
// canceled job. Whoever fixes that bug should remove the checkStepCompletion
// call here.
func (r *stressRun) retry(int) {
	ctx := r.t.Context()
	if !r.expect("checkStepCompletion", r.s.checkStepCompletion(ctx, r.fx.stepID, r.fx.jobID)) {
		return
	}
	n, err := r.s.RetryJob(ctx, r.fx.jobID)
	if r.expect("RetryJob", err) {
		r.counts.retries.Add(1)
		r.counts.revived.Add(int64(n))
	}
}

// ── control ──────────────────────────────────────────────────────────────────

// expect reports whether err is nil. Any error is unexpected: every outcome a
// race can legitimately produce is a typed result, not an error (a lease that
// loses is LeaseLost, a cancel that loses to completion returns nil, a sweep
// that finds a fresh heartbeat returns marked=false), and the one typed error
// a racer tolerates, a refused report, is handled by its caller. It fails the
// test, and the run stops after the current round, otherwise.
func (r *stressRun) expect(op string, err error) bool {
	if err == nil {
		return true
	}
	r.failf("%s: unexpected error under contention: %v", op, err)
	return false
}

// ── invariants ───────────────────────────────────────────────────────────────

// checkSnapshot asserts the invariants on the committed state it reads: I3, no
// pool over its cap, no task with two open attempts, and no open attempt on a
// task that is not in flight. Each check is one statement, so it sees one
// consistent snapshot, and every committed state must satisfy them because each
// H4a operation keeps them inside its transaction.
//
// The last check is the other half of "a task is held by at most one attempt":
// an operation that returns a task to ready, or ends it, without closing its
// attempt leaves an attempt open on a task nothing holds. I3 cannot see that
// (the attempt is open, so its claims are legitimately active), and the
// two-attempts check sees it only if the task is leased again while the stale
// attempt still holds its pool slots, which those very slots usually prevent.
func (r *stressRun) checkSnapshot(when string) {
	if r.failed.Load() {
		return
	}
	r.counts.snapshots.Add(1)
	ctx := r.t.Context()
	v, err := r.st.ClaimInvariantViolations(ctx)
	if !r.expect("ClaimInvariantViolations", err) {
		return
	}
	if len(v) != 0 {
		r.failf("%s: invariant I3 violated (an active claim on a closed attempt or a terminal task):\n%s", when, r.describeClaims(v))
		return
	}
	for i, pool := range r.fx.pools {
		n, err := r.st.ActiveClaimCount(ctx, pool.ID)
		if !r.expect("ActiveClaimCount", err) {
			return
		}
		if n > stressPoolCaps[i] {
			r.failf("%s: pool %s has %d active claims, cap %d", when, pool.Name, n, stressPoolCaps[i])
			return
		}
	}
	held, err := r.taskIDs(ctx, sqlStressTasksHeldTwice)
	if !r.expect("held-twice query", err) {
		return
	}
	if len(held) != 0 {
		r.failf("%s: tasks held twice (two running attempts): %v", when, held)
		return
	}
	stray, err := r.taskIDs(ctx, sqlStressOpenAttemptOffFlight)
	if !r.expect("stray-attempt query", err) {
		return
	}
	if len(stray) != 0 {
		r.failf("%s: tasks neither assigned nor running that still have a running attempt: %v", when, stray)
	}
}

// failf fails the test with the run's first failure; the run stops after the
// current round.
func (r *stressRun) failf(format string, args ...any) {
	if r.failed.CompareAndSwap(false, true) {
		r.t.Errorf(format, args...)
	}
}

const (
	// sqlStressTasksHeldTwice lists the tasks with more than one running attempt.
	sqlStressTasksHeldTwice = `SELECT task_id FROM task_attempts WHERE status = 'running'
		GROUP BY task_id HAVING COUNT(*) > 1 ORDER BY task_id`
	// sqlStressOpenAttemptOffFlight lists the tasks that are neither assigned nor
	// running but still have a running attempt.
	sqlStressOpenAttemptOffFlight = `SELECT DISTINCT a.task_id FROM task_attempts a JOIN tasks t ON t.id = a.task_id
		WHERE a.status = 'running' AND t.status NOT IN ('assigned', 'running') ORDER BY a.task_id`
)

// taskIDs runs query, which selects one task ID per row, on the history
// connection.
func (r *stressRun) taskIDs(ctx context.Context, query string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// describeClaims renders each violating claim with its attempt, its task, the
// task's attempts and the task's status history, which is what deciding the
// shape of a violation needs.
func (r *stressRun) describeClaims(ids []string) string {
	ctx := context.WithoutCancel(r.t.Context())
	hist, err := r.history(ctx)
	if err != nil {
		return fmt.Sprintf("  (task history unreadable: %v) claims %v", err, ids)
	}
	var b strings.Builder
	for _, id := range ids {
		var poolID, attemptID, attemptStatus, taskID, taskStatus sql.NullString
		err := r.db.QueryRowContext(ctx, `SELECT c.pool_id, c.task_attempt_id, a.status, a.task_id, t.status
			FROM usage_claims c LEFT JOIN task_attempts a ON a.id = c.task_attempt_id LEFT JOIN tasks t ON t.id = a.task_id
			WHERE c.id = ?`, id).Scan(&poolID, &attemptID, &attemptStatus, &taskID, &taskStatus)
		if err != nil {
			fmt.Fprintf(&b, "  claim %s: %v\n", id, err)
			continue
		}
		fmt.Fprintf(&b, "  claim %s pool %s: attempt %s (%s) of task %s (%s)\n",
			id, poolID.String, attemptID.String, attemptStatus.String, taskID.String, taskStatus.String)
		if !taskID.Valid {
			continue
		}
		attempts, err := r.st.ListTaskAttempts(ctx, taskID.String)
		if err != nil {
			fmt.Fprintf(&b, "    attempts: %v\n", err)
		}
		for _, a := range attempts {
			fmt.Fprintf(&b, "    attempt #%d %s on %s: %s\n", a.AttemptNumber, a.ID, a.WorkerID, a.Status)
		}
		fmt.Fprintf(&b, "    history: %s\n", strings.Join(hist[taskID.String], " "))
	}
	return b.String()
}

// history returns every task's status history as "from>to" steps in commit
// order, the first step of each being the seeding insert (">ready").
func (r *stressRun) history(ctx context.Context) (map[string][]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT task_id, COALESCE(from_status, ''), to_status FROM h4a_stress_task_history ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var id, from, to string
		if err := rows.Scan(&id, &from, &to); err != nil {
			return nil, err
		}
		out[id] = append(out[id], from+">"+to)
	}
	return out, rows.Err()
}

// assertLegalHistory replays each task's committed history (see
// [historyProblem]) and checks it ends in the task's current status. It
// reports the first problem of each task, with that task's whole history.
func (r *stressRun) assertLegalHistory() {
	t := r.t
	hist, err := r.history(t.Context())
	if err != nil {
		t.Fatalf("read task history: %v", err)
	}
	used := map[string]int{}
	for _, id := range r.fx.taskIDs {
		steps := hist[id]
		if problem := historyProblem(steps); problem != "" {
			t.Errorf("task %s: %s; history: %s", id, problem, strings.Join(steps, " "))
			continue
		}
		for _, s := range steps[1:] {
			used[s]++
		}
		_, last, _ := strings.Cut(steps[len(steps)-1], ">")
		if task := mustTaskOf(t, r.st, id); string(task.Status) != last {
			t.Errorf("task %s is %q but its history ends in %q: the history missed a write", id, task.Status, last)
		}
	}
	arrows := make([]string, 0, len(used))
	for a, n := range used {
		arrows = append(arrows, fmt.Sprintf("%s=%d", a, n))
	}
	sort.Strings(arrows)
	t.Logf("arrows exercised: %s", strings.Join(arrows, " "))
}

// historyProblem returns the first thing wrong with one task's history, or ""
// when it is legal: it must start with the seeding insert of a ready task,
// chain (each step leaves the status the previous step entered), and use only
// arrows the task table or [stressRetryArrows] allows.
func historyProblem(steps []string) string {
	if len(steps) == 0 || steps[0] != ">"+string(store.TaskStatusReady) {
		return "history does not start with the seeded insert >ready"
	}
	cur := string(store.TaskStatusReady)
	for i, s := range steps[1:] {
		from, to, _ := strings.Cut(s, ">")
		if from != cur {
			return fmt.Sprintf("step %d %q does not leave %q, the status the previous step entered", i+1, s, cur)
		}
		if !legalStressArrow(store.TaskStatus(from), store.TaskStatus(to)) {
			return fmt.Sprintf("step %d %q is not a legal arrow", i+1, s)
		}
		cur = to
	}
	return ""
}

func legalStressArrow(from, to store.TaskStatus) bool {
	return store.ValidateTaskTransition(from, to) == nil || stressRetryArrows[[2]store.TaskStatus{from, to}]
}

func (r *stressRun) logCounts(rounds int, elapsed time.Duration) {
	c := &r.counts
	r.t.Logf("stress run: %d rounds in %v: leases leased=%d lost=%d pool_full=%d other=%d; "+
		"reports running=%d succeeded=%d failed=%d canceled=%d rejected=%d; "+
		"cancels task=%d job=%d; reaped=%d; offline marked=%d reclaimed=%d; retries=%d revived=%d; snapshots=%d",
		rounds, elapsed.Round(time.Millisecond), c.leased.Load(), c.leaseLost.Load(), c.leasePoolFull.Load(), c.leaseOther.Load(),
		c.reports["running"].Load(), c.reports["succeeded"].Load(), c.reports["failed"].Load(), c.reports["canceled"].Load(),
		c.reportsRejected.Load(), c.taskCancels.Load(), c.jobCancels.Load(), c.reaped.Load(),
		c.offlined.Load(), c.offlineReclaimed.Load(), c.retries.Load(), c.revived.Load(), c.snapshots.Load())
}

// ── fixtures ─────────────────────────────────────────────────────────────────

// newStressScheduler builds a scheduler that lets a task fail maxAttempts
// times before its failure goes terminal, and requeues the earlier failures
// with no backoff, so the task is leasable again at once.
func newStressScheduler(t *testing.T, st store.Store, maxAttempts int) *Scheduler {
	t.Helper()
	cfg := DefaultConfig()
	cfg.RetryDelay = 0
	cfg.DefaultMaxAttempts = maxAttempts
	s := New(cfg, st, &recordBus{}, metrics.New(), slog.New(slog.DiscardHandler), ws.NoopNotifier{}, nil)
	s.ctx = t.Context()
	return s
}

// openStressHistory opens a second connection to the database at path and
// installs the task-history triggers. It must run before the fixture is
// seeded, so every task's history starts with its insert.
func openStressHistory(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatalf("open history connection: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close history connection: %v", err)
		}
	})
	for _, ddl := range stressHistoryDDL {
		if _, err := db.ExecContext(t.Context(), ddl); err != nil {
			t.Fatalf("install task history: %v", err)
		}
	}
	return db
}

// seedStressJob seeds one farm, queue, job and step with n ready tasks, the
// usage pools and four online workers.
func seedStressJob(t *testing.T, st *sqlite.Store, n int) stressFixture {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC()
	farm, err := st.CreateFarm(ctx, store.Farm{ID: uuid.NewString(), Name: "stress"})
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queue, err := st.CreateQueue(ctx, store.Queue{ID: uuid.NewString(), FarmID: farm.ID, Name: "stress"})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	fx := stressFixture{farmID: farm.ID}
	for i, limit := range stressPoolCaps {
		pool, err := st.CreateUsagePool(ctx, store.UsagePool{ID: uuid.NewString(), Name: fmt.Sprintf("lic-%d", i), MaxConcurrent: limit})
		if err != nil {
			t.Fatalf("CreateUsagePool: %v", err)
		}
		fx.pools = append(fx.pools, pool)
	}
	job, err := st.CreateJob(ctx, store.Job{
		ID: uuid.NewString(), FarmID: farm.ID, QueueID: queue.ID, Name: "stress",
		Status: store.JobStatusPending, TemplateFormat: store.TemplateFormatJSON, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	step, err := st.CreateStep(ctx, store.Step{
		ID: uuid.NewString(), JobID: job.ID, Name: "s", Status: store.StepStatusReady, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateStep: %v", err)
	}
	fx.jobID, fx.stepID = job.ID, step.ID
	for i := range n {
		task, err := st.CreateTask(ctx, store.Task{
			ID: uuid.NewString(), JobID: job.ID, StepID: step.ID, Name: fmt.Sprintf("t%d", i),
			Status: store.TaskStatusReady, CreatedAt: now.Add(time.Duration(i) * time.Microsecond), UpdatedAt: now,
		})
		if err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		fx.taskIDs = append(fx.taskIDs, task.ID)
	}
	for i := range 4 {
		id := fmt.Sprintf("stress-w%d", i)
		if _, err := st.RegisterWorker(ctx, store.Worker{
			ID: id, FarmID: farm.ID, Hostname: id, Status: store.WorkerStatusOnline, LastHeartbeatAt: &now,
		}); err != nil {
			t.Fatalf("RegisterWorker: %v", err)
		}
		fx.workers = append(fx.workers, id)
	}
	return fx
}

// assertAtMostOneOpenAttempt fails if any of taskIDs has more than one running
// attempt: a task must never be held twice.
func assertAtMostOneOpenAttempt(t *testing.T, st *sqlite.Store, taskIDs []string) {
	t.Helper()
	for _, id := range taskIDs {
		attempts, err := st.ListTaskAttempts(t.Context(), id)
		if err != nil {
			t.Fatalf("ListTaskAttempts: %v", err)
		}
		open := 0
		for _, a := range attempts {
			if a.Status == store.AttemptStatusRunning {
				open++
			}
		}
		if open > 1 {
			t.Fatalf("task %s has %d running attempts; a task must never be held twice", id, open)
		}
	}
}
