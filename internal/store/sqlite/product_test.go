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
