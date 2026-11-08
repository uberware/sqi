// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/storetest"
)

func mkCapacityGroup(farmID, name string) store.CapacityGroup {
	return store.CapacityGroup{
		ID:              uuid.NewString(),
		FarmID:          farmID,
		Name:            name,
		Kind:            store.CapacityGroupSized,
		Provider:        store.CapacityProviderExternal,
		Enabled:         true,
		QueueIDs:        []string{"q-1", "q-2"},
		MinMachines:     1,
		MaxMachines:     20,
		Priority:        5,
		MaxStepUp:       4,
		ScaleUpCooldown: 90 * time.Second,
		IdleTimeout:     15 * time.Minute,
		BootTimeout:     5 * time.Minute,
		RetireAction:    store.CapacityRetireShutdown,
		Profile: store.MachineProfile{
			CPUCount: 32, RAMMb: 131072, GPUCount: 2, GPUMemoryMb: 24576,
			OS: "linux", Arch: "amd64", ComputeLocation: "cloud-west",
			Tags:       map[string]string{"houdini": "20.5", "maya": ""},
			Extensions: []string{"SQI_LOG_CHECKS"},
			ExprLimits: store.WorkerExprLimits{OperationLimit: 20000, MemoryLimit: 4 << 20},
		},
	}
}

func mustCreateCapacityGroup(t *testing.T, st store.Store, g store.CapacityGroup) store.CapacityGroup {
	t.Helper()
	out, err := st.CreateCapacityGroup(t.Context(), g)
	if err != nil {
		t.Fatalf("CreateCapacityGroup %s: %v", g.Name, err)
	}
	return out
}

// sameCapacityGroup compares every stored field of two groups; the timestamps
// are stamped by the store and compared by the caller.
func sameCapacityGroup(t *testing.T, got, want store.CapacityGroup) {
	t.Helper()
	got.CreatedAt, got.UpdatedAt = time.Time{}, time.Time{}
	want.CreatedAt, want.UpdatedAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("group = %+v, want %+v", got, want)
	}
}

