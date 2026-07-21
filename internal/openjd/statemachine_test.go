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
