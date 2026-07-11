// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"time"
)

// JobStatus is the lifecycle state of a job as a whole.
type JobStatus string

const (
	// JobStatusPending means the job has been submitted but no tasks are
	// running yet (e.g. waiting on step dependency resolution or queue capacity).
	JobStatusPending JobStatus = "pending"
	// JobStatusRunning means at least one task is currently assigned or running.
	JobStatusRunning JobStatus = "running"
	// JobStatusPaused means the job has been administratively paused; no new
	// task assignments will be made until it is resumed.
	JobStatusPaused JobStatus = "paused"
	// JobStatusCompleted means all tasks across all steps succeeded.
	JobStatusCompleted JobStatus = "completed"
	// JobStatusFailed means one or more tasks failed and the job cannot proceed.
	JobStatusFailed JobStatus = "failed"
	// JobStatusCanceled means the job was explicitly canceled by a user or API
	// call before it could complete.
	JobStatusCanceled JobStatus = "canceled"
)

// TemplateFormat identifies the serialization format of [Job.RawTemplate].
type TemplateFormat string

const (
	// TemplateFormatYAML means the raw template was submitted as YAML.
	TemplateFormatYAML TemplateFormat = "yaml"
	// TemplateFormatJSON means the raw template was submitted as JSON.
	TemplateFormatJSON TemplateFormat = "json"
)

// Job is the top-level unit of work submitted to sqi-server. It holds the
// verbatim OpenJD template alongside derived metadata used by the scheduler.
type Job struct {
	ID             string
	FarmID         string
	QueueID        string
	Name           string
	Owner          string // human responsible for the job
	Submitter      string // authenticated identity that placed the job
	Priority       int    // higher = more urgent; default 50
	Status         JobStatus
	Project        string
	RawTemplate    string // verbatim OpenJD YAML or JSON as submitted
	TemplateFormat TemplateFormat
	// Parameters holds the fully-bound job-parameter values produced by
	// BindJobParameters at submission time, including applied defaults.
	// Nil or empty for jobs with no declared parameters or submitted before
	// the parameters-persistence migration (back-compat: scheduler falls back
	// to template defaults).
	Parameters map[string]string
	// FailedAttempts is the job's cumulative count of genuine task failures.
	FailedAttempts int
	// MaxAttempts, RetryDelaySeconds, and FailureLimit are per-job retry policy
	// overrides. Nil means "inherit" (Queue -> Farm -> server default).
	MaxAttempts       *int
	RetryDelaySeconds *int
	FailureLimit      *int
	// ParkReason is empty for a manual pause; set when the failure-limit sweep
	// auto-parks the job (e.g. "failure limit reached (25)").
	ParkReason  string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	StartedAt   *time.Time
	CompletedAt *time.Time
}

// JobSortField is a column by which [JobStore.ListJobs] results can be ordered.
type JobSortField string

const (
	// JobSortByCreatedAt orders jobs by submission time (default).
	JobSortByCreatedAt JobSortField = "created_at"
	// JobSortByPriority orders jobs by priority (higher values first when SortDesc).
	JobSortByPriority JobSortField = "priority"
	// JobSortByStatus orders jobs alphabetically by status string.
	JobSortByStatus JobSortField = "status"
	// JobSortByUpdatedAt orders jobs by the time of the most recent change.
	JobSortByUpdatedAt JobSortField = "updated_at"
	// JobSortByName orders jobs alphabetically by name.
	JobSortByName JobSortField = "name"
)

// DeletedJob is the summary of a job removed by [JobStore.DeleteTerminalJobsBefore],
// carrying the fields a WebSocket "removed" event needs.
type DeletedJob struct {
	ID      string
	Name    string
	FarmID  string
	QueueID string
}

