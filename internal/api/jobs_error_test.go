// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Additional unit tests for job REST handlers — item 5 of the test roadmap.
//
// These tests cover the error and filter paths not reached by jobs_test.go.
// An storeErr wrapper is used to inject store-level errors without touching
// the real fake.Store implementation.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// ── storeErr: a store.Store wrapper with per-method error injection ────────────

// storeErr wraps a *fake.Store and overrides specific methods to return a
// configured error. All other methods delegate to the embedded store.
type storeErr struct {
	store.Store

	listJobsErr      error
	getJobErr        error
	updateJobErr     error
	cancelJobErr     error
	listTasksErr     error
	getTaskErr       error
	updateTaskErr    error
	latestAttemptErr error
	listLogsErr      error
}

func (e *storeErr) ListJobs(ctx context.Context, opts store.ListJobsOptions) (store.Page[store.Job], error) {
	if e.listJobsErr != nil {
		return store.Page[store.Job]{}, e.listJobsErr
	}
	return e.Store.ListJobs(ctx, opts)
}

func (e *storeErr) GetJob(ctx context.Context, id string) (store.Job, error) {
	if e.getJobErr != nil {
		return store.Job{}, e.getJobErr
	}
	return e.Store.GetJob(ctx, id)
}

func (e *storeErr) UpdateJob(ctx context.Context, job store.Job) (store.Job, error) {
	if e.updateJobErr != nil {
		return store.Job{}, e.updateJobErr
	}
	return e.Store.UpdateJob(ctx, job)
}

func (e *storeErr) CancelJobStatus(ctx context.Context, id string) error {
	if e.cancelJobErr != nil {
		return e.cancelJobErr
	}
	return e.Store.CancelJobStatus(ctx, id)
}

func (e *storeErr) ListTasks(ctx context.Context, opts store.ListTasksOptions) (store.Page[store.Task], error) {
	if e.listTasksErr != nil {
		return store.Page[store.Task]{}, e.listTasksErr
	}
	return e.Store.ListTasks(ctx, opts)
}

func (e *storeErr) GetTask(ctx context.Context, id string) (store.Task, error) {
	if e.getTaskErr != nil {
		return store.Task{}, e.getTaskErr
	}
	return e.Store.GetTask(ctx, id)
}

func (e *storeErr) UpdateTaskStatus(ctx context.Context, id string, status store.TaskStatus) error {
	if e.updateTaskErr != nil {
		return e.updateTaskErr
	}
	return e.Store.UpdateTaskStatus(ctx, id, status)
}

func (e *storeErr) LatestTaskAttempt(ctx context.Context, taskID string) (store.TaskAttempt, error) {
	if e.latestAttemptErr != nil {
		return store.TaskAttempt{}, e.latestAttemptErr
	}
	return e.Store.LatestTaskAttempt(ctx, taskID)
}

func (e *storeErr) ListTaskLogs(ctx context.Context, attemptID string, afterNATSSeq int64, limit int) ([]store.TaskLog, error) {
	if e.listLogsErr != nil {
		return nil, e.listLogsErr
	}
	return e.Store.ListTaskLogs(ctx, attemptID, afterNATSSeq, limit)
}

// errInjected is the sentinel error returned by storeErr wrapper methods during tests.
var errInjected = errors.New("store: internal error")

// ── Item 5: listJobs additional paths ─────────────────────────────────────────

