// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

func TestSetTaskUnschedulableReason_OnlyWhileReady(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{},
				stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusAssigned, store.TaskStatusReady}})
			leased, ready := g.Tasks["a"][0].ID, g.Tasks["a"][1].ID

			// A task a lease has just taken is a guarded no-op, not an error and not
			// a write (F15).
			if err := st.SetTaskUnschedulableReason(t.Context(), leased, "no worker"); err != nil {
				t.Fatalf("SetTaskUnschedulableReason on assigned = %v, want nil no-op", err)
			}
			if got := mustTask(t, st, leased).UnschedulableReason; got != "" {
				t.Fatalf("reason = %q on a just-leased task, want empty (F15)", got)
			}

			// A ready task takes the reason, and an empty string clears it.
			if err := st.SetTaskUnschedulableReason(t.Context(), ready, "no worker"); err != nil {
				t.Fatalf("SetTaskUnschedulableReason on ready = %v", err)
			}
			if got := mustTask(t, st, ready).UnschedulableReason; got != "no worker" {
				t.Fatalf("reason = %q on a ready task, want %q", got, "no worker")
			}
			if err := st.SetTaskUnschedulableReason(t.Context(), ready, ""); err != nil {
				t.Fatalf("SetTaskUnschedulableReason clear on ready = %v", err)
			}
			if got := mustTask(t, st, ready).UnschedulableReason; got != "" {
				t.Fatalf("reason = %q after a clear, want empty", got)
			}

			if err := st.SetTaskUnschedulableReason(t.Context(), "nope", "x"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown task = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestUpdateTaskAttempt_OnlyWhileRunning(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{},
				stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusSucceeded, store.TaskStatusRunning}})

			// A closed attempt is never rewritten (F16): the write is a typed
			// ErrConflict and the row is untouched.
			closed := seedAttempt(t, st, g.Tasks["a"][0], store.AttemptStatusSucceeded)
			closed.SessionID = "late"
			closed.Status = store.AttemptStatusFailed
			if _, err := st.UpdateTaskAttempt(t.Context(), closed); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("UpdateTaskAttempt on a closed attempt = %v, want ErrConflict (F16)", err)
			}
			got, err := st.GetTaskAttempt(t.Context(), closed.ID)
			if err != nil {
				t.Fatalf("GetTaskAttempt: %v", err)
			}
			if got.Status != store.AttemptStatusSucceeded || got.SessionID != "" {
				t.Fatalf("closed attempt = {status %q, session %q}, want it untouched", got.Status, got.SessionID)
			}

			// A running attempt still takes the write, which is how a worker's
			// "running" report records its session.
			running := seedAttempt(t, st, g.Tasks["a"][1], store.AttemptStatusRunning)
			running.SessionID = "sess-1"
			updated, err := st.UpdateTaskAttempt(t.Context(), running)
			if err != nil {
				t.Fatalf("UpdateTaskAttempt on a running attempt: %v", err)
			}
			if updated.SessionID != "sess-1" || updated.Status != store.AttemptStatusRunning {
				t.Fatalf("running attempt after update = {status %q, session %q}, want {running, sess-1}", updated.Status, updated.SessionID)
			}

			if _, err := st.UpdateTaskAttempt(t.Context(), store.TaskAttempt{ID: "nope"}); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown attempt = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestDeleteWorkerIfRemovable(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name      string
		status    store.WorkerStatus
		heartbeat time.Time
		wantErr   error
	}{
		{"offline", store.WorkerStatusOffline, now, nil},
		{"disabled and dead", store.WorkerStatusDisabled, now.Add(-time.Hour), nil},
		{"disabled and live", store.WorkerStatusDisabled, now, store.ErrConflict},
		{"online", store.WorkerStatusOnline, now.Add(-time.Hour), store.ErrConflict},
	}
	for _, tc := range cases {
		for name, st := range newStores(t) {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				g := seedGraph(t, st, graphOpts{})
				seedWorker(t, st, g.Farm.ID, tc.status, tc.heartbeat)
				err := st.DeleteWorkerIfRemovable(t.Context(), fixtureWorkerID, now.Add(-time.Minute))
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("DeleteWorkerIfRemovable = %v, want %v", err, tc.wantErr)
				}
				_, getErr := st.GetWorker(t.Context(), fixtureWorkerID)
				switch {
				case tc.wantErr == nil && !errors.Is(getErr, store.ErrNotFound):
					t.Fatalf("GetWorker after a successful delete = %v, want ErrNotFound", getErr)
				case tc.wantErr != nil && getErr != nil:
					t.Fatalf("GetWorker after a refused delete = %v, want the row kept", getErr)
				}
			})
		}
	}

	t.Run("unknown worker", func(t *testing.T) {
		for name, st := range newStores(t) {
			t.Run(name, func(t *testing.T) {
				if err := st.DeleteWorkerIfRemovable(t.Context(), "nope", now); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("DeleteWorkerIfRemovable(unknown) = %v, want ErrNotFound", err)
				}
			})
		}
	})
}
