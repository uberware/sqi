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
	raw_template, template_format, parameters, created_at, updated_at, started_at, completed_at,
	failed_attempts, max_attempts, retry_delay_seconds, failure_limit, park_reason`

const (
	sqlInsertJob = `
INSERT INTO jobs (
	id, farm_id, queue_id, name, owner, submitter, priority, status, project,
	raw_template, template_format, parameters, created_at, updated_at, started_at, completed_at,
	failed_attempts, max_attempts, retry_delay_seconds, failure_limit, park_reason)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING ` + jobCols

	sqlGetJob = `SELECT ` + jobCols + ` FROM jobs WHERE id = ?`

	// sqlUpdateJob updates only the mutable user-settable fields of a job.
	// status, started_at, and completed_at are lifecycle columns managed
	// exclusively by UpdateJobStatus / CancelJobStatus and are intentionally
	// excluded here — writing them via UpdateJob would race with concurrent
	// scheduler status transitions. failed_attempts and park_reason are
	// likewise lifecycle-managed (by the scheduler's failure-limit sweep) and
	// are excluded for the same reason; max_attempts, retry_delay_seconds,
	// and failure_limit are the user-settable retry-policy overrides.
	sqlUpdateJob = `
UPDATE jobs
SET farm_id = ?, queue_id = ?, name = ?, owner = ?, submitter = ?, priority = ?,
	project = ?, raw_template = ?, template_format = ?,
	max_attempts = ?, retry_delay_seconds = ?, failure_limit = ?,
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

	sqlDeleteJobTasks        = `DELETE FROM tasks WHERE job_id = ?`
	sqlDeleteJobSteps        = `DELETE FROM steps WHERE job_id = ?`
	sqlDeleteJobDependencies = `DELETE FROM job_dependencies WHERE job_id = ?`
	sqlDeleteJobRow          = `DELETE FROM jobs WHERE id = ?`

	sqlInsertJobDependency = `
INSERT OR IGNORE INTO job_dependencies (job_id, depends_on_job_id, created_at)
VALUES (?, ?, ?)`

	sqlListJobDependencyIDs = `
SELECT depends_on_job_id FROM job_dependencies
WHERE job_id = ? ORDER BY depends_on_job_id`

	sqlListDependents = `
SELECT job_id FROM job_dependencies
WHERE depends_on_job_id = ? ORDER BY job_id`

	sqlListBlockedJobs = `SELECT ` + jobCols + ` FROM jobs WHERE status = 'blocked' ORDER BY created_at`

	// Selects terminal jobs whose effective completion time is before the
	// cutoff. The status set is closed by the caller appending 'failed'. The
	// {STATUS} and time predicate are assembled in Go with bound parameters.
	//
	// The trailing NOT EXISTS excludes a candidate that is still referenced as
	// depends_on_job_id by a non-terminal dependent: purging it would leave
	// the dependent's reconciler reading a GetJob ErrNotFound for an upstream
	// that actually succeeded, which the reconciler treats as "missing" and
	// wrongly cancels the dependent. Manual DELETE (DeleteJob) intentionally
	// keeps the cancel-dependents behavior — this guard only applies to the
	// automatic retention sweep.
	sqlSelectExpiredJobsPrefix = `
SELECT id, name, farm_id, queue_id FROM jobs
WHERE status IN (`
	sqlSelectExpiredJobsSuffix = `)
  AND COALESCE(completed_at, updated_at) < ?
  AND NOT EXISTS (
        SELECT 1 FROM job_dependencies jd
        JOIN jobs d ON d.id = jd.job_id
        WHERE jd.depends_on_job_id = jobs.id
          AND d.status NOT IN ('completed', 'failed', 'canceled'))`

	// sqlParkJob pauses a job and records why, but only while it is
	// non-terminal — a job that has already reached completed/failed/canceled
	// is left alone (zero rows affected is a legitimate no-op, not an error).
	sqlParkJob = `
UPDATE jobs SET status = 'paused', park_reason = ?, updated_at = ?
WHERE id = ? AND status NOT IN ('completed','failed','canceled')`

	// sqlResumeJob is the inverse of sqlParkJob: only a paused job is resumed.
	// An auto-parked job (non-empty park_reason) also gets its failure counter
	// reset — otherwise the very next genuine failure would re-park it — while
	// a manual pause/resume keeps the accumulated count.
	sqlResumeJob = `
UPDATE jobs
SET status = 'pending', updated_at = ?,
    failed_attempts = CASE WHEN park_reason != '' THEN 0 ELSE failed_attempts END,
    park_reason = ''
WHERE id = ? AND status = 'paused'`
)