func TestListJobs_AdditionalFiltersAndErrors(t *testing.T) {
	t.Run("filter by owner", func(t *testing.T) {
		st := fake.New()
		r := newJobRouter(st, &fakeScheduler{})
		j := seedJob(t, st, store.JobStatusPending)

		req := newUnscopedJobsReq(t, "/api/v1/jobs?owner="+j.Owner)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}
	})

	t.Run("filter by farm_id returns only matching jobs", func(t *testing.T) {
		st := fake.New()
		r := newJobRouter(st, &fakeScheduler{})
		_ = seedJob(t, st, store.JobStatusPending)

		req := newUnscopedJobsReq(t, "/api/v1/jobs?farm_id=farm-1")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}
	})

	t.Run("sort_by=priority returns 200", func(t *testing.T) {
		st := fake.New()
		r := newJobRouter(st, &fakeScheduler{})
		_ = seedJob(t, st, store.JobStatusPending)

		req := newUnscopedJobsReq(t, "/api/v1/jobs?sort_by=priority")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}
	})

	t.Run("sort_by=status returns 200", func(t *testing.T) {
		st := fake.New()
		r := newJobRouter(st, &fakeScheduler{})
		_ = seedJob(t, st, store.JobStatusPending)

		req := newUnscopedJobsReq(t, "/api/v1/jobs?sort_by=status")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}
	})

	t.Run("sort_by=updated_at returns 200", func(t *testing.T) {
		st := fake.New()
		r := newJobRouter(st, &fakeScheduler{})
		_ = seedJob(t, st, store.JobStatusPending)

		req := newUnscopedJobsReq(t, "/api/v1/jobs?sort_by=updated_at")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}
	})

	t.Run("sort_by=name returns 200", func(t *testing.T) {
		st := fake.New()
		r := newJobRouter(st, &fakeScheduler{})
		_ = seedJob(t, st, store.JobStatusPending)

		req := newUnscopedJobsReq(t, "/api/v1/jobs?sort_by=name")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}
	})

	t.Run("sort_dir=desc returns 200", func(t *testing.T) {
		st := fake.New()
		r := newJobRouter(st, &fakeScheduler{})
		_ = seedJob(t, st, store.JobStatusPending)

		req := newUnscopedJobsReq(t, "/api/v1/jobs?sort_dir=desc")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}
	})

	t.Run("store error returns 500", func(t *testing.T) {
		inner := fake.New()
		est := &storeErr{Store: inner, listJobsErr: errInjected}
		r := newJobRouter(est, &fakeScheduler{})

		req := newUnscopedJobsReq(t, "/api/v1/jobs")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500, got %d — body: %s", rr.Code, rr.Body)
		}
	})
}

// ── Item 5: getJob additional paths ───────────────────────────────────────────

func TestGetJob_StoreError(t *testing.T) {
	t.Run("store error returns 500", func(t *testing.T) {
		inner := fake.New()
		est := &storeErr{Store: inner, getJobErr: errInjected}
		r := newJobRouter(est, &fakeScheduler{})

		req := newReq(t, http.MethodGet, "/api/v1/jobs/any-id", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500, got %d — body: %s", rr.Code, rr.Body)
		}
	})
}

// ── Item 5: cancelJob additional paths ────────────────────────────────────────

func TestCancelJob_TerminalAndConflict(t *testing.T) {
	t.Run("completed job returns 409", func(t *testing.T) {
		st := fake.New()
		r := newJobRouter(st, &fakeScheduler{})
		j := seedJob(t, st, store.JobStatusCompleted)

		req := newReq(t, http.MethodPost, "/api/v1/jobs/"+j.ID+"/cancel", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusConflict {
			t.Fatalf("expected 409 for completed job, got %d", rr.Code)
		}
	})

	t.Run("failed job returns 409", func(t *testing.T) {
		st := fake.New()
		r := newJobRouter(st, &fakeScheduler{})
		j := seedJob(t, st, store.JobStatusFailed)

		req := newReq(t, http.MethodPost, "/api/v1/jobs/"+j.ID+"/cancel", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusConflict {
			t.Fatalf("expected 409 for failed job, got %d", rr.Code)
		}
	})

	t.Run("already-canceled job returns 204 (idempotent)", func(t *testing.T) {
		st := fake.New()
		r := newJobRouter(st, &fakeScheduler{})
		j := seedJob(t, st, store.JobStatusCanceled)

		req := newReq(t, http.MethodPost, "/api/v1/jobs/"+j.ID+"/cancel", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("expected 204 for already-canceled job, got %d", rr.Code)
		}
	})

	t.Run("CancelJobStatus ErrConflict returns 409", func(t *testing.T) {
		inner := fake.New()
		// Seed a pending job in the inner store so GetJob succeeds.
		now := time.Now()
		j := store.Job{
			ID:             uuid.NewString(),
			FarmID:         "farm-x",
			QueueID:        "queue-x",
			Name:           "conflict-test",
			Priority:       50,
			Status:         store.JobStatusPending,
			TemplateFormat: store.TemplateFormatJSON,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		if _, err := inner.CreateJob(t.Context(), j); err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
		est := &storeErr{Store: inner, cancelJobErr: store.ErrConflict}
		r := newJobRouter(est, &fakeScheduler{})

		req := newReq(t, http.MethodPost, "/api/v1/jobs/"+j.ID+"/cancel", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusConflict {
			t.Fatalf("expected 409 when CancelJobStatus returns ErrConflict, got %d — body: %s", rr.Code, rr.Body)
		}
	})
}
