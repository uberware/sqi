// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Unit tests for compute-location REST handlers.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

func newComputeLocationRouter(st store.Store) chi.Router {
	h := newComputeLocationHandler(st, newTestLogger())
	r := chi.NewRouter()
	r.Post("/api/v1/compute-locations", h.createComputeLocation)
	r.Get("/api/v1/compute-locations", h.listComputeLocations)
	r.Get("/api/v1/compute-locations/{id}", h.getComputeLocation)
	r.Put("/api/v1/compute-locations/{id}", h.updateComputeLocation)
	r.Delete("/api/v1/compute-locations/{id}", h.deleteComputeLocation)
	return r
}

func seedComputeLoc(t *testing.T, st *fake.Store) store.ComputeLocation {
	t.Helper()
	now := time.Now()
	loc := store.ComputeLocation{
		ID:        uuid.NewString(),
		Name:      "loc-" + uuid.NewString()[:8],
		CreatedAt: now,
		UpdatedAt: now,
	}
	created, err := st.CreateComputeLocation(t.Context(), loc)
	if err != nil {
		t.Fatalf("seedComputeLoc: %v", err)
	}
	return created
}

// ── createComputeLocation ─────────────────────────────────────────────────────

func TestCreateComputeLocation_InvalidJSON(t *testing.T) {
	st := fake.New()
	defer st.Close()

	req := newReq(t, http.MethodPost, "/api/v1/compute-locations", badJSON())
	rr := httptest.NewRecorder()
	newComputeLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestCreateComputeLocation_MissingName(t *testing.T) {
	st := fake.New()
	defer st.Close()

	req := newReq(t, http.MethodPost, "/api/v1/compute-locations", jsonBody(t, map[string]any{}))
	rr := httptest.NewRecorder()
	newComputeLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestCreateComputeLocation_InvalidName(t *testing.T) {
	st := fake.New()
	defer st.Close()

	req := newReq(t, http.MethodPost, "/api/v1/compute-locations", jsonBody(t, map[string]any{
		"name": "on prem", // space — invalid
	}))
	rr := httptest.NewRecorder()
	newComputeLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for name with space, got %d", rr.Code)
	}
	// Bad name must not have been persisted.
	locs, err := st.ListComputeLocations(t.Context())
	if err != nil {
		t.Fatalf("ListComputeLocations: %v", err)
	}
	if len(locs) != 0 {
		t.Errorf("expected no locations persisted, got %d", len(locs))
	}
}

func TestCreateComputeLocation_Conflict(t *testing.T) {
	st := fake.New()
	defer st.Close()
	loc := seedComputeLoc(t, st)

	req := newReq(t, http.MethodPost, "/api/v1/compute-locations", jsonBody(t, map[string]any{
		"name": loc.Name,
	}))
	rr := httptest.NewRecorder()
	newComputeLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d", rr.Code)
	}
}

func TestCreateComputeLocation_Success(t *testing.T) {
	st := fake.New()
	defer st.Close()

	req := newReq(t, http.MethodPost, "/api/v1/compute-locations", jsonBody(t, map[string]any{
		"name":        "on-prem",
		"description": "on-site machines",
	}))
	rr := httptest.NewRecorder()
	newComputeLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp computeLocationResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Name != "on-prem" {
		t.Errorf("name = %q, want %q", resp.Name, "on-prem")
	}
	if resp.ID == "" {
		t.Error("id must not be empty")
	}
}

// ── listComputeLocations ──────────────────────────────────────────────────────

func TestListComputeLocations_WorkerCount(t *testing.T) {
	st := fake.New()
	defer st.Close()

	// Create a compute location.
	req := newReq(t, http.MethodPost, "/api/v1/compute-locations", jsonBody(t, map[string]any{
		"name": "on-prem",
	}))
	rr := httptest.NewRecorder()
	newComputeLocationRouter(st).ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d: %s", rr.Code, rr.Body.String())
	}

	// Seed an online worker at that location.
	if _, err := st.RegisterWorker(t.Context(), store.Worker{
		ID:              "w1",
		ComputeLocation: "on-prem",
		Status:          store.WorkerStatusOnline,
	}); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}

	// List — worker_count must be 1.
	req = newReq(t, http.MethodGet, "/api/v1/compute-locations", nil)
	rr = httptest.NewRecorder()
	newComputeLocationRouter(st).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: want 200, got %d", rr.Code)
	}

	var list []computeLocationResponse
	if err := json.NewDecoder(rr.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("len(list) = %d, want 1", len(list))
	}
	if list[0].WorkerCount != 1 {
		t.Errorf("worker_count = %d, want 1", list[0].WorkerCount)
	}
}

