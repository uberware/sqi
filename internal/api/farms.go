// SPDX-License-Identifier: AGPL-3.0-only

package api

// Farm REST handlers — task 81.
//
// Route summary:
//
//	POST   /api/v1/farms      — create a farm
//	GET    /api/v1/farms      — list all farms
//	GET    /api/v1/farms/{id} — get farm by ID
//	PUT    /api/v1/farms/{id} — replace mutable fields
//	DELETE /api/v1/farms/{id} — delete farm

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

// farmHandler handles all farm-related REST endpoints.
type farmHandler struct {
	store  store.Store
	logger *slog.Logger
}

// newFarmHandler returns a farmHandler wired to the given store.
func newFarmHandler(st store.Store, logger *slog.Logger) *farmHandler {
	return &farmHandler{store: st, logger: logger}
}

// ── Wire-format types ─────────────────────────────────────────────────────────

// farmResponse is the JSON representation of a farm.
type farmResponse struct {
	ID                 string    `json:"id"`
	Name               string    `json:"name"`
	Description        string    `json:"description,omitempty"`
	MaxConcurrentTasks int       `json:"max_concurrent_tasks"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// createFarmRequest is the body accepted by POST /api/v1/farms.
type createFarmRequest struct {
	Name               string `json:"name"`
	Description        string `json:"description"`
	MaxConcurrentTasks int    `json:"max_concurrent_tasks"`
}

// updateFarmRequest is the body accepted by PUT /api/v1/farms/{id}.
type updateFarmRequest struct {
	Name               string `json:"name"`
	Description        string `json:"description"`
	MaxConcurrentTasks int    `json:"max_concurrent_tasks"`
}

// ── POST /api/v1/farms ────────────────────────────────────────────────────────

func (h *farmHandler) createFarm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req createFarmRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if req.Name == "" {
		writeProblem(w, r, http.StatusBadRequest, "name is required")
		return
	}

	now := time.Now().UTC()
	farm := store.Farm{
		ID:                 uuid.NewString(),
		Name:               req.Name,
		Description:        req.Description,
		MaxConcurrentTasks: req.MaxConcurrentTasks,
		CreatedAt:          now,
		UpdatedAt:          now,
	}

	created, err := h.store.CreateFarm(ctx, farm)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeProblem(w, r, http.StatusConflict, "a farm with that name already exists")
			return
		}
		h.logger.ErrorContext(ctx, "farms: create failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to create farm")
		return
	}

	writeJSON(w, http.StatusCreated, toFarmResponse(created))
}

// ── GET /api/v1/farms ─────────────────────────────────────────────────────────

func (h *farmHandler) listFarms(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	farms, err := h.store.ListFarms(ctx)
	if err != nil {
		h.logger.ErrorContext(ctx, "farms: list failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to list farms")
		return
	}

	resp := make([]farmResponse, len(farms))
	for i, f := range farms {
		resp[i] = toFarmResponse(f)
	}

	writeJSON(w, http.StatusOK, resp)
}

// ── GET /api/v1/farms/{id} ────────────────────────────────────────────────────

func (h *farmHandler) getFarm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	farm, err := h.store.GetFarm(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "farm not found")
			return
		}
		h.logger.ErrorContext(ctx, "farms: get failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to retrieve farm")
		return
	}

	writeJSON(w, http.StatusOK, toFarmResponse(farm))
}

// ── PUT /api/v1/farms/{id} ────────────────────────────────────────────────────

func (h *farmHandler) updateFarm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	var req updateFarmRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if req.Name == "" {
		writeProblem(w, r, http.StatusBadRequest, "name is required")
		return
	}

	farm := store.Farm{
		ID:                 id,
		Name:               req.Name,
		Description:        req.Description,
		MaxConcurrentTasks: req.MaxConcurrentTasks,
	}

	updated, err := h.store.UpdateFarm(ctx, farm)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "farm not found")
			return
		}
		if errors.Is(err, store.ErrConflict) {
			writeProblem(w, r, http.StatusConflict, "a farm with that name already exists")
			return
		}
		h.logger.ErrorContext(ctx, "farms: update failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to update farm")
		return
	}

	writeJSON(w, http.StatusOK, toFarmResponse(updated))
}

// ── DELETE /api/v1/farms/{id} ─────────────────────────────────────────────────

func (h *farmHandler) deleteFarm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	if err := h.store.DeleteFarm(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "farm not found")
			return
		}
		h.logger.ErrorContext(ctx, "farms: delete failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to delete farm")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ── Conversion helpers ────────────────────────────────────────────────────────

func toFarmResponse(f store.Farm) farmResponse {
	return farmResponse{
		ID:                 f.ID,
		Name:               f.Name,
		Description:        f.Description,
		MaxConcurrentTasks: f.MaxConcurrentTasks,
		CreatedAt:          f.CreatedAt,
		UpdatedAt:          f.UpdatedAt,
	}
}
