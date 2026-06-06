// SPDX-License-Identifier: AGPL-3.0-only

package api

// Additional unit tests for task REST handlers — item 6 of the test roadmap.
//
// Covers sort_by fields and store error paths not reached by tasks_test.go.
// Uses the storeErr wrapper defined in jobs_error_test.go.

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// ── listJobTasks: sort_by variants and store errors ───────────────────────────

func TestListJobTasks_SortAndErrors(t *testing.T) {
	for _, sortBy := range []string{"status", "updated_at", "name"} {
		t.Run("sort_by="+sortBy+" returns 200", func(t *testing.T) {
			st := fake.New()
			r := newTaskRouter(st)
			j, _ := seedTask(t, st, store.TaskStatusReady)

			req := newReq(t, http.MethodGet, "/api/v1/jobs/"+j.ID+"/tasks?sort_by="+sortBy, nil)
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("sort_by=%s: expected 200, got %d", sortBy, rr.Code)
			}
		})
	}

	t.Run("ListTasks store error returns 500", func(t *testing.T) {
		inner := fake.New()
		j, _ := seedTask(t, inner, store.TaskStatusReady)
		est := &storeErr{Store: inner, listTasksErr: errInjected}
		r := newTaskRouter(est)

		req := newReq(t, http.MethodGet, "/api/v1/jobs/"+j.ID+"/tasks", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500 on ListTasks error, got %d — body: %s", rr.Code, rr.Body)
		}
	})

	t.Run("GetJob internal error returns 500", func(t *testing.T) {
		inner := fake.New()
		est := &storeErr{Store: inner, getJobErr: errInjected}
		r := newTaskRouter(est)

		req := newReq(t, http.MethodGet, "/api/v1/jobs/some-id/tasks", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500 on GetJob error, got %d — body: %s", rr.Code, rr.Body)
		}
	})
}

// ── getTaskLogs: query param paths and store errors ───────────────────────────

func TestGetTaskLogs_ParamsAndErrors(t *testing.T) {
	t.Run("after_nats_seq and limit query params are respected", func(t *testing.T) {
		st := fake.New()
		r := newTaskRouter(st)
		ctx := t.Context()
		_, tk := seedTask(t, st, store.TaskStatusRunning)

		now := time.Now()
		attempt := store.TaskAttempt{
			ID:            uuid.NewString(),
			TaskID:        tk.ID,
			AttemptNumber: 1,
			Status:        store.AttemptStatusRunning,
			StartedAt:     now,
			CreatedAt:     now,
		}
		attempt, err := st.CreateTaskAttempt(ctx, attempt)
		if err != nil {
			t.Fatalf("CreateTaskAttempt: %v", err)
		}
		// Insert 3 log lines with NATSSeq 1, 2, 3.
		for i := range 3 {
			if _, err := st.CreateTaskLog(ctx, store.TaskLog{
				ID:         uuid.NewString(),
				TaskID:     tk.ID,
				AttemptID:  attempt.ID,
				SeqNum:     int64(i + 1),
				NATSSeq:    int64(i + 1),
				Stream:     store.LogStreamStdout,
				Data:       "line",
				At:         now,
				ReceivedAt: now,
			}); err != nil {
				t.Fatalf("CreateTaskLog: %v", err)
			}
		}

		// after_nats_seq=1 means we get lines 2 and 3.
		req := newReq(t, http.MethodGet, "/api/v1/tasks/"+tk.ID+"/logs?after_nats_seq=1&limit=10", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body)
		}
	})

	t.Run("GetTask internal error in logs path returns 500", func(t *testing.T) {
		inner := fake.New()
		est := &storeErr{Store: inner, getTaskErr: errInjected}
		r := newTaskRouter(est)

		req := newReq(t, http.MethodGet, "/api/v1/tasks/any-id/logs", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500 on GetTask error in logs path, got %d", rr.Code)
		}
	})

	t.Run("LatestTaskAttempt internal error returns 500", func(t *testing.T) {
		inner := fake.New()
		_, tk := seedTask(t, inner, store.TaskStatusRunning)
		est := &storeErr{Store: inner, latestAttemptErr: errInjected}
		r := newTaskRouter(est)

		req := newReq(t, http.MethodGet, "/api/v1/tasks/"+tk.ID+"/logs", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500 on LatestTaskAttempt error, got %d", rr.Code)
		}
	})

	t.Run("ListTaskLogs store error returns 500", func(t *testing.T) {
		inner := fake.New()
		ctx := t.Context()
		_, tk := seedTask(t, inner, store.TaskStatusRunning)

		// Create an attempt so LatestTaskAttempt succeeds.
		now := time.Now()
		if _, err := inner.CreateTaskAttempt(ctx, store.TaskAttempt{
			ID:            uuid.NewString(),
			TaskID:        tk.ID,
			AttemptNumber: 1,
			Status:        store.AttemptStatusRunning,
			StartedAt:     now,
			CreatedAt:     now,
		}); err != nil {
			t.Fatalf("CreateTaskAttempt: %v", err)
		}

		est := &storeErr{Store: inner, listLogsErr: errInjected}
		r := newTaskRouter(est)

		req := newReq(t, http.MethodGet, "/api/v1/tasks/"+tk.ID+"/logs", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500 on ListTaskLogs error, got %d", rr.Code)
		}
	})
}

// ── retryTask: store error paths ──────────────────────────────────────────────

func TestRetryTask_StoreErrors(t *testing.T) {
	t.Run("GetTask internal error returns 500", func(t *testing.T) {
		inner := fake.New()
		est := &storeErr{Store: inner, getTaskErr: errInjected}
		r := newTaskRouter(est)

		req := newReq(t, http.MethodPost, "/api/v1/tasks/any-id/retry", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500, got %d", rr.Code)
		}
	})

	t.Run("UpdateTaskStatus store error returns 500", func(t *testing.T) {
		inner := fake.New()
		_, tk := seedTask(t, inner, store.TaskStatusFailed)
		est := &storeErr{Store: inner, updateTaskErr: errInjected}
		r := newTaskRouter(est)

		req := newReq(t, http.MethodPost, "/api/v1/tasks/"+tk.ID+"/retry", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500 on UpdateTaskStatus error, got %d", rr.Code)
		}
	})
}
