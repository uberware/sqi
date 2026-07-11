// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Task REST handlers.
//
// Route summary:
//
//	GET  /api/v1/jobs/{id}/tasks    — list tasks for a job
//	GET  /api/v1/tasks/{id}         — task detail
//	GET  /api/v1/tasks/{id}/logs    — task logs, offset-based or tail-streaming
//	POST /api/v1/tasks/{id}/retry   — queue a new attempt for a failed/canceled task
//	POST /api/v1/tasks/{id}/cancel  — cancel a non-terminal task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/uberware/sqi/internal/store"
)

// taskCanceler is the subset of [scheduler.Scheduler] used by the task handler
// to cancel a single running task (close its attempt, signal the worker, and
// release usage-pool slots), and to retry a failed or canceled task with full
// graph revival.
type taskCanceler interface {
	CancelTask(ctx context.Context, taskID string) error
	RetryTask(ctx context.Context, taskID string) error
}

// taskHandler handles all task-related REST endpoints.
type taskHandler struct {
	store  store.Store
	sched  taskCanceler
	logger *slog.Logger
}

// newTaskHandler returns a taskHandler wired to the given store and scheduler.
func newTaskHandler(st store.Store, sched taskCanceler, logger *slog.Logger) *taskHandler {
	return &taskHandler{store: st, sched: sched, logger: logger}
}

// ── Wire-format types ─────────────────────────────────────────────────────────

// taskResponse is the JSON representation of a task.
type taskResponse struct {
	ID               string            `json:"id"`
	JobID            string            `json:"job_id"`
	StepID           string            `json:"step_id"`
	Name             string            `json:"name"`
	Parameters       map[string]string `json:"parameters,omitempty"`
	Status           string            `json:"status"`
	AssignedWorkerID string            `json:"assigned_worker_id,omitempty"`
	AssignedAt       *time.Time        `json:"assigned_at,omitempty"`
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
	// UnschedulableReason is set when a ready task cannot currently be
	// satisfied by any online worker; empty means schedulable.
	UnschedulableReason string `json:"unschedulable_reason,omitempty"`
	// FailureReason is the human-readable reason the task reached a terminal
	// non-success (failed or canceled). Empty for non-terminal tasks.
	FailureReason string `json:"failure_reason,omitempty"`
	// FailedAttempts counts attempts that genuinely ran and failed (does not
	// count worker crashes / unassignments that trigger a retry without a
	// genuine failure).
	FailedAttempts int `json:"failed_attempts"`
	// RetryAfter, when set and in the future, holds the task in the ready
	// state as a retry backoff; a worker will not be assigned the task until
	// this time passes.
	RetryAfter *time.Time `json:"retry_after,omitempty"`
}

// taskListResponse is the paginated result returned by GET /api/v1/jobs/{id}/tasks.
type taskListResponse struct {
	Items  []taskResponse `json:"items"`
	Total  int            `json:"total"`
	Limit  int            `json:"limit"`
	Offset int            `json:"offset"`
}

// taskLogResponse is the JSON representation of a single log chunk.
type taskLogResponse struct {
	ID         string    `json:"id"`
	TaskID     string    `json:"task_id"`
	AttemptID  string    `json:"attempt_id"`
	SeqNum     int64     `json:"seq_num"`
	NATSSeq    int64     `json:"nats_seq"`
	Stream     string    `json:"stream"`
	Data       string    `json:"data"`
	At         time.Time `json:"at"`
	ReceivedAt time.Time `json:"received_at"`
}

// taskLogsResponse is returned by non-streaming GET /api/v1/tasks/{id}/logs.
type taskLogsResponse struct {
	Items        []taskLogResponse `json:"items"`
	AfterNATSSeq int64             `json:"after_nats_seq"`
	Limit        int               `json:"limit"`
}

// retryResponse is returned by POST /api/v1/tasks/{id}/retry.
type retryResponse struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
}

// cancelResponse is returned by POST /api/v1/tasks/{id}/cancel.
type cancelResponse struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
}

// ── GET /api/v1/jobs/{id}/tasks ───────────────────────────────────────────────

