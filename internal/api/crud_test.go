// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Unit tests for the task-81 CRUD handlers: farms, queues, storage-locations,
// and usage-pools. Each test spins up a chi router with the fake in-memory
// store so the database is never touched.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func newTestLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// jsonBody serializes v and returns a *bytes.Buffer suitable for an HTTP body.
func jsonBody(t *testing.T, v any) *bytes.Buffer {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("jsonBody: %v", err)
	}
	return bytes.NewBuffer(b)
}

// newReq is a test helper that creates an HTTP request carrying the test
// context (satisfying the noctx linter).
func newReq(t *testing.T, method, target string, body io.Reader) *http.Request {
	t.Helper()
	return httptest.NewRequestWithContext(t.Context(), method, target, body)
}

// badJSON returns an io.Reader with syntactically invalid JSON, useful for
// testing that handlers reject malformed request bodies.
func badJSON() io.Reader {
	return strings.NewReader("{bad json}")
}

// mustListFarms retrieves all farms from the fake store, fataling on error.
func mustListFarms(t *testing.T, st *fake.Store) []store.Farm {
	t.Helper()
	farms, err := st.ListFarms(t.Context())
	if err != nil {
		t.Fatalf("ListFarms: %v", err)
	}
	return farms
}

// ── Farm tests ────────────────────────────────────────────────────────────────

func TestFarmCRUD(t *testing.T) {
	st := fake.New()
	logger := newTestLogger()
	h := newFarmHandler(st, logger)

	r := chi.NewRouter()
	r.Post("/farms", h.createFarm)
	r.Get("/farms", h.listFarms)
	r.Get("/farms/{id}", h.getFarm)
	r.Put("/farms/{id}", h.updateFarm)
	r.Delete("/farms/{id}", h.deleteFarm)

	// ── POST /farms — success ─────────────────────────────────────────────────
	t.Run("create farm", func(t *testing.T) {
		body := jsonBody(t, createFarmRequest{Name: "alpha", Description: "first farm", MaxConcurrentTasks: 10})
		req := newReq(t, http.MethodPost, "/farms", body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusCreated {
			t.Fatalf("expected 201, got %d — body: %s", rr.Code, rr.Body)
		}
		var resp farmResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Name != "alpha" {
			t.Errorf("name = %q, want %q", resp.Name, "alpha")
		}
		if resp.MaxConcurrentTasks != 10 {
			t.Errorf("max_concurrent_tasks = %d, want 10", resp.MaxConcurrentTasks)
		}
		if resp.ID == "" {
			t.Error("id must not be empty")
		}
	})

	// ── POST /farms — missing name ────────────────────────────────────────────
	t.Run("create farm missing name", func(t *testing.T) {
		body := jsonBody(t, createFarmRequest{Description: "no name"})
		req := newReq(t, http.MethodPost, "/farms", body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rr.Code)
		}
	})

	// ── POST /farms — duplicate name ──────────────────────────────────────────
	t.Run("create farm duplicate name", func(t *testing.T) {
		body := jsonBody(t, createFarmRequest{Name: "alpha"})
		req := newReq(t, http.MethodPost, "/farms", body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusConflict {
			t.Fatalf("expected 409, got %d", rr.Code)
		}
	})

	// ── GET /farms — list ─────────────────────────────────────────────────────
	t.Run("list farms", func(t *testing.T) {
		req := newReq(t, http.MethodGet, "/farms", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}
		var resp []farmResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp) != 1 {
			t.Errorf("expected 1 farm, got %d", len(resp))
		}
	})

	// ── GET /farms/{id} — found ───────────────────────────────────────────────
	t.Run("get farm found", func(t *testing.T) {
		id := mustListFarms(t, st)[0].ID
		req := newReq(t, http.MethodGet, "/farms/"+id, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}
	})

	// ── GET /farms/{id} — not found ───────────────────────────────────────────
	t.Run("get farm not found", func(t *testing.T) {
		req := newReq(t, http.MethodGet, "/farms/does-not-exist", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rr.Code)
		}
	})

	// ── PUT /farms/{id} — success ─────────────────────────────────────────────
	t.Run("update farm", func(t *testing.T) {
		id := mustListFarms(t, st)[0].ID
		body := jsonBody(t, updateFarmRequest{Name: "alpha-updated", MaxConcurrentTasks: 20})
		req := newReq(t, http.MethodPut, "/farms/"+id, body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body)
		}
		var resp farmResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Name != "alpha-updated" {
			t.Errorf("name = %q, want %q", resp.Name, "alpha-updated")
		}
		if resp.MaxConcurrentTasks != 20 {
			t.Errorf("max_concurrent_tasks = %d, want 20", resp.MaxConcurrentTasks)
		}
	})

	// ── DELETE /farms/{id} — success ─────────────────────────────────────────
	t.Run("delete farm", func(t *testing.T) {
		id := mustListFarms(t, st)[0].ID
		req := newReq(t, http.MethodDelete, "/farms/"+id, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("expected 204, got %d", rr.Code)
		}
		// Confirm it's gone.
		req2 := newReq(t, http.MethodGet, "/farms/"+id, nil)
		rr2 := httptest.NewRecorder()
		r.ServeHTTP(rr2, req2)
		if rr2.Code != http.StatusNotFound {
			t.Errorf("expected 404 after delete, got %d", rr2.Code)
		}
	})
}

