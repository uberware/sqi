// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

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
	// task's queue and farm, and their caps. The inner joins make a task whose
	// job, queue or farm row is missing read as unknown, which is Lost.
	sqlLeaseScope = `
SELECT j.queue_id, j.farm_id, q.max_concurrent_tasks, f.max_concurrent_tasks
FROM   tasks  t
JOIN   jobs   j ON t.job_id   = j.id
JOIN   queues q ON j.queue_id = q.id
JOIN   farms  f ON j.farm_id  = f.id
WHERE  t.id = ?`

	// sqlLeaseTaskGuarded moves one task ready → assigned only while it passes
	// sqlLeasableTask, the predicate ListReadyTasks lists by, so a task that
	// stopped being leasable after the scheduler chose it (leased by someone
	// else, still backing off, its job or queue paused, its job terminal) is
	// left alone and the lease is Lost. It is LeaseReadyTask's write
	// (sqlLeaseTaskWrite) under that predicate, whose t.status and
	// t.retry_after refer to the row being updated.
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

// leaseScope is the task's queue and farm, with the caps read for them. A cap
// of 0 means unlimited.
type leaseScope struct {
	queueID, farmID   string
	queueCap, farmCap int
}

// LeaseTask implements [store.TaskStore].
func (s *Store) LeaseTask(ctx context.Context, req store.LeaseRequest) (store.LeaseResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.LeaseResult{}, fmt.Errorf("sqlite: begin lease task: %w", mapErr(err))
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // rollback after commit is a no-op

	res, err := leaseTx(ctx, tx, req)
	if err != nil || res.Outcome != store.LeaseLeased {
		// Returning without Commit rolls the transaction back, so an error or a
		// non-leased outcome writes nothing, even when the task update or a
		// claim insert had already run.
		return res, err
	}
	if err := tx.Commit(); err != nil {
		return store.LeaseResult{}, fmt.Errorf("sqlite: commit lease task: %w", mapErr(err))
	}
	return res, nil
}

