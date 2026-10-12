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
			names := project(got, func(p store.Product) string { return p.Name })
			if want := []string{"blender", "studio/maya", "studio/nuke"}; !slices.Equal(names, want) {
				t.Fatalf("names = %v, want %v (ordered by name)", names, want)
			}
		})
	}
}

// TestParity_ListProducts_MixedCase pins name order across case. Product names
// are case-insensitive identities (a case-only duplicate is a conflict and a
// lookup ignores case on every backend), and every backend orders them that way.
func TestParity_ListProducts_MixedCase(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			mustCreateProduct(t, st, "apple")
			mustCreateProduct(t, st, "Banana")
			if _, err := st.CreateProduct(t.Context(), mkProduct("APPLE")); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("case-only duplicate: err = %v, want ErrConflict", err)
			}
			got, err := st.ListProducts(t.Context())
			if err != nil {
				t.Fatalf("ListProducts: %v", err)
			}
			names := project(got, func(p store.Product) string { return p.Name })
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

// TestParity_CaseInsensitiveColumns pins the columns SQLite declares COLLATE
// NOCASE, users.username and products.name: an equality or ORDER BY on either
// ignores ASCII case on every backend. PostgreSQL holds them COLLATE "C", so
// the shared SQL must ask for the fold itself.
func TestParity_CaseInsensitiveColumns(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			for _, username := range []string{"Bob", "alice"} {
				u := mkUser("viewer")
				u.Username = username
				if _, err := st.CreateUser(ctx, u); err != nil {
					t.Fatalf("CreateUser %s: %v", username, err)
				}
			}
			users, err := st.ListUsers(ctx)
			if err != nil {
				t.Fatalf("ListUsers: %v", err)
			}
			if got, want := project(users, func(u store.User) string { return u.Username }), []string{"alice", "Bob"}; !slices.Equal(got, want) {
				t.Errorf("ListUsers = %v, want %v", got, want)
			}

			for _, n := range []string{"Zeta", "script", "Alpha", "beta"} {
				mustCreateProduct(t, st, n)
			}
			products, err := st.ListProducts(ctx)
			if err != nil {
				t.Fatalf("ListProducts: %v", err)
			}
			if got, want := project(products, func(p store.Product) string { return p.Name }), []string{"Alpha", "beta", "script", "Zeta"}; !slices.Equal(got, want) {
				t.Errorf("ListProducts = %v, want %v", got, want)
			}

			if p, err := st.GetProductByName(ctx, "Script"); err != nil || p.Name != "script" {
				t.Errorf("GetProductByName(Script) = (%q, %v), want script", p.Name, err)
			}
			upd := mkProduct("SCRIPT")
			upd.Title = "renamed"
			if p, err := st.UpdateProduct(ctx, upd); err != nil || p.Name != "script" || p.Title != "renamed" {
				t.Errorf("UpdateProduct(SCRIPT) = (%q, %q, %v), want script updated", p.Name, p.Title, err)
			}
			if err := st.DeleteProduct(ctx, "ZETA"); err != nil {
				t.Errorf("DeleteProduct(ZETA) = %v, want Zeta deleted", err)
			}
			if _, err := st.GetProductByName(ctx, "Zeta"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("GetProductByName(Zeta) after delete = %v, want ErrNotFound", err)
			}
		})
	}
}
