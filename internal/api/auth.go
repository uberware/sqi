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
	"github.com/uberware/sqi/internal/auth/ldap"
	"github.com/uberware/sqi/internal/auth/oidc"
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
	// ldapVerifier is nil when directory auth is disabled; ldapCfg is only
	// read when it is non-nil. See ldaplogin.go.
	ldapVerifier ldap.Verifier
	ldapCfg      ldap.Config
	// oidcProvider is nil when SSO is disabled; oidcCfg and oidcStateKey are
	// only read when it is non-nil. See oidclogin.go.
	oidcProvider oidc.Provider
	oidcCfg      oidc.Config
	oidcStateKey []byte
}

// authHandlerDeps is what newAuthHandler needs. It is a struct rather than a
// parameter list because the list had grown past the point where a caller could
// get the order right by reading it — two adjacent strings (cookieName,
// cookieSecure) and two adjacent nilable backends are exactly the shape that
// silently transposes.
type authHandlerDeps struct {
	Store        store.Store
	Logger       *slog.Logger
	TTL          time.Duration
	CookieName   string
	CookieSecure string // "auto" | "true" | "false"
	LDAPVerifier ldap.Verifier
	LDAPConfig   ldap.Config
	OIDCProvider oidc.Provider
	OIDCConfig   oidc.Config
	OIDCStateKey []byte
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
// is not paid by auth-disabled servers or by every test binary that imports
// this package. An auth-enabled router forces it at construction via
// warmDummyHash — deferring it to the first request would make the first
// unknown-username login after each restart measurably slower than a
// known-username one, which is the very timing signal this exists to erase,
// and would turn mustDummyHash's fail-fast panic into a 500 on a live
// request instead of a startup abort.
var dummyHash = sync.OnceValue(mustDummyHash)

// warmDummyHash forces the dummy-hash derivation before the server accepts
// traffic. Call it from router construction when auth is enabled.
func warmDummyHash() { _ = dummyHash() }

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

func newAuthHandler(d authHandlerDeps) *authHandler {
	if d.CookieName == "" {
		d.CookieName = session.DefaultCookieName
	}
	if d.TTL <= 0 {
		d.TTL = 168 * time.Hour
	}
	return &authHandler{
		store:        d.Store,
		logger:       d.Logger,
		ttl:          d.TTL,
		cookieName:   d.CookieName,
		cookieSecure: d.CookieSecure,
		ldapVerifier: d.LDAPVerifier,
		ldapCfg:      d.LDAPConfig,
		oidcProvider: d.OIDCProvider,
		oidcCfg:      d.OIDCConfig,
		oidcStateKey: d.OIDCStateKey,
	}
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type userResponse struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name,omitempty"`
	Role        string `json:"role"`
	AuthSource  string `json:"auth_source"`
	// RoleEditable tells the client whether PATCH /users/{id} will accept a
	// role change for this account. It is computed server-side, from the same
	// predicate that guards the PATCH, precisely so no client has to
	// reconstruct the two-condition rule (auth_source AND role_source) — the
	// role_source half is not otherwise exposed by any endpoint, so a client
	// inferring from auth_source alone gets role_source=local wrong.
	RoleEditable bool      `json:"role_editable"`
	Disabled     bool      `json:"disabled"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// toUserResponse renders u for the wire. ldapRoleSource and oidcRoleSource are
// the active auth.ldap.role_source and auth.oidc.role_source; they are threaded
// in rather than read from a global so the values always come from the handler
// that owns the request.
func toUserResponse(u store.User, ldapRoleSource, oidcRoleSource string) userResponse {
	return userResponse{
		ID:           u.ID,
		Username:     u.Username,
		DisplayName:  u.DisplayName,
		Role:         u.Role,
		AuthSource:   u.AuthSource,
		RoleEditable: !directoryOwnsRole(u, ldapRoleSource, oidcRoleSource),
		Disabled:     u.Disabled,
		CreatedAt:    u.CreatedAt,
		UpdatedAt:    u.UpdatedAt,
	}
}

// invalidLoginDetail is the single detail string every failed login returns,
// whatever the reason. Unknown username, wrong local password, wrong directory
// password, an unreachable directory, no role mapping, and a username
// collision must be indistinguishable to the client — any difference turns
// login into a user-enumeration oracle.
const invalidLoginDetail = "invalid credentials"

// login verifies a username/password pair and, on success, mints a new
// server-side session and sets it as an HttpOnly cookie.
//
// Credentials are verified by the backend named in the account's AuthSource:
// the stored argon2id hash for local accounts, the directory for LDAP ones.
// OIDC accounts have no password backend at all and are refused here.
// A local account is never sent to the directory, not even on a wrong
// password — every failed login would otherwise cost a network round trip,
// and in Active Directory repeated bad binds against a real DN can lock the
// *directory* account, turning a brute force against sqi into an org-wide
// denial of service.
func (h *authHandler) login(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}

	u, err := h.store.GetUserByUsername(ctx, req.Username)
	if err != nil {
		h.loginUnknownUser(w, r, req, err)
		return
	}

	switch u.AuthSource {
	case store.AuthSourceLDAP:
		// The directory is the only authority for this account. Its stored
		// PasswordHash is a placeholder and must never be verified against.
		h.loginLDAP(w, r, req.Username, req.Password, &u)

	case store.AuthSourceLocal, "":
		// "" is a row written before the auth_source column existed, and it
		// means the same thing as "local". Nothing normalizes it on read:
		// SQLite backfills existing rows once, at migration time, and the
		// fake defaults the field in CreateUser. A row that still carries ""
		// — one the migration missed, or a hand-written insert — reaches this
		// switch as "", so the empty case is matched here rather than relied
		// on to have been rewritten upstream.
		h.loginLocal(w, r, req, u)

	case store.AuthSourceOIDC:
		// SSO accounts have no local password: their stored PasswordHash is a
		// placeholder. The login page renders the password form beside the SSO
		// button, so an SSO user typing a password here is routine, not a sign
		// of a corrupted row — log it at Info so it cannot bury the Error the
		// default branch below raises for a genuinely unknown auth_source. The
		// 401 is the same equalized response as every other failure path.
		h.logger.InfoContext(ctx, "auth: SSO account cannot use password login",
			slog.String("username", u.Username), slog.String("auth_source", u.AuthSource))
		writeProblem(w, r, http.StatusUnauthorized, invalidLoginDetail)

	default:
		// Nothing validates auth_source in Go (the SQL CHECK was deliberately
		// omitted so the Down migration works), so a hand-edited row or one
		// written by a newer binary is reachable here. Falling through to the
		// local-password branch would authenticate such an account against
		// whatever happens to sit in password_hash. Fail closed instead.
		h.logger.ErrorContext(ctx, "auth: unknown auth_source, refusing login",
			slog.String("username", u.Username), slog.String("auth_source", u.AuthSource))
		writeProblem(w, r, http.StatusUnauthorized, invalidLoginDetail)
	}
}

// loginUnknownUser handles a failed user lookup: JIT provisioning when the
// username is simply unknown and a directory is configured, an equalized 401
// otherwise.
func (h *authHandler) loginUnknownUser(w http.ResponseWriter, r *http.Request, req loginRequest, err error) {
	ctx := r.Context()
	if errors.Is(err, store.ErrNotFound) && h.ldapVerifier != nil {
		// Unknown username with LDAP on: this is the just-in-time
		// provisioning path. No dummy verify here — not because the
		// equalization is unnecessary, but because it is unattainable. The
		// dummy hash works below only because both sides of that comparison
		// are argon2id derivations of matching cost; a directory bind is a
		// network round trip whose cost no local computation can be made to
		// match. See the security-posture note at the top of ldaplogin.go.
		h.loginLDAP(w, r, req.Username, req.Password, nil)
		return
	}
	if !errors.Is(err, store.ErrNotFound) {
		// A real store/infrastructure failure (as opposed to "no such
		// user") deserves a server-side signal — otherwise a database
		// outage looks identical to a mistyped password with no way to
		// tell them apart from the logs. This must NOT change what the
		// client sees: the response below is the same either way.
		h.logger.WarnContext(ctx, "auth: user lookup failed", slog.Any("error", err))
	}
	// Run a dummy argon2id verify so this path costs about the same as the
	// known-user path, which always calls password.Verify. Without this,
	// returning immediately here makes an unknown-username request measurably
	// faster than a known-username one — a timing side channel that lets an
	// attacker enumerate valid usernames even though the response bodies are
	// byte-identical. Do not remove this as a "wasted" call.
	if _, verr := password.Verify(dummyHash(), req.Password); verr != nil {
		h.logger.WarnContext(ctx, "auth: dummy verify failed unexpectedly", slog.Any("error", verr))
	}
	writeProblem(w, r, http.StatusUnauthorized, invalidLoginDetail)
}

// loginLocal verifies req against the account's stored password hash. This is
// the pre-C1 behavior, unchanged.
func (h *authHandler) loginLocal(w http.ResponseWriter, r *http.Request, req loginRequest, u store.User) {
	ok, verr := password.Verify(u.PasswordHash, req.Password)
	if verr != nil || !ok || u.Disabled {
		writeProblem(w, r, http.StatusUnauthorized, invalidLoginDetail)
		return
	}

	// A failure here is a token-generation problem, a hashed-token collision,
	// or a foreign-key violation (the just-fetched user vanished mid-request)
	// — none are client errors, so this is a 500 rather than the CRUD-style
	// 409 "already exists", which would be misleading.
	if err := h.issueSession(w, r, u); err != nil {
		h.logger.ErrorContext(r.Context(), "auth: create session failed", slog.Any("error", err))
		writeProblem(w, r, http.StatusInternalServerError, "failed to create session")
		return
	}
	writeJSON(w, http.StatusOK, toUserResponse(u, h.ldapCfg.RoleSource, h.oidcCfg.RoleSource))
}

// logoutResponse is what POST /auth/logout returns. RedirectURL is present
// only under auth.oidc.logout_mode=provider with a provider that advertises an
// end-session endpoint; the client is expected to navigate to it. Every other
// logout returns an empty object, which a client may ignore.
type logoutResponse struct {
	RedirectURL string `json:"redirect_url,omitempty"`
}

// logout revokes the caller's server-side session (if the cookie resolves to
// one) and clears the cookie regardless.
//
// It also carries the two SSO logout mechanisms, which are separate and answer
// different problems:
//
//   - reauth_mode governs whether the NEXT SSO login is allowed to be silent.
//     Clearing sqi's session while the provider still considers the person
//     signed in is how a shared workstation signs the next person in as the
//     last one.
//   - logout_mode governs whether this logout also ends the session AT the
//     provider, i.e. signs the person out of every tool in the company. Off by
//     default because that is heavy-handed for someone mid-task elsewhere.
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

	// Mark this browser as having explicitly logged out, so the next SSO login
	// forces re-authentication. Written through setOIDCCookie, not a literal
	// http.Cookie, so its Path and attributes cannot drift from the ones
	// forceReauth reads back — scoped anywhere else, the marker is invisible to
	// the only code that looks for it and after_logout silently degrades to
	// never.
	if h.oidcProvider != nil && h.markReauthOnLogout() {
		h.setOIDCCookie(w, r, oidcReauthCookie, "1", int(reauthMarkerTTL.Seconds()))
	}

	var out logoutResponse
	if h.oidcProvider != nil && h.oidcCfg.LogoutMode == oidc.LogoutProvider {
		if u, ok := h.oidcProvider.EndSessionURL(ctx); ok {
			out.RedirectURL = u
		} else {
			// Loud, not silent: an operator who configured provider logout and
			// silently got local logout would believe a guarantee they do not
			// have. EndSessionURL reports the same false for "no end-session
			// endpoint advertised" and "discovery is unreachable", so this
			// message names both rather than asserting the wrong one.
			h.logger.ErrorContext(ctx,
				"auth: oidc logout_mode=provider but no end-session endpoint is available "+
					"(the provider advertises none, or discovery failed); degrading to local logout")
		}
	}
	writeJSON(w, http.StatusOK, out)
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
