// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Unit tests for farm REST handlers.
//
// Coverage targets (error paths not yet reached):
//   POST   /api/v1/farms      — invalid JSON, missing name, conflict, store error
//   GET    /api/v1/farms      — success
//   GET    /api/v1/farms/{id} — not found
//   PUT    /api/v1/farms/{id} — invalid JSON, missing name, not found, conflict
//   DELETE /api/v1/farms/{id} — not found, success

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

func newFarmRouter(st store.Store) chi.Router {
	h := newFarmHandler(st, newTestLogger())
	r := chi.NewRouter()
	r.Post("/api/v1/farms", h.createFarm)
	r.Get("/api/v1/farms", h.listFarms)
	r.Get("/api/v1/farms/{id}", h.getFarm)
	r.Put("/api/v1/farms/{id}", h.updateFarm)
	r.Delete("/api/v1/farms/{id}", h.deleteFarm)
	return r
}

func seedFarm(t *testing.T, st *fake.Store) store.Farm {
	t.Helper()
	now := time.Now()
	f := store.Farm{ID: uuid.NewString(), Name: "farm-" + uuid.NewString(), CreatedAt: now, UpdatedAt: now}
	created, err := st.CreateFarm(t.Context(), f)
	if err != nil {
		t.Fatalf("seedFarm: %v", err)
	}
	return created
}

// ── createFarm ────────────────────────────────────────────────────────────────