func scanJob(row scanner) (store.Job, error) {
	var j store.Job
	var status, templateFormat, paramsJSON string
	var createdAt, updatedAt string
	var startedAt, completedAt sql.NullString
	var maxAtt, delay, failLim sql.NullInt64

	if err := row.Scan(
		&j.ID, &j.FarmID, &j.QueueID, &j.Name, &j.Owner, &j.Submitter,
		&j.Priority, &status, &j.Project,
		&j.RawTemplate, &templateFormat, &paramsJSON,
		&createdAt, &updatedAt, &startedAt, &completedAt,
		&j.FailedAttempts, &maxAtt, &delay, &failLim, &j.ParkReason,
	); err != nil {
		return store.Job{}, err
	}

	j.Status = store.JobStatus(status)
	j.TemplateFormat = store.TemplateFormat(templateFormat)
	j.CreatedAt = mustTime(createdAt)
	j.UpdatedAt = mustTime(updatedAt)
	j.StartedAt = nullTextToTime(startedAt)
	j.CompletedAt = nullTextToTime(completedAt)
	j.MaxAttempts, j.RetryDelaySeconds, j.FailureLimit = intPtr(maxAtt), intPtr(delay), intPtr(failLim)

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
		nullTimeToText(job.StartedAt), nullTimeToText(job.CompletedAt),
		job.FailedAttempts, nullInt(job.MaxAttempts), nullInt(job.RetryDelaySeconds), nullInt(job.FailureLimit),
		job.ParkReason)
	out, err := scanJob(row)
	return out, mapErr(err)
}

// GetJob implements [store.JobStore].
func (s *Store) GetJob(ctx context.Context, id string) (store.Job, error) {
	row := s.stmtGetJob.QueryRowContext(ctx, id)
	out, err := scanJob(row)
	if err != nil {
		return store.Job{}, mapErr(err)
	}

	deps, err := s.ListJobDependencyIDs(ctx, id)
	if err != nil {
		return store.Job{}, err
	}
	out.DependsOn = deps

	return out, nil
}

