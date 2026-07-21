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
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/auth/policy"
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
	// RunAsUser and RunAsGroup are the OS identity tasks in this queue execute
	// as. Nil RunAsUser means no isolation: tasks run as the worker's own
	// user. Setting either requires the isolation.manage permission.
	RunAsUser  *string `json:"run_as_user,omitempty"`
	RunAsGroup *string `json:"run_as_group,omitempty"`
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
	// RunAsUser and RunAsGroup set the queue's OS identity. Setting either
	// requires the isolation.manage permission (admin only) — see
	// requireIsolationManage.
	RunAsUser  *string `json:"run_as_user,omitempty"`
	RunAsGroup *string `json:"run_as_group,omitempty"`
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
	// RunAsUser and RunAsGroup set the queue's OS identity. Setting either
	// requires the isolation.manage permission (admin only) — see
	// requireIsolationManage. Applied on update too: an update-only hole
	// would let an operator add isolation to an existing queue after the
	// fact, defeating the create-side check.
	RunAsUser  *string `json:"run_as_user,omitempty"`
	RunAsGroup *string `json:"run_as_group,omitempty"`
}

// queueListResponse is the paginated result returned by GET /api/v1/queues.
type queueListResponse struct {
	Items  []queueResponse `json:"items"`
	Total  int             `json:"total"`
	Limit  int             `json:"limit"`
	Offset int             `json:"offset"`
}

// requireIsolationManage enforces the field-level gate on a queue's OS
// identity: setting or clearing run_as_user or run_as_group is an escalation
// surface separate from ordinary queue management (policy.InfraManage, which
// the operator role holds via the route-level gate in router.go), so it
// requires policy.IsolationManage (admin only). userSet/groupSet report
// whether the client's JSON body actually contained the run_as_user /
// run_as_group key (by any value, including explicit null) — not whether the
// decoded value is non-nil, since a *string cannot distinguish "key omitted"
// from "key set to null" and the two must be treated differently by the
// caller (see decodeQueueBody and the comments on updateQueue). A request
// that touches neither key is unaffected — this must never turn an ordinary
// queue write for an operator into a 403. Returns true if the request may
// proceed; on false it has already written the 403 response.
func requireIsolationManage(w http.ResponseWriter, r *http.Request, userSet, groupSet bool) bool {
	if !userSet && !groupSet {
		return true
	}
	p, _ := auth.FromContext(r.Context())
	if !policy.Can(p, policy.IsolationManage) {
		writeProblem(w, r, http.StatusForbidden,
			"changing run_as_user or run_as_group requires the isolation.manage permission")
		return false
	}
	return true
}

// decodeQueueBody reads r.Body exactly once and decodes it both into req
// (the typed request struct) and into a map of raw top-level keys, so a
// handler can tell whether a field was present in the JSON body — including
// present with an explicit null — versus omitted entirely. That distinction
// is invisible on the typed struct alone, since a *string field is nil in
// both cases. The body bytes are read once with io.ReadAll and unmarshaled
// twice so r.Body (a non-seekable reader) is never consumed more than once.
func decodeQueueBody[T any](r *http.Request, req *T) (map[string]json.RawMessage, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20)) // 4 MiB cap, matches jobs.go precedent
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(body, req); err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// ── POST /api/v1/queues ───────────────────────────────────────────────────────

func (h *queueHandler) createQueue(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req createQueueRequest
	raw, err := decodeQueueBody(r, &req)
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}

	_, userSet := raw["run_as_user"]
	_, groupSet := raw["run_as_group"]
	if !requireIsolationManage(w, r, userSet, groupSet) {
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
	if problem := validateRetryOverrides(req.MaxAttempts, req.RetryDelaySeconds, req.FailureLimit); problem != "" {
		writeProblem(w, r, http.StatusBadRequest, problem)
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
		RunAsUser:          req.RunAsUser,
		RunAsGroup:         req.RunAsGroup,
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
	raw, err := decodeQueueBody(r, &req)
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}

	_, userSet := raw["run_as_user"]
	_, groupSet := raw["run_as_group"]
	if !requireIsolationManage(w, r, userSet, groupSet) {
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
	if problem := validateRetryOverrides(req.MaxAttempts, req.RetryDelaySeconds, req.FailureLimit); problem != "" {
		writeProblem(w, r, http.StatusBadRequest, problem)
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

	// PUT is a full replace for every field here EXCEPT run_as_user and
	// run_as_group, which deliberately deviate from strict replace semantics:
	// a key omitted from the body means "preserve the stored value", not
	// "clear it" (see the doc on updateQueueRequest.RunAsUser). That
	// preservation is expressed via PreserveRunAsUser/PreserveRunAsGroup and
	// resolved inside the single UpdateQueue statement — see
	// store.Queue.PreserveRunAsUser — rather than by reading the existing
	// queue here first: a read-then-write gap between an operator's
	// isolation-omitting PUT and a concurrent admin PUT that sets the
	// identity could otherwise let the operator's write silently clobber the
	// admin's with a stale nil (lost update).
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
		RunAsUser:          req.RunAsUser,
		RunAsGroup:         req.RunAsGroup,
		PreserveRunAsUser:  !userSet,
		PreserveRunAsGroup: !groupSet,
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
		RunAsUser:          qu.RunAsUser,
		RunAsGroup:         qu.RunAsGroup,
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