// JobStore is the persistence interface for [Job] records.
type JobStore interface {
	// CreateJob inserts a new job with all fields populated by the caller.
	CreateJob(ctx context.Context, job Job) (Job, error)

	// GetJob returns the job with the given ID, or [ErrNotFound].
	GetJob(ctx context.Context, id string) (Job, error)

	// ListJobs returns a paginated, filtered, and sorted page of jobs matching
	// opts. Call [Pagination.Validate] on opts.Pagination before passing it to
	// ensure sensible defaults are applied.
	ListJobs(ctx context.Context, opts ListJobsOptions) (Page[Job], error)

	// UpdateJob replaces the mutable user-settable fields of an existing job
	// (farm_id, queue_id, name, owner, submitter, priority, project,
	// raw_template, template_format) and updates UpdatedAt.
	//
	// status, started_at, and completed_at are lifecycle columns and are
	// intentionally excluded — use [UpdateJobStatus] or [CancelJobStatus]
	// for those. The returned Job reflects the current DB state of all columns.
	//
	// Returns [ErrNotFound] if the job does not exist.
	UpdateJob(ctx context.Context, job Job) (Job, error)

	// UpdateJobStatus transitions a job to a new status and updates UpdatedAt.
	// If the new status is [JobStatusRunning] and StartedAt is nil, StartedAt
	// is set to the current time. Terminal statuses set CompletedAt.
	// Returns [ErrNotFound] if the job does not exist.
	UpdateJobStatus(ctx context.Context, id string, status JobStatus) error

	// CancelJobStatus transitions a job to [JobStatusCanceled] only when the
	// job is not already in a terminal state, preventing a race where a
	// concurrent scheduler transition to completed/failed would be overwritten.
	//
	//   - Non-terminal job → canceled, returns nil.
	//   - Already canceled → no-op, returns nil (idempotent).
	//   - Already completed or failed → returns [ErrConflict].
	//   - Job not found → returns [ErrNotFound].
	CancelJobStatus(ctx context.Context, id string) error

	// DemoteStalledJobs returns any job in [JobStatusRunning] that currently has
	// no task in [TaskStatusAssigned] or [TaskStatusRunning] — yet still has at
	// least one schedulable (ready or pending) task — back to [JobStatusPending],
	// stamping updated_at = now. It reconciles the [JobStatusRunning] invariant
	// ("at least one task is currently assigned or running") after the heartbeat
	// sweep or stale-assigned reaper returns a job's last in-flight task to the
	// ready queue with no worker to pick it up, leaving the job spuriously marked
	// running. Jobs whose tasks are all terminal are left untouched so the
	// completion logic can finalize them to completed/failed. StartedAt is
	// preserved (the job did start). Returns the IDs of the demoted jobs.
	DemoteStalledJobs(ctx context.Context, now time.Time) ([]string, error)

	// DeleteJob hard-deletes a job and every row that belongs to it, in one
	// transaction and FK-safe order: usage_claims (for the job's task attempts),
	// task_logs, task_attempts, tasks, steps, then the jobs row.
	// Returns [ErrNotFound] when the job does not exist. The audit_log is left
	// intact (it references entities by id, not by foreign key).
	DeleteJob(ctx context.Context, id string) error

	// DeleteTerminalJobsBefore hard-deletes terminal jobs whose completion time
	// (completed_at, falling back to updated_at when NULL) is before cutoff, and
	// returns a summary of each removed job. completed and canceled jobs are
	// always eligible; failed jobs are included only when includeFailed is true.
	// Active jobs are never removed. Each removed job's children are deleted via
	// the same cascade as [DeleteJob].
	DeleteTerminalJobsBefore(ctx context.Context, cutoff time.Time, includeFailed bool) ([]DeletedJob, error)

	// ParkJob transitions a job to [JobStatusPaused] and records reason in
	// ParkReason, but only while the job is non-terminal (not completed,
	// failed, or canceled). A terminal job is a legitimate no-op: ParkJob
	// returns nil without modifying it. Returns [ErrNotFound] if the job does
	// not exist.
	ParkJob(ctx context.Context, jobID, reason string, now time.Time) error
}

// ListJobsOptions filters and orders [JobStore.ListJobs] results.
// Zero values mean "no filter / use defaults".
type ListJobsOptions struct {
	// Filters
	FarmID  string
	QueueID string
	Status  JobStatus // empty = all statuses
	Owner   string
	Project string
	// Search is a case-insensitive substring matched against name, id, owner,
	// and project. Empty = no search filter.
	Search string

	// Ordering — zero values use JobSortByCreatedAt / SortAsc.
	SortBy  JobSortField
	SortDir SortDir

	// Pagination — call Pagination.Validate() before use.
	Pagination Pagination
}
