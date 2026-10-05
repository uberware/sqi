// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

// Tests for statemachine.go.
//
// Covers all legal and illegal task/step transitions via table-driven tests.

import (
	"errors"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
)

// ── Step transitions ──────────────────────────────────────────────────────────

func TestValidateStepTransition(t *testing.T) {
	legal := []struct {
		from store.StepStatus
		to   store.StepStatus
	}{
		{store.StepStatusPending, store.StepStatusReady},
		{store.StepStatusPending, store.StepStatusCanceled},
		{store.StepStatusReady, store.StepStatusCompleted},
		{store.StepStatusReady, store.StepStatusFailed},
		{store.StepStatusReady, store.StepStatusCanceled},
		// A running step cannot be produced by any code path, but old databases
		// and fixtures hold one, and FinalizeStep must be able to finish it.
		{store.StepStatusRunning, store.StepStatusCompleted},
		{store.StepStatusRunning, store.StepStatusFailed},
		{store.StepStatusRunning, store.StepStatusCanceled},
		// RetryTasks reopens a step that owns a revived task.
		{store.StepStatusFailed, store.StepStatusPending},
		{store.StepStatusCanceled, store.StepStatusPending},
	}
	for _, tc := range legal {
		if err := openjd.ValidateStepTransition(tc.from, tc.to); err != nil {
			t.Errorf("expected legal step transition %q→%q, got error: %v", tc.from, tc.to, err)
		}
	}

	illegal := []struct {
		from store.StepStatus
		to   store.StepStatus
	}{
		{store.StepStatusPending, store.StepStatusRunning},
		{store.StepStatusPending, store.StepStatusCompleted},
		{store.StepStatusPending, store.StepStatusFailed},
		// Nothing writes running, so nothing may move a step into it.
		{store.StepStatusReady, store.StepStatusRunning},
		{store.StepStatusReady, store.StepStatusPending},
		{store.StepStatusRunning, store.StepStatusPending},
		{store.StepStatusRunning, store.StepStatusReady},
		// Completed is terminal, and RetryTasks never reopens it.
		{store.StepStatusCompleted, store.StepStatusPending},
		{store.StepStatusCompleted, store.StepStatusRunning},
		{store.StepStatusCompleted, store.StepStatusFailed},
		{store.StepStatusFailed, store.StepStatusRunning},
		{store.StepStatusFailed, store.StepStatusCompleted},
		{store.StepStatusFailed, store.StepStatusReady},
		{store.StepStatusCanceled, store.StepStatusRunning},
		{store.StepStatusCanceled, store.StepStatusReady},
	}
	for _, tc := range illegal {
		err := openjd.ValidateStepTransition(tc.from, tc.to)
		if err == nil {
			t.Errorf("expected error for illegal step transition %q→%q, got nil", tc.from, tc.to)
			continue
		}
		if !errors.Is(err, openjd.ErrInvalidTransition) {
			t.Errorf("step transition %q→%q: error should wrap ErrInvalidTransition", tc.from, tc.to)
		}
	}
}

func TestValidateStepTransition_UnknownStatus(t *testing.T) {
	err := openjd.ValidateStepTransition("bogus", store.StepStatusReady)
	if err == nil {
		t.Fatal("expected error for unknown step status, got nil")
	}
	if !errors.Is(err, openjd.ErrInvalidTransition) {
		t.Errorf("expected ErrInvalidTransition, got %v", err)
	}
}

// TestStepOperations_FromStatesAreLegal pins, for steps, that each named store
// operation's guarded from-states are legal in the table.
//
// FinalizeStep's SQL guard is "status NOT IN (terminal)", which also admits
// pending, and a pending step can be finalized. Canceling each of its pending
// tasks one at a time leaves it pending with only terminal tasks; CancelTask
// drives step completion, with the startup reconcile as its backstop, and
// finalizes it pending → canceled: a legal arrow, listed below as its own
// entry.
// pending → failed is reachable too, but only through a window: a single-task
// retry whose dependency resolution never ran (the server stopped, or the store
// failed, after RetryTasks committed) leaves the step pending beside a sibling
// that is still failed, and canceling the revived task then makes it
// finalizable as failed. The table has no pending → failed arrow, so it is not
// listed. The startup reconcile closes the server-stop case (it releases or
// cascade-cancels such a step before anything can finalize it); a store
// failure with the server still up leaves the window open.
// pending → completed cannot happen: a pending step's tasks are never leased,
// so none succeeds while it is pending.
// Update this table in the same change as any guard.
func TestStepOperations_FromStatesAreLegal(t *testing.T) {
	ops := []struct {
		op   string
		from []store.StepStatus
		to   []store.StepStatus
	}{
		{"ReleaseStep", []store.StepStatus{store.StepStatusPending}, []store.StepStatus{store.StepStatusReady}},
		{"CancelPendingStep", []store.StepStatus{store.StepStatusPending}, []store.StepStatus{store.StepStatusCanceled}},
		{
			// Its guard is "status NOT IN (terminal)"; a blocked job's steps are
			// all pending, but the guard itself admits the open statuses.
			"CancelBlockedJob",
			[]store.StepStatus{store.StepStatusPending, store.StepStatusReady, store.StepStatusRunning},
			[]store.StepStatus{store.StepStatusCanceled},
		},
		{
			"FinalizeStep",
			[]store.StepStatus{store.StepStatusReady, store.StepStatusRunning},
			[]store.StepStatus{store.StepStatusCompleted, store.StepStatusFailed, store.StepStatusCanceled},
		},
		// A pending step whose tasks were all canceled one at a time (see above).
		{"FinalizeStep", []store.StepStatus{store.StepStatusPending}, []store.StepStatus{store.StepStatusCanceled}},
		{"RetryTasks", []store.StepStatus{store.StepStatusFailed, store.StepStatusCanceled}, []store.StepStatus{store.StepStatusPending}},
		// A job cancel cancels a pending step outright and gives any other open
		// step FinalizeStep's outcome.
		{"CancelJobExecution", []store.StepStatus{store.StepStatusPending}, []store.StepStatus{store.StepStatusCanceled}},
		{
			"CancelJobExecution",
			[]store.StepStatus{store.StepStatusReady, store.StepStatusRunning},
			[]store.StepStatus{store.StepStatusCompleted, store.StepStatusFailed, store.StepStatusCanceled},
		},
	}
	for _, o := range ops {
		for _, f := range o.from {
			for _, to := range o.to {
				if err := openjd.ValidateStepTransition(f, to); err != nil {
					t.Errorf("%s: %v", o.op, err)
				}
			}
		}
	}
}
