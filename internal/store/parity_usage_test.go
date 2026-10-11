// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/storetest"
)

func mustCreatePool(t *testing.T, st store.Store, name string, maxConcurrent int) store.UsagePool {
	t.Helper()
	p, err := st.CreateUsagePool(t.Context(), store.UsagePool{
		ID: uuid.NewString(), Name: name, ServerHint: "10.0.0.50:5053", MaxConcurrent: maxConcurrent,
	})
	if err != nil {
		t.Fatalf("CreateUsagePool %s: %v", name, err)
	}
	return p
}

func TestParity_CreateUsagePool(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			want := store.UsagePool{
				ID: uuid.NewString(), Name: "nuke", ServerHint: "10.0.0.50:5053", MaxConcurrent: 4,
			}
			got, err := st.CreateUsagePool(ctx, want)
			if err != nil {
				t.Fatalf("CreateUsagePool: %v", err)
			}
			if got.ID != want.ID || got.Name != "nuke" || got.ServerHint != "10.0.0.50:5053" || got.MaxConcurrent != 4 {
				t.Fatalf("CreateUsagePool returned (%+v), want fields persisted", got)
			}
			// Verify stored.
			stored, err := st.GetUsagePool(ctx, want.ID)
			if err != nil || stored.Name != "nuke" {
				t.Fatalf("GetUsagePool after CreateUsagePool: %v / %s, want success and name nuke", err, stored.Name)
			}
			// Duplicate name: ErrConflict.
			if _, err := st.CreateUsagePool(ctx, store.UsagePool{ID: uuid.NewString(), Name: "nuke", MaxConcurrent: 5}); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("duplicate name: err = %v, want ErrConflict", err)
			}
		})
	}
}

func TestParity_UpdateUsagePool(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			orig := mustCreatePool(t, st, "nuke", 4)
			_, later := subSecond()
			time.Sleep(time.Millisecond)

			upd := orig
			upd.Name = "nuke-updated"
			upd.ServerHint = "10.0.0.100:5053"
			upd.MaxConcurrent = 10
			upd.UpdatedAt = later // Should be updated to a fresh time.

			got, err := st.UpdateUsagePool(ctx, upd)
			if err != nil {
				t.Fatalf("UpdateUsagePool: %v", err)
			}
			if got.Name != "nuke-updated" || got.ServerHint != "10.0.0.100:5053" || got.MaxConcurrent != 10 {
				t.Fatalf("UpdateUsagePool returned (%+v), want mutable fields updated", got)
			}
			if !got.CreatedAt.Equal(orig.CreatedAt) || got.UpdatedAt.Before(orig.UpdatedAt) {
				t.Fatalf("UpdateUsagePool: CreatedAt changed or UpdatedAt not advanced")
			}
			// Verify stored.
			stored, err := st.GetUsagePool(ctx, orig.ID)
			if err != nil || stored.Name != "nuke-updated" {
				t.Fatalf("GetUsagePool after UpdateUsagePool: %v / %s, want success and name nuke-updated", err, stored.Name)
			}
			// Unknown ID: ErrNotFound.
			if _, err := st.UpdateUsagePool(ctx, store.UsagePool{ID: uuid.NewString(), Name: "foo", MaxConcurrent: 1}); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown id: err = %v, want ErrNotFound", err)
			}
			// Rename to conflicting name: ErrConflict.
			mustCreatePool(t, st, "taken", 5)
			if _, err := st.UpdateUsagePool(ctx, store.UsagePool{ID: orig.ID, Name: "taken", MaxConcurrent: 1}); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("rename to taken name: err = %v, want ErrConflict", err)
			}
		})
	}
}

func TestParity_DeleteUsagePool(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			pool := mustCreatePool(t, st, "nuke", 4)
			keep := mustCreatePool(t, st, "kept", 5)
			if err := st.DeleteUsagePool(ctx, pool.ID); err != nil {
				t.Fatalf("DeleteUsagePool: %v", err)
			}
			if _, err := st.GetUsagePool(ctx, pool.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetUsagePool after delete: err = %v, want ErrNotFound", err)
			}
			if err := st.DeleteUsagePool(ctx, pool.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("second delete: err = %v, want ErrNotFound", err)
			}
			if _, err := st.GetUsagePool(ctx, keep.ID); err != nil {
				t.Fatalf("other pool: %v, want it kept", err)
			}
		})
	}
}

func TestParity_GetUsagePool(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			p := mustCreatePool(t, st, "nuke", 4)
			got, err := st.GetUsagePool(t.Context(), p.ID)
			if err != nil || got.ID != p.ID || got.Name != "nuke" || got.ServerHint != "10.0.0.50:5053" || got.MaxConcurrent != 4 {
				t.Fatalf("GetUsagePool = (%+v, %v), want %+v", got, err, p)
			}
			if _, err := st.GetUsagePool(t.Context(), uuid.NewString()); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown id: err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestParity_ListUsagePools(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			if got, err := st.ListUsagePools(t.Context()); err != nil || len(got) != 0 {
				t.Fatalf("empty store: ListUsagePools = (%+v, %v), want none", got, err)
			}
			for _, n := range []string{"nuke", "houdini", "maya"} {
				mustCreatePool(t, st, n, 1)
			}
			got, err := st.ListUsagePools(t.Context())
			if err != nil {
				t.Fatalf("ListUsagePools: %v", err)
			}
			var names []string
			for _, p := range got {
				names = append(names, p.Name)
			}
			if want := []string{"houdini", "maya", "nuke"}; !slices.Equal(names, want) {
				t.Fatalf("names = %v, want %v (ordered by name)", names, want)
			}
		})
	}
}

func TestParity_ListUsagePoolUtilization(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			if got, err := st.ListUsagePoolUtilization(ctx); err != nil || len(got) != 0 {
				t.Fatalf("empty store: ListUsagePoolUtilization = (%+v, %v), want none", got, err)
			}
			nuke, maya := mustCreatePool(t, st, "nuke", 5), mustCreatePool(t, st, "maya", 3)
			mustCreatePool(t, st, "houdini", 2)
			g := seedStepA(t, st, graphOpts{}, store.TaskStatusReady, store.TaskStatusReady, store.TaskStatusReady)
			// Two active claims on nuke; maya's one claim is released when its
			// attempt completes.
			holdClaims(t, st, nuke, g.Tasks["a"][0], g.Tasks["a"][1])
			done := leaseClaiming(t, st, g.Tasks["a"][2].ID, true, maya)
			storetest.Complete(t, st, done, store.TaskStatusSucceeded)

			got, err := st.ListUsagePoolUtilization(ctx)
			if err != nil {
				t.Fatalf("ListUsagePoolUtilization: %v", err)
			}
			type row struct {
				name       string
				inUse, max int
			}
			var rows []row
			for _, u := range got {
				rows = append(rows, row{u.Name, u.InUse, u.MaxConcurrent})
			}
			want := []row{{"houdini", 0, 2}, {"maya", 0, 3}, {"nuke", 2, 5}}
			if !slices.Equal(rows, want) {
				t.Fatalf("utilization = %+v, want %+v (ordered by name)", rows, want)
			}
		})
	}
}
