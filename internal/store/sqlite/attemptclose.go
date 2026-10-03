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
// attempt. The task row must already have left assigned/running, so a lease
// cannot be adding an attempt behind this close.
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
// that holds one of the tasks is therefore waited for by the task UPDATE, and
// the attempt and claims it committed are seen by the two statements after it.
// Closing attempts first would miss an attempt a lease created in between.
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
// is closed and its claims released. The task row is read again after the lock
// rather than before it, so on Postgres the returned prior task and the
// "already terminal" answer are the ones the anchor protects.
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
