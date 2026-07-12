// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Unit tests for the job REST handlers.
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
	"github.com/uberware/sqi/internal/scheduler"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/ws"
)

// ── fakeScheduler ─────────────────────────────────────────────────────────────

// fakeScheduler implements [jobCanceler] for tests so cancelJob and retryJob
// can run without a live NATS/scheduler instance.
type fakeScheduler struct {
	cancelErr    error    // non-nil forces CancelJob to return this error
	retryCount   int      // value returned by RetryJob
	retryErr     error    // non-nil forces RetryJob to return this error
	wokenQueue   []string // queue IDs passed to WakeQueue, in call order
	canceledJobs []string // job IDs passed to CancelJob, in call order
	retriedJobs  []string // job IDs passed to RetryJob, in call order
}

func (f *fakeScheduler) CancelJob(_ context.Context, id string) error {
	f.canceledJobs = append(f.canceledJobs, id)
	return f.cancelErr
}

func (f *fakeScheduler) RetryJob(_ context.Context, id string) (int, error) {
	f.retriedJobs = append(f.retriedJobs, id)
	return f.retryCount, f.retryErr
}

func (f *fakeScheduler) WakeQueue(queueID string) {
	f.wokenQueue = append(f.wokenQueue, queueID)
}

// ── router helpers ────────────────────────────────────────────────────────────

