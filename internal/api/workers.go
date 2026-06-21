// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Worker REST handlers.
//
// Route summary:
//
//	GET /api/v1/workers — list workers with pagination + filters
//	GET /api/v1/workers/{id} — worker detail with capability and current-task
//	POST /api/v1/workers/{id}/disable — administratively pause a worker
//	POST /api/v1/workers/{id}/enable — re-enable a disabled worker

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/ws"
)

// workerHandler handles all worker-related REST endpoints.
type workerHandler struct {
	store store.Store
	// notifier pushes a removed event when a worker is deleted so other
	// connected clients drop it live. May be nil (no push).
	notifier ws.Notifier
	// offlineThreshold is the heartbeat-timeout window used to decide whether a
	// disabled worker is dead — and therefore removable — from its last
	// heartbeat age. It mirrors the scheduler's WorkerTimeout.
	offlineThreshold time.Duration
	logger           *slog.Logger
}

// newWorkerHandler returns a workerHandler wired to the given store. notifier
// may be nil in tests that do not exercise WebSocket push.
func newWorkerHandler(st store.Store, notifier ws.Notifier, offlineThreshold time.Duration, logger *slog.Logger) *workerHandler {
	return &workerHandler{
		store:            st,
		notifier:         notifier,
		offlineThreshold: offlineThreshold,
		logger:           logger,
	}
}

// workerRemovable reports whether a worker may be hard-deleted: it is offline,
// or it is administratively disabled but its last heartbeat is older than the
// offline threshold (i.e. the machine is actually gone, not merely paused). A
// live disabled worker is never removable — removing it would let it
// re-register as online, silently undoing the operator's Disable.
func workerRemovable(wk store.Worker, threshold time.Duration, now time.Time) bool {
	switch wk.Status {
	case store.WorkerStatusOffline:
		return true
	case store.WorkerStatusDisabled:
		return wk.LastHeartbeatAt != nil && wk.LastHeartbeatAt.Before(now.Add(-threshold))
	default:
		return false
	}
}

// ── Wire-format types ─────────────────────────────────────────────────────────

// gpuInfoResponse is the JSON representation of [store.GPUInfo].
type gpuInfoResponse struct {
	Vendor string `json:"vendor,omitempty"`
	Model  string `json:"model,omitempty"`
	VRAMMb int    `json:"vram_mb,omitempty"`
	Count  int    `json:"count,omitempty"`
}

// currentTaskResponse carries the minimal fields callers need to identify a
// task a worker is currently executing. One entry appears in
// workerDetailResponse.CurrentTasks for every task in [store.TaskStatusAssigned]
// or [store.TaskStatusRunning] state on the worker.
type currentTaskResponse struct {
	ID         string     `json:"id"`
	JobID      string     `json:"job_id"`
	Name       string     `json:"name"`
	Status     string     `json:"status"`
	AssignedAt *time.Time `json:"assigned_at,omitempty"`
}

