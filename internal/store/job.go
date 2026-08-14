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
	// JobStatusBlocked means the job is waiting on one or more cross-job
	// dependencies (other whole jobs) to complete. Its tasks are held pending
	// and never leased until every upstream job reaches JobStatusCompleted, at
	// which point it is released to JobStatusPending. If any upstream fails, is
	// canceled, or is deleted, the blocked job is canceled (upstream-failed).
	JobStatusBlocked JobStatus = "blocked"
	// JobStatusCompleted means all tasks across all steps succeeded.
	JobStatusCompleted JobStatus = "completed"
	// JobStatusFailed means one or more tasks failed and the job cannot proceed.
	JobStatusFailed JobStatus = "failed"
	// JobStatusCanceled means the job was explicitly canceled by a user or API
	// call before it could complete.
	JobStatusCanceled JobStatus = "canceled"
)

// IsTerminal reports whether s is a terminal job state (completed, failed,
// canceled). A terminal job cannot be modified and its tasks are never
// eligible for assignment.
func (s JobStatus) IsTerminal() bool {
	switch s {
	case JobStatusCompleted, JobStatusFailed, JobStatusCanceled:
		return true
	}
	return false
}

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
	// DependsOn holds the IDs of the upstream jobs this job waits on (whole-job
	// cross-job dependencies). Populated by GetJob; ListJobs leaves it nil to
	// avoid N+1 queries. Empty for jobs with no cross-job dependencies.
	DependsOn []string
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

// JobSubmission is everything one job submission creates.
//
// It exists so a job, its dependency edges, its steps and its tasks are
// created together or not at all — see [JobStore.CreateJobSubmission].
type JobSubmission struct {
	Job       Job
	DependsOn []string
	Steps     []Step
	Tasks     []Task
}