// CreateJobDependencies implements [store.JobStore]. It records that jobID
// waits on each upstream ID; duplicate edges (job_id, depends_on_job_id) are
// silently ignored via INSERT OR IGNORE.
func (s *Store) CreateJobDependencies(ctx context.Context, jobID string, upstreamIDs []string) error {
	if len(upstreamIDs) == 0 {
		return nil
	}
	now := time.Now().UTC()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return mapErr(err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // rollback after commit is a no-op

	for _, up := range upstreamIDs {
		if _, err := tx.ExecContext(ctx, sqlInsertJobDependency, jobID, up, timeToText(now)); err != nil {
			return fmt.Errorf("sqlite: create job dependency %s->%s: %w", jobID, up, mapErr(err))
		}
	}
	return mapErr(tx.Commit())
}

// ListJobDependencyIDs implements [store.JobStore]. It returns the upstream
// job IDs jobID waits on, ordered by upstream job ID.
func (s *Store) ListJobDependencyIDs(ctx context.Context, jobID string) ([]string, error) {
	return s.queryIDs(ctx, sqlListJobDependencyIDs, jobID)
}

// ListDependents implements [store.JobStore]. It returns the IDs of jobs
// waiting on upstreamJobID, ordered by dependent job ID.
func (s *Store) ListDependents(ctx context.Context, upstreamJobID string) ([]string, error) {
	return s.queryIDs(ctx, sqlListDependents, upstreamJobID)
}

// ListBlockedJobs implements [store.JobStore]. It returns every job currently
// in [store.JobStatusBlocked].
func (s *Store) ListBlockedJobs(ctx context.Context) ([]store.Job, error) {
	rows, err := s.db.QueryContext(ctx, sqlListBlockedJobs)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list blocked jobs: %w", mapErr(err))
	}
	defer rows.Close()

	var jobs []store.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// queryIDs runs a single-column string query and returns the values.
func (s *Store) queryIDs(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: query ids: %w", mapErr(err))
	}
	defer rows.Close()

	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
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
		where += ` AND owner = ? COLLATE NOCASE`
		args = append(args, opts.Owner)
	}
	if opts.Project != "" {
		where += ` AND project = ?`
		args = append(args, opts.Project)
	}
	if frag, sargs := searchClause([]string{"name", "id", "owner", "project"}, opts.Search); frag != "" {
		where += frag
		args = append(args, sargs...)
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
// owner, submitter, priority, project, raw_template, template_format,
// max_attempts, retry_delay_seconds, failure_limit).
// status, started_at, completed_at, failed_attempts, and park_reason are
// never touched here; use UpdateJobStatus, CancelJobStatus, or the
// scheduler's failure-limit sweep for those.
func (s *Store) UpdateJob(ctx context.Context, job store.Job) (store.Job, error) {
	now := timeToText(time.Now().UTC())
	row := s.stmtUpdateJob.QueryRowContext(ctx,
		job.FarmID, job.QueueID, job.Name, job.Owner, job.Submitter, job.Priority,
		job.Project, job.RawTemplate, string(job.TemplateFormat),
		nullInt(job.MaxAttempts), nullInt(job.RetryDelaySeconds), nullInt(job.FailureLimit),
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

	args := append([]any(nil), statuses...)
	args = append(args, timeToText(cutoff.UTC()))
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var deleted []store.DeletedJob
	for rows.Next() {
		var d store.DeletedJob
		if err := rows.Scan(&d.ID, &d.Name, &d.FarmID, &d.QueueID); err != nil {
			return nil, err
		}
		deleted = append(deleted, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	for _, d := range deleted {
		for _, q := range []string{
			sqlDeleteJobCheckouts, sqlDeleteJobTaskLogs, sqlDeleteJobAttempts,
			sqlDeleteJobTasks, sqlDeleteJobSteps, sqlDeleteJobDependencies, sqlDeleteJobRow,
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
		sqlDeleteJobDependencies,
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

// ParkJob implements [store.JobStore].
//
// A zero-row UPDATE is ambiguous between "job not found" and "job already
// terminal"; a follow-up GetJob disambiguates the same way [Store.CancelJobStatus]
// does — terminal is a legitimate no-op (nil), missing propagates [store.ErrNotFound].
func (s *Store) ParkJob(ctx context.Context, jobID, reason string, now time.Time) error {
	nowText := timeToText(now.UTC())

	res, err := s.db.ExecContext(ctx, sqlParkJob, reason, nowText, jobID)
	if err != nil {
		return mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil // successfully parked
	}

	// Zero rows: job was not found or was already terminal.
	if _, err := s.GetJob(ctx, jobID); err != nil {
		return err // propagates ErrNotFound
	}
	return nil // already terminal — legitimate no-op
}

// ResumeJob implements [store.JobStore].
//
// A zero-row UPDATE is ambiguous between "job not found" and "job no longer
// paused"; a follow-up GetJob disambiguates the same way [Store.ParkJob] does —
// not-paused is a legitimate no-op (nil), missing propagates [store.ErrNotFound].
func (s *Store) ResumeJob(ctx context.Context, jobID string, now time.Time) error {
	res, err := s.db.ExecContext(ctx, sqlResumeJob, timeToText(now.UTC()), jobID)
	if err != nil {
		return mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil // successfully resumed
	}

	if _, err := s.GetJob(ctx, jobID); err != nil {
		return err // propagates ErrNotFound
	}
	return nil // no longer paused — legitimate no-op
}
