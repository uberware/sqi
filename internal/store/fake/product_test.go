// SPDX-License-Identifier: AGPL-3.0-or-later

package fake_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

func sampleProduct(name string) store.Product {
	return store.Product{
		ID: uuid.NewString(), Name: name, Title: "T " + name,
		Version: "1.0.0", Source: store.SourceCustom,
		Template: "x", Format: store.TemplateFormatYAML,
	}
}

func TestFakeProduct_CRUDAndConflict(t *testing.T) {
	st := fake.New()
	ctx := context.Background()

	if _, err := st.CreateProduct(ctx, sampleProduct("alpha")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.CreateProduct(ctx, sampleProduct("alpha")); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("dup: want ErrConflict, got %v", err)
	}
	got, err := st.GetProductByName(ctx, "alpha")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got.Title = "Updated"
	if _, err := st.UpdateProduct(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := st.DeleteProduct(ctx, "alpha"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.GetProductByName(ctx, "alpha"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after delete: want ErrNotFound, got %v", err)
	}
}

func TestFakeProduct_CaseInsensitiveConflict(t *testing.T) {
	st := fake.New()
	ctx := context.Background()

	if _, err := st.CreateProduct(ctx, sampleProduct("alpha")); err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	if _, err := st.CreateProduct(ctx, sampleProduct("ALPHA")); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("dup ALPHA: want ErrConflict, got %v", err)
	}
}

func TestFakeProduct_UpdateNotFound(t *testing.T) {
	st := fake.New()
	ctx := context.Background()

	if _, err := st.UpdateProduct(ctx, sampleProduct("nonexistent")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("update nonexistent: want ErrNotFound, got %v", err)
	}
}

func TestFakeProduct_OriginRoundTrip(t *testing.T) {
	st := fake.New()
	ctx := context.Background()
	in := store.Product{ID: "p1", Name: "studio/maya", Title: "Maya", Source: store.SourceInstalled, OriginRef: "studio/maya", OriginFingerprint: "abc123"}
	if _, err := st.CreateProduct(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetProductByName(ctx, "studio/maya")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.OriginRef != "studio/maya" || got.OriginFingerprint != "abc123" {
		t.Fatalf("fake dropped origin: %+v", got)
	}
}

// The fake stores the whole store.Product value, so Readme needs no explicit
// handling. This test exists so a future refactor to field-by-field copying
// cannot silently drop it.
func TestFakeProduct_ReadmeRoundTrips(t *testing.T) {
	st := fake.New()
	ctx := context.Background()
	if _, err := st.CreateProduct(ctx, store.Product{
		ID: "p1", Name: "readme-probe", Title: "Readme Probe", Readme: "# Docs\n",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := st.GetProductByName(ctx, "readme-probe")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Readme != "# Docs\n" {
		t.Errorf("readme = %q, want %q", got.Readme, "# Docs\n")
	}
}
