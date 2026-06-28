// SPDX-License-Identifier: AGPL-3.0-or-later

package product_test

import (
	"context"
	"errors"
	"testing"

	"github.com/uberware/sqi/internal/product"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

func newCatalog() *product.Catalog {
	return product.NewCatalog(fake.New())
}

func customProduct(name string) store.Product {
	return store.Product{
		Name: name, Title: "T", Version: "1.0.0",
		Template: goodTemplate, Format: store.TemplateFormatYAML,
	}
}

func TestCatalog_ListMergesBuiltinsAndStored(t *testing.T) {
	c := newCatalog()
	ctx := context.Background()
	if _, err := c.Create(ctx, customProduct("zzz-custom")); err != nil {
		t.Fatalf("create: %v", err)
	}
	list, err := c.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// 3 built-ins + 1 custom, name-ordered, custom last.
	if len(list) != 4 || list[len(list)-1].Name != "zzz-custom" {
		t.Fatalf("merge wrong: %+v", list)
	}
}

func TestCatalog_GetPrefersBuiltinThenStoredThenNotFound(t *testing.T) {
	c := newCatalog()
	ctx := context.Background()
	if p, err := c.GetByName(ctx, "script"); err != nil || p.Source != store.SourceBuiltin {
		t.Fatalf("builtin get: %v %+v", err, p)
	}
	if _, err := c.Create(ctx, customProduct("mine")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if p, err := c.GetByName(ctx, "mine"); err != nil || p.Source != store.SourceCustom {
		t.Fatalf("custom get: %v %+v", err, p)
	}
	if _, err := c.GetByName(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing get: want ErrNotFound, got %v", err)
	}
}

func TestCatalog_CreateRejectsBuiltinShadow(t *testing.T) {
	c := newCatalog()
	if _, err := c.Create(context.Background(), customProduct("script")); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("shadow create: want ErrConflict, got %v", err)
	}
}

func TestCatalog_UpdateDeleteBuiltinRefused(t *testing.T) {
	c := newCatalog()
	ctx := context.Background()
	if _, err := c.Update(ctx, customProduct("script")); !errors.Is(err, product.ErrReadOnly) {
		t.Fatalf("update builtin: want ErrReadOnly, got %v", err)
	}
	if err := c.Delete(ctx, "script"); !errors.Is(err, product.ErrReadOnly) {
		t.Fatalf("delete builtin: want ErrReadOnly, got %v", err)
	}
}

func TestCatalog_CreateAssignsIDAndSource(t *testing.T) {
	c := newCatalog()
	got, err := c.Create(context.Background(), customProduct("fresh"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got.ID == "" || got.Source != store.SourceCustom {
		t.Fatalf("create did not stamp id/source: %+v", got)
	}
}

func TestCatalog_CreateRejectsBuiltinShadowCaseInsensitive(t *testing.T) {
	c := newCatalog()
	// "Script" (mixed-case) must still be rejected as shadowing the "script" built-in.
	if _, err := c.Create(context.Background(), customProduct("Script")); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("shadow create (mixed-case): want ErrConflict, got %v", err)
	}
}

func TestCatalog_UpdateDeleteBuiltinCaseInsensitive(t *testing.T) {
	c := newCatalog()
	ctx := context.Background()
	if _, err := c.Update(ctx, customProduct("SCRIPT")); !errors.Is(err, product.ErrReadOnly) {
		t.Fatalf("update builtin (uppercase): want ErrReadOnly, got %v", err)
	}
	if err := c.Delete(ctx, "SCRIPT"); !errors.Is(err, product.ErrReadOnly) {
		t.Fatalf("delete builtin (uppercase): want ErrReadOnly, got %v", err)
	}
}
