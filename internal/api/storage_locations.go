// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Storage-location REST handlers — task 81.
//
// Route summary:
//
//	POST   /api/v1/storage-locations      — create a storage location
//	GET    /api/v1/storage-locations      — list all storage locations
//	GET    /api/v1/storage-locations/{id} — get location by ID
//	PUT    /api/v1/storage-locations/{id} — replace mutable fields
//	DELETE /api/v1/storage-locations/{id} — delete location

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

// storageLocationHandler handles all storage-location REST endpoints.
type storageLocationHandler struct {
	store  store.Store
	logger *slog.Logger
}

// newStorageLocationHandler returns a storageLocationHandler wired to the given store.
func newStorageLocationHandler(st store.Store, logger *slog.Logger) *storageLocationHandler {
	return &storageLocationHandler{store: st, logger: logger}
}

// ── Wire-format types ─────────────────────────────────────────────────────────

// storageLocationResponse is the JSON representation of a storage location.
type storageLocationResponse struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	Description string            `json:"description,omitempty"`
	Roots       map[string]string `json:"roots"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// createStorageLocationRequest is the body accepted by POST /api/v1/storage-locations.
type createStorageLocationRequest struct {
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	Description string            `json:"description"`
	Roots       map[string]string `json:"roots"`
}

// updateStorageLocationRequest is the body accepted by PUT /api/v1/storage-locations/{id}.
type updateStorageLocationRequest struct {
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	Description string            `json:"description"`
	Roots       map[string]string `json:"roots"`
}

// ── POST /api/v1/storage-locations ───────────────────────────────────────────

func (h *storageLocationHandler) createStorageLocation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req createStorageLocationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if req.Name == "" {
		writeProblem(w, r, http.StatusBadRequest, "name is required")
		return
	}
	if !isValidStorageLocationType(req.Type) {
		writeProblem(w, r, http.StatusBadRequest, `type must be "filesystem" or "s3"`)
		return
	}

	now := time.Now().UTC()
	loc := store.StorageLocation{
		ID:          uuid.NewString(),
		Name:        req.Name,
		Type:        store.StorageLocationType(req.Type),
		Description: req.Description,
		Roots:       req.Roots,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	created, err := h.store.CreateStorageLocation(ctx, loc)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeProblem(w, r, http.StatusConflict, "a storage location with that name already exists")
			return
		}
		h.logger.ErrorContext(ctx, "storage-locations: create failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to create storage location")
		return
	}

	writeJSON(w, http.StatusCreated, toStorageLocationResponse(created))
}

// ── GET /api/v1/storage-locations ────────────────────────────────────────────

func (h *storageLocationHandler) listStorageLocations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	locs, err := h.store.ListStorageLocations(ctx)
	if err != nil {
		h.logger.ErrorContext(ctx, "storage-locations: list failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to list storage locations")
		return
	}

	resp := make([]storageLocationResponse, len(locs))
	for i, l := range locs {
		resp[i] = toStorageLocationResponse(l)
	}

	writeJSON(w, http.StatusOK, resp)
}

// ── GET /api/v1/storage-locations/{id} ───────────────────────────────────────

func (h *storageLocationHandler) getStorageLocation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	loc, err := h.store.GetStorageLocation(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "storage location not found")
			return
		}
		h.logger.ErrorContext(ctx, "storage-locations: get failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to retrieve storage location")
		return
	}

	writeJSON(w, http.StatusOK, toStorageLocationResponse(loc))
}

// ── PUT /api/v1/storage-locations/{id} ───────────────────────────────────────

func (h *storageLocationHandler) updateStorageLocation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	var req updateStorageLocationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if req.Name == "" {
		writeProblem(w, r, http.StatusBadRequest, "name is required")
		return
	}
	if !isValidStorageLocationType(req.Type) {
		writeProblem(w, r, http.StatusBadRequest, `type must be "filesystem" or "s3"`)
		return
	}

	loc := store.StorageLocation{
		ID:          id,
		Name:        req.Name,
		Type:        store.StorageLocationType(req.Type),
		Description: req.Description,
		Roots:       req.Roots,
	}

	updated, err := h.store.UpdateStorageLocation(ctx, loc)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "storage location not found")
			return
		}
		if errors.Is(err, store.ErrConflict) {
			writeProblem(w, r, http.StatusConflict, "a storage location with that name already exists")
			return
		}
		h.logger.ErrorContext(ctx, "storage-locations: update failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to update storage location")
		return
	}

	writeJSON(w, http.StatusOK, toStorageLocationResponse(updated))
}

// ── DELETE /api/v1/storage-locations/{id} ────────────────────────────────────

func (h *storageLocationHandler) deleteStorageLocation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	if err := h.store.DeleteStorageLocation(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "storage location not found")
			return
		}
		h.logger.ErrorContext(ctx, "storage-locations: delete failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to delete storage location")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ── Conversion helpers ────────────────────────────────────────────────────────

func toStorageLocationResponse(l store.StorageLocation) storageLocationResponse {
	roots := l.Roots
	if roots == nil {
		roots = map[string]string{}
	}
	return storageLocationResponse{
		ID:          l.ID,
		Name:        l.Name,
		Type:        string(l.Type),
		Description: l.Description,
		Roots:       roots,
		CreatedAt:   l.CreatedAt,
		UpdatedAt:   l.UpdatedAt,
	}
}

// isValidStorageLocationType returns true for the two recognized type values.
func isValidStorageLocationType(t string) bool {
	return t == string(store.StorageLocationTypeFilesystem) || t == string(store.StorageLocationTypeS3)
}
