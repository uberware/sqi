// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Unit tests for the auth REST handlers: login, logout, me. Login is
// reachable unauthenticated (mounted outside the middleware.Auth group);
// logout and me require a valid session cookie.

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	neturl "net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/auth/oidc"
	"github.com/uberware/sqi/internal/auth/password"
	"github.com/uberware/sqi/internal/auth/policy"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// newAuthTestServer builds a real router (via NewRouter) with auth enabled:
// a session.Authenticator backed by st, a 1-hour session TTL, and the
// production cookie name. Rate limiting is disabled so repeated login
// attempts in a single test don't 429.
func newAuthTestServer(t *testing.T, st store.Store) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(authRouter(st))
	t.Cleanup(srv.Close)
	return srv
}

// seedAuthUser inserts a user with an argon2id-hashed password directly via
// the store, bypassing the user-admin endpoints and their permission checks.
func seedAuthUser(t *testing.T, st store.Store, username, pw, role string) store.User {
	t.Helper()
	h, err := password.Hash(pw)
	if err != nil {
		t.Fatalf("password.Hash: %v", err)
	}
	u, err := st.CreateUser(t.Context(), store.User{
		ID:           uuid.NewString(),
		Username:     username,
		PasswordHash: h,
		Role:         role,
	})
	if err != nil {
		t.Fatalf("seedAuthUser: %v", err)
	}
	return u
}

// doRequest issues an HTTP request carrying the test context (satisfying the
// noctx linter) with an optional JSON body and optional cookie.
//
// When a cookie is present, a real browser sends an Origin header on unsafe
// methods (POST/PUT/PATCH/DELETE) even for same-origin requests — that's
// exactly what middleware.CSRF (mounted when AuthEnabled is true) requires to
// distinguish a same-origin browser call from a cross-site one riding the
// ambient cookie. Set Origin from url's own scheme+host to model that.
func doRequest(t *testing.T, method, url string, body any, cookie *http.Cookie) *http.Response {
	t.Helper()
	return doRequestWithClient(t, http.DefaultClient, method, url, body, cookie)
}

// doRequestWithClient is [doRequest] with a caller-supplied client. A TLS
// httptest server needs its own client (http.DefaultClient cannot verify the
// self-signed certificate), and everything else about the request — the JSON
// body, the cookie, and the Origin header that goes with it — must stay
// identical, or a TLS test hits the CSRF middleware for reasons the test
// itself does not explain.
func doRequestWithClient(t *testing.T, client *http.Client, method, url string, body any, cookie *http.Cookie) *http.Response {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, url, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
		if u, parseErr := neturl.Parse(url); parseErr == nil && u.Host != "" {
			req.Header.Set("Origin", u.Scheme+"://"+u.Host)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	// Callers are responsible for closing resp.Body (typically via
	// `defer resp.Body.Close()` right after the call), matching the
	// convention in router_test.go and ws_test.go.
	return resp
}

// sessionCookie returns the sqi_session cookie set on resp, or nil.
func sessionCookie(resp *http.Response) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == "sqi_session" {
			return c
		}
	}
	return nil
}

func decodeProblem(t *testing.T, resp *http.Response) string {
	t.Helper()
	var pd struct {
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pd); err != nil {
		t.Fatalf("decode problem body: %v", err)
	}
	return pd.Detail
}

// ── login ─────────────────────────────────────────────────────────────────────

