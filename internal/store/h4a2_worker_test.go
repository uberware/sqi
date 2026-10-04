// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

func registerInstance(t *testing.T, st store.Store, farmID, instance string) []store.Task {
	t.Helper()
	now := time.Now().UTC()
	_, reclaimed, err := st.RegisterWorker(t.Context(), store.Worker{
		ID: fixtureWorkerID, FarmID: farmID, Hostname: "node", Status: store.WorkerStatusOnline,
		LastHeartbeatAt: &now, InstanceID: instance,
	})
	if err != nil {
		t.Fatalf("RegisterWorker(%q): %v", instance, err)
	}
	return reclaimed
}

// TestRegisterWorker_ReclaimsAfterARestart pins item 9 viii (first half) and
// Review Focus 2.
func TestRegisterWorker_ReclaimsAfterARestart(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning},
				stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusRunning}})
			task := g.Tasks["a"][0]
			pool := seedPool(t, st, 1)
			a := seedAttempt(t, st, task, store.AttemptStatusRunning)
			seedClaim(t, st, pool.ID, a.ID)

			// A legacy registration (no instance ID), then the first one with an
			// ID: nothing proves a restart, so nothing is reclaimed.
			if r := registerInstance(t, st, g.Farm.ID, ""); len(r) != 0 {
				t.Fatalf("legacy register reclaimed %d", len(r))
			}
			if r := registerInstance(t, st, g.Farm.ID, "i1"); len(r) != 0 {
				t.Fatalf("first instance register reclaimed %d, want 0 (stored ID was empty)", len(r))
			}
			// The upgrade from a build that sent no ID left the in-flight task,
			// its attempt and its claim exactly as they were.
			if got := mustTask(t, st, task.ID); got.Status != store.TaskStatusRunning || got.AssignedWorkerID != fixtureWorkerID {
				t.Fatalf("after the legacy-to-ID register: task = %q on %q, want running on %q", got.Status, got.AssignedWorkerID, fixtureWorkerID)
			}
			if at := mustAttempt(t, st, a.ID); at.Status != store.AttemptStatusRunning {
				t.Fatalf("after the legacy-to-ID register: attempt = %q, want running", at.Status)
			}
			if n := activeClaims(t, st, pool.ID); n != 1 {
				t.Fatalf("after the legacy-to-ID register: active claims = %d, want 1", n)
			}
			// A reconnect re-register of the same process.
			if r := registerInstance(t, st, g.Farm.ID, "i1"); len(r) != 0 {
				t.Fatalf("same-instance register reclaimed %d", len(r))
			}
			// An old worker build without IDs: never reclaims, keeps the stored ID.
			if r := registerInstance(t, st, g.Farm.ID, ""); len(r) != 0 {
				t.Fatalf("empty-instance register reclaimed %d", len(r))
			}
			if got := mustWorker(t, st, fixtureWorkerID).InstanceID; got != "i1" {
				t.Fatalf("stored instance = %q, want i1 (kept)", got)
			}
			if got := mustTask(t, st, task.ID).Status; got != store.TaskStatusRunning {
				t.Fatalf("task = %q, want still running", got)
			}
			// A new process.
			r := registerInstance(t, st, g.Farm.ID, "i2")
			if len(r) != 1 || r[0].ID != task.ID || r[0].Status != store.TaskStatusReady {
				t.Fatalf("restart reclaimed %+v, want the task, ready", r)
			}
			if at := mustAttempt(t, st, a.ID); at.Status != store.AttemptStatusFailed || at.Message != store.FailureReasonWorkerRestarted {
				t.Fatalf("attempt = %+v, want failed with the restart message", at)
			}
			if n := activeClaims(t, st, pool.ID); n != 0 {
				t.Fatalf("active claims = %d, want 0", n)
			}
			w := mustWorker(t, st, fixtureWorkerID)
			if w.Status != store.WorkerStatusOnline || w.InstanceID != "i2" {
				t.Fatalf("worker = %q/%q, want online/i2", w.Status, w.InstanceID)
			}
		})
	}
}