// listJobTasks returns a paginated list of tasks belonging to the given job.
//
// Query parameters:
//   - status   — filter by [store.TaskStatus]
//   - sort_by  — created_at | status | updated_at | name  (default: created_at)
//   - sort_dir — asc | desc  (default: asc)
//   - limit    — page size, 1–1000  (default: 50)
//   - offset   — zero-based item offset  (default: 0)
func (h *taskHandler) listJobTasks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	jobID := chi.URLParam(r, "id")

	// Verify the job exists so we can return 404 rather than an empty list.
	if _, err := h.store.GetJob(ctx, jobID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "job not found")
			return
		}
		h.logger.ErrorContext(ctx, "tasks: get job failed", slog.String("job_id", jobID), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to retrieve job")
		return
	}

	q := r.URL.Query()
	pg := store.Pagination{
		Limit:  parseIntQuery(q.Get("limit"), store.DefaultLimit),
		Offset: parseIntQuery(q.Get("offset"), 0),
	}
	pg.Validate() //nolint:errcheck // Validate only clamps; never errors

	opts := store.ListTasksOptions{
		JobID:   jobID,
		Status:  store.TaskStatus(q.Get("status")),
		SortBy:  toTaskSortField(q.Get("sort_by")),
		SortDir: toSortDir(q.Get("sort_dir")),

		Pagination: pg,
	}

	page, err := h.store.ListTasks(ctx, opts)
	if err != nil {
		h.logger.ErrorContext(ctx, "tasks: list failed", slog.String("job_id", jobID), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to list tasks")
		return
	}

	resp := taskListResponse{
		Items:  make([]taskResponse, len(page.Items)),
		Total:  page.Total,
		Limit:  page.Limit,
		Offset: page.Offset,
	}
	for i, t := range page.Items {
		resp.Items[i] = toTaskResponse(t)
	}

	writeJSON(w, http.StatusOK, resp)
}

// ── GET /api/v1/tasks/{id} ────────────────────────────────────────────────────

// getTask returns a single task by ID with its full detail.
func (h *taskHandler) getTask(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	task, err := h.store.GetTask(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "task not found")
			return
		}
		h.logger.ErrorContext(ctx, "tasks: get failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to retrieve task")
		return
	}

	writeJSON(w, http.StatusOK, toTaskResponse(task))
}

// ── GET /api/v1/tasks/{id}/logs ───────────────────────────────────────────────

// getTaskLogs returns log chunks for the latest attempt of the given task.
//
// Query parameters:
//   - after_nats_seq — return chunks with NATSSeq > this value (default: 0)
//   - limit          — maximum chunks per page, 1–1000 (default: 100)
//   - tail           — when "true", stream new chunks as they arrive using
//     chunked transfer encoding until the task reaches a terminal state.
//     The response is a series of newline-delimited JSON log chunk objects.
//     Each flushed line is a JSON [taskLogResponse] followed by a newline.
//
// Offset-based pagination: call repeatedly with the nats_seq of the last
// received item as after_nats_seq to walk forward without repeating chunks.
//
// When no attempts exist yet, an empty result is returned rather than a 404
// so pollers do not need to handle that as a special case.
func (h *taskHandler) getTaskLogs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	// Verify the task exists.
	if _, err := h.store.GetTask(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "task not found")
			return
		}
		h.logger.ErrorContext(ctx, "tasks: get task for logs failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to retrieve task")
		return
	}

	q := r.URL.Query()
	afterNATSSeq := int64(parseIntQuery(q.Get("after_nats_seq"), 0))
	limit := max(1, min(parseIntQuery(q.Get("limit"), 100), 1000))
	tail := q.Get("tail") == "true"

	// Resolve the latest attempt for this task. If none exists, return empty.
	attempt, err := h.store.LatestTaskAttempt(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			if tail {
				h.streamTaskLogs(w, r, id, afterNATSSeq, limit)
				return
			}
			writeJSON(w, http.StatusOK, taskLogsResponse{
				Items:        []taskLogResponse{},
				AfterNATSSeq: afterNATSSeq,
				Limit:        limit,
			})
			return
		}
		h.logger.ErrorContext(ctx, "tasks: latest attempt failed", slog.String("task_id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to retrieve task attempt")
		return
	}

	if tail {
		h.streamTaskLogs(w, r, id, afterNATSSeq, limit)
		return
	}

	logs, err := h.store.ListTaskLogs(ctx, attempt.ID, afterNATSSeq, limit)
	if err != nil {
		h.logger.ErrorContext(ctx, "tasks: list logs failed", slog.String("task_id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to retrieve task logs")
		return
	}

	items := make([]taskLogResponse, len(logs))
	for i, l := range logs {
		items[i] = toTaskLogResponse(l)
	}

	// Report the cursor to advance to: the highest nats_seq on this page so the
	// next poll fetches only newer chunks. With no new chunks the caller's
	// position is preserved (echo the request) rather than rewound. Logs come
	// back ordered by nats_seq ascending, so the last item carries the max.
	nextAfter := afterNATSSeq
	if n := len(logs); n > 0 {
		nextAfter = logs[n-1].NATSSeq
	}

	writeJSON(w, http.StatusOK, taskLogsResponse{
		Items:        items,
		AfterNATSSeq: nextAfter,
		Limit:        limit,
	})
}