func TestParity_CapacityGroupCRUD(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			farm := mustCreateFarm(t, st, "main")

			low := mkCapacityGroup(farm.ID, "alpha")
			low.Priority = 1
			high := mkCapacityGroup(farm.ID, "zeta")
			high.Priority = 9
			highB := mkCapacityGroup(farm.ID, "beta")
			highB.Priority = 9

			created := mustCreateCapacityGroup(t, st, low)
			sameCapacityGroup(t, created, low)
			if created.CreatedAt.IsZero() || !created.UpdatedAt.Equal(created.CreatedAt) {
				t.Fatalf("created timestamps = %v / %v, want both stamped and equal", created.CreatedAt, created.UpdatedAt)
			}
			mustCreateCapacityGroup(t, st, high)
			mustCreateCapacityGroup(t, st, highB)

			// A duplicate name conflicts.
			dup := mkCapacityGroup(farm.ID, "alpha")
			if _, err := st.CreateCapacityGroup(ctx, dup); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("duplicate name: err = %v, want ErrConflict", err)
			}
			// An unknown farm conflicts too.
			if _, err := st.CreateCapacityGroup(ctx, mkCapacityGroup(uuid.NewString(), "orphan")); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("unknown farm: err = %v, want ErrConflict", err)
			}

			// Get and get-by-name round-trip every field.
			got, err := st.GetCapacityGroup(ctx, low.ID)
			if err != nil {
				t.Fatalf("GetCapacityGroup: %v", err)
			}
			sameCapacityGroup(t, got, low)
			if !got.CreatedAt.Equal(created.CreatedAt) {
				t.Fatalf("CreatedAt = %v, want %v", got.CreatedAt, created.CreatedAt)
			}
			byName, err := st.GetCapacityGroupByName(ctx, "alpha")
			if err != nil || byName.ID != low.ID {
				t.Fatalf("GetCapacityGroupByName = %+v, %v, want %s", byName, err, low.ID)
			}
			if _, err := st.GetCapacityGroup(ctx, uuid.NewString()); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown id: err = %v, want ErrNotFound", err)
			}
			if _, err := st.GetCapacityGroupByName(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown name: err = %v, want ErrNotFound", err)
			}

			// Listed by priority descending, then name.
			list, err := st.ListCapacityGroups(ctx)
			if err != nil {
				t.Fatalf("ListCapacityGroups: %v", err)
			}
			var order []string
			for _, g := range list {
				order = append(order, g.Name)
			}
			if strings.Join(order, ",") != "beta,zeta,alpha" {
				t.Fatalf("list order = %v, want beta, zeta, alpha", order)
			}

			// Update replaces the mutable fields, stamps UpdatedAt and keeps the rest.
			time.Sleep(time.Millisecond)
			upd := got
			upd.Name = "alpha-2"
			upd.Kind = store.CapacityGroupNamed
			upd.Provider = store.CapacityProviderWakeOnLAN
			upd.Enabled = false
			upd.QueueIDs = nil
			upd.MinMachines, upd.MaxMachines, upd.Priority, upd.MaxStepUp = 0, 3, 2, 1
			upd.ScaleUpCooldown, upd.IdleTimeout, upd.BootTimeout = 0, 2*time.Minute, 3*time.Minute
			upd.RetireAction = store.CapacityRetireSleep
			upd.Profile = store.MachineProfile{OS: "windows"}
			updated, err := st.UpdateCapacityGroup(ctx, upd)
			if err != nil {
				t.Fatalf("UpdateCapacityGroup: %v", err)
			}
			sameCapacityGroup(t, updated, upd)
			if !updated.CreatedAt.Equal(created.CreatedAt) || !updated.UpdatedAt.After(created.UpdatedAt) {
				t.Fatalf("update timestamps: created %v updated %v, want created kept and updated advanced", updated.CreatedAt, updated.UpdatedAt)
			}
			reread, err := st.GetCapacityGroup(ctx, low.ID)
			if err != nil {
				t.Fatalf("GetCapacityGroup after update: %v", err)
			}
			sameCapacityGroup(t, reread, upd)

			// Renaming onto another group's name conflicts; an unknown ID is not found.
			clash := reread
			clash.Name = "zeta"
			if _, err := st.UpdateCapacityGroup(ctx, clash); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("rename onto taken name: err = %v, want ErrConflict", err)
			}
			ghost := reread
			ghost.ID = uuid.NewString()
			if _, err := st.UpdateCapacityGroup(ctx, ghost); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("update unknown id: err = %v, want ErrNotFound", err)
			}

			// Delete removes the group; deleting it again is not found.
			if err := st.DeleteCapacityGroup(ctx, low.ID); err != nil {
				t.Fatalf("DeleteCapacityGroup: %v", err)
			}
			if _, err := st.GetCapacityGroup(ctx, low.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("get after delete: err = %v, want ErrNotFound", err)
			}
			if err := st.DeleteCapacityGroup(ctx, low.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("second delete: err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestParity_DeleteFarmWithCapacityGroupConflicts(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			farm := mustCreateFarm(t, st, "main")
			g := mustCreateCapacityGroup(t, st, mkCapacityGroup(farm.ID, "alpha"))
			if err := st.DeleteFarm(ctx, farm.ID); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("DeleteFarm with a group: err = %v, want ErrConflict", err)
			}
			if err := st.DeleteCapacityGroup(ctx, g.ID); err != nil {
				t.Fatalf("DeleteCapacityGroup: %v", err)
			}
			if err := st.DeleteFarm(ctx, farm.ID); err != nil {
				t.Fatalf("DeleteFarm after the group is gone: %v", err)
			}
		})
	}
}

