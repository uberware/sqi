// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Queue REST handlers.
//
// Route summary:
//
//	POST   /api/v1/queues      — create a queue
//	GET    /api/v1/queues      — list queues with pagination + filters
//	GET    /api/v1/queues/{id} — get queue by ID
//	PUT    /api/v1/queues/{id} — replace mutable fields
//	DELETE /api/v1/queues/{id} — delete queue

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
)

// queueHandler handles all queue-related REST endpoints.
type queueHandler struct {
	store  store.Store
	logger *slog.Logger
}

// newQueueHandler returns a queueHandler wired to the given store.
func newQueueHandler(st store.Store, logger *slog.Logger) *queueHandler {
	return &queueHandler{store: st, logger: logger}
}

// ── Wire-format types ─────────────────────────────────────────────────────────

// queueResponse is the JSON representation of a queue.
type queueResponse struct {
	ID                 string    `json:"id"`
	FarmID             string    `json:"farm_id"`
	Name               string    `json:"name"`
	Description        string    `json:"description,omitempty"`
	Priority           int       `json:"priority"`
	MaxConcurrentTasks int       `json:"max_concurrent_tasks"`
	Paused             bool      `json:"paused"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
	// MaxAttempts, RetryDelaySeconds, and FailureLimit are the configured
	// queue-level retry-policy overrides. Nil means inherit (farm -> server
	// default).
	MaxAttempts       *int `json:"max_attempts,omitempty"`
	RetryDelaySeconds *int `json:"retry_delay_seconds,omitempty"`
	FailureLimit      *int `json:"failure_limit,omitempty"`
}

// createQueueRequest is the body accepted by POST /api/v1/queues.
type createQueueRequest struct {
	FarmID             string `json:"farm_id"`
	Name               string `json:"name"`
	Description        string `json:"description"`
	Priority           int    `json:"priority"`
	MaxConcurrentTasks int    `json:"max_concurrent_tasks"`
	Paused             bool   `json:"paused"`
	// MaxAttempts, RetryDelaySeconds, and FailureLimit set the queue's retry
	// policy overrides. Nil means inherit.
	MaxAttempts       *int `json:"max_attempts,omitempty"`
	RetryDelaySeconds *int `json:"retry_delay_seconds,omitempty"`
	FailureLimit      *int `json:"failure_limit,omitempty"`
}

// updateQueueRequest is the body accepted by PUT /api/v1/queues/{id}.
type updateQueueRequest struct {
	FarmID             string `json:"farm_id"`
	Name               string `json:"name"`
	Description        string `json:"description"`
	Priority           int    `json:"priority"`
	MaxConcurrentTasks int    `json:"max_concurrent_tasks"`
	Paused             bool   `json:"paused"`
	// MaxAttempts, RetryDelaySeconds, and FailureLimit set the queue's retry
	// policy overrides. Nil means inherit.
	MaxAttempts       *int `json:"max_attempts,omitempty"`
	RetryDelaySeconds *int `json:"retry_delay_seconds,omitempty"`
	FailureLimit      *int `json:"failure_limit,omitempty"`
}

// queueListResponse is the paginated result returned by GET /api/v1/queues.
type queueListResponse struct {
	Items  []queueResponse `json:"items"`
	Total  int             `json:"total"`
	Limit  int             `json:"limit"`
	Offset int             `json:"offset"`
}

// ── POST /api/v1/queues ───────────────────────────────────────────────────────

func (h *queueHandler) createQueue(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req createQueueRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if req.FarmID == "" {
		writeProblem(w, r, http.StatusBadRequest, "farm_id is required")
		return
	}
	if req.Name == "" {
		writeProblem(w, r, http.StatusBadRequest, "name is required")
		return
	}

	// Validate farm exists.
	if _, err := h.store.GetFarm(ctx, req.FarmID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusBadRequest, "farm not found")
			return
		}
		h.logger.ErrorContext(ctx, "queues: farm lookup failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to validate farm")
		return
	}

	now := time.Now().UTC()
	queue := store.Queue{
		ID:                 uuid.NewString(),
		FarmID:             req.FarmID,
		Name:               req.Name,
		Description:        req.Description,
		Priority:           req.Priority,
		MaxConcurrentTasks: req.MaxConcurrentTasks,
		Paused:             req.Paused,
		CreatedAt:          now,
		UpdatedAt:          now,
		MaxAttempts:        req.MaxAttempts,
		RetryDelaySeconds:  req.RetryDelaySeconds,
		FailureLimit:       req.FailureLimit,
	}

	created, err := h.store.CreateQueue(ctx, queue)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeProblem(w, r, http.StatusConflict, "a queue with that name already exists in this farm")
			return
		}
		h.logger.ErrorContext(ctx, "queues: create failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to create queue")
		return
	}

	writeJSON(w, http.StatusCreated, toQueueResponse(created))
}

// ── GET /api/v1/queues ────────────────────────────────────────────────────────

// listQueues returns a paginated, filtered list of queues.
//
// Query parameters:
//   - farm_id   — filter by farm
//   - paused    — "true" or "false" to filter by paused state
//   - sort_by   — name | priority | created_at (default: name)
//   - sort_dir  — asc | desc (default: asc)
//   - limit     — page size, 1–1000 (default: 50)
//   - offset    — zero-based item offset (default: 0)
func (h *queueHandler) listQueues(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()

	pg := store.Pagination{
		Limit:  parseIntQuery(q.Get("limit"), store.DefaultLimit),
		Offset: parseIntQuery(q.Get("offset"), 0),
	}
	pg.Validate() //nolint:errcheck // Validate only clamps; never errors

	var pausedFilter *bool
	if v := q.Get("paused"); v == "true" {
		t := true
		pausedFilter = &t
	} else if v == "false" {
		f := false
		pausedFilter = &f
	}

	opts := store.ListQueuesOptions{
		FarmID:     q.Get("farm_id"),
		Paused:     pausedFilter,
		SortBy:     toQueueSortField(q.Get("sort_by")),
		SortDir:    toSortDir(q.Get("sort_dir")),
		Pagination: pg,
	}

	page, err := h.store.ListQueues(ctx, opts)
	if err != nil {
		h.logger.ErrorContext(ctx, "queues: list failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to list queues")
		return
	}

	resp := queueListResponse{
		Items:  make([]queueResponse, len(page.Items)),
		Total:  page.Total,
		Limit:  page.Limit,
		Offset: page.Offset,
	}
	for i, qu := range page.Items {
		resp.Items[i] = toQueueResponse(qu)
	}

	writeJSON(w, http.StatusOK, resp)
}

// ── GET /api/v1/queues/{id} ───────────────────────────────────────────────────

func (h *queueHandler) getQueue(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	queue, err := h.store.GetQueue(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "queue not found")
			return
		}
		h.logger.ErrorContext(ctx, "queues: get failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to retrieve queue")
		return
	}

	writeJSON(w, http.StatusOK, toQueueResponse(queue))
}

// ── PUT /api/v1/queues/{id} ───────────────────────────────────────────────────

func (h *queueHandler) updateQueue(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	var req updateQueueRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if req.FarmID == "" {
		writeProblem(w, r, http.StatusBadRequest, "farm_id is required")
		return
	}
	if req.Name == "" {
		writeProblem(w, r, http.StatusBadRequest, "name is required")
		return
	}

	// Validate farm exists.
	if _, err := h.store.GetFarm(ctx, req.FarmID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusBadRequest, "farm not found")
			return
		}
		h.logger.ErrorContext(ctx, "queues: farm lookup failed on update", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to validate farm")
		return
	}

	queue := store.Queue{
		ID:                 id,
		FarmID:             req.FarmID,
		Name:               req.Name,
		Description:        req.Description,
		Priority:           req.Priority,
		MaxConcurrentTasks: req.MaxConcurrentTasks,
		Paused:             req.Paused,
		MaxAttempts:        req.MaxAttempts,
		RetryDelaySeconds:  req.RetryDelaySeconds,
		FailureLimit:       req.FailureLimit,
	}

	updated, err := h.store.UpdateQueue(ctx, queue)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "queue not found")
			return
		}
		if errors.Is(err, store.ErrConflict) {
			writeProblem(w, r, http.StatusConflict, "a queue with that name already exists in this farm")
			return
		}
		h.logger.ErrorContext(ctx, "queues: update failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to update queue")
		return
	}

	writeJSON(w, http.StatusOK, toQueueResponse(updated))
}

// ── DELETE /api/v1/queues/{id} ────────────────────────────────────────────────

func (h *queueHandler) deleteQueue(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	if err := h.store.DeleteQueue(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "queue not found")
			return
		}
		h.logger.ErrorContext(ctx, "queues: delete failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to delete queue")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ── Conversion helpers ────────────────────────────────────────────────────────

func toQueueResponse(qu store.Queue) queueResponse {
	return queueResponse{
		ID:                 qu.ID,
		FarmID:             qu.FarmID,
		Name:               qu.Name,
		Description:        qu.Description,
		Priority:           qu.Priority,
		MaxConcurrentTasks: qu.MaxConcurrentTasks,
		Paused:             qu.Paused,
		CreatedAt:          qu.CreatedAt,
		UpdatedAt:          qu.UpdatedAt,
		MaxAttempts:        qu.MaxAttempts,
		RetryDelaySeconds:  qu.RetryDelaySeconds,
		FailureLimit:       qu.FailureLimit,
	}
}

// ── Sort helpers ──────────────────────────────────────────────────────────────

func toQueueSortField(s string) store.QueueSortField {
	switch s {
	case "priority":
		return store.QueueSortByPriority
	case "created_at":
		return store.QueueSortByCreatedAt
	default:
		return store.QueueSortByName
	}
}
