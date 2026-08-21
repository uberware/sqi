// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Unit tests for the worker REST handlers.
//
// Route coverage:
//   GET  /api/v1/workers              — listWorkers
//   GET  /api/v1/workers/{id}         — getWorker
//   POST /api/v1/workers/{id}/disable — disableWorker
//   POST /api/v1/workers/{id}/enable  — enableWorker
//   DELETE /api/v1/workers/{id}       — removeWorker

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/ws"
)

// testOfflineThreshold is the heartbeat-timeout window used by the worker
// handler under test to decide whether a disabled worker is dead.
const testOfflineThreshold = 30 * time.Second

// recordingNotifier captures NotifyWorker events for assertions.
type recordingNotifier struct {
	ws.NoopNotifier

	events []ws.WorkerEvent
}

func (n *recordingNotifier) NotifyWorker(e ws.WorkerEvent) { n.events = append(n.events, e) }

// ── router helper ─────────────────────────────────────────────────────────────

func newWorkerRouter(st store.Store) chi.Router {
	return newWorkerRouterWithNotifier(st, nil)
}

func newWorkerRouterWithNotifier(st store.Store, notifier ws.Notifier) chi.Router {
	return newWorkerRouterWith(st, notifier, storeRevoker{store: st})
}

func newWorkerRouterWithRevoker(st store.Store, revoker WorkerRevoker) chi.Router {
	return newWorkerRouterWith(st, nil, revoker)
}

func newWorkerRouterWith(st store.Store, notifier ws.Notifier, revoker WorkerRevoker) chi.Router {
	h := newWorkerHandler(st, notifier, revoker, testOfflineThreshold, newTestLogger())
	r := chi.NewRouter()
	r.Get("/api/v1/workers", h.listWorkers)
	r.Get("/api/v1/workers/{id}", h.getWorker)
	r.Post("/api/v1/workers/{id}/disable", h.disableWorker)
	r.Post("/api/v1/workers/{id}/enable", h.enableWorker)
	r.Delete("/api/v1/workers/{id}", h.removeWorker)
	return r
}

// ── seed helper ───────────────────────────────────────────────────────────────

