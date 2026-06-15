// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Unit tests for usage-pool REST handlers.

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

func newUsagePoolRouter(st store.Store) chi.Router {
	h := newUsagePoolHandler(st, newTestLogger())
	r := chi.NewRouter()
	r.Post("/api/v1/usage-pools", h.createUsagePool)
	r.Get("/api/v1/usage-pools", h.listUsagePools)
	r.Get("/api/v1/usage-pools/{id}", h.getUsagePool)
	r.Put("/api/v1/usage-pools/{id}", h.updateUsagePool)
	r.Delete("/api/v1/usage-pools/{id}", h.deleteUsagePool)
	return r
}

func seedUsagePool(t *testing.T, st *fake.Store) store.UsagePool {
	t.Helper()
	now := time.Now()
	p := store.UsagePool{
		ID:            uuid.NewString(),
		Name:          "pool-" + uuid.NewString(),
		MaxConcurrent: 5,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	created, err := st.CreateUsagePool(t.Context(), p)
	if err != nil {
		t.Fatalf("seedUsagePool: %v", err)
	}
	return created
}

// ── createUsagePool ───────────────────────────────────────────────────────────

func TestCreateUsagePool_InvalidJSON(t *testing.T) {
	st := fake.New()
	defer st.Close()

	req := newReq(t, http.MethodPost, "/api/v1/usage-pools", badJSON())
	rr := httptest.NewRecorder()
	newUsagePoolRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestCreateUsagePool_MissingName(t *testing.T) {
	st := fake.New()
	defer st.Close()

	req := newReq(t, http.MethodPost, "/api/v1/usage-pools", jsonBody(t, map[string]any{
		"max_concurrent": 2,
	}))
	rr := httptest.NewRecorder()
	newUsagePoolRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestCreateUsagePool_ZeroMaxConcurrent(t *testing.T) {
	st := fake.New()
	defer st.Close()

	req := newReq(t, http.MethodPost, "/api/v1/usage-pools", jsonBody(t, map[string]any{
		"name":           "pool1",
		"max_concurrent": 0,
	}))
	rr := httptest.NewRecorder()
	newUsagePoolRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestCreateUsagePool_Conflict(t *testing.T) {
	st := fake.New()
	defer st.Close()
	pool := seedUsagePool(t, st)

	req := newReq(t, http.MethodPost, "/api/v1/usage-pools", jsonBody(t, map[string]any{
		"name":           pool.Name,
		"max_concurrent": 5,
	}))
	rr := httptest.NewRecorder()
	newUsagePoolRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d", rr.Code)
	}
}

func TestCreateUsagePool_Success(t *testing.T) {
	st := fake.New()
	defer st.Close()

	req := newReq(t, http.MethodPost, "/api/v1/usage-pools", jsonBody(t, map[string]any{
		"name":           "new-pool",
		"max_concurrent": 3,
	}))
	rr := httptest.NewRecorder()
	newUsagePoolRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── listUsagePools ────────────────────────────────────────────────────────────

func TestListUsagePools_Success(t *testing.T) {
	st := fake.New()
	defer st.Close()
	seedUsagePool(t, st)

	req := newReq(t, http.MethodGet, "/api/v1/usage-pools", nil)
	rr := httptest.NewRecorder()
	newUsagePoolRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
}

// ── getUsagePool ──────────────────────────────────────────────────────────────

func TestGetUsagePool_NotFound(t *testing.T) {
	st := fake.New()
	defer st.Close()

	req := newReq(t, http.MethodGet, "/api/v1/usage-pools/missing", nil)
	rr := httptest.NewRecorder()
	newUsagePoolRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
}

func TestGetUsagePool_Found(t *testing.T) {
	st := fake.New()
	defer st.Close()
	pool := seedUsagePool(t, st)

	req := newReq(t, http.MethodGet, "/api/v1/usage-pools/"+pool.ID, nil)
	rr := httptest.NewRecorder()
	newUsagePoolRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
}

// ── updateUsagePool ───────────────────────────────────────────────────────────

func TestUpdateUsagePool_InvalidJSON(t *testing.T) {
	st := fake.New()
	defer st.Close()
	pool := seedUsagePool(t, st)

	req := newReq(t, http.MethodPut, "/api/v1/usage-pools/"+pool.ID, badJSON())
	rr := httptest.NewRecorder()
	newUsagePoolRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestUpdateUsagePool_MissingName(t *testing.T) {
	st := fake.New()
	defer st.Close()
	pool := seedUsagePool(t, st)

	req := newReq(t, http.MethodPut, "/api/v1/usage-pools/"+pool.ID, jsonBody(t, map[string]any{
		"max_concurrent": 5,
	}))
	rr := httptest.NewRecorder()
	newUsagePoolRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestUpdateUsagePool_NotFound(t *testing.T) {
	st := fake.New()
	defer st.Close()

	req := newReq(t, http.MethodPut, "/api/v1/usage-pools/missing", jsonBody(t, map[string]any{
		"name":           "anything",
		"max_concurrent": 5,
	}))
	rr := httptest.NewRecorder()
	newUsagePoolRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
}

func TestUpdateUsagePool_Conflict(t *testing.T) {
	st := fake.New()
	defer st.Close()
	p1 := seedUsagePool(t, st)
	p2 := seedUsagePool(t, st)

	req := newReq(t, http.MethodPut, "/api/v1/usage-pools/"+p1.ID, jsonBody(t, map[string]any{
		"name":           p2.Name, // collision
		"max_concurrent": 5,
	}))
	rr := httptest.NewRecorder()
	newUsagePoolRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d", rr.Code)
	}
}

func TestUpdateUsagePool_Success(t *testing.T) {
	st := fake.New()
	defer st.Close()
	pool := seedUsagePool(t, st)

	req := newReq(t, http.MethodPut, "/api/v1/usage-pools/"+pool.ID, jsonBody(t, map[string]any{
		"name":           pool.Name,
		"max_concurrent": 10,
	}))
	rr := httptest.NewRecorder()
	newUsagePoolRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── deleteUsagePool ───────────────────────────────────────────────────────────

func TestDeleteUsagePool_NotFound(t *testing.T) {
	st := fake.New()
	defer st.Close()

	req := newReq(t, http.MethodDelete, "/api/v1/usage-pools/missing", nil)
	rr := httptest.NewRecorder()
	newUsagePoolRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
}

func TestDeleteUsagePool_Success(t *testing.T) {
	st := fake.New()
	defer st.Close()
	pool := seedUsagePool(t, st)

	req := newReq(t, http.MethodDelete, "/api/v1/usage-pools/"+pool.ID, nil)
	rr := httptest.NewRecorder()
	newUsagePoolRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d", rr.Code)
	}
}
