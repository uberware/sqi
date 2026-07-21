// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

// Tests for statemachine.go — the task state machine that [store.Store]
// implementations enforce on every status write.
//
// The task machine moved here from internal/openjd so the store can enforce it
// without an import cycle (openjd imports store). openjd.ValidateTaskTransition
// now delegates here and keeps its own tests.

import (
	"errors"
	"testing"

	"github.com/uberware/sqi/internal/store"
)

func TestValidateTaskTransition_Legal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		from store.TaskStatus
		to   store.TaskStatus
	}{
		{"pending to ready", store.TaskStatusPending, store.TaskStatusReady},
		{"pending to canceled", store.TaskStatusPending, store.TaskStatusCanceled},
		{"ready to assigned", store.TaskStatusReady, store.TaskStatusAssigned},
		{"ready to canceled", store.TaskStatusReady, store.TaskStatusCanceled},
		{"assigned to running", store.TaskStatusAssigned, store.TaskStatusRunning},
		{"assigned to ready", store.TaskStatusAssigned, store.TaskStatusReady},
		{"assigned to canceled", store.TaskStatusAssigned, store.TaskStatusCanceled},
		{"running to succeeded", store.TaskStatusRunning, store.TaskStatusSucceeded},
		{"running to failed", store.TaskStatusRunning, store.TaskStatusFailed},
		{"running to ready", store.TaskStatusRunning, store.TaskStatusReady},
		{"running to canceled", store.TaskStatusRunning, store.TaskStatusCanceled},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := store.ValidateTaskTransition(tc.from, tc.to); err != nil {
				t.Errorf("ValidateTaskTransition(%q, %q) = %v, want nil", tc.from, tc.to, err)
			}
		})
	}
}

// TestValidateTaskTransition_AssignedToTerminal pins the arrows added when
// store-level enforcement was introduced. A worker publishes "running" before
// it publishes a terminal status, but status.Publisher.publishWithRetry gives
// up after MaxRetries and returns — so the "running" message can be dropped
// permanently while the task still completes. The terminal message then arrives
// with the task row still in 'assigned'. Rejecting that would strand finished
// work in 'assigned' until the heartbeat sweep reclaimed it.
func TestValidateTaskTransition_AssignedToTerminal(t *testing.T) {
	t.Parallel()

	for _, to := range []store.TaskStatus{store.TaskStatusSucceeded, store.TaskStatusFailed} {
		t.Run(string(to), func(t *testing.T) {
			t.Parallel()
			if err := store.ValidateTaskTransition(store.TaskStatusAssigned, to); err != nil {
				t.Errorf("ValidateTaskTransition(assigned, %q) = %v, want nil", to, err)
			}
		})
	}
}

func TestValidateTaskTransition_Illegal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		from store.TaskStatus
		to   store.TaskStatus
	}{
		{"pending skips ready", store.TaskStatusPending, store.TaskStatusAssigned},
		{"pending to running", store.TaskStatusPending, store.TaskStatusRunning},
		{"ready to running", store.TaskStatusReady, store.TaskStatusRunning},
		{"ready to succeeded", store.TaskStatusReady, store.TaskStatusSucceeded},
		{"succeeded is terminal", store.TaskStatusSucceeded, store.TaskStatusReady},
		{"failed is terminal", store.TaskStatusFailed, store.TaskStatusRunning},
		{"canceled is terminal", store.TaskStatusCanceled, store.TaskStatusReady},
		{"failed cannot succeed", store.TaskStatusFailed, store.TaskStatusSucceeded},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := store.ValidateTaskTransition(tc.from, tc.to)
			if !errors.Is(err, store.ErrInvalidTransition) {
				t.Errorf("ValidateTaskTransition(%q, %q) = %v, want ErrInvalidTransition", tc.from, tc.to, err)
			}
		})
	}
}

func TestValidateTaskTransition_UnknownStatus(t *testing.T) {
	t.Parallel()

	err := store.ValidateTaskTransition("bogus", store.TaskStatusReady)
	if !errors.Is(err, store.ErrInvalidTransition) {
		t.Errorf("ValidateTaskTransition(bogus, ready) = %v, want ErrInvalidTransition", err)
	}
}
