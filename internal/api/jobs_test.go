// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Unit tests for the job REST handlers — task 85.
//
// Each sub-test spins up a chi router wired to the in-memory fake store so no
// real database or NATS instance is required.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// ── fakeScheduler ─────────────────────────────────────────────────────────────

// fakeScheduler implements [jobCanceler] for tests so cancelJob can run
// without a live NATS/scheduler instance.
type fakeScheduler struct {
	cancelErr error // non-nil forces CancelJob to return this error
}

func (f *fakeScheduler) CancelJob(_ context.Context, _ string) error {
	return f.cancelErr
}

// ── router helpers ────────────────────────────────────────────────────────────

// newJobRouter wires a jobHandler onto a chi router with the same paths as
// router.go, using the provided store and fakeScheduler.
func newJobRouter(st store.Store, sched jobCanceler) chi.Router {
	sub := openjd.NewSubmitter(st)
	h := newJobHandler(st, sub, sched, newTestLogger())
	r := chi.NewRouter()
	r.Post("/api/v1/jobs", h.submitJob)
	r.Get("/api/v1/jobs", h.listJobs)
	r.Get("/api/v1/jobs/{id}", h.getJob)
	r.Patch("/api/v1/jobs/{id}", h.patchJob)
	r.Delete("/api/v1/jobs/{id}", h.cancelJob)
	return r
}

// seedJob pre-populates the fake store with one farm, one queue, and one job.
// Returns the seeded job for use in subsequent assertions.
func seedJob(t *testing.T, st *fake.Store, status store.JobStatus) store.Job {
	t.Helper()
	ctx := t.Context()

	farm, err := st.CreateFarm(ctx, store.Farm{ID: "farm-1", Name: "f"})
	if err != nil {
		// Farm may already exist from a prior seedJob call in the same store.
		farms, listErr := st.ListFarms(ctx)
		if listErr != nil {
			t.Fatalf("seedJob: ListFarms: %v", listErr)
		}
		for _, f := range farms {
			if f.ID == "farm-1" {
				farm = f
				break
			}
		}
	}

	queue, err := st.CreateQueue(ctx, store.Queue{ID: "queue-1", FarmID: farm.ID, Name: "q"})
	if err != nil {
		page, listErr := st.ListQueues(ctx, store.ListQueuesOptions{})
		if listErr != nil {
			t.Fatalf("seedJob: ListQueues: %v", listErr)
		}
		for _, q := range page.Items {
			if q.ID == "queue-1" {
				queue = q
				break
			}
		}
	}

	now := time.Now()
	job := store.Job{
		ID:             uuid.NewString(),
		FarmID:         farm.ID,
		QueueID:        queue.ID,
		Name:           "test-job",
		Owner:          "alice",
		Priority:       50,
		Status:         status,
		TemplateFormat: store.TemplateFormatJSON,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	created, err := st.CreateJob(ctx, job)
	if err != nil {
		t.Fatalf("seedJob: %v", err)
	}
	return created
}

// minimalOpenJDJSON returns a minimal valid OpenJD template as a JSON string.
// It uses a unique job name so repeated submissions don't collide.
func minimalOpenJDJSON(name string) string {
	return `{
  "specificationVersion": "jobtemplate-2023-09",
  "name": "` + name + `",
  "steps": [
    {
      "name": "Step1",
      "script": {
        "actions": {
          "onRun": { "command": "echo", "args": ["hello"] }
        }
      }
    }
  ]
}`
}

// ── POST /api/v1/jobs ─────────────────────────────────────────────────────────

func TestSubmitJob(t *testing.T) {
	st := fake.New()

	// Pre-seed a farm + queue so the submission can reference them.
	ctx := t.Context()
	_, err := st.CreateFarm(ctx, store.Farm{ID: "farm-1", Name: "farm-one"})
	if err != nil {
		t.Fatalf("create farm: %v", err)
	}
	_, err = st.CreateQueue(ctx, store.Queue{ID: "queue-1", FarmID: "farm-1", Name: "render"})
	if err != nil {
		t.Fatalf("create queue: %v", err)
	}

	r := newJobRouter(st, &fakeScheduler{})

	t.Run("valid submission returns 201 and job body", func(t *testing.T) {
		body := strings.NewReader(minimalOpenJDJSON("SubmitTest"))
		req := newReq(t, http.MethodPost, "/api/v1/jobs?farm_id=farm-1&queue_id=queue-1&owner=alice", body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusCreated {
			t.Fatalf("expected 201, got %d — body: %s", rr.Code, rr.Body)
		}
		var resp jobResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.ID == "" {
			t.Error("id must not be empty")
		}
		if resp.FarmID != "farm-1" {
			t.Errorf("farm_id = %q, want farm-1", resp.FarmID)
		}
		if resp.Owner != "alice" {
			t.Errorf("owner = %q, want alice", resp.Owner)
		}
		if resp.Status != "pending" {
			t.Errorf("status = %q, want pending", resp.Status)
		}
	})

	t.Run("missing farm_id returns 400", func(t *testing.T) {
		body := strings.NewReader(minimalOpenJDJSON("MissingFarm"))
		req := newReq(t, http.MethodPost, "/api/v1/jobs?queue_id=queue-1", body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rr.Code)
		}
	})

	t.Run("missing queue_id returns 400", func(t *testing.T) {
		body := strings.NewReader(minimalOpenJDJSON("MissingQueue"))
		req := newReq(t, http.MethodPost, "/api/v1/jobs?farm_id=farm-1", body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rr.Code)
		}
	})

	t.Run("invalid JSON template returns 422", func(t *testing.T) {
		body := strings.NewReader(`{"specificationVersion": "bad-version", "name": "x", "steps": []}`)
		req := newReq(t, http.MethodPost, "/api/v1/jobs?farm_id=farm-1&queue_id=queue-1", body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("expected 422, got %d — body: %s", rr.Code, rr.Body)
		}
	})

	t.Run("YAML content type accepted", func(t *testing.T) {
		yaml := `specificationVersion: jobtemplate-2023-09
name: YAMLJob
steps:
  - name: S1
    script:
      actions:
        onRun:
          command: echo
          args: ["ok"]
`
		body := strings.NewReader(yaml)
		req := newReq(t, http.MethodPost, "/api/v1/jobs?farm_id=farm-1&queue_id=queue-1", body)
		req.Header.Set("Content-Type", "application/yaml")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("expected 201 for YAML, got %d — body: %s", rr.Code, rr.Body)
		}
	})

	t.Run("response includes Content-Type application/json", func(t *testing.T) {
		body := strings.NewReader(minimalOpenJDJSON("CTCheck"))
		req := newReq(t, http.MethodPost, "/api/v1/jobs?farm_id=farm-1&queue_id=queue-1", body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		ct := rr.Header().Get("Content-Type")
		if !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
	})
}

