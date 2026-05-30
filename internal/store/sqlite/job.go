// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/uberware/sqi/internal/store"
)

const jobCols = `
	id, farm_id, queue_id, name, owner, submitter, priority, status, project,
	raw_template, template_format, created_at, updated_at, started_at, completed_at`

const (
	sqlInsertJob = `
INSERT INTO jobs (
	id, farm_id, queue_id, name, owner, submitter, priority, status, project,
	raw_template, template_format, created_at, updated_at, started_at, completed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING ` + jobCols

	sqlGetJob = `SELECT ` + jobCols + ` FROM jobs WHERE id = ?`

	sqlUpdateJob = `
UPDATE jobs
SET farm_id = ?, queue_id = ?, name = ?, owner = ?, submitter = ?, priority = ?,
	status = ?, project = ?, raw_template = ?, template_format = ?,
	updated_at = ?, started_at = ?, completed_at = ?
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
)

func scanJob(row scanner) (store.Job, error) {
	var j store.Job
	var status, templateFormat string
	var createdAt, updatedAt string
	var startedAt, completedAt sql.NullString

	if err := row.Scan(
		&j.ID, &j.FarmID, &j.QueueID, &j.Name, &j.Owner, &j.Submitter,
		&j.Priority, &status, &j.Project,
		&j.RawTemplate, &templateFormat,
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
	return j, nil
}

// CreateJob implements [store.JobStore].
func (s *Store) CreateJob(ctx context.Context, job store.Job) (store.Job, error) {
	now := timeToText(time.Now().UTC())
	row := s.stmtInsertJob.QueryRowContext(ctx,
		job.ID, job.FarmID, job.QueueID, job.Name, job.Owner, job.Submitter,
		job.Priority, string(job.Status), job.Project,
		job.RawTemplate, string(job.TemplateFormat),
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
func (s *Store) UpdateJob(ctx context.Context, job store.Job) (store.Job, error) {
	now := timeToText(time.Now().UTC())
	row := s.stmtUpdateJob.QueryRowContext(ctx,
		job.FarmID, job.QueueID, job.Name, job.Owner, job.Submitter, job.Priority,
		string(job.Status), job.Project, job.RawTemplate, string(job.TemplateFormat),
		now, nullTimeToText(job.StartedAt), nullTimeToText(job.CompletedAt), job.ID)
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
