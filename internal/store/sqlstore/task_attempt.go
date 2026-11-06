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
	exit_code, started_at, ended_at, created_at, message,
	progress, status_text, progress_at`

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
	var endedAt, progressAt sql.NullString
	var progress sql.NullInt64

	if err := row.Scan(
		&a.ID, &a.TaskID, &a.WorkerID, &sessionID, &a.AttemptNumber, &status,
		&exitCode, &startedAt, &endedAt, &createdAt, &a.Message,
		&progress, &a.StatusText, &progressAt,
	); err != nil {
		return store.TaskAttempt{}, err
	}

	a.SessionID = sessionID.String
	a.Status = store.AttemptStatus(status)
	a.StartedAt = mustTime(startedAt)
	a.CreatedAt = mustTime(createdAt)
	a.EndedAt = nullTextToTime(endedAt)
	a.ProgressAt = nullTextToTime(progressAt)
	a.Progress = nullIntPtr(progress)
	a.ExitCode = nullIntPtr(exitCode)

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

// sqlLatestAttemptProgress selects the snapshot of each listed task's latest
// attempt; the IN list is completed with one placeholder per task. Attempts are
// opened at lease time, so a pending or ready task (never leased, or requeued)
// only has a closed earlier attempt, whose snapshot is stale and excluded.
const sqlLatestAttemptProgress = `
SELECT a.task_id, a.progress, a.status_text, a.progress_at
FROM   task_attempts a
JOIN   tasks t ON t.id = a.task_id
WHERE  a.attempt_number = (SELECT MAX(m.attempt_number) FROM task_attempts m WHERE m.task_id = a.task_id)
  AND  t.status NOT IN ('pending', 'ready')
  AND  (a.progress_at IS NOT NULL OR a.status_text != '')
  AND  a.task_id IN (`

// LatestAttemptProgress implements [store.TaskAttemptStore].
func (s *Store) LatestAttemptProgress(ctx context.Context, taskIDs []string) (map[string]store.TaskProgress, error) {
	out := make(map[string]store.TaskProgress, len(taskIDs))
	if len(taskIDs) == 0 {
		return out, nil
	}
	args := make([]any, len(taskIDs))
	for i, id := range taskIDs {
		args[i] = id
	}
	// qmarks holds only "?" marks; every id is bound.
	rows, err := s.r.QueryContext(ctx, sqlLatestAttemptProgress+qmarks(len(taskIDs))+`)`, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var progress sql.NullInt64
		var progressAt sql.NullString
		var p store.TaskProgress
		if err := rows.Scan(&id, &progress, &p.StatusText, &progressAt); err != nil {
			return nil, mapErr(err)
		}
		p.Progress = nullIntPtr(progress)
		p.ProgressAt = nullTextToTime(progressAt)
		out[id] = p
	}
	return out, mapErr(rows.Err())
}

const sqlRunningProgressByJob = `
SELECT COALESCE(SUM(COALESCE(a.progress, 0)), 0)
FROM   tasks t
JOIN   task_attempts a ON a.task_id = t.id
WHERE  t.job_id = ? AND t.status = 'running'
  AND  a.attempt_number = (SELECT MAX(m.attempt_number) FROM task_attempts m WHERE m.task_id = t.id)`

// RunningProgressByJob implements [store.TaskAttemptStore]. The percents are
// summed as integers and divided once, so the result does not depend on the
// backend's numeric type.
func (s *Store) RunningProgressByJob(ctx context.Context, jobID string) (float64, error) {
	var sum int64
	if err := s.r.QueryRowContext(ctx, sqlRunningProgressByJob, jobID).Scan(&sum); err != nil {
		return 0, mapErr(err)
	}
	return float64(sum) / 100.0, nil
}

// sqlRunningProgressByJobs is sqlRunningProgressByJob grouped over a list of
// jobs; the IN list is completed with one placeholder per job.
const sqlRunningProgressByJobs = `
SELECT t.job_id, COALESCE(SUM(COALESCE(a.progress, 0)), 0)
FROM   tasks t
JOIN   task_attempts a ON a.task_id = t.id
WHERE  t.status = 'running'
  AND  a.attempt_number = (SELECT MAX(m.attempt_number) FROM task_attempts m WHERE m.task_id = t.id)
  AND  t.job_id IN (`

// RunningProgressByJobs implements [store.TaskAttemptStore]. Sums stay integer
// per job and are divided once, as in RunningProgressByJob.
func (s *Store) RunningProgressByJobs(ctx context.Context, jobIDs []string) (map[string]float64, error) {
	out := make(map[string]float64, len(jobIDs))
	if len(jobIDs) == 0 {
		return out, nil
	}
	args := make([]any, len(jobIDs))
	for i, id := range jobIDs {
		args[i] = id
	}
	// qmarks holds only "?" marks; every id is bound.
	rows, err := s.r.QueryContext(ctx, sqlRunningProgressByJobs+qmarks(len(jobIDs))+`) GROUP BY t.job_id`, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var sum int64
		if err := rows.Scan(&id, &sum); err != nil {
			return nil, mapErr(err)
		}
		out[id] = float64(sum) / 100.0
	}
	return out, mapErr(rows.Err())
}
