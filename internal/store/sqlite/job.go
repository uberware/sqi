// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/uberware/sqi/internal/store"
)

const jobCols = `
	id, farm_id, queue_id, name, owner, submitter, priority, status, project,
	raw_template, template_format, parameters, created_at, updated_at, started_at, completed_at`

const (
	sqlInsertJob = `
INSERT INTO jobs (
	id, farm_id, queue_id, name, owner, submitter, priority, status, project,
	raw_template, template_format, parameters, created_at, updated_at, started_at, completed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING ` + jobCols

	sqlGetJob = `SELECT ` + jobCols + ` FROM jobs WHERE id = ?`

	// sqlUpdateJob updates only the mutable user-settable fields of a job.
	// status, started_at, and completed_at are lifecycle columns managed
	// exclusively by UpdateJobStatus / CancelJobStatus and are intentionally
	// excluded here — writing them via UpdateJob would race with concurrent
	// scheduler status transitions.
	sqlUpdateJob = `
UPDATE jobs
SET farm_id = ?, queue_id = ?, name = ?, owner = ?, submitter = ?, priority = ?,
	project = ?, raw_template = ?, template_format = ?,
	updated_at = ?
WHERE id = ?
RETURNING ` + jobCols

	// COALESCE(started_at, ?) sets started_at on first transition to 'running';
	// subsequent calls preserve the original value.
	sqlUpdateJobStatus = `
UPDATE jobs
SET status      = ?,
	started_at  = COALESCE(started_at, ?),
	completed_at = ?,
	updated_at  = ?
WHERE id = ?`

	// sqlCancelJobStatus transitions a job to 'canceled' only when it is not
	// already in a terminal state.  Zero rows affected means the job was either
	// not found or already terminal; the caller distinguishes these two cases by
	// following up with a GetJob when rows == 0.
	sqlCancelJobStatus = `
UPDATE jobs
SET status       = 'canceled',
	completed_at = ?,
	updated_at   = ?
WHERE id = ?
  AND status NOT IN ('completed', 'failed', 'canceled')`

	// sqlDemoteStalledJobs demotes any running job with no in-flight (assigned or
	// running) task back to pending, but only while it still has schedulable
	// (ready or pending) work — a job whose tasks are all terminal is left for the
	// completion logic to finalize. started_at is intentionally preserved.
	// RETURNING yields the demoted job IDs so the caller can fan out WS events.
	sqlDemoteStalledJobs = `
UPDATE jobs
SET status = 'pending', updated_at = ?
WHERE status = 'running'
  AND NOT EXISTS (
        SELECT 1 FROM tasks t
        WHERE t.job_id = jobs.id AND t.status IN ('assigned', 'running'))
  AND EXISTS (
        SELECT 1 FROM tasks t
        WHERE t.job_id = jobs.id AND t.status IN ('ready', 'pending'))
RETURNING id`

	// Job-deletion cascade. Each statement scopes to one job via job_id (or a
	// task subquery for grandchild tables). Run inside a single transaction in
	// this exact order so enforced foreign keys never block a delete.
	sqlDeleteJobCheckouts = `
DELETE FROM usage_claims
WHERE task_attempt_id IN (
	SELECT ta.id FROM task_attempts ta
	JOIN tasks t ON t.id = ta.task_id
	WHERE t.job_id = ?)`

	sqlDeleteJobTaskLogs = `
DELETE FROM task_logs
WHERE task_id IN (SELECT id FROM tasks WHERE job_id = ?)`

	sqlDeleteJobAttempts = `
DELETE FROM task_attempts
WHERE task_id IN (SELECT id FROM tasks WHERE job_id = ?)`

	sqlDeleteJobTasks = `DELETE FROM tasks WHERE job_id = ?`
	sqlDeleteJobSteps = `DELETE FROM steps WHERE job_id = ?`
	sqlDeleteJobRow   = `DELETE FROM jobs WHERE id = ?`

	// Selects terminal jobs whose effective completion time is before the
	// cutoff. The status set is closed by the caller appending 'failed'. The
	// {STATUS} and time predicate are assembled in Go with bound parameters.
	sqlSelectExpiredJobsPrefix = `
SELECT id, name, farm_id, queue_id FROM jobs
WHERE status IN (`
	sqlSelectExpiredJobsSuffix = `)
  AND COALESCE(completed_at, updated_at) < ?`
)

func scanJob(row scanner) (store.Job, error) {
	var j store.Job
	var status, templateFormat, paramsJSON string
	var createdAt, updatedAt string
	var startedAt, completedAt sql.NullString

	if err := row.Scan(
		&j.ID, &j.FarmID, &j.QueueID, &j.Name, &j.Owner, &j.Submitter,
		&j.Priority, &status, &j.Project,
		&j.RawTemplate, &templateFormat, &paramsJSON,
		&createdAt, &updatedAt, &startedAt, &completedAt,
	); err != nil {
		return store.Job{}, err
	}

	j.Status = store.JobStatus(status)
	j.TemplateFormat = store.TemplateFormat(templateFormat)
	j.CreatedAt = mustTime(createdAt)
	j.UpdatedAt = mustTime(updatedAt)
	j.StartedAt = nullTextToTime(startedAt)
	j.CompletedAt = nullTextToTime(completedAt)

	params, err := unmarshalJSON(paramsJSON, map[string]string{})
	if err != nil {
		return store.Job{}, err
	}
	j.Parameters = params

	return j, nil
}