func TestLogin_SetsCookieAndOmitsSecret(t *testing.T) {
	st := fake.New()
	seedAuthUser(t, st, "alice", "hunter2!", "operator")
	srv := newAuthTestServer(t, st)

	resp := doRequest(t, http.MethodPost, srv.URL+"/api/v1/auth/login",
		map[string]string{"username": "alice", "password": "hunter2!"}, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", resp.StatusCode)
	}

	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, leaked := got["password_hash"]; leaked {
		t.Fatal("login response leaked password_hash")
	}
	if got["username"] != "alice" {
		t.Errorf("response username = %v, want alice", got["username"])
	}

	c := sessionCookie(resp)
	if c == nil {
		t.Fatal("login did not set a sqi_session cookie")
	}
	if !c.HttpOnly {
		t.Error("cookie is not HttpOnly")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie SameSite = %v, want Lax", c.SameSite)
	}
	if c.Path != "/" {
		t.Errorf("cookie Path = %q, want /", c.Path)
	}
	if c.Value == "" {
		t.Error("cookie value is empty")
	}
	if want := int(time.Hour.Seconds()); c.MaxAge != want {
		t.Errorf("cookie MaxAge = %d, want %d", c.MaxAge, want)
	}
	if c.Secure {
		t.Error("cookie is Secure, want false (CookieSecure=\"false\" in test config)")
	}
}

func TestLogin_InvalidCredentialsIndistinguishable(t *testing.T) {
	st := fake.New()
	seedAuthUser(t, st, "alice", "hunter2!", "operator")
	seedAuthUser(t, st, "bob-disabled", "hunter2!", "operator")
	if u, err := st.GetUserByUsername(t.Context(), "bob-disabled"); err == nil {
		u.Disabled = true
		if _, err := st.UpdateUser(t.Context(), u); err != nil {
			t.Fatalf("disable bob: %v", err)
		}
	} else {
		t.Fatalf("get bob: %v", err)
	}
	srv := newAuthTestServer(t, st)

	login := func(username, pw string) (int, string) {
		resp := doRequest(t, http.MethodPost, srv.URL+"/api/v1/auth/login",
			map[string]string{"username": username, "password": pw}, nil)
		defer resp.Body.Close()
		return resp.StatusCode, decodeProblem(t, resp)
	}

	tests := []struct {
		name     string
		username string
		password string
	}{
		{"wrong password", "alice", "wrong"},
		{"unknown user", "nobody", "whatever"},
		{"disabled user, correct password", "bob-disabled", "hunter2!"},
	}
	var firstDetail string
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, detail := login(tt.username, tt.password)
			if status != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", status)
			}
			if i == 0 {
				firstDetail = detail
			} else if detail != firstDetail {
				t.Errorf("detail = %q, want identical to first case %q (user enumeration)", detail, firstDetail)
			}
		})
	}
}

// TestDummyHash_MatchesPasswordPackageParams guards the fix for the
// unknown-user timing side channel: login's unknown-user branch calls
// password.Verify(dummyHash, ...) so that path costs the same as the
// known-user branch, which always calls password.Verify for real. Timing
// itself is not asserted here (that would be flaky); instead this pins the
// structural guarantee that makes the fix work — dummyHash is a genuine,
// verifiable argon2id hash whose cost parameters match whatever
// password.Hash currently produces (not a stale hardcoded literal that
// could silently drift out of sync if the parameters are ever raised).
func TestDummyHash_MatchesPasswordPackageParams(t *testing.T) {
	probe, err := password.Hash("unrelated-probe-value")
	if err != nil {
		t.Fatalf("password.Hash: %v", err)
	}
	paramsOf := func(encoded string) string {
		parts := strings.Split(encoded, "$")
		if len(parts) != 6 {
			t.Fatalf("unexpected encoded hash format: %q", encoded)
		}
		return parts[3] // "m=<mem>,t=<time>,p=<threads>"
	}
	if got, want := paramsOf(dummyHash()), paramsOf(probe); got != want {
		t.Errorf("dummyHash params = %q, want %q (must match password.Hash's current defaults)", got, want)
	}
	ok, verr := password.Verify(dummyHash(), dummyVerifyPlaintext)
	if verr != nil {
		t.Fatalf("password.Verify(dummyHash(), dummyVerifyPlaintext): %v", verr)
	}
	if !ok {
		t.Fatal("dummyHash does not verify against the plaintext it was derived from")
	}
}

