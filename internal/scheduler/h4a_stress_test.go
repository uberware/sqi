// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// The H4a concurrent stress test (spec §8.4). Goroutines race every operation
// H4a made atomic against one job whose tasks claim capped usage pools, on the
// real SQLite store, and a monitor checks the invariants on every snapshot it
// reads while they run. SQLite serializes writers, so this cannot prove the
// group-1 races closed (spec §3.2); what it catches, non-deterministically, is a
// group-2 regression: an operation whose own transaction leaves the database in
// a state the invariants forbid, or two operations whose composition does.

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
	// stressIterations is how many leases each leaser, and how many reports
	// each reporter, attempts. The other racers run until the last of those
	// finishes. Sized for under ten seconds under -race on a slow Windows host,
	// well inside the package's time budget.
	stressIterations = 400
	// stressReapAfter is the reaper's assignment timeout.
	stressReapAfter = 20 * time.Millisecond
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

// stressRun is one run of the stress test.
type stressRun struct {
	t  *testing.T
	st *sqlite.Store
	db *sql.DB // a second connection to the same file: history and diagnostics
	s  *Scheduler
	fx stressFixture
	// reportedBy[g] is reporter g's scheduler. Two schedulers share the store as
	// two server handlers would: reporter 0's lets every failure go terminal and
	// reporter 1's requeues a task until its third failure, so both arms of the
	// failure fork race (a cancel and the retry after it reset a task's failure
	// count too often for one policy to reach both).
	reportedBy [2]*Scheduler
	counts     stressCounts

	// locks[i] serializes a lease of task i with a report on task i. Leases,
	// reports and nothing else take it; every other racer runs unlocked. The
	// reporter reads the task's latest attempt and reports on it under the
	// lock, so no lease can supersede that attempt in between. That interleaving
	// (a worker's late report for an attempt the reaper closed and a new lease
	// replaced completes the task while the new attempt holds its claims) is a
	// known pre-existing shape the store does not prevent; it is outside H4a and
	// would turn this test into a test of it.
	locks []sync.Mutex

	stop     chan struct{}
	stopOnce sync.Once
	// failed is set by the first failure, which alone is reported: what fails
	// after it is usually its consequence.
	failed atomic.Bool
}

// TestH4a_ConcurrentStress_SQLite races leases, CancelJob, CancelTask, the
// reaper, OfflineStaleWorker, RetryTasks (through RetryJob) and worker reports
// against one job with capped usage pools, on the real SQLite store. While they
// run, a monitor asserts on every snapshot it reads that invariant I3 holds, no
// pool is over its cap and no task has two open attempts; afterwards the test
// asserts the same again and that every task's whole status history is made of
// legal arrows. It cannot prove group 1 (spec §3.2) because SQLite serializes
// writes; it catches group-2 regressions non-deterministically. Runs in make
// test.
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
		t:     t,
		st:    st,
		db:    openStressHistory(t, path),
		s:     newStressScheduler(t, st, 1),
		fx:    seedStressJob(t, st, stressTasks),
		locks: make([]sync.Mutex, stressTasks),
		stop:  make(chan struct{}),
	}
	r.reportedBy = [2]*Scheduler{r.s, newStressScheduler(t, st, 3)}
	r.counts.reports = map[string]*atomic.Int64{}
	for _, s := range []string{"running", "succeeded", "failed", "canceled"} {
		r.counts.reports[s] = &atomic.Int64{}
	}

	start := time.Now()
	var drivers, racers sync.WaitGroup
	for g := range 2 {
		drivers.Go(func() { r.leaser(g) })
	}
	for g := range 2 {
		drivers.Go(func() { r.reporter(g) })
	}
	// The paces keep the racers' writes from starving the leases and reports on
	// the single write connection, and keep a job cancel (which cancels every
	// task) rare enough that work is in flight most of the time.
	racers.Go(func() { r.race(30*time.Millisecond, r.cancelTask) })
	racers.Go(func() { r.race(300*time.Millisecond, r.cancelJob) })
	racers.Go(func() { r.race(10*time.Millisecond, r.reap) })
	racers.Go(func() { r.race(15*time.Millisecond, r.offline) })
	racers.Go(func() { r.race(20*time.Millisecond, r.retry) })
	racers.Go(func() { r.race(2*time.Millisecond, func(int) { r.checkSnapshot("during the run") }) })
	drivers.Wait()
	r.halt()
	racers.Wait()
	elapsed := time.Since(start)

	r.logCounts(elapsed)
	if t.Failed() {
		return
	}
	r.checkSnapshot("after the run")
	assertAtMostOneOpenAttempt(t, st, r.fx.taskIDs)
	r.assertLegalHistory()
	r.assertItRaced()
}

// ── racers ───────────────────────────────────────────────────────────────────

// leaser leases tasks to the fixture's workers in turn, claiming one or two
// pools per lease. It aims each lease at a task it last saw ready, so most
// leases contend for real work rather than bounce off a task in flight; the
// task may still be gone by the time the lease runs. Every lease outcome is
// legitimate under contention; only an error fails the test.
func (r *stressRun) leaser(g int) {
	for i := range stressIterations {
		if r.stopped() {
			return
		}
		idx, ok := r.find((i*5+g*11)%stressTasks, store.TaskStatusReady)
		if !ok {
			continue
		}
		r.lease(idx, r.fx.workers[(i+g)%len(r.fx.workers)])
	}
}

