// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"errors"
	"slices"
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

// TestDisabledWorker_StaysDisabledAndIsReclaimed pins H4a2 §5.2 and §5.3 under
// the disabled flag: a dead disabled worker is swept like any online worker (it
// goes offline and its task is reclaimed) and stays disabled, an offline one is
// not swept again, and neither a re-registration nor a graceful deregister
// clears the flag. Liveness and the flag are separate facts.
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
			wantWorker(t, st, store.WorkerStatusOffline, true, "after the sweep")
			// Offline now: no longer listed, and the guarded write declines.
			listed, err = st.ListStaleWorkers(ctx, cutoff)
			if err != nil {
				t.Fatalf("ListStaleWorkers (offline): %v", err)
			}
			if len(listed) != 0 {
				t.Fatalf("offline disabled worker listed again: %+v", listed)
			}
			if _, ok, err = st.OfflineStaleWorker(ctx, fixtureWorkerID, cutoff, now); err != nil || ok {
				t.Fatalf("OfflineStaleWorker (offline) = (%v, %v), want (false, nil)", ok, err)
			}
			// Re-registration (any NATS reconnect) brings it online, still disabled;
			// a deregister takes it offline, still disabled.
			if _, _, err := st.RegisterWorker(ctx, store.Worker{ID: fixtureWorkerID, FarmID: g.Farm.ID, Hostname: "node", Status: store.WorkerStatusOnline, LastHeartbeatAt: &now}); err != nil {
				t.Fatalf("RegisterWorker: %v", err)
			}
			wantWorker(t, st, store.WorkerStatusOnline, true, "after re-register")
			if _, _, err := st.OfflineWorker(ctx, fixtureWorkerID, "", now); err != nil {
				t.Fatalf("OfflineWorker: %v", err)
			}
			wantWorker(t, st, store.WorkerStatusOffline, true, "after deregister")
		})
	}
}

// wantWorker asserts the fixture worker's liveness and disabled flag, and that
// its effective status is disabled exactly when the flag is set.
func wantWorker(t *testing.T, st store.Store, liveness store.WorkerStatus, disabled bool, when string) {
	t.Helper()
	w := mustWorker(t, st, fixtureWorkerID)
	if w.Status != liveness || w.Disabled != disabled {
		t.Fatalf("%s: worker = %q disabled=%v, want %q disabled=%v", when, w.Status, w.Disabled, liveness, disabled)
	}
	want := liveness
	if disabled {
		want = store.WorkerStatusDisabled
	}
	if got := w.EffectiveStatus(); got != want {
		t.Fatalf("%s: EffectiveStatus = %q, want %q", when, got, want)
	}
}

// TestSetWorkerDisabled pins the admin write: it sets and clears the flag, never
// touches liveness, returns the stored row, and is ErrNotFound for an unknown
// worker.
func TestSetWorkerDisabled(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx, now := t.Context(), time.Now().UTC()
			g := seedGraph(t, st, graphOpts{})
			seedWorker(t, st, g.Farm.ID, store.WorkerStatusOffline, now)

			w, err := st.SetWorkerDisabled(ctx, fixtureWorkerID, true)
			if err != nil || !w.Disabled || w.Status != store.WorkerStatusOffline {
				t.Fatalf("disable = (%q disabled=%v, %v), want offline disabled", w.Status, w.Disabled, err)
			}
			wantWorker(t, st, store.WorkerStatusOffline, true, "after disable")
			if w, err = st.SetWorkerDisabled(ctx, fixtureWorkerID, true); err != nil || !w.Disabled {
				t.Fatalf("disable again = (disabled=%v, %v), want idempotent", w.Disabled, err)
			}
			if w, err = st.SetWorkerDisabled(ctx, fixtureWorkerID, false); err != nil || w.Disabled {
				t.Fatalf("enable = (disabled=%v, %v), want cleared", w.Disabled, err)
			}
			wantWorker(t, st, store.WorkerStatusOffline, false, "after enable")
			if _, err := st.SetWorkerDisabled(ctx, "nope", true); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("SetWorkerDisabled(unknown) = %v, want ErrNotFound", err)
			}
		})
	}
}