// CreateJob implements [store.JobStore].
func (s *Store) CreateJob(ctx context.Context, job store.Job) (store.Job, error) {
	paramsJSON, err := marshalJSON(job.Parameters)
	if err != nil {
		return store.Job{}, err
	}
	now := timeToText(time.Now().UTC())
	row := s.stmtInsertJob.QueryRowContext(ctx,
		job.ID, job.FarmID, job.QueueID, job.Name, job.Owner, job.Submitter,
		job.Priority, string(job.Status), job.Project,
		job.RawTemplate, string(job.TemplateFormat), paramsJSON,
		now, now,
		nullTimeToText(job.StartedAt), nullTimeToText(job.CompletedAt))
	out, err := scanJob(row)
	return out, mapErr(err)
}

// GetJob implements [store.JobStore].
func (s *Store) GetJob(ctx context.Context, id string) (store.Job, error) {
	row := s.stmtGetJob.QueryRowContext(ctx, id)
	out, err := scanJob(row)
	return out, mapErr(err)
}

// jobSortColumns maps [store.JobSortField] values to their safe, hard-coded
// SQL column names. Only columns present in this map are accepted; anything
// else falls back to the default. This eliminates any risk of SQL injection
// through the sort field.
var jobSortColumns = map[store.JobSortField]string{
	store.JobSortByCreatedAt: "created_at",
	store.JobSortByPriority:  "priority",
	store.JobSortByStatus:    "status",
	store.JobSortByUpdatedAt: "updated_at",
	store.JobSortByName:      "name",
}

