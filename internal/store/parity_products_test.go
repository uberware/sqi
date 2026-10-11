// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
)

func mkProduct(name string) store.Product {
	return store.Product{
		ID: uuid.NewString(), Name: name, Title: name + " title", Description: "desc", Readme: "# " + name,
		Category: "Rendering", Version: "1.0.0", Source: store.SourceCustom,
		Template: "specificationVersion: jobtemplate-2023-09\nname: " + name + "\n", Format: store.TemplateFormatYAML,
	}
}

func mustCreateProduct(t *testing.T, st store.Store, name string) store.Product {
	t.Helper()
	p, err := st.CreateProduct(t.Context(), mkProduct(name))
	if err != nil {
		t.Fatalf("CreateProduct %s: %v", name, err)
	}
	return p
}

func sameProduct(a, b store.Product) bool {
	return a.ID == b.ID && a.Name == b.Name && a.Title == b.Title && a.Description == b.Description &&
		a.Readme == b.Readme && a.Category == b.Category && a.Version == b.Version && a.Source == b.Source &&
		a.Template == b.Template && a.Format == b.Format && a.OriginRef == b.OriginRef &&
		a.OriginFingerprint == b.OriginFingerprint
}

func TestParity_CreateProduct(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			want := mkProduct("studio/maya-render")
			want.Source, want.OriginRef, want.OriginFingerprint = store.SourceInstalled, "maya-render", "sha256:abc"
			got, err := st.CreateProduct(t.Context(), want)
			if err != nil || !sameProduct(got, want) {
				t.Fatalf("CreateProduct = (%+v, %v), want %+v", got, err, want)
			}
			if _, err := st.CreateProduct(t.Context(), mkProduct("studio/maya-render")); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("duplicate name: err = %v, want ErrConflict", err)
			}
		})
	}
}

func TestParity_GetProductByName(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			p := mustCreateProduct(t, st, "studio/nuke")
			got, err := st.GetProductByName(t.Context(), "studio/nuke")
			if err != nil || !sameProduct(got, p) {
				t.Fatalf("GetProductByName = (%+v, %v), want %+v", got, err, p)
			}
			if _, err := st.GetProductByName(t.Context(), "studio/missing"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown name: err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestParity_ListProducts(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			if got, err := st.ListProducts(t.Context()); err != nil || len(got) != 0 {
				t.Fatalf("empty store: ListProducts = (%+v, %v), want none", got, err)
			}
			for _, n := range []string{"studio/nuke", "blender", "studio/maya"} {
				mustCreateProduct(t, st, n)
			}
			got, err := st.ListProducts(t.Context())
			if err != nil {
				t.Fatalf("ListProducts: %v", err)
			}
			var names []string
			for _, p := range got {
				names = append(names, p.Name)
			}
			if want := []string{"blender", "studio/maya", "studio/nuke"}; !slices.Equal(names, want) {
				t.Fatalf("names = %v, want %v (ordered by name)", names, want)
			}
		})
	}
}

// TestParity_ListProducts_MixedCase pins name order across case. Product names
// are case-insensitive identities (a case-only duplicate is a conflict and a
// lookup ignores case on both backends), and SQLite orders them that way too.
func TestParity_ListProducts_MixedCase(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			if name == "fake" {
				t.Skip("divergence: fake ListProducts sorts names byte-wise; SQLite's NOCASE column orders them case-insensitively")
			}
			mustCreateProduct(t, st, "apple")
			mustCreateProduct(t, st, "Banana")
			if _, err := st.CreateProduct(t.Context(), mkProduct("APPLE")); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("case-only duplicate: err = %v, want ErrConflict", err)
			}
			got, err := st.ListProducts(t.Context())
			if err != nil {
				t.Fatalf("ListProducts: %v", err)
			}
			var names []string
			for _, p := range got {
				names = append(names, p.Name)
			}
			if want := []string{"apple", "Banana"}; !slices.Equal(names, want) {
				t.Fatalf("names = %v, want %v", names, want)
			}
		})
	}
}

func TestParity_UpdateProduct(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			p := mustCreateProduct(t, st, "studio/nuke")

			// The product is addressed by name; the caller need not know its ID.
			upd := mkProduct("studio/nuke")
			upd.ID = ""
			upd.Title, upd.Version, upd.Template = "Nuke 15", "2.0.0", "specificationVersion: jobtemplate-2023-09\nname: v2\n"
			got, err := st.UpdateProduct(ctx, upd)
			if err != nil || got.ID != p.ID || got.Title != "Nuke 15" || got.Version != "2.0.0" || got.Template != upd.Template {
				t.Fatalf("UpdateProduct = (%+v, %v), want %s updated in place", got, err, p.ID)
			}
			stored, err := st.GetProductByName(ctx, "studio/nuke")
			if err != nil || stored.ID != p.ID || stored.Title != "Nuke 15" || stored.Version != "2.0.0" {
				t.Fatalf("GetProductByName after update = (%+v, %v), want the update stored", stored, err)
			}
			if _, err := st.UpdateProduct(ctx, mkProduct("studio/missing")); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown name: err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestParity_DeleteProduct(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			mustCreateProduct(t, st, "studio/nuke")
			mustCreateProduct(t, st, "studio/maya")
			if err := st.DeleteProduct(ctx, "studio/nuke"); err != nil {
				t.Fatalf("DeleteProduct: %v", err)
			}
			if _, err := st.GetProductByName(ctx, "studio/nuke"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("GetProductByName after delete: err = %v, want ErrNotFound", err)
			}
			if err := st.DeleteProduct(ctx, "studio/nuke"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("second delete: err = %v, want ErrNotFound", err)
			}
			if _, err := st.GetProductByName(ctx, "studio/maya"); err != nil {
				t.Fatalf("other product: %v, want it kept", err)
			}
		})
	}
}
