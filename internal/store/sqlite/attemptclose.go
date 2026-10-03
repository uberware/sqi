// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uberware/sqi/internal/store"
)

const (
	// sqlCASTaskStatus is the I1 guard for a task status write: it lands only
	// while the task still holds the status the caller validated against.
	// unschedulable_reason is only meaningful while a task is ready (set by the
	// scheduler sweep), so it is cleared here so a task carries no stale
	// annotation once it leaves ready for any reason.
	sqlCASTaskStatus = `
UPDATE tasks SET status = ?, updated_at = ?, unschedulable_reason = ''
WHERE id = ? AND status = ?`

	// sqlCloseRunningAttempt closes an attempt only while it is still running,
	// so a redelivered terminal report never rewrites an attempt the server
	// already closed. An empty session or message leaves the stored value.
	sqlCloseRunningAttempt = `
UPDATE task_attempts
SET    status = ?, exit_code = ?, ended_at = ?,
       session_id = COALESCE(NULLIF(?, ''), session_id),
       message = COALESCE(NULLIF(?, ''), message)
WHERE  id = ? AND status = 'running'`

	sqlTaskJobID = `SELECT job_id FROM tasks WHERE id = ?`

	// casMaxAttempts bounds the compare-and-set loop. On SQLite the guarded
	// UPDATE can never miss, because the transaction is serialized; under
	// Postgres READ COMMITTED a miss means another writer moved the task
	// between this transaction's read and write, and re-reading inside the same
	// transaction sees the committed value.
	casMaxAttempts = 3
)

// casResult is the outcome of [casTaskStatusTx].
type casResult int

const (
	// casApplied means the task moved to the requested status.
	casApplied casResult = iota
	// casSame means the task already held the requested status; nothing was
	// written.
	casSame
	// casRejected means the status was not written: the state machine refused
	// the arrow, the task does not exist, or the status kept changing. The
	// error returned alongside says which.
	casRejected
)

// casWriteTaskStatus writes to only while the task still holds observed.
func casWriteTaskStatus(ctx context.Context, tx *sql.Tx, id string, observed, to store.TaskStatus, now time.Time) (bool, error) {
	res, err := tx.ExecContext(ctx, sqlCASTaskStatus, string(to), timeToText(now.UTC()), id, string(observed))
	if err != nil {
		return false, mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, mapErr(err)
	}
	return n == 1, nil
}

// casTaskStatusTx is invariant I1 for tasks: read the status, validate the
// arrow, write only while the status is unchanged, and retry a bounded number
// of times when it was not. Writing the current status is casSame (a no-op);
// an illegal arrow is casRejected, with the validation error returned
// alongside it so callers can wrap it. An unknown task is casRejected with
// [store.ErrNotFound].
func casTaskStatusTx(ctx context.Context, tx *sql.Tx, id string, to store.TaskStatus, now time.Time) (casResult, error) {
	for range casMaxAttempts {
		var current string
		if err := tx.QueryRowContext(ctx, sqlSelectTaskStatus, id).Scan(&current); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return casRejected, store.ErrNotFound
			}
			return casRejected, fmt.Errorf("sqlite: select task status: %w", mapErr(err))
		}
		if store.TaskStatus(current) == to {
			return casSame, nil
		}
		if err := store.ValidateTaskTransition(store.TaskStatus(current), to); err != nil {
			return casRejected, fmt.Errorf("sqlite: task %s: %w", id, err)
		}
		ok, err := casWriteTaskStatus(ctx, tx, id, store.TaskStatus(current), to, now)
		if err != nil {
			return casRejected, err
		}
		if ok {
			return casApplied, nil
		}
	}
	return casRejected, fmt.Errorf("sqlite: task %s: status kept changing under compare-and-set", id)
}

// closeAttemptAndReleaseTx is invariant I3 inside an open transaction: close
// the attempt if it is still running, then release every claim it holds. The
// release is unconditional so a redelivery (attempt already closed) and an
// attempt that never held a claim are both harmless no-ops. The attempt's
// ended_at is the worker's c.EndedAt; the claims' released_at is server time
// (releasedText), as every claim release has always been, so a skewed worker
// clock can never put it before the claim's claimed_at.
func closeAttemptAndReleaseTx(ctx context.Context, tx *sql.Tx, c store.AttemptCompletion, endedText, releasedText string) error {
	if _, err := tx.ExecContext(ctx, sqlCloseRunningAttempt,
		string(c.AttemptStatus), nullInt(c.ExitCode), endedText, c.SessionID, c.Message, c.AttemptID); err != nil {
		return fmt.Errorf("sqlite: close attempt %s: %w", c.AttemptID, mapErr(err))
	}
	if _, err := tx.ExecContext(ctx, sqlReleaseAttemptClaims, releasedText, c.AttemptID); err != nil {
		return fmt.Errorf("sqlite: release claims of attempt %s: %w", c.AttemptID, mapErr(err))
	}
	return nil
}

