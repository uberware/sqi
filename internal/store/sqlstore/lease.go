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
	// unknown, which is Lost. A task's job never changes; the caps can, so the
	// anchored ones are re-read under their locks (sqlLeaseCaps).
	sqlLeaseScope = `
SELECT t.job_id, j.queue_id, j.farm_id, q.max_concurrent_tasks, f.max_concurrent_tasks
FROM   tasks  t
JOIN   jobs   j ON t.job_id   = j.id
JOIN   queues q ON j.queue_id = q.id
JOIN   farms  f ON j.farm_id  = f.id
WHERE  t.id = ?`

	// sqlLeaseWorkerRefuses is true when the worker's row says it cannot take
	// work: offline, or disabled. It runs under the worker row's shared lock, so
	// a sweep or a deregister taking the worker offline is either waited for or
	// seen. A worker with no row is not refused: the store does not require
	// registration (the scheduler does, before it leases).
	sqlLeaseWorkerRefuses = `
SELECT EXISTS (SELECT 1 FROM workers WHERE id = ? AND (status <> 'online' OR disabled))`

	// sqlLeaseCaps re-reads the queue's and the farm's caps under their locks.
	sqlLeaseCaps = `
SELECT q.max_concurrent_tasks, f.max_concurrent_tasks
FROM   queues q, farms f
WHERE  q.id = ? AND f.id = ?`

	// sqlLeaseTaskGuarded moves one task ready → assigned only while it passes
	// sqlLeasableTask, the predicate ListReadyTasks lists by, so a task that
	// stopped being leasable after the scheduler chose it (leased by someone
	// else, still backing off, its job or queue paused, its job terminal) is
	// left alone and the lease is Lost. It is sqlLeaseTaskWrite under that
	// predicate, whose t.status and t.retry_after refer to the row being
	// updated.
	sqlLeaseTaskGuarded = sqlLeaseTaskWrite + `
  AND  EXISTS (SELECT 1
               FROM   jobs   j
               JOIN   queues q ON j.queue_id = q.id
               WHERE  j.id = t.job_id
                 AND  ` + sqlLeasableTask + `)`

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
	var res store.LeaseResult
	err := s.inTx(ctx, "lease task", func(tx *dbTx) (txOutcome, error) {
		var err error
		res, err = leaseTx(ctx, tx, req)
		if err != nil || res.Outcome != store.LeaseLeased {
			// A non-leased outcome writes nothing, even when the task update or
			// a claim insert had already run.
			return txRollback, err
		}
		return txCommit, nil
	})
	if err != nil {
		return store.LeaseResult{}, err
	}
	return res, nil
}

// leaseTx runs the lease's statements inside tx. The caller commits only a
// Leased result.
//
// Statement order. The scope read comes first, only because it names the
// anchor rows and says which of the queue and the farm are capped. The anchors
// follow, in the global lock order (worker, job, queue, farm, pools): the
// requesting worker's row and the task's job row FOR SHARE, then the queue row
// and the farm row FOR UPDATE, each only when it has a cap, then every
// requested pool row FOR UPDATE in pool-ID order. Two leases therefore never
// wait on each other for a worker or a job row, and take the rows they both
// lock exclusively in the same order, so they cannot deadlock on Postgres.
//
// Under those locks the lease re-checks the worker (sqlLeaseWorkerRefuses: the
// lease of an offline or disabled worker is Lost and writes nothing) and
// re-reads the anchored caps (rereadCapsTx), so the worker state and the caps
// it decides on are the committed ones, not the ones read before the locks.
// Every operation that holds the worker row or the job row FOR UPDATE and this
// lease now wait for each other, which closes, on Postgres:
//
//   - a cancel's reported set. CancelJobExecution and CancelTaskExecution hold
//     the job row from the read of the tasks they report to the UPDATE that
//     cancels them, so no lease commits in between: the set is exact, and a
//     lease that waited finds the job or task no longer leasable;
//   - a worker going offline against a lease. Taking a worker offline (the
//     heartbeat sweep, a deregister, a restart's reclaim) holds the worker row
//     from its marking UPDATE to commit: a lease already holding the row is
//     waited for and its task reclaimed, and a lease that waited sees the
//     worker offline and is Lost;
//   - the latest-attempt checks. CompleteTaskAttempt and the other
//     job-anchored attempt writers hold the job row from the check to the task
//     write, so no lease adds a newer attempt between the two;
//   - demotion. A demotion that holds the job row while it re-checks that no
//     task of the job is in flight waits for a lease in flight and sees its
//     task, or is waited for and leaves a job the lease then finds stopped.
//
// The cost: completions and other job-row writers serialize briefly with
// leases on the same job (they already serialized among themselves on that
// row), and a write to a worker row, such as a heartbeat, waits briefly for
// that worker's lease in flight. Leases among themselves never wait.
//
// The task's guarded UPDATE runs before any count: it takes the task's row
// lock, and every count after it includes the task itself. The queue, farm and
// pool counts each run under their anchor, so no other lease can change them
// between this count and this write (I5). A queue or farm read as uncapped is
// not anchored: a cap set on it concurrently orders after this lease, exactly
// as if the lease had committed first, and setting a cap never evicts running
// work. The pool caps are read under their anchors already. The attempt is
// inserted before its claims, which reference it. On SQLite the locks do
// nothing, since the single write connection serializes the whole
// transaction, but the worker re-check is a real check there too: it refuses a
// worker that went offline or was disabled after the scheduler read it.
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
	ok, err := scope.recheckUnderLocksTx(ctx, tx, req.WorkerID)
	if err != nil {
		return store.LeaseResult{}, err
	}
	if !ok {
		return lost, nil
	}

	nowText := TimeText(req.Now)
	moved, err := leaseTaskRowTx(ctx, tx, req, nowText)
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
// row and the task's job row shared (leases never wait on each other, but
// every operation holding either row FOR UPDATE waits for the lease and is
// waited for by it), then the queue and the farm exclusively, each only when
// capped, then each pool exclusively in the order of claims, which the caller
// has sorted by pool ID.
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

