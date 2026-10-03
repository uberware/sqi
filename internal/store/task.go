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

// Server-originated failure/termination reasons. [TaskStore.FailureReasonSummary]
// groups by exact string, so every producer of these annotations (scheduler,
// SQLite store, fake store) must share these values — a wording tweak in one
// producer would otherwise split the per-job dominant-reason counts.
const (
	// FailureReasonCanceledByUser annotates tasks canceled by an explicit
	// user/API cancel of the task or its job.
	FailureReasonCanceledByUser = "canceled by user"
	// FailureReasonUpstreamFailed annotates tasks cascade-canceled because an
	// upstream step dependency failed or was canceled.
	FailureReasonUpstreamFailed = "canceled: upstream step failed"
	// FailureReasonWorkerOffline is the attempt message recorded when the
	// heartbeat sweep terminates attempts of a worker that went offline.
	FailureReasonWorkerOffline = "worker went offline"
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

// FailureSummary aggregates a job's failed tasks by [Task.FailureReason],
// for the job-detail failure banner. The zero value ({0 "" 0}) means the job
// has no failed tasks with a recorded reason.
type FailureSummary struct {
	// FailedCount is the total number of failed tasks with a non-empty
	// FailureReason.
	FailedCount int
	// DominantReason is the most frequent FailureReason among failed tasks;
	// ties are broken by reason string ascending for determinism.
	DominantReason string
	// DistinctReasons is the number of distinct FailureReason values.
	DistinctReasons int
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

// AttemptCompletion is a worker's terminal report for one attempt.
type AttemptCompletion struct {
	// AttemptID is the attempt the worker is reporting on.
	AttemptID string
	// TaskID is the task the attempt belongs to.
	TaskID string
	// TaskStatus is the status the task should end up in: succeeded, failed or
	// canceled.
	TaskStatus TaskStatus
	// AttemptStatus is the attempt's terminal status.
	AttemptStatus AttemptStatus
	// ExitCode is the process exit code, or nil when there is none.
	ExitCode *int
	// SessionID is the OpenJD session ID; "" leaves the stored value unchanged.
	SessionID string
	// Message is the attempt's human-readable outcome; "" leaves the stored
	// value unchanged.
	Message string
	// FailureReason, when non-empty, is stamped on the task if the task ends up
	// holding TaskStatus. Callers pass "" for a success.
	FailureReason string
	// EndedAt is when the attempt ended, as the worker reports it. It feeds only
	// the attempt's ended_at: the task row's updated_at and the released claims'
	// released_at are stamped with server time.
	EndedAt time.Time
}

// CompletionResult reports what [TaskStore.CompleteTaskAttempt] did to the
// task. The attempt is closed and its claims released in every non-error case.
type CompletionResult struct {
	// Applied is true when the task now holds the requested status (it moved
	// there, or was already there on a redelivery).
	Applied bool
	// Rejected is true when the state machine refused the transition (the task
	// had already reached a different terminal status). The caller acks it.
	Rejected bool
}

// LeaseOutcome says what [TaskStore.LeaseTask] did. Every outcome other than
// [LeaseLeased] wrote nothing.
type LeaseOutcome string

const (
	// LeaseLeased means the task is now assigned to the worker, with a running
	// attempt and every requested usage claim.
	LeaseLeased LeaseOutcome = "leased"
	// LeaseLost means the task is no longer leasable: it is unknown, another
	// lease took it, it is still backing off, or its job or queue is paused or
	// its job is terminal.
	LeaseLost LeaseOutcome = "lost"
	// LeaseQueueFull means the task's queue is at its MaxConcurrentTasks.
	LeaseQueueFull LeaseOutcome = "queue_full"
	// LeaseFarmFull means the task's farm is at its MaxConcurrentTasks.
	LeaseFarmFull LeaseOutcome = "farm_full"
	// LeasePoolFull means a requested usage pool is at its max_concurrent, or
	// no longer exists. [LeaseResult.FullPool] names it.
	LeasePoolFull LeaseOutcome = "pool_full"
)

// LeaseRequest asks [TaskStore.LeaseTask] to lease one task to one worker.
type LeaseRequest struct {
	// TaskID is the task to lease.
	TaskID string
	// WorkerID is the worker the task is assigned to and the attempt runs on.
	WorkerID string
	// AttemptID is the caller-generated ID of the attempt the lease creates.
	AttemptID string
	// Now stamps the assignment, the attempt's start and the claims, and is
	// the instant a retry backoff is compared against.
	Now time.Time
	// Claims are the usage-pool slots the attempt must hold. ClaimID, PoolID
	// and PoolName are used; MaxConcurrent is ignored, because the pool's cap
	// is read in the lease's own transaction.
	Claims []UsagePoolClaim
}

// LeaseResult is what [TaskStore.LeaseTask] did.
type LeaseResult struct {
	// Outcome is the lease's outcome.
	Outcome LeaseOutcome
	// Attempt is the attempt the lease created. It is set only when Outcome is
	// [LeaseLeased].
	Attempt TaskAttempt
	// FullPool is the name of the full or missing pool when Outcome is
	// [LeasePoolFull].
	FullPool string
}

// TaskStore is the persistence interface for [Task] records.
type TaskStore interface {
	// CreateTask inserts a new task. The caller must populate all fields
	// including a unique ID.
	//
	// It has no production callers — submission writes tasks through
	// [JobStore.CreateJobSubmission]. See [JobStore.CreateJob] for what that
	// means for anyone changing this method or building fixtures with it.
	CreateTask(ctx context.Context, task Task) (Task, error)

	// GetTask returns the task with the given ID, or [ErrNotFound].
	GetTask(ctx context.Context, id string) (Task, error)

	// ListTasks returns a paginated, filtered, and sorted page of tasks
	// matching opts. Call [Pagination.Validate] on opts.Pagination before
	// passing it to ensure sensible defaults are applied.
	ListTasks(ctx context.Context, opts ListTasksOptions) (Page[Task], error)

	// UpdateTaskStatus transitions a task to a new status and updates
	// UpdatedAt. The write is a compare-and-set (invariant I1): it is made only
	// while the task still holds the status it was validated against, so a
	// concurrent writer is never overwritten. Writing the status the task
	// already holds is a no-op, not an error. Returns [ErrNotFound] if the task
	// does not exist and [ErrInvalidTransition] if the state machine refuses the
	// move.
	UpdateTaskStatus(ctx context.Context, id string, status TaskStatus) error

	// CompleteTaskAttempt applies a worker's terminal report in one
	// transaction (invariant I3):
	//  1. close the attempt if it is still running;
	//  2. release every active claim the attempt holds;
	//  3. move the task to c.TaskStatus by compare-and-set;
	//  4. stamp c.FailureReason when the task ends up holding c.TaskStatus.
	// Steps 1 and 2 commit even when step 3 is rejected, so a canceled task's
	// late report never leaks a usage slot. Returns ErrNotFound for an unknown
	// task. A redelivery is safe: the attempt is already closed, so it is not
	// rewritten, and a task already holding c.TaskStatus is a no-op.
	CompleteTaskAttempt(ctx context.Context, c AttemptCompletion) (CompletionResult, error)

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

	// ReclaimStaleAssignedTasks returns tasks stuck in [TaskStatusAssigned] whose
	// assigned_at is older than cutoff to [TaskStatusReady], clearing
	// assigned_worker_id and assigned_at so the scheduler can reassign them.
	// Tasks in [TaskStatusRunning] are left untouched — only assignments that
	// never started are reclaimed (e.g. the assignment message expired from the
	// work stream before the worker pulled it).
	//
	// Inside the same transaction it closes each reclaimed task's running
	// attempts as [AttemptStatusFailed] and releases the claims of the task's
	// closed attempts (invariant I3), so the caller never looks an attempt up
	// afterwards: a task leased again after this call keeps its new attempt and
	// claims. Returns exactly the tasks this call reclaimed (invariant I2), as
	// they are after the reset: [TaskStatusReady] with an empty
	// assigned_worker_id.
	//
	// Statement order (spec 4.1): the tasks are reset first, then their attempts
	// are closed, then their claims released. A Postgres implementation must take
	// its anchors (each candidate task's job row, sorted) BEFORE the UPDATE, by
	// selecting the candidate IDs FOR UPDATE first; an anchor taken after the
	// UPDATE ... RETURNING is the wrong order there.
	ReclaimStaleAssignedTasks(ctx context.Context, cutoff time.Time) ([]Task, error)

	// CountActiveTasksInQueue returns the number of tasks for the given queue
	// that are currently in [TaskStatusAssigned] or [TaskStatusRunning] state.
	// Used by the scheduler's per-queue policy gate.
	CountActiveTasksInQueue(ctx context.Context, queueID string) (int, error)

	// CountActiveTasksInFarm returns the number of tasks across all queues in
	// the given farm that are currently in [TaskStatusAssigned] or
	// [TaskStatusRunning] state. Used by the scheduler's per-farm policy gate.
	CountActiveTasksInFarm(ctx context.Context, farmID string) (int, error)

	// CountReadyTasksByQueue returns the number of LEASABLE ready tasks for
	// each queue within the given farm, keyed by queue ID: tasks in
	// [TaskStatusReady] whose backoff has elapsed (RetryAfter unset or <= now)
	// on an unpaused queue under a schedulable (not paused/terminal) job —
	// the same eligibility predicate as [ListReadyTasks]. Queues with no such
	// tasks are omitted from the map. Used by the scheduler to update the
	// [SchedulerQueueDepth] Prometheus gauge and to wake lease waiters; the
	// filters keep it from waking workers for work nothing can lease (tasks
	// still backing off, or under an auto-parked job).
	CountReadyTasksByQueue(ctx context.Context, farmID string, now time.Time) (map[string]int, error)

	// CancelJobExecution cancels a job's work in one transaction (invariant I3),
	// in this statement order:
	//  1. move every non-terminal task of the job (pending, ready, assigned or
	//     running) to [TaskStatusCanceled], clearing AssignedWorkerID and
	//     AssignedAt, and stamp reason as FailureReason on each task that has
	//     none yet, so a more specific cause recorded earlier (e.g. a
	//     cascade-cancel) is never clobbered;
	//  2. close every running attempt of the job's tasks as
	//     [AttemptStatusCanceled], ended at now;
	//  3. release every active claim held by an attempt of the job's tasks that
	//     is no longer running.
	// It returns the tasks that were in [TaskStatusAssigned] or
	// [TaskStatusRunning] when step 1 ran, each as it was before the cancel (its
	// AssignedWorkerID intact), so the caller can signal the workers. Tasks
	// already terminal are not modified, and a job with nothing left to cancel
	// returns no tasks and no error. now stamps the tasks, the attempts and the
	// claims. The job's own status is not changed; that is
	// [JobStore.CancelJobStatus].
	//
	// Statement order is part of the contract on Postgres: tasks are canceled
	// first, then attempts closed and claims released, so a concurrent LeaseTask
	// holding a task row is waited for and its attempt and claims are seen.
	CancelJobExecution(ctx context.Context, jobID, reason string, now time.Time) ([]Task, error)

	// CancelTaskExecution cancels one task in one transaction (invariant I3),
	// in the same statement order as [TaskStore.CancelJobExecution]: move the
	// task to [TaskStatusCanceled] (stamping reason only if it has none), close
	// its running attempt as [AttemptStatusCanceled], then release the claims
	// of its closed attempts. Unlike the job-wide cancel it leaves
	// AssignedWorkerID and AssignedAt in place, so a canceled task still shows
	// the worker that held it.
	//
	// It returns the task as it was before the cancel, and whether it was
	// canceled. A task that is already terminal, whether it was when the call
	// started or it completed first, is returned unchanged with false and a
	// nil error: that is the "lost the race to completion" outcome, not a
	// failure. An unknown task is [ErrNotFound]. The same statement-order
	// contract applies on Postgres.
	CancelTaskExecution(ctx context.Context, taskID, reason string, now time.Time) (Task, bool, error)

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

	// CountTasksByJob returns the number of tasks for the given job keyed by
	// status. Statuses with zero tasks are omitted from the returned map.
	// Used by the REST layer to include aggregate task counts in job responses.
	CountTasksByJob(ctx context.Context, jobID string) (map[TaskStatus]int, error)

	// CommittedCores returns the sum of CPU-core reservations held by the worker
	// across its assigned and running tasks: Σ COALESCE(required_cores,
	// fullMachineCost). Callers pass the worker's CPUCount as fullMachineCost so
	// an undeclared task (required_cores NULL) counts as the whole machine.
	CommittedCores(ctx context.Context, workerID string, fullMachineCost int) (int, error)

	// LeaseTask leases one ready task to a worker in a single transaction
	// (invariants I3 and I5):
	//  1. move the task ready → assigned, guarded on the same eligibility
	//     predicate as ListReadyTasks (queue not paused, job not paused or
	//     terminal, backoff elapsed);
	//  2. re-check the queue's and farm's MaxConcurrentTasks against values read
	//     in the transaction;
	//  3. insert the attempt, numbered MAX(attempt_number)+1;
	//  4. for each claim, in pool-ID order, re-check the pool's max_concurrent
	//     as read in the transaction (a deleted pool counts as full) and insert
	//     the claim.
	// Any outcome other than LeaseLeased writes nothing at all, so there is no
	// rollback path for the caller. An unknown task is LeaseLost, not an error.
	LeaseTask(ctx context.Context, req LeaseRequest) (LeaseResult, error)

	// SetTaskUnschedulableReason sets (or, with an empty string, clears) the
	// reason a ready task cannot be scheduled. Returns ErrNotFound if id is unknown.
	SetTaskUnschedulableReason(ctx context.Context, id, reason string) error

	// SetTaskFailureReason sets (or, with an empty string, clears) the
	// human-readable reason the task reached a terminal non-success. Returns
	// ErrNotFound if id is unknown.
	SetTaskFailureReason(ctx context.Context, id, reason string) error

	// SetTaskFailureReasonIfEmpty sets the failure reason only when the task
	// currently has no reason recorded (an empty failure_reason). It is a
	// legitimate no-op — not an error — when the task already carries a reason,
	// so a more specific cause (e.g. a cascade-cancel) is never clobbered by a
	// later, less specific one (e.g. a user cancel). A zero-row update (task
	// unknown or already annotated) is NOT reported as ErrNotFound.
	SetTaskFailureReasonIfEmpty(ctx context.Context, id, reason string) error

	// CountUnschedulableTasksByJob returns the number of tasks for the given
	// job that are currently in [TaskStatusReady] with a non-empty
	// UnschedulableReason. Used by the REST layer to surface an
	// "unschedulable" count alongside the per-status task counts in job
	// responses.
	CountUnschedulableTasksByJob(ctx context.Context, jobID string) (int, error)

	// FailureReasonSummary counts the job's [TaskStatusFailed] tasks that
	// carry a non-empty FailureReason, grouped by reason. DominantReason is
	// the most frequent reason; ties are broken by reason string ascending
	// for determinism. A job with no such tasks returns the zero value and a
	// nil error. Used by the REST layer to power the job-detail failure
	// banner.
	FailureReasonSummary(ctx context.Context, jobID string) (FailureSummary, error)

	// RecordTaskFailure records a genuine (worker-reported) failure of a single
	// task ATTEMPT exactly once, tying the counter increment to the attempt's
	// running→failed transition so an at-least-once redelivery of the same
	// status message cannot double-count.
	//
	// In one transaction it closes the attempt as failed (setting ended_at,
	// exit_code, session_id, and message) ONLY when the attempt is still
	// running; on that first close it increments both the task's and its
	// enclosing job's FailedAttempts counters. When the attempt is already
	// terminal (a redelivery), it does NOT increment — it simply returns the
	// CURRENT counters so the caller's retry/park decision is stable across
	// redeliveries. exitCode nil leaves the attempt's exit_code unchanged;
	// sessionID "" leaves the session_id unchanged; message "" leaves the
	// attempt's message unchanged. Returns the authoritative post-call task and
	// job FailedAttempts values, plus firstClose — true iff this call performed
	// the running→failed close (i.e. this is the first delivery, so retry/park
	// ACTIONS are authorized; a redelivery must first check that the attempt is
	// still relevant before re-driving them). The same transaction releases
	// every active usage claim the attempt holds (invariant I3), on a
	// redelivery too. Returns [ErrNotFound] if the task does not exist.
	RecordTaskFailure(ctx context.Context, attemptID, taskID string, exitCode *int, sessionID, message string, now time.Time) (taskFailed, jobFailed int, firstClose bool, err error)

	// RequeueTaskForRetry transitions a task back to [TaskStatusReady],
	// clearing AssignedWorkerID and AssignedAt, and stamps RetryAfter so the
	// task is excluded from [ListReadyTasks] until the backoff elapses.
	//
	// The transition is guarded: only a task currently in [TaskStatusAssigned]
	// or [TaskStatusRunning] is requeued, so a stale or redelivered failure
	// report can never resurrect a task that has since been canceled,
	// succeeded, or already returned to ready. It reports whether the task was
	// actually requeued; false (task missing or not assigned/running) is a
	// legitimate no-op, not an error.
	RequeueTaskForRetry(ctx context.Context, taskID string, retryAfter, now time.Time) (bool, error)
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