// CompleteTaskAttempt implements [store.TaskStore].
//
// Anchor: the task's job row, the parent every job-level operation (cancel,
// retry, finalize) locks, so this is ordered against them on Postgres. The
// statements run in the order of spec 5.2: close the attempt, release its
// claims, then move the task. The claims are released before the task row is
// touched, and the release commits even when the task move is refused, so a
// canceled task's late report still frees its pool slots.
func (s *Store) CompleteTaskAttempt(ctx context.Context, c store.AttemptCompletion) (store.CompletionResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.CompletionResult{}, fmt.Errorf("sqlite: begin complete task attempt: %w", mapErr(err))
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // rollback after commit is a no-op

	var jobID string
	if err := tx.QueryRowContext(ctx, sqlTaskJobID, c.TaskID).Scan(&jobID); err != nil {
		return store.CompletionResult{}, mapErr(err)
	}
	if err := lockAnchors(ctx, tx, jobAnchor(jobID)); err != nil {
		return store.CompletionResult{}, err
	}
	// Server time stamps the claims' released_at and the task row's updated_at:
	// c.EndedAt comes from the worker's clock and belongs to the attempt only.
	now := time.Now().UTC()
	if err := closeAttemptAndReleaseTx(ctx, tx, c, timeToText(c.EndedAt.UTC()), timeToText(now)); err != nil {
		return store.CompletionResult{}, err
	}

	result := store.CompletionResult{Applied: true}
	cas, err := casTaskStatusTx(ctx, tx, c.TaskID, c.TaskStatus, now)
	switch {
	case cas == casRejected && errors.Is(err, store.ErrInvalidTransition):
		// The task is already terminal in some other status. The attempt close
		// and claim release above still commit: only the task write is refused.
		result = store.CompletionResult{Rejected: true}
	case err != nil:
		return store.CompletionResult{}, err
	case c.FailureReason != "":
		if _, err := tx.ExecContext(ctx, sqlSetTaskFailureReason, c.FailureReason, timeToText(now), c.TaskID); err != nil {
			return store.CompletionResult{}, fmt.Errorf("sqlite: stamp failure reason on task %s: %w", c.TaskID, mapErr(err))
		}
	}
	if err := tx.Commit(); err != nil {
		return store.CompletionResult{}, fmt.Errorf("sqlite: commit complete task attempt: %w", mapErr(err))
	}
	return result, nil
}

const (
	// sqlSelectActiveJobTasks reads the tasks a job cancel is about to move out
	// of assigned/running, with their worker still set: the UPDATE that follows
	// clears it, and SQLite's RETURNING cannot report the pre-update value (I2).
	sqlSelectActiveJobTasks = `SELECT ` + taskCols + ` FROM tasks WHERE job_id = ? AND status IN ('assigned', 'running')`

	// sqlReleaseClosedJobClaims releases the claims of the job's attempts that
	// are no longer running (I3). The status guard means a claim is never
	// released while its attempt is still open.
	sqlReleaseClosedJobClaims = `
UPDATE usage_claims SET released_at = ?
WHERE  released_at IS NULL
  AND  task_attempt_id IN (SELECT ta.id FROM task_attempts ta JOIN tasks t ON t.id = ta.task_id
                           WHERE t.job_id = ? AND ta.status != 'running')`

	// sqlCancelOneTask is the single-task cancel. Unlike [sqlCancelJobTasks] it
	// leaves assigned_worker_id and assigned_at in place, as a status write
	// through [Store.UpdateTaskStatus] always has, so a canceled task still
	// shows the worker that held it. The reason is stamped only on a task with
	// none yet.
	sqlCancelOneTask = `
UPDATE tasks
SET    status = 'canceled', updated_at = ?, unschedulable_reason = '',
       failure_reason = CASE WHEN failure_reason = '' THEN ? ELSE failure_reason END
WHERE  id = ? AND status IN ('pending', 'ready', 'assigned', 'running')`

	// sqlCloseTaskRunningAttempts closes a task's running attempts with the
	// given status. An empty message leaves the stored one, so a close that has
	// nothing to say never blanks what the attempt already recorded.
	sqlCloseTaskRunningAttempts = `
UPDATE task_attempts SET status = ?, ended_at = ?, message = COALESCE(NULLIF(?, ''), message)
WHERE  task_id = ? AND status = 'running'`

	sqlReleaseClosedTaskClaims = `
UPDATE usage_claims SET released_at = ?
WHERE  released_at IS NULL
  AND  task_attempt_id IN (SELECT id FROM task_attempts WHERE task_id = ? AND status != 'running')`
)

