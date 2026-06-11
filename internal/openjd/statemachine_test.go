// SPDX-License-Identifier: AGPL-3.0-or-later

package openjd_test

// Tests for statemachine.go — item 7 of the test roadmap.
//
// Covers all legal and illegal task/step transitions via table-driven tests.

import (
	"errors"
	"testing"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
)

// ── Task transitions ──────────────────────────────────────────────────────────

func TestValidateTaskTransition(t *testing.T) {
	legal := []struct {
		from store.TaskStatus
		to   store.TaskStatus
	}{
		{store.TaskStatusPending, store.TaskStatusReady},
		{store.TaskStatusPending, store.TaskStatusCanceled},
		{store.TaskStatusReady, store.TaskStatusAssigned},
		{store.TaskStatusReady, store.TaskStatusCanceled},
		{store.TaskStatusAssigned, store.TaskStatusRunning},
		{store.TaskStatusAssigned, store.TaskStatusReady},
		{store.TaskStatusAssigned, store.TaskStatusCanceled},
		{store.TaskStatusRunning, store.TaskStatusSucceeded},
		{store.TaskStatusRunning, store.TaskStatusFailed},
		{store.TaskStatusRunning, store.TaskStatusReady},
		{store.TaskStatusRunning, store.TaskStatusCanceled},
	}
	for _, tc := range legal {
		if err := openjd.ValidateTaskTransition(tc.from, tc.to); err != nil {
			t.Errorf("expected legal transition %q→%q, got error: %v", tc.from, tc.to, err)
		}
	}

	illegal := []struct {
		from store.TaskStatus
		to   store.TaskStatus
	}{
		{store.TaskStatusPending, store.TaskStatusRunning},
		{store.TaskStatusPending, store.TaskStatusSucceeded},
		{store.TaskStatusReady, store.TaskStatusRunning},
		{store.TaskStatusReady, store.TaskStatusSucceeded},
		{store.TaskStatusSucceeded, store.TaskStatusRunning},
		{store.TaskStatusSucceeded, store.TaskStatusFailed},
		{store.TaskStatusFailed, store.TaskStatusRunning},
		{store.TaskStatusFailed, store.TaskStatusSucceeded},
		{store.TaskStatusCanceled, store.TaskStatusRunning},
		{store.TaskStatusCanceled, store.TaskStatusSucceeded},
	}
	for _, tc := range illegal {
		err := openjd.ValidateTaskTransition(tc.from, tc.to)
		if err == nil {
			t.Errorf("expected error for illegal transition %q→%q, got nil", tc.from, tc.to)
			continue
		}
		if !errors.Is(err, openjd.ErrInvalidTransition) {
			t.Errorf("transition %q→%q: error %v should wrap ErrInvalidTransition", tc.from, tc.to, err)
		}
	}
}

func TestValidateTaskTransition_UnknownStatus(t *testing.T) {
	err := openjd.ValidateTaskTransition("bogus", store.TaskStatusReady)
	if err == nil {
		t.Fatal("expected error for unknown status, got nil")
	}
	if !errors.Is(err, openjd.ErrInvalidTransition) {
		t.Errorf("expected ErrInvalidTransition, got %v", err)
	}
}

// ── Step transitions ──────────────────────────────────────────────────────────

func TestValidateStepTransition(t *testing.T) {
	legal := []struct {
		from store.StepStatus
		to   store.StepStatus
	}{
		{store.StepStatusPending, store.StepStatusReady},
		{store.StepStatusPending, store.StepStatusCanceled},
		{store.StepStatusReady, store.StepStatusRunning},
		{store.StepStatusReady, store.StepStatusCanceled},
		{store.StepStatusRunning, store.StepStatusCompleted},
		{store.StepStatusRunning, store.StepStatusFailed},
		{store.StepStatusRunning, store.StepStatusCanceled},
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
		{store.StepStatusReady, store.StepStatusCompleted},
		{store.StepStatusCompleted, store.StepStatusRunning},
		{store.StepStatusCompleted, store.StepStatusFailed},
		{store.StepStatusFailed, store.StepStatusRunning},
		{store.StepStatusFailed, store.StepStatusCompleted},
		{store.StepStatusCanceled, store.StepStatusRunning},
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
