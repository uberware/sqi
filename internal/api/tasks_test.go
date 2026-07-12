// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Unit tests for the task REST handlers.
//
// Route coverage:
//   GET  /api/v1/jobs/{id}/tasks    — listJobTasks
//   GET  /api/v1/tasks/{id}         — getTask
//   GET  /api/v1/tasks/{id}/logs    — getTaskLogs (non-streaming path)
//   POST /api/v1/tasks/{id}/retry   — retryTask
//   POST /api/v1/tasks/{id}/cancel  — cancelTask

import (
	"context"
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

// fakeTaskCanceler implements [taskCanceler] for tests.
type fakeTaskCanceler struct {
	cancelErr     error            // non-nil forces CancelTask to return this error
	canceledTasks []string         // task IDs passed to CancelTask, in call order
	retryErr      error            // non-nil forces RetryTask to return this error
	retriedTasks  []string         // task IDs passed to RetryTask, in call order
	retryStore    store.Store      // if set, RetryTask updates the task status in the store
	retryStatus   store.TaskStatus // status to set via retryStore (defaults to TaskStatusReady)
}

func (f *fakeTaskCanceler) CancelTask(_ context.Context, id string) error {
	f.canceledTasks = append(f.canceledTasks, id)
	return f.cancelErr
}

func (f *fakeTaskCanceler) RetryTask(ctx context.Context, id string) error {
	f.retriedTasks = append(f.retriedTasks, id)
	if f.retryErr != nil {
		return f.retryErr
	}
	if f.retryStore != nil {
		status := f.retryStatus
		if status == "" {
			status = store.TaskStatusReady
		}
		return f.retryStore.UpdateTaskStatus(ctx, id, status)
	}
	return nil
}

func newTaskRouter(st store.Store) chi.Router {
	return newTaskRouterCanceler(st, &fakeTaskCanceler{retryStore: st})
}

func newTaskRouterCanceler(st store.Store, sched taskCanceler) chi.Router {
	h := newTaskHandler(st, sched, newTestLogger())
	r := chi.NewRouter()
	r.Get("/api/v1/jobs/{id}/tasks", h.listJobTasks)
	r.Get("/api/v1/tasks/{id}", h.getTask)
	r.Get("/api/v1/tasks/{id}/logs", h.getTaskLogs)
	r.Get("/api/v1/tasks/{id}/attempts", h.getTaskAttempts)
	r.Post("/api/v1/tasks/{id}/retry", h.retryTask)
	r.Post("/api/v1/tasks/{id}/cancel", h.cancelTask)
	return r
}

// ── seed helpers ──────────────────────────────────────────────────────────────

// seedTask inserts a minimal job + task into st, returning both.
func seedTask(t *testing.T, st *fake.Store, taskStatus store.TaskStatus) (store.Job, store.Task) {
	t.Helper()
	ctx := t.Context()

	now := time.Now()
	job := store.Job{
		ID:             uuid.NewString(),
		FarmID:         "farm-1",
		QueueID:        "queue-1",
		Name:           "job-for-task-test",
		Priority:       50,
		Status:         store.JobStatusRunning,
		TemplateFormat: store.TemplateFormatJSON,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	j, err := st.CreateJob(ctx, job)
	if err != nil {
		t.Fatalf("seedTask: CreateJob: %v", err)
	}

	step := store.Step{
		ID:        uuid.NewString(),
		JobID:     j.ID,
		Name:      "Step1",
		StepOrder: 0,
		Status:    store.StepStatusRunning,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s, err := st.CreateStep(ctx, step)
	if err != nil {
		t.Fatalf("seedTask: CreateStep: %v", err)
	}

	task := store.Task{
		ID:        uuid.NewString(),
		JobID:     j.ID,
		StepID:    s.ID,
		Name:      "task-0",
		Status:    taskStatus,
		CreatedAt: now,
		UpdatedAt: now,
	}
	tk, err := st.CreateTask(ctx, task)
	if err != nil {
		t.Fatalf("seedTask: CreateTask: %v", err)
	}

	return j, tk
}

// ── GET /api/v1/jobs/{id}/tasks ───────────────────────────────────────────────

func TestListJobTasks(t *testing.T) {
	t.Run("returns tasks for existing job", func(t *testing.T) {
		st := fake.New()
		r := newTaskRouter(st)
		j, tk := seedTask(t, st, store.TaskStatusReady)

		req := newReq(t, http.MethodGet, "/api/v1/jobs/"+j.ID+"/tasks", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body)
		}
		var resp taskListResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Total != 1 {
			t.Errorf("total = %d, want 1", resp.Total)
		}
		if len(resp.Items) != 1 {
			t.Fatalf("items len = %d, want 1", len(resp.Items))
		}
		if resp.Items[0].ID != tk.ID {
			t.Errorf("task id = %q, want %q", resp.Items[0].ID, tk.ID)
		}
	})

	t.Run("unknown job id returns 404", func(t *testing.T) {
		st := fake.New()
		r := newTaskRouter(st)
		req := newReq(t, http.MethodGet, "/api/v1/jobs/no-such-job/tasks", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rr.Code)
		}
	})

	t.Run("filter by status", func(t *testing.T) {
		st := fake.New()
		r := newTaskRouter(st)
		j, _ := seedTask(t, st, store.TaskStatusFailed)

		// Only failed tasks should appear.
		req := newReq(t, http.MethodGet, "/api/v1/jobs/"+j.ID+"/tasks?status=failed", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}
		var resp taskListResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, item := range resp.Items {
			if item.Status != "failed" {
				t.Errorf("unexpected status %q in failed filter", item.Status)
			}
		}
	})

	t.Run("pagination parameters respected", func(t *testing.T) {
		st := fake.New()
		r := newTaskRouter(st)
		j, _ := seedTask(t, st, store.TaskStatusReady)

		req := newReq(t, http.MethodGet, "/api/v1/jobs/"+j.ID+"/tasks?limit=1&offset=0", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}
		var resp taskListResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Limit != 1 {
			t.Errorf("limit = %d, want 1", resp.Limit)
		}
	})
}

// ── GET /api/v1/tasks/{id} ────────────────────────────────────────────────────

func TestGetTask(t *testing.T) {
	t.Run("returns task for existing id", func(t *testing.T) {
		st := fake.New()
		r := newTaskRouter(st)
		_, tk := seedTask(t, st, store.TaskStatusRunning)

		req := newReq(t, http.MethodGet, "/api/v1/tasks/"+tk.ID, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body)
		}
		var resp taskResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.ID != tk.ID {
			t.Errorf("id = %q, want %q", resp.ID, tk.ID)
		}
		if resp.Status != "running" {
			t.Errorf("status = %q, want running", resp.Status)
		}
	})

	t.Run("unknown task id returns 404", func(t *testing.T) {
		st := fake.New()
		r := newTaskRouter(st)
		req := newReq(t, http.MethodGet, "/api/v1/tasks/ghost", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rr.Code)
		}
	})

	t.Run("includes unschedulable_reason when set", func(t *testing.T) {
		st := fake.New()
		r := newTaskRouter(st)
		_, tk := seedTask(t, st, store.TaskStatusReady)

		const reason = "no online worker satisfies required capabilities"
		if err := st.SetTaskUnschedulableReason(t.Context(), tk.ID, reason); err != nil {
			t.Fatalf("SetTaskUnschedulableReason: %v", err)
		}

		req := newReq(t, http.MethodGet, "/api/v1/tasks/"+tk.ID, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body)
		}
		var resp taskResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.UnschedulableReason != reason {
			t.Errorf("unschedulable_reason = %q, want %q", resp.UnschedulableReason, reason)
		}
	})

	t.Run("includes failed_attempts and retry_after when set", func(t *testing.T) {
		st := fake.New()
		r := newTaskRouter(st)
		_, tk := seedTask(t, st, store.TaskStatusReady)

		now := time.Now()
		att, err := st.CreateTaskAttempt(t.Context(), store.TaskAttempt{
			ID: uuid.NewString(), TaskID: tk.ID, WorkerID: "w1",
			AttemptNumber: 1, Status: store.AttemptStatusRunning, StartedAt: now,
		})
		if err != nil {
			t.Fatalf("CreateTaskAttempt: %v", err)
		}
		if _, _, err := st.RecordTaskFailure(t.Context(), att.ID, tk.ID, nil, "", "", now); err != nil {
			t.Fatalf("RecordTaskFailure: %v", err)
		}
		retryAfter := now.Add(30 * time.Second)
		if err := st.RequeueTaskForRetry(t.Context(), tk.ID, retryAfter, now); err != nil {
			t.Fatalf("RequeueTaskForRetry: %v", err)
		}

		req := newReq(t, http.MethodGet, "/api/v1/tasks/"+tk.ID, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body)
		}
		var resp taskResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.FailedAttempts != 1 {
			t.Errorf("failed_attempts = %d, want 1", resp.FailedAttempts)
		}
		if resp.RetryAfter == nil || !resp.RetryAfter.Equal(retryAfter) {
			t.Errorf("retry_after = %v, want %v", resp.RetryAfter, retryAfter)
		}
	})

	t.Run("includes failure_reason when set", func(t *testing.T) {
		st := fake.New()
		r := newTaskRouter(st)
		_, tk := seedTask(t, st, store.TaskStatusFailed)

		const reason = "staging"
		if err := st.SetTaskFailureReason(t.Context(), tk.ID, reason); err != nil {
			t.Fatalf("SetTaskFailureReason: %v", err)
		}

		req := newReq(t, http.MethodGet, "/api/v1/tasks/"+tk.ID, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body)
		}
		var resp taskResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.FailureReason != reason {
			t.Errorf("failure_reason = %q, want %q", resp.FailureReason, reason)
		}
	})
}