// closeTaskAttemptsTx is invariant I3 for one task inside an open transaction:
// it closes the task's running attempts with status and message, then releases
// the claims of every attempt of the task that is no longer running. The
// release is by attempt status, so a claim is never released while its attempt
// is open, and it also repairs a claim an earlier close left on a finished
// attempt. A non-empty message is recorded over the attempt's own; an empty one
// leaves it.
//
// Callers run it AFTER writing the task row in the same transaction (a cancel to
// canceled, a reap or an offline reclaim to ready), and that write is the
// guarantee: it holds the task row until commit, so no lease can add an attempt
// to the task behind this close. A lease that has already committed needs no
// waiting: its attempt and claims are visible to the statements here. A lease
// still in flight is waited for only when the write's predicate matches the
// row's pre-lease version: a cancel's does (it matches ready), while the reaper's
// and the offline reclaim's do not (they match assigned/running), so those skip a
// row a lease is moving out of ready. A lease that starts after the write blocks
// until commit and then opens a new attempt of its own, which this close never
// sees.
func closeTaskAttemptsTx(ctx context.Context, tx *sql.Tx, taskID string, status store.AttemptStatus, message, nowText string) error {
	if _, err := tx.ExecContext(ctx, sqlCloseTaskRunningAttempts, string(status), nowText, message, taskID); err != nil {
		return fmt.Errorf("sqlite: close attempts of task %s: %w", taskID, mapErr(err))
	}
	if _, err := tx.ExecContext(ctx, sqlReleaseClosedTaskClaims, nowText, taskID); err != nil {
		return fmt.Errorf("sqlite: release claims of task %s: %w", taskID, mapErr(err))
	}
	return nil
}

// CancelJobExecution implements [store.TaskStore].
//
// Anchor: the job row, the parent every job-level operation locks. The anchor
// comes before the SELECT that reports the active tasks (I2), and the writes run
// in the order spec 4.1 requires on Postgres: the tasks are canceled first, and
// only then are their attempts closed and their claims released. A LeaseTask
// that holds one of the tasks is therefore waited for by the task UPDATE (whose
// predicate matches the task both as ready and as assigned), and the attempt
// and claims it committed are seen by the two statements after it. Closing
// attempts first would miss an attempt a lease created in between.
//
// The anchor does not make the reported set exact on Postgres. LeaseTask does
// not take the job row, so a lease that commits between the SELECT and the
// task UPDATE is canceled, and its attempt and claims closed, but it is not in
// the returned set and its worker gets no cancel signal. H4c must close that:
// either LeaseTask also anchors the job row, or the set comes from the
// UPDATE itself. None of it can arise here, where the single write connection
// serializes the lease against this whole transaction.
func (s *Store) CancelJobExecution(ctx context.Context, jobID, reason string, now time.Time) ([]store.Task, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: begin cancel job execution: %w", mapErr(err))
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // rollback after commit is a no-op

	// Order (spec 4.1): anchor, then the SELECT of the active set, then tasks,
	// attempts, claims. See the doc comment for why tasks come first.
	if err := lockAnchors(ctx, tx, jobAnchor(jobID)); err != nil {
		return nil, err
	}
	active, err := queryTasksTx(ctx, tx, sqlSelectActiveJobTasks, jobID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: select active tasks of job %s: %w", jobID, err)
	}
	nowText := timeToText(now.UTC())
	if _, err := tx.ExecContext(ctx, sqlCancelJobTasks, nowText, reason, jobID); err != nil {
		return nil, fmt.Errorf("sqlite: cancel tasks of job %s: %w", jobID, mapErr(err))
	}
	if _, err := tx.ExecContext(ctx, sqlCancelJobAttempts, nowText, jobID); err != nil {
		return nil, fmt.Errorf("sqlite: close attempts of job %s: %w", jobID, mapErr(err))
	}
	if _, err := tx.ExecContext(ctx, sqlReleaseClosedJobClaims, nowText, jobID); err != nil {
		return nil, fmt.Errorf("sqlite: release claims of job %s: %w", jobID, mapErr(err))
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: commit cancel job execution: %w", mapErr(err))
	}
	return active, nil
}

