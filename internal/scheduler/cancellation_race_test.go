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

	attempt  store.TaskAttempt
	terminal store.TaskStatus
	t        *testing.T
}

func (s *completeBeforeCancelStore) CancelTaskExecution(
	ctx context.Context, id, reason string, now time.Time,
) (store.Task, bool, error) {
	if id == s.attempt.TaskID {
		// The worker's terminal report lands first.
		s.reportTerminal(ctx)
	}
	return s.Store.CancelTaskExecution(ctx, id, reason, now)
}

// reportTerminal applies the worker's terminal report on the leased attempt
// with the writes the status consumer makes for it: a success is one
// CompleteTaskAttempt; a failure with no retry left is RecordTaskFailure,
// which closes the attempt, then CompleteTaskAttempt, which ends the task.
func (s *completeBeforeCancelStore) reportTerminal(ctx context.Context) {
	now := time.Now().UTC()
	exit := 0
	attemptStatus := store.AttemptStatusSucceeded
	if s.terminal == store.TaskStatusFailed {
		exit, attemptStatus = 1, store.AttemptStatusFailed
		if _, _, _, err := s.RecordTaskFailure(ctx, s.attempt.ID, s.attempt.TaskID, &exit, "", "boom", now); err != nil {
			s.t.Errorf("RecordTaskFailure in hook: %v", err)
		}
	}
	res, err := s.CompleteTaskAttempt(ctx, store.AttemptCompletion{
		AttemptID: s.attempt.ID, TaskID: s.attempt.TaskID, TaskStatus: s.terminal, AttemptStatus: attemptStatus,
		ExitCode: &exit, EndedAt: now,
	})
	if err != nil || !res.Applied {
		s.t.Errorf("CompleteTaskAttempt in hook = (%+v, %v), want applied", res, err)
	}
}

func TestCancelTask_LosesRaceToCompletion_IsNoOp(t *testing.T) {
	for _, terminal := range []store.TaskStatus{
		store.TaskStatusSucceeded,
		store.TaskStatusFailed,
	} {
		t.Run(string(terminal), func(t *testing.T) {
			st := newCheckedFake(t)
			bus := &stubBus{}
			_, tasks, attempts := seedCancelJob(t, st, cancelTask{status: store.TaskStatusRunning, worker: "w1"})
			tk := tasks[0]

			s := newTestScheduler(&completeBeforeCancelStore{Store: st, attempt: attempts[0], terminal: terminal, t: t}, bus)

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

// TestCancelTask_RealErrorStillPropagates pins that CancelTask treats only
// ErrInvalidTransition as a no-op and does not swallow every error from the
// cancel operation.
func TestCancelTask_RealErrorStillPropagates(t *testing.T) {
	st := newCheckedFake(t)
	bus := &stubBus{}
	_, tasks, _ := seedCancelJob(t, st, cancelTask{status: store.TaskStatusRunning, worker: "w1"})
	tk := tasks[0]

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
