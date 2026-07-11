// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"time"
)

// TaskStatus is the lifecycle state of an individual task.
type TaskStatus string

const (
	// TaskStatusPending means the task exists but its step's dependencies have
	// not yet been satisfied.
	TaskStatusPending TaskStatus = "pending"
	// TaskStatusReady means the task is eligible for assignment; its step's
	// dependencies have completed.
	TaskStatusReady TaskStatus = "ready"
	// TaskStatusAssigned means the task has been assigned to a worker but the
	// worker has not yet confirmed it is running.
	TaskStatusAssigned TaskStatus = "assigned"
	// TaskStatusRunning means the worker has confirmed the task is executing.
	TaskStatusRunning TaskStatus = "running"
	// TaskStatusSucceeded means the task completed successfully.
	TaskStatusSucceeded TaskStatus = "succeeded"
	// TaskStatusFailed means the task exited with a non-zero code or the
	// worker reported a fatal error.
	TaskStatusFailed TaskStatus = "failed"
	// TaskStatusCanceled means the task was explicitly canceled before it could
	// complete.
	TaskStatusCanceled TaskStatus = "canceled"
)

// Task is the atomic unit of work — one process on one worker. Tasks are
// derived from an OpenJD step's parameter space expansion.
type Task struct {
	ID               string
	JobID            string // denormalized from Step for query efficiency
	StepID           string
	Name             string
	Parameters       map[string]string // resolved parameter values for this task
	Status           TaskStatus
	AssignedWorkerID string     // empty when unassigned
	AssignedAt       *time.Time // nil when unassigned
	CreatedAt        time.Time
	UpdatedAt        time.Time

	// RequiredCores is the task's declared CPU reservation (OpenJD
	// amount.worker.vcpu min). Nil means undeclared — the scheduler treats the
	// cost as the running worker's full CPUCount (one such task per worker).
	RequiredCores *int

	// UnschedulableReason is set when a ready task cannot be satisfied by any
	// online worker (empty = schedulable). An annotation on the task, not a
	// status.
	UnschedulableReason string

	// FailureReason is the human-readable reason the task reached a terminal
	// non-success (failed or canceled). Empty for non-terminal tasks; cleared
	// on retry. A denormalized copy of the latest terminal attempt's reason.
	FailureReason string

	// FailedAttempts counts attempts that GENUINELY ran and failed
	// (worker-reported "failed"). Lost/reclaimed attempts never increment it.
	FailedAttempts int

	// RetryAfter, when non-nil and in the future, holds the task in the ready
	// queue until the backoff delay elapses. Nil means immediately eligible.
	RetryAfter *time.Time
}

// TaskSortField is a column by which [TaskStore.ListTasks] results can be ordered.
type TaskSortField string

const (
	// TaskSortByCreatedAt orders tasks by creation time (default).
	TaskSortByCreatedAt TaskSortField = "created_at"
	// TaskSortByStatus orders tasks alphabetically by status string.
	TaskSortByStatus TaskSortField = "status"
	// TaskSortByUpdatedAt orders tasks by the time of the most recent change.
	TaskSortByUpdatedAt TaskSortField = "updated_at"
	// TaskSortByName orders tasks alphabetically by name.
	TaskSortByName TaskSortField = "name"
)