func TestCreateFarm_InvalidJSON(t *testing.T) {
	st := fake.New()
	defer st.Close()
	r := newFarmRouter(st)

	req := newReq(t, http.MethodPost, "/api/v1/farms", badJSON())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestCreateFarm_MissingName(t *testing.T) {
	st := fake.New()
	defer st.Close()
	r := newFarmRouter(st)

	req := newReq(t, http.MethodPost, "/api/v1/farms", jsonBody(t, map[string]any{}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestCreateFarm_Conflict(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm := seedFarm(t, st)
	r := newFarmRouter(st)

	req := newReq(t, http.MethodPost, "/api/v1/farms", jsonBody(t, map[string]any{
		"name": farm.Name, // duplicate
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d", rr.Code)
	}
}

func TestCreateFarm_Success(t *testing.T) {
	st := fake.New()
	defer st.Close()
	r := newFarmRouter(st)

	req := newReq(t, http.MethodPost, "/api/v1/farms", jsonBody(t, map[string]any{
		"name": "brand-new-farm",
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestCreateFarm_PersistsRetryPolicy verifies that max_attempts,
// retry_delay_seconds, and failure_limit are threaded through to the
// persisted farm and echoed in the create response.
func TestCreateFarm_PersistsRetryPolicy(t *testing.T) {
	st := fake.New()
	defer st.Close()
	r := newFarmRouter(st)

	req := newReq(t, http.MethodPost, "/api/v1/farms", jsonBody(t, map[string]any{
		"name":                "retry-policy-farm",
		"max_attempts":        4,
		"retry_delay_seconds": 15,
		"failure_limit":       10,
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["max_attempts"] != float64(4) {
		t.Errorf("max_attempts = %v, want 4", resp["max_attempts"])
	}
	if resp["retry_delay_seconds"] != float64(15) {
		t.Errorf("retry_delay_seconds = %v, want 15", resp["retry_delay_seconds"])
	}
	if resp["failure_limit"] != float64(10) {
		t.Errorf("failure_limit = %v, want 10", resp["failure_limit"])
	}

	id, ok := resp["id"].(string)
	if !ok {
		t.Fatalf("id not a string: %v", resp["id"])
	}
	stored, err := st.GetFarm(t.Context(), id)
	if err != nil {
		t.Fatalf("GetFarm: %v", err)
	}
	if stored.MaxAttempts == nil || *stored.MaxAttempts != 4 {
		t.Errorf("stored max_attempts = %v, want 4", stored.MaxAttempts)
	}
	if stored.RetryDelaySeconds == nil || *stored.RetryDelaySeconds != 15 {
		t.Errorf("stored retry_delay_seconds = %v, want 15", stored.RetryDelaySeconds)
	}
	if stored.FailureLimit == nil || *stored.FailureLimit != 10 {
		t.Errorf("stored failure_limit = %v, want 10", stored.FailureLimit)
	}
}

// TestUpdateFarm_PersistsRetryPolicy verifies that PUT updates the retry
// policy overrides and echoes them in the response.
func TestUpdateFarm_PersistsRetryPolicy(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm := seedFarm(t, st)
	r := newFarmRouter(st)

	req := newReq(t, http.MethodPut, "/api/v1/farms/"+farm.ID, jsonBody(t, map[string]any{
		"name":                farm.Name,
		"max_attempts":        7,
		"retry_delay_seconds": 30,
		"failure_limit":       40,
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["max_attempts"] != float64(7) {
		t.Errorf("max_attempts = %v, want 7", resp["max_attempts"])
	}
	if resp["retry_delay_seconds"] != float64(30) {
		t.Errorf("retry_delay_seconds = %v, want 30", resp["retry_delay_seconds"])
	}
	if resp["failure_limit"] != float64(40) {
		t.Errorf("failure_limit = %v, want 40", resp["failure_limit"])
	}

	stored, err := st.GetFarm(t.Context(), farm.ID)
	if err != nil {
		t.Fatalf("GetFarm: %v", err)
	}
	if stored.MaxAttempts == nil || *stored.MaxAttempts != 7 {
		t.Errorf("stored max_attempts = %v, want 7", stored.MaxAttempts)
	}
	if stored.RetryDelaySeconds == nil || *stored.RetryDelaySeconds != 30 {
		t.Errorf("stored retry_delay_seconds = %v, want 30", stored.RetryDelaySeconds)
	}
	if stored.FailureLimit == nil || *stored.FailureLimit != 40 {
		t.Errorf("stored failure_limit = %v, want 40", stored.FailureLimit)
	}
}

// ── listFarms ─────────────────────────────────────────────────────────────────

func TestListFarms_Success(t *testing.T) {
	st := fake.New()
	defer st.Close()
	seedFarm(t, st)
	r := newFarmRouter(st)

	req := newReq(t, http.MethodGet, "/api/v1/farms", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
}

// ── getFarm ───────────────────────────────────────────────────────────────────

func TestGetFarm_NotFound(t *testing.T) {
	st := fake.New()
	defer st.Close()
	r := newFarmRouter(st)

	req := newReq(t, http.MethodGet, "/api/v1/farms/missing", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
}

func TestGetFarm_Found(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm := seedFarm(t, st)
	r := newFarmRouter(st)

	req := newReq(t, http.MethodGet, "/api/v1/farms/"+farm.ID, nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
}

// ── updateFarm ────────────────────────────────────────────────────────────────

func TestUpdateFarm_InvalidJSON(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm := seedFarm(t, st)
	r := newFarmRouter(st)

	req := newReq(t, http.MethodPut, "/api/v1/farms/"+farm.ID, badJSON())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestUpdateFarm_MissingName(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm := seedFarm(t, st)
	r := newFarmRouter(st)

	req := newReq(t, http.MethodPut, "/api/v1/farms/"+farm.ID, jsonBody(t, map[string]any{}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestUpdateFarm_NotFound(t *testing.T) {
	st := fake.New()
	defer st.Close()
	r := newFarmRouter(st)

	req := newReq(t, http.MethodPut, "/api/v1/farms/missing", jsonBody(t, map[string]any{
		"name": "anything",
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
}

func TestUpdateFarm_Conflict(t *testing.T) {
	st := fake.New()
	defer st.Close()
	f1 := seedFarm(t, st)
	f2 := seedFarm(t, st)
	r := newFarmRouter(st)

	req := newReq(t, http.MethodPut, "/api/v1/farms/"+f1.ID, jsonBody(t, map[string]any{
		"name": f2.Name, // collision
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d", rr.Code)
	}
}

func TestUpdateFarm_Success(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm := seedFarm(t, st)
	r := newFarmRouter(st)

	req := newReq(t, http.MethodPut, "/api/v1/farms/"+farm.ID, jsonBody(t, map[string]any{
		"name":                 farm.Name,
		"max_concurrent_tasks": 5,
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── deleteFarm ────────────────────────────────────────────────────────────────

func TestDeleteFarm_NotFound(t *testing.T) {
	st := fake.New()
	defer st.Close()
	r := newFarmRouter(st)

	req := newReq(t, http.MethodDelete, "/api/v1/farms/missing", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
}

func TestDeleteFarm_Success(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm := seedFarm(t, st)
	r := newFarmRouter(st)

	req := newReq(t, http.MethodDelete, "/api/v1/farms/"+farm.ID, nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d", rr.Code)
	}
}
