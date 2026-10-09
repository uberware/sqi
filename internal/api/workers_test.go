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
	"slices"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/store/storetest"
	"github.com/uberware/sqi/internal/ws"
)

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
	return newWorkerRouterWith(st, notifier, storeRevoker{store: st}, nil)
}

func newWorkerRouterWithRevoker(st store.Store, revoker WorkerRevoker) chi.Router {
	return newWorkerRouterWith(st, nil, revoker, nil)
}

// recordingWaker records the workers the handler asked the scheduler to wake.
type recordingWaker struct{ woken []string }

func (w *recordingWaker) WakeWorker(id string) { w.woken = append(w.woken, id) }

func newWorkerRouterWith(st store.Store, notifier ws.Notifier, revoker WorkerRevoker, waker workerWaker) chi.Router {
	h := newWorkerHandler(st, notifier, revoker, waker, newTestLogger())
	r := chi.NewRouter()
	r.Get("/api/v1/workers", h.listWorkers)
	r.Get("/api/v1/workers/{id}", h.getWorker)
	r.Post("/api/v1/workers/{id}/disable", h.disableWorker)
	r.Post("/api/v1/workers/{id}/enable", h.enableWorker)
	r.Delete("/api/v1/workers/{id}", h.removeWorker)
	return r
}

// ── seed helper ───────────────────────────────────────────────────────────────

// seedWorker registers a worker with the given effective status. A disabled
// status is an online worker an operator then disabled; seedDisabledWorker
// seeds one with a chosen liveness.
func seedWorker(t *testing.T, st *fake.Store, status store.WorkerStatus) store.Worker {
	t.Helper()
	if status == store.WorkerStatusDisabled {
		return seedDisabledWorker(t, st, store.WorkerStatusOnline)
	}
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
	created, _, err := st.RegisterWorker(t.Context(), w)
	if err != nil {
		t.Fatalf("seedWorker: %v", err)
	}
	return created
}

// seedDisabledWorker seeds a disabled worker whose liveness is online (a paused
// machine) or offline (one that has since gone away).
func seedDisabledWorker(t *testing.T, st *fake.Store, liveness store.WorkerStatus) store.Worker {
	t.Helper()
	w := seedWorker(t, st, liveness)
	w, err := st.SetWorkerDisabled(t.Context(), w.ID, true)
	if err != nil {
		t.Fatalf("SetWorkerDisabled: %v", err)
	}
	return w
}

// seedWorkerTask seeds a job of its own holding one task, name, that workerID
// holds in status. See [seedWorkerTaskForJob].
func seedWorkerTask(
	t *testing.T,
	st *fake.Store,
	workerID string,
	status store.TaskStatus,
	name string,
) store.Task {
	t.Helper()
	return seedWorkerTaskForJob(t, st, workerID, "job-"+uuid.NewString()[:8], "", status, name)
}

