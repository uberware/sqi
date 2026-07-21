// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

// Parity tests: the fake must enforce the task state machine exactly as the
// SQLite store does. Handler and scheduler tests inject this fake, so a fake
// that accepted transitions SQLite rejects would let those tests pass on
// behavior that fails in production.

import (
	"context"
	"errors"
	"testing"

	"github.com/uberware/sqi/internal/store"
)

// fakeTaskAt returns a fake store holding one task walked to want along legal
// arrows only.
func fakeTaskAt(t *testing.T, want store.TaskStatus) (*Store, string) {
	t.Helper()
	ctx := context.Background()
	s := New()
	t.Cleanup(func() { _ = s.Close() })

	if _, err := s.CreateTask(ctx, store.Task{
		ID:     "t1",
		JobID:  "j1",
		StepID: "s1",
		Name:   "t1",
		Status: store.TaskStatusPending,
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	var path []store.TaskStatus
	switch want {
	case store.TaskStatusPending:
	case store.TaskStatusReady:
		path = []store.TaskStatus{store.TaskStatusReady}
	case store.TaskStatusAssigned:
		path = []store.TaskStatus{store.TaskStatusReady, store.TaskStatusAssigned}
	case store.TaskStatusRunning:
		path = []store.TaskStatus{store.TaskStatusReady, store.TaskStatusAssigned, store.TaskStatusRunning}
	default:
		path = []store.TaskStatus{
			store.TaskStatusReady, store.TaskStatusAssigned, store.TaskStatusRunning, want,
		}
	}
	for _, st := range path {
		if err := s.UpdateTaskStatus(ctx, "t1", st); err != nil {
			t.Fatalf("walk to %q: UpdateTaskStatus(%q): %v", want, st, err)
		}
	}
	return s, "t1"
}

func TestFakeUpdateTaskStatus_RejectsIllegalTransition(t *testing.T) {
	s, id := fakeTaskAt(t, store.TaskStatusSucceeded)

	err := s.UpdateTaskStatus(context.Background(), id, store.TaskStatusReady)
	if !errors.Is(err, store.ErrInvalidTransition) {
		t.Fatalf("UpdateTaskStatus(succeeded → ready) = %v, want ErrInvalidTransition", err)
	}

	got, getErr := s.GetTask(context.Background(), id)
	if getErr != nil {
		t.Fatalf("GetTask: %v", getErr)
	}
	if got.Status != store.TaskStatusSucceeded {
		t.Errorf("status = %q after rejected transition, want succeeded (unchanged)", got.Status)
	}
}

func TestFakeUpdateTaskStatus_AllowsLegalTransition(t *testing.T) {
	s, id := fakeTaskAt(t, store.TaskStatusRunning)

	if err := s.UpdateTaskStatus(context.Background(), id, store.TaskStatusSucceeded); err != nil {
		t.Fatalf("UpdateTaskStatus(running → succeeded): %v", err)
	}
}

func TestFakeUpdateTaskStatus_SameStatusIsNoOp(t *testing.T) {
	s, id := fakeTaskAt(t, store.TaskStatusRunning)

	if err := s.UpdateTaskStatus(context.Background(), id, store.TaskStatusRunning); err != nil {
		t.Errorf("UpdateTaskStatus(running → running) = %v, want nil (no-op)", err)
	}
}

func TestFakeUpdateTaskStatus_AssignedToTerminal(t *testing.T) {
	s, id := fakeTaskAt(t, store.TaskStatusAssigned)

	if err := s.UpdateTaskStatus(context.Background(), id, store.TaskStatusSucceeded); err != nil {
		t.Errorf("UpdateTaskStatus(assigned → succeeded) = %v, want nil", err)
	}
}

func TestFakeUpdateTaskStatus_NotFound(t *testing.T) {
	s, _ := fakeTaskAt(t, store.TaskStatusReady)

	err := s.UpdateTaskStatus(context.Background(), "no-such-task", store.TaskStatusAssigned)
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UpdateTaskStatus(missing) = %v, want ErrNotFound", err)
	}
}
