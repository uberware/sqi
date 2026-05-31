// SPDX-License-Identifier: AGPL-3.0-only

package openjd

import (
	"errors"
	"fmt"

	"github.com/uberware/sqi/internal/store"
)

// ErrInvalidTransition is returned when a requested status transition is not
// permitted by the task or step state machine.
//
// Use errors.Is to test:
//
//	err := ValidateTaskTransition(from, to)
//	if errors.Is(err, ErrInvalidTransition) { ... }
var ErrInvalidTransition = errors.New("openjd: invalid state transition")

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
//	running  → succeeded  worker reports clean exit (exit code 0)
//	running  → failed     worker reports non-zero exit or fatal error
//	running  → ready      reassignment: running worker became unreachable
//	running  → canceled   task canceled while executing
//
// Terminal states (succeeded, failed, canceled) have no outgoing transitions.

var validTaskTransitions = map[store.TaskStatus]map[store.TaskStatus]struct{}{
	store.TaskStatusPending: {
		store.TaskStatusReady:    {},
		store.TaskStatusCanceled: {},
	},
	store.TaskStatusReady: {
		store.TaskStatusAssigned: {},
		store.TaskStatusCanceled: {},
	},
	store.TaskStatusAssigned: {
		store.TaskStatusRunning:  {},
		store.TaskStatusReady:    {},
		store.TaskStatusCanceled: {},
	},
	store.TaskStatusRunning: {
		store.TaskStatusSucceeded: {},
		store.TaskStatusFailed:    {},
		store.TaskStatusReady:     {},
		store.TaskStatusCanceled:  {},
	},
	// Terminal states — no outgoing transitions.
	store.TaskStatusSucceeded: {},
	store.TaskStatusFailed:    {},
	store.TaskStatusCanceled:  {},
}

// ValidateTaskTransition returns nil if transitioning a task from old to new
// status is permitted by the state machine, or a descriptive error wrapping
// [ErrInvalidTransition] otherwise.
func ValidateTaskTransition(from, to store.TaskStatus) error {
	targets, known := validTaskTransitions[from]
	if !known {
		return fmt.Errorf("%w: unknown task status %q", ErrInvalidTransition, from)
	}
	if _, ok := targets[to]; ok {
		return nil
	}
	return fmt.Errorf("%w: task %q → %q not permitted", ErrInvalidTransition, from, to)
}

// ── Step state machine ────────────────────────────────────────────────────────
//
// Permitted step status transitions:
//
//	pending  → ready      all dependency steps reached completed
//	pending  → canceled   job canceled before dependencies were satisfied
//	ready    → running    at least one task in this step has been assigned
//	ready    → canceled   step canceled before any task ran
//	running  → completed  all tasks in the step succeeded
//	running  → failed     at least one task failed and the step cannot proceed
//	running  → canceled   step canceled while tasks were executing
//
// Terminal states (completed, failed, canceled) have no outgoing transitions.

var validStepTransitions = map[store.StepStatus]map[store.StepStatus]struct{}{
	store.StepStatusPending: {
		store.StepStatusReady:    {},
		store.StepStatusCanceled: {},
	},
	store.StepStatusReady: {
		store.StepStatusRunning:  {},
		store.StepStatusCanceled: {},
	},
	store.StepStatusRunning: {
		store.StepStatusCompleted: {},
		store.StepStatusFailed:    {},
		store.StepStatusCanceled:  {},
	},
	// Terminal states — no outgoing transitions.
	store.StepStatusCompleted: {},
	store.StepStatusFailed:    {},
	store.StepStatusCanceled:  {},
}

// ValidateStepTransition returns nil if transitioning a step from old to new
// status is permitted by the state machine, or a descriptive error wrapping
// [ErrInvalidTransition] otherwise.
func ValidateStepTransition(from, to store.StepStatus) error {
	targets, known := validStepTransitions[from]
	if !known {
		return fmt.Errorf("%w: unknown step status %q", ErrInvalidTransition, from)
	}
	if _, ok := targets[to]; ok {
		return nil
	}
	return fmt.Errorf("%w: step %q → %q not permitted", ErrInvalidTransition, from, to)
}