// ── GET /api/v1/jobs ──────────────────────────────────────────────────────────

func TestListJobs(t *testing.T) {
	st := fake.New()
	r := newJobRouter(st, &fakeScheduler{})

	t.Run("empty store returns empty items", func(t *testing.T) {
		req := newReq(t, http.MethodGet, "/api/v1/jobs", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}
		var resp jobListResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Total != 0 {
			t.Errorf("total = %d, want 0", resp.Total)
		}
	})

	// Seed two jobs: one pending, one running. The IDs are not used in
	// subsequent assertions — the sub-tests query via filters and totals.
	_ = seedJob(t, st, store.JobStatusPending)
	_ = seedJob(t, st, store.JobStatusRunning)

	t.Run("lists all jobs", func(t *testing.T) {
		req := newReq(t, http.MethodGet, "/api/v1/jobs", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}
		var resp jobListResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Total < 2 {
			t.Errorf("total = %d, want >= 2", resp.Total)
		}
		if resp.Limit != store.DefaultLimit {
			t.Errorf("limit = %d, want %d", resp.Limit, store.DefaultLimit)
		}
		if resp.Offset != 0 {
			t.Errorf("offset = %d, want 0", resp.Offset)
		}
	})

	t.Run("filter by status", func(t *testing.T) {
		req := newReq(t, http.MethodGet, "/api/v1/jobs?status=running", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		var resp jobListResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, item := range resp.Items {
			if item.Status != "running" {
				t.Errorf("got item with status %q in running filter", item.Status)
			}
		}
	})

	t.Run("filter by queue_id", func(t *testing.T) {
		req := newReq(t, http.MethodGet, "/api/v1/jobs?queue_id=queue-1", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		var resp jobListResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, item := range resp.Items {
			if item.QueueID != "queue-1" {
				t.Errorf("got item with queue_id %q in queue-1 filter", item.QueueID)
			}
		}
	})

	t.Run("pagination limit and offset", func(t *testing.T) {
		req := newReq(t, http.MethodGet, "/api/v1/jobs?limit=1&offset=0", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		var resp jobListResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Items) != 1 {
			t.Errorf("items len = %d, want 1 (limit=1)", len(resp.Items))
		}
		if resp.Limit != 1 {
			t.Errorf("limit = %d, want 1", resp.Limit)
		}
	})
}

// ── GET /api/v1/jobs/{id} ─────────────────────────────────────────────────────

func TestGetJob(t *testing.T) {
	st := fake.New()
	r := newJobRouter(st, &fakeScheduler{})

	j := seedJob(t, st, store.JobStatusPending)

	t.Run("existing job returns 200 with steps and task counts", func(t *testing.T) {
		req := newReq(t, http.MethodGet, "/api/v1/jobs/"+j.ID, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body)
		}
		var resp jobDetailResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.ID != j.ID {
			t.Errorf("id = %q, want %q", resp.ID, j.ID)
		}
		// Steps and task counts fields must be present (zero-value is fine for an empty job).
		if resp.Steps == nil {
			t.Error("steps must not be nil")
		}
	})

	t.Run("unknown id returns 404", func(t *testing.T) {
		req := newReq(t, http.MethodGet, "/api/v1/jobs/does-not-exist", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rr.Code)
		}
		// Error body should be problem+json.
		ct := rr.Header().Get("Content-Type")
		if !strings.HasPrefix(ct, "application/problem+json") {
			t.Errorf("Content-Type = %q, want application/problem+json", ct)
		}
	})
}