// ── Queue tests ───────────────────────────────────────────────────────────────

func TestQueueCRUD(t *testing.T) {
	st := fake.New()
	logger := newTestLogger()

	// Pre-seed a farm so queue creation can reference it.
	farm, err := st.CreateFarm(t.Context(), store.Farm{ID: "farm-1", Name: "farm-one"})
	if err != nil {
		t.Fatalf("pre-seed farm: %v", err)
	}

	h := newQueueHandler(st, logger)

	r := chi.NewRouter()
	r.Post("/queues", h.createQueue)
	r.Get("/queues", h.listQueues)
	r.Get("/queues/{id}", h.getQueue)
	r.Put("/queues/{id}", h.updateQueue)
	r.Delete("/queues/{id}", h.deleteQueue)

	var createdID string

	// ── POST — success ────────────────────────────────────────────────────────
	t.Run("create queue", func(t *testing.T) {
		body := jsonBody(t, createQueueRequest{FarmID: farm.ID, Name: "render", Priority: 5, MaxConcurrentTasks: 4})
		req := newReq(t, http.MethodPost, "/queues", body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("expected 201, got %d — body: %s", rr.Code, rr.Body)
		}
		var resp queueResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		createdID = resp.ID
	})

	// ── POST — bad farm_id ────────────────────────────────────────────────────
	t.Run("create queue bad farm", func(t *testing.T) {
		body := jsonBody(t, createQueueRequest{FarmID: "no-such-farm", Name: "x"})
		req := newReq(t, http.MethodPost, "/queues", body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rr.Code)
		}
	})

	// ── PUT — farm validation ─────────────────────────────────────────────────
	t.Run("update queue validates farm", func(t *testing.T) {
		body := jsonBody(t, updateQueueRequest{FarmID: "ghost-farm", Name: "render"})
		req := newReq(t, http.MethodPut, "/queues/"+createdID, body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for unknown farm_id, got %d", rr.Code)
		}
	})

	// ── PUT — success ─────────────────────────────────────────────────────────
	t.Run("update queue success", func(t *testing.T) {
		body := jsonBody(t, updateQueueRequest{FarmID: farm.ID, Name: "render-hd", Priority: 10})
		req := newReq(t, http.MethodPut, "/queues/"+createdID, body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body)
		}
	})

	// ── GET list ──────────────────────────────────────────────────────────────
	t.Run("list queues", func(t *testing.T) {
		req := newReq(t, http.MethodGet, "/queues?farm_id="+farm.ID, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}
		var resp queueListResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Total < 1 {
			t.Errorf("expected at least 1 queue, total=%d", resp.Total)
		}
	})

	// ── DELETE ────────────────────────────────────────────────────────────────
	t.Run("delete queue", func(t *testing.T) {
		req := newReq(t, http.MethodDelete, "/queues/"+createdID, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("expected 204, got %d", rr.Code)
		}
	})
}

// ── Storage-location tests ────────────────────────────────────────────────────

