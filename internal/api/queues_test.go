// SPDX-License-Identifier: AGPL-3.0-only

package api

// Unit tests for queue REST handlers.
//
// Coverage targets:
//   POST   /api/v1/queues      — createQueue (validation, conflict, store error)
//   GET    /api/v1/queues      — listQueues  (success, store error, paused filter)
//   GET    /api/v1/queues/{id} — getQueue    (found, not found, store error)
//   PUT    /api/v1/queues/{id} — updateQueue (success, validation, not found, conflict)
//   DELETE /api/v1/queues/{id} — deleteQueue (success, not found, store error)

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

// ── router helper ─────────────────────────────────────────────────────────────

func newQueueRouter(st store.Store) chi.Router {
	h := newQueueHandler(st, newTestLogger())
	r := chi.NewRouter()
	r.Post("/api/v1/queues", h.createQueue)
	r.Get("/api/v1/queues", h.listQueues)
	r.Get("/api/v1/queues/{id}", h.getQueue)
	r.Put("/api/v1/queues/{id}", h.updateQueue)
	r.Delete("/api/v1/queues/{id}", h.deleteQueue)
	return r
}

// seedFarmAndQueue creates a farm and a queue in st, returning both.
func seedFarmAndQueue(t *testing.T, st *fake.Store) (store.Farm, store.Queue) {
	t.Helper()
	ctx := t.Context()
	now := time.Now()

	farm := store.Farm{ID: uuid.NewString(), Name: "farm-q-" + uuid.NewString(), CreatedAt: now, UpdatedAt: now}
	f, err := st.CreateFarm(ctx, farm)
	if err != nil {
		t.Fatalf("seedFarmAndQueue: CreateFarm: %v", err)
	}

	q := store.Queue{
		ID: uuid.NewString(), FarmID: f.ID, Name: "queue-" + uuid.NewString(),
		Priority: 10, CreatedAt: now, UpdatedAt: now,
	}
	created, err := st.CreateQueue(ctx, q)
	if err != nil {
		t.Fatalf("seedFarmAndQueue: CreateQueue: %v", err)
	}
	return f, created
}

// ── createQueue ───────────────────────────────────────────────────────────────

func TestCreateQueue_MissingFarmID(t *testing.T) {
	st := fake.New()
	defer st.Close()
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPost, "/api/v1/queues", jsonBody(t, map[string]any{
		"name": "q1",
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestCreateQueue_MissingName(t *testing.T) {
	st := fake.New()
	defer st.Close()
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPost, "/api/v1/queues", jsonBody(t, map[string]any{
		"farm_id": "some-id",
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestCreateQueue_FarmNotFound(t *testing.T) {
	st := fake.New()
	defer st.Close()
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPost, "/api/v1/queues", jsonBody(t, map[string]any{
		"farm_id": "nonexistent",
		"name":    "q1",
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestCreateQueue_Conflict(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm, q := seedFarmAndQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPost, "/api/v1/queues", jsonBody(t, map[string]any{
		"farm_id": farm.ID,
		"name":    q.Name, // duplicate name in same farm
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d", rr.Code)
	}
}

func TestCreateQueue_InvalidJSON(t *testing.T) {
	st := fake.New()
	defer st.Close()
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPost, "/api/v1/queues", badJSON())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestCreateQueue_Success(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm, _ := seedFarmAndQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPost, "/api/v1/queues", jsonBody(t, map[string]any{
		"farm_id": farm.ID,
		"name":    "brand-new-queue",
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── listQueues ────────────────────────────────────────────────────────────────

func TestListQueues_Success(t *testing.T) {
	st := fake.New()
	defer st.Close()
	seedFarmAndQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodGet, "/api/v1/queues", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
}

func TestListQueues_PausedFilter(t *testing.T) {
	st := fake.New()
	defer st.Close()
	r := newQueueRouter(st)

	for _, paused := range []string{"true", "false"} {
		req := newReq(t, http.MethodGet, "/api/v1/queues?paused="+paused, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("paused=%s: want 200, got %d", paused, rr.Code)
		}
	}
}

// ── getQueue ──────────────────────────────────────────────────────────────────

func TestGetQueue_NotFound(t *testing.T) {
	st := fake.New()
	defer st.Close()
	r := newQueueRouter(st)

	req := newReq(t, http.MethodGet, "/api/v1/queues/missing", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
}

func TestGetQueue_Found(t *testing.T) {
	st := fake.New()
	defer st.Close()
	_, q := seedFarmAndQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodGet, "/api/v1/queues/"+q.ID, nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
}

// ── updateQueue ───────────────────────────────────────────────────────────────

func TestUpdateQueue_InvalidJSON(t *testing.T) {
	st := fake.New()
	defer st.Close()
	_, q := seedFarmAndQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPut, "/api/v1/queues/"+q.ID, badJSON())
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestUpdateQueue_MissingFarmID(t *testing.T) {
	st := fake.New()
	defer st.Close()
	_, q := seedFarmAndQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPut, "/api/v1/queues/"+q.ID, jsonBody(t, map[string]any{
		"name": "new-name",
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestUpdateQueue_FarmNotFound(t *testing.T) {
	st := fake.New()
	defer st.Close()
	_, q := seedFarmAndQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPut, "/api/v1/queues/"+q.ID, jsonBody(t, map[string]any{
		"farm_id": "no-such-farm",
		"name":    "renamed",
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestUpdateQueue_NotFound(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm, _ := seedFarmAndQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPut, "/api/v1/queues/missing", jsonBody(t, map[string]any{
		"farm_id": farm.ID,
		"name":    "renamed",
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
}

func TestUpdateQueue_Conflict(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm, q1 := seedFarmAndQueue(t, st)
	// Create a second queue whose name we'll try to collide with.
	now := time.Now()
	q2 := store.Queue{ID: uuid.NewString(), FarmID: farm.ID, Name: "other-queue-" + uuid.NewString(), CreatedAt: now, UpdatedAt: now}
	if _, err := st.CreateQueue(t.Context(), q2); err != nil {
		t.Fatalf("CreateQueue q2: %v", err)
	}
	r := newQueueRouter(st)

	// Try to rename q1 to q2's name — should conflict.
	req := newReq(t, http.MethodPut, "/api/v1/queues/"+q1.ID, jsonBody(t, map[string]any{
		"farm_id": farm.ID,
		"name":    q2.Name,
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d", rr.Code)
	}
}

func TestUpdateQueue_Success(t *testing.T) {
	st := fake.New()
	defer st.Close()
	farm, q := seedFarmAndQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodPut, "/api/v1/queues/"+q.ID, jsonBody(t, map[string]any{
		"farm_id": farm.ID,
		"name":    q.Name, // same name, no conflict
	}))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── deleteQueue ───────────────────────────────────────────────────────────────

func TestDeleteQueue_NotFound(t *testing.T) {
	st := fake.New()
	defer st.Close()
	r := newQueueRouter(st)

	req := newReq(t, http.MethodDelete, "/api/v1/queues/missing", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
}

func TestDeleteQueue_Success(t *testing.T) {
	st := fake.New()
	defer st.Close()
	_, q := seedFarmAndQueue(t, st)
	r := newQueueRouter(st)

	req := newReq(t, http.MethodDelete, "/api/v1/queues/"+q.ID, nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d", rr.Code)
	}
}
