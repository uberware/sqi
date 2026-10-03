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

// validStepTransitions is the step lifecycle sqi actually implements. It is a
// TEST-TIME SPECIFICATION (H4a, decision D4): no runtime path consults it.
// Every step write is a named store operation guarded in its own SQL
// (invariant I1), and TestStepOperations_FromStatesAreLegal (in this package's
// statemachine_test.go) asserts each operation's from-states are legal here.
//
//	pending  → ready      ReleaseStep (dependencies satisfied)
//	pending  → canceled   CancelPendingStep / CancelBlockedJob
//	ready    → completed  FinalizeStep
//	ready    → failed     FinalizeStep
//	ready    → canceled   FinalizeStep (a task was canceled) / CancelBlockedJob
//	failed   → pending    RetryTasks
//	canceled → pending    RetryTasks
//
// There is no running status: nothing writes StepStatusRunning, which
// survives in the enum and wire types only. Making it real is a separate,
// user-visible feature, not a correctness fix. The running row stays, because
// old databases and test fixtures hold running steps that FinalizeStep must be
// able to finish.
//
// Completed is terminal: it has no outgoing transition, and RetryTasks never
// reopens a completed step.
var validStepTransitions = map[store.StepStatus]map[store.StepStatus]struct{}{
	store.StepStatusPending: {store.StepStatusReady: {}, store.StepStatusCanceled: {}},
	store.StepStatusReady: {
		store.StepStatusCompleted: {}, store.StepStatusFailed: {}, store.StepStatusCanceled: {},
	},
	store.StepStatusRunning:   {store.StepStatusCompleted: {}, store.StepStatusFailed: {}, store.StepStatusCanceled: {}},
	store.StepStatusCompleted: {},
	store.StepStatusFailed:    {store.StepStatusPending: {}},
	store.StepStatusCanceled:  {store.StepStatusPending: {}},
}

// ValidateStepTransition returns nil if transitioning a step from old to new
// status is permitted by the step lifecycle table, or a descriptive error
// wrapping [ErrInvalidTransition] otherwise.
//
// No runtime path calls it: step writes are guarded in SQL, not checked here.
// It is the query side of the test-time specification in validStepTransitions.
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