func TestLogin_InvalidJSONBody(t *testing.T) {
	st := fake.New()
	srv := newAuthTestServer(t, st)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/api/v1/auth/login",
		bytes.NewReader([]byte("{bad json}")))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// ── logout ────────────────────────────────────────────────────────────────────

func TestLogout_RevokesSessionAndClearsCookie(t *testing.T) {
	st := fake.New()
	seedAuthUser(t, st, "alice", "hunter2!", "operator")
	srv := newAuthTestServer(t, st)

	loginResp := doRequest(t, http.MethodPost, srv.URL+"/api/v1/auth/login",
		map[string]string{"username": "alice", "password": "hunter2!"}, nil)
	defer loginResp.Body.Close()
	cookie := sessionCookie(loginResp)
	if cookie == nil {
		t.Fatal("login did not set a cookie")
	}

	// The session works before logout.
	meResp := doRequest(t, http.MethodGet, srv.URL+"/api/v1/auth/me", nil, cookie)
	defer meResp.Body.Close()
	if meResp.StatusCode != http.StatusOK {
		t.Fatalf("pre-logout /auth/me status = %d, want 200", meResp.StatusCode)
	}

	logoutResp := doRequest(t, http.MethodPost, srv.URL+"/api/v1/auth/logout", nil, cookie)
	defer logoutResp.Body.Close()
	// 200 with a JSON body, not 204: logout now reports whether the client
	// should also navigate to the provider's end-session endpoint.
	if logoutResp.StatusCode != http.StatusOK {
		t.Fatalf("logout status = %d, want 200", logoutResp.StatusCode)
	}
	cleared := sessionCookie(logoutResp)
	if cleared == nil {
		t.Fatal("logout did not clear the cookie")
	}
	if cleared.MaxAge >= 0 {
		t.Errorf("cleared cookie MaxAge = %d, want negative", cleared.MaxAge)
	}

	// The old cookie must no longer authenticate — server-side revocation.
	meAfter := doRequest(t, http.MethodGet, srv.URL+"/api/v1/auth/me", nil, cookie)
	defer meAfter.Body.Close()
	if meAfter.StatusCode != http.StatusUnauthorized {
		t.Fatalf("post-logout /auth/me status = %d, want 401", meAfter.StatusCode)
	}
}

func TestLogout_UnauthenticatedRejected(t *testing.T) {
	// logout is mounted inside the auth group, so an unauthenticated request
	// never reaches the handler at all — it is gated 401 by middleware.Auth.
	st := fake.New()
	srv := newAuthTestServer(t, st)

	resp := doRequest(t, http.MethodPost, srv.URL+"/api/v1/auth/logout", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (logout requires a principal)", resp.StatusCode)
	}
}

// ── logout: SSO logout modes ─────────────────────────────────────────────────

// newLogoutHandler builds an authHandler with SSO wired in and returns it with
// the buffer its logger writes to. No router: logout tolerates a request with
// no resolvable session, so driving the handler directly keeps these tests
// about the two logout mechanisms rather than about session plumbing.
func newLogoutHandler(t *testing.T, p oidc.Provider, cfg oidc.Config) (*authHandler, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	h := newAuthHandler(authHandlerDeps{
		Store:        fake.New(),
		Logger:       slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		TTL:          time.Hour,
		CookieName:   "sqi_session",
		CookieSecure: "false",
		OIDCProvider: p,
		OIDCConfig:   cfg,
	})
	return h, &buf
}

// doLogout drives h.logout and returns the recorder plus the decoded body.
func doLogout(t *testing.T, h *authHandler) (*httptest.ResponseRecorder, logoutResponse) {
	t.Helper()
	rr := httptest.NewRecorder()
	h.logout(rr, newReq(t, http.MethodPost, "/api/v1/auth/logout", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("logout status = %d, want 200: %s", rr.Code, rr.Body)
	}
	var out logoutResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode logout body %q: %v", rr.Body, err)
	}
	return rr, out
}

