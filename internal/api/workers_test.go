// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Unit tests for the worker REST handlers — task 85.
//
// Route coverage:
//   GET  /api/v1/workers              — listWorkers
//   GET  /api/v1/workers/{id}         — getWorker
//   POST /api/v1/workers/{id}/disable — disableWorker
//   POST /api/v1/workers/{id}/enable  — enableWorker

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

// ── router helper ─────────────────────────────────────────────────────────────

func newWorkerRouter(st store.Store) chi.Router {
	h := newWorkerHandler(st, newTestLogger())
	r := chi.NewRouter()
	r.Get("/api/v1/workers", h.listWorkers)
	r.Get("/api/v1/workers/{id}", h.getWorker)
	r.Post("/api/v1/workers/{id}/disable", h.disableWorker)
	r.Post("/api/v1/workers/{id}/enable", h.enableWorker)
	return r
}

// ── seed helper ───────────────────────────────────────────────────────────────

func seedWorker(t *testing.T, st *fake.Store, status store.WorkerStatus) store.Worker {
	t.Helper()
	now := time.Now()
	w := store.Worker{
		ID:           uuid.NewString(),
		FarmID:       "farm-1",
		Hostname:     "node-" + uuid.NewString()[:8],
		OS:           "linux",
		OSVersion:    "22.04",
		CPUCount:     16,
		RAMMb:        32768,
		Status:       status,
		RegisteredAt: now,
		UpdatedAt:    now,
	}
	created, err := st.RegisterWorker(t.Context(), w)
	if err != nil {
		t.Fatalf("seedWorker: %v", err)
	}
	return created
}

// ── GET /api/v1/workers ───────────────────────────────────────────────────────

func TestListWorkers(t *testing.T) {
	t.Run("empty store returns empty page", func(t *testing.T) {
		st := fake.New()
		r := newWorkerRouter(st)
		req := newReq(t, http.MethodGet, "/api/v1/workers", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}
		var resp workerListResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Total != 0 {
			t.Errorf("total = %d, want 0", resp.Total)
		}
	})

	t.Run("returns seeded workers", func(t *testing.T) {
		st := fake.New()
		r := newWorkerRouter(st)
		seedWorker(t, st, store.WorkerStatusOnline)
		seedWorker(t, st, store.WorkerStatusOffline)

		req := newReq(t, http.MethodGet, "/api/v1/workers", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}
		var resp workerListResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Total != 2 {
			t.Errorf("total = %d, want 2", resp.Total)
		}
	})

	t.Run("filter by status", func(t *testing.T) {
		st := fake.New()
		r := newWorkerRouter(st)
		seedWorker(t, st, store.WorkerStatusOnline)
		seedWorker(t, st, store.WorkerStatusDisabled)

		req := newReq(t, http.MethodGet, "/api/v1/workers?status=online", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}
		var resp workerListResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, item := range resp.Items {
			if item.Status != "online" {
				t.Errorf("got status %q in online filter", item.Status)
			}
		}
	})

	t.Run("filter by farm_id", func(t *testing.T) {
		st := fake.New()
		r := newWorkerRouter(st)
		w1 := seedWorker(t, st, store.WorkerStatusOnline)

		// Insert a worker in a different farm.
		now := time.Now()
		if _, err := st.RegisterWorker(t.Context(), store.Worker{
			ID:           uuid.NewString(),
			FarmID:       "farm-other",
			Hostname:     "other-node",
			Status:       store.WorkerStatusOnline,
			RegisteredAt: now,
			UpdatedAt:    now,
		}); err != nil {
			t.Fatalf("RegisterWorker other-node: %v", err)
		}

		req := newReq(t, http.MethodGet, "/api/v1/workers?farm_id=farm-1", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}
		var resp workerListResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, item := range resp.Items {
			if item.FarmID != "farm-1" {
				t.Errorf("got farm_id %q in farm-1 filter", item.FarmID)
			}
		}
		_ = w1
	})

	t.Run("pagination limit respected", func(t *testing.T) {
		st := fake.New()
		r := newWorkerRouter(st)
		seedWorker(t, st, store.WorkerStatusOnline)
		seedWorker(t, st, store.WorkerStatusOnline)
		seedWorker(t, st, store.WorkerStatusOnline)

		req := newReq(t, http.MethodGet, "/api/v1/workers?limit=2", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}
		var resp workerListResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Items) != 2 {
			t.Errorf("items len = %d, want 2", len(resp.Items))
		}
		if resp.Limit != 2 {
			t.Errorf("limit = %d, want 2", resp.Limit)
		}
		if resp.Total != 3 {
			t.Errorf("total = %d, want 3", resp.Total)
		}
	})
}

