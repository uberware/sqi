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

	"github.com/uberware/sqi/internal/auth"
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
	userID, ok := requireCallerSubject(w, r, selfServiceUnavailable)
	if !ok {
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
	// A failure here is logged, not surfaced: the password change already
	// succeeded and must not be reported as failed. The caller simply has to
	// log in again with the new password.
	if err := h.issueSession(w, r, u); err != nil {
		h.logger.ErrorContext(ctx, "selfservice: re-issue session failed", slog.Any("error", err))
	}
	w.WriteHeader(http.StatusNoContent)
}

// updateMeRequest carries only display_name — deliberately. `role`,
// `disabled`, and `username` are absent from this struct, so a request body
// carrying them is ignored by the decoder. Keeping them out of the type makes
// self-service privilege escalation unrepresentable rather than merely
// checked-for; do not widen this struct.
type updateMeRequest struct {
	DisplayName *string `json:"display_name"`
}

// updateMe changes the caller's own profile and returns the same principal
// shape GET /auth/me returns, so the client refreshes identity from one shape.
//
// The record is re-read and mutated field-by-field rather than built from the
// request: every field the caller may not touch keeps its stored value by
// construction.
func (h *authHandler) updateMe(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID, ok := requireCallerSubject(w, r, selfServiceUnavailable)
	if !ok {
		return
	}

	var req updateMeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}

	u, err := h.store.GetUser(ctx, userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "account not found")
			return
		}
		h.logger.ErrorContext(ctx, "selfservice: user lookup failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to update account")
		return
	}

	if req.DisplayName != nil {
		u.DisplayName = *req.DisplayName
	}
	updated, err := h.store.UpdateUser(ctx, u)
	if err != nil {
		h.logger.ErrorContext(ctx, "selfservice: update user failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to update account")
		return
	}

	// Sourced from the freshly-updated row rather than the request principal,
	// which still carries the pre-update values.
	p, _ := auth.FromContext(ctx)
	resp := toPrincipalResponse(p)
	resp.Username = updated.Username
	resp.DisplayName = updated.DisplayName
	writeJSON(w, http.StatusOK, resp)
}
