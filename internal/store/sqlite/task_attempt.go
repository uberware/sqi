// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/uberware/sqi/internal/store"
)

const attemptCols = `
	id, task_id, worker_id, session_id, attempt_number, status,
	exit_code, started_at, ended_at, created_at`

const (
	sqlInsertAttempt = `
INSERT INTO task_attempts (
	id, task_id, worker_id, session_id, attempt_number, status,
	exit_code, started_at, ended_at, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING ` + attemptCols

	sqlGetAttempt = `SELECT ` + attemptCols + ` FROM task_attempts WHERE id = ?`

	sqlListAttempts = `SELECT ` + attemptCols + `
FROM task_attempts WHERE task_id = ?
ORDER BY attempt_number ASC`

	sqlUpdateAttempt = `
UPDATE task_attempts
SET status = ?, exit_code = ?, ended_at = ?
WHERE id = ?
RETURNING ` + attemptCols
)

func scanAttempt(row scanner) (store.TaskAttempt, error) {
	var a store.TaskAttempt
	var sessionID sql.NullString
	var status, startedAt, createdAt string
	var exitCode sql.NullInt64
	var endedAt sql.NullString

	if err := row.Scan(
		&a.ID, &a.TaskID, &a.WorkerID, &sessionID, &a.AttemptNumber, &status,
		&exitCode, &startedAt, &endedAt, &createdAt,
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
		exitCode, timeToText(attempt.StartedAt), nullTimeToText(attempt.EndedAt), now)
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
func (s *Store) UpdateTaskAttempt(ctx context.Context, attempt store.TaskAttempt) (store.TaskAttempt, error) {
	var exitCode sql.NullInt64
	if attempt.ExitCode != nil {
		exitCode = sql.NullInt64{Int64: int64(*attempt.ExitCode), Valid: true}
	}
	row := s.stmtUpdateAttempt.QueryRowContext(ctx,
		string(attempt.Status), exitCode, nullTimeToText(attempt.EndedAt), attempt.ID)
	out, err := scanAttempt(row)
	return out, mapErr(err)
}
