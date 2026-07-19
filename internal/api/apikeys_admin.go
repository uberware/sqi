// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Admin-scoped API-key endpoints (Phase 3, component B3), gated on
// apikeys.admin. Admins may list and revoke another user's keys but never
// create one: minting a credential someone else is accountable for is a
// materially different act from revoking one.
//
//	GET    /api/v1/users/{id}/api-keys          — list that user's keys
//	DELETE /api/v1/users/{id}/api-keys/{keyId}  — revoke one of that user's keys

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/uberware/sqi/internal/store"
)

func (h *apiKeysHandler) listForUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := chi.URLParam(r, "id")

	keys, err := h.store.ListAPIKeysForUser(ctx, userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "user not found")
			return
		}
		h.logger.ErrorContext(ctx, "apikeys: admin list failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to list API keys")
		return
	}

	out := make([]apiKeyResponse, 0, len(keys))
	for _, k := range keys {
		out = append(out, toAPIKeyResponse(k))
	}
	writeJSON(w, http.StatusOK, out)
}

// revokeForUser revokes a key scoped to the named user. RevokeAPIKey is
// already owner-scoped, so a key id that does not belong to this user returns
// ErrNotFound -> 404; that existing scoping is the authorization check.
func (h *apiKeysHandler) revokeForUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := chi.URLParam(r, "id")
	keyID := chi.URLParam(r, "keyId")

	if err := h.store.RevokeAPIKey(ctx, keyID, userID, time.Now().UTC()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "API key not found")
			return
		}
		h.logger.ErrorContext(ctx, "apikeys: admin revoke failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to revoke API key")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
