// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Self-service account endpoints (Phase 3, component B3). Both routes resolve
// their target from the authenticated principal — there is no id in the path,
// so cross-user access is structurally impossible rather than guarded against.
//
//	PUT   /api/v1/auth/password — change own password (re-issues the session)
//	PATCH /api/v1/auth/me       — change own display name

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/auth/password"
	"github.com/uberware/sqi/internal/store"
)

// selfServiceUnavailable is returned when auth is disabled: the anonymous
// superuser has no user record to act on. Mirrors the /api-keys precedent.
const selfServiceUnavailable = "self-service account changes require authentication to be enabled"

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// changePassword verifies the caller's current password, sets the new one,
// destroys every session for the account, and issues a fresh session for this
// caller so the active device stays logged in while others are evicted.
//
// A wrong current password is 403, not 401: the caller IS authenticated, and
// the web installs a 401 -> login interceptor that would eject them mid-form.
func (h *authHandler) changePassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := callerSubject(r)
	if !ok {
		writeProblem(w, r, http.StatusConflict, selfServiceUnavailable)
		return
	}

	var req changePasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.NewPassword == "" {
		writeProblem(w, r, http.StatusBadRequest, "new_password is required")
		return
	}

	u, err := h.store.GetUser(ctx, userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "account not found")
			return
		}
		h.logger.ErrorContext(ctx, "selfservice: user lookup failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to change password")
		return
	}

	valid, verr := password.Verify(u.PasswordHash, req.CurrentPassword)
	if verr != nil || !valid {
		writeProblem(w, r, http.StatusForbidden, "current password is incorrect")
		return
	}

	hash, err := password.Hash(req.NewPassword)
	if err != nil {
		h.logger.ErrorContext(ctx, "selfservice: hash failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to change password")
		return
	}
	if err := h.store.SetUserPassword(ctx, u.ID, hash); err != nil {
		h.logger.ErrorContext(ctx, "selfservice: set password failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to change password")
		return
	}

	// Evict every session for this account, then re-issue one for the caller.
	// API keys are deliberately NOT revoked: they are an independent
	// credential, and silently killing a user's automation because they
	// rotated a password would be a nasty surprise.
	if err := h.store.DeleteSessionsForUser(ctx, u.ID); err != nil {
		h.logger.WarnContext(ctx, "selfservice: delete sessions failed", slog.Any("error", err))
	}
	h.issueSession(w, r, u)
	w.WriteHeader(http.StatusNoContent)
}

// issueSession mints a session for u and sets the cookie, matching login's
// cookie attributes. A failure here is logged rather than surfaced: the
// password change already succeeded and must not be reported as failed — the
// caller simply falls back to logging in again.
func (h *authHandler) issueSession(w http.ResponseWriter, r *http.Request, u store.User) {
	ctx := r.Context()
	tok, err := password.GenerateToken()
	if err != nil {
		h.logger.ErrorContext(ctx, "selfservice: token generation failed", slog.Any("error", err))
		return
	}
	now := time.Now().UTC()
	if _, err := h.store.CreateSession(ctx, store.Session{
		ID:        uuid.NewString(),
		TokenHash: password.HashToken(tok),
		UserID:    u.ID,
		ExpiresAt: now.Add(h.ttl),
		CreatedAt: now,
	}); err != nil {
		h.logger.ErrorContext(ctx, "selfservice: create session failed", slog.Any("error", err))
		return
	}
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is resolved dynamically by h.secure(r) from the 3-valued CookieSecure config; HttpOnly and SameSite are set explicitly
		Name:     h.cookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.secure(r),
		MaxAge:   int(h.ttl.Seconds()),
	})
}