// recheckUnderLocksTx runs the lease's checks that must see the rows as they are
// under its anchors: the worker re-check, then the cap re-read. ok is false when
// the lease must be Lost: the worker's row says it cannot take work.
func (sc *leaseScope) recheckUnderLocksTx(ctx context.Context, tx *dbTx, workerID string) (ok bool, err error) {
	var refuses bool
	if err := tx.QueryRowContext(ctx, sqlLeaseWorkerRefuses, workerID).Scan(&refuses); err != nil {
		return false, fmt.Errorf("store: check worker %s for lease: %w", workerID, mapErr(err))
	}
	if refuses {
		return false, nil
	}
	return sc.rereadCapsTx(ctx, tx)
}

// rereadCapsTx replaces the caps read before the locks with the values under
// them, for the rows that were anchored (capped). A row read as uncapped stays
// unanchored and uncapped: a cap committed concurrently orders after this
// lease, exactly as if the lease had committed first, and setting a cap never
// evicts running work. An anchored row whose cap was cleared meanwhile re-reads
// as 0, unlimited, which fullTx skips. ok is false when the queue or the farm
// row is gone, which the lease's job-row lock and the foreign keys rule out
// but which reads as Lost, as the scope read's inner joins do.
func (sc *leaseScope) rereadCapsTx(ctx context.Context, tx *dbTx) (ok bool, err error) {
	if sc.queueCap <= 0 && sc.farmCap <= 0 {
		return true, nil
	}
	var q, f int
	err = tx.QueryRowContext(ctx, sqlLeaseCaps, sc.queueID, sc.farmID).Scan(&q, &f)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: re-read lease caps: %w", mapErr(err))
	}
	if sc.queueCap > 0 {
		sc.queueCap = q
	}
	if sc.farmCap > 0 {
		sc.farmCap = f
	}
	return true, nil
}

// leaseTaskRowTx makes the guarded ready → assigned write and reports whether
// it landed.
func leaseTaskRowTx(ctx context.Context, tx *dbTx, req store.LeaseRequest, nowText string) (bool, error) {
	res, err := tx.ExecContext(ctx, sqlLeaseTaskGuarded, req.WorkerID, nowText, nowText, req.TaskID, nowText)
	if err != nil {
		return false, fmt.Errorf("store: lease task %s: %w", req.TaskID, mapErr(err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, mapErr(err)
	}
	return n == 1, nil
}

// fullTx re-counts the queue's and the farm's active tasks, with the same
// statements the scheduler's policy gate counts with. It runs after the
// lease's own task update, so each count includes the task being leased: a
// count above the cap means the task did not fit. It returns the outcome of
// the first full one, queue before farm, or "" when both have room.
func (sc *leaseScope) fullTx(ctx context.Context, tx *dbTx) (store.LeaseOutcome, error) {
	checks := []struct {
		capacity  int
		query, id string
		full      store.LeaseOutcome
	}{
		{sc.queueCap, sqlCountActiveTasksInQueue, sc.queueID, store.LeaseQueueFull},
		{sc.farmCap, sqlCountActiveTasksInFarm, sc.farmID, store.LeaseFarmFull},
	}
	for _, c := range checks {
		if c.capacity <= 0 {
			continue
		}
		var active int
		if err := tx.QueryRowContext(ctx, c.query, c.id).Scan(&active); err != nil {
			return "", fmt.Errorf("store: count active tasks for %s check: %w", c.full, mapErr(err))
		}
		if active > c.capacity {
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
