// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlstore

import (
	"context"
	"database/sql"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// LatestTaskAttempt implements [store.TaskAttemptStore].
func (s *Store) LatestTaskAttempt(ctx context.Context, taskID string) (store.TaskAttempt, error) {
	row := s.stmtLatestAttempt.QueryRowContext(ctx, taskID)
	out, err := scanAttempt(row)
	return out, mapErr(err)
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

	// sqlCancelJobAttempts closes out all running attempts for tasks belonging
	// to the given job, not only those of tasks a cancel just moved: an attempt
	// left open on a task that is already terminal is the same leak.
	// [Store.CancelJobExecution] runs it after the tasks are canceled.
	sqlCancelJobAttempts = `
UPDATE task_attempts
SET    status = 'canceled', ended_at = ?
WHERE  status = 'running'
  AND  task_id IN (SELECT id FROM tasks WHERE job_id = ?)`
)

const sqlListOpenAttempts = `
SELECT a.id, a.task_id, a.worker_id, a.created_at
FROM   task_attempts a
JOIN   tasks t ON t.id = a.task_id
WHERE  a.status = 'running'
  AND  t.status IN ('assigned', 'running')
  AND  a.attempt_number = (SELECT MAX(m.attempt_number) FROM task_attempts m WHERE m.task_id = a.task_id)
  AND  a.created_at < ?
ORDER  BY a.created_at, a.id`

// ListOpenAttempts implements [store.TaskAttemptStore].
func (s *Store) ListOpenAttempts(ctx context.Context, createdBefore time.Time) ([]store.OpenAttempt, error) {
	rows, err := s.r.QueryContext(ctx, sqlListOpenAttempts, TimeText(createdBefore))
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []store.OpenAttempt
	for rows.Next() {
		var a store.OpenAttempt
		var created string
		if err := rows.Scan(&a.AttemptID, &a.TaskID, &a.WorkerID, &created); err != nil {
			return nil, mapErr(err)
		}
		a.CreatedAt = mustTime(created)
		out = append(out, a)
	}
	return out, mapErr(rows.Err())
}

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