// TestDisabledWorker_StaysDisabledAndIsReclaimed pins H4a2 §5.2 and §5.3: a dead
// disabled worker is listed and reclaimed by the sweep but stays disabled, an
// idle one is neither listed nor rewritten, and a re-registration or a graceful
// deregister keeps it disabled.
func TestDisabledWorker_StaysDisabledAndIsReclaimed(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx, now := t.Context(), time.Now().UTC()
			g := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning},
				stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusRunning}})
			seedAttempt(t, st, g.Tasks["a"][0], store.AttemptStatusRunning)
			seedWorker(t, st, g.Farm.ID, store.WorkerStatusDisabled, now.Add(-time.Hour))
			cutoff := now.Add(-time.Minute)

			listed, err := st.ListStaleWorkers(ctx, cutoff)
			if err != nil || len(listed) != 1 {
				t.Fatalf("ListStaleWorkers = (%d, %v), want the dead disabled worker", len(listed), err)
			}
			tasks, ok, err := st.OfflineStaleWorker(ctx, fixtureWorkerID, cutoff, now)
			if err != nil || !ok || len(tasks) != 1 {
				t.Fatalf("OfflineStaleWorker = (%d, %v, %v), want (1, true, nil)", len(tasks), ok, err)
			}
			if w := mustWorker(t, st, fixtureWorkerID); w.Status != store.WorkerStatusDisabled {
				t.Fatalf("worker = %q, want disabled (kept)", w.Status)
			}
			// Idle now: no longer listed, and the guarded write declines.
			listed, err = st.ListStaleWorkers(ctx, cutoff)
			if err != nil {
				t.Fatalf("ListStaleWorkers (idle): %v", err)
			}
			if len(listed) != 0 {
				t.Fatalf("idle disabled worker listed again: %+v", listed)
			}
			_, ok, err = st.OfflineStaleWorker(ctx, fixtureWorkerID, cutoff, now)
			if err != nil {
				t.Fatalf("OfflineStaleWorker (idle): %v", err)
			}
			if ok {
				t.Fatal("idle disabled worker rewritten by the sweep")
			}
			// Re-registration (any NATS reconnect) and a deregister keep disabled.
			if _, _, err := st.RegisterWorker(ctx, store.Worker{ID: fixtureWorkerID, FarmID: g.Farm.ID, Hostname: "node", Status: store.WorkerStatusOnline, LastHeartbeatAt: &now}); err != nil {
				t.Fatalf("RegisterWorker: %v", err)
			}
			if w := mustWorker(t, st, fixtureWorkerID); w.Status != store.WorkerStatusDisabled {
				t.Fatalf("after re-register: worker = %q, want disabled", w.Status)
			}
			if _, err := st.OfflineWorker(ctx, fixtureWorkerID, now); err != nil {
				t.Fatalf("OfflineWorker: %v", err)
			}
			if w := mustWorker(t, st, fixtureWorkerID); w.Status != store.WorkerStatusDisabled {
				t.Fatalf("after deregister: worker = %q, want disabled", w.Status)
			}
		})
	}
}

// TestDeleteWorkerIfRemovable_RefusesAWorkerWithWorkInFlight pins H4a2 §5.4: a
// worker that is otherwise removable (offline) is refused while a task is still
// running on it, and removed once it is not.
func TestDeleteWorkerIfRemovable_RefusesAWorkerWithWorkInFlight(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx, now := t.Context(), time.Now().UTC()
			g := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning},
				stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusRunning}})
			seedWorker(t, st, g.Farm.ID, store.WorkerStatusOffline, now.Add(-time.Hour))
			if err := st.DeleteWorkerIfRemovable(ctx, fixtureWorkerID, now); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("delete with a running task: err = %v, want ErrConflict", err)
			}
			if err := st.UpdateTaskStatus(ctx, g.Tasks["a"][0].ID, store.TaskStatusSucceeded); err != nil {
				t.Fatalf("UpdateTaskStatus: %v", err)
			}
			if err := st.DeleteWorkerIfRemovable(ctx, fixtureWorkerID, now); err != nil {
				t.Fatalf("delete once idle: %v", err)
			}
		})
	}
}
