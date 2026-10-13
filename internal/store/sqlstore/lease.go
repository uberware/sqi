// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/uberware/sqi/internal/store"
)

const (
	// sqlLeaseScope reads what LeaseTask needs before it takes its anchors: the
	// task's job, queue and farm, and the queue's and farm's caps. The inner
	// joins make a task whose job, queue or farm row is missing read as
	// unknown, which is Lost. A task's job never changes, but the job's queue
	// and farm can (UpdateJob moves a job), and so can the caps, so under the
	// anchors the guarded task write (sqlLeaseTaskGuarded) checks the queue and
	// farm again and each anchored cap is re-read with its count
	// (sqlLeaseQueueCapCount, sqlLeaseFarmCapCount).
	sqlLeaseScope = `
SELECT t.job_id, j.queue_id, j.farm_id, q.max_concurrent_tasks, f.max_concurrent_tasks
FROM   tasks  t
JOIN   jobs   j ON t.job_id   = j.id
JOIN   queues q ON j.queue_id = q.id
JOIN   farms  f ON j.farm_id  = f.id
WHERE  t.id = ?`

	// sqlLeaseTaskGuarded moves one task ready → assigned only while it passes
	// sqlLeasableTask, the predicate ListReadyTasks lists by, so a task that
	// stopped being leasable after the scheduler chose it (leased by someone
	// else, still backing off, its job or queue paused, its job terminal) is
	// left alone and the lease is Lost. It is sqlLeaseTaskWrite under that
	// predicate, whose t.status and t.retry_after refer to the row being
	// updated, and under the lease's two re-checks, which run under the
	// anchors and so see the committed rows:
	//
	//   - the job is still in the queue and farm the scope read named. A job
	//     moved since (UpdateJob) is Lost, because the anchors and counts are
	//     for the old rows (I5). The job row is held FOR SHARE, so a move
	//     either committed before the lock and is seen here, or waits for the
	//     lease.
	//   - the worker is not disabled, the one worker state that refuses new
	//     work. The worker row is held FOR SHARE, so a disable that committed
	//     before the lock is seen. An offline worker is not refused: the
	//     heartbeat sweep can mark a live worker offline after one missed
	//     window and heartbeats never restore online, so refusing it would
	//     starve it for good (see "Known gaps" in docs/architecture.md). A
	//     worker with no row is not refused either: the store does not require
	//     registration (the scheduler does, before it leases).
	//
	// Binds: worker, assigned_at, updated_at, task, queue, farm, now, worker.
	sqlLeaseTaskGuarded = sqlLeaseTaskWrite + `
  AND  EXISTS (SELECT 1
               FROM   jobs   j
               JOIN   queues q ON j.queue_id = q.id
               WHERE  j.id = t.job_id
                 AND  j.queue_id = ? AND j.farm_id = ?
                 AND  ` + sqlLeasableTask + `)
  AND  NOT EXISTS (SELECT 1 FROM workers w WHERE w.id = ? AND w.disabled)`

	// sqlLeaseQueueCapCount and sqlLeaseFarmCapCount read an anchored queue's
	// or farm's cap together with its active-task count (the policy gate's own
	// count statement), so both come from under the row's anchor in one
	// statement. Binds: the row's id, twice.
	sqlLeaseQueueCapCount = `SELECT max_concurrent_tasks, (` + sqlCountActiveTasksInQueue + `) FROM queues WHERE id = ?`
	sqlLeaseFarmCapCount  = `SELECT max_concurrent_tasks, (` + sqlCountActiveTasksInFarm + `) FROM farms WHERE id = ?`

	// sqlNextAttemptNumber numbers the lease's attempt in SQL, inside the
	// lease's transaction: 1 for a fresh task, one past the highest otherwise.
	sqlNextAttemptNumber = `
SELECT COALESCE(MAX(attempt_number), 0) + 1 FROM task_attempts WHERE task_id = ?`
)