// Under logout_mode=provider the response carries the provider's end-session
// URL. It is built from client_id + post_logout_redirect_uri and carries no
// token: sqi deliberately does not store ID tokens, so there is no id_token_hint
// to send and no plaintext bearer secret in a schema that otherwise holds only
// hashes.
func TestLogout_ProviderModeReturnsEndSessionURL(t *testing.T) {
	const endSession = "https://idp.example.com/logout?client_id=sqi&post_logout_redirect_uri=https%3A%2F%2Ffarm.example.com%2F"
	cfg := oidcTestCfg()
	cfg.LogoutMode = oidc.LogoutProvider
	cfg.PostLogoutRedirectURL = "https://farm.example.com/"
	p := &fakeOIDCProvider{endSessionOK: true, endSessionURL: endSession}
	h, logs := newLogoutHandler(t, p, cfg)

	_, out := doLogout(t, h)

	if out.RedirectURL != endSession {
		t.Errorf("redirect_url = %q, want %q", out.RedirectURL, endSession)
	}
	if n := p.endSessionCallCount(); n != 1 {
		t.Errorf("EndSessionURL called %d times, want exactly 1", n)
	}
	// No token of any kind may appear: not an id_token_hint, not the session
	// token, not an access token.
	for _, forbidden := range []string{"id_token", "token", "hint"} {
		if strings.Contains(out.RedirectURL, forbidden) {
			t.Errorf("redirect_url %q contains %q — sqi stores no tokens and must send none", out.RedirectURL, forbidden)
		}
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("a supported provider logged an error: %s", logs)
	}
}

// A provider that advertises no end-session endpoint must not silently become a
// local logout: the response still succeeds (there is nothing the user can do),
// but the operator gets an ERROR line. Degrading silently would let an operator
// believe a guarantee they do not have.
func TestLogout_ProviderModeDegradesWhenUnsupported(t *testing.T) {
	cfg := oidcTestCfg()
	cfg.LogoutMode = oidc.LogoutProvider
	// endSessionOK is false: no endpoint advertised, or discovery unreachable —
	// the Provider interface reports the same false for both.
	p := &fakeOIDCProvider{}
	h, logs := newLogoutHandler(t, p, cfg)

	rr, out := doLogout(t, h)

	if out.RedirectURL != "" {
		t.Errorf("redirect_url = %q, want it absent", out.RedirectURL)
	}
	// Exact match, not Contains: the point is that redirect_url is omitempty
	// and no other field is set, so the encoded object is empty — a
	// substring check would pass even if a fatter body happened to contain
	// the two literal characters "{}" somewhere inside it.
	if got := strings.TrimSpace(rr.Body.String()); got != "{}" {
		t.Errorf("body = %q, want the empty object %q (redirect_url is omitempty)", got, "{}")
	}
	logged := logs.String()
	if !strings.Contains(logged, "level=ERROR") {
		t.Errorf("degraded to local logout without an ERROR line; logs: %s", logged)
	}
	if !strings.Contains(logged, "logout_mode=provider") {
		t.Errorf("the error line does not name the setting that was not honored: %s", logged)
	}
}

// logout_mode=local (the default) must not produce a redirect at all: ending
// the person's session in every company tool is not something sqi does unasked.
func TestLogout_LocalModeReturnsNoRedirect(t *testing.T) {
	cfg := oidcTestCfg()
	cfg.LogoutMode = oidc.LogoutLocal
	p := &fakeOIDCProvider{endSessionOK: true, endSessionURL: "https://idp.example.com/logout"}
	h, _ := newLogoutHandler(t, p, cfg)

	_, out := doLogout(t, h)

	if out.RedirectURL != "" {
		t.Errorf("redirect_url = %q, want none under logout_mode=local", out.RedirectURL)
	}
	if n := p.endSessionCallCount(); n != 0 {
		t.Error("EndSessionURL was called under logout_mode=local")
	}
}