// ── PATCH /api/v1/jobs/{id} ───────────────────────────────────────────────────

func TestPatchJob(t *testing.T) {
	st := fake.New()
	r := newJobRouter(st, &fakeScheduler{})

	t.Run("update priority persists the new value", func(t *testing.T) {
		j := seedJob(t, st, store.JobStatusPending)
		p := 99
		body := jsonBody(t, patchJobRequest{Priority: &p})
		req := newReq(t, http.MethodPatch, "/api/v1/jobs/"+j.ID, body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body)
		}
		var resp jobResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Priority != 99 {
			t.Errorf("priority = %d, want 99", resp.Priority)
		}
		// Verify the store was updated.
		stored, err := st.GetJob(t.Context(), j.ID)
		if err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		if stored.Priority != 99 {
			t.Errorf("stored priority = %d, want 99", stored.Priority)
		}
	})

	t.Run("pause a running job", func(t *testing.T) {
		j := seedJob(t, st, store.JobStatusRunning)
		body := jsonBody(t, patchJobRequest{Action: "pause"})
		req := newReq(t, http.MethodPatch, "/api/v1/jobs/"+j.ID, body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body)
		}
		var resp jobResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Status != "paused" {
			t.Errorf("status = %q, want paused", resp.Status)
		}
	})

	t.Run("resume a paused job", func(t *testing.T) {
		j := seedJob(t, st, store.JobStatusPaused)
		body := jsonBody(t, patchJobRequest{Action: "resume"})
		req := newReq(t, http.MethodPatch, "/api/v1/jobs/"+j.ID, body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body)
		}
		var resp jobResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Status != "pending" {
			t.Errorf("status = %q, want pending after resume", resp.Status)
		}
	})

	t.Run("patch a failed job returns 409 (terminal)", func(t *testing.T) {
		j := seedJob(t, st, store.JobStatusFailed)
		p := 80
		body := jsonBody(t, patchJobRequest{Priority: &p})
		req := newReq(t, http.MethodPatch, "/api/v1/jobs/"+j.ID, body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusConflict {
			t.Fatalf("expected 409 for failed job, got %d", rr.Code)
		}
	})

	t.Run("patch a canceled job returns 409 (terminal)", func(t *testing.T) {
		j := seedJob(t, st, store.JobStatusCanceled)
		p := 80
		body := jsonBody(t, patchJobRequest{Priority: &p})
		req := newReq(t, http.MethodPatch, "/api/v1/jobs/"+j.ID, body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusConflict {
			t.Fatalf("expected 409 for canceled job, got %d", rr.Code)
		}
	})

	t.Run("pause a completed job returns 409", func(t *testing.T) {
		j := seedJob(t, st, store.JobStatusCompleted)
		body := jsonBody(t, patchJobRequest{Action: "pause"})
		req := newReq(t, http.MethodPatch, "/api/v1/jobs/"+j.ID, body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusConflict {
			t.Fatalf("expected 409, got %d", rr.Code)
		}
	})

	t.Run("resume a non-paused job returns 409", func(t *testing.T) {
		j := seedJob(t, st, store.JobStatusPending)
		body := jsonBody(t, patchJobRequest{Action: "resume"})
		req := newReq(t, http.MethodPatch, "/api/v1/jobs/"+j.ID, body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusConflict {
			t.Fatalf("expected 409 resuming non-paused job, got %d", rr.Code)
		}
	})

	t.Run("unknown action returns 400", func(t *testing.T) {
		j := seedJob(t, st, store.JobStatusPending)
		body := jsonBody(t, patchJobRequest{Action: "explode"})
		req := newReq(t, http.MethodPatch, "/api/v1/jobs/"+j.ID, body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for bad action, got %d", rr.Code)
		}
	})

	t.Run("move to valid queue_id", func(t *testing.T) {
		j := seedJob(t, st, store.JobStatusPending)
		// Create a second queue to move the job into. Ignore ErrConflict —
		// queue-2 may already exist from a prior sub-test on the shared store.
		if _, err := st.CreateQueue(t.Context(), store.Queue{
			ID:     "queue-2",
			FarmID: "farm-1",
			Name:   "queue-two",
		}); err != nil && !errors.Is(err, store.ErrConflict) {
			t.Fatalf("CreateQueue queue-2: %v", err)
		}
		body := jsonBody(t, patchJobRequest{QueueID: "queue-2"})
		req := newReq(t, http.MethodPatch, "/api/v1/jobs/"+j.ID, body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body)
		}
		var resp jobResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.QueueID != "queue-2" {
			t.Errorf("queue_id = %q, want queue-2", resp.QueueID)
		}
	})

	t.Run("move to non-existent queue_id returns 400", func(t *testing.T) {
		j := seedJob(t, st, store.JobStatusPending)
		body := jsonBody(t, patchJobRequest{QueueID: "ghost-queue"})
		req := newReq(t, http.MethodPatch, "/api/v1/jobs/"+j.ID, body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for unknown queue, got %d", rr.Code)
		}
	})

	t.Run("unknown job id returns 404", func(t *testing.T) {
		p := 10
		body := jsonBody(t, patchJobRequest{Priority: &p})
		req := newReq(t, http.MethodPatch, "/api/v1/jobs/no-such-job", body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rr.Code)
		}
	})

	t.Run("invalid JSON body returns 400", func(t *testing.T) {
		j := seedJob(t, st, store.JobStatusPending)
		req := newReq(t, http.MethodPatch, "/api/v1/jobs/"+j.ID, bytes.NewBufferString("{bad json"))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for malformed body, got %d", rr.Code)
		}
	})
}

