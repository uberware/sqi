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

			// A task a lease has just taken is a guarded no-op, not an error
			// and not a write, and the caller is told so: written is false.
			written, err := st.SetTaskUnschedulableReason(t.Context(), leased, "no worker")
			if err != nil {
				t.Fatalf("SetTaskUnschedulableReason on assigned = %v, want nil no-op", err)
			}
			if written {
				t.Fatal("written = true on a just-leased task, want false (the write declined)")
			}
			if got := mustTask(t, st, leased).UnschedulableReason; got != "" {
				t.Fatalf("reason = %q on a just-leased task, want empty", got)
			}

			// A ready task takes the reason, and an empty string clears it.
			written, err = st.SetTaskUnschedulableReason(t.Context(), ready, "no worker")
			if err != nil {
				t.Fatalf("SetTaskUnschedulableReason on ready = %v", err)
			}
			if !written {
				t.Fatal("written = false on a ready task, want true")
			}
			if got := mustTask(t, st, ready).UnschedulableReason; got != "no worker" {
				t.Fatalf("reason = %q on a ready task, want %q", got, "no worker")
			}
			written, err = st.SetTaskUnschedulableReason(t.Context(), ready, "")
			if err != nil {
				t.Fatalf("SetTaskUnschedulableReason clear on ready = %v", err)
			}
			if !written {
				t.Fatal("written = false on a clear of a ready task, want true")
			}
			if got := mustTask(t, st, ready).UnschedulableReason; got != "" {
				t.Fatalf("reason = %q after a clear, want empty", got)
			}

			written, err = st.SetTaskUnschedulableReason(t.Context(), "nope", "x")
			if !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown task = %v, want ErrNotFound", err)
			}
			if written {
				t.Fatal("written = true for an unknown task, want false")
			}
		})
	}
}

// TestWorker_Removable pins the one Go statement of the removability rule that
// the API's pre-check and the fake's guarded delete both call: a worker is
// removable exactly when it is offline, disabled or not. A disabled worker that
// is still online is the operator's paused machine, not a dead one.
func TestWorker_Removable(t *testing.T) {
	cases := []struct {
		name     string
		liveness store.WorkerStatus
		disabled bool
		want     bool
	}{
		{"offline", store.WorkerStatusOffline, false, true},
		{"offline and disabled", store.WorkerStatusOffline, true, true},
		{"online", store.WorkerStatusOnline, false, false},
		{"online and disabled", store.WorkerStatusOnline, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := store.Worker{Status: tc.liveness, Disabled: tc.disabled}
			if got := w.Removable(); got != tc.want {
				t.Fatalf("Removable = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDeleteWorkerIfRemovable(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name     string
		liveness store.WorkerStatus
		disabled bool
		wantErr  error
	}{
		{"offline", store.WorkerStatusOffline, false, nil},
		{"offline and disabled", store.WorkerStatusOffline, true, nil},
		{"online and disabled", store.WorkerStatusOnline, true, store.ErrConflict},
		{"online", store.WorkerStatusOnline, false, store.ErrConflict},
	}
	for _, tc := range cases {
		for name, st := range newStores(t) {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				g := seedGraph(t, st, graphOpts{})
				if _, _, err := st.RegisterWorker(t.Context(), store.Worker{
					ID: fixtureWorkerID, FarmID: g.Farm.ID, Hostname: "node", Status: tc.liveness, LastHeartbeatAt: &now,
				}); err != nil {
					t.Fatalf("RegisterWorker: %v", err)
				}
				if tc.disabled {
					if _, err := st.SetWorkerDisabled(t.Context(), fixtureWorkerID, true); err != nil {
						t.Fatalf("SetWorkerDisabled: %v", err)
					}
				}
				err := st.DeleteWorkerIfRemovable(t.Context(), fixtureWorkerID)
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
				if err := st.DeleteWorkerIfRemovable(t.Context(), "nope"); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("DeleteWorkerIfRemovable(unknown) = %v, want ErrNotFound", err)
				}
			})
		}
	})
}

// TestDeleteWorkerIfRemovable_KeepsAttemptHistory pins that removing a worker
// keeps the attempts it ran: task_attempts.worker_id is a snapshot, not a
// foreign key (migration 00012).
func TestDeleteWorkerIfRemovable_KeepsAttemptHistory(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			now := time.Now().UTC()
			g := seedGraph(t, st, graphOpts{},
				stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusRunning}})
			task := g.Tasks["a"][0]
			a := g.Attempts[task.ID]
			if _, err := st.CompleteTaskAttempt(ctx, store.AttemptCompletion{
				AttemptID: a.ID, TaskID: task.ID, TaskStatus: store.TaskStatusSucceeded,
				AttemptStatus: store.AttemptStatusSucceeded, EndedAt: now,
			}); err != nil {
				t.Fatalf("CompleteTaskAttempt: %v", err)
			}
			seedWorker(t, st, g.Farm.ID, store.WorkerStatusOffline, now)
			if err := st.DeleteWorkerIfRemovable(ctx, fixtureWorkerID); err != nil {
				t.Fatalf("DeleteWorkerIfRemovable: %v", err)
			}
			got, err := st.GetTaskAttempt(ctx, a.ID)
			if err != nil {
				t.Fatalf("GetTaskAttempt after the worker was deleted: %v", err)
			}
			if got.WorkerID != fixtureWorkerID {
				t.Fatalf("attempt worker = %q, want %q kept", got.WorkerID, fixtureWorkerID)
			}
		})
	}
}