// TestLogout_ReauthMarkerCookie pins the marker cookie's own attributes. The
// Path is the load-bearing one: forceReauth reads the cookie back on
// /auth/oidc/login, and a marker written at any other scope is invisible to it,
// which turns reauth_mode=after_logout into reauth_mode=never with no error
// anywhere.
func TestLogout_ReauthMarkerCookie(t *testing.T) {
	tests := []struct {
		name string
		mode string
		want bool // a non-empty marker cookie is set
	}{
		{"after_logout sets the marker", oidc.ReauthAfterLogout, true},
		{"unset mode defaults to after_logout", "", true},
		{"never sets no marker", oidc.ReauthNever, false},
		{"always needs no marker", oidc.ReauthAlways, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := oidcTestCfg()
			cfg.ReauthMode = tt.mode
			h, _ := newLogoutHandler(t, &fakeOIDCProvider{}, cfg)

			rr, _ := doLogout(t, h)

			var marker *http.Cookie
			for _, c := range rr.Result().Cookies() {
				if c.Name == oidcReauthCookie && c.Value != "" {
					marker = c
				}
			}
			if tt.want != (marker != nil) {
				t.Fatalf("marker cookie set = %v, want %v", marker != nil, tt.want)
			}
			if marker == nil {
				return
			}
			if marker.Path != oidcCookiePath {
				t.Errorf("marker Path = %q, want %q — forceReauth reads it only at that scope",
					marker.Path, oidcCookiePath)
			}
			// Pin the constant's own value, not just the plumbing that
			// copies it onto the cookie: comparing only against
			// oidcCookiePath would still pass if the constant itself were
			// widened (e.g. to "/").
			if marker.Path != "/api/v1/auth/oidc" {
				t.Errorf("marker Path = %q, want %q", marker.Path, "/api/v1/auth/oidc")
			}
			if !marker.HttpOnly {
				t.Error("marker cookie is not HttpOnly")
			}
			if marker.SameSite != http.SameSiteLaxMode {
				t.Errorf("marker SameSite = %v, want Lax", marker.SameSite)
			}
			if marker.MaxAge <= 0 {
				t.Errorf("marker MaxAge = %d, want positive", marker.MaxAge)
			}
		})
	}
}

// TestLogout_SetsReauthMarkerUnderAfterLogout is the end-to-end version, and it
// is the one that matters: it drives a real logout followed by a real SSO login
// through a cookie jar, so the browser's own path-scoping rules decide whether
// the marker is delivered. Asserting on the Set-Cookie alone cannot catch a
// marker written where the reader never looks.
func TestLogout_SetsReauthMarkerUnderAfterLogout(t *testing.T) {
	tests := []struct {
		name          string
		mode          string
		doLogoutFirst bool // perform a logout before the SSO login
		want          bool // the authorization URL carries prompt=login
	}{
		{"after_logout, no prior logout", oidc.ReauthAfterLogout, false, false},
		{"after_logout, after a logout", oidc.ReauthAfterLogout, true, true},
		{"never, even after a logout", oidc.ReauthNever, true, false},
		{"always, without any logout", oidc.ReauthAlways, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := fake.New()
			seedAuthUser(t, st, "alice", "hunter2!", "operator")
			p := &fakeOIDCProvider{identity: aliceOIDCIdentity()}
			cfg := oidcTestCfg()
			cfg.ReauthMode = tt.mode
			ts := newOIDCServer(t, st, p, cfg)

			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatalf("cookiejar.New: %v", err)
			}
			client := &http.Client{
				Jar:           jar,
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			}

			if tt.doLogoutFirst {
				jarPost(t, client, ts.srv.URL+"/api/v1/auth/login",
					map[string]string{"username": "alice", "password": "hunter2!"})
				jarPost(t, client, ts.srv.URL+"/api/v1/auth/logout", nil)
			}

			// The jar decides whether the marker is attached — exactly as a
			// browser would.
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
				ts.srv.URL+"/api/v1/auth/oidc/login", nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("GET login: %v", err)
			}
			defer resp.Body.Close()
			loc, err := neturl.Parse(resp.Header.Get("Location"))
			if err != nil {
				t.Fatalf("parse Location: %v", err)
			}

			gotPrompt := loc.Query().Get("prompt") == "login"
			if gotPrompt != tt.want {
				t.Errorf("prompt=login in the authorization URL = %v, want %v (Location %q)",
					gotPrompt, tt.want, resp.Header.Get("Location"))
			}
			if got := p.forcedReauth(); got != tt.want {
				t.Errorf("forceReauth = %v, want %v", got, tt.want)
			}
		})
	}
}

