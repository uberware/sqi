// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// A task can reach a terminal state while a cancel for it is in flight. The
// documented contract is that canceling an already-terminal task is a silent
// no-op, so losing that race must behave the same way it would have if the
// cancel had arrived afterwards: return nil, publish nothing, and leave the
// terminal status alone. It must not surface a 500 to the caller.
//
// CancelTask makes its decision inside a single store operation
// ([store.TaskStore.CancelTaskExecution]), which reports a terminal task as
// "not canceled" rather than as an error. completeBeforeCancelStore reproduces
// the race deterministically: the task is running when CancelTask starts, and
// the wrapper completes it immediately before the store operation runs. No
// sleeps, no goroutines, no flakiness.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

type completeBeforeCancelStore struct {
	store.Store

	taskID   string
	terminal store.TaskStatus
	t        *testing.T
}

func (s *completeBeforeCancelStore) CancelTaskExecution(
	ctx context.Context, id, reason string, now time.Time,
) (store.Task, bool, error) {
	if id == s.taskID {
		// The worker's terminal report lands first.
		if err := fixtures(s.t, s.Store).UpdateTaskStatus(ctx, id, s.terminal); err != nil {
			s.t.Errorf("complete task in hook: %v", err)
		}
	}
	return s.Store.CancelTaskExecution(ctx, id, reason, now)
}

func TestCancelTask_LosesRaceToCompletion_IsNoOp(t *testing.T) {
	for _, terminal := range []store.TaskStatus{
		store.TaskStatusSucceeded,
		store.TaskStatusFailed,
	} {
		t.Run(string(terminal), func(t *testing.T) {
			st := newCheckedFake(t)
			bus := &stubBus{}
			job := seedCancelJob(t, st)
			tk := seedTaskForJob(t, st, job, "w1", store.TaskStatusRunning)

			s := newTestScheduler(&completeBeforeCancelStore{Store: st, taskID: tk.ID, terminal: terminal, t: t}, bus)

			if err := s.CancelTask(t.Context(), tk.ID); err != nil {
				t.Fatalf("CancelTask losing the race to completion = %v, want nil (no-op)", err)
			}

			stored, err := st.GetTask(t.Context(), tk.ID)
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			if stored.Status != terminal {
				t.Errorf("status = %q, want %q — a completed task must not be overwritten by a losing cancel",
					stored.Status, terminal)
			}
			if len(bus.cancelCalls) != 0 {
				t.Errorf("cancel signals = %v, want none for a task that completed first", bus.cancelCalls)
			}
		})
	}
}

// TestCancelTask_RealErrorStillPropagates guards against the fix being written
// as a blanket "swallow every error from the cancel operation".
func TestCancelTask_RealErrorStillPropagates(t *testing.T) {
	st := newCheckedFake(t)
	bus := &stubBus{}
	job := seedCancelJob(t, st)
	tk := seedTaskForJob(t, st, job, "w1", store.TaskStatusRunning)

	s := newTestScheduler(&failingCancelStore{Store: st, taskID: tk.ID}, bus)

	err := s.CancelTask(t.Context(), tk.ID)
	if err == nil {
		t.Fatal("CancelTask = nil, want the underlying store error to propagate")
	}
	if !errors.Is(err, errStoreUnavailable) {
		t.Errorf("error = %v, want the store failure", err)
	}
	if errors.Is(err, store.ErrInvalidTransition) {
		t.Errorf("error = %v, want the store failure, not ErrInvalidTransition", err)
	}
	if len(bus.cancelCalls) != 0 {
		t.Errorf("cancel signals = %v, want none when the store failed", bus.cancelCalls)
	}
}

var errStoreUnavailable = errors.New("store unavailable")

// failingCancelStore fails CancelTaskExecution for one task, standing in for a
// store outage during the cancel.
type failingCancelStore struct {
	store.Store

	taskID string
}

func (s *failingCancelStore) CancelTaskExecution(
	ctx context.Context, id, reason string, now time.Time,
) (store.Task, bool, error) {
	if id == s.taskID {
		return store.Task{}, false, errStoreUnavailable
	}
	return s.Store.CancelTaskExecution(ctx, id, reason, now)
}
