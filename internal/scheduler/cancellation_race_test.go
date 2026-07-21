// SPDX-License-Identifier: AGPL-3.0-or-later

package scheduler

// CancelTask's "already terminal" guard is a read followed by a separate write,
// so a task can reach a terminal state in between: the guard sees `running` and
// lets the cancel through, but by the time UpdateTaskStatus runs the row is
// `succeeded` and the state machine rejects the write.
//
// The documented contract is that canceling an already-terminal task is a
// silent no-op. Losing that race must therefore behave the same way it would
// have if the guard had seen the newer value — return nil — not surface a 500
// to the caller.
//
// staleReadStore reproduces the race deterministically. The underlying task is
// already terminal; the wrapper's GetTask hands back the pre-completion status,
// standing in for a guard that read a moment too early. No sleeps, no
// goroutines, no flakiness.

import (
	"context"
	"errors"
	"testing"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

type staleReadStore struct {
	store.Store

	taskID      string
	staleStatus store.TaskStatus
}

func (s *staleReadStore) GetTask(ctx context.Context, id string) (store.Task, error) {
	task, err := s.Store.GetTask(ctx, id)
	if err != nil || id != s.taskID {
		return task, err
	}
	task.Status = s.staleStatus // what the guard would have read pre-completion
	return task, nil
}

func TestCancelTask_LosesRaceToCompletion_IsNoOp(t *testing.T) {
	for _, terminal := range []store.TaskStatus{
		store.TaskStatusSucceeded,
		store.TaskStatusFailed,
	} {
		t.Run(string(terminal), func(t *testing.T) {
			st := fake.New()
			bus := &stubBus{}
			job := seedCancelJob(t, st)
			tk := seedTaskForJob(t, st, job, "w1", terminal)

			// The guard reads "running"; the row is already terminal.
			s := newTestScheduler(&staleReadStore{
				Store:       st,
				taskID:      tk.ID,
				staleStatus: store.TaskStatusRunning,
			}, bus)

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
		})
	}
}

// TestCancelTask_RealErrorStillPropagates guards against the fix being written
// as a blanket "swallow every error from UpdateTaskStatus".
func TestCancelTask_RealErrorStillPropagates(t *testing.T) {
	st := fake.New()
	bus := &stubBus{}
	job := seedCancelJob(t, st)
	tk := seedTaskForJob(t, st, job, "w1", store.TaskStatusRunning)

	s := newTestScheduler(&failingUpdateStore{Store: st, taskID: tk.ID}, bus)

	err := s.CancelTask(t.Context(), tk.ID)
	if err == nil {
		t.Fatal("CancelTask = nil, want the underlying store error to propagate")
	}
	if errors.Is(err, store.ErrInvalidTransition) {
		t.Errorf("error = %v, want the store failure, not ErrInvalidTransition", err)
	}
}

var errStoreUnavailable = errors.New("store unavailable")

type failingUpdateStore struct {
	store.Store

	taskID string
}

func (s *failingUpdateStore) UpdateTaskStatus(ctx context.Context, id string, status store.TaskStatus) error {
	if id == s.taskID {
		return errStoreUnavailable
	}
	return s.Store.UpdateTaskStatus(ctx, id, status)
}