func seedWorker(t *testing.T, st *fake.Store, status store.WorkerStatus) store.Worker {
	t.Helper()
	now := time.Now()
	w := store.Worker{
		ID:           uuid.NewString(),
		FarmID:       "farm-1",
		Name:         "worker-" + uuid.NewString()[:4],
		Hostname:     "node-" + uuid.NewString()[:8],
		OS:           "linux",
		OSVersion:    "22.04",
		Version:      "v0.1.0",
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

func seedWorkerTask(
	t *testing.T,
	st *fake.Store,
	workerID string,
	status store.TaskStatus,
	name string,
) store.Task {
	t.Helper()
	now := time.Now()
	task := store.Task{
		ID:               uuid.NewString(),
		JobID:            "job-" + uuid.NewString()[:8],
		StepID:           "step-" + uuid.NewString()[:8],
		Name:             name,
		Status:           status,
		AssignedWorkerID: workerID,
		AssignedAt:       &now,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	created, err := st.CreateTask(t.Context(), task)
	if err != nil {
		t.Fatalf("seedWorkerTask: %v", err)
	}
	return created
}

// seedWorkerTaskForJob is like seedWorkerTask but pins the task to a specific
// (already-seeded) job id, so owner-scoping tests can look up the job's real
// owner via store.GetJob.
func seedWorkerTaskForJob(
	t *testing.T,
	st *fake.Store,
	workerID, jobID string,
	status store.TaskStatus,
	name string,
) store.Task {
	t.Helper()
	now := time.Now()
	task := store.Task{
		ID:               uuid.NewString(),
		JobID:            jobID,
		StepID:           "step-" + uuid.NewString()[:8],
		Name:             name,
		Status:           status,
		AssignedWorkerID: workerID,
		AssignedAt:       &now,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	created, err := st.CreateTask(t.Context(), task)
	if err != nil {
		t.Fatalf("seedWorkerTaskForJob: %v", err)
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

func TestListWorkers_SearchParam(t *testing.T) {
	st := fake.New()
	ctx := t.Context()
	for _, w := range []store.Worker{
		{ID: "w1", Hostname: "alpha.local", Status: store.WorkerStatusOnline, Tags: map[string]string{}},
		{ID: "w2", Hostname: "beta.local", Status: store.WorkerStatusOnline, Tags: map[string]string{}},
	} {
		if _, err := st.RegisterWorker(ctx, w); err != nil {
			t.Fatal(err)
		}
	}
	r := newWorkerRouter(st)

	req := newReq(t, http.MethodGet, "/api/v1/workers?search=alpha", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var resp workerListResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 {
		t.Errorf("total = %d, want 1", resp.Total)
	}
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
		if resp.Name != w.Name {
			t.Errorf("name = %q, want %q", resp.Name, w.Name)
		}
		if resp.Status != "online" {
			t.Errorf("status = %q, want online", resp.Status)
		}
		if resp.OS != "linux" {
			t.Errorf("os = %q, want linux", resp.OS)
		}
		if resp.Version != "v0.1.0" {
			t.Errorf("version = %q, want v0.1.0", resp.Version)
		}
		// No active task seeded — current_tasks should be empty.
		if len(resp.CurrentTasks) != 0 {
			t.Errorf("expected empty current_tasks, got %+v", resp.CurrentTasks)
		}
	})

	t.Run("returns all assigned and running tasks in current_tasks", func(t *testing.T) {
		st := fake.New()
		r := newWorkerRouter(st)
		w := seedWorker(t, st, store.WorkerStatusOnline)

		// Two active tasks on this worker (one assigned, one running) plus a
		// finished one that must be excluded, and one on another worker.
		seedWorkerTask(t, st, w.ID, store.TaskStatusRunning, "render-001")
		seedWorkerTask(t, st, w.ID, store.TaskStatusAssigned, "render-002")
		seedWorkerTask(t, st, w.ID, store.TaskStatusSucceeded, "render-done")
		seedWorkerTask(t, st, "other-worker", store.TaskStatusRunning, "elsewhere")

		req := newReq(t, http.MethodGet, "/api/v1/workers/"+w.ID, nil)
		// In production the auth middleware always attaches a principal —
		// the anonymous superuser when auth is disabled — before a request
		// reaches this handler; scopeFilter's "no principal" branch is a
		// fail-closed guard against misordered middleware, not the normal
		// path this test exercises.
		req = req.WithContext(auth.NewContext(req.Context(), auth.Principal{Superuser: true, Kind: auth.KindAnonymous}))
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body)
		}
		var resp workerDetailResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.CurrentTasks) != 2 {
			t.Fatalf("current_tasks len = %d, want 2 — got %+v", len(resp.CurrentTasks), resp.CurrentTasks)
		}
		names := map[string]bool{}
		for _, ct := range resp.CurrentTasks {
			names[ct.Name] = true
		}
		if !names["render-001"] || !names["render-002"] {
			t.Errorf("expected render-001 and render-002, got %+v", resp.CurrentTasks)
		}
		if names["render-done"] || names["elsewhere"] {
			t.Errorf("current_tasks leaked an excluded task: %+v", resp.CurrentTasks)
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

// TestGetWorker_OwnerScoping is the regression test for I-1: an owner-scoped
// caller (no jobs.read.all) must not see another owner's task — including its
// name, which for an expanded OpenJD task can carry parameter values such as
// scene paths — in current_tasks. Unscoped principals (operator, and the
// auth-off anonymous superuser) must keep seeing every current task exactly
// as before this fix.
func TestGetWorker_OwnerScoping(t *testing.T) {
	newSeededRouter := func(t *testing.T) (chi.Router, store.Worker) {
		t.Helper()
		st := fake.New()
		seedOwnedJob(t, st, "job-alice", "alice")
		seedOwnedJob(t, st, "job-bob", "bob")
		w := seedWorker(t, st, store.WorkerStatusOnline)
		seedWorkerTaskForJob(t, st, w.ID, "job-alice", store.TaskStatusRunning, "alice-task")
		seedWorkerTaskForJob(t, st, w.ID, "job-bob", store.TaskStatusRunning, "bob-task")
		return newWorkerRouter(st), w
	}

	get := func(t *testing.T, r chi.Router, w store.Worker, principal *auth.Principal) workerDetailResponse {
		t.Helper()
		req := newReq(t, http.MethodGet, "/api/v1/workers/"+w.ID, nil)
		if principal != nil {
			req = req.WithContext(auth.NewContext(req.Context(), *principal))
		}
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body)
		}
		var resp workerDetailResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp
	}

	t.Run("scoped user does not see another owner's task", func(t *testing.T) {
		r, w := newSeededRouter(t)
		alice := auth.Principal{Username: "alice", Roles: []string{"user"}}
		resp := get(t, r, w, &alice)

		if len(resp.CurrentTasks) != 1 {
			t.Fatalf("current_tasks len = %d, want 1 — got %+v", len(resp.CurrentTasks), resp.CurrentTasks)
		}
		if resp.CurrentTasks[0].Name != "alice-task" {
			t.Errorf("current_tasks[0].Name = %q, want alice-task", resp.CurrentTasks[0].Name)
		}
		for _, ct := range resp.CurrentTasks {
			if ct.JobID == "job-bob" || ct.Name == "bob-task" {
				t.Fatalf("scoped alice leaked bob's task: %+v", resp.CurrentTasks)
			}
		}
	})

	t.Run("operator (unscoped) still sees everything", func(t *testing.T) {
		r, w := newSeededRouter(t)
		bob := auth.Principal{Username: "someone", Roles: []string{"operator"}}
		resp := get(t, r, w, &bob)

		if len(resp.CurrentTasks) != 2 {
			t.Fatalf("current_tasks len = %d, want 2 — got %+v", len(resp.CurrentTasks), resp.CurrentTasks)
		}
	})

	t.Run("auth-off anonymous superuser still sees everything", func(t *testing.T) {
		r, w := newSeededRouter(t)
		anon := auth.Principal{Superuser: true, Kind: auth.KindAnonymous}
		resp := get(t, r, w, &anon)

		if len(resp.CurrentTasks) != 2 {
			t.Fatalf("current_tasks len = %d, want 2 — got %+v", len(resp.CurrentTasks), resp.CurrentTasks)
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

// ── DELETE /api/v1/workers/{id} ──────────────────────────────────────────────

// seedWorkerWithHeartbeat seeds a worker in the given status with a heartbeat
// aged by age (0 = none).
func seedWorkerWithHeartbeat(t *testing.T, st *fake.Store, status store.WorkerStatus, age time.Duration) store.Worker {
	t.Helper()
	w := seedWorker(t, st, status)
	if age > 0 {
		if err := st.UpdateWorkerHeartbeat(t.Context(), w.ID, time.Now().Add(-age)); err != nil {
			t.Fatalf("UpdateWorkerHeartbeat: %v", err)
		}
		// UpdateWorkerHeartbeat does not change status; keep the seeded one.
		if err := st.UpdateWorkerStatus(t.Context(), w.ID, status); err != nil {
			t.Fatalf("UpdateWorkerStatus: %v", err)
		}
	}
	return w
}

func TestRemoveWorker(t *testing.T) {
	t.Run("offline worker is removed (204) and emits a removed event", func(t *testing.T) {
		st := fake.New()
		notifier := &recordingNotifier{}
		r := newWorkerRouterWithNotifier(st, notifier)
		w := seedWorker(t, st, store.WorkerStatusOffline)

		req := newReq(t, http.MethodDelete, "/api/v1/workers/"+w.ID, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("expected 204, got %d — body: %s", rr.Code, rr.Body)
		}
		if _, err := st.GetWorker(t.Context(), w.ID); err == nil {
			t.Error("worker should be deleted")
		}
		if len(notifier.events) != 1 || notifier.events[0].WorkerID != w.ID ||
			notifier.events[0].Status != ws.WorkerStatusRemoved {
			t.Errorf("events = %+v, want one removed event for %s", notifier.events, w.ID)
		}
	})

	t.Run("dead disabled worker is removed (204)", func(t *testing.T) {
		st := fake.New()
		r := newWorkerRouter(st)
		w := seedWorkerWithHeartbeat(t, st, store.WorkerStatusDisabled, time.Hour)

		req := newReq(t, http.MethodDelete, "/api/v1/workers/"+w.ID, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("expected 204, got %d — body: %s", rr.Code, rr.Body)
		}
	})

	t.Run("online worker returns 409", func(t *testing.T) {
		st := fake.New()
		r := newWorkerRouter(st)
		w := seedWorker(t, st, store.WorkerStatusOnline)

		req := newReq(t, http.MethodDelete, "/api/v1/workers/"+w.ID, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusConflict {
			t.Fatalf("expected 409, got %d", rr.Code)
		}
		if _, err := st.GetWorker(t.Context(), w.ID); err != nil {
			t.Error("worker should survive a rejected remove")
		}
	})

	t.Run("live disabled worker returns 409", func(t *testing.T) {
		st := fake.New()
		r := newWorkerRouter(st)
		// Heartbeat well within the threshold → still alive.
		w := seedWorkerWithHeartbeat(t, st, store.WorkerStatusDisabled, time.Second)

		req := newReq(t, http.MethodDelete, "/api/v1/workers/"+w.ID, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusConflict {
			t.Fatalf("expected 409, got %d", rr.Code)
		}
	})

	t.Run("unknown worker returns 404", func(t *testing.T) {
		st := fake.New()
		r := newWorkerRouter(st)
		req := newReq(t, http.MethodDelete, "/api/v1/workers/ghost", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rr.Code)
		}
	})

	t.Run("active credential is revoked", func(t *testing.T) {
		st := fake.New()
		r := newWorkerRouter(st)
		w := seedWorker(t, st, store.WorkerStatusOffline)
		if _, err := st.CreateWorkerCredential(t.Context(), store.WorkerCredential{
			ID: uuid.NewString(), WorkerID: w.ID, PublicKey: genPublicKey(t), EnrolledAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed CreateWorkerCredential: %v", err)
		}

		req := newReq(t, http.MethodDelete, "/api/v1/workers/"+w.ID, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("expected 204, got %d — body: %s", rr.Code, rr.Body)
		}
		if _, err := st.GetActiveWorkerCredentialByWorkerID(t.Context(), w.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("GetActiveWorkerCredentialByWorkerID after delete = %v, want store.ErrNotFound (credential revoked)", err)
		}
	})

	t.Run("no credential still succeeds", func(t *testing.T) {
		st := fake.New()
		rev := &recordingRevoker{err: store.ErrNotFound}
		r := newWorkerRouterWithRevoker(st, rev)
		w := seedWorker(t, st, store.WorkerStatusOffline)

		req := newReq(t, http.MethodDelete, "/api/v1/workers/"+w.ID, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("expected 204, got %d — body: %s", rr.Code, rr.Body)
		}
		if rev.calledWith != w.ID {
			t.Errorf("revoker called with %q, want %q — delete must still call through the revoker even when it has nothing to revoke", rev.calledWith, w.ID)
		}
	})

	t.Run("already-revoked credential still succeeds", func(t *testing.T) {
		st := fake.New()
		r := newWorkerRouter(st)
		w := seedWorker(t, st, store.WorkerStatusOffline)
		if _, err := st.CreateWorkerCredential(t.Context(), store.WorkerCredential{
			ID: uuid.NewString(), WorkerID: w.ID, PublicKey: genPublicKey(t), EnrolledAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed CreateWorkerCredential: %v", err)
		}
		if err := st.RevokeWorkerCredential(t.Context(), w.ID, time.Now().UTC()); err != nil {
			t.Fatalf("seed RevokeWorkerCredential: %v", err)
		}

		req := newReq(t, http.MethodDelete, "/api/v1/workers/"+w.ID, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("expected 204, got %d — body: %s", rr.Code, rr.Body)
		}
	})

	t.Run("revoker failure does not fail the delete", func(t *testing.T) {
		st := fake.New()
		rev := &recordingRevoker{err: errors.New("broker reload failed")}
		r := newWorkerRouterWithRevoker(st, rev)
		w := seedWorker(t, st, store.WorkerStatusOffline)

		req := newReq(t, http.MethodDelete, "/api/v1/workers/"+w.ID, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("expected 204, got %d — body: %s", rr.Code, rr.Body)
		}
		if _, err := st.GetWorker(t.Context(), w.ID); err == nil {
			t.Error("worker should still be deleted even when the revoke fails")
		}
	})
}

// TestWorkerResponse_Removable verifies the server-authoritative removable flag
// for each status/liveness combination via the detail endpoint.
func TestWorkerResponse_Removable(t *testing.T) {
	cases := []struct {
		name   string
		status store.WorkerStatus
		age    time.Duration
		want   bool
	}{
		{"offline", store.WorkerStatusOffline, 0, true},
		{"online", store.WorkerStatusOnline, 0, false},
		{"disabled dead", store.WorkerStatusDisabled, time.Hour, true},
		{"disabled live", store.WorkerStatusDisabled, time.Second, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := fake.New()
			r := newWorkerRouter(st)
			w := seedWorkerWithHeartbeat(t, st, tc.status, tc.age)

			req := newReq(t, http.MethodGet, "/api/v1/workers/"+w.ID, nil)
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", rr.Code)
			}
			var resp workerDetailResponse
			if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Removable != tc.want {
				t.Errorf("removable = %v, want %v", resp.Removable, tc.want)
			}
		})
	}
}
