// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
)

func mkComputeLocation(name string) store.ComputeLocation {
	return store.ComputeLocation{ID: uuid.NewString(), Name: name, Description: name + " desc"}
}

func mustCreateComputeLocation(t *testing.T, st store.Store, name string) store.ComputeLocation {
	t.Helper()
	loc, err := st.CreateComputeLocation(t.Context(), mkComputeLocation(name))
	if err != nil {
		t.Fatalf("CreateComputeLocation %s: %v", name, err)
	}
	return loc
}

func TestParity_CreateComputeLocation(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			want := mkComputeLocation("gpu")
			got, err := st.CreateComputeLocation(t.Context(), want)
			if err != nil || got.ID != want.ID || got.Name != want.Name || got.Description != want.Description {
				t.Fatalf("CreateComputeLocation = (%+v, %v), want %+v", got, err, want)
			}
			if _, err := st.CreateComputeLocation(t.Context(), mkComputeLocation("gpu")); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("duplicate name: err = %v, want ErrConflict", err)
			}
		})
	}
}

func TestParity_GetComputeLocation(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			loc := mustCreateComputeLocation(t, st, "gpu")
			got, err := st.GetComputeLocation(t.Context(), loc.ID)
			if err != nil || got.ID != loc.ID || got.Name != "gpu" || got.Description != loc.Description {
				t.Fatalf("GetComputeLocation = (%+v, %v), want %+v", got, err, loc)
			}
			if _, err := st.GetComputeLocation(t.Context(), uuid.NewString()); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown id: err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestParity_GetComputeLocationByName(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			loc := mustCreateComputeLocation(t, st, "gpu")
			got, err := st.GetComputeLocationByName(t.Context(), "gpu")
			if err != nil || got.ID != loc.ID {
				t.Fatalf("GetComputeLocationByName = (%+v, %v), want %s", got, err, loc.ID)
			}
			// The match is exact: a name differing only in case is another name.
			if _, err := st.GetComputeLocationByName(t.Context(), "GPU"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("case-differing name: err = %v, want ErrNotFound", err)
			}
			if _, err := st.GetComputeLocationByName(t.Context(), "nowhere"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown name: err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestParity_ListComputeLocations(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			if got, err := st.ListComputeLocations(t.Context()); err != nil || len(got) != 0 {
				t.Fatalf("empty store: ListComputeLocations = (%+v, %v), want none", got, err)
			}
			for _, n := range []string{"charlie", "alpha", "bravo"} {
				mustCreateComputeLocation(t, st, n)
			}
			got, err := st.ListComputeLocations(t.Context())
			if err != nil {
				t.Fatalf("ListComputeLocations: %v", err)
			}
			var names []string
			for _, l := range got {
				names = append(names, l.Name)
			}
			if want := []string{"alpha", "bravo", "charlie"}; !slices.Equal(names, want) {
				t.Fatalf("names = %v, want %v (ordered by name)", names, want)
			}
		})
	}
}

func TestParity_UpdateComputeLocation(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			loc := mustCreateComputeLocation(t, st, "gpu")
			mustCreateComputeLocation(t, st, "cpu")

			upd := loc
			upd.Name, upd.Description = "gpu-2", "renamed"
			got, err := st.UpdateComputeLocation(ctx, upd)
			if err != nil || got.ID != loc.ID || got.Name != "gpu-2" || got.Description != "renamed" {
				t.Fatalf("UpdateComputeLocation = (%+v, %v), want renamed", got, err)
			}
			if !got.CreatedAt.Equal(loc.CreatedAt) || got.UpdatedAt.IsZero() || got.UpdatedAt.Before(loc.UpdatedAt) {
				t.Fatalf("timestamps = created %v updated %v, want created %v kept and updated advanced from %v",
					got.CreatedAt, got.UpdatedAt, loc.CreatedAt, loc.UpdatedAt)
			}
			if stored, err := st.GetComputeLocation(ctx, loc.ID); err != nil || stored.Name != "gpu-2" {
				t.Fatalf("GetComputeLocation after update = (%+v, %v), want gpu-2", stored, err)
			}

			upd.Name = "cpu"
			if _, err := st.UpdateComputeLocation(ctx, upd); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("rename onto a taken name: err = %v, want ErrConflict", err)
			}
			if _, err := st.UpdateComputeLocation(ctx, mkComputeLocation("ghost")); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown id: err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestParity_DeleteComputeLocation(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			loc := mustCreateComputeLocation(t, st, "gpu")
			keep := mustCreateComputeLocation(t, st, "cpu")
			if err := st.DeleteComputeLocation(ctx, loc.ID); err != nil {
				t.Fatalf("DeleteComputeLocation: %v", err)
			}
			if _, err := st.GetComputeLocation(ctx, loc.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetComputeLocation after delete: err = %v, want ErrNotFound", err)
			}
			if err := st.DeleteComputeLocation(ctx, loc.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("second delete: err = %v, want ErrNotFound", err)
			}
			if _, err := st.GetComputeLocation(ctx, keep.ID); err != nil {
				t.Fatalf("other location: %v, want it kept", err)
			}
		})
	}
}

