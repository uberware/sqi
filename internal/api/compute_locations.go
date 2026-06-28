// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Compute-location REST handlers.
//
//	POST   /api/v1/compute-locations      — create
//	GET    /api/v1/compute-locations      — list (with worker_count)
//	GET    /api/v1/compute-locations/{id} — get (with worker_count)
//	PUT    /api/v1/compute-locations/{id} — replace mutable fields
//	DELETE /api/v1/compute-locations/{id} — delete (non-blocking)

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/openjd"
	"github.com/uberware/sqi/internal/store"
)

type computeLocationHandler struct {
	store  store.Store
	logger *slog.Logger
}

func newComputeLocationHandler(st store.Store, logger *slog.Logger) *computeLocationHandler {
	return &computeLocationHandler{store: st, logger: logger}
}

type computeLocationResponse struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	WorkerCount int       `json:"worker_count"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type computeLocationRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (h *computeLocationHandler) onlineWorkerCount(ctx context.Context, name string) int {
	pg := store.Pagination{Limit: store.MaxLimit}
	pg.Validate() //nolint:errcheck // Validate only clamps; never errors
	page, err := h.store.ListWorkers(ctx, store.ListWorkersOptions{
		ComputeLocation: name,
		Status:          store.WorkerStatusOnline,
		Pagination:      pg,
	})
	if err != nil {
		h.logger.WarnContext(ctx, "compute-locations: worker count failed",
			slog.String("compute_location", name), slog.Any("error", err))
		return 0
	}
	return page.Total
}

func validateComputeLocationName(w http.ResponseWriter, r *http.Request, name string) bool {
	if name == "" {
		writeProblem(w, r, http.StatusBadRequest, "name is required")
		return false
	}
	if !openjd.ValidLocationName(name) {
		writeProblem(w, r, http.StatusBadRequest, `name must not contain whitespace, "/", or quotes`)
		return false
	}
	return true
}

func (h *computeLocationHandler) createComputeLocation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req computeLocationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !validateComputeLocationName(w, r, req.Name) {
		return
	}
	now := time.Now().UTC()
	created, err := h.store.CreateComputeLocation(ctx, store.ComputeLocation{
		ID: uuid.NewString(), Name: req.Name, Description: req.Description,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeProblem(w, r, http.StatusConflict, "a compute location with that name already exists")
			return
		}
		h.logger.ErrorContext(ctx, "compute-locations: create failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to create compute location")
		return
	}
	writeJSON(w, http.StatusCreated, h.toResponse(ctx, created))
}

func (h *computeLocationHandler) listComputeLocations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	locs, err := h.store.ListComputeLocations(ctx)
	if err != nil {
		h.logger.ErrorContext(ctx, "compute-locations: list failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to list compute locations")
		return
	}
	resp := make([]computeLocationResponse, len(locs))
	for i, l := range locs {
		resp[i] = h.toResponse(ctx, l)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *computeLocationHandler) getComputeLocation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	loc, err := h.store.GetComputeLocation(ctx, chi.URLParam(r, "id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "compute location not found")
			return
		}
		h.logger.ErrorContext(ctx, "compute-locations: get failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to retrieve compute location")
		return
	}
	writeJSON(w, http.StatusOK, h.toResponse(ctx, loc))
}

func (h *computeLocationHandler) updateComputeLocation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	var req computeLocationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !validateComputeLocationName(w, r, req.Name) {
		return
	}
	updated, err := h.store.UpdateComputeLocation(ctx, store.ComputeLocation{
		ID: id, Name: req.Name, Description: req.Description,
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "compute location not found")
			return
		}
		if errors.Is(err, store.ErrConflict) {
			writeProblem(w, r, http.StatusConflict, "a compute location with that name already exists")
			return
		}
		h.logger.ErrorContext(ctx, "compute-locations: update failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to update compute location")
		return
	}
	writeJSON(w, http.StatusOK, h.toResponse(ctx, updated))
}

func (h *computeLocationHandler) deleteComputeLocation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := h.store.DeleteComputeLocation(ctx, chi.URLParam(r, "id")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "compute location not found")
			return
		}
		h.logger.ErrorContext(ctx, "compute-locations: delete failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to delete compute location")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *computeLocationHandler) toResponse(ctx context.Context, l store.ComputeLocation) computeLocationResponse {
	return computeLocationResponse{
		ID: l.ID, Name: l.Name, Description: l.Description,
		WorkerCount: h.onlineWorkerCount(ctx, l.Name),
		CreatedAt:   l.CreatedAt, UpdatedAt: l.UpdatedAt,
	}
}
