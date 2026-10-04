// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"errors"
	"fmt"
)

// ErrInvalidTransition is returned when a requested status transition is not
// permitted by the task state machine.
//
// Use errors.Is to test:
//
//	err := ValidateTaskTransition(from, to)
//	if errors.Is(err, ErrInvalidTransition) { ... }
var ErrInvalidTransition = errors.New("store: invalid state transition")

// ── Task state machine ────────────────────────────────────────────────────────
//
// Permitted task status transitions:
//
//	pending  → ready      dependency resolution: all dependency steps completed
//	pending  → canceled   job canceled before step dependencies were satisfied
//	ready    → assigned   scheduler assigns task to a worker
//	ready    → canceled   task canceled while waiting for a worker
//	assigned → running    worker confirms execution has started
//	assigned → ready      reassignment: assigned worker disconnected or timed out
//	assigned → canceled   task canceled after assignment but before confirmation
//	assigned → succeeded  worker's "running" message was dropped (see below)
//	assigned → failed     worker's "running" message was dropped (see below)
//	running  → succeeded  worker reports clean exit (exit code 0)
//	running  → failed     worker reports non-zero exit or fatal error
//	running  → ready      reassignment: running worker became unreachable
//	running  → canceled   task canceled while executing
//
// Terminal states (succeeded, failed, canceled) have no outgoing transitions.
//
// assigned → succeeded/failed look like skipped states but are reachable in
// normal operation: a worker publishes "running" before any terminal status,
// but status.Publisher.publishWithRetry gives up after MaxRetries and returns,
// so that message can be lost for good while the task still runs to completion.
// The terminal message then lands on a row still in 'assigned'. Rejecting it
// would strand finished work until the heartbeat sweep reclaimed it.
//
// A transition from a status to itself is not listed here and is not valid:
// callers that must tolerate duplicate delivery treat same-status writes as a
// no-op before consulting this table (see [TaskStore.UpdateTaskStatus]).
var validTaskTransitions = map[TaskStatus]map[TaskStatus]struct{}{
	TaskStatusPending: {
		TaskStatusReady:    {},
		TaskStatusCanceled: {},
	},
	TaskStatusReady: {
		TaskStatusAssigned: {},
		TaskStatusCanceled: {},
	},
	TaskStatusAssigned: {
		TaskStatusRunning:   {},
		TaskStatusReady:     {},
		TaskStatusCanceled:  {},
		TaskStatusSucceeded: {},
		TaskStatusFailed:    {},
	},
	TaskStatusRunning: {
		TaskStatusSucceeded: {},
		TaskStatusFailed:    {},
		TaskStatusReady:     {},
		TaskStatusCanceled:  {},
	},
	// Terminal states — no outgoing transitions.
	TaskStatusSucceeded: {},
	TaskStatusFailed:    {},
	TaskStatusCanceled:  {},
}

// ValidateTaskTransition returns nil if transitioning a task from one status to
// another is permitted by the state machine, or a descriptive error wrapping
// [ErrInvalidTransition] otherwise.
//
// This lives in package store rather than package openjd because the store is
// what enforces it on every write; openjd imports store, so the store cannot
// import openjd. There is no openjd counterpart to call instead: package openjd
// owns only the STEP machine ([github.com/uberware/sqi/internal/openjd.ValidateStepTransition]),
// with its own separate [ErrInvalidTransition] sentinel — match errors against
// the sentinel from the same package as the machine you called, or errors.Is
// silently stops matching.
func ValidateTaskTransition(from, to TaskStatus) error {
	targets, known := validTaskTransitions[from]
	if !known {
		return fmt.Errorf("%w: unknown task status %q", ErrInvalidTransition, from)
	}
	if _, ok := targets[to]; ok {
		return nil
	}
	return fmt.Errorf("%w: task %q → %q not permitted", ErrInvalidTransition, from, to)
}

// ── Job state machine ─────────────────────────────────────────────────────────

// JobTransitions is the job lifecycle as sqi implements it. It is a TEST-TIME
// SPECIFICATION (H4a, decision D4), not consulted at run time: every job write
// is a named store operation guarded in its own SQL (invariant I1), and the
// transition-table test asserts each operation's from-states are legal here.
//
// The arrows come from the operations themselves:
//
//	pending → running                 PromoteJobRunning, on the first running report
//	pending/running/paused → terminal FinalizeJob, derived from the steps
//	running → pending                 DemoteStalledJobs
//	paused → pending                  ResumeJob, and RetryTasks on an auto-parked job
//	blocked → pending                 ReleaseBlockedJob
//	failed/canceled → pending         RetryTasks
//	any non-terminal → paused         ParkJob (PauseJob for pending and running)
//	any non-terminal → canceled       CancelJobExecution and CancelJobStatus
//	                                  (CancelBlockedJob for blocked)
//
// FinalizeJob's pending → completed is real: a task can reach assigned →
// succeeded with its running report dropped, so the job is never promoted. Its
// SQL guard also admits blocked, but a blocked job's steps are all pending, so
// it never finalizes one and the table has no blocked → terminal arrow besides
// the cancels.
//
// A write to a job's current status is a no-op, not a transition, so no status
// lists itself.
var JobTransitions = map[JobStatus][]JobStatus{
	JobStatusPending:   {JobStatusRunning, JobStatusPaused, JobStatusCompleted, JobStatusFailed, JobStatusCanceled},
	JobStatusRunning:   {JobStatusPending, JobStatusPaused, JobStatusCompleted, JobStatusFailed, JobStatusCanceled},
	JobStatusPaused:    {JobStatusPending, JobStatusCompleted, JobStatusFailed, JobStatusCanceled},
	JobStatusBlocked:   {JobStatusPending, JobStatusPaused, JobStatusCanceled},
	JobStatusCompleted: {},
	JobStatusFailed:    {JobStatusPending},
	JobStatusCanceled:  {JobStatusPending},
}
