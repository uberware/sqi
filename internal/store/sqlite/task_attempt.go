// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// LatestTaskAttempt implements [store.TaskAttemptStore].
func (s *Store) LatestTaskAttempt(ctx context.Context, taskID string) (store.TaskAttempt, error) {
	row := s.stmtLatestAttempt.QueryRowContext(ctx, taskID)
	out, err := scanAttempt(row)
	return out, mapErr(err)
}

// TerminateWorkerAttempts closes every running attempt of the tasks currently
// assigned to workerID with the given status and end time, recording
// [store.FailureReasonWorkerOffline] as the message, and returns how many it
// closed. It releases no claims and leaves the tasks alone; a worker is taken
// offline through [Store.OfflineStaleWorker] and [Store.OfflineWorker], which do
// all three in one transaction.
//
// Test fixture only: a blind write that is not part of store.Store (H4a). H4b decides its fate.
func (s *Store) TerminateWorkerAttempts(ctx context.Context, workerID string, status store.AttemptStatus, endedAt time.Time) (int, error) {
	res, err := s.stmtTerminateWorkerAttempts.ExecContext(ctx, string(status), timeToText(endedAt), store.FailureReasonWorkerOffline, workerID)
	if err != nil {
		return 0, mapErr(err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// CancelJobAttempts closes every running attempt of the job's tasks as canceled,
// ended at endedAt, and returns how many it closed. It releases no claims and
// leaves the tasks alone; a job is canceled through [Store.CancelJobExecution],
// which does all three in one transaction.
//
// Test fixture only: a blind write that is not part of store.Store (H4a). H4b decides its fate.
func (s *Store) CancelJobAttempts(ctx context.Context, jobID string, endedAt time.Time) (int, error) {
	res, err := s.stmtCancelJobAttempts.ExecContext(ctx, timeToText(endedAt), jobID)
	if err != nil {
		return 0, mapErr(err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

const attemptCols = `
	id, task_id, worker_id, session_id, attempt_number, status,
	exit_code, started_at, ended_at, created_at, message`

const (
	sqlInsertAttempt = `
INSERT INTO task_attempts (
	id, task_id, worker_id, session_id, attempt_number, status,
	exit_code, started_at, ended_at, created_at, message)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING ` + attemptCols

	sqlGetAttempt = `SELECT ` + attemptCols + ` FROM task_attempts WHERE id = ?`

	sqlLatestAttempt = `SELECT ` + attemptCols + `
FROM task_attempts WHERE task_id = ?
ORDER BY attempt_number DESC
LIMIT 1`

	sqlListAttempts = `SELECT ` + attemptCols + `
FROM task_attempts WHERE task_id = ?
ORDER BY attempt_number ASC`

	// sqlUpdateAttempt writes only while the attempt is still running, so a
	// late or echoed report can never overwrite an attempt something else
	// already closed (F16). The status test is evaluated inside the UPDATE (I1).
	// It is [sqlCloseRunningAttempt], the same guarded write with the same
	// binds, returning the row it wrote.
	sqlUpdateAttempt = sqlCloseRunningAttempt + `
RETURNING ` + attemptCols

	// sqlTerminateWorkerAttempts closes out all running attempts for tasks
	// currently assigned to the given worker. Must be called before
	// ReclaimWorkerTasks so that assigned_worker_id is still set on the tasks.
	// The message is bound at call time to [store.FailureReasonWorkerOffline].
	sqlTerminateWorkerAttempts = `
UPDATE task_attempts
SET status = ?, ended_at = ?, message = ?
WHERE status = 'running'
  AND task_id IN (
    SELECT id FROM tasks
    WHERE assigned_worker_id = ?
      AND status IN ('assigned', 'running')
  )`

	// sqlCancelJobAttempts closes out all running attempts for tasks belonging
	// to the given job, not only those of tasks a cancel just moved: an attempt
	// left open on a task that is already terminal is the same leak. It is the
	// one statement behind both [Store.CancelJobExecution], which runs it after
	// the tasks are canceled, and the [Store.CancelJobAttempts] fixture.
	sqlCancelJobAttempts = `
UPDATE task_attempts
SET    status = 'canceled', ended_at = ?
WHERE  status = 'running'
  AND  task_id IN (SELECT id FROM tasks WHERE job_id = ?)`
)

func scanAttempt(row scanner) (store.TaskAttempt, error) {
	var a store.TaskAttempt
	var sessionID sql.NullString
	var status, startedAt, createdAt string
	var exitCode sql.NullInt64
	var endedAt sql.NullString

	if err := row.Scan(
		&a.ID, &a.TaskID, &a.WorkerID, &sessionID, &a.AttemptNumber, &status,
		&exitCode, &startedAt, &endedAt, &createdAt, &a.Message,
	); err != nil {
		return store.TaskAttempt{}, err
	}

	a.SessionID = sessionID.String
	a.Status = store.AttemptStatus(status)
	a.StartedAt = mustTime(startedAt)
	a.CreatedAt = mustTime(createdAt)
	a.EndedAt = nullTextToTime(endedAt)

	if exitCode.Valid {
		code := int(exitCode.Int64)
		a.ExitCode = &code
	}

	return a, nil
}

// CreateTaskAttempt implements [store.TaskAttemptStore].
func (s *Store) CreateTaskAttempt(ctx context.Context, attempt store.TaskAttempt) (store.TaskAttempt, error) {
	now := timeToText(time.Now().UTC())

	var exitCode sql.NullInt64
	if attempt.ExitCode != nil {
		exitCode = sql.NullInt64{Int64: int64(*attempt.ExitCode), Valid: true}
	}

	row := s.stmtInsertAttempt.QueryRowContext(ctx,
		attempt.ID, attempt.TaskID, attempt.WorkerID,
		nullString(attempt.SessionID), attempt.AttemptNumber, string(attempt.Status),
		exitCode, timeToText(attempt.StartedAt), nullTimeToText(attempt.EndedAt), now, attempt.Message)
	out, err := scanAttempt(row)
	return out, mapErr(err)
}

// GetTaskAttempt implements [store.TaskAttemptStore].
func (s *Store) GetTaskAttempt(ctx context.Context, id string) (store.TaskAttempt, error) {
	row := s.stmtGetAttempt.QueryRowContext(ctx, id)
	out, err := scanAttempt(row)
	return out, mapErr(err)
}

// ListTaskAttempts implements [store.TaskAttemptStore].
func (s *Store) ListTaskAttempts(ctx context.Context, taskID string) ([]store.TaskAttempt, error) {
	rows, err := s.stmtListAttempts.QueryContext(ctx, taskID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var attempts []store.TaskAttempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, a)
	}
	return attempts, rows.Err()
}

// UpdateTaskAttempt implements [store.TaskAttemptStore].
// If attempt.SessionID is non-empty it is written to the record; an empty
// value is treated as "no change" via COALESCE so a caller that has no
// session ID to record does not overwrite an existing one.
//
// The write applies only while the attempt is running. A zero-row result is
// told apart by a read afterwards: an attempt that exists but is closed is
// [store.ErrConflict], one that does not exist is [store.ErrNotFound].
//
// It must not be used to close an attempt: it can write a terminal status but
// releases no usage claims (I3). The closing operations are CompleteTaskAttempt,
// RecordTaskFailure, CancelJobExecution, CancelTaskExecution,
// ReclaimStaleAssignedTasks, OfflineStaleWorker and OfflineWorker.
func (s *Store) UpdateTaskAttempt(ctx context.Context, attempt store.TaskAttempt) (store.TaskAttempt, error) {
	var exitCode sql.NullInt64
	if attempt.ExitCode != nil {
		exitCode = sql.NullInt64{Int64: int64(*attempt.ExitCode), Valid: true}
	}
	row := s.stmtUpdateAttempt.QueryRowContext(ctx,
		string(attempt.Status), exitCode, nullTimeToText(attempt.EndedAt),
		attempt.SessionID, // COALESCE(NULLIF(?, ''), session_id)
		attempt.Message,   // COALESCE(NULLIF(?, ''), message)
		attempt.ID)
	out, err := scanAttempt(row)
	if errors.Is(err, sql.ErrNoRows) {
		if _, gerr := s.GetTaskAttempt(ctx, attempt.ID); gerr != nil {
			return store.TaskAttempt{}, gerr // ErrNotFound
		}
		return store.TaskAttempt{}, store.ErrConflict // closed: never rewritten (F16)
	}
	return out, mapErr(err)
}
