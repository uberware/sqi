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

	// HostRequirements declares the worker capabilities required by this step.
	// Nil means any capable worker is eligible.
	// Populated from the OpenJD hostRequirements block at submission time (task 50).
	HostRequirements *StepHostRequirements

	// ComputeLocation constrains tasks to workers in the named compute location
	// (e.g. "onprem_linux", "cloud_aws_us_east"). Empty means any location is
	// eligible. Mirrors the "attr.worker.computelocation" attribute in
	// HostRequirements and is stored separately for fast SQL-level pre-filtering.
	ComputeLocation string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// ── Host requirements ─────────────────────────────────────────────────────────

// StepHostRequirements is the scheduler-facing representation of a step's
// hostRequirements block, normalised from the raw OpenJD model at submission
// time. The scheduler's matching logic (task 50) reads these fields to
// determine whether a given worker can run a task.
//
// Capability names follow the conventions established in the OpenJD spec and
// documented in [matcher.go]:
//
//   - "attr.worker.os.family"        → worker OS family ("linux", "windows", "darwin")
//   - "attr.worker.os.version"       → worker OS version string
//   - "attr.worker.computelocation"  → worker compute location name
//   - "attr.worker.tag.<key>"        → arbitrary worker tag value
//   - "amount.worker.vcpu"           → worker CPU count
//   - "amount.worker.memory.mb"      → worker RAM in MiB
//   - "amount.worker.gpu.count"      → worker GPU count
//   - "amount.worker.gpu.memory.mb"  → worker GPU VRAM in MiB
//   - "amount.worker.licensepool.<name>" → named license pool (capacity check)
type StepHostRequirements struct {
	// Amounts are quantifiable requirements such as CPUs, RAM, or GPU VRAM.
	// License pool requirements use the "amount.worker.licensepool." prefix and
	// are also listed in LicensePools for quick iteration.
	Amounts []StepAmountRequirement `json:"amounts,omitempty"`

	// Attributes are categorical requirements such as OS family or custom tags.
	// The "attr.worker.computelocation" entry, if present, is mirrored as the
	// enclosing [Step.ComputeLocation] field for SQL-level pre-filtering.
	Attributes []StepAttributeRequirement `json:"attributes,omitempty"`

	// LicensePools lists the names of [LicensePool] records that must have
	// remaining capacity before a task from this step can be assigned.
	// Derived from amounts named "amount.worker.licensepool.<name>" at
	// submission time.
	LicensePools []string `json:"license_pools,omitempty"`
}

// StepAmountRequirement is a single quantifiable host capability requirement.
type StepAmountRequirement struct {
	// Name is the capability name, e.g. "amount.worker.vcpu".
	Name string `json:"name"`
	// Min, if non-nil, is the minimum acceptable value as a decimal string.
	// The scheduler parses it with strconv.ParseFloat for comparison.
	Min *string `json:"min,omitempty"`
	// Max, if non-nil, is the maximum acceptable value as a decimal string.
	Max *string `json:"max,omitempty"`
}

// StepAttributeRequirement is a single categorical host capability requirement.
type StepAttributeRequirement struct {
	// Name is the capability name, e.g. "attr.worker.os.family".
	Name string `json:"name"`
	// AnyOf, if non-empty, requires the host attribute to equal at least one
	// of the listed values.  String comparison is case-insensitive for
	// well-known attributes (os.family, computelocation); exact for custom tags.
	AnyOf []string `json:"any_of,omitempty"`
	// AllOf, if non-empty, requires the host attribute to equal every listed
	// value.  Useful for multi-valued tag semantics (e.g. requiring two
	// software packages to both be installed).
	AllOf []string `json:"all_of,omitempty"`
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
