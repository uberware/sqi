// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Unit tests for product REST handlers.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/openjd"
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

// productTestSrv bundles the HTTP handler and the underlying fake store so
// tests can both serve requests and seed the store directly.
type productTestSrv struct {
	chi.Router

	st *fake.Store
}

// newProductTestServer creates a fake-backed product test server with the
// Submitter wired in (needed for submit-by-product tests).
func newProductTestServer(t *testing.T) *productTestSrv {
	t.Helper()
	st := fake.New()
	h := newProductHandler(product.NewCatalog(st), openjd.NewSubmitter(st), nil, newTestLogger())
	r := chi.NewRouter()
	r.Get("/api/v1/products", h.listProducts)
	r.Post("/api/v1/products", h.createProduct)
	r.Get("/api/v1/products/{name}", h.getProduct)
	r.Put("/api/v1/products/{name}", h.updateProduct)
	r.Delete("/api/v1/products/{name}", h.deleteProduct)
	r.Post("/api/v1/products/{name}/jobs", h.submitProductJob)
	return &productTestSrv{Router: r, st: st}
}

// newProductRouter builds a product router for CRUD-only tests (no Submitter
// needed). Also registers the submit route so route resolution is consistent.
func newProductRouter(st store.Store) chi.Router {
	h := newProductHandler(product.NewCatalog(st), openjd.NewSubmitter(st), nil, newTestLogger())
	r := chi.NewRouter()
	r.Get("/api/v1/products", h.listProducts)
	r.Post("/api/v1/products", h.createProduct)
	r.Get("/api/v1/products/{name}", h.getProduct)
	r.Put("/api/v1/products/{name}", h.updateProduct)
	r.Delete("/api/v1/products/{name}", h.deleteProduct)
	r.Post("/api/v1/products/{name}/jobs", h.submitProductJob)
	return r
}

// seedProductSubmitPrereqs inserts a farm and queue into the test server's
// fake store and returns their IDs.
func seedProductSubmitPrereqs(t *testing.T, srv *productTestSrv) (farmID, queueID string) {
	t.Helper()
	ctx := t.Context()

	farm, err := srv.st.CreateFarm(ctx, store.Farm{
		ID:   uuid.NewString(),
		Name: "test-farm-" + uuid.NewString()[:8],
	})
	if err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	queue, err := srv.st.CreateQueue(ctx, store.Queue{
		ID:     uuid.NewString(),
		FarmID: farm.ID,
		Name:   "test-queue-" + uuid.NewString()[:8],
	})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	return farm.ID, queue.ID
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
}

func TestProducts_SubmitBuiltin(t *testing.T) {
	srv := newProductTestServer(t)
	farmID, queueID := seedProductSubmitPrereqs(t, srv)

	body := jsonBody(t, map[string]any{
		"farm_id": farmID, "queue_id": queueID,
		"parameters": map[string]string{"Command": "echo hi"},
	})
	req := newReq(t, http.MethodPost, "/api/v1/products/script/jobs", bytes.NewReader(body.Bytes()))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("submit status = %d body=%s", rec.Code, rec.Body)
	}
}

func TestProducts_SubmitUnknownProductIs404(t *testing.T) {
	srv := newProductTestServer(t)
	body := jsonBody(t, map[string]any{"farm_id": "f", "queue_id": "q"})
	req := newReq(t, http.MethodPost, "/api/v1/products/ghost/jobs", bytes.NewReader(body.Bytes()))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestProducts_UpdatePreservesSource(t *testing.T) {
	srv := newProductRouter(fake.New())

	// Create a custom product — source is stamped as "custom" by the catalog.
	createBody := jsonBody(t, map[string]any{
		"name": "my-product", "title": "Original", "version": "1.0.0",
		"template": validTemplate, "format": "yaml",
	})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newReq(t, http.MethodPost, "/api/v1/products", createBody))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", rec.Code, rec.Body)
	}

	// PUT with a changed title — source must not change.
	putBody := jsonBody(t, map[string]any{
		"title": "Updated", "version": "1.0.0",
		"template": validTemplate, "format": "yaml",
	})
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, newReq(t, http.MethodPut, "/api/v1/products/my-product", putBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("put status = %d body=%s", rec.Code, rec.Body)
	}

	// GET and verify: title changed, source still "custom".
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, newReq(t, http.MethodGet, "/api/v1/products/my-product", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["source"] != "custom" {
		t.Errorf("source = %q, want %q", got["source"], "custom")
	}
	if got["title"] != "Updated" {
		t.Errorf("title = %q, want %q", got["title"], "Updated")
	}
}

func TestProducts_UpdateUnknownIs404(t *testing.T) {
	srv := newProductRouter(fake.New())
	putBody := jsonBody(t, map[string]any{
		"title": "X", "version": "1.0.0",
		"template": validTemplate, "format": "yaml",
	})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newReq(t, http.MethodPut, "/api/v1/products/ghost", putBody))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