func TestStorageLocationCRUD(t *testing.T) {
	st := fake.New()
	logger := newTestLogger()
	h := newStorageLocationHandler(st, logger)

	r := chi.NewRouter()
	r.Post("/storage-locations", h.createStorageLocation)
	r.Get("/storage-locations", h.listStorageLocations)
	r.Get("/storage-locations/{id}", h.getStorageLocation)
	r.Put("/storage-locations/{id}", h.updateStorageLocation)
	r.Delete("/storage-locations/{id}", h.deleteStorageLocation)

	var createdID string

	// ── POST — success ────────────────────────────────────────────────────────
	t.Run("create storage location", func(t *testing.T) {
		body := jsonBody(t, createStorageLocationRequest{
			Name:  "nas_shows",
			Roots: map[string]string{"default": "/mnt/nas/shows"},
		})
		req := newReq(t, http.MethodPost, "/storage-locations", body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("expected 201, got %d — body: %s", rr.Code, rr.Body)
		}
		var resp storageLocationResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		createdID = resp.ID
		if resp.Roots["default"] != "/mnt/nas/shows" {
			t.Errorf("roots mismatch: %v", resp.Roots)
		}
	})

	// ── POST — type must not be supplied ─────────────────────────────────────
	t.Run("create storage location invalid type", func(t *testing.T) {
		body := jsonBody(t, map[string]any{"name": "x", "type": "nfs"})
		req := newReq(t, http.MethodPost, "/storage-locations", body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for invalid type, got %d", rr.Code)
		}
	})

	// ── GET — not found ───────────────────────────────────────────────────────
	t.Run("get storage location not found", func(t *testing.T) {
		req := newReq(t, http.MethodGet, "/storage-locations/nope", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rr.Code)
		}
	})

	// ── PUT — success ─────────────────────────────────────────────────────────
	t.Run("update storage location", func(t *testing.T) {
		body := jsonBody(t, updateStorageLocationRequest{
			Name:  "nas_shows",
			Roots: map[string]string{"default": "s3://bucket/shows"},
		})
		req := newReq(t, http.MethodPut, "/storage-locations/"+createdID, body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body)
		}
	})

	// ── DELETE ────────────────────────────────────────────────────────────────
	t.Run("delete storage location", func(t *testing.T) {
		req := newReq(t, http.MethodDelete, "/storage-locations/"+createdID, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("expected 204, got %d", rr.Code)
		}
	})
}

// ── Usage-pool tests ──────────────────────────────────────────────────────────

func TestUsagePoolCRUD(t *testing.T) {
	st := fake.New()
	logger := newTestLogger()
	h := newUsagePoolHandler(st, logger)

	r := chi.NewRouter()
	r.Post("/usage-pools", h.createUsagePool)
	r.Get("/usage-pools", h.listUsagePools)
	r.Get("/usage-pools/{id}", h.getUsagePool)
	r.Put("/usage-pools/{id}", h.updateUsagePool)
	r.Delete("/usage-pools/{id}", h.deleteUsagePool)

	var createdID string

	// ── POST — success ────────────────────────────────────────────────────────
	t.Run("create usage pool", func(t *testing.T) {
		body := jsonBody(t, createUsagePoolRequest{
			Name:          "arnold_render",
			ServerHint:    "10.0.0.50:5053",
			MaxConcurrent: 20,
		})
		req := newReq(t, http.MethodPost, "/usage-pools", body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("expected 201, got %d — body: %s", rr.Code, rr.Body)
		}
		var resp usagePoolResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		createdID = resp.ID
		if resp.MaxConcurrent != 20 {
			t.Errorf("max_concurrent = %d, want 20", resp.MaxConcurrent)
		}
		// A freshly created pool has no claims: in_use 0, available == max.
		if resp.InUse != 0 {
			t.Errorf("in_use = %d, want 0", resp.InUse)
		}
		if resp.Available != 20 {
			t.Errorf("available = %d, want 20", resp.Available)
		}
	})

	// ── POST — max_concurrent validation ─────────────────────────────────────
	t.Run("create usage pool zero max_concurrent", func(t *testing.T) {
		body := jsonBody(t, createUsagePoolRequest{Name: "x", MaxConcurrent: 0})
		req := newReq(t, http.MethodPost, "/usage-pools", body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for MaxConcurrent=0, got %d", rr.Code)
		}
	})

	// ── POST — duplicate name ─────────────────────────────────────────────────
	t.Run("create usage pool duplicate", func(t *testing.T) {
		body := jsonBody(t, createUsagePoolRequest{Name: "arnold_render", MaxConcurrent: 5})
		req := newReq(t, http.MethodPost, "/usage-pools", body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusConflict {
			t.Fatalf("expected 409, got %d", rr.Code)
		}
	})

	// ── GET list ──────────────────────────────────────────────────────────────
	t.Run("list usage pools", func(t *testing.T) {
		req := newReq(t, http.MethodGet, "/usage-pools", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}
		var resp []usagePoolResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp) < 1 {
			t.Errorf("expected at least 1 pool, got %d", len(resp))
		}
	})

	// ── PUT — success ─────────────────────────────────────────────────────────
	t.Run("update usage pool", func(t *testing.T) {
		body := jsonBody(t, updateUsagePoolRequest{
			Name:          "arnold_render",
			MaxConcurrent: 30,
		})
		req := newReq(t, http.MethodPut, "/usage-pools/"+createdID, body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body)
		}
	})

	// ── DELETE ────────────────────────────────────────────────────────────────
	t.Run("delete usage pool", func(t *testing.T) {
		req := newReq(t, http.MethodDelete, "/usage-pools/"+createdID, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("expected 204, got %d", rr.Code)
		}
		// Confirm gone.
		req2 := newReq(t, http.MethodGet, "/usage-pools/"+createdID, nil)
		rr2 := httptest.NewRecorder()
		r.ServeHTTP(rr2, req2)
		if rr2.Code != http.StatusNotFound {
			t.Errorf("expected 404 after delete, got %d", rr2.Code)
		}
	})
}