// workerResponse is the JSON representation of a worker returned by the list
// endpoint.
type workerResponse struct {
	ID              string            `json:"id"`
	FarmID          string            `json:"farm_id"`
	QueueID         string            `json:"queue_id,omitempty"`
	Name            string            `json:"name,omitempty"`
	Hostname        string            `json:"hostname"`
	IPAddress       string            `json:"ip_address,omitempty"`
	ComputeLocation string            `json:"compute_location,omitempty"`
	OS              string            `json:"os,omitempty"`
	OSVersion       string            `json:"os_version,omitempty"`
	CPUCount        int               `json:"cpu_count,omitempty"`
	RAMMb           int               `json:"ram_mb,omitempty"`
	GPU             gpuInfoResponse   `json:"gpu"`
	Tags            map[string]string `json:"tags,omitempty"`
	Status          string            `json:"status"`
	Removable       bool              `json:"removable"`
	LastHeartbeatAt *time.Time        `json:"last_heartbeat_at,omitempty"`
	RegisteredAt    time.Time         `json:"registered_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

// workerDetailResponse extends [workerResponse] with the active-task detail
// returned by GET /api/v1/workers/{id}. CurrentTasks is always present (an empty
// array when the worker has no active tasks).
type workerDetailResponse struct {
	workerResponse

	CurrentTasks []currentTaskResponse `json:"current_tasks"`
}

// workerListResponse is the paginated result returned by GET /api/v1/workers.
type workerListResponse struct {
	Items  []workerResponse `json:"items"`
	Total  int              `json:"total"`
	Limit  int              `json:"limit"`
	Offset int              `json:"offset"`
}

// workerActionResponse is returned by the disable/enable endpoints.
type workerActionResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// ── GET /api/v1/workers ───────────────────────────────────────────────────────

// listWorkers returns a paginated, filtered list of workers.
//
// Query parameters:
//   - farm_id          — filter by farm
//   - queue_id         — filter by queue affinity
//   - compute_location — filter by compute location
//   - status           — filter by worker status (online | offline | disabled)
//   - sort_by          — hostname | status | registered_at | last_heartbeat_at (default: hostname)
//   - sort_dir         — asc | desc (default: asc)
//   - limit            — page size, 1–1000 (default: 50)
//   - offset           — zero-based item offset (default: 0)
func (h *workerHandler) listWorkers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()

	pg := store.Pagination{
		Limit:  parseIntQuery(q.Get("limit"), store.DefaultLimit),
		Offset: parseIntQuery(q.Get("offset"), 0),
	}
	pg.Validate() //nolint:errcheck // Validate only clamps; never errors

	opts := store.ListWorkersOptions{
		FarmID:          q.Get("farm_id"),
		QueueID:         q.Get("queue_id"),
		ComputeLocation: q.Get("compute_location"),
		Status:          store.WorkerStatus(q.Get("status")),
		Search:          q.Get("search"),
		SortBy:          toWorkerSortField(q.Get("sort_by")),
		SortDir:         toSortDir(q.Get("sort_dir")),
		Pagination:      pg,
	}

	page, err := h.store.ListWorkers(ctx, opts)
	if err != nil {
		h.logger.ErrorContext(ctx, "workers: list failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to list workers")
		return
	}

	resp := workerListResponse{
		Items:  make([]workerResponse, len(page.Items)),
		Total:  page.Total,
		Limit:  page.Limit,
		Offset: page.Offset,
	}
	for i, wk := range page.Items {
		resp.Items[i] = h.toWorkerResponse(wk)
	}

	writeJSON(w, http.StatusOK, resp)
}

// ── GET /api/v1/workers/{id} ──────────────────────────────────────────────────

// getWorker returns a single worker by ID with its capabilities and the task
// it is currently executing (if any).
func (h *workerHandler) getWorker(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	wk, err := h.store.GetWorker(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "worker not found")
			return
		}
		h.logger.ErrorContext(ctx, "workers: get failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to retrieve worker")
		return
	}

	resp := workerDetailResponse{
		workerResponse: h.toWorkerResponse(wk),
		CurrentTasks:   []currentTaskResponse{},
	}

	// Attach active-task detail: every task in the assigned or running state that
	// points to this worker. A worker can execute several tasks at once (bounded
	// by its core count), so we return all of them ordered oldest-first.
	// ListTasks is reused here to avoid adding a Store method for a path that
	// only fires on the detail view.
	taskPage, err := h.store.ListTasks(ctx, store.ListTasksOptions{
		WorkerID: id,
		Statuses: []store.TaskStatus{
			store.TaskStatusAssigned,
			store.TaskStatusRunning,
		},
		SortBy:     store.TaskSortByCreatedAt,
		SortDir:    store.SortAsc,
		Pagination: store.Pagination{Limit: store.MaxLimit, Offset: 0},
	})
	if err != nil {
		h.logger.ErrorContext(ctx, "workers: list active tasks failed",
			slog.String("worker_id", id), slog.Any("error", err))
		// Non-fatal: return the worker without active tasks rather than 500.
	} else {
		resp.CurrentTasks = make([]currentTaskResponse, len(taskPage.Items))
		for i, t := range taskPage.Items {
			resp.CurrentTasks[i] = currentTaskResponse{
				ID:         t.ID,
				JobID:      t.JobID,
				Name:       t.Name,
				Status:     string(t.Status),
				AssignedAt: t.AssignedAt,
			}
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// ── POST /api/v1/workers/{id}/disable ────────────────────────────────────────

// disableWorker administratively pauses a worker so it receives no new task
// assignments. Workers already in [store.WorkerStatusDisabled] are accepted
// idempotently and return 200 with the current status.
func (h *workerHandler) disableWorker(w http.ResponseWriter, r *http.Request) {
	h.setWorkerStatus(w, r, store.WorkerStatusDisabled)
}

// ── POST /api/v1/workers/{id}/enable ─────────────────────────────────────────

// enableWorker re-enables a disabled worker. Online or offline workers are
// accepted idempotently (their status is left unchanged) and the current status
// is returned.
func (h *workerHandler) enableWorker(w http.ResponseWriter, r *http.Request) {
	h.setWorkerStatus(w, r, store.WorkerStatusOnline)
}

// setWorkerStatus is the shared implementation for disable and enable. It
// resolves the transition according to the table below and writes the result.
//
// For disable (target = WorkerStatusDisabled):
//   - online  → disabled  (200)
//   - offline → disabled  (200)
//   - disabled → disabled (200, idempotent)
//
// For enable (target = WorkerStatusOnline):
//   - disabled → online (200)
//   - online   → online (200, idempotent)
//   - offline  → online (200, idempotent — worker will move to offline again on
//     next heartbeat sweep if it stays quiet)
func (h *workerHandler) setWorkerStatus(w http.ResponseWriter, r *http.Request, target store.WorkerStatus) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	if _, err := h.store.GetWorker(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "worker not found")
			return
		}
		h.logger.ErrorContext(ctx, "workers: get for status update failed",
			slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to retrieve worker")
		return
	}

	if err := h.store.UpdateWorkerStatus(ctx, id, target); err != nil {
		h.logger.ErrorContext(ctx, "workers: status update failed",
			slog.String("id", id),
			slog.String("target_status", string(target)),
			slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to update worker status")
		return
	}

	writeJSON(w, http.StatusOK, workerActionResponse{
		ID:     id,
		Status: string(target),
	})
}

// ── DELETE /api/v1/workers/{id} ──────────────────────────────────────────────

// removeWorker hard-deletes a worker record. Only removable workers are
// accepted: offline workers, or disabled workers whose last heartbeat is older
// than the offline threshold (the machine is gone). Online and live-disabled
// workers return 409 Conflict. Offline workers already had their in-flight tasks
// reclaimed when they went offline, so no reclaim is needed here.
func (h *workerHandler) removeWorker(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	wk, err := h.store.GetWorker(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "worker not found")
			return
		}
		h.logger.ErrorContext(ctx, "workers: get for remove failed",
			slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to retrieve worker")
		return
	}

	if !workerRemovable(wk, h.offlineThreshold, time.Now()) {
		writeProblem(w, r, http.StatusConflict,
			"worker is not removable: only offline or dead disabled workers can be removed")
		return
	}

	if err := h.store.DeleteWorker(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "worker not found")
			return
		}
		h.logger.ErrorContext(ctx, "workers: delete failed",
			slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to remove worker")
		return
	}

	if h.notifier != nil {
		h.notifier.NotifyWorker(ws.WorkerEvent{
			WorkerID: wk.ID,
			Name:     wk.Name,
			Hostname: wk.Hostname,
			FarmID:   wk.FarmID,
			Status:   ws.WorkerStatusRemoved,
		})
	}

	w.WriteHeader(http.StatusNoContent)
}

// ── Conversion helpers ────────────────────────────────────────────────────────

// toWorkerResponse converts a [store.Worker] into the API wire type, stamping
// the server-authoritative Removable flag from the worker's current liveness.
func (h *workerHandler) toWorkerResponse(wk store.Worker) workerResponse {
	return h.toWorkerResponseAt(wk, time.Now())
}

// toWorkerResponseAt is toWorkerResponse with an explicit clock, for tests.
func (h *workerHandler) toWorkerResponseAt(wk store.Worker, now time.Time) workerResponse {
	resp := buildWorkerResponse(wk)
	resp.Removable = workerRemovable(wk, h.offlineThreshold, now)
	return resp
}

// buildWorkerResponse maps the static worker fields; the Removable flag is
// stamped by the handler since it depends on the configured threshold.
func buildWorkerResponse(wk store.Worker) workerResponse {
	return workerResponse{
		ID:              wk.ID,
		FarmID:          wk.FarmID,
		QueueID:         wk.QueueID,
		Name:            wk.Name,
		Hostname:        wk.Hostname,
		IPAddress:       wk.IPAddress,
		ComputeLocation: wk.ComputeLocation,
		OS:              wk.OS,
		OSVersion:       wk.OSVersion,
		CPUCount:        wk.CPUCount,
		RAMMb:           wk.RAMMb,
		GPU: gpuInfoResponse{
			Vendor: wk.GPUInfo.Vendor,
			Model:  wk.GPUInfo.Model,
			VRAMMb: wk.GPUInfo.VRAMMb,
			Count:  wk.GPUInfo.Count,
		},
		Tags:            wk.Tags,
		Status:          string(wk.Status),
		LastHeartbeatAt: wk.LastHeartbeatAt,
		RegisteredAt:    wk.RegisteredAt,
		UpdatedAt:       wk.UpdatedAt,
	}
}

// ── Sort helpers ──────────────────────────────────────────────────────────────

// toWorkerSortField maps a query-parameter string to a [store.WorkerSortField].
// Unknown values fall back to WorkerSortByHostname.
func toWorkerSortField(s string) store.WorkerSortField {
	switch s {
	case "status":
		return store.WorkerSortByStatus
	case "registered_at":
		return store.WorkerSortByRegisteredAt
	case "last_heartbeat_at":
		return store.WorkerSortByLastHeartbeatAt
	default:
		return store.WorkerSortByHostname
	}
}
