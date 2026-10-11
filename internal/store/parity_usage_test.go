// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"errors"
	"slices"
	"testing"

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
