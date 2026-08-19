// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/sqlite"
)

func newProductStore(t *testing.T) store.Store {
	t.Helper()
	db := t.TempDir() + "/test.db"
	st, err := sqlite.Open(context.Background(), db, sqlite.Options{AutoMigrate: true})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func sampleProduct(name string) store.Product {
	return store.Product{
		ID:       uuid.NewString(),
		Name:     name,
		Title:    "Title " + name,
		Version:  "1.0.0",
		Source:   store.SourceCustom,
		Template: "specificationVersion: jobtemplate-2023-09\nname: X\nsteps: []",
		Format:   store.TemplateFormatYAML,
	}
}

func TestSQLiteProduct_CRUD(t *testing.T) {
	st := newProductStore(t)
	ctx := context.Background()

	created, err := st.CreateProduct(ctx, sampleProduct("alpha"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Name != "alpha" || created.Source != store.SourceCustom {
		t.Fatalf("unexpected created: %+v", created)
	}

	got, err := st.GetProductByName(ctx, "alpha")
	if err != nil || got.ID != created.ID {
		t.Fatalf("get: %v %+v", err, got)
	}

	got.Title = "Updated"
	updated, err := st.UpdateProduct(ctx, got)
	if err != nil || updated.Title != "Updated" {
		t.Fatalf("update: %v %+v", err, updated)
	}

	if err := st.DeleteProduct(ctx, "alpha"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.GetProductByName(ctx, "alpha"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get after delete: want ErrNotFound, got %v", err)
	}
}

func TestSQLiteProduct_DuplicateName(t *testing.T) {
	st := newProductStore(t)
	ctx := context.Background()
	if _, err := st.CreateProduct(ctx, sampleProduct("dup")); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := st.CreateProduct(ctx, sampleProduct("dup")); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second create: want ErrConflict, got %v", err)
	}
}

func TestSQLiteProduct_ListOrdered(t *testing.T) {
	st := newProductStore(t)
	ctx := context.Background()
	for _, n := range []string{"gamma", "alpha", "beta"} {
		if _, err := st.CreateProduct(ctx, sampleProduct(n)); err != nil {
			t.Fatalf("create %s: %v", n, err)
		}
	}
	list, err := st.ListProducts(ctx)
	if err != nil || len(list) != 3 {
		t.Fatalf("list: %v len=%d", err, len(list))
	}
	if list[0].Name != "alpha" || list[1].Name != "beta" || list[2].Name != "gamma" {
		t.Fatalf("not name-ordered: %v", []string{list[0].Name, list[1].Name, list[2].Name})
	}
}

func TestProduct_OriginRoundTrip(t *testing.T) {
	st := newProductStore(t) // existing helper in this package
	ctx := context.Background()
	in := store.Product{
		ID: "p1", Name: "studio/maya", Title: "Maya", Source: store.SourceInstalled,
		Template:  "specificationVersion: jobtemplate-2023-09\nname: M\nsteps: []\n",
		Format:    store.TemplateFormatYAML,
		OriginRef: "studio/maya", OriginFingerprint: "abc123",
	}
	if _, err := st.CreateProduct(ctx, in); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := st.GetProductByName(ctx, "studio/maya")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.OriginRef != "studio/maya" || got.OriginFingerprint != "abc123" {
		t.Fatalf("origin not persisted: ref=%q fp=%q", got.OriginRef, got.OriginFingerprint)
	}
}

func TestProduct_UpdateOriginRoundTrip(t *testing.T) {
	st := newProductStore(t)
	ctx := context.Background()
	in := store.Product{
		ID: uuid.NewString(), Name: "studio/nuke", Title: "Nuke", Source: store.SourceInstalled,
		Template:  "specificationVersion: jobtemplate-2023-09\nname: N\nsteps: []\n",
		Format:    store.TemplateFormatYAML,
		OriginRef: "studio/nuke@v1", OriginFingerprint: "fp111",
	}
	created, err := st.CreateProduct(ctx, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	created.OriginRef = "studio/nuke@v2"
	created.OriginFingerprint = "fp222"
	updated, err := st.UpdateProduct(ctx, created)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.OriginRef != "studio/nuke@v2" || updated.OriginFingerprint != "fp222" {
		t.Fatalf("update returned stale origin: ref=%q fp=%q", updated.OriginRef, updated.OriginFingerprint)
	}

	fetched, err := st.GetProductByName(ctx, "studio/nuke")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if fetched.OriginRef != "studio/nuke@v2" || fetched.OriginFingerprint != "fp222" {
		t.Fatalf("persisted origin wrong: ref=%q fp=%q", fetched.OriginRef, fetched.OriginFingerprint)
	}
}

func TestProduct_ReadmeRoundTrips(t *testing.T) {
	st := newProductStore(t)
	ctx := context.Background()

	const readme = "# Heading\n\nBody with code.\n"
	created, err := st.CreateProduct(ctx, store.Product{
		ID: "p1", Name: "readme-probe", Title: "Readme Probe",
		Description: "short blurb", Readme: readme,
		Source: store.SourceCustom, Template: "specificationVersion: jobtemplate-2023-09\nname: X\nsteps: []", Format: store.TemplateFormatYAML,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Readme != readme {
		t.Errorf("create returned readme %q, want %q", created.Readme, readme)
	}

	got, err := st.GetProductByName(ctx, "readme-probe")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Readme != readme {
		t.Errorf("get returned readme %q, want %q", got.Readme, readme)
	}

	list, err := st.ListProducts(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].Readme != readme {
		t.Errorf("list returned %+v, want one row carrying the readme", list)
	}

	updated, err := st.UpdateProduct(ctx, store.Product{
		Name: "readme-probe", Title: "Readme Probe", Description: "short blurb",
		Readme: "replaced", Template: "specificationVersion: jobtemplate-2023-09\nname: X\nsteps: []", Format: store.TemplateFormatYAML,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Readme != "replaced" {
		t.Errorf("update returned readme %q, want %q", updated.Readme, "replaced")
	}
}

// A product created without a readme reads back as "" rather than failing the
// scan. That is what the migration's empty-string default buys for
// pre-existing rows.
func TestProduct_ReadmeDefaultsEmpty(t *testing.T) {
	st := newProductStore(t)
	ctx := context.Background()

	if _, err := st.CreateProduct(ctx, store.Product{
		ID: "p2", Name: "no-readme", Title: "No Readme",
		Source: store.SourceCustom, Template: "specificationVersion: jobtemplate-2023-09\nname: Y\nsteps: []", Format: store.TemplateFormatYAML,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := st.GetProductByName(ctx, "no-readme")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Readme != "" {
		t.Errorf("readme = %q, want empty", got.Readme)
	}
}
