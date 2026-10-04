// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
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
