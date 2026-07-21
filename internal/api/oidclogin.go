// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// OIDC/SSO login (Phase 3, component C2).
//
//	GET /api/v1/auth/oidc/login    — start the authorization-code flow (public)
//	GET /api/v1/auth/oidc/callback — finish it and mint the session  (public)
//
// Both routes are PUBLIC — they cannot sit behind middleware.Auth, because
// their whole purpose is to produce the principal that middleware would
// require. That has three consequences this file must honor:
//
//  1. The callback is a browser NAVIGATION, not an API call. A JSON problem
//     body would be shown to nobody, so every failure redirects to the login
//     page with a generic marker and the detail goes only to the server log.
//  2. middleware.CSRF does not cover these routes (it guards the authenticated
//     group). A public GET that mints a session is inherent to the protocol;
//     the signed state cookie IS the defense, which is why it is validated
//     before anything else happens.
//  3. The post-login destination is a CONSTANT. Honoring a "return to"
//     parameter is how open redirects are built, and an attacker-chosen
//     destination on a page that has just minted a session is worth real money.
//
// Logging, stated plainly: internal/auth/oidc logs nothing when it rejects a
// token — its logger fires only on a failed discovery. Since the browser is
// told only "something went wrong", this file is the ONLY place a provider
// misconfiguration can surface, so every refusal is logged here, server-side,
// at a level matching how alarming it is.

import (
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/uberware/sqi/internal/auth/oidc"
	"github.com/uberware/sqi/internal/store"
)

const (
	// oidcStateCookie holds the sealed per-login flow state.
	oidcStateCookie = "sqi_oidc_state"
	// oidcReauthCookie marks that this browser explicitly logged out, so the
	// next SSO login forces re-authentication under ReauthAfterLogout.
	oidcReauthCookie = "sqi_oidc_reauth"
	// oidcCookiePath scopes both cookies above to the only routes that read
	// them, so they are not attached to every API call and every UI asset.
	oidcCookiePath = "/api/v1/auth/oidc"
	// oidcAppRoot is the only place a callback ever sends a browser.
	oidcAppRoot = "/"
	// reauthMarkerTTL bounds how long a logout's re-auth marker stays useful.
	// It is a convenience for the browser, not a security control: the marker
	// only ever causes an EXTRA credential prompt, so an expired one degrades to
	// a silent SSO login, which is what reauth_mode=never does by design.
	reauthMarkerTTL = 24 * time.Hour
	// oidcErrorRedirect is the login page with a generic failure marker. It
	// carries no reason: distinguishing "unknown user" from "no role mapping"
	// would turn the callback into an enumeration oracle.
	oidcErrorRedirect = "/?sso_error=1"
)

// oidcLogin starts the authorization-code flow: mint one-time state, seal it
// into a cookie, and send the browser to the provider.
func (h *authHandler) oidcLogin(w http.ResponseWriter, r *http.Request) {
	fs, err := oidc.NewFlowState()
	if err != nil {
		// Only reachable if crypto/rand is broken.
		h.failSSO(w, r, slog.LevelError, "auth: oidc flow state generation failed", slog.Any("error", err))
		return
	}
	sealed, err := oidc.SealState(h.oidcStateKey, fs)
	if err != nil {
		h.failSSO(w, r, slog.LevelError, "auth: oidc state cookie could not be sealed", slog.Any("error", err))
		return
	}

	// This may perform (or wait on) provider discovery, so it gets the
	// request's context: a browser that navigated away must not leave a
	// goroutine waiting out the full discovery timeout.
	authURL, err := h.oidcProvider.AuthCodeURL(r.Context(), fs.State, fs.Nonce, fs.Challenge(), h.forceReauth(r))
	if err != nil {
		// Almost always an unreachable or misconfigured provider. The user sees
		// only the generic marker, so this log is the sole diagnostic.
		h.failSSO(w, r, slog.LevelError, "auth: oidc authorization URL could not be built",
			slog.Any("error", err))
		return
	}

	// Both cookies must be written before http.Redirect, which commits the
	// response header.
	// The cookie's own expiry is only what a cooperating browser honors; the
	// authoritative bound is the issued-at inside the sealed payload, which
	// oidc.OpenState checks against the same oidc.StateTTL.
	h.setOIDCCookie(w, r, oidcStateCookie, sealed, int(oidc.StateTTL.Seconds()))
	// The marker has done its job for this login; leaving it set would re-prompt
	// on every subsequent login, not just the one after a logout.
	h.setOIDCCookie(w, r, oidcReauthCookie, "", -1)
	// G107/G710: this is the one redirect in sqi that deliberately leaves the
	// origin, and it is not an open redirect: authURL is built by
	// oidc.Provider from the operator-configured issuer's discovered
	// authorization endpoint, and the only request-derived value reaching it is
	// r.Context(). Nothing from the query string, the body, or a header can
	// influence the destination. The callback's destinations are constants.
	http.Redirect(w, r, authURL, http.StatusFound) //nolint:gosec // G710: destination comes from the configured provider's discovery document, never from the request
}