// TaskStore is the persistence interface for [Task] records.
type TaskStore interface {
	// CreateTask inserts a new task. The caller must populate all fields
	// including a unique ID.
	CreateTask(ctx context.Context, task Task) (Task, error)

	// GetTask returns the task with the given ID, or [ErrNotFound].
	GetTask(ctx context.Context, id string) (Task, error)

	// ListTasks returns a paginated, filtered, and sorted page of tasks
	// matching opts. Call [Pagination.Validate] on opts.Pagination before
	// passing it to ensure sensible defaults are applied.
	ListTasks(ctx context.Context, opts ListTasksOptions) (Page[Task], error)

	// UpdateTaskStatus transitions a task to a new status and updates
	// UpdatedAt. Returns [ErrNotFound] if the task does not exist.
	UpdateTaskStatus(ctx context.Context, id string, status TaskStatus) error

	// AssignTask atomically sets AssignedWorkerID, AssignedAt, and Status to
	// [TaskStatusAssigned] for the given task. Returns [ErrNotFound] if the
	// task does not exist.
	AssignTask(ctx context.Context, id, workerID string, assignedAt time.Time) error

	// ListReadyTasks returns up to limit tasks in [TaskStatusReady] that
	// belong to non-paused queues within the given farm, excluding:
	//   - tasks whose RetryAfter is set and after now (still backing off), and
	//   - tasks whose job is paused or in a terminal status (completed,
	//     failed, canceled),
	// ordered by:
	//   1. job priority descending (higher values first),
	//   2. job submission time ascending (earlier jobs win ties),
	//   3. step order ascending (earlier steps in a job run before later ones),
	//   4. task creation time ascending (stable tiebreaker within a step).
	//
	// Used by the scheduler's assignment loop.
	ListReadyTasks(ctx context.Context, farmID string, now time.Time, limit int) ([]Task, error)

	// ReclaimWorkerTasks resets all tasks assigned to workerID that are still
	// in [TaskStatusAssigned] or [TaskStatusRunning] back to [TaskStatusReady]
	// so they can be reassigned by the scheduler. Called by the heartbeat
	// timeout sweep after a worker is marked offline.
	// Returns the number of tasks reclaimed.
	ReclaimWorkerTasks(ctx context.Context, workerID string) (int, error)

	// ReclaimStaleAssignedTasks returns tasks stuck in [TaskStatusAssigned] whose
	// assigned_at is older than cutoff to [TaskStatusReady], clearing
	// assigned_worker_id and assigned_at so the scheduler can reassign them.
	// Tasks in [TaskStatusRunning] are left untouched — only assignments that
	// never started are reclaimed (e.g. the assignment message expired from the
	// work stream before the worker pulled it). Returns the reclaimed tasks,
	// each still carrying its pre-reset assigned_worker_id, so the caller can
	// close their attempts and release usage-pool claims.
	ReclaimStaleAssignedTasks(ctx context.Context, cutoff time.Time) ([]Task, error)

	// CountActiveTasksInQueue returns the number of tasks for the given queue
	// that are currently in [TaskStatusAssigned] or [TaskStatusRunning] state.
	// Used by the scheduler's per-queue policy gate.
	CountActiveTasksInQueue(ctx context.Context, queueID string) (int, error)

	// CountActiveTasksInFarm returns the number of tasks across all queues in
	// the given farm that are currently in [TaskStatusAssigned] or
	// [TaskStatusRunning] state. Used by the scheduler's per-farm policy gate.
	CountActiveTasksInFarm(ctx context.Context, farmID string) (int, error)

	// CountReadyTasksByQueue returns the number of tasks in [TaskStatusReady]
	// state for each queue within the given farm, keyed by queue ID.
	// Queues with no ready tasks are omitted from the map.
	// Used by the scheduler to update the [SchedulerQueueDepth] Prometheus
	// gauge.
	CountReadyTasksByQueue(ctx context.Context, farmID string) (map[string]int, error)

	// CancelJobTasks transitions all non-terminal tasks for the given job to
	// [TaskStatusCanceled], clearing AssignedWorkerID and AssignedAt on each,
	// and returns the subset that were in [TaskStatusAssigned] or
	// [TaskStatusRunning] at the time of the call (with their AssignedWorkerID
	// intact) so the caller can publish NATS cancel signals to the appropriate
	// workers.
	//
	// The SELECT and UPDATE run inside a single database transaction so no
	// concurrent assignment can race between observation and cancellation.
	// Tasks already in a terminal state (succeeded, failed, canceled) are not
	// modified.
	CancelJobTasks(ctx context.Context, jobID string, now time.Time) ([]Task, error)

	// RetryTasks revives failed/canceled tasks so they can run again. It
	// transitions every task of jobID in [TaskStatusFailed] or
	// [TaskStatusCanceled] — or, when taskIDs is non-nil, only those of the
	// given IDs that are failed/canceled — back to [TaskStatusPending],
	// clearing each revived task's genuine-failure state (FailedAttempts reset
	// to zero, RetryAfter cleared). Any of their enclosing steps that are
	// currently in a terminal status are reset to [StepStatusPending], and the
	// job itself is reset to [JobStatusPending] when it is currently terminal
	// (failed/canceled) — likewise clearing the job's FailedAttempts and
	// ParkReason; a non-terminal job is left unchanged. All updates run in a
	// single transaction.
	//
	// Resetting to pending (rather than ready) lets the caller re-run
	// [openjd.ResolveDependencies] to re-gate the revived tasks in dependency
	// order. Tasks not in a terminal-retryable state are not modified. Returns
	// the revived task rows (each with Status == pending), or an empty slice
	// when nothing matched.
	RetryTasks(ctx context.Context, jobID string, taskIDs []string, now time.Time) ([]Task, error)

	// TransitionStepPendingTasks transitions every task of the given step that is
	// currently in [TaskStatusPending] to status `to`, updates UpdatedAt, and
	// returns the affected task rows. It is used to promote a step's tasks to
	// [TaskStatusReady] once its dependencies resolve, and to cancel them when an
	// upstream dependency fails.
	//
	// The transition is applied as a single statement covering all matching rows
	// regardless of count, so it is not subject to the [MaxLimit] pagination
	// ceiling. Tasks not in pending state are not modified.
	TransitionStepPendingTasks(ctx context.Context, stepID string, to TaskStatus) ([]Task, error)

	// CountTasksByJob returns the number of tasks for the given job keyed by
	// status. Statuses with zero tasks are omitted from the returned map.
	// Used by the REST layer to include aggregate task counts in job responses.
	CountTasksByJob(ctx context.Context, jobID string) (map[TaskStatus]int, error)

	// CommittedCores returns the sum of CPU-core reservations held by the worker
	// across its assigned and running tasks: Σ COALESCE(required_cores,
	// fullMachineCost). Callers pass the worker's CPUCount as fullMachineCost so
	// an undeclared task (required_cores NULL) counts as the whole machine.
	CommittedCores(ctx context.Context, workerID string, fullMachineCost int) (int, error)

	// LeaseReadyTask atomically transitions a task from [TaskStatusReady] to
	// [TaskStatusAssigned], setting assigned_worker_id and assigned_at. It
	// returns true iff the task was still ready (exactly one row changed); a
	// false return means another worker leased it first. This is the race guard
	// for concurrent lease requests.
	LeaseReadyTask(ctx context.Context, taskID, workerID string, now time.Time) (bool, error)

	// SetTaskUnschedulableReason sets (or, with an empty string, clears) the
	// reason a ready task cannot be scheduled. Returns ErrNotFound if id is unknown.
	SetTaskUnschedulableReason(ctx context.Context, id, reason string) error

	// SetTaskFailureReason sets (or, with an empty string, clears) the
	// human-readable reason the task reached a terminal non-success. Returns
	// ErrNotFound if id is unknown.
	SetTaskFailureReason(ctx context.Context, id, reason string) error

	// CountUnschedulableTasksByJob returns the number of tasks for the given
	// job that are currently in [TaskStatusReady] with a non-empty
	// UnschedulableReason. Used by the REST layer to surface an
	// "unschedulable" count alongside the per-status task counts in job
	// responses.
	CountUnschedulableTasksByJob(ctx context.Context, jobID string) (int, error)

	// RecordTaskFailure records a genuine (worker-reported) failure of a single
	// task ATTEMPT exactly once, tying the counter increment to the attempt's
	// running→failed transition so an at-least-once redelivery of the same
	// status message cannot double-count.
	//
	// In one transaction it closes the attempt as failed (setting ended_at,
	// exit_code, and session_id) ONLY when the attempt is still running; on that
	// first close it increments both the task's and its enclosing job's
	// FailedAttempts counters. When the attempt is already terminal (a
	// redelivery), it does NOT increment — it simply returns the CURRENT
	// counters so the caller's retry/park decision is stable across
	// redeliveries. exitCode nil leaves the attempt's exit_code unchanged;
	// sessionID "" leaves the session_id unchanged. Returns the authoritative
	// post-call task and job FailedAttempts values. Returns [ErrNotFound] if the
	// task does not exist.
	RecordTaskFailure(ctx context.Context, attemptID, taskID string, exitCode *int, sessionID string, now time.Time) (taskFailed, jobFailed int, err error)

	// RequeueTaskForRetry transitions a task back to [TaskStatusReady],
	// clearing AssignedWorkerID and AssignedAt, and stamps RetryAfter so the
	// task is excluded from [ListReadyTasks] until the backoff elapses.
	// Returns [ErrNotFound] if the task does not exist.
	RequeueTaskForRetry(ctx context.Context, taskID string, retryAfter, now time.Time) error
}

// ListTasksOptions filters and orders [TaskStore.ListTasks] results.
// Zero values mean "no filter / use defaults".
type ListTasksOptions struct {
	// Filters
	JobID    string
	StepID   string
	Status   TaskStatus   // empty = all statuses (mutually exclusive with Statuses)
	Statuses []TaskStatus // IN-filter; takes precedence over Status when non-empty
	WorkerID string       // filter by assigned worker

	// Ordering — zero values use TaskSortByCreatedAt / SortAsc.
	SortBy  TaskSortField
	SortDir SortDir

	// Pagination — call Pagination.Validate() before use.
	Pagination Pagination
}