// ── GET /api/v1/tasks/{id}/logs ───────────────────────────────────────────────

func TestGetTaskLogs(t *testing.T) {
	t.Run("task with no attempt returns empty log list", func(t *testing.T) {
		st := fake.New()
		r := newTaskRouter(st)
		_, tk := seedTask(t, st, store.TaskStatusReady)

		req := newReq(t, http.MethodGet, "/api/v1/tasks/"+tk.ID+"/logs", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body)
		}
		var resp taskLogsResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Items) != 0 {
			t.Errorf("expected empty log list, got %d items", len(resp.Items))
		}
	})

	t.Run("unknown task id returns 404", func(t *testing.T) {
		st := fake.New()
		r := newTaskRouter(st)
		req := newReq(t, http.MethodGet, "/api/v1/tasks/ghost/logs", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rr.Code)
		}
	})

	t.Run("returns log chunks from latest attempt", func(t *testing.T) {
		st := fake.New()
		r := newTaskRouter(st)
		ctx := t.Context()
		_, tk := seedTask(t, st, store.TaskStatusRunning)

		// Insert a task attempt and two log chunks.
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

		for i, msg := range []string{"line one", "line two"} {
			_, err := st.CreateTaskLog(ctx, store.TaskLog{
				ID:         uuid.NewString(),
				TaskID:     tk.ID,
				AttemptID:  attempt.ID,
				SeqNum:     int64(i + 1),
				NATSSeq:    int64(i + 1),
				Stream:     store.LogStreamStdout,
				Data:       msg,
				At:         now,
				ReceivedAt: now,
			})
			if err != nil {
				t.Fatalf("CreateTaskLog: %v", err)
			}
		}

		req := newReq(t, http.MethodGet, "/api/v1/tasks/"+tk.ID+"/logs", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body)
		}
		var resp taskLogsResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Items) != 2 {
			t.Errorf("expected 2 log items, got %d", len(resp.Items))
		}
		if resp.Items[0].Data != "line one" {
			t.Errorf("items[0].data = %q, want %q", resp.Items[0].Data, "line one")
		}
	})

	t.Run("after_nats_seq returns the cursor to advance to", func(t *testing.T) {
		st := fake.New()
		r := newTaskRouter(st)
		ctx := t.Context()
		_, tk := seedTask(t, st, store.TaskStatusRunning)

		now := time.Now()
		attempt, err := st.CreateTaskAttempt(ctx, store.TaskAttempt{
			ID:            uuid.NewString(),
			TaskID:        tk.ID,
			AttemptNumber: 1,
			Status:        store.AttemptStatusRunning,
			StartedAt:     now,
			CreatedAt:     now,
		})
		if err != nil {
			t.Fatalf("CreateTaskAttempt: %v", err)
		}
		for _, seq := range []int64{5, 9, 12} {
			if _, err := st.CreateTaskLog(ctx, store.TaskLog{
				ID:         uuid.NewString(),
				TaskID:     tk.ID,
				AttemptID:  attempt.ID,
				SeqNum:     seq,
				NATSSeq:    seq,
				Stream:     store.LogStreamStdout,
				Data:       "x",
				At:         now,
				ReceivedAt: now,
			}); err != nil {
				t.Fatalf("CreateTaskLog: %v", err)
			}
		}

		// A page with chunks reports the highest nats_seq it returned, so the
		// next poll (after_nats_seq=12) fetches only newer chunks. Echoing the
		// request (the old bug) would leave the cursor stuck at 0.
		req := newReq(t, http.MethodGet, "/api/v1/tasks/"+tk.ID+"/logs", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		var resp taskLogsResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.AfterNATSSeq != 12 {
			t.Errorf("after_nats_seq = %d, want 12 (highest returned nats_seq)", resp.AfterNATSSeq)
		}

		// An empty page leaves the cursor where the caller asked, so a poller
		// holds its position instead of rewinding.
		req = newReq(t, http.MethodGet, "/api/v1/tasks/"+tk.ID+"/logs?after_nats_seq=12", nil)
		rr = httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Items) != 0 {
			t.Fatalf("expected empty page after cursor, got %d items", len(resp.Items))
		}
		if resp.AfterNATSSeq != 12 {
			t.Errorf("empty-page after_nats_seq = %d, want 12 (echo request)", resp.AfterNATSSeq)
		}
	})
}