// leaseScope is the task's job, queue and farm, with the caps read for the
// queue and the farm. A cap of 0 means unlimited.
type leaseScope struct {
	jobID, queueID, farmID string
	queueCap, farmCap      int
}

// LeaseTask implements [store.TaskStore].
func (s *Store) LeaseTask(ctx context.Context, req store.LeaseRequest) (store.LeaseResult, error) {
	return inTxValue(ctx, s, "lease task", func(tx *dbTx) (store.LeaseResult, txOutcome, error) {
		res, err := leaseTx(ctx, tx, req)
		if err != nil || res.Outcome != store.LeaseLeased {
			// A non-leased outcome writes nothing, even when the task update or
			// a claim insert had already run.
			return res, txRollback, err
		}
		return res, txCommit, nil
	})
}

// leaseTx runs the lease's statements inside tx. The caller commits only a
// Leased result.
//
// Statement order. The scope read comes first, only because it names the
// anchor rows and says which of the queue and the farm are capped. The anchors
// follow, in the global lock order (worker, job, queue, farm, pools): the
// requesting worker's row and the task's job row FOR SHARE, then the queue row
// and the farm row, each only when it has a cap, then every requested pool row
// in pool-ID order, all three FOR NO KEY UPDATE. Two leases therefore never
// wait on each other for a worker or a job row, and take the capacity rows
// they share in the same order, so they cannot deadlock on Postgres.
//
// Under those locks the guarded task write (sqlLeaseTaskGuarded) re-checks
// that the worker is not disabled and that the job is still in the queue and
// farm the scope read named, and fullTx re-reads each anchored cap with its
// count, so the lease decides on committed values, not the ones read before
// the locks. An offline worker is not refused (see sqlLeaseTaskGuarded).
//
// Because the lease holds the worker and job rows FOR SHARE, every operation
// that holds either row FOR UPDATE and the lease wait for each other. On
// Postgres that makes:
//
//   - a cancel's reported set exact. CancelJobExecution and
//     CancelTaskExecution hold the job row from the read of the tasks they
//     report to the UPDATE that cancels them, so no lease commits in between,
//     and a lease that waited finds the job or task no longer leasable;
//   - an offline transition complete. Taking a worker offline (the heartbeat
//     sweep, a deregister, a restart's reclaim) holds the worker row from its
//     marking UPDATE to commit, so a lease in flight is waited for and its task
//     is reclaimed. A lease that waited for the transition then proceeds:
//     served if the worker is alive, recovered by the stale-assignment reaper
//     if not;
//   - the latest-attempt checks sound. CompleteTaskAttempt and the other
//     job-anchored attempt writers hold the job row from the check to the task
//     write, so no lease adds a newer attempt between the two;
//   - demotion safe. DemoteStalledJobs holds the job row while it re-checks
//     that no task of the job is in flight: it either waits for a lease in
//     flight and sees its task, so the job stays running, or the lease waits
//     and then lands on the pending job, as a lease on any job with only
//     waiting tasks does.
//
// The cost: completions and other job-row writers serialize briefly with
// leases on the same job, and a write to a worker row, such as a heartbeat,
// waits briefly for that worker's lease in flight.
//
// The task's guarded UPDATE runs before any count: it takes the task's row
// lock, and every count after it includes the task itself. The queue, farm and
// pool counts each run under their anchor, so no other lease can change them
// between this count and this write (I5). A queue or farm read as uncapped is
// not anchored (see fullTx). The attempt is inserted before its claims, which
// reference it. On SQLite the locks do nothing, since the single write
// connection serializes the whole transaction, but the worker re-check still
// refuses a worker disabled after the scheduler read it.
func leaseTx(ctx context.Context, tx *dbTx, req store.LeaseRequest) (store.LeaseResult, error) {
	lost := store.LeaseResult{Outcome: store.LeaseLost}
	scope, found, err := readLeaseScopeTx(ctx, tx, req.TaskID)
	if err != nil {
		return store.LeaseResult{}, err
	}
	if !found {
		return lost, nil
	}
	claims := slices.Clone(req.Claims)
	slices.SortStableFunc(claims, func(a, b store.UsagePoolClaim) int { return cmp.Compare(a.PoolID, b.PoolID) })
	if err := tx.lockAnchors(ctx, scope.anchors(req.WorkerID, claims)...); err != nil {
		return store.LeaseResult{}, err
	}

	nowText := TimeText(req.Now)
	moved, err := scope.leaseTaskRowTx(ctx, tx, req, nowText)
	if err != nil {
		return store.LeaseResult{}, err
	}
	if !moved {
		return lost, nil
	}
	if full, err := scope.fullTx(ctx, tx); err != nil || full != "" {
		return store.LeaseResult{Outcome: full}, err
	}
	attempt, err := insertLeaseAttemptTx(ctx, tx, req, nowText)
	if err != nil {
		return store.LeaseResult{}, err
	}
	for _, c := range claims {
		if res, err := claimPoolTx(ctx, tx, c, attempt.ID, nowText); err != nil || res.Outcome != "" {
			return res, err
		}
	}
	return store.LeaseResult{Outcome: store.LeaseLeased, Attempt: attempt}, nil
}