func TestCapacityGroup_Validate(t *testing.T) {
	valid := mkCapacityGroup("farm", "ok")
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid group: %v", err)
	}
	longName := "a" + strings.Repeat("b", 62)
	tests := []struct {
		name   string
		mutate func(*store.CapacityGroup)
		want   string // substring of the error; empty = valid
	}{
		{"name 63 chars", func(g *store.CapacityGroup) { g.Name = longName }, ""},
		{"name with dot underscore dash", func(g *store.CapacityGroup) { g.Name = "a.b_c-d" }, ""},
		{"name empty", func(g *store.CapacityGroup) { g.Name = "" }, "name"},
		{"name 64 chars", func(g *store.CapacityGroup) { g.Name = longName + "c" }, "name"},
		{"name leading dash", func(g *store.CapacityGroup) { g.Name = "-a" }, "name"},
		{"name with space", func(g *store.CapacityGroup) { g.Name = "a b" }, "name"},
		{"kind named", func(g *store.CapacityGroup) { g.Kind = store.CapacityGroupNamed }, ""},
		{"kind unknown", func(g *store.CapacityGroup) { g.Kind = "pooled" }, "kind"},
		{"provider wake_on_lan", func(g *store.CapacityGroup) { g.Provider = "wake_on_lan" }, ""},
		{"provider script", func(g *store.CapacityGroup) { g.Provider = "script:boot" }, ""},
		{"provider empty script name", func(g *store.CapacityGroup) { g.Provider = "script:" }, "provider"},
		{"provider unknown", func(g *store.CapacityGroup) { g.Provider = "aws" }, "provider"},
		{"min negative", func(g *store.CapacityGroup) { g.MinMachines = -1 }, "min_machines"},
		{"min equals max", func(g *store.CapacityGroup) { g.MinMachines, g.MaxMachines = 5, 5 }, ""},
		{"min above max", func(g *store.CapacityGroup) { g.MinMachines, g.MaxMachines = 6, 5 }, "min_machines"},
		{"max zero", func(g *store.CapacityGroup) { g.MinMachines, g.MaxMachines = 0, 0 }, "max_machines"},
		{"max 10000", func(g *store.CapacityGroup) { g.MaxMachines = 10000 }, ""},
		{"max 10001", func(g *store.CapacityGroup) { g.MaxMachines = 10001 }, "max_machines"},
		{"step up zero", func(g *store.CapacityGroup) { g.MaxStepUp = 0 }, "max_step_up"},
		{"idle 1m", func(g *store.CapacityGroup) { g.IdleTimeout = time.Minute }, ""},
		{"idle under 1m", func(g *store.CapacityGroup) { g.IdleTimeout = time.Minute - 1 }, "idle_timeout"},
		{"boot 1m", func(g *store.CapacityGroup) { g.BootTimeout = time.Minute }, ""},
		{"boot under 1m", func(g *store.CapacityGroup) { g.BootTimeout = 0 }, "boot_timeout"},
		{"cooldown zero", func(g *store.CapacityGroup) { g.ScaleUpCooldown = 0 }, ""},
		{"cooldown negative", func(g *store.CapacityGroup) { g.ScaleUpCooldown = -time.Second }, "scale_up_cooldown"},
		{"sized without cpu", func(g *store.CapacityGroup) { g.Profile.CPUCount = 0 }, "cpu_count"},
		{"named without cpu", func(g *store.CapacityGroup) { g.Kind, g.Profile.CPUCount = store.CapacityGroupNamed, 0 }, ""},
		{"retire sleep", func(g *store.CapacityGroup) { g.RetireAction = "sleep" }, ""},
		{"retire unknown", func(g *store.CapacityGroup) { g.RetireAction = "hibernate" }, "retire_action"},
		{"retire empty", func(g *store.CapacityGroup) { g.RetireAction = "" }, "retire_action"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := mkCapacityGroup("farm", "ok")
			tc.mutate(&g)
			err := g.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("Validate = %v, want nil", err)
			case tc.want != "" && err == nil:
				t.Fatalf("Validate = nil, want an error naming %q", tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("Validate = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

// capacityWorker registers a worker in farmID with the given liveness and
// heartbeat time.
func capacityWorker(t *testing.T, st store.Store, farmID, id, hostname string, status store.WorkerStatus, beat time.Time) store.Worker {
	t.Helper()
	beat = beat.UTC()
	w, _, err := st.RegisterWorker(t.Context(), store.Worker{
		ID: id, FarmID: farmID, Hostname: hostname, Status: status, LastHeartbeatAt: &beat,
	})
	if err != nil {
		t.Fatalf("RegisterWorker %s: %v", id, err)
	}
	return w
}

func TestParity_SetWorkerCapacityGroup(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			farm := mustCreateFarm(t, st, "main")
			a := mustCreateCapacityGroup(t, st, mkCapacityGroup(farm.ID, "alpha"))
			b := mustCreateCapacityGroup(t, st, mkCapacityGroup(farm.ID, "beta"))
			w := capacityWorker(t, st, farm.ID, "w-1", "node", store.WorkerStatusOnline, time.Now())
			if w.CapacityGroupID != "" || w.Ephemeral {
				t.Fatalf("new worker = group %q ephemeral %v, want none", w.CapacityGroupID, w.Ephemeral)
			}

			got, err := st.SetWorkerCapacityGroup(ctx, w.ID, a.ID, true)
			if err != nil || got.CapacityGroupID != a.ID || !got.Ephemeral {
				t.Fatalf("set = (%+v, %v), want group %s ephemeral", got, err, a.ID)
			}
			if stored := mustWorker(t, st, w.ID); stored.CapacityGroupID != a.ID || !stored.Ephemeral {
				t.Fatalf("stored = group %q ephemeral %v, want %s ephemeral", stored.CapacityGroupID, stored.Ephemeral, a.ID)
			}

			// Setting the same group again is not an error and may change ephemeral.
			got, err = st.SetWorkerCapacityGroup(ctx, w.ID, a.ID, false)
			if err != nil || got.CapacityGroupID != a.ID || got.Ephemeral {
				t.Fatalf("re-set = (%+v, %v), want group %s, not ephemeral", got, err, a.ID)
			}

			// A different group conflicts and changes nothing.
			if _, err := st.SetWorkerCapacityGroup(ctx, w.ID, b.ID, true); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("different group: err = %v, want ErrConflict", err)
			}
			if stored := mustWorker(t, st, w.ID); stored.CapacityGroupID != a.ID || stored.Ephemeral {
				t.Fatalf("after the conflict = group %q ephemeral %v, want %s, not ephemeral", stored.CapacityGroupID, stored.Ephemeral, a.ID)
			}

			// Unknown worker and unknown group are not found.
			if _, err := st.SetWorkerCapacityGroup(ctx, "ghost", a.ID, false); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown worker: err = %v, want ErrNotFound", err)
			}
			if _, err := st.SetWorkerCapacityGroup(ctx, w.ID, uuid.NewString(), false); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown group: err = %v, want ErrNotFound", err)
			}

			// An empty group clears the membership and ephemeral, and then another group may be set.
			if _, err := st.SetWorkerCapacityGroup(ctx, w.ID, a.ID, true); err != nil {
				t.Fatalf("set ephemeral: %v", err)
			}
			got, err = st.SetWorkerCapacityGroup(ctx, w.ID, "", true)
			if err != nil || got.CapacityGroupID != "" || got.Ephemeral {
				t.Fatalf("clear = (%+v, %v), want no group and not ephemeral", got, err)
			}
			if _, err := st.SetWorkerCapacityGroup(ctx, "ghost", "", false); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("clear unknown worker: err = %v, want ErrNotFound", err)
			}
			got, err = st.SetWorkerCapacityGroup(ctx, w.ID, b.ID, false)
			if err != nil || got.CapacityGroupID != b.ID {
				t.Fatalf("set after clear = (%+v, %v), want group %s", got, err, b.ID)
			}
		})
	}
}