func TestListJobs_SearchParam(t *testing.T) {
	st := fake.New()
	ctx := t.Context()
	if _, err := st.CreateFarm(ctx, store.Farm{ID: "farm-1", Name: "f"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "queue-1", FarmID: "farm-1", Name: "q"}); err != nil {
		t.Fatal(err)
	}
	for _, j := range []store.Job{
		{ID: "a", FarmID: "farm-1", QueueID: "queue-1", Name: "Alpha", Status: store.JobStatusPending, Priority: 50, TemplateFormat: store.TemplateFormatYAML},
		{ID: "b", FarmID: "farm-1", QueueID: "queue-1", Name: "Beta", Status: store.JobStatusPending, Priority: 50, TemplateFormat: store.TemplateFormatYAML},
	} {
		if _, err := st.CreateJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	r := newJobRouter(st, &fakeScheduler{})

	req := newReq(t, http.MethodGet, "/api/v1/jobs?search=alpha", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var resp jobListResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 {
		t.Errorf("total = %d, want 1", resp.Total)
	}
}

// testRetryDefaults is the server-level retry-policy fallback wired into the
// test router — deliberately non-zero so effective_retry assertions can tell
// "server default" apart from the clamp floor.
var testRetryDefaults = scheduler.RetryPolicy{
	MaxAttempts:  3,
	RetryDelay:   30 * time.Second,
	FailureLimit: 0,
}

// newJobRouter wires a jobHandler onto a chi router with the same paths as
// router.go, using the provided store and fakeScheduler.
func newJobRouter(st store.Store, sched jobCanceler) chi.Router {
	sub := openjd.NewSubmitter(st)
	h := newJobHandler(st, sub, sched, ws.NoopNotifier{}, newTestLogger(), testRetryDefaults)
	r := chi.NewRouter()
	r.Post("/api/v1/jobs", h.submitJob)
	r.Get("/api/v1/jobs", h.listJobs)
	r.Get("/api/v1/jobs/{id}", h.getJob)
	r.Patch("/api/v1/jobs/{id}", h.patchJob)
	r.Post("/api/v1/jobs/{id}/cancel", h.cancelJob)
	r.Post("/api/v1/jobs/{id}/retry", h.retryJob)
	r.Delete("/api/v1/jobs/{id}", h.deleteJob)
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

// TestSubmitJob_PersistsRetryPolicy verifies that the max_attempts,
// retry_delay_seconds, and failure_limit query parameters are threaded through
// to the persisted job and echoed in the create response.
func TestSubmitJob_PersistsRetryPolicy(t *testing.T) {
	st := fake.New()
	ctx := t.Context()
	if _, err := st.CreateFarm(ctx, store.Farm{ID: "farm-1", Name: "farm-one"}); err != nil {
		t.Fatalf("create farm: %v", err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "queue-1", FarmID: "farm-1", Name: "render"}); err != nil {
		t.Fatalf("create queue: %v", err)
	}

	r := newJobRouter(st, &fakeScheduler{})

	body := strings.NewReader(minimalOpenJDJSON("RetryPolicyTest"))
	req := newReq(t, http.MethodPost,
		"/api/v1/jobs?farm_id=farm-1&queue_id=queue-1&max_attempts=5&retry_delay_seconds=10&failure_limit=25", body)
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
	if resp.MaxAttempts == nil || *resp.MaxAttempts != 5 {
		t.Errorf("max_attempts = %v, want 5", resp.MaxAttempts)
	}
	if resp.RetryDelaySeconds == nil || *resp.RetryDelaySeconds != 10 {
		t.Errorf("retry_delay_seconds = %v, want 10", resp.RetryDelaySeconds)
	}
	if resp.FailureLimit == nil || *resp.FailureLimit != 25 {
		t.Errorf("failure_limit = %v, want 25", resp.FailureLimit)
	}

	stored, err := st.GetJob(ctx, resp.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if stored.MaxAttempts == nil || *stored.MaxAttempts != 5 {
		t.Errorf("stored max_attempts = %v, want 5", stored.MaxAttempts)
	}
	if stored.RetryDelaySeconds == nil || *stored.RetryDelaySeconds != 10 {
		t.Errorf("stored retry_delay_seconds = %v, want 10", stored.RetryDelaySeconds)
	}
	if stored.FailureLimit == nil || *stored.FailureLimit != 25 {
		t.Errorf("stored failure_limit = %v, want 25", stored.FailureLimit)
	}
}

// TestSubmitJobWakesQueue verifies Fix 2a: a successful submission wakes parked
// lease waiters on the created job's queue.
func TestSubmitJobWakesQueue(t *testing.T) {
	st := fake.New()
	ctx := t.Context()
	if _, err := st.CreateFarm(ctx, store.Farm{ID: "farm-1", Name: "farm-one"}); err != nil {
		t.Fatalf("create farm: %v", err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "queue-1", FarmID: "farm-1", Name: "render"}); err != nil {
		t.Fatalf("create queue: %v", err)
	}

	sched := &fakeScheduler{}
	r := newJobRouter(st, sched)

	body := strings.NewReader(minimalOpenJDJSON("WakeTest"))
	req := newReq(t, http.MethodPost, "/api/v1/jobs?farm_id=farm-1&queue_id=queue-1", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — body: %s", rr.Code, rr.Body)
	}
	if len(sched.wokenQueue) != 1 || sched.wokenQueue[0] != "queue-1" {
		t.Errorf("WakeQueue calls = %v, want [queue-1]", sched.wokenQueue)
	}
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

	t.Run("includes task_counts per item", func(t *testing.T) {
		st := fake.New()
		r := newJobRouter(st, &fakeScheduler{})
		ctx := t.Context()
		job := seedJob(t, st, store.JobStatusRunning)

		now := time.Now()
		step, err := st.CreateStep(ctx, store.Step{
			ID: uuid.NewString(), JobID: job.ID, Name: "Step1",
			Status: store.StepStatusRunning, CreatedAt: now, UpdatedAt: now,
		})
		if err != nil {
			t.Fatalf("CreateStep: %v", err)
		}
		for _, ts := range []store.TaskStatus{store.TaskStatusSucceeded, store.TaskStatusRunning} {
			if _, err := st.CreateTask(ctx, store.Task{
				ID: uuid.NewString(), JobID: job.ID, StepID: step.ID,
				Name: "t", Status: ts, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("CreateTask: %v", err)
			}
		}

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
		var found *jobListItemResponse
		for i := range resp.Items {
			if resp.Items[i].ID == job.ID {
				found = &resp.Items[i]
				break
			}
		}
		if found == nil {
			t.Fatal("seeded job not present in list response")
		}
		if found.TaskCounts.Total != 2 {
			t.Errorf("task_counts.total = %d, want 2", found.TaskCounts.Total)
		}
		if found.TaskCounts.Succeeded != 1 {
			t.Errorf("task_counts.succeeded = %d, want 1", found.TaskCounts.Succeeded)
		}
		if found.TaskCounts.Running != 1 {
			t.Errorf("task_counts.running = %d, want 1", found.TaskCounts.Running)
		}
	})

	t.Run("includes unschedulable count in task_counts per item", func(t *testing.T) {
		st := fake.New()
		r := newJobRouter(st, &fakeScheduler{})
		ctx := t.Context()
		job := seedJob(t, st, store.JobStatusRunning)

		now := time.Now()
		step, err := st.CreateStep(ctx, store.Step{
			ID: uuid.NewString(), JobID: job.ID, Name: "Step1",
			Status: store.StepStatusRunning, CreatedAt: now, UpdatedAt: now,
		})
		if err != nil {
			t.Fatalf("CreateStep: %v", err)
		}
		// Two ready-but-unschedulable tasks and one plain ready task.
		for range 2 {
			tk, err := st.CreateTask(ctx, store.Task{
				ID: uuid.NewString(), JobID: job.ID, StepID: step.ID,
				Name: "t", Status: store.TaskStatusReady, CreatedAt: now, UpdatedAt: now,
			})
			if err != nil {
				t.Fatalf("CreateTask: %v", err)
			}
			if err := st.SetTaskUnschedulableReason(ctx, tk.ID, "no worker matches"); err != nil {
				t.Fatalf("SetTaskUnschedulableReason: %v", err)
			}
		}
		if _, err := st.CreateTask(ctx, store.Task{
			ID: uuid.NewString(), JobID: job.ID, StepID: step.ID,
			Name: "t", Status: store.TaskStatusReady, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}

		req := newReq(t, http.MethodGet, "/api/v1/jobs", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body)
		}
		var resp jobListResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		var found *jobListItemResponse
		for i := range resp.Items {
			if resp.Items[i].ID == job.ID {
				found = &resp.Items[i]
				break
			}
		}
		if found == nil {
			t.Fatal("seeded job not present in list response")
		}
		if found.TaskCounts.Unschedulable != 2 {
			t.Errorf("task_counts.unschedulable = %d, want 2", found.TaskCounts.Unschedulable)
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

	t.Run("filter by farm_id", func(t *testing.T) {
		// Matching farm returns the seeded jobs; a non-matching farm returns none.
		req := newReq(t, http.MethodGet, "/api/v1/jobs?farm_id=farm-1", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		var resp jobListResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Total < 2 {
			t.Errorf("farm-1 total = %d, want >= 2", resp.Total)
		}
		for _, item := range resp.Items {
			if item.FarmID != "farm-1" {
				t.Errorf("got item with farm_id %q in farm-1 filter", item.FarmID)
			}
		}

		req = newReq(t, http.MethodGet, "/api/v1/jobs?farm_id=does-not-exist", nil)
		rr = httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Total != 0 {
			t.Errorf("nonexistent-farm total = %d, want 0 (farm_id must filter)", resp.Total)
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

	t.Run("includes unschedulable count in task_counts", func(t *testing.T) {
		st := fake.New()
		r := newJobRouter(st, &fakeScheduler{})
		ctx := t.Context()
		job := seedJob(t, st, store.JobStatusRunning)

		now := time.Now()
		step, err := st.CreateStep(ctx, store.Step{
			ID: uuid.NewString(), JobID: job.ID, Name: "Step1",
			Status: store.StepStatusRunning, CreatedAt: now, UpdatedAt: now,
		})
		if err != nil {
			t.Fatalf("CreateStep: %v", err)
		}
		const wantUnschedulable = 3
		for range wantUnschedulable {
			tk, err := st.CreateTask(ctx, store.Task{
				ID: uuid.NewString(), JobID: job.ID, StepID: step.ID,
				Name: "t", Status: store.TaskStatusReady, CreatedAt: now, UpdatedAt: now,
			})
			if err != nil {
				t.Fatalf("CreateTask: %v", err)
			}
			if err := st.SetTaskUnschedulableReason(ctx, tk.ID, "no worker matches"); err != nil {
				t.Fatalf("SetTaskUnschedulableReason: %v", err)
			}
		}

		req := newReq(t, http.MethodGet, "/api/v1/jobs/"+job.ID, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body)
		}
		var resp jobDetailResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.TaskCounts.Unschedulable != wantUnschedulable {
			t.Errorf("task_counts.unschedulable = %d, want %d", resp.TaskCounts.Unschedulable, wantUnschedulable)
		}
	})
}

func TestJobDetail_FailureSummary(t *testing.T) {
	t.Run("includes failure_summary when the job has failed tasks with reasons", func(t *testing.T) {
		st := fake.New()
		r := newJobRouter(st, &fakeScheduler{})
		ctx := t.Context()
		job := seedJob(t, st, store.JobStatusRunning)

		now := time.Now()
		step, err := st.CreateStep(ctx, store.Step{
			ID: uuid.NewString(), JobID: job.ID, Name: "Step1",
			Status: store.StepStatusRunning, CreatedAt: now, UpdatedAt: now,
		})
		if err != nil {
			t.Fatalf("CreateStep: %v", err)
		}
		for range 2 {
			tk, err := st.CreateTask(ctx, store.Task{
				ID: uuid.NewString(), JobID: job.ID, StepID: step.ID,
				Name: "t", Status: store.TaskStatusFailed, CreatedAt: now, UpdatedAt: now,
			})
			if err != nil {
				t.Fatalf("CreateTask: %v", err)
			}
			if err := st.SetTaskFailureReason(ctx, tk.ID, "staging"); err != nil {
				t.Fatalf("SetTaskFailureReason: %v", err)
			}
		}

		req := newReq(t, http.MethodGet, "/api/v1/jobs/"+job.ID, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body)
		}

		var resp map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		fs, ok := resp["failure_summary"].(map[string]any)
		if !ok {
			t.Fatalf("failure_summary missing or wrong type: %v", resp["failure_summary"])
		}
		if fs["failed_count"] != float64(2) {
			t.Errorf("failure_summary.failed_count = %v, want 2", fs["failed_count"])
		}
		if fs["dominant_reason"] != "staging" {
			t.Errorf("failure_summary.dominant_reason = %v, want %q", fs["dominant_reason"], "staging")
		}
		if fs["distinct_reasons"] != float64(1) {
			t.Errorf("failure_summary.distinct_reasons = %v, want 1", fs["distinct_reasons"])
		}
	})

	t.Run("omits failure_summary when the job has no failed tasks", func(t *testing.T) {
		st := fake.New()
		r := newJobRouter(st, &fakeScheduler{})
		job := seedJob(t, st, store.JobStatusPending)

		req := newReq(t, http.MethodGet, "/api/v1/jobs/"+job.ID, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body)
		}

		var resp map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if _, ok := resp["failure_summary"]; ok {
			t.Errorf("failure_summary should be omitted, got %v", resp["failure_summary"])
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

	t.Run("update retry policy persists the new values", func(t *testing.T) {
		j := seedJob(t, st, store.JobStatusPending)
		maxAttempts, delay, limit := 7, 30, 40
		body := jsonBody(t, patchJobRequest{
			MaxAttempts:       &maxAttempts,
			RetryDelaySeconds: &delay,
			FailureLimit:      &limit,
		})
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
		if resp.MaxAttempts == nil || *resp.MaxAttempts != 7 {
			t.Errorf("max_attempts = %v, want 7", resp.MaxAttempts)
		}
		if resp.RetryDelaySeconds == nil || *resp.RetryDelaySeconds != 30 {
			t.Errorf("retry_delay_seconds = %v, want 30", resp.RetryDelaySeconds)
		}
		if resp.FailureLimit == nil || *resp.FailureLimit != 40 {
			t.Errorf("failure_limit = %v, want 40", resp.FailureLimit)
		}
		// Verify the store was updated.
		stored, err := st.GetJob(t.Context(), j.ID)
		if err != nil {
			t.Fatalf("GetJob: %v", err)
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

	t.Run("resume an auto-parked job clears park state and re-arms the limit", func(t *testing.T) {
		j := seedJob(t, st, store.JobStatusRunning)
		if err := st.ParkJob(t.Context(), j.ID, "failure limit reached (2)", time.Now().UTC()); err != nil {
			t.Fatalf("ParkJob: %v", err)
		}

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
		if resp.ParkReason != "" {
			t.Errorf("park_reason = %q, want cleared in the response", resp.ParkReason)
		}
		if resp.FailedAttempts != 0 {
			t.Errorf("failed_attempts = %d, want 0 in the response", resp.FailedAttempts)
		}

		// And the store agrees — the reset is persisted, not just echoed.
		stored, err := st.GetJob(t.Context(), j.ID)
		if err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		if stored.ParkReason != "" || stored.FailedAttempts != 0 || stored.Status != store.JobStatusPending {
			t.Errorf("persisted park state not cleared: %+v", stored)
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

// ── POST /api/v1/jobs/{id}/cancel ────────────────────────────────────────────

func TestCancelJob(t *testing.T) {
	t.Run("existing job returns 204 and is marked canceled in store", func(t *testing.T) {
		st := fake.New()
		r := newJobRouter(st, &fakeScheduler{})
		j := seedJob(t, st, store.JobStatusRunning)
		req := newReq(t, http.MethodPost, "/api/v1/jobs/"+j.ID+"/cancel", nil)
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
		req := newReq(t, http.MethodPost, "/api/v1/jobs/ghost/cancel", nil)
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
		req := newReq(t, http.MethodPost, "/api/v1/jobs/"+j.ID+"/cancel", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500 when scheduler errors, got %d — body: %s", rr.Code, rr.Body)
		}
	})
}

// ── DELETE /api/v1/jobs/{id} ──────────────────────────────────────────────────

func TestJobHandler_DeleteJob_RemovesJob(t *testing.T) {
	t.Parallel()
	st := fake.New()
	ctx := context.Background()

	// Seed farm + queue + a terminal job with a known ID.
	if _, err := st.CreateFarm(ctx, store.Farm{ID: "farm-1", Name: "f"}); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "queue-1", FarmID: "farm-1", Name: "q"}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	now := time.Now()
	if _, err := st.CreateJob(ctx, store.Job{
		ID:             "job-1",
		FarmID:         "farm-1",
		QueueID:        "queue-1",
		Name:           "test-job",
		Owner:          "alice",
		Priority:       50,
		Status:         store.JobStatusCompleted,
		TemplateFormat: store.TemplateFormatJSON,
		CreatedAt:      now,
		UpdatedAt:      now,
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	r := newJobRouter(st, &fakeScheduler{})
	req := newReq(t, http.MethodDelete, "/api/v1/jobs/job-1", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204; body=%s", rr.Code, rr.Body)
	}
	if _, err := st.GetJob(ctx, "job-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("job still present after delete: err=%v", err)
	}
}

func TestJobHandler_DeleteJob_NotFound(t *testing.T) {
	t.Parallel()
	st := fake.New()
	r := newJobRouter(st, &fakeScheduler{})
	req := newReq(t, http.MethodDelete, "/api/v1/jobs/missing", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestJobHandler_CancelJob_NewRoute(t *testing.T) {
	t.Parallel()
	st := fake.New()
	ctx := context.Background()

	// Seed farm + queue + a running job with a known ID.
	if _, err := st.CreateFarm(ctx, store.Farm{ID: "farm-1", Name: "f"}); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "queue-1", FarmID: "farm-1", Name: "q"}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	now := time.Now()
	if _, err := st.CreateJob(ctx, store.Job{
		ID:             "job-run",
		FarmID:         "farm-1",
		QueueID:        "queue-1",
		Name:           "running-job",
		Owner:          "alice",
		Priority:       50,
		Status:         store.JobStatusRunning,
		TemplateFormat: store.TemplateFormatJSON,
		CreatedAt:      now,
		UpdatedAt:      now,
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	r := newJobRouter(st, &fakeScheduler{})
	req := newReq(t, http.MethodPost, "/api/v1/jobs/job-run/cancel", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("cancel status = %d, want 204; body=%s", rr.Code, rr.Body)
	}
}

// ── POST /api/v1/jobs/{id}/retry ─────────────────────────────────────────────

// TestRetryJob_OK verifies that retrying a failed job returns 200 with the
// number of tasks revived.
func TestRetryJob_OK(t *testing.T) {
	st := fake.New()
	ctx := t.Context()

	if _, err := st.CreateFarm(ctx, store.Farm{ID: "farm-1", Name: "f"}); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "queue-1", FarmID: "farm-1", Name: "q"}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	now := time.Now()
	if _, err := st.CreateJob(ctx, store.Job{
		ID: "j1", FarmID: "farm-1", QueueID: "queue-1",
		Name: "failed-job", Owner: "alice", Priority: 50,
		Status:         store.JobStatusFailed,
		TemplateFormat: store.TemplateFormatJSON,
		CreatedAt:      now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	sched := &fakeScheduler{retryCount: 1}
	r := newJobRouter(st, sched)
	rr := httptest.NewRecorder()
	req := newReq(t, http.MethodPost, "/api/v1/jobs/j1/retry", nil)
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		JobID   string `json:"job_id"`
		Retried int    `json:"retried"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.JobID != "j1" || resp.Retried != 1 {
		t.Errorf("resp = %+v, want {j1 1}", resp)
	}
}

// TestRetryJob_NotFound verifies that retrying a non-existent job returns 404.
func TestRetryJob_NotFound(t *testing.T) {
	st := fake.New()
	r := newJobRouter(st, &fakeScheduler{})
	rr := httptest.NewRecorder()
	req := newReq(t, http.MethodPost, "/api/v1/jobs/missing/retry", nil)
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

// TestRetryJob_NoEligibleTasks verifies that retrying a job with no
// failed/canceled tasks is idempotent and returns 200 with retried=0.
func TestRetryJob_NoEligibleTasks(t *testing.T) {
	st := fake.New()
	ctx := t.Context()

	if _, err := st.CreateFarm(ctx, store.Farm{ID: "farm-1", Name: "f"}); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "queue-1", FarmID: "farm-1", Name: "q"}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	now := time.Now()
	if _, err := st.CreateJob(ctx, store.Job{
		ID: "j1", FarmID: "farm-1", QueueID: "queue-1",
		Name: "completed-job", Owner: "alice", Priority: 50,
		Status:         store.JobStatusCompleted,
		TemplateFormat: store.TemplateFormatJSON,
		CreatedAt:      now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	r := newJobRouter(st, &fakeScheduler{}) // retryCount defaults to 0
	rr := httptest.NewRecorder()
	req := newReq(t, http.MethodPost, "/api/v1/jobs/j1/retry", nil)
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp struct {
		Retried int `json:"retried"`
	}
	//nolint:errcheck // best-effort decode; zero value is the right fallback on failure
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Retried != 0 {
		t.Errorf("retried = %d, want 0", resp.Retried)
	}
}

// ── parseParamQueryParams unit tests ─────────────────────────────────────────

func TestParseParamQueryParams(t *testing.T) {
	tests := []struct {
		name  string
		query map[string][]string
		want  map[string]string
	}{
		{
			name:  "nil input returns nil",
			query: nil,
			want:  nil,
		},
		{
			name:  "no param. keys returns nil",
			query: map[string][]string{"farm_id": {"f1"}, "queue_id": {"q1"}},
			want:  nil,
		},
		{
			name: "single param.* key extracted",
			query: map[string][]string{
				"farm_id":          {"f1"},
				"param.FrameStart": {"1"},
			},
			want: map[string]string{"FrameStart": "1"},
		},
		{
			name: "multiple param.* keys extracted",
			query: map[string][]string{
				"param.FrameStart": {"1"},
				"param.FrameEnd":   {"100"},
				"owner":            {"alice"},
			},
			want: map[string]string{"FrameStart": "1", "FrameEnd": "100"},
		},
		{
			name: "bare 'param.' prefix with empty name is ignored",
			query: map[string][]string{
				"param.": {"val"},
			},
			want: nil,
		},
		{
			name: "first value wins when multi-valued",
			query: map[string][]string{
				"param.Scene": {"shot_010", "shot_020"},
			},
			want: map[string]string{"Scene": "shot_010"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseParamQueryParams(tc.query)
			if len(got) != len(tc.want) {
				t.Fatalf("len(got) = %d, want %d — got: %v", len(got), len(tc.want), got)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("got[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

// ── POST /api/v1/jobs with param.* query parameters ──────────────────────────

// templateWithIntParam is a minimal OpenJD template with a single required
// INT job parameter so that submit tests can exercise param.* binding.
func templateWithIntParam(jobName string) string {
	return `{
  "specificationVersion": "jobtemplate-2023-09",
  "name": "` + jobName + `",
  "parameterDefinitions": [
    { "name": "Frame", "type": "INT" }
  ],
  "steps": [
    {
      "name": "Render",
      "script": { "actions": { "onRun": { "command": "render" } } }
    }
  ]
}`
}

func TestSubmitJobWithParams(t *testing.T) {
	st := fake.New()
	ctx := t.Context()

	if _, err := st.CreateFarm(ctx, store.Farm{ID: "farm-1", Name: "farm-one"}); err != nil {
		t.Fatalf("create farm: %v", err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "queue-1", FarmID: "farm-1", Name: "render"}); err != nil {
		t.Fatalf("create queue: %v", err)
	}

	r := newJobRouter(st, &fakeScheduler{})

	t.Run("valid param.* value yields 201", func(t *testing.T) {
		body := strings.NewReader(templateWithIntParam("ParamJob1"))
		req := newReq(t, http.MethodPost,
			"/api/v1/jobs?farm_id=farm-1&queue_id=queue-1&param.Frame=42",
			body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusCreated {
			t.Fatalf("expected 201, got %d — body: %s", rr.Code, rr.Body)
		}
	})

	t.Run("invalid param.* value (non-INT) yields 422", func(t *testing.T) {
		body := strings.NewReader(templateWithIntParam("ParamJob2"))
		req := newReq(t, http.MethodPost,
			"/api/v1/jobs?farm_id=farm-1&queue_id=queue-1&param.Frame=not-an-int",
			body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("expected 422 for invalid param value, got %d — body: %s", rr.Code, rr.Body)
		}
	})

	t.Run("missing required param yields 422", func(t *testing.T) {
		body := strings.NewReader(templateWithIntParam("ParamJob3"))
		req := newReq(t, http.MethodPost,
			"/api/v1/jobs?farm_id=farm-1&queue_id=queue-1",
			// Frame is required but no param.Frame is provided.
			body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("expected 422 for missing required param, got %d — body: %s", rr.Code, rr.Body)
		}
	})

	t.Run("unknown param.* key yields 422", func(t *testing.T) {
		body := strings.NewReader(templateWithIntParam("ParamJob4"))
		req := newReq(t, http.MethodPost,
			"/api/v1/jobs?farm_id=farm-1&queue_id=queue-1&param.Frame=1&param.Ghost=x",
			body)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("expected 422 for unknown param, got %d — body: %s", rr.Code, rr.Body)
		}
	})
}

// ── DELETE /api/v1/jobs/{id} — active-job cancel-then-delete ─────────────────

// newJobRouterWithSched is like newJobRouter but returns the fakeScheduler
// pointer so tests can inspect recorded CancelJob calls.
func newJobRouterWithSched(st store.Store) (chi.Router, *fakeScheduler) {
	sched := &fakeScheduler{}
	return newJobRouter(st, sched), sched
}

// TestJobHandler_DeleteJob_ActiveJobCancelsAndDeletes verifies that deleting a
// non-terminal job calls CancelJob on the scheduler before hard-deleting the row.
func TestJobHandler_DeleteJob_ActiveJobCancelsAndDeletes(t *testing.T) {
	t.Parallel()
	st := fake.New()
	ctx := t.Context()

	if _, err := st.CreateFarm(ctx, store.Farm{ID: "farm-1", Name: "f"}); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "queue-1", FarmID: "farm-1", Name: "q"}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	now := time.Now()
	if _, err := st.CreateJob(ctx, store.Job{
		ID:             "job-active",
		FarmID:         "farm-1",
		QueueID:        "queue-1",
		Name:           "active-job",
		Status:         store.JobStatusRunning,
		TemplateFormat: store.TemplateFormatJSON,
		CreatedAt:      now,
		UpdatedAt:      now,
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	r, sched := newJobRouterWithSched(st)
	req := newReq(t, http.MethodDelete, "/api/v1/jobs/job-active", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204; body=%s", rr.Code, rr.Body)
	}
	if _, err := st.GetJob(ctx, "job-active"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("job still present after active-job delete: err=%v", err)
	}
	if len(sched.canceledJobs) != 1 || sched.canceledJobs[0] != "job-active" {
		t.Errorf("CancelJob calls = %v, want [job-active]", sched.canceledJobs)
	}
}

// TestJobHandler_DeleteJob_CancelFailureReturns500 verifies that when the
// scheduler's CancelJob returns an error the handler responds 500 and does not
// delete the job.
func TestJobHandler_DeleteJob_CancelFailureReturns500(t *testing.T) {
	t.Parallel()
	st := fake.New()
	ctx := t.Context()

	if _, err := st.CreateFarm(ctx, store.Farm{ID: "farm-1", Name: "f"}); err != nil {
		t.Fatalf("CreateFarm: %v", err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "queue-1", FarmID: "farm-1", Name: "q"}); err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}
	now := time.Now()
	if _, err := st.CreateJob(ctx, store.Job{
		ID:             "job-run-fail",
		FarmID:         "farm-1",
		QueueID:        "queue-1",
		Name:           "running-job",
		Status:         store.JobStatusRunning,
		TemplateFormat: store.TemplateFormatJSON,
		CreatedAt:      now,
		UpdatedAt:      now,
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	sched := &fakeScheduler{cancelErr: errors.New("scheduler unavailable")}
	r := newJobRouter(st, sched)
	req := newReq(t, http.MethodDelete, "/api/v1/jobs/job-run-fail", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("DELETE status = %d, want 500 when CancelJob fails; body=%s", rr.Code, rr.Body)
	}
	// Job must not have been deleted.
	if _, err := st.GetJob(ctx, "job-run-fail"); err != nil {
		t.Fatalf("job should still exist after cancel failure: err=%v", err)
	}
}

// TestSubmitJob_RetryOverrideQueryValidation asserts the submit endpoint
// rejects malformed or out-of-range retry-override query parameters with 400
// instead of silently submitting with different behavior than intended — and
// still accepts the legitimate boundary value failure_limit=0 ("disable an
// inherited limit").
func TestSubmitJob_RetryOverrideQueryValidation(t *testing.T) {
	st := fake.New()
	ctx := t.Context()
	if _, err := st.CreateFarm(ctx, store.Farm{ID: "farm-1", Name: "farm-one"}); err != nil {
		t.Fatalf("create farm: %v", err)
	}
	if _, err := st.CreateQueue(ctx, store.Queue{ID: "queue-1", FarmID: "farm-1", Name: "render"}); err != nil {
		t.Fatalf("create queue: %v", err)
	}
	r := newJobRouter(st, &fakeScheduler{})

	tests := []struct {
		name     string
		query    string
		wantCode int
		wantBody string
	}{
		{"non-integer max_attempts", "max_attempts=3O", http.StatusBadRequest, "max_attempts must be an integer"},
		{"zero max_attempts", "max_attempts=0", http.StatusBadRequest, "max_attempts must be >= 1"},
		{"negative retry_delay_seconds", "retry_delay_seconds=-1", http.StatusBadRequest, "retry_delay_seconds must be >= 0"},
		{"negative failure_limit", "failure_limit=-5", http.StatusBadRequest, "failure_limit must be >= 0"},
		{"failure_limit zero is a valid override", "failure_limit=0", http.StatusCreated, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := strings.NewReader(minimalOpenJDJSON("OverrideValidation-" + tt.name))
			req := newReq(t, http.MethodPost, "/api/v1/jobs?farm_id=farm-1&queue_id=queue-1&"+tt.query, body)
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != tt.wantCode {
				t.Fatalf("code = %d, want %d — body: %s", rr.Code, tt.wantCode, rr.Body)
			}
			if tt.wantBody != "" && !strings.Contains(rr.Body.String(), tt.wantBody) {
				t.Errorf("body %q does not contain %q", rr.Body.String(), tt.wantBody)
			}
		})
	}
}

// TestPatchJob_RejectsInvalidRetryOverrides asserts PATCH validates the retry
// override fields at the boundary instead of storing nonsense that would be
// echoed to every client and silently clamped at schedule time.
func TestPatchJob_RejectsInvalidRetryOverrides(t *testing.T) {
	st := fake.New()
	r := newJobRouter(st, &fakeScheduler{})
	j := seedJob(t, st, store.JobStatusPending)

	zero, neg := 0, -1
	tests := []struct {
		name string
		req  patchJobRequest
		want string
	}{
		{"zero max_attempts", patchJobRequest{MaxAttempts: &zero}, "max_attempts must be >= 1"},
		{"negative retry_delay_seconds", patchJobRequest{RetryDelaySeconds: &neg}, "retry_delay_seconds must be >= 0"},
		{"negative failure_limit", patchJobRequest{FailureLimit: &neg}, "failure_limit must be >= 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := newReq(t, http.MethodPatch, "/api/v1/jobs/"+j.ID, jsonBody(t, tt.req))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400 — body: %s", rr.Code, rr.Body)
			}
			if !strings.Contains(rr.Body.String(), tt.want) {
				t.Errorf("body %q does not contain %q", rr.Body.String(), tt.want)
			}
		})
	}
}

// TestGetJob_EffectiveRetry asserts the job detail response reports the
// RESOLVED retry policy after job -> queue -> farm -> server-default
// inheritance, alongside the configured nullable overrides.
func TestGetJob_EffectiveRetry(t *testing.T) {
	st := fake.New()
	r := newJobRouter(st, &fakeScheduler{})
	ctx := t.Context()

	t.Run("all inherited from server defaults", func(t *testing.T) {
		j := seedJob(t, st, store.JobStatusPending)
		req := newReq(t, http.MethodGet, "/api/v1/jobs/"+j.ID, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("code = %d — body: %s", rr.Code, rr.Body)
		}
		var resp jobDetailResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.EffectiveRetry == nil {
			t.Fatal("effective_retry missing from job detail")
		}
		want := effectiveRetryResponse{MaxAttempts: 3, RetryDelaySeconds: 30, FailureLimit: 0}
		if *resp.EffectiveRetry != want {
			t.Errorf("effective_retry = %+v, want server defaults %+v", *resp.EffectiveRetry, want)
		}
	})

	t.Run("job and farm overrides win over defaults", func(t *testing.T) {
		j := seedJob(t, st, store.JobStatusPending)

		// Farm supplies the delay; the job itself supplies max attempts.
		farm, err := st.GetFarm(ctx, j.FarmID)
		if err != nil {
			t.Fatalf("GetFarm: %v", err)
		}
		delay := 60
		farm.RetryDelaySeconds = &delay
		if _, err := st.UpdateFarm(ctx, farm); err != nil {
			t.Fatalf("UpdateFarm: %v", err)
		}
		maxAttempts := 5
		j.MaxAttempts = &maxAttempts
		if _, err := st.UpdateJob(ctx, j); err != nil {
			t.Fatalf("UpdateJob: %v", err)
		}

		req := newReq(t, http.MethodGet, "/api/v1/jobs/"+j.ID, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("code = %d — body: %s", rr.Code, rr.Body)
		}
		var resp jobDetailResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.EffectiveRetry == nil {
			t.Fatal("effective_retry missing from job detail")
		}
		want := effectiveRetryResponse{MaxAttempts: 5, RetryDelaySeconds: 60, FailureLimit: 0}
		if *resp.EffectiveRetry != want {
			t.Errorf("effective_retry = %+v, want %+v", *resp.EffectiveRetry, want)
		}
	})
}