// jarPost issues a cookie-jar-backed POST, setting the Origin header a browser
// would send on an unsafe same-origin request (middleware.CSRF requires it).
func jarPost(t *testing.T, client *http.Client, url string, body any) {
	t.Helper()
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		payload = b
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if u, perr := neturl.Parse(url); perr == nil && u.Host != "" {
		req.Header.Set("Origin", u.Scheme+"://"+u.Host)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		t.Fatalf("POST %s: status %d", url, resp.StatusCode)
	}
}

// ── me ────────────────────────────────────────────────────────────────────────

func TestMe_ReturnsPrincipal(t *testing.T) {
	st := fake.New()
	seedAuthUser(t, st, "alice", "hunter2!", "operator")
	srv := newAuthTestServer(t, st)

	loginResp := doRequest(t, http.MethodPost, srv.URL+"/api/v1/auth/login",
		map[string]string{"username": "alice", "password": "hunter2!"}, nil)
	defer loginResp.Body.Close()
	cookie := sessionCookie(loginResp)
	if cookie == nil {
		t.Fatal("login did not set a cookie")
	}

	resp := doRequest(t, http.MethodGet, srv.URL+"/api/v1/auth/me", nil, cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got struct {
		Subject string   `json:"subject"`
		Roles   []string `json:"roles"`
		Kind    string   `json:"kind"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Kind != "user" {
		t.Errorf("kind = %q, want user", got.Kind)
	}
	if len(got.Roles) != 1 || got.Roles[0] != "operator" {
		t.Errorf("roles = %v, want [operator]", got.Roles)
	}
	if got.Subject == "" {
		t.Error("subject is empty")
	}
}

func TestMe_UnauthenticatedReturns401(t *testing.T) {
	st := fake.New()
	srv := newAuthTestServer(t, st)

	resp := doRequest(t, http.MethodGet, srv.URL+"/api/v1/auth/me", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAuthMeReturnsUsernameAndPermissions(t *testing.T) {
	h := &authHandler{}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/auth/me", nil)
	req = req.WithContext(auth.NewContext(req.Context(), auth.Principal{
		Subject:     "u-1",
		Username:    "alice",
		DisplayName: "Alice",
		Roles:       []string{"user"},
		Kind:        auth.KindUser,
	}))
	rec := httptest.NewRecorder()

	h.me(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got principalResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Username != "alice" {
		t.Errorf("username = %q, want %q", got.Username, "alice")
	}
	want := []string{"apikeys.self", "infra.read", "jobs.read", "jobs.write", "products.read", "workers.read"}
	if !reflect.DeepEqual(got.Permissions, want) {
		t.Errorf("permissions = %v, want %v", got.Permissions, want)
	}
}

func TestAuthMeSuperuserReportsAllPermissions(t *testing.T) {
	h := &authHandler{}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/auth/me", nil)
	req = req.WithContext(auth.NewContext(req.Context(), auth.Principal{
		DisplayName: "anonymous",
		Kind:        auth.KindAnonymous,
		Superuser:   true,
	}))
	rec := httptest.NewRecorder()

	h.me(rec, req)

	var got principalResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Permissions) != len(policy.All) {
		t.Errorf("permissions len = %d, want %d (full set)", len(got.Permissions), len(policy.All))
	}
}