// ── GET /api/v1/tasks/{id}/attempts ───────────────────────────────────────────

func TestGetTaskAttempts(t *testing.T) {
	t.Run("returns attempts ordered by attempt number", func(t *testing.T) {
		st := fake.New()
		r := newTaskRouter(st)
		ctx := t.Context()
		_, tk := seedTask(t, st, store.TaskStatusRunning)

		// Attempt 1: failed with a message and exit code.
		startedAt1 := time.Now().Add(-time.Hour)
		endedAt1 := startedAt1.Add(time.Minute)
		attempt1 := store.TaskAttempt{
			ID:            uuid.NewString(),
			TaskID:        tk.ID,
			WorkerID:      "worker-1",
			AttemptNumber: 1,
			Status:        store.AttemptStatusRunning,
			StartedAt:     startedAt1,
			CreatedAt:     startedAt1,
		}
		attempt1, err := st.CreateTaskAttempt(ctx, attempt1)
		if err != nil {
			t.Fatalf("CreateTaskAttempt: %v", err)
		}
		exitCode := 1
		attempt1.Status = store.AttemptStatusFailed
		attempt1.ExitCode = &exitCode
		attempt1.Message = "worker not configured for staging"
		attempt1.EndedAt = &endedAt1
		if _, err := st.UpdateTaskAttempt(ctx, attempt1); err != nil {
			t.Fatalf("UpdateTaskAttempt: %v", err)
		}

		// Attempt 2: still running, no exit code or end time.
		startedAt2 := time.Now()
		attempt2 := store.TaskAttempt{
			ID:            uuid.NewString(),
			TaskID:        tk.ID,
			WorkerID:      "worker-2",
			AttemptNumber: 2,
			Status:        store.AttemptStatusRunning,
			StartedAt:     startedAt2,
			CreatedAt:     startedAt2,
		}
		if _, err := st.CreateTaskAttempt(ctx, attempt2); err != nil {
			t.Fatalf("CreateTaskAttempt: %v", err)
		}

		req := newReq(t, http.MethodGet, "/api/v1/tasks/"+tk.ID+"/attempts", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body)
		}
		var resp struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Items) != 2 {
			t.Fatalf("want 2 attempts, got %d", len(resp.Items))
		}
		if resp.Items[0]["attempt_number"] != float64(1) ||
			resp.Items[0]["message"] != "worker not configured for staging" ||
			resp.Items[0]["exit_code"] != float64(1) ||
			resp.Items[0]["status"] != "failed" {
			t.Fatalf("attempt 1 wrong: %v", resp.Items[0])
		}
		if resp.Items[1]["attempt_number"] != float64(2) ||
			resp.Items[1]["status"] != "running" {
			t.Fatalf("attempt 2 wrong: %v", resp.Items[1])
		}
		if _, ok := resp.Items[1]["exit_code"]; ok {
			t.Errorf("attempt 2 exit_code should be omitted while running, got %v", resp.Items[1]["exit_code"])
		}
		if _, ok := resp.Items[1]["ended_at"]; ok {
			t.Errorf("attempt 2 ended_at should be omitted while running, got %v", resp.Items[1]["ended_at"])
		}
		if _, ok := resp.Items[0]["session_id"]; ok {
			t.Errorf("session_id should be omitted from the wire, got %v", resp.Items[0]["session_id"])
		}
		if _, ok := resp.Items[0]["created_at"]; ok {
			t.Errorf("created_at should be omitted from the wire, got %v", resp.Items[0]["created_at"])
		}
	})

	t.Run("unknown task id returns 404", func(t *testing.T) {
		st := fake.New()
		r := newTaskRouter(st)
		req := newReq(t, http.MethodGet, "/api/v1/tasks/ghost/attempts", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rr.Code)
		}
	})

	t.Run("task with no attempts returns empty items", func(t *testing.T) {
		st := fake.New()
		r := newTaskRouter(st)
		_, tk := seedTask(t, st, store.TaskStatusReady)

		req := newReq(t, http.MethodGet, "/api/v1/tasks/"+tk.ID+"/attempts", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d — body: %s", rr.Code, rr.Body)
		}
		var resp taskAttemptsResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Items == nil || len(resp.Items) != 0 {
			t.Fatalf("expected empty items, got %v", resp.Items)
		}
	})
}