func TestParity_ListCapacityGroupMembers(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			farm := mustCreateFarm(t, st, "main")
			a := mustCreateCapacityGroup(t, st, mkCapacityGroup(farm.ID, "alpha"))
			b := mustCreateCapacityGroup(t, st, mkCapacityGroup(farm.ID, "beta"))
			now := time.Now()
			for _, m := range []struct{ id, host, group string }{
				{"w-1", "zulu", a.ID}, {"w-2", "alpha", a.ID}, {"w-3", "mike", b.ID}, {"w-4", "kilo", ""},
			} {
				capacityWorker(t, st, farm.ID, m.id, m.host, store.WorkerStatusOnline, now)
				if m.group != "" {
					if _, err := st.SetWorkerCapacityGroup(ctx, m.id, m.group, false); err != nil {
						t.Fatalf("SetWorkerCapacityGroup %s: %v", m.id, err)
					}
				}
			}

			members, err := st.ListCapacityGroupMembers(ctx, a.ID)
			if err != nil {
				t.Fatalf("ListCapacityGroupMembers: %v", err)
			}
			ids := project(members, func(w store.Worker) string { return w.ID })
			if strings.Join(ids, ",") != "w-2,w-1" {
				t.Fatalf("members of alpha = %v, want w-2, w-1 (by hostname)", ids)
			}
			if none, err := st.ListCapacityGroupMembers(ctx, uuid.NewString()); err != nil || len(none) != 0 {
				t.Fatalf("members of an unknown group = (%v, %v), want none", none, err)
			}

			page, err := st.ListWorkers(ctx, store.ListWorkersOptions{CapacityGroupID: b.ID, Pagination: store.Pagination{Limit: 10}})
			if err != nil || page.Total != 1 || page.Items[0].ID != "w-3" {
				t.Fatalf("ListWorkers by group = (%+v, %v), want only w-3", page, err)
			}
		})
	}
}

