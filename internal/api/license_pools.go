// SPDX-License-Identifier: AGPL-3.0-only

package api

// License-pool REST handlers — task 81.
//
// Route summary:
//
//	POST   /api/v1/license-pools      — create a license pool
//	GET    /api/v1/license-pools      — list all license pools
//	GET    /api/v1/license-pools/{id} — get pool by ID
//	PUT    /api/v1/license-pools/{id} — replace mutable fields
//	DELETE /api/v1/license-pools/{id} — delete pool

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

// licensePoolHandler handles all license-pool REST endpoints.
type licensePoolHandler struct {
	store  store.Store
	logger *slog.Logger
}

// newLicensePoolHandler returns a licensePoolHandler wired to the given store.
func newLicensePoolHandler(st store.Store, logger *slog.Logger) *licensePoolHandler {
	return &licensePoolHandler{store: st, logger: logger}
}

// ── Wire-format types ─────────────────────────────────────────────────────────

// licensePoolResponse is the JSON representation of a license pool.
type licensePoolResponse struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Product       string    `json:"product"`
	ServerHint    string    `json:"server_hint,omitempty"`
	MaxConcurrent int       `json:"max_concurrent"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// createLicensePoolRequest is the body accepted by POST /api/v1/license-pools.
type createLicensePoolRequest struct {
	Name          string `json:"name"`
	Product       string `json:"product"`
	ServerHint    string `json:"server_hint"`
	MaxConcurrent int    `json:"max_concurrent"`
}

// updateLicensePoolRequest is the body accepted by PUT /api/v1/license-pools/{id}.
type updateLicensePoolRequest struct {
	Name          string `json:"name"`
	Product       string `json:"product"`
	ServerHint    string `json:"server_hint"`
	MaxConcurrent int    `json:"max_concurrent"`
}

// ── POST /api/v1/license-pools ───────────────────────────────────────────────

func (h *licensePoolHandler) createLicensePool(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req createLicensePoolRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if req.Name == "" {
		writeProblem(w, r, http.StatusBadRequest, "name is required")
		return
	}
	if req.Product == "" {
		writeProblem(w, r, http.StatusBadRequest, "product is required")
		return
	}
	if req.MaxConcurrent <= 0 {
		writeProblem(w, r, http.StatusBadRequest, "max_concurrent must be greater than zero")
		return
	}

	now := time.Now().UTC()
	pool := store.LicensePool{
		ID:            uuid.NewString(),
		Name:          req.Name,
		Product:       req.Product,
		ServerHint:    req.ServerHint,
		MaxConcurrent: req.MaxConcurrent,
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	created, err := h.store.CreateLicensePool(ctx, pool)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeProblem(w, r, http.StatusConflict, "a license pool with that name already exists")
			return
		}
		h.logger.ErrorContext(ctx, "license-pools: create failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to create license pool")
		return
	}

	writeJSON(w, http.StatusCreated, toLicensePoolResponse(created))
}

// ── GET /api/v1/license-pools ────────────────────────────────────────────────

func (h *licensePoolHandler) listLicensePools(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	pools, err := h.store.ListLicensePools(ctx)
	if err != nil {
		h.logger.ErrorContext(ctx, "license-pools: list failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to list license pools")
		return
	}

	resp := make([]licensePoolResponse, len(pools))
	for i, p := range pools {
		resp[i] = toLicensePoolResponse(p)
	}

	writeJSON(w, http.StatusOK, resp)
}

// ── GET /api/v1/license-pools/{id} ───────────────────────────────────────────

func (h *licensePoolHandler) getLicensePool(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	pool, err := h.store.GetLicensePool(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "license pool not found")
			return
		}
		h.logger.ErrorContext(ctx, "license-pools: get failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to retrieve license pool")
		return
	}

	writeJSON(w, http.StatusOK, toLicensePoolResponse(pool))
}

// ── PUT /api/v1/license-pools/{id} ───────────────────────────────────────────

func (h *licensePoolHandler) updateLicensePool(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	var req updateLicensePoolRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if req.Name == "" {
		writeProblem(w, r, http.StatusBadRequest, "name is required")
		return
	}
	if req.Product == "" {
		writeProblem(w, r, http.StatusBadRequest, "product is required")
		return
	}
	if req.MaxConcurrent <= 0 {
		writeProblem(w, r, http.StatusBadRequest, "max_concurrent must be greater than zero")
		return
	}

	pool := store.LicensePool{
		ID:            id,
		Name:          req.Name,
		Product:       req.Product,
		ServerHint:    req.ServerHint,
		MaxConcurrent: req.MaxConcurrent,
	}

	updated, err := h.store.UpdateLicensePool(ctx, pool)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "license pool not found")
			return
		}
		if errors.Is(err, store.ErrConflict) {
			writeProblem(w, r, http.StatusConflict, "a license pool with that name already exists")
			return
		}
		h.logger.ErrorContext(ctx, "license-pools: update failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to update license pool")
		return
	}

	writeJSON(w, http.StatusOK, toLicensePoolResponse(updated))
}

// ── DELETE /api/v1/license-pools/{id} ────────────────────────────────────────

func (h *licensePoolHandler) deleteLicensePool(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	if err := h.store.DeleteLicensePool(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "license pool not found")
			return
		}
		h.logger.ErrorContext(ctx, "license-pools: delete failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to delete license pool")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ── Conversion helpers ────────────────────────────────────────────────────────

func toLicensePoolResponse(p store.LicensePool) licensePoolResponse {
	return licensePoolResponse{
		ID:            p.ID,
		Name:          p.Name,
		Product:       p.Product,
		ServerHint:    p.ServerHint,
		MaxConcurrent: p.MaxConcurrent,
		CreatedAt:     p.CreatedAt,
		UpdatedAt:     p.UpdatedAt,
	}
}
