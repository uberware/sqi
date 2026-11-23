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
//	assigned → succeeded  no "running" preceded the terminal status (see below)
//	assigned → failed     no "running" preceded the terminal status (see below)
//	running  → succeeded  worker reports clean exit (exit code 0)
//	running  → failed     worker reports non-zero exit or fatal error
//	running  → ready      reassignment: running worker became unreachable
//	running  → canceled   task canceled while executing
//
// Terminal states (succeeded, failed, canceled) have no outgoing transitions.
//
// assigned → succeeded/failed look like skipped states but are reachable in
// normal operation: a worker that starts the task's process publishes
// "running" before the terminal status, but that message can still be missing
// when the terminal one is stored: the worker retries a first "running" in
// the background until the stream acknowledges it, and drops one still pending
// when the task's terminal status is ready to send. A task that ends before
// its process starts (a refused or failed dispatch) sends no "running" at all.
// The terminal message then lands on a row still in 'assigned'. Rejecting it
// would strand finished work until the heartbeat sweep reclaimed it.
//
// A transition from a status to itself is not listed here and is not valid:
// callers that must tolerate duplicate delivery treat same-status writes as a
// no-op before consulting this table (see [TaskStore.CompleteTaskAttempt]).
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
// It lives in package store because [TaskStore.CompleteTaskAttempt] enforces
// it, and openjd imports store, so store cannot import openjd. Package openjd
// owns only the step machine
// ([github.com/uberware/sqi/internal/openjd.ValidateStepTransition]), with its
// own [ErrInvalidTransition] sentinel: match an error against the sentinel of
// the package whose machine returned it, or errors.Is does not match.
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

// JobTransitions is the job lifecycle as sqi implements it. It is a test-time
// specification, not consulted at run time: every job write is a named store
// operation guarded in its own SQL (invariant I1), and the transition-table
// test asserts each operation's from-states are legal here.
//
// The arrows come from the operations themselves:
//
//	pending → running                 PromoteJobRunning, on the first running report
//	pending/running/paused → terminal FinalizeJob, derived from the steps
//	running → pending                 DemoteStalledJobs
//	paused → pending                  ResumeJob, and RetryTasks on an auto-parked job
//	paused → blocked                  ResumeJob, for a job still holding its dependencies
//	blocked → pending                 ReleaseBlockedJob
//	failed/canceled → pending         RetryTasks
//	any non-terminal → paused         ParkJob (PauseJob for pending, running and blocked)
//	any non-terminal → canceled       CancelJobExecution and CancelJobStatus
//	                                  (CancelBlockedJob for blocked)
//
// FinalizeJob's pending → completed is real: a task can reach assigned →
// succeeded with its running report dropped, so the job is never promoted. Its
// SQL guard also admits blocked, and a blocked job can be finalized: canceling
// the only task of its only step (CancelTask) finalizes that step pending →
// canceled, and FinalizeJob then moves the job blocked → canceled, an arrow the
// table already lists. blocked → completed and blocked → failed cannot happen,
// because a blocked job's tasks are never leased, so none of them succeeds or
// fails.
//
// A write to a job's current status is a no-op, not a transition, so no status
// lists itself.
var JobTransitions = map[JobStatus][]JobStatus{
	JobStatusPending:   {JobStatusRunning, JobStatusPaused, JobStatusCompleted, JobStatusFailed, JobStatusCanceled},
	JobStatusRunning:   {JobStatusPending, JobStatusPaused, JobStatusCompleted, JobStatusFailed, JobStatusCanceled},
	JobStatusPaused:    {JobStatusPending, JobStatusBlocked, JobStatusCompleted, JobStatusFailed, JobStatusCanceled},
	JobStatusBlocked:   {JobStatusPending, JobStatusPaused, JobStatusCanceled},
	JobStatusCompleted: {},
	JobStatusFailed:    {JobStatusPending},
	JobStatusCanceled:  {JobStatusPending},
}