// JobStore is the persistence interface for [Job] records.
type JobStore interface {
	// CreateJob inserts a new job with all fields populated by the caller.
	//
	// It has NO production callers. Submission was its only one and now goes
	// through [JobStore.CreateJobSubmission]; the same is true of
	// [JobStore.CreateJobDependencies], [StepStore.CreateStep] and
	// [TaskStore.CreateTask]. All four are test-only API surface kept for
	// fixture construction, with two consequences worth knowing:
	//
	//   - No production test exercises them, so they can drift from the path
	//     production actually takes without anything going red. The fake's
	//     CreateStep and CreateTask already differ: they do not stamp
	//     CreatedAt/UpdatedAt, while its CreateJobSubmission does (per row, so
	//     tasks within a step get distinct created_at values — SQLite relies on
	//     that for the ready-task ordering tiebreaker and ListTasks paging).
	//     A fixture built from these creators therefore has zero timestamps
	//     where a real submission has meaningful ones.
	//   - A behavior change made here does not reach production. Change
	//     CreateJobSubmission too, or the change is cosmetic.
	CreateJob(ctx context.Context, job Job) (Job, error)

	// CreateJobSubmission atomically creates a job, its dependency edges, its
	// steps and its tasks. On ANY error nothing is written.
	//
	// It exists because creating those rows through separate calls left two
	// defects with no cure at the call site: a failed submission stranded a
	// pending job that no sweep reaps, and a submission whose write failed
	// after some steps were persisted produced a job whose missing steps made
	// checkJobCompletion — which derives job status from the steps that exist —
	// report it completed. The second needs a STORE failure specifically: an
	// expansion failure left the step row too, because the old code wrote it
	// before expanding its tasks, so that case hung pending rather than
	// completing. Both are properties of partial creation, so both end here.
	//
	// The returned JobSubmission carries the rows as stored, the way
	// [JobStore.CreateJob], [StepStore.CreateStep] and [TaskStore.CreateTask]
	// each return theirs. Read the edges back from its DependsOn field, not
	// from its Job.DependsOn: the returned [Job] is scanned straight from the
	// insert, which does not join the edge table, so Job.DependsOn is
	// backend-dependent and must not be relied on. Only [JobStore.GetJob]
	// populates it.
	CreateJobSubmission(ctx context.Context, sub JobSubmission) (JobSubmission, error)

	// GetJob returns the job with the given ID, or [ErrNotFound].
	GetJob(ctx context.Context, id string) (Job, error)

	// CreateJobDependencies records that jobID waits on each ID in upstreamIDs
	// (whole-job cross-job dependencies). Duplicate edges are ignored.
	//
	// Submission no longer calls this: the edges are written by
	// [JobStore.CreateJobSubmission], in the same transaction as the job row
	// whose blocked status they justify. See [JobStore.CreateJob] on what that
	// leaves this method.
	CreateJobDependencies(ctx context.Context, jobID string, upstreamIDs []string) error

	// ListJobDependencyIDs returns the IDs of the upstream jobs jobID waits on,
	// ordered by upstream job ID. Some IDs may reference jobs that have since
	// been deleted — the caller (the reconciler) treats a missing upstream as
	// unsatisfiable. Returns an empty slice when there are no dependencies.
	ListJobDependencyIDs(ctx context.Context, jobID string) ([]string, error)

	// ListDependents returns the IDs of jobs that declared a dependency on
	// upstreamJobID (i.e. jobs waiting for it), ordered by dependent job ID.
	// Returns an empty slice when none.
	ListDependents(ctx context.Context, upstreamJobID string) ([]string, error)

	// ListBlockedJobs returns every job currently in JobStatusBlocked, for the
	// scheduler's periodic reconciliation sweep.
	ListBlockedJobs(ctx context.Context) ([]Job, error)

	// ListJobs returns a paginated, filtered, and sorted page of jobs matching
	// opts. Call [Pagination.Validate] on opts.Pagination before passing it to
	// ensure sensible defaults are applied.
	ListJobs(ctx context.Context, opts ListJobsOptions) (Page[Job], error)

	// UpdateJob replaces the mutable user-settable fields of an existing job
	// (farm_id, queue_id, name, owner, submitter, priority, project,
	// raw_template, template_format, max_attempts, retry_delay_seconds,
	// failure_limit) and updates UpdatedAt.
	//
	// status, started_at, completed_at, failed_attempts, and park_reason are
	// lifecycle columns and are intentionally excluded — use [UpdateJobStatus],
	// [CancelJobStatus], or the scheduler's failure-limit sweep for those. The
	// returned Job reflects the current DB state of all columns.
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
	// task_logs, task_attempts, tasks, steps, the job's own job_dependencies
	// rows (its outgoing edges), then the jobs row. Incoming edges — rows where
	// the deleted job is the depends_on_job_id of some other job — are
	// deliberately left intact for the reconciler to observe as "upstream
	// deleted". Returns [ErrNotFound] when the job does not exist. The
	// audit_log is left intact (it references entities by id, not by foreign
	// key).
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

	// ResumeJob transitions a paused job back to [JobStatusPending] — the
	// inverse of [ParkJob] and of a manual pause. An auto-parked job
	// (non-empty ParkReason) additionally has its ParkReason cleared and its
	// FailedAttempts reset to zero, re-arming the failure limit; without the
	// reset the very next genuine failure would immediately re-park the job.
	// A manually paused job (empty ParkReason) keeps its FailedAttempts.
	// A job that is no longer paused is a legitimate no-op: ResumeJob returns
	// nil without modifying it. Returns [ErrNotFound] if the job does not
	// exist.
	ResumeJob(ctx context.Context, jobID string, now time.Time) error
}

// ListJobsOptions filters and orders [JobStore.ListJobs] results.
// Zero values mean "no filter / use defaults".
type ListJobsOptions struct {
	// Filters
	FarmID  string
	QueueID string
	Status  JobStatus // empty = all statuses
	// Owner filters by job owner, compared case-insensitively. Empty = no
	// filter.
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
