// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"time"
)

// AttemptStatus is the terminal-or-active state of a single task execution.
type AttemptStatus string

const (
	// AttemptStatusRunning means the worker is actively executing the task.
	AttemptStatusRunning AttemptStatus = "running"
	// AttemptStatusSucceeded means the task exited cleanly.
	AttemptStatusSucceeded AttemptStatus = "succeeded"
	// AttemptStatusFailed means the task exited with a non-zero code or the
	// worker reported a fatal error.
	AttemptStatusFailed AttemptStatus = "failed"
	// AttemptStatusCanceled means the attempt was interrupted before completion.
	AttemptStatusCanceled AttemptStatus = "canceled"
)

// TaskAttempt records a single execution of a [Task] on a specific worker.
// A task may have multiple attempts if it is retried after failure.
//
// SessionID is the OpenJD session identifier reported by the worker. Multiple
// task attempts that share the same session ID ran within the same OpenJD
// Session on the same worker — they shared a working directory and environment
// setup. See sqi.md §7.4 for why sessions are not a first-class table.
type TaskAttempt struct {
	ID            string
	TaskID        string
	WorkerID      string
	SessionID     string // OpenJD session ID; may be empty for legacy workers
	AttemptNumber int    // 1-based; incremented on each retry
	Status        AttemptStatus
	ExitCode      *int // nil while running or if the process was signaled
	StartedAt     time.Time
	EndedAt       *time.Time // nil while running
	CreatedAt     time.Time
}

// TaskAttemptStore is the persistence interface for [TaskAttempt] records.
type TaskAttemptStore interface {
	// CreateTaskAttempt inserts a new attempt record. Called when the server
	// assigns a task to a worker and the worker acknowledges it.
	CreateTaskAttempt(ctx context.Context, attempt TaskAttempt) (TaskAttempt, error)

	// GetTaskAttempt returns the attempt with the given ID, or [ErrNotFound].
	GetTaskAttempt(ctx context.Context, id string) (TaskAttempt, error)

	// LatestTaskAttempt returns the attempt with the highest AttemptNumber for
	// the given task, or [ErrNotFound] if no attempts exist yet. Used by the
	// scheduler to determine the correct AttemptNumber when creating a retry.
	LatestTaskAttempt(ctx context.Context, taskID string) (TaskAttempt, error)

	// ListTaskAttempts returns all attempts for the given task, ordered by
	// AttemptNumber ascending.
	ListTaskAttempts(ctx context.Context, taskID string) ([]TaskAttempt, error)

	// UpdateTaskAttempt replaces the mutable fields of an existing attempt
	// (Status, ExitCode, EndedAt). Returns [ErrNotFound] if it does not exist.
	UpdateTaskAttempt(ctx context.Context, attempt TaskAttempt) (TaskAttempt, error)

	// TerminateWorkerAttempts marks all running [TaskAttempt] records for tasks
	// currently assigned to workerID as the given terminal status with the
	// supplied end time. Called by the heartbeat sweep before reclaiming tasks
	// from an offline worker so that each attempt has a closed EndedAt.
	// Returns the number of attempts updated.
	TerminateWorkerAttempts(ctx context.Context, workerID string, status AttemptStatus, endedAt time.Time) (int, error)

	// CancelJobAttempts marks all running [TaskAttempt] records for tasks
	// belonging to the given job as [AttemptStatusCanceled] with the supplied
	// end time (task 54). Should be called before [TaskStore.CancelJobTasks] so
	// that attempts are closed while the tasks still carry their assigned worker.
	// Returns the number of attempts updated.
	CancelJobAttempts(ctx context.Context, jobID string, endedAt time.Time) (int, error)
}
