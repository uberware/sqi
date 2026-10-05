// SPDX-License-Identifier: AGPL-3.0-or-later

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
	// StepStatusRunning is reserved and never written: no store operation moves
	// a step to running, so a step with running tasks stays ready (H4a decision
	// D4). It survives for the wire types and for rows written outside the store
	// operations (test fixtures, through CreateStep or the concrete stores'
	// UpdateStepStatus), which FinalizeStep can still finish.
	StepStatusRunning StepStatus = "running"
	// StepStatusCompleted means all tasks in this step succeeded.
	StepStatusCompleted StepStatus = "completed"
	// StepStatusFailed means one or more tasks failed.
	StepStatusFailed StepStatus = "failed"
	// StepStatusCanceled means the step was canceled. A step reaches it through
	// CancelPendingStep (the cascade from a failed or canceled upstream step),
	// CancelBlockedJob (a blocked job canceled before it ran), or FinalizeStep
	// once all its tasks are terminal and a canceled one is among them with no
	// failed one. A job cancel (CancelJobExecution) also finalizes every open
	// step of the job.
	StepStatusCanceled StepStatus = "canceled"
)

// IsTerminal reports whether s is a terminal step state (completed, failed,
// canceled).
func (s StepStatus) IsTerminal() bool {
	switch s {
	case StepStatusCompleted, StepStatusFailed, StepStatusCanceled:
		return true
	}
	return false
}

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
	// Populated from the OpenJD hostRequirements block at submission time.
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
// time. The scheduler's matching logic reads these fields to
// determine whether a given worker can run a task.
//
// Capability names follow the conventions established in the OpenJD spec and
// documented in [matcher.go]:
//
//   - "attr.worker.os.family"        → worker OS family ("linux", "windows", "macos";
//     a worker reporting GOOS "darwin" matches "macos" — see scheduler.osFamily)
//   - "attr.worker.os.version"       → worker OS version string
//   - "attr.worker.computelocation"  → worker compute location name
//   - "attr.worker.tag.<key>"        → arbitrary worker tag value
//   - "amount.worker.vcpu"           → worker CPU count
//   - "amount.worker.memory.mb"      → worker RAM in MiB
//   - "amount.worker.gpu.count"      → worker GPU count
//   - "amount.worker.gpu.memory.mb"  → worker GPU VRAM in MiB
//   - "amount.worker.usagepool.<name>" → named usage pool (capacity check)
type StepHostRequirements struct {
	// Amounts are quantifiable requirements such as CPUs, RAM, or GPU VRAM.
	// Usage pool requirements use the "amount.worker.usagepool." prefix and
	// are also listed in UsagePools for quick iteration.
	Amounts []StepAmountRequirement `json:"amounts,omitempty"`

	// Attributes are categorical requirements such as OS family or custom tags.
	// The "attr.worker.computelocation" entry, if present, is mirrored as the
	// enclosing [Step.ComputeLocation] field for SQL-level pre-filtering.
	Attributes []StepAttributeRequirement `json:"attributes,omitempty"`

	// UsagePools lists the names of [UsagePool] records that must have
	// remaining capacity before a task from this step can be assigned.
	// Derived from amounts named "amount.worker.usagepool.<name>" at
	// submission time.
	UsagePools []string `json:"usage_pools,omitempty"`
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
	//
	// It has no production callers — submission writes steps through
	// [JobStore.CreateJobSubmission]. See [JobStore.CreateJob] for what that
	// means for anyone changing this method or building fixtures with it.
	CreateStep(ctx context.Context, step Step) (Step, error)

	// GetStep returns the step with the given ID, or [ErrNotFound].
	GetStep(ctx context.Context, id string) (Step, error)

	// ListSteps returns all steps for the given job, ordered by StepOrder
	// ascending.
	ListSteps(ctx context.Context, jobID string) ([]Step, error)

	// FinalizeStep derives the step's terminal status from its tasks and writes
	// it in one statement (invariant I4): failed if any task failed, else
	// canceled if any was canceled, else completed. It returns the step's
	// terminal status and whether THIS call wrote it. While any task is
	// non-terminal it returns ("", false, nil) and writes nothing. A step that
	// is already terminal returns its status with false, so a redelivered
	// completion still drives the caller's idempotent propagation. A step with
	// no tasks completes. Not bounded by MaxLimit. Returns ErrNotFound for an
	// unknown step.
	FinalizeStep(ctx context.Context, id string, now time.Time) (StepStatus, bool, error)

	// ReleaseStep moves a pending step to ready and every one of its pending
	// tasks to ready, in one transaction. Guarded on the step being pending
	// (invariant I1): if it is not, nothing is written and it returns
	// (false, nil, nil). Returns the promoted tasks. ErrNotFound for an unknown
	// step.
	ReleaseStep(ctx context.Context, id string, now time.Time) (bool, []Task, error)

	// CancelPendingStep moves a pending step to canceled and every one of its
	// pending tasks to canceled, stamping reason on tasks that carry none, in
	// one transaction. Guarded on the step being pending and, decided inside
	// the same transaction (invariant I4), on at least one of its upstream
	// steps being failed or canceled; otherwise nothing is written and it
	// returns (false, nil, nil). The second guard is what makes the failure
	// cascade safe against a retry that revives the upstream after the caller
	// read the step list. Returns the canceled tasks. ErrNotFound for an
	// unknown step.
	CancelPendingStep(ctx context.Context, id, reason string, now time.Time) (bool, []Task, error)

	// ListStuckSteps returns every non-terminal step that has at least one task
	// and no non-terminal task, and whose job is not terminal: steps
	// FinalizeStep would finalize but that no future task report will ever
	// trigger. Used once at scheduler start. Ordered by job ID, then step order.
	//
	// The job condition is deliberate. Since H4a2 a job cancel finalizes the
	// job's steps in its own transaction (CancelJobExecution), and migration
	// 00033 repaired the jobs canceled by earlier releases, so a terminal job
	// carries open steps only in a database that skipped that repair; those are
	// the migration's to finalize, not this start-up pass's, which on a healthy
	// farm must stay one query and no writes. The repair target is a live job
	// whose step never finalized (so the job never completed); a terminal job
	// has no downstream that needs its steps finalized, and jobs blocked on it
	// follow its job status. A paused job is live and is listed. A step whose
	// job row is missing is not listed.
	ListStuckSteps(ctx context.Context) ([]Step, error)

	// ListJobIDsWithPendingSteps returns, ascending, the IDs of jobs that are
	// not terminal and not blocked and that have at least one pending step.
	// Used once at scheduler start to re-run dependency resolution, so a step a
	// retry reset to pending but never released (the server stopped between
	// RetryTasks and ResolveDependencies) is released or cascade-canceled
	// (H4a2 §3.5). A blocked job is excluded: its steps wait on another job.
	ListJobIDsWithPendingSteps(ctx context.Context) ([]string, error)
}