// oidcCallback finishes the flow. The ORDER of the steps below is the security
// property, not an implementation detail — see readFlowState.
func (h *authHandler) oidcCallback(w http.ResponseWriter, r *http.Request) {
	fs, ok := h.readFlowState(w, r)
	if !ok {
		return
	}

	id, err := h.oidcProvider.Exchange(r.Context(), r.URL.Query().Get("code"), fs.Verifier, fs.Nonce)
	if err != nil {
		h.failSSO(w, r, slog.LevelError, "auth: oidc code exchange or id-token validation failed",
			slog.Any("error", err))
		return
	}

	role, ok := h.oidcCfg.MapRole(id.Groups)
	if !ok {
		// default_role is empty and nothing matched: this deployment requires
		// group membership to sign in at all. Routine, so Info.
		h.failSSO(w, r, slog.LevelInfo, "auth: oidc login rejected, no role mapping matched",
			slog.String("username", id.Username), slog.Any("groups", id.Groups))
		return
	}

	u, err := h.resolveExternalUser(r, store.AuthSourceOIDC, externalIdentity{
		ExternalID:  id.Subject,
		Username:    id.Username,
		DisplayName: id.DisplayName,
	}, role, h.oidcCfg.RoleSource == oidc.RoleSourceDirectory)
	if err != nil {
		// resolveExternalUser already logged the specifics; this records that a
		// login was refused because of it. A disabled account is an operator's
		// deliberate override rather than a fault, so it does not warn.
		level := slog.LevelWarn
		if errors.Is(err, errExternalAccountDisabled) {
			level = slog.LevelInfo
		}
		h.failSSO(w, r, level, "auth: oidc login refused during account resolution",
			slog.String("username", id.Username), slog.Any("error", err))
		return
	}

	if err := h.issueSession(w, r, u); err != nil {
		h.failSSO(w, r, slog.LevelError, "auth: oidc session creation failed",
			slog.String("username", u.Username), slog.Any("error", err))
		return
	}
	http.Redirect(w, r, oidcAppRoot, http.StatusFound)
}