// streamTaskLogs streams log chunks to the client using chunked transfer
// encoding. Each emitted line is a newline-delimited JSON [taskLogResponse].
// Streaming continues until the task reaches a terminal state (succeeded,
// failed, or canceled) and all available chunks have been delivered, or until
// the client disconnects.
//
// The poll interval is 500 ms — coarse enough to avoid hammering SQLite but
// fine enough that log output feels responsive in a terminal.
func (h *taskHandler) streamTaskLogs(w http.ResponseWriter, r *http.Request, taskID string, afterNATSSeq int64, limit int) {
	ctx := r.Context()

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeProblem(w, r, http.StatusInternalServerError, "streaming not supported by this server")
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	const pollInterval = 500 * time.Millisecond
	cursor := afterNATSSeq

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(pollInterval):
		}

		// Re-fetch the latest attempt on every tick; it may change on retry.
		attempt, err := h.store.LatestTaskAttempt(ctx, taskID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// No attempt yet; keep polling.
				continue
			}
			h.logger.ErrorContext(ctx, "tasks: stream latest attempt failed",
				slog.String("task_id", taskID), slog.Any("error", err))
			return
		}

		logs, err := h.store.ListTaskLogs(ctx, attempt.ID, cursor, limit)
		if err != nil {
			h.logger.ErrorContext(ctx, "tasks: stream list logs failed",
				slog.String("task_id", taskID), slog.String("attempt_id", attempt.ID), slog.Any("error", err))
			return
		}

		for _, l := range logs {
			b, merr := json.Marshal(toTaskLogResponse(l))
			if merr != nil {
				h.logger.ErrorContext(ctx, "tasks: stream marshal failed",
					slog.String("task_id", taskID), slog.String("attempt_id", attempt.ID), slog.Any("error", merr))
				return
			}
			if _, werr := fmt.Fprintf(w, "%s\n", b); werr != nil {
				return // client disconnected
			}
			cursor = l.NATSSeq
		}

		if len(logs) > 0 {
			flusher.Flush()
		}

		// Stop streaming once the task is terminal and no more chunks arrived.
		task, terr := h.store.GetTask(ctx, taskID)
		if terr != nil {
			return
		}

		if isTerminalTask(task.Status) && len(logs) == 0 {
			return
		}
	}
}

// isTerminalTask reports whether a task in the given status can no longer
// produce new log output.
func isTerminalTask(s store.TaskStatus) bool {
	return s == store.TaskStatusSucceeded ||
		s == store.TaskStatusFailed ||
		s == store.TaskStatusCanceled
}

// ── POST /api/v1/tasks/{id}/retry ─────────────────────────────────────────────