// ── POST /api/v1/tasks/{id}/retry ─────────────────────────────────────────────

func TestRetryTask(t *testing.T) {
	t.Run("failed task is revived via scheduler", func(t *testing.T) {
		st := fake.New()
		r := newTaskRouter(st)
		_, tk := seedTask(t, st, store.TaskStatusFailed)

		req := newReq(t, http.MethodPost, "/api/v1/tasks/"+tk.ID+"/retry", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusAccepted {
			t.Fatalf("expected 202, got %d — body: %s", rr.Code, rr.Body)
		}
		var resp retryResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Status != "ready" && resp.Status != "pending" {
			t.Errorf("status = %q, want ready or pending", resp.Status)
		}
		if resp.TaskID != tk.ID {
			t.Errorf("task_id = %q, want %q", resp.TaskID, tk.ID)
		}
	})

	t.Run("canceled task is reset to ready", func(t *testing.T) {
		st := fake.New()
		r := newTaskRouter(st)
		_, tk := seedTask(t, st, store.TaskStatusCanceled)

		req := newReq(t, http.MethodPost, "/api/v1/tasks/"+tk.ID+"/retry", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusAccepted {
			t.Fatalf("expected 202, got %d — body: %s", rr.Code, rr.Body)
		}
	})

	t.Run("running task returns 409", func(t *testing.T) {
		st := fake.New()
		r := newTaskRouter(st)
		_, tk := seedTask(t, st, store.TaskStatusRunning)

		req := newReq(t, http.MethodPost, "/api/v1/tasks/"+tk.ID+"/retry", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusConflict {
			t.Fatalf("expected 409 for running task, got %d", rr.Code)
		}
	})

	t.Run("succeeded task returns 409", func(t *testing.T) {
		st := fake.New()
		r := newTaskRouter(st)
		_, tk := seedTask(t, st, store.TaskStatusSucceeded)

		req := newReq(t, http.MethodPost, "/api/v1/tasks/"+tk.ID+"/retry", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusConflict {
			t.Fatalf("expected 409 for succeeded task, got %d", rr.Code)
		}
	})

	t.Run("retry with unsatisfied deps returns pending status", func(t *testing.T) {
		st := fake.New()
		sched := &fakeTaskCanceler{retryStore: st, retryStatus: store.TaskStatusPending}
		r := newTaskRouterCanceler(st, sched)
		_, tk := seedTask(t, st, store.TaskStatusFailed)

		req := newReq(t, http.MethodPost, "/api/v1/tasks/"+tk.ID+"/retry", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusAccepted {
			t.Fatalf("expected 202, got %d — body: %s", rr.Code, rr.Body)
		}
		var resp retryResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Status != "pending" {
			t.Errorf("status = %q, want pending", resp.Status)
		}
		if resp.TaskID != tk.ID {
			t.Errorf("task_id = %q, want %q", resp.TaskID, tk.ID)
		}
	})

	t.Run("unknown task id returns 404", func(t *testing.T) {
		st := fake.New()
		r := newTaskRouter(st)
		req := newReq(t, http.MethodPost, "/api/v1/tasks/ghost/retry", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rr.Code)
		}
	})
}

// ── POST /api/v1/tasks/{id}/cancel ────────────────────────────────────────────

func TestCancelTask(t *testing.T) {
	cancelable := []store.TaskStatus{
		store.TaskStatusPending,
		store.TaskStatusReady,
		store.TaskStatusAssigned,
		store.TaskStatusRunning,
	}
	for _, status := range cancelable {
		t.Run("non-terminal "+string(status)+" → 202", func(t *testing.T) {
			st := fake.New()
			sched := &fakeTaskCanceler{}
			r := newTaskRouterCanceler(st, sched)
			_, tk := seedTask(t, st, status)

			req := newReq(t, http.MethodPost, "/api/v1/tasks/"+tk.ID+"/cancel", nil)
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != http.StatusAccepted {
				t.Fatalf("expected 202, got %d — body: %s", rr.Code, rr.Body)
			}
			var resp cancelResponse
			if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Status != "canceled" {
				t.Errorf("status = %q, want canceled", resp.Status)
			}
			if resp.TaskID != tk.ID {
				t.Errorf("task_id = %q, want %q", resp.TaskID, tk.ID)
			}
			if len(sched.canceledTasks) != 1 || sched.canceledTasks[0] != tk.ID {
				t.Errorf("CancelTask calls = %v, want [%s]", sched.canceledTasks, tk.ID)
			}
		})
	}

	terminal := []store.TaskStatus{
		store.TaskStatusSucceeded,
		store.TaskStatusFailed,
		store.TaskStatusCanceled,
	}
	for _, status := range terminal {
		t.Run("terminal "+string(status)+" → 409", func(t *testing.T) {
			st := fake.New()
			sched := &fakeTaskCanceler{}
			r := newTaskRouterCanceler(st, sched)
			_, tk := seedTask(t, st, status)

			req := newReq(t, http.MethodPost, "/api/v1/tasks/"+tk.ID+"/cancel", nil)
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != http.StatusConflict {
				t.Fatalf("expected 409 for %s, got %d", status, rr.Code)
			}
			if len(sched.canceledTasks) != 0 {
				t.Errorf("CancelTask should not be called for terminal task, got %v", sched.canceledTasks)
			}
		})
	}

	t.Run("unknown task → 404", func(t *testing.T) {
		st := fake.New()
		r := newTaskRouterCanceler(st, &fakeTaskCanceler{})
		req := newReq(t, http.MethodPost, "/api/v1/tasks/ghost/cancel", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rr.Code)
		}
	})
}
