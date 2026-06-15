// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Usage-pool REST handlers.
//
// Route summary:
//
//	POST   /api/v1/usage-pools      — create a usage pool
//	GET    /api/v1/usage-pools      — list all usage pools
//	GET    /api/v1/usage-pools/{id} — get pool by ID
//	PUT    /api/v1/usage-pools/{id} — replace mutable fields
//	DELETE /api/v1/usage-pools/{id} — delete pool

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
)

// usagePoolHandler handles all usage-pool REST endpoints.
type usagePoolHandler struct {
	store  store.Store
	logger *slog.Logger
}

// newUsagePoolHandler returns a usagePoolHandler wired to the given store.
func newUsagePoolHandler(st store.Store, logger *slog.Logger) *usagePoolHandler {
	return &usagePoolHandler{store: st, logger: logger}
}

// ── Wire-format types ─────────────────────────────────────────────────────────

// usagePoolResponse is the JSON representation of a usage pool.
type usagePoolResponse struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ServerHint    string `json:"server_hint,omitempty"`
	MaxConcurrent int    `json:"max_concurrent"`
	// InUse is the number of slots currently claimed (active, unreleased).
	InUse int `json:"in_use"`
	// Available is the number of free slots: max(max_concurrent - in_use, 0).
	Available int       `json:"available"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// createUsagePoolRequest is the body accepted by POST /api/v1/usage-pools.
type createUsagePoolRequest struct {
	Name          string `json:"name"`
	ServerHint    string `json:"server_hint"`
	MaxConcurrent int    `json:"max_concurrent"`
}

// updateUsagePoolRequest is the body accepted by PUT /api/v1/usage-pools/{id}.
type updateUsagePoolRequest struct {
	Name          string `json:"name"`
	ServerHint    string `json:"server_hint"`
	MaxConcurrent int    `json:"max_concurrent"`
}

// ── POST /api/v1/usage-pools ─────────────────────────────────────────────────

func (h *usagePoolHandler) createUsagePool(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req createUsagePoolRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if req.Name == "" {
		writeProblem(w, r, http.StatusBadRequest, "name is required")
		return
	}
	if req.MaxConcurrent <= 0 {
		writeProblem(w, r, http.StatusBadRequest, "max_concurrent must be greater than zero")
		return
	}

	now := time.Now().UTC()
	pool := store.UsagePool{
		ID:            uuid.NewString(),
		Name:          req.Name,
		ServerHint:    req.ServerHint,
		MaxConcurrent: req.MaxConcurrent,
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	created, err := h.store.CreateUsagePool(ctx, pool)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeProblem(w, r, http.StatusConflict, "a usage pool with that name already exists")
			return
		}
		h.logger.ErrorContext(ctx, "usage-pools: create failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to create usage pool")
		return
	}

	// A newly created pool has no claims yet, so in-use is 0 by construction —
	// no need to query for it.
	writeJSON(w, http.StatusCreated, toUsagePoolResponse(created, 0))
}

// ── GET /api/v1/usage-pools ──────────────────────────────────────────────────

func (h *usagePoolHandler) listUsagePools(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	usage, err := h.store.ListUsagePoolUtilization(ctx)
	if err != nil {
		h.logger.ErrorContext(ctx, "usage-pools: list failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to list usage pools")
		return
	}

	resp := make([]usagePoolResponse, len(usage))
	for i, u := range usage {
		resp[i] = toUsagePoolResponse(u.UsagePool, u.InUse)
	}

	writeJSON(w, http.StatusOK, resp)
}

// ── GET /api/v1/usage-pools/{id} ─────────────────────────────────────────────

func (h *usagePoolHandler) getUsagePool(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	pool, err := h.store.GetUsagePool(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "usage pool not found")
			return
		}
		h.logger.ErrorContext(ctx, "usage-pools: get failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to retrieve usage pool")
		return
	}

	writeJSON(w, http.StatusOK, toUsagePoolResponse(pool, h.poolInUse(ctx, pool.ID)))
}

// ── PUT /api/v1/usage-pools/{id} ─────────────────────────────────────────────

func (h *usagePoolHandler) updateUsagePool(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	var req updateUsagePoolRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if req.Name == "" {
		writeProblem(w, r, http.StatusBadRequest, "name is required")
		return
	}
	if req.MaxConcurrent <= 0 {
		writeProblem(w, r, http.StatusBadRequest, "max_concurrent must be greater than zero")
		return
	}

	pool := store.UsagePool{
		ID:            id,
		Name:          req.Name,
		ServerHint:    req.ServerHint,
		MaxConcurrent: req.MaxConcurrent,
	}

	updated, err := h.store.UpdateUsagePool(ctx, pool)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "usage pool not found")
			return
		}
		if errors.Is(err, store.ErrConflict) {
			writeProblem(w, r, http.StatusConflict, "a usage pool with that name already exists")
			return
		}
		h.logger.ErrorContext(ctx, "usage-pools: update failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to update usage pool")
		return
	}

	writeJSON(w, http.StatusOK, toUsagePoolResponse(updated, h.poolInUse(ctx, updated.ID)))
}

// ── DELETE /api/v1/usage-pools/{id} ──────────────────────────────────────────

func (h *usagePoolHandler) deleteUsagePool(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	if err := h.store.DeleteUsagePool(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "usage pool not found")
			return
		}
		h.logger.ErrorContext(ctx, "usage-pools: delete failed", slog.String("id", id), slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to delete usage pool")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ── Conversion helpers ────────────────────────────────────────────────────────

func toUsagePoolResponse(p store.UsagePool, inUse int) usagePoolResponse {
	available := max(p.MaxConcurrent-inUse, 0)
	return usagePoolResponse{
		ID:            p.ID,
		Name:          p.Name,
		ServerHint:    p.ServerHint,
		MaxConcurrent: p.MaxConcurrent,
		InUse:         inUse,
		Available:     available,
		CreatedAt:     p.CreatedAt,
		UpdatedAt:     p.UpdatedAt,
	}
}

// poolInUse returns the current active claim count for a pool. Usage is
// best-effort: on error it logs and returns 0 so a usage lookup never fails an
// otherwise-successful response (the response will then report the pool as fully
// available, which a subsequent refresh corrects).
func (h *usagePoolHandler) poolInUse(ctx context.Context, poolID string) int {
	n, err := h.store.ActiveClaimCount(ctx, poolID)
	if err != nil {
		h.logger.WarnContext(ctx, "usage-pools: active claim count failed",
			slog.String("id", poolID), slog.Any("error", err))
		return 0
	}
	return n
}
