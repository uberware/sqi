// SPDX-License-Identifier: AGPL-3.0-only

package api

// Unit tests for license-pool REST handlers.

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

func newLicensePoolRouter(st store.Store) chi.Router {
	h := newLicensePoolHandler(st, newTestLogger())
	r := chi.NewRouter()
	r.Post("/api/v1/license-pools", h.createLicensePool)
	r.Get("/api/v1/license-pools", h.listLicensePools)
	r.Get("/api/v1/license-pools/{id}", h.getLicensePool)
	r.Put("/api/v1/license-pools/{id}", h.updateLicensePool)
	r.Delete("/api/v1/license-pools/{id}", h.deleteLicensePool)
	return r
}

func seedLicensePool(t *testing.T, st *fake.Store) store.LicensePool {
	t.Helper()
	now := time.Now()
	p := store.LicensePool{
		ID:            uuid.NewString(),
		Name:          "pool-" + uuid.NewString(),
		Product:       "maya",
		MaxConcurrent: 5,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	created, err := st.CreateLicensePool(t.Context(), p)
	if err != nil {
		t.Fatalf("seedLicensePool: %v", err)
	}
	return created
}

// ── createLicensePool ─────────────────────────────────────────────────────────

func TestCreateLicensePool_InvalidJSON(t *testing.T) {
	st := fake.New()
	defer st.Close()

	req := newReq(t, http.MethodPost, "/api/v1/license-pools", badJSON())
	rr := httptest.NewRecorder()
	newLicensePoolRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestCreateLicensePool_MissingName(t *testing.T) {
	st := fake.New()
	defer st.Close()

	req := newReq(t, http.MethodPost, "/api/v1/license-pools", jsonBody(t, map[string]any{
		"product":        "maya",
		"max_concurrent": 2,
	}))
	rr := httptest.NewRecorder()
	newLicensePoolRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestCreateLicensePool_MissingProduct(t *testing.T) {
	st := fake.New()
	defer st.Close()

	req := newReq(t, http.MethodPost, "/api/v1/license-pools", jsonBody(t, map[string]any{
		"name":           "pool1",
		"max_concurrent": 2,
	}))
	rr := httptest.NewRecorder()
	newLicensePoolRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestCreateLicensePool_ZeroMaxConcurrent(t *testing.T) {
	st := fake.New()
	defer st.Close()

	req := newReq(t, http.MethodPost, "/api/v1/license-pools", jsonBody(t, map[string]any{
		"name":           "pool1",
		"product":        "maya",
		"max_concurrent": 0,
	}))
	rr := httptest.NewRecorder()
	newLicensePoolRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestCreateLicensePool_Conflict(t *testing.T) {
	st := fake.New()
	defer st.Close()
	pool := seedLicensePool(t, st)

	req := newReq(t, http.MethodPost, "/api/v1/license-pools", jsonBody(t, map[string]any{
		"name":           pool.Name,
		"product":        "maya",
		"max_concurrent": 5,
	}))
	rr := httptest.NewRecorder()
	newLicensePoolRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d", rr.Code)
	}
}

func TestCreateLicensePool_Success(t *testing.T) {
	st := fake.New()
	defer st.Close()

	req := newReq(t, http.MethodPost, "/api/v1/license-pools", jsonBody(t, map[string]any{
		"name":           "new-pool",
		"product":        "houdini",
		"max_concurrent": 3,
	}))
	rr := httptest.NewRecorder()
	newLicensePoolRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── listLicensePools ──────────────────────────────────────────────────────────

func TestListLicensePools_Success(t *testing.T) {
	st := fake.New()
	defer st.Close()
	seedLicensePool(t, st)

	req := newReq(t, http.MethodGet, "/api/v1/license-pools", nil)
	rr := httptest.NewRecorder()
	newLicensePoolRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
}

// ── getLicensePool ────────────────────────────────────────────────────────────

func TestGetLicensePool_NotFound(t *testing.T) {
	st := fake.New()
	defer st.Close()

	req := newReq(t, http.MethodGet, "/api/v1/license-pools/missing", nil)
	rr := httptest.NewRecorder()
	newLicensePoolRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
}

func TestGetLicensePool_Found(t *testing.T) {
	st := fake.New()
	defer st.Close()
	pool := seedLicensePool(t, st)

	req := newReq(t, http.MethodGet, "/api/v1/license-pools/"+pool.ID, nil)
	rr := httptest.NewRecorder()
	newLicensePoolRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
}

// ── updateLicensePool ─────────────────────────────────────────────────────────

func TestUpdateLicensePool_InvalidJSON(t *testing.T) {
	st := fake.New()
	defer st.Close()
	pool := seedLicensePool(t, st)

	req := newReq(t, http.MethodPut, "/api/v1/license-pools/"+pool.ID, badJSON())
	rr := httptest.NewRecorder()
	newLicensePoolRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestUpdateLicensePool_MissingName(t *testing.T) {
	st := fake.New()
	defer st.Close()
	pool := seedLicensePool(t, st)

	req := newReq(t, http.MethodPut, "/api/v1/license-pools/"+pool.ID, jsonBody(t, map[string]any{
		"product":        "maya",
		"max_concurrent": 5,
	}))
	rr := httptest.NewRecorder()
	newLicensePoolRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestUpdateLicensePool_NotFound(t *testing.T) {
	st := fake.New()
	defer st.Close()

	req := newReq(t, http.MethodPut, "/api/v1/license-pools/missing", jsonBody(t, map[string]any{
		"name":           "anything",
		"product":        "maya",
		"max_concurrent": 5,
	}))
	rr := httptest.NewRecorder()
	newLicensePoolRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
}

func TestUpdateLicensePool_Conflict(t *testing.T) {
	st := fake.New()
	defer st.Close()
	p1 := seedLicensePool(t, st)
	p2 := seedLicensePool(t, st)

	req := newReq(t, http.MethodPut, "/api/v1/license-pools/"+p1.ID, jsonBody(t, map[string]any{
		"name":           p2.Name, // collision
		"product":        "maya",
		"max_concurrent": 5,
	}))
	rr := httptest.NewRecorder()
	newLicensePoolRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d", rr.Code)
	}
}

func TestUpdateLicensePool_Success(t *testing.T) {
	st := fake.New()
	defer st.Close()
	pool := seedLicensePool(t, st)

	req := newReq(t, http.MethodPut, "/api/v1/license-pools/"+pool.ID, jsonBody(t, map[string]any{
		"name":           pool.Name,
		"product":        pool.Product,
		"max_concurrent": 10,
	}))
	rr := httptest.NewRecorder()
	newLicensePoolRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── deleteLicensePool ─────────────────────────────────────────────────────────

func TestDeleteLicensePool_NotFound(t *testing.T) {
	st := fake.New()
	defer st.Close()

	req := newReq(t, http.MethodDelete, "/api/v1/license-pools/missing", nil)
	rr := httptest.NewRecorder()
	newLicensePoolRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
}

func TestDeleteLicensePool_Success(t *testing.T) {
	st := fake.New()
	defer st.Close()
	pool := seedLicensePool(t, st)

	req := newReq(t, http.MethodDelete, "/api/v1/license-pools/"+pool.ID, nil)
	rr := httptest.NewRecorder()
	newLicensePoolRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d", rr.Code)
	}
}