func TestListComputeLocations_Success(t *testing.T) {
	st := fake.New()
	defer st.Close()
	seedComputeLoc(t, st)

	req := newReq(t, http.MethodGet, "/api/v1/compute-locations", nil)
	rr := httptest.NewRecorder()
	newComputeLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
}

// ── getComputeLocation ────────────────────────────────────────────────────────

func TestGetComputeLocation_NotFound(t *testing.T) {
	st := fake.New()
	defer st.Close()

	req := newReq(t, http.MethodGet, "/api/v1/compute-locations/missing", nil)
	rr := httptest.NewRecorder()
	newComputeLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
}

func TestGetComputeLocation_Found(t *testing.T) {
	st := fake.New()
	defer st.Close()
	loc := seedComputeLoc(t, st)

	req := newReq(t, http.MethodGet, "/api/v1/compute-locations/"+loc.ID, nil)
	rr := httptest.NewRecorder()
	newComputeLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
}

// ── updateComputeLocation ─────────────────────────────────────────────────────

func TestUpdateComputeLocation_InvalidJSON(t *testing.T) {
	st := fake.New()
	defer st.Close()
	loc := seedComputeLoc(t, st)

	req := newReq(t, http.MethodPut, "/api/v1/compute-locations/"+loc.ID, badJSON())
	rr := httptest.NewRecorder()
	newComputeLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestUpdateComputeLocation_MissingName(t *testing.T) {
	st := fake.New()
	defer st.Close()
	loc := seedComputeLoc(t, st)

	req := newReq(t, http.MethodPut, "/api/v1/compute-locations/"+loc.ID, jsonBody(t, map[string]any{
		"description": "no name",
	}))
	rr := httptest.NewRecorder()
	newComputeLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestUpdateComputeLocation_NotFound(t *testing.T) {
	st := fake.New()
	defer st.Close()

	req := newReq(t, http.MethodPut, "/api/v1/compute-locations/missing", jsonBody(t, map[string]any{
		"name": "foo",
	}))
	rr := httptest.NewRecorder()
	newComputeLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
}

func TestUpdateComputeLocation_Conflict(t *testing.T) {
	st := fake.New()
	defer st.Close()
	loc1 := seedComputeLoc(t, st)
	loc2 := seedComputeLoc(t, st)

	req := newReq(t, http.MethodPut, "/api/v1/compute-locations/"+loc1.ID, jsonBody(t, map[string]any{
		"name": loc2.Name,
	}))
	rr := httptest.NewRecorder()
	newComputeLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d", rr.Code)
	}
}

func TestUpdateComputeLocation_Success(t *testing.T) {
	st := fake.New()
	defer st.Close()
	loc := seedComputeLoc(t, st)

	req := newReq(t, http.MethodPut, "/api/v1/compute-locations/"+loc.ID, jsonBody(t, map[string]any{
		"name":        loc.Name,
		"description": "updated description",
	}))
	rr := httptest.NewRecorder()
	newComputeLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── deleteComputeLocation ─────────────────────────────────────────────────────

func TestDeleteComputeLocation_NotFound(t *testing.T) {
	st := fake.New()
	defer st.Close()

	req := newReq(t, http.MethodDelete, "/api/v1/compute-locations/missing", nil)
	rr := httptest.NewRecorder()
	newComputeLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
}

func TestDeleteComputeLocation_Success(t *testing.T) {
	st := fake.New()
	defer st.Close()
	loc := seedComputeLoc(t, st)

	req := newReq(t, http.MethodDelete, "/api/v1/compute-locations/"+loc.ID, nil)
	rr := httptest.NewRecorder()
	newComputeLocationRouter(st).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d", rr.Code)
	}
	// Confirm gone.
	req2 := newReq(t, http.MethodGet, "/api/v1/compute-locations/"+loc.ID, nil)
	rr2 := httptest.NewRecorder()
	newComputeLocationRouter(st).ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusNotFound {
		t.Errorf("want 404 after delete, got %d", rr2.Code)
	}
}