// ── DELETE /api/v1/jobs/{id} ──────────────────────────────────────────────────

func TestCancelJob(t *testing.T) {
	t.Run("existing job returns 204 and is marked canceled in store", func(t *testing.T) {
		st := fake.New()
		r := newJobRouter(st, &fakeScheduler{})
		j := seedJob(t, st, store.JobStatusRunning)
		req := newReq(t, http.MethodDelete, "/api/v1/jobs/"+j.ID, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("expected 204, got %d — body: %s", rr.Code, rr.Body)
		}
		stored, err := st.GetJob(t.Context(), j.ID)
		if err != nil {
			t.Fatalf("GetJob after cancel: %v", err)
		}
		if stored.Status != store.JobStatusCanceled {
			t.Errorf("status = %q, want canceled", stored.Status)
		}
	})

	t.Run("unknown id returns 404", func(t *testing.T) {
		st := fake.New()
		r := newJobRouter(st, &fakeScheduler{})
		req := newReq(t, http.MethodDelete, "/api/v1/jobs/ghost", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rr.Code)
		}
	})

	t.Run("scheduler error returns 500", func(t *testing.T) {
		st := fake.New()
		sched := &fakeScheduler{cancelErr: errors.New("nats unavailable")}
		r := newJobRouter(st, sched)
		j := seedJob(t, st, store.JobStatusRunning)
		req := newReq(t, http.MethodDelete, "/api/v1/jobs/"+j.ID, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500 when scheduler errors, got %d — body: %s", rr.Code, rr.Body)
		}
	})
}