// seedWorkerTaskForJob is like seedWorkerTask but names the job and its owner,
// so owner-scoping tests can look up the job's real owner via store.GetJob. The
// job, its step and its task are one submission: a task cannot be added to a
// job that already exists. An assigned or running task is leased to workerID
// (see [storetest.SubmitLeasing]); a succeeded one is run to completion on workerID, so
// it stays attributed to the worker the way a finished task is.
func seedWorkerTaskForJob(
	t *testing.T,
	st *fake.Store,
	workerID, jobID, owner string,
	status store.TaskStatus,
	name string,
) store.Task {
	t.Helper()
	farm, queue := seedFarmQueue(t, st)
	stepID, taskID := "step-"+uuid.NewString()[:8], uuid.NewString()
	seeded := status
	if status == store.TaskStatusSucceeded {
		seeded = store.TaskStatusRunning
	}
	out, attempts := storetest.SubmitLeasing(t, st, store.JobSubmission{
		Job: store.Job{
			ID: jobID, FarmID: farm.ID, QueueID: queue.ID, Name: jobID, Owner: owner,
			Priority: 50, Status: store.JobStatusRunning, TemplateFormat: store.TemplateFormatJSON,
		},
		Steps: []store.Step{{ID: stepID, JobID: jobID, Name: "Step1", Status: store.StepStatusRunning}},
		Tasks: []store.Task{{ID: taskID, JobID: jobID, StepID: stepID, Name: name, Status: seeded}},
	}, storetest.LeaseTo(workerID))
	if status != store.TaskStatusSucceeded {
		return out.Tasks[0]
	}
	storetest.Complete(t, st, attempts[taskID], store.TaskStatusSucceeded)
	task, err := st.GetTask(t.Context(), taskID)
	if err != nil {
		t.Fatalf("seedWorkerTaskForJob: GetTask: %v", err)
	}
	return task
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
		if _, _, err := st.RegisterWorker(t.Context(), store.Worker{
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
		if _, _, err := st.RegisterWorker(ctx, w); err != nil {
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

// TestGetWorker_OwnerScoping pins that an owner-scoped caller (no
// jobs.read.all) must not see another owner's task — including its name,
// which for an expanded OpenJD task can carry parameter values such as scene
// paths — in current_tasks. Unscoped principals (operator, and the auth-off
// anonymous superuser) must keep seeing every current task.
func TestGetWorker_OwnerScoping(t *testing.T) {
	newSeededRouter := func(t *testing.T) (chi.Router, store.Worker) {
		t.Helper()
		st := fake.New()
		w := seedWorker(t, st, store.WorkerStatusOnline)
		seedWorkerTaskForJob(t, st, w.ID, "job-alice", "alice", store.TaskStatusRunning, "alice-task")
		seedWorkerTaskForJob(t, st, w.ID, "job-bob", "bob", store.TaskStatusRunning, "bob-task")
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
		// Confirm store state: disabled, liveness untouched.
		stored, err := st.GetWorker(t.Context(), w.ID)
		if err != nil {
			t.Fatalf("GetWorker: %v", err)
		}
		if !stored.Disabled || stored.Status != store.WorkerStatusOnline {
			t.Errorf("stored = %q disabled=%v, want online disabled", stored.Status, stored.Disabled)
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
		if stored.Disabled || stored.Status != store.WorkerStatusOnline {
			t.Errorf("stored = %q disabled=%v, want online enabled", stored.Status, stored.Disabled)
		}
	})

	t.Run("enabling a disabled worker that went offline leaves it offline", func(t *testing.T) {
		// Enable clears the operator's flag and nothing else: liveness is the
		// worker's to report, so a machine that is gone stays offline until it
		// registers again.
		st := fake.New()
		r := newWorkerRouter(st)
		w := seedDisabledWorker(t, st, store.WorkerStatusOffline)

		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, newReq(t, http.MethodPost, "/api/v1/workers/"+w.ID+"/enable", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body)
		}
		var resp workerActionResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Status != "offline" {
			t.Errorf("status = %q, want offline", resp.Status)
		}
		stored, err := st.GetWorker(t.Context(), w.ID)
		if err != nil {
			t.Fatalf("GetWorker: %v", err)
		}
		if stored.Disabled || stored.Status != store.WorkerStatusOffline {
			t.Errorf("stored = %q disabled=%v, want offline enabled", stored.Status, stored.Disabled)
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

// TestEnableWorker_WakesItsParkedLeases pins that enabling a worker wakes the
// lease requests the scheduler parked while it was disabled, so it is leased
// work at once rather than after the rest of the hold; disabling wakes nothing,
// and neither does an enable that fails.
func TestEnableWorker_WakesItsParkedLeases(t *testing.T) {
	st := fake.New()
	waker := &recordingWaker{}
	r := newWorkerRouterWith(st, nil, storeRevoker{store: st}, waker)
	w := seedWorker(t, st, store.WorkerStatusOnline)

	for _, step := range []struct {
		path      string
		wantCode  int
		wantWoken []string
	}{
		{"/api/v1/workers/" + w.ID + "/disable", http.StatusOK, nil},
		{"/api/v1/workers/" + w.ID + "/enable", http.StatusOK, []string{w.ID}},
		{"/api/v1/workers/ghost/enable", http.StatusNotFound, []string{w.ID}},
	} {
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, newReq(t, http.MethodPost, step.path, nil))
		if rr.Code != step.wantCode {
			t.Fatalf("POST %s: expected %d, got %d — body: %s", step.path, step.wantCode, rr.Code, rr.Body)
		}
		if !slices.Equal(waker.woken, step.wantWoken) {
			t.Fatalf("after POST %s: woken = %v, want %v", step.path, waker.woken, step.wantWoken)
		}
	}
}

// ── DELETE /api/v1/workers/{id} ──────────────────────────────────────────────

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
		w := seedDisabledWorker(t, st, store.WorkerStatusOffline)

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
		// Still online: a paused machine, not a gone one.
		w := seedDisabledWorker(t, st, store.WorkerStatusOnline)

		req := newReq(t, http.MethodDelete, "/api/v1/workers/"+w.ID, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusConflict {
			t.Fatalf("expected 409, got %d", rr.Code)
		}
	})

	t.Run("offline worker with a running task returns 409 until it is idle", func(t *testing.T) {
		// The guarded delete refuses a worker with work in flight, which the
		// status-only pre-check cannot see.
		st := fake.New()
		r := newWorkerRouter(st)
		w := seedWorker(t, st, store.WorkerStatusOffline)
		task := seedWorkerTask(t, st, w.ID, store.TaskStatusRunning, "busy")

		req := newReq(t, http.MethodDelete, "/api/v1/workers/"+w.ID, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusConflict {
			t.Fatalf("expected 409, got %d — body: %s", rr.Code, rr.Body)
		}
		if _, err := st.GetWorker(t.Context(), w.ID); err != nil {
			t.Fatalf("worker should survive a refused remove: GetWorker: %v", err)
		}

		attempt, err := st.LatestTaskAttempt(t.Context(), task.ID)
		if err != nil {
			t.Fatalf("LatestTaskAttempt: %v", err)
		}
		storetest.Complete(t, st, attempt, store.TaskStatusSucceeded)
		rr = httptest.NewRecorder()
		r.ServeHTTP(rr, newReq(t, http.MethodDelete, "/api/v1/workers/"+w.ID, nil))
		if rr.Code != http.StatusNoContent {
			t.Fatalf("once idle: expected 204, got %d — body: %s", rr.Code, rr.Body)
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

	t.Run("revoker failure blocks the delete", func(t *testing.T) {
		// The worker row must survive a revoke failure: revoking runs
		// BEFORE deleting specifically so that a failure here never leaves
		// a deleted worker whose credential nothing revoked and nothing
		// will ever reap.
		st := fake.New()
		rev := &recordingRevoker{err: errors.New("store write failed")}
		r := newWorkerRouterWithRevoker(st, rev)
		w := seedWorker(t, st, store.WorkerStatusOffline)

		req := newReq(t, http.MethodDelete, "/api/v1/workers/"+w.ID, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500, got %d — body: %s", rr.Code, rr.Body)
		}
		if _, err := st.GetWorker(t.Context(), w.ID); err != nil {
			t.Errorf("worker should NOT be deleted when the revoke fails: GetWorker: %v", err)
		}
	})
}

// TestWorkerResponse_Removable verifies the server-authoritative removable flag
// for each status/liveness combination via the detail endpoint.
func TestWorkerResponse_Removable(t *testing.T) {
	cases := []struct {
		name       string
		liveness   store.WorkerStatus
		disabled   bool
		wantStatus string
		want       bool
	}{
		{"offline", store.WorkerStatusOffline, false, "offline", true},
		{"online", store.WorkerStatusOnline, false, "online", false},
		{"disabled dead", store.WorkerStatusOffline, true, "disabled", true},
		{"disabled live", store.WorkerStatusOnline, true, "disabled", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := fake.New()
			r := newWorkerRouter(st)
			var w store.Worker
			if tc.disabled {
				w = seedDisabledWorker(t, st, tc.liveness)
			} else {
				w = seedWorker(t, st, tc.liveness)
			}

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
			if resp.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", resp.Status, tc.wantStatus)
			}
		})
	}
}