func TestParity_RegisterWorkerKeepsMembership(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			farm := mustCreateFarm(t, st, "main")
			g := mustCreateCapacityGroup(t, st, mkCapacityGroup(farm.ID, "alpha"))
			beat := time.Now().UTC()
			reg := func(instance string) store.Worker {
				w, _, err := st.RegisterWorker(ctx, store.Worker{
					ID: "w-1", FarmID: farm.ID, Hostname: "node", Status: store.WorkerStatusOnline,
					LastHeartbeatAt: &beat, InstanceID: instance,
				})
				if err != nil {
					t.Fatalf("RegisterWorker %s: %v", instance, err)
				}
				return w
			}
			reg("A")
			if _, err := st.SetWorkerCapacityGroup(ctx, "w-1", g.ID, true); err != nil {
				t.Fatalf("SetWorkerCapacityGroup: %v", err)
			}

			// A registration never carries the membership, and never clears it.
			got := reg("B")
			if got.CapacityGroupID != g.ID || !got.Ephemeral {
				t.Fatalf("after re-registration = group %q ephemeral %v, want %s ephemeral", got.CapacityGroupID, got.Ephemeral, g.ID)
			}
			if stored := mustWorker(t, st, "w-1"); stored.CapacityGroupID != g.ID || !stored.Ephemeral || stored.InstanceID != "B" {
				t.Fatalf("stored = group %q ephemeral %v instance %q, want %s ephemeral B", stored.CapacityGroupID, stored.Ephemeral, stored.InstanceID, g.ID)
			}
		})
	}
}

func TestParity_DeleteEphemeralWorker(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			now := time.Now().UTC()
			g := seedGraph(t, st, graphOpts{jobStatus: store.JobStatusRunning},
				stepSpec{name: "a", status: store.StepStatusReady, tasks: []store.TaskStatus{store.TaskStatusRunning}})
			group := mustCreateCapacityGroup(t, st, mkCapacityGroup(g.Farm.ID, "alpha"))

			// expectKept asserts a refused delete changed nothing.
			expectKept := func(id, why string) {
				t.Helper()
				w, deleted, err := st.DeleteEphemeralWorker(ctx, id, now)
				if err != nil || deleted || w.ID != "" {
					t.Fatalf("%s: DeleteEphemeralWorker = (%+v, %v, %v), want (zero, false, nil)", why, w, deleted, err)
				}
				if _, err := st.GetWorker(ctx, id); err != nil {
					t.Fatalf("%s: GetWorker: %v, want the worker kept", why, err)
				}
			}

			// Online.
			capacityWorker(t, st, g.Farm.ID, "w-online", "a", store.WorkerStatusOnline, now)
			mustCreateCredential(t, st, mkCredential("w-online"))
			if _, err := st.SetWorkerCapacityGroup(ctx, "w-online", group.ID, true); err != nil {
				t.Fatalf("SetWorkerCapacityGroup: %v", err)
			}
			expectKept("w-online", "online")
			if _, err := storetest.ActiveWorkerCredential(ctx, st, "w-online"); err != nil {
				t.Fatalf("credential of the kept worker: %v, want it untouched", err)
			}

			// Offline but not ephemeral, and offline but in no group.
			capacityWorker(t, st, g.Farm.ID, "w-named", "b", store.WorkerStatusOffline, now)
			if _, err := st.SetWorkerCapacityGroup(ctx, "w-named", group.ID, false); err != nil {
				t.Fatalf("SetWorkerCapacityGroup: %v", err)
			}
			expectKept("w-named", "not ephemeral")
			capacityWorker(t, st, g.Farm.ID, "w-loose", "c", store.WorkerStatusOffline, now)
			expectKept("w-loose", "no group")

			// Unknown.
			if w, deleted, err := st.DeleteEphemeralWorker(ctx, "ghost", now); err != nil || deleted || w.ID != "" {
				t.Fatalf("unknown worker: (%+v, %v, %v), want (zero, false, nil)", w, deleted, err)
			}

			// Ephemeral and offline, but holding a leased task (the fixture's w1).
			capacityWorker(t, st, g.Farm.ID, fixtureWorkerID, "d", store.WorkerStatusOffline, now)
			if _, err := st.SetWorkerCapacityGroup(ctx, fixtureWorkerID, group.ID, true); err != nil {
				t.Fatalf("SetWorkerCapacityGroup: %v", err)
			}
			expectKept(fixtureWorkerID, "holds a running task")

			// Ephemeral, offline and idle: deleted, with its credential revoked.
			capacityWorker(t, st, g.Farm.ID, "w-gone", "e", store.WorkerStatusOffline, now)
			mustCreateCredential(t, st, mkCredential("w-gone"))
			if _, err := st.SetWorkerCapacityGroup(ctx, "w-gone", group.ID, true); err != nil {
				t.Fatalf("SetWorkerCapacityGroup: %v", err)
			}
			w, deleted, err := st.DeleteEphemeralWorker(ctx, "w-gone", now)
			if err != nil || !deleted || w.ID != "w-gone" || w.CapacityGroupID != group.ID || !w.Ephemeral {
				t.Fatalf("DeleteEphemeralWorker = (%+v, %v, %v), want the deleted ephemeral member", w, deleted, err)
			}
			if _, err := st.GetWorker(ctx, "w-gone"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetWorker after delete: err = %v, want ErrNotFound", err)
			}
			if _, err := storetest.ActiveWorkerCredential(ctx, st, "w-gone"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("credential after delete: err = %v, want ErrNotFound (revoked)", err)
			}

			// An ephemeral worker with no credential is still deleted.
			capacityWorker(t, st, g.Farm.ID, "w-nocred", "f", store.WorkerStatusOffline, now)
			if _, err := st.SetWorkerCapacityGroup(ctx, "w-nocred", group.ID, true); err != nil {
				t.Fatalf("SetWorkerCapacityGroup: %v", err)
			}
			if _, deleted, err := st.DeleteEphemeralWorker(ctx, "w-nocred", now); err != nil || !deleted {
				t.Fatalf("no credential: deleted = %v, err = %v, want true, nil", deleted, err)
			}
		})
	}
}

