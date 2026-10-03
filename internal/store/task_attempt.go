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
// setup. See docs/architecture.md ("SQLite schema overview") for why sessions
// are not a first-class table.
type TaskAttempt struct {
	ID            string
	TaskID        string
	WorkerID      string
	SessionID     string // OpenJD session ID; may be empty for legacy workers
	AttemptNumber int    // 1-based; incremented on each retry
	Status        AttemptStatus
	ExitCode      *int // nil while running or if the process was signaled
	// Message is the human-readable reason for a terminal attempt (worker- or
	// server-supplied); empty while running or on success.
	Message   string
	StartedAt time.Time
	EndedAt   *time.Time // nil while running
	CreatedAt time.Time
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
	// (Status, ExitCode, EndedAt; SessionID and Message only when non-empty).
	// It writes only while the attempt is still running, evaluated inside the
	// write, so a late or echoed report can never overwrite an attempt that
	// something else already closed (F16). Returns [ErrConflict] when the
	// attempt exists but is closed, and [ErrNotFound] if it does not exist.
	UpdateTaskAttempt(ctx context.Context, attempt TaskAttempt) (TaskAttempt, error)
}
