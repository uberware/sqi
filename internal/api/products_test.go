// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Unit tests for product REST handlers.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/uberware/sqi/internal/product"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

const validTemplate = `specificationVersion: jobtemplate-2023-09
name: Demo
steps:
  - name: Run
    script:
      actions:
        onRun:
          command: echo
          args: ["hi"]`

func newProductRouter(st store.Store) chi.Router {
	h := newProductHandler(product.NewCatalog(st), newTestLogger())
	r := chi.NewRouter()
	r.Get("/api/v1/products", h.listProducts)
	r.Post("/api/v1/products", h.createProduct)
	r.Get("/api/v1/products/{name}", h.getProduct)
	r.Put("/api/v1/products/{name}", h.updateProduct)
	r.Delete("/api/v1/products/{name}", h.deleteProduct)
	return r
}

func TestProducts_ListIncludesBuiltins(t *testing.T) {
	srv := newProductRouter(fake.New())
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/products", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) < 3 {
		t.Fatalf("expected >=3 built-ins, got %d", len(got))
	}
}

func TestProducts_CreateThenGet(t *testing.T) {
	srv := newProductRouter(fake.New())
	req := newReq(t, http.MethodPost, "/api/v1/products", jsonBody(t, map[string]any{
		"name": "mine", "title": "Mine", "version": "1.0.0",
		"template": validTemplate, "format": "yaml",
	}))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", rec.Code, rec.Body)
	}
	req = newReq(t, http.MethodGet, "/api/v1/products/mine", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d", rec.Code)
	}
}

func TestProducts_CreateBadTemplateIs400(t *testing.T) {
	srv := newProductRouter(fake.New())
	req := newReq(t, http.MethodPost, "/api/v1/products", jsonBody(t, map[string]any{
		"name": "bad", "title": "Bad",
		"template": "specificationVersion: nope\nsteps: []", "format": "yaml",
	}))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestProducts_GetUnknownIs404(t *testing.T) {
	srv := newProductRouter(fake.New())
	req := newReq(t, http.MethodGet, "/api/v1/products/ghost", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestProducts_MutateBuiltinIs403(t *testing.T) {
	srv := newProductRouter(fake.New())
	req := newReq(t, http.MethodDelete, "/api/v1/products/script", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	_ = context.Background()
}
