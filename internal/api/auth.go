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
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/auth/password"
	"github.com/uberware/sqi/internal/auth/policy"
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

// dummyVerifyPlaintext is an arbitrary throwaway string used only to derive
// dummyHash below; it has no security meaning of its own.
const dummyVerifyPlaintext = "sqi-auth-timing-equalization-dummy"

// dummyHash is a real, valid argon2id encoded hash — produced by
// password.Hash itself, not a hardcoded literal — computed once on first
// use. It exists solely so login's unknown-user path can run a genuine
// argon2id derivation of matching cost to a real password check; see the
// comment in login for why. Deriving it via password.Hash (rather than
// hardcoding the encoded string) means it automatically tracks the
// package's argon2 parameters if they are ever raised, instead of silently
// falling out of sync and reopening the timing side channel.
//
// Computed lazily (not at package init) so the ~19 MiB / 30-60 ms derivation
// is not paid at startup by auth-disabled servers, or by every test binary
// that imports this package.
var dummyHash = sync.OnceValue(mustDummyHash)

func mustDummyHash() string {
	h, err := password.Hash(dummyVerifyPlaintext)
	if err != nil {
		// password.Hash only fails if crypto/rand.Read fails, i.e. the
		// process' entropy source is broken. That's not something a single
		// request can recover from, so fail fast at startup instead of
		// leaving dummyHash empty and silently breaking the timing fix.
		panic("auth: failed to precompute dummy password hash: " + err.Error())
	}
	return h
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
		if !errors.Is(err, store.ErrNotFound) {
			// A real store/infrastructure failure (as opposed to "no such
			// user") deserves a server-side signal — otherwise a database
			// outage looks identical to a mistyped password with no way to
			// tell them apart from the logs. This must NOT change what the
			// client sees: the response below is the same either way.
			h.logger.WarnContext(ctx, "auth: user lookup failed", slog.Any("error", err))
		}
		// Run a dummy argon2id verify so this path costs about the same as
		// the known-user path below, which always calls password.Verify.
		// Without this, returning immediately here makes an unknown-username
		// request measurably faster than a known-username one — a timing
		// side channel that lets an attacker enumerate valid usernames even
		// though the response bodies are byte-identical. Do not remove this
		// as a "wasted" call.
		if _, verr := password.Verify(dummyHash(), req.Password); verr != nil {
			h.logger.WarnContext(ctx, "auth: dummy verify failed unexpectedly", slog.Any("error", verr))
		}
		writeProblem(w, r, http.StatusUnauthorized, invalid)
		return
	}
	ok, verr := password.Verify(u.PasswordHash, req.Password)
	if verr != nil || !ok || u.Disabled {
		writeProblem(w, r, http.StatusUnauthorized, invalid)
		return
	}

	// A failure here is a token-generation problem, a hashed-token collision,
	// or a foreign-key violation (the just-fetched user vanished mid-request)
	// — none are client errors, so this is a 500 rather than the CRUD-style
	// 409 "already exists", which would be misleading.
	if err := h.issueSession(w, r, u); err != nil {
		h.logger.ErrorContext(ctx, "auth: create session failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to create session")
		return
	}
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

// issueSession mints a session for u and sets the session cookie. It is the
// single place session cookies are minted — login and changePassword both go
// through it, so the cookie attribute set cannot drift between them.
func (h *authHandler) issueSession(w http.ResponseWriter, r *http.Request, u store.User) error {
	ctx := r.Context()
	tok, err := password.GenerateToken()
	if err != nil {
		return fmt.Errorf("generate session token: %w", err)
	}
	now := time.Now().UTC()
	if _, err := h.store.CreateSession(ctx, store.Session{
		ID:        uuid.NewString(),
		TokenHash: password.HashToken(tok),
		UserID:    u.ID,
		ExpiresAt: now.Add(h.ttl),
		CreatedAt: now,
	}); err != nil {
		return fmt.Errorf("create session: %w", err)
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
	return nil
}

type principalResponse struct {
	Subject     string   `json:"subject"`
	Username    string   `json:"username,omitempty"`
	DisplayName string   `json:"display_name"`
	Roles       []string `json:"roles"`
	Permissions []string `json:"permissions"`
	Kind        string   `json:"kind"`
}

// toPrincipalResponse maps a principal to the wire shape /auth/me returns.
// Both /auth/me and PATCH /auth/me go through it so the two can't drift.
func toPrincipalResponse(p auth.Principal) principalResponse {
	roles := p.Roles
	if roles == nil {
		roles = []string{}
	}
	return principalResponse{
		Subject:     p.Subject,
		Username:    p.Username,
		DisplayName: p.DisplayName,
		Roles:       roles,
		Permissions: policy.PermissionsFor(p),
		Kind:        string(p.Kind),
	}
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
	writeJSON(w, http.StatusOK, toPrincipalResponse(p))
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