// ListJobs implements [store.JobStore].
// Dynamic filters use parameterised ad-hoc queries due to the variable WHERE
// clause. The sort column is looked up from a hard-coded allow-list before
// being interpolated into the SQL to prevent injection.
func (s *Store) ListJobs(ctx context.Context, opts store.ListJobsOptions) (store.Page[store.Job], error) {
	opts.Pagination.Validate() //nolint:errcheck // Validate only clamps; never errors

	where := ` WHERE 1=1`
	args := make([]any, 0, 5)

	if opts.FarmID != "" {
		where += ` AND farm_id = ?`
		args = append(args, opts.FarmID)
	}
	if opts.QueueID != "" {
		where += ` AND queue_id = ?`
		args = append(args, opts.QueueID)
	}
	if opts.Status != "" {
		where += ` AND status = ?`
		args = append(args, string(opts.Status))
	}
	if opts.Owner != "" {
		where += ` AND owner = ?`
		args = append(args, opts.Owner)
	}
	if opts.Project != "" {
		where += ` AND project = ?`
		args = append(args, opts.Project)
	}

	// COUNT total matching rows (same WHERE, no ORDER/LIMIT).
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs`+where, args...).Scan(&total); err != nil {
		return store.Page[store.Job]{}, mapErr(err)
	}

	// Resolve sort column — default to created_at if unrecognized.
	col, ok := jobSortColumns[opts.SortBy]
	if !ok {
		col = "created_at"
	}
	dir := sortDirKeyword(opts.SortDir)

	// col comes from jobSortColumns (hard-coded allow-list); dir is "ASC" or
	// "DESC" from sortDirKeyword; where uses only ? placeholders for user values.
	q := `SELECT ` + jobCols + ` FROM jobs` + where + //nolint:gosec // see comment above
		` ORDER BY ` + col + ` ` + dir +
		` LIMIT ? OFFSET ?`
	rows, err := s.db.QueryContext(ctx, q, append(args, opts.Pagination.Limit, opts.Pagination.Offset)...)
	if err != nil {
		return store.Page[store.Job]{}, mapErr(err)
	}
	defer rows.Close()

	jobs := make([]store.Job, 0)
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return store.Page[store.Job]{}, err
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return store.Page[store.Job]{}, err
	}
	return store.Page[store.Job]{
		Items:  jobs,
		Total:  total,
		Limit:  opts.Pagination.Limit,
		Offset: opts.Pagination.Offset,
	}, nil
}

// UpdateJob implements [store.JobStore].
// Only mutable user-settable fields are written (farm_id, queue_id, name,
// owner, submitter, priority, project, raw_template, template_format).
// status, started_at, and completed_at are never touched here; use
// UpdateJobStatus or CancelJobStatus for those.
func (s *Store) UpdateJob(ctx context.Context, job store.Job) (store.Job, error) {
	now := timeToText(time.Now().UTC())
	row := s.stmtUpdateJob.QueryRowContext(ctx,
		job.FarmID, job.QueueID, job.Name, job.Owner, job.Submitter, job.Priority,
		job.Project, job.RawTemplate, string(job.TemplateFormat),
		now, job.ID)
	out, err := scanJob(row)
	return out, mapErr(err)
}

// UpdateJobStatus implements [store.JobStore].
func (s *Store) UpdateJobStatus(ctx context.Context, id string, status store.JobStatus) error {
	now := time.Now().UTC()
	nowText := timeToText(now)

	// maybeStarted is non-null only when transitioning to running, so
	// COALESCE(started_at, ?) sets it on first transition and preserves it thereafter.
	var maybeStarted sql.NullString
	if status == store.JobStatusRunning {
		maybeStarted = sql.NullString{String: nowText, Valid: true}
	}

	// maybeCompleted is non-null for all terminal states.
	var maybeCompleted sql.NullString
	switch status {
	case store.JobStatusCompleted, store.JobStatusFailed, store.JobStatusCanceled:
		maybeCompleted = sql.NullString{String: nowText, Valid: true}
	}

	res, err := s.stmtUpdateJobStatus.ExecContext(ctx,
		string(status), maybeStarted, maybeCompleted, nowText, id)
	if err != nil {
		return mapErr(err)
	}
	return checkRowsAffected(res)
}

// CancelJobStatus implements [store.JobStore].
//
// It issues a conditional UPDATE that only fires when the job is not already
// in a terminal state. When zero rows are affected the job is either missing
// or already terminal; a follow-up GetJob disambiguates:
//   - not found → [store.ErrNotFound]
//   - already canceled → nil (idempotent)
//   - completed or failed → [store.ErrConflict]
func (s *Store) CancelJobStatus(ctx context.Context, id string) error {
	now := time.Now().UTC()
	nowText := timeToText(now)

	res, err := s.db.ExecContext(ctx, sqlCancelJobStatus, nowText, nowText, id)
	if err != nil {
		return mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil // successfully transitioned to canceled
	}

	// Zero rows: job was not found or was already terminal.
	job, err := s.GetJob(ctx, id)
	if err != nil {
		return err // propagates ErrNotFound
	}
	if job.Status == store.JobStatusCanceled {
		return nil // already canceled — idempotent
	}
	return store.ErrConflict // completed or failed — cannot cancel
}

// DemoteStalledJobs implements [store.JobStore].
//
// The single UPDATE ... RETURNING statement is atomic: a concurrent task
// transition to assigned/running cannot slip between the EXISTS check and the
// write, so a job that has just been re-activated is never spuriously demoted.
func (s *Store) DemoteStalledJobs(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, sqlDemoteStalledJobs, timeToText(now.UTC()))
	if err != nil {
		return nil, fmt.Errorf("sqlite: demote stalled jobs: %w", mapErr(err))
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("sqlite: scan demoted job id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, mapErr(rows.Err())
}

// DeleteTerminalJobsBefore implements [store.JobStore]. It selects the eligible
// job IDs first, then deletes each via the shared cascade, all inside one
// transaction so a partially-applied sweep can never leave orphaned child rows.
func (s *Store) DeleteTerminalJobsBefore(
	ctx context.Context, cutoff time.Time, includeFailed bool,
) ([]store.DeletedJob, error) {
	statuses := []any{string(store.JobStatusCompleted), string(store.JobStatusCanceled)}
	if includeFailed {
		statuses = append(statuses, string(store.JobStatusFailed))
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(statuses)), ",")
	// placeholders is "?,?[,?]" — only ? marks; status values are bound.
	query := sqlSelectExpiredJobsPrefix + placeholders + sqlSelectExpiredJobsSuffix //nolint:gosec // see comment

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, mapErr(err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // rollback after commit is a no-op

	args := append(statuses, timeToText(cutoff.UTC()))
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	var deleted []store.DeletedJob
	for rows.Next() {
		var d store.DeletedJob
		if err := rows.Scan(&d.ID, &d.Name, &d.FarmID, &d.QueueID); err != nil {
			rows.Close() //nolint:errcheck // scan already failing
			return nil, err
		}
		deleted = append(deleted, d)
	}
	if err := rows.Err(); err != nil {
		rows.Close() //nolint:errcheck // err already set
		return nil, err
	}
	rows.Close() //nolint:errcheck // done iterating before further writes

	for _, d := range deleted {
		for _, q := range []string{
			sqlDeleteJobCheckouts, sqlDeleteJobTaskLogs, sqlDeleteJobAttempts,
			sqlDeleteJobTasks, sqlDeleteJobSteps, sqlDeleteJobRow,
		} {
			if _, err := tx.ExecContext(ctx, q, d.ID); err != nil {
				return nil, mapErr(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, mapErr(err)
	}
	return deleted, nil
}

// DeleteJob implements [store.JobStore]. The cascade runs in a single
// transaction; if the jobs-row delete affects zero rows the job did not exist
// and the transaction is rolled back with [store.ErrNotFound].
func (s *Store) DeleteJob(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return mapErr(err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // rollback after commit is a no-op

	for _, q := range []string{
		sqlDeleteJobCheckouts,
		sqlDeleteJobTaskLogs,
		sqlDeleteJobAttempts,
		sqlDeleteJobTasks,
		sqlDeleteJobSteps,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return mapErr(err)
		}
	}

	res, err := tx.ExecContext(ctx, sqlDeleteJobRow, id)
	if err != nil {
		return mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return mapErr(tx.Commit())
}