// retryTask revives a failed or canceled task through the scheduler, which
// resets the task (and its step and job when terminal) and re-resolves
// dependencies. The response status reflects the task's actual post-revival
// state, which is [store.TaskStatusReady] when the step's dependencies are
// already satisfied, or [store.TaskStatusPending] when they are not.
//
// Only tasks in the [store.TaskStatusFailed] or [store.TaskStatusCanceled]
// state may be retried. Tasks that are pending, ready, assigned, running, or
// already succeeded are rejected with 409 Conflict.
func (h *taskHandler) retryTask(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	task, err := h.store.GetTask(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "task not found")
			return
		}
		h.logger.ErrorContext(ctx, "tasks: retry get failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to retrieve task")
		return
	}

	if task.Status != store.TaskStatusFailed && task.Status != store.TaskStatusCanceled {
		writeProblem(w, r, http.StatusConflict,
			fmt.Sprintf("task cannot be retried in status %q; only failed or canceled tasks are eligible", task.Status))
		return
	}

	if err = h.sched.RetryTask(ctx, id); err != nil {
		h.logger.ErrorContext(ctx, "tasks: retry failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to queue task retry")
		return
	}

	// Report the task's actual post-revival status (ready when its step's deps
	// are satisfied, otherwise pending).
	status := string(store.TaskStatusPending)
	if updated, gErr := h.store.GetTask(ctx, id); gErr == nil {
		status = string(updated.Status)
	} else {
		h.logger.WarnContext(ctx, "tasks: retry status re-fetch failed", slog.String("id", id), slog.Any("error", gErr))
	}

	writeJSON(w, http.StatusAccepted, retryResponse{
		TaskID: id,
		Status: status,
	})
}

// ── Conversion helpers ────────────────────────────────────────────────────────

// toTaskResponse converts a [store.Task] into the API wire type.
func toTaskResponse(t store.Task) taskResponse {
	return taskResponse{
		ID:                  t.ID,
		JobID:               t.JobID,
		StepID:              t.StepID,
		Name:                t.Name,
		Parameters:          t.Parameters,
		Status:              string(t.Status),
		AssignedWorkerID:    t.AssignedWorkerID,
		AssignedAt:          t.AssignedAt,
		CreatedAt:           t.CreatedAt,
		UpdatedAt:           t.UpdatedAt,
		UnschedulableReason: t.UnschedulableReason,
		FailureReason:       t.FailureReason,
		FailedAttempts:      t.FailedAttempts,
		RetryAfter:          t.RetryAfter,
	}
}

// toTaskLogResponse converts a [store.TaskLog] into the API wire type.
func toTaskLogResponse(l store.TaskLog) taskLogResponse {
	return taskLogResponse{
		ID:         l.ID,
		TaskID:     l.TaskID,
		AttemptID:  l.AttemptID,
		SeqNum:     l.SeqNum,
		NATSSeq:    l.NATSSeq,
		Stream:     string(l.Stream),
		Data:       l.Data,
		At:         l.At,
		ReceivedAt: l.ReceivedAt,
	}
}

// ── Sort helpers ──────────────────────────────────────────────────────────────

// toTaskSortField maps a query-parameter string to a [store.TaskSortField].
// Unknown values fall back to the default sort field.
func toTaskSortField(s string) store.TaskSortField {
	switch s {
	case "status":
		return store.TaskSortByStatus
	case "updated_at":
		return store.TaskSortByUpdatedAt
	case "name":
		return store.TaskSortByName
	default:
		return store.TaskSortByCreatedAt
	}
}

// ── POST /api/v1/tasks/{id}/cancel ────────────────────────────────────────────

// cancelTask transitions a single non-terminal task to canceled via the
// scheduler, which closes its attempt, signals the assigned worker, and
// releases held usage-pool slots.
//
// Tasks already in a terminal state (succeeded, failed, canceled) are rejected
// with 409 Conflict.
func (h *taskHandler) cancelTask(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	task, err := h.store.GetTask(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "task not found")
			return
		}
		h.logger.ErrorContext(ctx, "tasks: cancel get failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to retrieve task")
		return
	}

	if isTerminalTask(task.Status) {
		writeProblem(w, r, http.StatusConflict,
			fmt.Sprintf("task cannot be canceled in status %q; only non-terminal tasks are eligible", task.Status))
		return
	}

	if err = h.sched.CancelTask(ctx, id); err != nil {
		h.logger.ErrorContext(ctx, "tasks: cancel failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to cancel task")
		return
	}

	writeJSON(w, http.StatusAccepted, cancelResponse{
		TaskID: id,
		Status: string(store.TaskStatusCanceled),
	})
}