// TestUsagePoolUtilizationReporting verifies that list/get responses report live
// utilization (in_use / available), and that available never goes negative.
func TestUsagePoolUtilizationReporting(t *testing.T) {
	st := fake.New()
	logger := newTestLogger()
	h := newUsagePoolHandler(st, logger)
	ctx := context.Background()

	r := chi.NewRouter()
	r.Get("/usage-pools", h.listUsagePools)
	r.Get("/usage-pools/{id}", h.getUsagePool)

	pool, err := st.CreateUsagePool(ctx, store.UsagePool{
		ID: "p1", Name: "arnold", MaxConcurrent: 3,
	})
	if err != nil {
		t.Fatalf("CreateUsagePool: %v", err)
	}

	// Two active claims → in_use 2, available 1.
	for _, id := range []string{"co1", "co2"} {
		if _, err := st.CreateClaim(ctx, store.UsageClaim{
			ID: id, PoolID: pool.ID, TaskAttemptID: "a-" + id, ClaimedAt: time.Now(),
		}); err != nil {
			t.Fatalf("CreateClaim %s: %v", id, err)
		}
	}

	t.Run("list reports usage", func(t *testing.T) {
		req := newReq(t, http.MethodGet, "/usage-pools", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		var resp []usagePoolResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp) != 1 {
			t.Fatalf("len = %d, want 1", len(resp))
		}
		if resp[0].InUse != 2 || resp[0].Available != 1 {
			t.Errorf("got in_use=%d available=%d, want 2/1", resp[0].InUse, resp[0].Available)
		}
	})

	t.Run("get reports usage", func(t *testing.T) {
		req := newReq(t, http.MethodGet, "/usage-pools/p1", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		var resp usagePoolResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.InUse != 2 || resp.Available != 1 {
			t.Errorf("got in_use=%d available=%d, want 2/1", resp.InUse, resp.Available)
		}
	})

	t.Run("available floors at zero", func(t *testing.T) {
		// Add two more claims (4 total) against a max of 3 → available 0, not -1.
		for _, id := range []string{"co3", "co4"} {
			if _, err := st.CreateClaim(ctx, store.UsageClaim{
				ID: id, PoolID: pool.ID, TaskAttemptID: "a-" + id, ClaimedAt: time.Now(),
			}); err != nil {
				t.Fatalf("CreateClaim %s: %v", id, err)
			}
		}
		req := newReq(t, http.MethodGet, "/usage-pools/p1", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		var resp usagePoolResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.InUse != 4 || resp.Available != 0 {
			t.Errorf("got in_use=%d available=%d, want 4/0", resp.InUse, resp.Available)
		}
	})
}