// readLeaseScopeTx reads the task's job, queue and farm and the queue's and
// farm's caps. found is false when the task, or its job, queue or farm row,
// does not exist.
func readLeaseScopeTx(ctx context.Context, tx *dbTx, taskID string) (scope leaseScope, found bool, err error) {
	err = tx.QueryRowContext(ctx, sqlLeaseScope, taskID).Scan(&scope.jobID, &scope.queueID, &scope.farmID, &scope.queueCap, &scope.farmCap)
	if errors.Is(err, sql.ErrNoRows) {
		return leaseScope{}, false, nil
	}
	if err != nil {
		return leaseScope{}, false, fmt.Errorf("store: read lease scope of task %s: %w", taskID, mapErr(err))
	}
	return scope, true, nil
}

// anchors lists the lease's anchor rows in the global lock order: the worker
// row and the task's job row shared, then the queue and the farm, each only
// when capped, then each pool in the order of claims, which the caller has
// sorted by pool ID. The queue, farm and pool anchors are [LockNoKeyUpdate].
func (sc *leaseScope) anchors(workerID string, claims []store.UsagePoolClaim) []Anchor {
	out := make([]Anchor, 0, 4+len(claims))
	out = append(out, sharedAnchor(workerAnchor(workerID)), sharedAnchor(jobAnchor(sc.jobID)))
	if sc.queueCap > 0 {
		out = append(out, queueAnchor(sc.queueID))
	}
	if sc.farmCap > 0 {
		out = append(out, farmAnchor(sc.farmID))
	}
	for _, c := range claims {
		out = append(out, poolAnchor(c.PoolID))
	}
	return out
}

