// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite_test

// Tests that UpdateTaskStatus enforces the task state machine
// ([store.ValidateTaskTransition]) rather than writing whatever status it is
// handed. The check and the write share one transaction, so a concurrent
// writer cannot slip a task between the read and the update.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/sqlite"
)

// transitionFixture returns a store with one task, walked to from.
func transitionFixture(t *testing.T, from store.TaskStatus) (*sqlite.Store, string) {
	t.Helper()
	s := openTestStore(t)
	insertFarm(t, s, "f1", "F1")
	insertQueue(t, s, "q1", "f1", "Q1")
	insertJob(t, s, "j1", "f1", "q1")
	insertStep(t, s, "s1", "j1", "S1", 0)
	insertTask(t, s, "t1", "j1", "s1")
	walkTaskTo(t, s, "t1", from)
	return s, "t1"
}

func TestUpdateTaskStatus_RejectsIllegalTransition(t *testing.T) {
	tests := []struct {
		name string
		from store.TaskStatus
		to   store.TaskStatus
	}{
		{"succeeded is terminal", store.TaskStatusSucceeded, store.TaskStatusReady},
		{"failed cannot succeed", store.TaskStatusFailed, store.TaskStatusSucceeded},
		{"canceled is terminal", store.TaskStatusCanceled, store.TaskStatusRunning},
		{"ready cannot start running", store.TaskStatusReady, store.TaskStatusRunning},
		{"pending cannot be assigned", store.TaskStatusPending, store.TaskStatusAssigned},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, id := transitionFixture(t, tc.from)
			ctx := context.Background()

			err := s.UpdateTaskStatus(ctx, id, tc.to)
			if !errors.Is(err, store.ErrInvalidTransition) {
				t.Fatalf("UpdateTaskStatus(%q → %q) = %v, want ErrInvalidTransition", tc.from, tc.to, err)
			}

			// The row must be untouched.
			got, getErr := s.GetTask(ctx, id)
			if getErr != nil {
				t.Fatalf("GetTask: %v", getErr)
			}
			if got.Status != tc.from {
				t.Errorf("status = %q after rejected transition, want %q (unchanged)", got.Status, tc.from)
			}
		})
	}
}

func TestUpdateTaskStatus_AllowsLegalTransition(t *testing.T) {
	s, id := transitionFixture(t, store.TaskStatusRunning)
	ctx := context.Background()

	if err := s.UpdateTaskStatus(ctx, id, store.TaskStatusSucceeded); err != nil {
		t.Fatalf("UpdateTaskStatus(running → succeeded): %v", err)
	}
	got, err := s.GetTask(ctx, id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != store.TaskStatusSucceeded {
		t.Errorf("status = %q, want succeeded", got.Status)
	}
}

// TestUpdateTaskStatus_AssignedToTerminal covers the arrow that exists because
// a worker's "running" publish can be dropped after MaxRetries while the task
// still completes.
func TestUpdateTaskStatus_AssignedToTerminal(t *testing.T) {
	s, id := transitionFixture(t, store.TaskStatusAssigned)
	ctx := context.Background()

	if err := s.UpdateTaskStatus(ctx, id, store.TaskStatusSucceeded); err != nil {
		t.Fatalf("UpdateTaskStatus(assigned → succeeded): %v", err)
	}
	got, err := s.GetTask(ctx, id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != store.TaskStatusSucceeded {
		t.Errorf("status = %q, want succeeded", got.Status)
	}
}

// TestUpdateTaskStatus_SameStatusIsNoOp pins idempotency. Task status arrives
// over JetStream, which is at-least-once: a redelivered "running" message must
// not turn into an error, or the consumer would Nak it and redeliver forever.
func TestUpdateTaskStatus_SameStatusIsNoOp(t *testing.T) {
	s, id := transitionFixture(t, store.TaskStatusRunning)
	ctx := context.Background()

	if err := s.UpdateTaskStatus(ctx, id, store.TaskStatusRunning); err != nil {
		t.Fatalf("UpdateTaskStatus(running → running) = %v, want nil (no-op)", err)
	}
	got, err := s.GetTask(ctx, id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != store.TaskStatusRunning {
		t.Errorf("status = %q, want running", got.Status)
	}
}

func TestUpdateTaskStatus_NotFound(t *testing.T) {
	s, _ := transitionFixture(t, store.TaskStatusReady)

	err := s.UpdateTaskStatus(context.Background(), "no-such-task", store.TaskStatusAssigned)
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UpdateTaskStatus(missing) = %v, want ErrNotFound", err)
	}
}

// TestUpdateTaskStatus_ConcurrentRacesToOneWinner is the reason the check and
// the write share a transaction. Two goroutines race assigned → running and
// assigned → canceled; exactly one must win and the other must be rejected,
// never both applied.
func TestUpdateTaskStatus_ConcurrentRacesToOneWinner(t *testing.T) {
	s, id := transitionFixture(t, store.TaskStatusRunning)
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make([]error, 2)
	targets := []store.TaskStatus{store.TaskStatusSucceeded, store.TaskStatusFailed}

	wg.Add(2)
	for i, target := range targets {
		go func() {
			defer wg.Done()
			errs[i] = s.UpdateTaskStatus(ctx, id, target)
		}()
	}
	wg.Wait()

	okCount := 0
	for _, err := range errs {
		switch {
		case err == nil:
			okCount++
		case errors.Is(err, store.ErrInvalidTransition):
			// expected loser
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if okCount != 1 {
		t.Errorf("%d of 2 concurrent terminal transitions succeeded, want exactly 1", okCount)
	}
}