// TestListWorkers_FiltersByEffectiveStatus pins that the status filter, which
// the worker gauge and the API use, reads the effective status: a disabled
// worker is listed only as disabled, whatever its liveness, so the three
// buckets partition the workers exactly as when disabled was a status. It also
// pins the two other readers that must not count a disabled worker as an
// ordinary one: the idle count and offline retention.
func TestListWorkers_FiltersByEffectiveStatus(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx, now := t.Context(), time.Now().UTC()
			g := seedGraph(t, st, graphOpts{})
			register := func(id string, liveness store.WorkerStatus, disabled bool) {
				t.Helper()
				if _, _, err := st.RegisterWorker(ctx, store.Worker{ID: id, FarmID: g.Farm.ID, Hostname: id, Status: liveness, LastHeartbeatAt: &now}); err != nil {
					t.Fatalf("RegisterWorker(%s): %v", id, err)
				}
				if disabled {
					if _, err := st.SetWorkerDisabled(ctx, id, true); err != nil {
						t.Fatalf("SetWorkerDisabled(%s): %v", id, err)
					}
				}
			}
			register("on", store.WorkerStatusOnline, false)
			register("off", store.WorkerStatusOffline, false)
			register("dis-on", store.WorkerStatusOnline, true)
			register("dis-off", store.WorkerStatusOffline, true)

			for status, want := range map[store.WorkerStatus][]string{
				store.WorkerStatusOnline:   {"on"},
				store.WorkerStatusOffline:  {"off"},
				store.WorkerStatusDisabled: {"dis-off", "dis-on"},
			} {
				page, err := st.ListWorkers(ctx, store.ListWorkersOptions{Status: status, Pagination: store.Pagination{Limit: 10}})
				if err != nil {
					t.Fatalf("ListWorkers(%s): %v", status, err)
				}
				var got []string
				for _, w := range page.Items {
					got = append(got, w.ID)
				}
				if page.Total != len(want) || !slices.Equal(got, want) {
					t.Fatalf("ListWorkers(%s) = %v (total %d), want %v", status, got, page.Total, want)
				}
			}
			// A disabled worker is never idle capacity, whatever its liveness.
			if n, err := st.CountIdleWorkers(ctx, g.Farm.ID); err != nil || n != 1 {
				t.Fatalf("CountIdleWorkers = (%d, %v), want 1 (the enabled online worker)", n, err)
			}
			// Retention deletes only enabled offline workers: a disabled one is
			// kept until an operator removes it.
			removed, err := st.DeleteOfflineWorkersBefore(ctx, now.Add(time.Hour))
			if err != nil || len(removed) != 1 || removed[0].ID != "off" {
				t.Fatalf("DeleteOfflineWorkersBefore = (%+v, %v), want only %q", removed, err, "off")
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
			if err := st.DeleteWorkerIfRemovable(ctx, fixtureWorkerID); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("delete with a running task: err = %v, want ErrConflict", err)
			}
			if err := fixtures(t, st).UpdateTaskStatus(ctx, g.Tasks["a"][0].ID, store.TaskStatusSucceeded); err != nil {
				t.Fatalf("UpdateTaskStatus: %v", err)
			}
			if err := st.DeleteWorkerIfRemovable(ctx, fixtureWorkerID); err != nil {
				t.Fatalf("delete once idle: %v", err)
			}
		})
	}
}

// TestOfflineWorker_IgnoresASupersededInstance pins the whole-branch review's
// stale-deregister race: a deregister from a process the worker's latest
// registration replaced must not take the new process offline or reclaim the
// task it is running. An empty instance ID on either side proves nothing, so
// the deregister applies as it did before instance IDs existed.
func TestOfflineWorker_IgnoresASupersededInstance(t *testing.T) {
	cases := []struct {
		name               string
		stored, deregister string
		wantOffline        bool
	}{
		{"same process", "i2", "i2", true},
		{"superseded process", "i2", "i1", false},
		{"deregister sends no ID", "i2", "", true},
		{"row has no ID", "", "i1", true},
	}
	for _, tc := range cases {
		for name, st := range newStores(t) {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				g := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning},
					stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusRunning}})
				task := g.Tasks["a"][0]
				pool := seedPool(t, st, 1)
				a := seedAttempt(t, st, task, store.AttemptStatusRunning)
				seedClaim(t, st, pool.ID, a.ID)
				registerInstance(t, st, g.Farm.ID, tc.stored)

				reclaimed, offlined, err := st.OfflineWorker(t.Context(), fixtureWorkerID, tc.deregister, time.Now().UTC())
				if err != nil {
					t.Fatalf("OfflineWorker: %v", err)
				}
				if offlined != tc.wantOffline {
					t.Fatalf("OfflineWorker offlined = %v, want %v", offlined, tc.wantOffline)
				}
				w, err := st.GetWorker(t.Context(), fixtureWorkerID)
				if err != nil {
					t.Fatalf("GetWorker: %v", err)
				}
				if !tc.wantOffline {
					if len(reclaimed) != 0 || w.Status != store.WorkerStatusOnline {
						t.Fatalf("superseded deregister: reclaimed %d, worker %q; want 0 and online", len(reclaimed), w.Status)
					}
					if got := mustTask(t, st, task.ID).Status; got != store.TaskStatusRunning {
						t.Fatalf("task = %q, want still running", got)
					}
					if n := activeClaims(t, st, pool.ID); n != 1 {
						t.Fatalf("active claims = %d, want 1", n)
					}
					return
				}
				if len(reclaimed) != 1 || w.Status != store.WorkerStatusOffline {
					t.Fatalf("deregister: reclaimed %d, worker %q; want 1 and offline", len(reclaimed), w.Status)
				}
				if n := activeClaims(t, st, pool.ID); n != 0 {
					t.Fatalf("active claims = %d, want 0", n)
				}
			})
		}
	}
}