// leaseTaskRowTx makes the guarded ready → assigned write
// (sqlLeaseTaskGuarded) and reports whether it landed.
func (sc *leaseScope) leaseTaskRowTx(ctx context.Context, tx *dbTx, req store.LeaseRequest, nowText string) (bool, error) {
	res, err := tx.ExecContext(ctx, sqlLeaseTaskGuarded,
		req.WorkerID, nowText, nowText, req.TaskID, sc.queueID, sc.farmID, nowText, req.WorkerID)
	if err != nil {
		return false, fmt.Errorf("store: lease task %s: %w", req.TaskID, mapErr(err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, mapErr(err)
	}
	return n == 1, nil
}

// fullTx re-counts the active tasks of the queue and the farm that the scope
// read found capped, and therefore anchored, with the same count statements
// the scheduler's policy gate uses, re-reading each cap in the same statement.
// It runs after the lease's own task update, so each count includes the task
// being leased: a count above the cap means the task did not fit. It returns
// the outcome of the first full one, queue before farm, or "" when both have
// room.
//
// A row read as uncapped is not anchored and not checked: a cap set on it
// concurrently orders after this lease, as if the lease had committed first,
// and setting a cap never evicts running work. An anchored row whose cap was
// cleared meanwhile re-reads as 0, unlimited.
func (sc *leaseScope) fullTx(ctx context.Context, tx *dbTx) (store.LeaseOutcome, error) {
	checks := []struct {
		capped    bool
		query, id string
		full      store.LeaseOutcome
	}{
		{sc.queueCap > 0, sqlLeaseQueueCapCount, sc.queueID, store.LeaseQueueFull},
		{sc.farmCap > 0, sqlLeaseFarmCapCount, sc.farmID, store.LeaseFarmFull},
	}
	for _, c := range checks {
		if !c.capped {
			continue
		}
		var capacity, active int
		if err := tx.QueryRowContext(ctx, c.query, c.id, c.id).Scan(&capacity, &active); err != nil {
			return "", fmt.Errorf("store: count active tasks for %s check: %w", c.full, mapErr(err))
		}
		if capacity > 0 && active > capacity {
			return c.full, nil
		}
	}
	return "", nil
}

// insertLeaseAttemptTx inserts the lease's running attempt, numbered by
// sqlNextAttemptNumber.
func insertLeaseAttemptTx(ctx context.Context, tx *dbTx, req store.LeaseRequest, nowText string) (store.TaskAttempt, error) {
	var num int
	if err := tx.QueryRowContext(ctx, sqlNextAttemptNumber, req.TaskID).Scan(&num); err != nil {
		return store.TaskAttempt{}, fmt.Errorf("store: number lease attempt of task %s: %w", req.TaskID, mapErr(err))
	}
	// sqlInsertAttempt binds: id, task_id, worker_id, session_id,
	// attempt_number, status, exit_code, started_at, ended_at, created_at,
	// message.
	row := tx.QueryRowContext(ctx, sqlInsertAttempt,
		req.AttemptID, req.TaskID, req.WorkerID, nil, num, string(store.AttemptStatusRunning),
		nil, nowText, nil, nowText, "")
	a, err := scanAttempt(row)
	if err != nil {
		return store.TaskAttempt{}, fmt.Errorf("store: insert lease attempt: %w", mapErr(err))
	}
	return a, nil
}

// claimPoolTx claims one slot of c's pool for attemptID. The pool's cap is
// read here, in the lease's transaction, never taken from the caller's copy,
// and a pool that no longer exists counts as full. It returns a zero result
// when the claim was written and a PoolFull result naming the pool when not.
func claimPoolTx(ctx context.Context, tx *dbTx, c store.UsagePoolClaim, attemptID, nowText string) (store.LeaseResult, error) {
	pool, err := scanPool(tx.QueryRowContext(ctx, sqlGetPool, c.PoolID))
	if errors.Is(err, sql.ErrNoRows) {
		// Deleted after the scheduler decided the task was eligible.
		return store.LeaseResult{Outcome: store.LeasePoolFull, FullPool: c.PoolName}, nil
	}
	if err != nil {
		return store.LeaseResult{}, fmt.Errorf("store: read pool %q for lease: %w", c.PoolName, mapErr(err))
	}
	if pool.MaxConcurrent > 0 {
		var active int
		if err := tx.QueryRowContext(ctx, sqlActiveClaimCount, c.PoolID).Scan(&active); err != nil {
			return store.LeaseResult{}, fmt.Errorf("store: count active claims for pool %q: %w", pool.Name, mapErr(err))
		}
		if active >= pool.MaxConcurrent {
			return store.LeaseResult{Outcome: store.LeasePoolFull, FullPool: pool.Name}, nil
		}
	}
	if _, err := tx.ExecContext(ctx, sqlInsertClaim, c.ClaimID, c.PoolID, attemptID, nowText); err != nil {
		return store.LeaseResult{}, fmt.Errorf("store: insert claim for pool %q: %w", pool.Name, mapErr(err))
	}
	return store.LeaseResult{}, nil
}