// readFlowState performs the four checks that must precede any use of the
// callback's parameters, in this order:
//
//  1. Read the state cookie and clear it IMMEDIATELY — unconditionally, before
//     anything can return early. A callback replayed with the same code must
//     not find a state cookie still waiting for it.
//  2. Verify the cookie's HMAC (oidc.OpenState), so a cookie-tossing sibling
//     subdomain cannot supply flow state of its own choosing.
//  3. Compare the state query parameter against the cookie's, in constant
//     time. This is the CSRF defense for a route middleware.CSRF cannot guard.
//  4. Only then look at a provider-reported error, which is attacker-reachable
//     text and must not be acted on (or logged) before the request is proven to
//     belong to a flow this server started.
//
// It reports false once it has already redirected the browser.
func (h *authHandler) readFlowState(w http.ResponseWriter, r *http.Request) (oidc.FlowState, bool) {
	c, cookieErr := r.Cookie(oidcStateCookie)
	h.setOIDCCookie(w, r, oidcStateCookie, "", -1)
	if cookieErr != nil || c.Value == "" {
		h.failSSO(w, r, slog.LevelWarn, "auth: oidc callback carried no state cookie")
		return oidc.FlowState{}, false
	}

	fs, err := oidc.OpenState(h.oidcStateKey, c.Value)
	if err != nil {
		// Also what an in-flight login across a server restart looks like: the
		// signing key is per-boot by design.
		h.failSSO(w, r, slog.LevelWarn, "auth: oidc callback state cookie did not verify",
			slog.Any("error", err))
		return oidc.FlowState{}, false
	}

	// Constant-time, like any other secret comparison: a byte-by-byte compare
	// leaks how much of a guessed state is right.
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("state")), []byte(fs.State)) != 1 {
		h.failSSO(w, r, slog.LevelWarn, "auth: oidc callback state parameter does not match the state cookie")
		return oidc.FlowState{}, false
	}

	if code := r.URL.Query().Get("error"); code != "" {
		// The provider refused (consent declined, account locked, ...). Its code
		// goes to the log only — never to the browser, which gets the same
		// generic marker as every other failure.
		h.failSSO(w, r, slog.LevelWarn, "auth: oidc provider returned an error",
			slog.String("provider_error", code))
		return oidc.FlowState{}, false
	}

	return fs, true
}

// failSSO logs why a login failed and sends the browser to the generic error
// destination. Splitting the two apart is exactly the mistake this exists to
// prevent: the response says nothing useful, so a caller that forgot to log
// would leave an operator with a login that fails silently and no trace of why.
func (h *authHandler) failSSO(w http.ResponseWriter, r *http.Request, level slog.Level, msg string, attrs ...any) {
	h.logger.Log(r.Context(), level, msg, attrs...)
	http.Redirect(w, r, oidcErrorRedirect, http.StatusFound)
}

// forceReauth decides whether to ask the provider to re-prompt for credentials.
func (h *authHandler) forceReauth(r *http.Request) bool {
	switch h.oidcCfg.ReauthMode {
	case oidc.ReauthAlways:
		return true
	case oidc.ReauthNever:
		return false
	default:
		// ReauthAfterLogout, and the zero value, which means the same: re-prompt
		// only when this browser explicitly logged out. Defaulting an unknown
		// mode here rather than in config keeps the safer behavior — boot
		// validation rejects unknown modes, so this is unreachable in practice.
		c, err := r.Cookie(oidcReauthCookie)
		return err == nil && c.Value != ""
	}
}

// markReauthOnLogout reports whether logout should leave the re-auth marker
// behind. It is the exact complement of forceReauth's default arm and must stay
// that way: the two are a pair, and a mode that sets no marker but reads one
// (or the reverse) is a mode where after_logout quietly becomes never.
//
// Under ReauthAlways the marker is redundant (every login re-prompts) and under
// ReauthNever it must not be written at all — it would sit in the browser
// waiting for someone to switch the mode.
func (h *authHandler) markReauthOnLogout() bool {
	switch h.oidcCfg.ReauthMode {
	case oidc.ReauthAlways, oidc.ReauthNever:
		return false
	default:
		// ReauthAfterLogout, and the zero value, which means the same.
		return true
	}
}

// setOIDCCookie writes one of this file's two flow cookies. A negative maxAge
// with an empty value clears it.
func (h *authHandler) setOIDCCookie(w http.ResponseWriter, r *http.Request, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is resolved dynamically by h.secure(r) from the 3-valued CookieSecure config; HttpOnly and SameSite are set explicitly
		Name:  name,
		Value: value,
		Path:  oidcCookiePath,
		// HttpOnly: page script must not be able to read or forge flow state.
		HttpOnly: true,
		// Lax, not Strict: the provider's redirect back to the callback is a
		// cross-site top-level navigation, and Strict would withhold the cookie
		// on exactly the request that needs it, breaking every login.
		SameSite: http.SameSiteLaxMode,
		Secure:   h.secure(r),
		MaxAge:   maxAge,
	})
}