// CancelTaskExecution implements [store.TaskStore].
//
// Anchor and statement order are CancelJobExecution's: the task's job row first
// (the task's job id never changes, so it is read before the lock), then the
// task as it is under that lock, then the task is canceled before its attempt
// is closed and its claims released. The task row is read after the lock rather
// than before it, so on Postgres the returned prior task is current against
// every job-anchored writer. The "already terminal" answer comes from the
// guarded UPDATE itself, so it is exact on any store. The prior task is NOT
// protected against a LeaseTask, which does not take the job row: on Postgres a
// lease that commits between the read and the UPDATE is canceled while prior
// still shows the task ready with no worker, so no cancel signal reaches that
// worker. That is CancelJobExecution's gap, and H4c closes both the same way.
func (s *Store) CancelTaskExecution(ctx context.Context, taskID, reason string, now time.Time) (store.Task, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.Task{}, false, fmt.Errorf("sqlite: begin cancel task execution: %w", mapErr(err))
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // rollback after commit is a no-op

	// Order (spec 4.1): the task's job id, the anchor on that job row, the task
	// as it is under the anchor, then task, attempt, claims. See the doc comment.
	var jobID string
	if err := tx.QueryRowContext(ctx, sqlTaskJobID, taskID).Scan(&jobID); err != nil {
		return store.Task{}, false, mapErr(err) // sql.ErrNoRows is ErrNotFound
	}
	if err := lockAnchors(ctx, tx, jobAnchor(jobID)); err != nil {
		return store.Task{}, false, err
	}
	prior, err := scanTask(tx.QueryRowContext(ctx, sqlGetTask, taskID))
	if err != nil {
		return store.Task{}, false, mapErr(err)
	}
	nowText := timeToText(now.UTC())
	res, err := tx.ExecContext(ctx, sqlCancelOneTask, nowText, reason, taskID)
	if err != nil {
		return store.Task{}, false, fmt.Errorf("sqlite: cancel task %s: %w", taskID, mapErr(err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return store.Task{}, false, fmt.Errorf("sqlite: cancel task %s: %w", taskID, mapErr(err))
	}
	if n == 0 {
		// Already terminal: nothing was written, and the deferred rollback
		// leaves the attempts and claims for whatever closed the task.
		return prior, false, nil
	}
	if err := closeTaskAttemptsTx(ctx, tx, taskID, store.AttemptStatusCanceled, "", nowText); err != nil {
		return store.Task{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return store.Task{}, false, fmt.Errorf("sqlite: commit cancel task execution: %w", mapErr(err))
	}
	return prior, true, nil
}

const (
	// sqlOfflineStaleWorker is the I1 guard for the heartbeat sweep's write: the
	// worker goes offline only while it is still online and its heartbeat is still
	// older than the cutoff. A NULL heartbeat compares false, so a worker with no
	// recorded heartbeat is never stale. Its binds are updated_at, id, cutoff.
	sqlOfflineStaleWorker = `
UPDATE workers SET status = 'offline', updated_at = ?
WHERE  id = ? AND status = 'online' AND last_heartbeat_at < ?`

	// sqlOfflineWorker is the unconditional write behind a graceful deregister.
	// Its binds are updated_at, id.
	sqlOfflineWorker = `UPDATE workers SET status = 'offline', updated_at = ? WHERE id = ?`

	// sqlReclaimWorkerTasksReturning returns a worker's in-flight tasks to ready
	// and RETURNING hands back exactly the rows it changed, as they are after the
	// reset (I2). Its binds are updated_at, worker id.
	sqlReclaimWorkerTasksReturning = sqlReclaimWorkerTasks + `
RETURNING ` + taskCols

	// sqlWorkerInFlightJobIDs lists, sorted and without repeats, the jobs the
	// worker's assigned and running tasks belong to: the job rows an offline
	// transition anchors.
	sqlWorkerInFlightJobIDs = `
SELECT DISTINCT job_id FROM tasks
WHERE  assigned_worker_id = ? AND status IN ('assigned', 'running')
ORDER BY job_id`
)

// OfflineStaleWorker implements [store.WorkerStore].
func (s *Store) OfflineStaleWorker(ctx context.Context, id string, cutoff, now time.Time) ([]store.Task, bool, error) {
	return s.offlineWorker(ctx, id, now, sqlOfflineStaleWorker, timeToText(now.UTC()), id, timeToText(cutoff.UTC()))
}

// OfflineWorker implements [store.WorkerStore].
func (s *Store) OfflineWorker(ctx context.Context, id string, now time.Time) ([]store.Task, error) {
	tasks, ok, err := s.offlineWorker(ctx, id, now, sqlOfflineWorker, timeToText(now.UTC()), id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, store.ErrNotFound
	}
	return tasks, nil
}

// offlineWorker takes the worker offline with the given guarded statement and,
// only if that statement matched, closes the attempts of its in-flight tasks,
// releases their claims and returns the tasks to ready, all in one transaction
// (invariants I1, I2 and I3). The bool is whether the worker was taken offline.
//
// Anchors and statement order (spec 4.1): the worker row first, then each job row
// the worker's tasks belong to, sorted by id; the statements run worker, tasks,
// then attempts and claims. The tasks are reclaimed before their attempts are
// closed so that, on Postgres, a running or terminal report holding the row of an
// already assigned/running task is waited for by the reclaim's UPDATE (which then
// re-checks its predicate against the committed row), and the attempt and claims
// that report committed are seen by the statements after it. Closing attempts
// first would let a report move the task (assigned to running, say) after its
// attempt was already closed as failed. A LeaseTask still in flight is NOT waited
// for: it is moving a ready row, which the reclaim's predicate does not match in
// the version its snapshot sees, so the UPDATE skips that row. The gap that
// leaves is the one described next (LeaseTask does not anchor the worker); until
// H4c closes it, the stale-assignment reaper recovers a task a lease hands a
// worker that has already gone offline.
//
// What H4c must do on Postgres. The marking UPDATE takes the worker row lock
// itself, so the worker anchor is already first. The in-flight job IDs are read
// UNLOCKED, after the worker row is held and before any task row is touched, and
// the job rows are locked sorted before the reclaim UPDATE; a task row locked
// before its job row is the inversion every job-level operation would deadlock
// on. That read is only a candidate set: a task leased to this worker after it
// has an unanchored job row, because LeaseTask does not take the worker row.
// H4c must either have LeaseTask take the worker row FOR SHARE, or re-read the
// set once the worker row is held and repeat until it is stable. None of that
// can arise here: the single write connection serializes every writer, so the
// set read below cannot change before the reclaim, and lockAnchors does nothing.
func (s *Store) offlineWorker(ctx context.Context, id string, now time.Time, markSQL string, markArgs ...any) ([]store.Task, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("sqlite: begin offline worker: %w", mapErr(err))
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // rollback after commit is a no-op

	// Order (spec 4.1): the worker row, then the guarded mark, then the job rows,
	// then the tasks, attempts and claims. See the doc comment.
	if err := lockAnchors(ctx, tx, workerAnchor(id)); err != nil {
		return nil, false, err
	}
	res, err := tx.ExecContext(ctx, markSQL, markArgs...)
	if err != nil {
		return nil, false, fmt.Errorf("sqlite: mark worker %s offline: %w", id, mapErr(err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, false, fmt.Errorf("sqlite: mark worker %s offline: %w", id, mapErr(err))
	}
	if n == 0 {
		// The guard did not match (unknown worker, not online, or a fresh
		// heartbeat): nothing was written and the deferred rollback ends the
		// transaction.
		return nil, false, nil
	}
	jobAnchors, err := workerJobAnchorsTx(ctx, tx, id)
	if err != nil {
		return nil, false, err
	}
	if err := lockAnchors(ctx, tx, jobAnchors...); err != nil {
		return nil, false, err
	}
	nowText := timeToText(now.UTC())
	reclaimed, err := queryTasksTx(ctx, tx, sqlReclaimWorkerTasksReturning, nowText, id)
	if err != nil {
		return nil, false, fmt.Errorf("sqlite: reclaim tasks of worker %s: %w", id, err)
	}
	for _, t := range reclaimed {
		if err := closeTaskAttemptsTx(ctx, tx, t.ID, store.AttemptStatusFailed, store.FailureReasonWorkerOffline, nowText); err != nil {
			return nil, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("sqlite: commit offline worker: %w", mapErr(err))
	}
	return reclaimed, true, nil
}

// workerJobAnchorsTx returns the job-row anchors of the jobs the worker's
// assigned and running tasks belong to, sorted by id.
func workerJobAnchorsTx(ctx context.Context, tx *sql.Tx, workerID string) ([]anchor, error) {
	rows, err := tx.QueryContext(ctx, sqlWorkerInFlightJobIDs, workerID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list in-flight jobs of worker %s: %w", workerID, mapErr(err))
	}
	defer rows.Close()
	var anchors []anchor
	for rows.Next() {
		var jobID string
		if err := rows.Scan(&jobID); err != nil {
			return nil, fmt.Errorf("sqlite: scan in-flight job of worker %s: %w", workerID, err)
		}
		anchors = append(anchors, jobAnchor(jobID))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list in-flight jobs of worker %s: %w", workerID, mapErr(err))
	}
	return anchors, nil
}