// leaseTx runs the lease's statements inside tx. The caller commits only a
// Leased result.
//
// Statement order (spec §4.1). The anchors are the queue row and the farm row,
// each only when it has a cap, then every requested pool row in pool-ID order:
// the anchor table's order, so two leases lock the rows they share in the same
// order and cannot deadlock on Postgres. The scope read precedes them only
// because it names those rows and says which are capped. The task's guarded
// UPDATE runs before any count: it takes the task's row lock, so a concurrent
// cancel or reap of this task is ordered against the lease, and every count
// after it includes the task itself. The queue, farm and pool counts each run
// under their anchor, so no other lease can change them between this count
// and this write (I5). The attempt is inserted before its claims, which
// reference it. H4c: the caps used here were read before the anchors were
// taken, so on Postgres they must be re-read under the anchor locks.
func leaseTx(ctx context.Context, tx *sql.Tx, req store.LeaseRequest) (store.LeaseResult, error) {
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
	if err := lockAnchors(ctx, tx, scope.anchors(claims)...); err != nil {
		return store.LeaseResult{}, err
	}

	nowText := timeToText(req.Now)
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

// readLeaseScopeTx reads the task's queue and farm and their caps. found is
// false when the task, or its job, queue or farm row, does not exist.
func readLeaseScopeTx(ctx context.Context, tx *sql.Tx, taskID string) (scope leaseScope, found bool, err error) {
	err = tx.QueryRowContext(ctx, sqlLeaseScope, taskID).Scan(&scope.queueID, &scope.farmID, &scope.queueCap, &scope.farmCap)
	if errors.Is(err, sql.ErrNoRows) {
		return leaseScope{}, false, nil
	}
	if err != nil {
		return leaseScope{}, false, fmt.Errorf("sqlite: read lease scope of task %s: %w", taskID, mapErr(err))
	}
	return scope, true, nil
}

// anchors lists the lease's anchor rows in the spec §4.1 lock order: the queue
// and the farm, each only when capped, then each pool in the order of claims,
// which the caller has sorted by pool ID.
func (sc leaseScope) anchors(claims []store.UsagePoolClaim) []anchor {
	out := make([]anchor, 0, 2+len(claims))
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

// leaseTaskRowTx makes the guarded ready → assigned write and reports whether
// it landed.
func leaseTaskRowTx(ctx context.Context, tx *sql.Tx, req store.LeaseRequest, nowText string) (bool, error) {
	res, err := tx.ExecContext(ctx, sqlLeaseTaskGuarded, req.WorkerID, nowText, nowText, req.TaskID, nowText)
	if err != nil {
		return false, fmt.Errorf("sqlite: lease task %s: %w", req.TaskID, mapErr(err))
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
func (sc leaseScope) fullTx(ctx context.Context, tx *sql.Tx) (store.LeaseOutcome, error) {
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
			return "", fmt.Errorf("sqlite: count active tasks for %s check: %w", c.full, mapErr(err))
		}
		if active > c.capacity {
			return c.full, nil
		}
	}
	return "", nil
}

// insertLeaseAttemptTx inserts the lease's running attempt, numbered by
// sqlNextAttemptNumber.
func insertLeaseAttemptTx(ctx context.Context, tx *sql.Tx, req store.LeaseRequest, nowText string) (store.TaskAttempt, error) {
	var num int
	if err := tx.QueryRowContext(ctx, sqlNextAttemptNumber, req.TaskID).Scan(&num); err != nil {
		return store.TaskAttempt{}, fmt.Errorf("sqlite: number lease attempt of task %s: %w", req.TaskID, mapErr(err))
	}
	// sqlInsertAttempt binds: id, task_id, worker_id, session_id,
	// attempt_number, status, exit_code, started_at, ended_at, created_at,
	// message.
	row := tx.QueryRowContext(ctx, sqlInsertAttempt,
		req.AttemptID, req.TaskID, req.WorkerID, nil, num, string(store.AttemptStatusRunning),
		nil, nowText, nil, nowText, "")
	a, err := scanAttempt(row)
	if err != nil {
		return store.TaskAttempt{}, fmt.Errorf("sqlite: insert lease attempt: %w", mapErr(err))
	}
	return a, nil
}

// claimPoolTx claims one slot of c's pool for attemptID. The pool's cap is
// read here, in the lease's transaction, never taken from the caller's copy,
// and a pool that no longer exists counts as full. It returns a zero result
// when the claim was written and a PoolFull result naming the pool when not.
func claimPoolTx(ctx context.Context, tx *sql.Tx, c store.UsagePoolClaim, attemptID, nowText string) (store.LeaseResult, error) {
	pool, err := scanPool(tx.QueryRowContext(ctx, sqlGetPool, c.PoolID))
	if errors.Is(err, sql.ErrNoRows) {
		// Deleted after the scheduler decided the task was eligible.
		return store.LeaseResult{Outcome: store.LeasePoolFull, FullPool: c.PoolName}, nil
	}
	if err != nil {
		return store.LeaseResult{}, fmt.Errorf("sqlite: read pool %q for lease: %w", c.PoolName, mapErr(err))
	}
	if pool.MaxConcurrent > 0 {
		var active int
		if err := tx.QueryRowContext(ctx, sqlActiveClaimCount, c.PoolID).Scan(&active); err != nil {
			return store.LeaseResult{}, fmt.Errorf("sqlite: count active claims for pool %q: %w", pool.Name, mapErr(err))
		}
		if active >= pool.MaxConcurrent {
			return store.LeaseResult{Outcome: store.LeasePoolFull, FullPool: pool.Name}, nil
		}
	}
	if _, err := tx.ExecContext(ctx, sqlInsertClaim, c.ClaimID, c.PoolID, attemptID, nowText); err != nil {
		return store.LeaseResult{}, fmt.Errorf("sqlite: insert claim for pool %q: %w", pool.Name, mapErr(err))
	}
	return store.LeaseResult{}, nil
}