func mkStorageLocation(name string) store.StorageLocation {
	return store.StorageLocation{
		ID: uuid.NewString(), Name: name, Type: store.StorageLocationTypeFilesystem, Description: name + " desc",
		Roots: map[string]string{"default": "/mnt/" + name, "windows_workers": `Z:\` + name},
	}
}

func mustCreateStorageLocation(t *testing.T, st store.Store, name string) store.StorageLocation {
	t.Helper()
	loc, err := st.CreateStorageLocation(t.Context(), mkStorageLocation(name))
	if err != nil {
		t.Fatalf("CreateStorageLocation %s: %v", name, err)
	}
	return loc
}

func sameStorageLocation(a, b store.StorageLocation) bool {
	return a.ID == b.ID && a.Name == b.Name && a.Type == b.Type && a.Description == b.Description && maps.Equal(a.Roots, b.Roots)
}

func TestParity_CreateStorageLocation(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			want := mkStorageLocation("shows")
			got, err := st.CreateStorageLocation(t.Context(), want)
			if err != nil || !sameStorageLocation(got, want) {
				t.Fatalf("CreateStorageLocation = (%+v, %v), want %+v", got, err, want)
			}
			if _, err := st.CreateStorageLocation(t.Context(), mkStorageLocation("shows")); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("duplicate name: err = %v, want ErrConflict", err)
			}
		})
	}
}

func TestParity_GetStorageLocation(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			loc := mustCreateStorageLocation(t, st, "shows")
			got, err := st.GetStorageLocation(t.Context(), loc.ID)
			if err != nil || !sameStorageLocation(got, loc) {
				t.Fatalf("GetStorageLocation = (%+v, %v), want %+v", got, err, loc)
			}
			if _, err := st.GetStorageLocation(t.Context(), uuid.NewString()); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown id: err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestParity_GetStorageLocationByName(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			loc := mustCreateStorageLocation(t, st, "shows")
			mustCreateStorageLocation(t, st, "assets")
			got, err := st.GetStorageLocationByName(t.Context(), "shows")
			if err != nil || !sameStorageLocation(got, loc) {
				t.Fatalf("GetStorageLocationByName = (%+v, %v), want %+v", got, err, loc)
			}
			if _, err := st.GetStorageLocationByName(t.Context(), "nowhere"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown name: err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestParity_ListStorageLocations(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			if got, err := st.ListStorageLocations(t.Context()); err != nil || len(got) != 0 {
				t.Fatalf("empty store: ListStorageLocations = (%+v, %v), want none", got, err)
			}
			created := map[string]store.StorageLocation{}
			for _, n := range []string{"shows", "assets", "renders"} {
				created[n] = mustCreateStorageLocation(t, st, n)
			}
			got, err := st.ListStorageLocations(t.Context())
			if err != nil {
				t.Fatalf("ListStorageLocations: %v", err)
			}
			var names []string
			for _, l := range got {
				names = append(names, l.Name)
				if !sameStorageLocation(l, created[l.Name]) {
					t.Fatalf("listed %+v, want %+v", l, created[l.Name])
				}
			}
			if want := []string{"assets", "renders", "shows"}; !slices.Equal(names, want) {
				t.Fatalf("names = %v, want %v (ordered by name)", names, want)
			}
		})
	}
}

func TestParity_UpdateStorageLocation(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			loc := mustCreateStorageLocation(t, st, "shows")
			mustCreateStorageLocation(t, st, "assets")

			upd := loc
			upd.Name, upd.Type, upd.Description = "shows-2", store.StorageLocationTypeS3, "moved"
			upd.Roots = map[string]string{"default": "s3://bucket/shows"}
			got, err := st.UpdateStorageLocation(ctx, upd)
			if err != nil || !sameStorageLocation(got, upd) {
				t.Fatalf("UpdateStorageLocation = (%+v, %v), want %+v", got, err, upd)
			}
			if !got.CreatedAt.Equal(loc.CreatedAt) || got.UpdatedAt.IsZero() || got.UpdatedAt.Before(loc.UpdatedAt) {
				t.Fatalf("timestamps = created %v updated %v, want created %v kept and updated advanced from %v",
					got.CreatedAt, got.UpdatedAt, loc.CreatedAt, loc.UpdatedAt)
			}
			if stored, err := st.GetStorageLocation(ctx, loc.ID); err != nil || !sameStorageLocation(stored, upd) {
				t.Fatalf("GetStorageLocation after update = (%+v, %v), want %+v", stored, err, upd)
			}

			upd.Name = "assets"
			if _, err := st.UpdateStorageLocation(ctx, upd); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("rename onto a taken name: err = %v, want ErrConflict", err)
			}
			if _, err := st.UpdateStorageLocation(ctx, mkStorageLocation("ghost")); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown id: err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestParity_DeleteStorageLocation(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			loc := mustCreateStorageLocation(t, st, "shows")
			keep := mustCreateStorageLocation(t, st, "assets")
			if err := st.DeleteStorageLocation(ctx, loc.ID); err != nil {
				t.Fatalf("DeleteStorageLocation: %v", err)
			}
			if _, err := st.GetStorageLocation(ctx, loc.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetStorageLocation after delete: err = %v, want ErrNotFound", err)
			}
			if err := st.DeleteStorageLocation(ctx, loc.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("second delete: err = %v, want ErrNotFound", err)
			}
			if _, err := st.GetStorageLocation(ctx, keep.ID); err != nil {
				t.Fatalf("other location: %v, want it kept", err)
			}
		})
	}
}
