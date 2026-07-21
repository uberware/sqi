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
// import openjd. [github.com/uberware/sqi/internal/openjd.ValidateTaskTransition]
// delegates here.
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
