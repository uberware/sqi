// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"time"
)

// StepStatus is the lifecycle state of a step within a job.
type StepStatus string

const (
	// StepStatusPending means the step is waiting for its dependencies to complete.
	StepStatusPending StepStatus = "pending"
	// StepStatusReady means all dependencies have succeeded; tasks can be scheduled.
	StepStatusReady StepStatus = "ready"
	// StepStatusRunning means at least one task in this step is running.
	StepStatusRunning StepStatus = "running"
	// StepStatusCompleted means all tasks in this step succeeded.
	StepStatusCompleted StepStatus = "completed"
	// StepStatusFailed means one or more tasks failed.
	StepStatusFailed StepStatus = "failed"
	// StepStatusCanceled means the step was canceled, typically because the
	// parent job was canceled.
	StepStatusCanceled StepStatus = "canceled"
)

// Step is one stage within a [Job]. Steps may depend on other steps; a step's
// tasks are not scheduled until all its dependencies have reached
// [StepStatusCompleted].
type Step struct {
	ID        string
	JobID     string
	Name      string
	DependsOn []string // names of steps that must complete before this one
	StepOrder int      // position within the job for deterministic ordering
	Status    StepStatus
	CreatedAt time.Time
	UpdatedAt time.Time
}

// StepStore is the persistence interface for [Step] records.
type StepStore interface {
	// CreateStep inserts a new step. The (JobID, Name) pair must be unique
	// within the job; returns [ErrConflict] if violated.
	CreateStep(ctx context.Context, step Step) (Step, error)

	// GetStep returns the step with the given ID, or [ErrNotFound].
	GetStep(ctx context.Context, id string) (Step, error)

	// ListSteps returns all steps for the given job, ordered by StepOrder
	// ascending.
	ListSteps(ctx context.Context, jobID string) ([]Step, error)

	// UpdateStepStatus transitions a step to a new status and updates
	// UpdatedAt. Returns [ErrNotFound] if the step does not exist.
	UpdateStepStatus(ctx context.Context, id string, status StepStatus) error
}
