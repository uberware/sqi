// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
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
