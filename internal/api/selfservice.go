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
	if u.AuthSource != store.AuthSourceLocal {
		// Falling through would compare the caller's password against the
		// external placeholder hash and answer "current password is incorrect"
		// — accurate but misleading, since no password would ever work here.
		writeProblem(w, r, http.StatusConflict, externalPasswordConflictDetail(u))
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
	// The new password and the session eviction land together or not at all.
	// The client reports "other devices have been signed out" on success, so
	// a partial result here would make that message a lie with nothing to
	// detect it — exactly the wrong outcome for someone rotating a password
	// after a suspected compromise. On failure nothing changed and the caller
	// can safely retry.
	//
	// API keys are deliberately NOT revoked: they are an independent
	// credential, and silently killing a user's automation because they
	// rotated a password would be a nasty surprise.
	if err := h.store.SetUserPasswordAndEvictSessions(ctx, u.ID, hash); err != nil {
		h.logger.ErrorContext(ctx, "selfservice: set password failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to change password")
		return
	}

	// The password is changed and every session is gone, including the
	// caller's. Re-issuing one keeps this device signed in; if it fails the
	// change still stands, so this is logged rather than surfaced — the
	// caller simply logs in again with the new password.
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

	if req.DisplayName == nil {
		// Nothing to change; report the caller's current identity unchanged.
		p, _ := auth.FromContext(ctx)
		writeJSON(w, http.StatusOK, toPrincipalResponse(p))
		return
	}

	// A targeted single-column update, NOT a read-modify-write through
	// UpdateUser: that writes display_name, role, and disabled together, so a
	// save racing a concurrent admin demotion or disable would write the
	// stale values back and silently revert it.
	updated, err := h.store.SetUserDisplayName(ctx, userID, *req.DisplayName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "account not found")
			return
		}
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
