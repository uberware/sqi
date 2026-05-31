// SPDX-License-Identifier: AGPL-3.0-only

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
// derived from an OpenJD step's parameter space expansion (task 42).
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
	// belong to non-paused queues within the given farm, ordered by job
	// priority descending then CreatedAt ascending. Used by the scheduler's
	// assignment loop (task 46).
	ListReadyTasks(ctx context.Context, farmID string, limit int) ([]Task, error)

	// ReclaimWorkerTasks resets all tasks assigned to workerID that are still
	// in [TaskStatusAssigned] or [TaskStatusRunning] back to [TaskStatusReady]
	// so they can be reassigned by the scheduler. Called by the heartbeat
	// timeout sweep (task 48) after a worker is marked offline.
	// Returns the number of tasks reclaimed.
	ReclaimWorkerTasks(ctx context.Context, workerID string) (int, error)
}

// ListTasksOptions filters and orders [TaskStore.ListTasks] results.
// Zero values mean "no filter / use defaults".
type ListTasksOptions struct {
	// Filters
	JobID    string
	StepID   string
	Status   TaskStatus // empty = all statuses
	WorkerID string     // filter by assigned worker

	// Ordering — zero values use TaskSortByCreatedAt / SortAsc.
	SortBy  TaskSortField
	SortDir SortDir

	// Pagination — call Pagination.Validate() before use.
	Pagination Pagination
}