func TestParity_DeleteCapacityGroupWithMemberConflicts(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			farm := mustCreateFarm(t, st, "main")
			g := mustCreateCapacityGroup(t, st, mkCapacityGroup(farm.ID, "alpha"))
			capacityWorker(t, st, farm.ID, "w-1", "node", store.WorkerStatusOnline, time.Now())
			if _, err := st.SetWorkerCapacityGroup(ctx, "w-1", g.ID, false); err != nil {
				t.Fatalf("SetWorkerCapacityGroup: %v", err)
			}
			if err := st.DeleteCapacityGroup(ctx, g.ID); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("delete with a member: err = %v, want ErrConflict", err)
			}
			if _, err := st.GetCapacityGroup(ctx, g.ID); err != nil {
				t.Fatalf("GetCapacityGroup: %v, want the group kept", err)
			}
			// Once the member leaves, the group can go.
			if _, err := st.SetWorkerCapacityGroup(ctx, "w-1", "", false); err != nil {
				t.Fatalf("clear: %v", err)
			}
			if err := st.DeleteCapacityGroup(ctx, g.ID); err != nil {
				t.Fatalf("delete after the member left: %v", err)
			}
		})
	}
}

func TestParity_RetentionKeepsNonEphemeralGroupMembers(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			farm := mustCreateFarm(t, st, "main")
			group := mustCreateCapacityGroup(t, st, mkCapacityGroup(farm.ID, "alpha"))
			old := time.Now().Add(-time.Hour)
			capacityWorker(t, st, farm.ID, "w-named", "a", store.WorkerStatusOffline, old)
			capacityWorker(t, st, farm.ID, "w-eph", "b", store.WorkerStatusOffline, old)
			capacityWorker(t, st, farm.ID, "w-loose", "c", store.WorkerStatusOffline, old)
			for id, ephemeral := range map[string]bool{"w-named": false, "w-eph": true} {
				if _, err := st.SetWorkerCapacityGroup(ctx, id, group.ID, ephemeral); err != nil {
					t.Fatalf("SetWorkerCapacityGroup %s: %v", id, err)
				}
			}

			removed, err := st.DeleteOfflineWorkersBefore(ctx, time.Now())
			if err != nil {
				t.Fatalf("DeleteOfflineWorkersBefore: %v", err)
			}
			got := project(removed, func(w store.Worker) string { return w.ID })
			slices.Sort(got)
			if want := []string{"w-eph", "w-loose"}; !slices.Equal(got, want) {
				t.Fatalf("removed = %v, want %v", got, want)
			}
			if _, err := st.GetWorker(ctx, "w-named"); err != nil {
				t.Fatalf("GetWorker w-named: %v, want the non-ephemeral member kept", err)
			}
		})
	}
}
