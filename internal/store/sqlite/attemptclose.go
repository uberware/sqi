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
// attempt that never held a claim are both harmless no-ops.
func closeAttemptAndReleaseTx(ctx context.Context, tx *sql.Tx, c store.AttemptCompletion, endedText string) error {
	if _, err := tx.ExecContext(ctx, sqlCloseRunningAttempt,
		string(c.AttemptStatus), nullInt(c.ExitCode), endedText, c.SessionID, c.Message, c.AttemptID); err != nil {
		return fmt.Errorf("sqlite: close attempt %s: %w", c.AttemptID, mapErr(err))
	}
	if _, err := tx.ExecContext(ctx, sqlReleaseAttemptClaims, endedText, c.AttemptID); err != nil {
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
	endedText := timeToText(c.EndedAt.UTC())
	if err := closeAttemptAndReleaseTx(ctx, tx, c, endedText); err != nil {
		return store.CompletionResult{}, err
	}

	result := store.CompletionResult{Applied: true}
	cas, err := casTaskStatusTx(ctx, tx, c.TaskID, c.TaskStatus, c.EndedAt)
	switch {
	case cas == casRejected && errors.Is(err, store.ErrInvalidTransition):
		// The task is already terminal in some other status. The attempt close
		// and claim release above still commit: only the task write is refused.
		result = store.CompletionResult{Rejected: true}
	case err != nil:
		return store.CompletionResult{}, err
	case c.FailureReason != "":
		if _, err := tx.ExecContext(ctx, sqlSetTaskFailureReason, c.FailureReason, endedText, c.TaskID); err != nil {
			return store.CompletionResult{}, fmt.Errorf("sqlite: stamp failure reason on task %s: %w", c.TaskID, mapErr(err))
		}
	}
	if err := tx.Commit(); err != nil {
		return store.CompletionResult{}, fmt.Errorf("sqlite: commit complete task attempt: %w", mapErr(err))
	}
	return result, nil
}