// find returns the first task, scanning from start, whose status is one of
// want when read. The read is unlocked and only a hint.
func (r *stressRun) find(start int, want ...store.TaskStatus) (int, bool) {
	for k := range stressTasks {
		idx := (start + k) % stressTasks
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

func (r *stressRun) lease(idx int, workerID string) {
	r.locks[idx].Lock()
	defer r.locks[idx].Unlock()
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

// reporter plays the workers: for a task in flight it reports on the task's
// latest attempt, "running" for half the assigned ones and otherwise a
// terminal status, through the scheduler's own report handler (so a failure
// runs the auto-retry fork and a terminal report finalizes the step and job).
func (r *stressRun) reporter(g int) {
	for i := range stressIterations {
		if r.stopped() {
			return
		}
		if idx, ok := r.find((i*7+g*13)%stressTasks, store.TaskStatusAssigned, store.TaskStatusRunning); ok {
			r.report(g, idx, i)
		}
	}
}

func (r *stressRun) report(g, idx, i int) {
	r.locks[idx].Lock()
	defer r.locks[idx].Unlock()
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

// race runs op every pace until the drivers finish.
func (r *stressRun) race(pace time.Duration, op func(n int)) {
	for n := 0; !r.stopped(); n++ {
		op(n)
		time.Sleep(pace)
	}
}

func (r *stressRun) cancelTask(n int) {
	r.counts.taskCancels.Add(1)
	r.expect("CancelTask", r.s.CancelTask(r.t.Context(), r.fx.taskIDs[(n*3)%stressTasks]))
}

func (r *stressRun) cancelJob(int) {
	r.counts.jobCancels.Add(1)
	r.expect("CancelJob", r.s.CancelJob(r.t.Context(), r.fx.jobID))
}

// reap runs the reaper with an assignment timeout of stressReapAfter, short
// enough that it reclaims assignments all through the run and long enough that
// the reporter reaches most of them first.
func (r *stressRun) reap(int) {
	reclaimed, err := r.st.ReclaimStaleAssignedTasks(r.t.Context(), time.Now().UTC().Add(-stressReapAfter))
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

// retry finalizes the step if every task is terminal, as a worker's cancel
// echo or the start-up reconcile would, and then retries the job. The step has
// to be terminal first: RetryTasks resets only a failed or canceled step, and
// a revived task in a step still marked ready stays pending forever.
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

func (r *stressRun) halt() { r.stopOnce.Do(func() { close(r.stop) }) }

func (r *stressRun) stopped() bool {
	select {
	case <-r.stop:
		return true
	default:
		return false
	}
}

// expect reports whether err is nil. Any error is unexpected: every outcome a
// race can legitimately produce is a typed result, not an error (a lease that
// loses is LeaseLost, a cancel that loses to completion returns nil, a sweep
// that finds a fresh heartbeat returns marked=false), and the one typed error
// a racer tolerates, a refused report, is handled by its caller. It fails the
// test and stops the run otherwise.
func (r *stressRun) expect(op string, err error) bool {
	if err == nil {
		return true
	}
	r.failf("%s: unexpected error under contention: %v", op, err)
	return false
}

// ── invariants ───────────────────────────────────────────────────────────────

// checkSnapshot asserts the invariants on the committed state it reads: I3, no
// pool over its cap and no task with two open attempts. Each check is one
// statement, so it sees one consistent snapshot, and every committed state must
// satisfy them because each H4a operation keeps them inside its transaction.
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
	held, err := r.tasksWithTwoOpenAttempts(ctx)
	if !r.expect("open-attempt query", err) {
		return
	}
	if len(held) != 0 {
		r.failf("%s: tasks held twice (two running attempts): %v", when, held)
	}
}

// failf fails the test with the run's first failure and stops the run.
func (r *stressRun) failf(format string, args ...any) {
	if r.failed.CompareAndSwap(false, true) {
		r.t.Errorf(format, args...)
	}
	r.halt()
}

func (r *stressRun) tasksWithTwoOpenAttempts(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT task_id FROM task_attempts WHERE status = 'running'
		GROUP BY task_id HAVING COUNT(*) > 1 ORDER BY task_id`)
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

// assertItRaced fails a run in which a racer never took effect: a pass on an
// idle database proves nothing.
func (r *stressRun) assertItRaced() {
	c := &r.counts
	for name, n := range map[string]int64{
		"leases":                    c.leased.Load(),
		"running reports applied":   c.reports["running"].Load(),
		"terminal reports applied":  c.reports["succeeded"].Load() + c.reports["failed"].Load() + c.reports["canceled"].Load(),
		"tasks reclaimed by reaper": c.reaped.Load(),
		"tasks reclaimed offline":   c.offlineReclaimed.Load(),
		"tasks revived by retry":    c.revived.Load(),
	} {
		if n == 0 {
			r.t.Errorf("the stress run made no %s; it did not race what it claims to", name)
		}
	}
}

func (r *stressRun) logCounts(elapsed time.Duration) {
	c := &r.counts
	r.t.Logf("stress run %v: leases leased=%d lost=%d pool_full=%d other=%d; "+
		"reports running=%d succeeded=%d failed=%d canceled=%d rejected=%d; "+
		"cancels task=%d job=%d; reaped=%d; offline marked=%d reclaimed=%d; retries=%d revived=%d; snapshots=%d",
		elapsed.Round(time.Millisecond), c.leased.Load(), c.leaseLost.Load(), c.leasePoolFull.Load(), c.leaseOther.Load(),
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
