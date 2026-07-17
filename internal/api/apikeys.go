// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// API-key REST handlers (Phase 3, component A2). Self-scoped to the caller's
// user until B1 adds admin-broad visibility. The raw key is returned exactly
// once, in the create response; it is never stored in clear or logged.
//
//	POST   /api/v1/api-keys        — issue a key for the caller (secret shown once)
//	GET    /api/v1/api-keys        — list the caller's keys (no secret)
//	DELETE /api/v1/api-keys/{id}   — revoke one of the caller's keys

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/auth/password"
	"github.com/uberware/sqi/internal/store"
)

// apiKeyPrefixLen is how many leading chars of the raw key are stored for
// list identification. A prefix of 32 random bytes cannot be brute-forced.
const apiKeyPrefixLen = 12

type apiKeysHandler struct {
	store  store.Store
	logger *slog.Logger
}

func newAPIKeysHandler(st store.Store, logger *slog.Logger) *apiKeysHandler {
	return &apiKeysHandler{store: st, logger: logger}
}

type apiKeyCreateRequest struct {
	Name      string     `json:"name"`
	ExpiresAt *time.Time `json:"expires_at"`
}

type apiKeyResponse struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// apiKeyCreatedResponse is the create-only shape carrying the raw secret.
type apiKeyCreatedResponse struct {
	apiKeyResponse

	Secret string `json:"secret"`
}

func toAPIKeyResponse(k store.APIKey) apiKeyResponse {
	return apiKeyResponse{
		ID: k.ID, Name: k.Name, Prefix: k.Prefix,
		ExpiresAt: k.ExpiresAt, LastUsedAt: k.LastUsedAt, CreatedAt: k.CreatedAt,
	}
}

// callerSubject returns the authenticated user id, or ("", false) when the
// principal is anonymous (auth disabled) — in which case API-key management is
// inert (no real user to own the key).
func callerSubject(r *http.Request) (string, bool) {
	p, ok := auth.FromContext(r.Context())
	if !ok || p.Kind == auth.KindAnonymous || p.Subject == "" {
		return "", false
	}
	return p.Subject, true
}

func (h *apiKeysHandler) create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := callerSubject(r)
	if !ok {
		writeProblem(w, r, http.StatusConflict, "API keys require authentication to be enabled")
		return
	}
	var req apiKeyCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeProblem(w, r, http.StatusBadRequest, "name is required")
		return
	}
	now := time.Now().UTC()
	if req.ExpiresAt != nil && !req.ExpiresAt.After(now) {
		writeProblem(w, r, http.StatusBadRequest, "expires_at must be in the future")
		return
	}
	raw, err := password.GenerateToken()
	if err != nil {
		h.logger.ErrorContext(ctx, "apikeys: token generation failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to create API key")
		return
	}
	raw = "sqi_" + raw
	created, err := h.store.CreateAPIKey(ctx, store.APIKey{
		ID: uuid.NewString(), UserID: userID, Name: req.Name,
		Prefix: raw[:apiKeyPrefixLen], TokenHash: password.HashToken(raw),
		ExpiresAt: req.ExpiresAt, CreatedAt: now,
	})
	if err != nil {
		h.logger.ErrorContext(ctx, "apikeys: create failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to create API key")
		return
	}
	writeJSON(w, http.StatusCreated, apiKeyCreatedResponse{
		apiKeyResponse: toAPIKeyResponse(created),
		Secret:         raw,
	})
}

func (h *apiKeysHandler) list(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := callerSubject(r)
	if !ok {
		writeProblem(w, r, http.StatusConflict, "API keys require authentication to be enabled")
		return
	}
	keys, err := h.store.ListAPIKeysForUser(ctx, userID)
	if err != nil {
		h.logger.ErrorContext(ctx, "apikeys: list failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to list API keys")
		return
	}
	resp := make([]apiKeyResponse, len(keys))
	for i, k := range keys {
		resp[i] = toAPIKeyResponse(k)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *apiKeysHandler) revoke(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := callerSubject(r)
	if !ok {
		writeProblem(w, r, http.StatusConflict, "API keys require authentication to be enabled")
		return
	}
	err := h.store.RevokeAPIKey(ctx, chi.URLParam(r, "id"), userID, time.Now().UTC())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "API key not found")
			return
		}
		h.logger.ErrorContext(ctx, "apikeys: revoke failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to revoke API key")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
