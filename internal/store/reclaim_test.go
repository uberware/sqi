// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/storetest"
)

func TestReclaimStaleAssignedTasks_ClosesAndReleases(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{}, stepSpec{
				name: "a", status: store.StepStatusReady,
				tasks: []store.TaskStatus{store.TaskStatusReady, store.TaskStatusReady},
			})
			pool := seedPool(t, st, 0)
			stale := leaseClaiming(t, st, g.Tasks["a"][0].ID, false, pool)
			live := leaseClaiming(t, st, g.Tasks["a"][1].ID, true, pool)

			got, err := st.ReclaimStaleAssignedTasks(t.Context(), time.Now().UTC().Add(time.Minute))
			if err != nil || len(got) != 1 || got[0].ID != g.Tasks["a"][0].ID {
				t.Fatalf("ReclaimStaleAssignedTasks = (%+v, %v), want only the assigned task", got, err)
			}
			if a := mustAttempt(t, st, stale.ID); a.Status != store.AttemptStatusFailed || a.EndedAt == nil {
				t.Fatalf("reaped attempt = %+v, want failed with ended_at", a)
			}
			if a := mustAttempt(t, st, live.ID); a.Status != store.AttemptStatusRunning {
				t.Fatalf("running task's attempt = %q, want untouched", a.Status)
			}
			if n := activeClaims(t, st, pool.ID); n != 1 {
				t.Fatalf("active claims = %d, want 1 (only the running task's)", n)
			}
			if v := storetest.ClaimViolations(t, st); len(v) != 0 {
				t.Fatalf("I3 violations: %v", v)
			}
		})
	}
}

// TestReclaimStaleAssignedTasks_ReturnsPostResetRows pins invariant I2: the
// returned rows are the rows as the UPDATE left them, on both backends, so a
// caller can never read a worker or status the reset already cleared.
func TestReclaimStaleAssignedTasks_ReturnsPostResetRows(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{}, stepSpec{
				name: "a", status: store.StepStatusReady,
				tasks: []store.TaskStatus{store.TaskStatusAssigned},
			})

			got, err := st.ReclaimStaleAssignedTasks(t.Context(), time.Now().UTC().Add(time.Minute))
			if err != nil || len(got) != 1 {
				t.Fatalf("ReclaimStaleAssignedTasks = (%+v, %v), want one task", got, err)
			}
			r := got[0]
			if r.Status != store.TaskStatusReady || r.AssignedWorkerID != "" || r.AssignedAt != nil {
				t.Fatalf("returned row = status %q worker %q assigned_at %v, want the post-reset row",
					r.Status, r.AssignedWorkerID, r.AssignedAt)
			}
			stored := mustTask(t, st, g.Tasks["a"][0].ID)
			if stored.Status != r.Status || stored.AssignedWorkerID != r.AssignedWorkerID || !stored.UpdatedAt.Equal(r.UpdatedAt) {
				t.Fatalf("returned row %+v differs from the stored row %+v", r, stored)
			}
		})
	}
}

// TestReclaimStaleAssignedTasks_OnlyStaleAndOnlyAssigned checks the selection:
// a fresh assignment keeps its status and its open attempt, and a second call
// finds nothing, so it returns nothing and writes nothing.
func TestReclaimStaleAssignedTasks_OnlyStaleAndOnlyAssigned(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{}, stepSpec{
				name: "a", status: store.StepStatusReady,
				tasks: []store.TaskStatus{store.TaskStatusAssigned, store.TaskStatusReady},
			})
			fresh := g.Attempts[g.Tasks["a"][0].ID]

			got, err := st.ReclaimStaleAssignedTasks(t.Context(), time.Now().UTC().Add(-time.Hour))
			if err != nil || len(got) != 0 {
				t.Fatalf("ReclaimStaleAssignedTasks with an old cutoff = (%+v, %v), want nothing", got, err)
			}
			if mustTask(t, st, g.Tasks["a"][0].ID).Status != store.TaskStatusAssigned {
				t.Fatal("a fresh assignment was reclaimed")
			}
			if a := mustAttempt(t, st, fresh.ID); a.Status != store.AttemptStatusRunning {
				t.Fatalf("a fresh assignment's attempt = %q, want running", a.Status)
			}

			cutoff := time.Now().UTC().Add(time.Minute)
			if got, err = st.ReclaimStaleAssignedTasks(t.Context(), cutoff); err != nil || len(got) != 1 {
				t.Fatalf("first reclaim = (%+v, %v), want the one assigned task", got, err)
			}
			if got, err = st.ReclaimStaleAssignedTasks(t.Context(), cutoff); err != nil || len(got) != 0 {
				t.Fatalf("second reclaim = (%+v, %v), want nothing", got, err)
			}
		})
	}
}

// TestReclaimStaleAssignedTasks_ReleasesLeakedClaimOfClosedAttempt: the claims
// released are those of every closed attempt of a reclaimed task, so a claim an
// earlier close leaked on a finished attempt is repaired when the task is
// reaped (I3), and a pool slot is never left held by a task that is ready again.
func TestReclaimStaleAssignedTasks_ReleasesLeakedClaimOfClosedAttempt(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			g := seedGraph(t, st, graphOpts{}, stepSpec{
				name: "a", status: store.StepStatusReady,
				tasks: []store.TaskStatus{store.TaskStatusReady},
			})
			task := g.Tasks["a"][0]
			pool := seedPool(t, st, 0)
			// A failed first attempt and a second lease, through production;
			// then the claim the first attempt's close leaked, which production
			// cannot leave behind and is therefore injected.
			earlier := storetest.FailAndRequeue(t, st, leaseReq(task), time.Now().UTC().Add(-time.Minute))
			storetest.Lease(t, st, store.LeaseRequest{TaskID: task.ID, WorkerID: fixtureWorkerID})
			seedClaim(t, st, pool.ID, earlier.ID)
			if n := activeClaims(t, st, pool.ID); n != 1 {
				t.Fatalf("fixture: active claims = %d, want 1", n)
			}

			if got, err := st.ReclaimStaleAssignedTasks(t.Context(), time.Now().UTC().Add(time.Minute)); err != nil || len(got) != 1 {
				t.Fatalf("ReclaimStaleAssignedTasks = (%+v, %v), want one task", got, err)
			}
			if n := activeClaims(t, st, pool.ID); n != 0 {
				t.Fatalf("active claims = %d, want 0", n)
			}
		})
	}
}
