// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Authentication endpoints (Phase 3, component A1).
//
//	POST /api/v1/auth/login  — username+password -> Set-Cookie session (public)
//	POST /api/v1/auth/logout — revoke the current session (authenticated)
//	GET  /api/v1/auth/me     — current principal (authenticated)

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/auth/password"
	"github.com/uberware/sqi/internal/auth/session"
	"github.com/uberware/sqi/internal/store"
)

type authHandler struct {
	store        store.Store
	logger       *slog.Logger
	ttl          time.Duration
	cookieName   string
	cookieSecure string // "auto" | "true" | "false"
}

func newAuthHandler(st store.Store, logger *slog.Logger, ttl time.Duration, cookieName, cookieSecure string) *authHandler {
	if cookieName == "" {
		cookieName = session.DefaultCookieName
	}
	if ttl <= 0 {
		ttl = 168 * time.Hour
	}
	return &authHandler{store: st, logger: logger, ttl: ttl, cookieName: cookieName, cookieSecure: cookieSecure}
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type userResponse struct {
	ID          string    `json:"id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name,omitempty"`
	Role        string    `json:"role"`
	Disabled    bool      `json:"disabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func toUserResponse(u store.User) userResponse {
	return userResponse{
		ID:          u.ID,
		Username:    u.Username,
		DisplayName: u.DisplayName,
		Role:        u.Role,
		Disabled:    u.Disabled,
		CreatedAt:   u.CreatedAt,
		UpdatedAt:   u.UpdatedAt,
	}
}

// login verifies a username/password pair and, on success, mints a new
// server-side session and sets it as an HttpOnly cookie. Unknown username,
// wrong password, and disabled accounts all produce an identical 401 response
// so the endpoint cannot be used to enumerate valid usernames.
func (h *authHandler) login(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}

	const invalid = "invalid credentials"
	u, err := h.store.GetUserByUsername(ctx, req.Username)
	if err != nil {
		writeProblem(w, r, http.StatusUnauthorized, invalid)
		return
	}
	ok, verr := password.Verify(u.PasswordHash, req.Password)
	if verr != nil || !ok || u.Disabled {
		writeProblem(w, r, http.StatusUnauthorized, invalid)
		return
	}

	tok, err := password.GenerateToken()
	if err != nil {
		h.logger.ErrorContext(ctx, "auth: token generation failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to create session")
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
		// A conflict here is a hashed-token collision or a foreign-key
		// violation (the just-fetched user vanished mid-request) — either
		// way it is not a client error, so it is reported as a 500 rather
		// than reusing the CRUD-style 409 "already exists" message, which
		// would be misleading here.
		h.logger.ErrorContext(ctx, "auth: create session failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to create session")
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
	writeJSON(w, http.StatusOK, toUserResponse(u))
}

// logout revokes the caller's server-side session (if the cookie resolves to
// one) and clears the cookie regardless.
func (h *authHandler) logout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if c, err := r.Cookie(h.cookieName); err == nil && c.Value != "" {
		sess, serr := h.store.GetSessionByTokenHash(ctx, password.HashToken(c.Value), time.Now().UTC())
		if serr == nil {
			if derr := h.store.DeleteSession(ctx, sess.ID); derr != nil && !errors.Is(derr, store.ErrNotFound) {
				h.logger.WarnContext(ctx, "auth: delete session failed", slog.Any("error", derr))
			}
		}
	}
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is resolved dynamically by h.secure(r) from the 3-valued CookieSecure config; HttpOnly and SameSite are set explicitly
		Name:     h.cookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.secure(r),
		MaxAge:   -1,
	})
	w.WriteHeader(http.StatusNoContent)
}

type principalResponse struct {
	Subject     string   `json:"subject"`
	DisplayName string   `json:"display_name"`
	Roles       []string `json:"roles"`
	Kind        string   `json:"kind"`
}

// me returns the principal attached to the request context by middleware.Auth.
// It is a method (rather than a free function) purely so it can be registered
// the same way as login/logout (authH.me); it needs no handler state.
func (*authHandler) me(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		writeProblem(w, r, http.StatusUnauthorized, "authentication required")
		return
	}
	roles := p.Roles
	if roles == nil {
		roles = []string{}
	}
	writeJSON(w, http.StatusOK, principalResponse{
		Subject:     p.Subject,
		DisplayName: p.DisplayName,
		Roles:       roles,
		Kind:        string(p.Kind),
	})
}

// secure resolves the cookie's Secure attribute from the configured
// cookieSecure mode ("auto" | "true" | "false") and the request's scheme.
func (h *authHandler) secure(r *http.Request) bool {
	switch h.cookieSecure {
	case "true":
		return true
	case "false":
		return false
	default: // "auto"
		if r.TLS != nil {
			return true
		}
		return r.Header.Get("X-Forwarded-Proto") == "https"
	}
}
