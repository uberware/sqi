// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd

import (
	"errors"
	"fmt"

	"github.com/uberware/sqi/internal/store"
)

// ErrInvalidTransition is returned when a requested step status transition is
// not permitted by the step state machine.
//
// Use errors.Is to test:
//
//	err := ValidateStepTransition(from, to)
//	if errors.Is(err, ErrInvalidTransition) { ... }
//
// The task state machine lives in package store, which enforces it on every
// status write, and carries its own [store.ErrInvalidTransition].
var ErrInvalidTransition = errors.New("openjd: invalid state transition")

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