// ── GET /api/v1/workers/{id} ──────────────────────────────────────────────────

func TestGetWorker(t *testing.T) {
	t.Run("existing worker returns 200 with detail", func(t *testing.T) {
		st := fake.New()
		r := newWorkerRouter(st)
		w := seedWorker(t, st, store.WorkerStatusOnline)

		req := newReq(t, http.MethodGet, "/api/v1/workers/"+w.ID, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body)
		}
		var resp workerDetailResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.ID != w.ID {
			t.Errorf("id = %q, want %q", resp.ID, w.ID)
		}
		if resp.Status != "online" {
			t.Errorf("status = %q, want online", resp.Status)
		}
		if resp.OS != "linux" {
			t.Errorf("os = %q, want linux", resp.OS)
		}
		// No active task seeded — current_task should be nil.
		if resp.CurrentTask != nil {
			t.Errorf("expected nil current_task, got %+v", resp.CurrentTask)
		}
	})

	t.Run("unknown worker returns 404", func(t *testing.T) {
		st := fake.New()
		r := newWorkerRouter(st)
		req := newReq(t, http.MethodGet, "/api/v1/workers/ghost", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rr.Code)
		}
	})
}

// ── POST /api/v1/workers/{id}/disable ────────────────────────────────────────

func TestDisableWorker(t *testing.T) {
	t.Run("online worker becomes disabled", func(t *testing.T) {
		st := fake.New()
		r := newWorkerRouter(st)
		w := seedWorker(t, st, store.WorkerStatusOnline)

		req := newReq(t, http.MethodPost, "/api/v1/workers/"+w.ID+"/disable", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body)
		}
		var resp workerActionResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Status != "disabled" {
			t.Errorf("status = %q, want disabled", resp.Status)
		}
		if resp.ID != w.ID {
			t.Errorf("id = %q, want %q", resp.ID, w.ID)
		}
		// Confirm store state.
		stored, err := st.GetWorker(t.Context(), w.ID)
		if err != nil {
			t.Fatalf("GetWorker: %v", err)
		}
		if stored.Status != store.WorkerStatusDisabled {
			t.Errorf("stored status = %q, want disabled", stored.Status)
		}
	})

	t.Run("disabling an already-disabled worker is idempotent (200)", func(t *testing.T) {
		st := fake.New()
		r := newWorkerRouter(st)
		w := seedWorker(t, st, store.WorkerStatusDisabled)

		req := newReq(t, http.MethodPost, "/api/v1/workers/"+w.ID+"/disable", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 (idempotent), got %d", rr.Code)
		}
	})

	t.Run("unknown worker returns 404", func(t *testing.T) {
		st := fake.New()
		r := newWorkerRouter(st)
		req := newReq(t, http.MethodPost, "/api/v1/workers/ghost/disable", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rr.Code)
		}
	})
}

// ── POST /api/v1/workers/{id}/enable ─────────────────────────────────────────

func TestEnableWorker(t *testing.T) {
	t.Run("disabled worker becomes online", func(t *testing.T) {
		st := fake.New()
		r := newWorkerRouter(st)
		w := seedWorker(t, st, store.WorkerStatusDisabled)

		req := newReq(t, http.MethodPost, "/api/v1/workers/"+w.ID+"/enable", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body)
		}
		var resp workerActionResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Status != "online" {
			t.Errorf("status = %q, want online", resp.Status)
		}
		// Confirm store state.
		stored, err := st.GetWorker(t.Context(), w.ID)
		if err != nil {
			t.Fatalf("GetWorker: %v", err)
		}
		if stored.Status != store.WorkerStatusOnline {
			t.Errorf("stored status = %q, want online", stored.Status)
		}
	})

	t.Run("enabling an already-online worker is idempotent (200)", func(t *testing.T) {
		st := fake.New()
		r := newWorkerRouter(st)
		w := seedWorker(t, st, store.WorkerStatusOnline)

		req := newReq(t, http.MethodPost, "/api/v1/workers/"+w.ID+"/enable", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 (idempotent), got %d", rr.Code)
		}
	})

	t.Run("unknown worker returns 404", func(t *testing.T) {
		st := fake.New()
		r := newWorkerRouter(st)
		req := newReq(t, http.MethodPost, "/api/v1/workers/ghost/enable", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rr.Code)
		}
	})
}
