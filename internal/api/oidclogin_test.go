// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Tests for the OIDC/SSO browser round trip: GET /auth/oidc/login and
// GET /auth/oidc/callback.
//
// These routes are unlike every other route in this package in three ways the
// tests below pin deliberately:
//
//   - they are browser NAVIGATIONS, so every outcome is a 302 and every failure
//     is the same generic 302 (a distinguishable failure would make the
//     callback an enumeration oracle);
//   - they are registered only when SSO is configured, so an unconfigured
//     deployment 404s rather than advertising a route that cannot work;
//   - their post-login destination is a constant, never a request parameter.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/auth/oidc"
	"github.com/uberware/sqi/internal/auth/rolemap"
	"github.com/uberware/sqi/internal/auth/session"
	"github.com/uberware/sqi/internal/health"
	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/product"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// ── fixtures ─────────────────────────────────────────────────────────────────

// oidcProviderBase is the fake provider's authorization endpoint. Tests assert
// the login redirect points at it, so it must be an absolute cross-origin URL:
// the whole point of the login route is to leave this server.
const oidcProviderBase = "https://idp.example.com/authorize"

// fakeOIDCProvider is a scripted oidc.Provider. Every test drives this, so none
// of them needs a real identity provider. The mutex is not decoration: the
// handler runs on the httptest server's goroutine while assertions run on the
// test's.
type fakeOIDCProvider struct {
	mu            sync.Mutex
	authErr       error
	identity      oidc.Identity
	exchangeErr   error
	lastState     string
	lastNonce     string
	lastChallenge string
	lastReauth    bool
	lastCode      string
	lastVerifier  string
	exchanges     int
	// endSessionURL/endSessionOK script EndSessionURL. The zero value models a
	// provider that advertises no end-session endpoint — which is also what an
	// unreachable discovery document looks like from the caller's side.
	endSessionURL string
	endSessionOK  bool
	lastPostOut   string
}

func (f *fakeOIDCProvider) AuthCodeURL(
	_ context.Context, state, nonce, codeChallenge string, forceReauth bool,
) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastState, f.lastNonce, f.lastChallenge, f.lastReauth = state, nonce, codeChallenge, forceReauth
	if f.authErr != nil {
		return "", f.authErr
	}
	q := url.Values{"state": {state}, "nonce": {nonce}, "code_challenge": {codeChallenge}}
	if forceReauth {
		q.Set("prompt", "login")
	}
	return oidcProviderBase + "?" + q.Encode(), nil
}

func (f *fakeOIDCProvider) Exchange(_ context.Context, code, codeVerifier, _ string) (oidc.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exchanges++
	f.lastCode, f.lastVerifier = code, codeVerifier
	if f.exchangeErr != nil {
		return oidc.Identity{}, f.exchangeErr
	}
	return f.identity, nil
}

func (f *fakeOIDCProvider) EndSessionURL(_ context.Context, postLogoutRedirect string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastPostOut = postLogoutRedirect
	if !f.endSessionOK {
		return "", false
	}
	return f.endSessionURL, true
}

// postLogoutRedirectSeen reports the argument of the most recent EndSessionURL.
func (f *fakeOIDCProvider) postLogoutRedirectSeen() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastPostOut
}

// exchangeCount reports how many times the provider was asked to redeem a code.
func (f *fakeOIDCProvider) exchangeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exchanges
}

// forcedReauth reports the forceReauth flag of the most recent AuthCodeURL.
func (f *fakeOIDCProvider) forcedReauth() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastReauth
}

// aliceOIDCIdentity is the identity a successful exchange reports by default.
func aliceOIDCIdentity() oidc.Identity {
	return oidc.Identity{
		Subject:     "sub-1",
		Username:    "alice",
		DisplayName: "Alice Anderson",
		Groups:      []string{"admins"},
	}
}

func oidcTestCfg() oidc.Config {
	return oidc.Config{
		RoleSource:  oidc.RoleSourceDirectory,
		DefaultRole: "read-only",
		ReauthMode:  oidc.ReauthAfterLogout,
		RoleMap:     []rolemap.Mapping{{Group: "admins", Role: "admin"}},
	}
}

// authRouterOIDC mirrors authRouter with an SSO provider wired in. A nil
// provider models auth.oidc.enabled=false, in which case the routes must not
// be registered at all.
func authRouterOIDC(st store.Store, p oidc.Provider, cfg oidc.Config, key []byte) chi.Router {
	return NewRouter(
		Config{DisableRateLimit: true, AuthEnabled: true},
		Deps{
			Store:        st,
			Products:     product.NewCatalog(st),
			Auth:         session.New(st, "sqi_session", nil),
			SessionTTL:   time.Hour,
			CookieName:   "sqi_session",
			CookieSecure: "false",
			OIDCProvider: p,
			OIDCConfig:   cfg,
			OIDCStateKey: key,
		},
		newTestLogger(), metrics.New(), health.NewRegistry(),
	)
}

// oidcTestServer is an SSO-enabled test server plus the state-signing key the
// router was built with, so tests can seal and open state cookies themselves.
type oidcTestServer struct {
	srv *httptest.Server
	key []byte
}

func newOIDCServer(t *testing.T, st store.Store, p oidc.Provider, cfg oidc.Config) oidcTestServer {
	t.Helper()
	key, err := oidc.NewSigningKey()
	if err != nil {
		t.Fatalf("NewSigningKey: %v", err)
	}
	srv := httptest.NewServer(authRouterOIDC(st, p, cfg, key))
	t.Cleanup(srv.Close)
	return oidcTestServer{srv: srv, key: key}
}

// ssoResponse is a fully-consumed response. These routes only ever answer with
// a status, a Location, and Set-Cookie headers, so capturing those (rather than
// passing a live *http.Response around) keeps every assertion helper free of
// body-lifetime bookkeeping.
type ssoResponse struct {
	status   int
	location string
	cookies  []*http.Cookie
	body     string
}

// cookie returns the Set-Cookie of the given name, or nil.
func (s ssoResponse) cookie(name string) *http.Cookie {
	for _, c := range s.cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// sessionIssued reports whether a non-empty session cookie was set.
func (s ssoResponse) sessionIssued() bool {
	c := s.cookie("sqi_session")
	return c != nil && c.Value != ""
}

// getNoRedirect issues a GET carrying cookies and captures the response without
// following the redirect — the redirect target IS the assertion.
func getNoRedirect(t *testing.T, rawURL string, cookies ...*http.Cookie) ssoResponse {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return ssoResponse{
		status:   resp.StatusCode,
		location: resp.Header.Get("Location"),
		cookies:  resp.Cookies(),
		body:     string(body),
	}
}

// startLogin drives GET /auth/oidc/login and returns the state cookie it set
// plus the state parameter it put in the authorization URL.
func startLogin(t *testing.T, ts oidcTestServer, reauth *http.Cookie) (*http.Cookie, string) {
	t.Helper()
	var cookies []*http.Cookie
	if reauth != nil {
		cookies = append(cookies, reauth)
	}
	resp := getNoRedirect(t, ts.srv.URL+"/api/v1/auth/oidc/login", cookies...)
	if resp.status != http.StatusFound {
		t.Fatalf("login status = %d, want 302", resp.status)
	}
	state := resp.cookie(oidcStateCookie)
	if state == nil || state.Value == "" {
		t.Fatal("login set no state cookie")
	}
	loc, err := url.Parse(resp.location)
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	return state, loc.Query().Get("state")
}

// callback drives GET /auth/oidc/callback with the given query and cookies.
func callback(t *testing.T, ts oidcTestServer, q url.Values, cookies ...*http.Cookie) ssoResponse {
	t.Helper()
	return getNoRedirect(t, ts.srv.URL+"/api/v1/auth/oidc/callback?"+q.Encode(), cookies...)
}

// ── login ────────────────────────────────────────────────────────────────────

func TestOIDCLogin_RedirectsToProviderAndSealsState(t *testing.T) {
	p := &fakeOIDCProvider{identity: aliceOIDCIdentity()}
	ts := newOIDCServer(t, fake.New(), p, oidcTestCfg())

	resp := getNoRedirect(t, ts.srv.URL+"/api/v1/auth/oidc/login")

	if resp.status != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.status)
	}
	loc := resp.location
	if !strings.HasPrefix(loc, oidcProviderBase) {
		t.Fatalf("Location = %q, want a redirect to the provider", loc)
	}

	c := resp.cookie(oidcStateCookie)
	if c == nil {
		t.Fatal("no state cookie set")
	}
	if !c.HttpOnly {
		t.Error("state cookie is not HttpOnly; page script could read or forge the flow state")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("state cookie SameSite = %v, want Lax (Strict would not survive the provider's redirect back)", c.SameSite)
	}
	if c.Path != oidcCookiePath {
		t.Errorf("state cookie Path = %q, want %q", c.Path, oidcCookiePath)
	}
	// The same bound the server enforces on the sealed issued-at, so a
	// cooperating browser and the server agree on when a flow is dead.
	if c.MaxAge != int(oidc.StateTTL.Seconds()) {
		t.Errorf("state cookie MaxAge = %d, want %d", c.MaxAge, int(oidc.StateTTL.Seconds()))
	}

	// The state in the URL must be the state that was sealed into the cookie —
	// otherwise the comparison the callback makes proves nothing.
	fs, err := oidc.OpenState(ts.key, c.Value)
	if err != nil {
		t.Fatalf("OpenState on the cookie the server just set: %v", err)
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if got := u.Query().Get("state"); got != fs.State {
		t.Errorf("state param = %q, sealed state = %q; they must match", got, fs.State)
	}
	if got := u.Query().Get("code_challenge"); got != fs.Challenge() {
		t.Errorf("code_challenge = %q, want the S256 challenge of the sealed verifier %q", got, fs.Challenge())
	}
	if u.Query().Get("nonce") != fs.Nonce {
		t.Error("nonce in the authorization URL does not match the sealed nonce")
	}
}

func TestOIDCRoutes_NotRegisteredWhenSSODisabled(t *testing.T) {
	st := fake.New()
	srv := httptest.NewServer(authRouter(st)) // no OIDCProvider
	t.Cleanup(srv.Close)

	for _, path := range []string{"/api/v1/auth/oidc/login", "/api/v1/auth/oidc/callback"} {
		t.Run(path, func(t *testing.T) {
			resp := getNoRedirect(t, srv.URL+path)
			if resp.status != http.StatusNotFound {
				t.Errorf("status = %d, want 404 — an unconfigured deployment must not advertise the route",
					resp.status)
			}
		})
	}
}

// TestOIDCRoutes_NotRegisteredWhenStateKeyEmpty pins that a provider is not on
// its own enough to mount these routes. They are public, so middleware.CSRF
// cannot cover them and the HMAC-signed state cookie is their only defense
// against a forged callback — and an HMAC keyed with nothing is one any
// attacker can compute. A provider wired up without a key must 404, not serve a
// login whose callback anyone can forge.
func TestOIDCRoutes_NotRegisteredWhenStateKeyEmpty(t *testing.T) {
	keys := []struct {
		name string
		key  []byte
	}{
		{name: "nil key", key: nil},
		{name: "zero-length key", key: []byte{}},
	}
	for _, k := range keys {
		t.Run(k.name, func(t *testing.T) {
			p := &fakeOIDCProvider{identity: aliceOIDCIdentity()}
			srv := httptest.NewServer(authRouterOIDC(fake.New(), p, oidcTestCfg(), k.key))
			t.Cleanup(srv.Close)

			for _, path := range []string{"/api/v1/auth/oidc/login", "/api/v1/auth/oidc/callback"} {
				t.Run(path, func(t *testing.T) {
					resp := getNoRedirect(t, srv.URL+path)
					if resp.status != http.StatusNotFound {
						t.Errorf("status = %d, want 404 — without a signing key the state cookie is forgeable",
							resp.status)
					}
					if resp.sessionIssued() {
						t.Error("a keyless SSO route issued a session cookie")
					}
				})
			}
		})
	}
}

func TestOIDCLogin_ReauthMode(t *testing.T) {
	tests := []struct {
		name        string
		mode        string
		reauthValue string // non-empty sets the reauth marker cookie
		want        bool
	}{
		{name: "always, no marker", mode: oidc.ReauthAlways, want: true},
		{name: "always, marker set", mode: oidc.ReauthAlways, reauthValue: "1", want: true},
		{name: "never, marker set", mode: oidc.ReauthNever, reauthValue: "1", want: false},
		{name: "after_logout, no marker", mode: oidc.ReauthAfterLogout, want: false},
		{name: "after_logout, marker set", mode: oidc.ReauthAfterLogout, reauthValue: "1", want: true},
		{name: "unset mode defaults to after_logout", mode: "", reauthValue: "1", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &fakeOIDCProvider{identity: aliceOIDCIdentity()}
			cfg := oidcTestCfg()
			cfg.ReauthMode = tt.mode
			ts := newOIDCServer(t, fake.New(), p, cfg)

			var marker *http.Cookie
			if tt.reauthValue != "" {
				marker = &http.Cookie{Name: oidcReauthCookie, Value: tt.reauthValue}
			}
			startLogin(t, ts, marker)

			if got := p.forcedReauth(); got != tt.want {
				t.Errorf("forceReauth = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOIDCLogin_ClearsReauthMarker(t *testing.T) {
	p := &fakeOIDCProvider{identity: aliceOIDCIdentity()}
	ts := newOIDCServer(t, fake.New(), p, oidcTestCfg())

	resp := getNoRedirect(t, ts.srv.URL+"/api/v1/auth/oidc/login",
		&http.Cookie{Name: oidcReauthCookie, Value: "1"})

	c := resp.cookie(oidcReauthCookie)
	if c == nil {
		t.Fatal("login did not clear the reauth marker; every later login would re-prompt")
	}
	if c.Value != "" || c.MaxAge >= 0 {
		t.Errorf("reauth marker = %q (MaxAge %d), want a cleared cookie", c.Value, c.MaxAge)
	}
}

func TestOIDCLogin_ProviderUnavailableRedirectsToError(t *testing.T) {
	p := &fakeOIDCProvider{authErr: oidc.ErrDiscovery}
	ts := newOIDCServer(t, fake.New(), p, oidcTestCfg())

	resp := getNoRedirect(t, ts.srv.URL+"/api/v1/auth/oidc/login")

	assertGenericSSOFailure(t, resp)
}

// ── callback ─────────────────────────────────────────────────────────────────

// assertGenericSSOFailure pins the one response every SSO failure produces.
func assertGenericSSOFailure(t *testing.T, resp ssoResponse) {
	t.Helper()
	if resp.status != http.StatusFound {
		t.Errorf("status = %d, want 302", resp.status)
	}
	if got := resp.location; got != oidcErrorRedirect {
		t.Errorf("Location = %q, want the generic %q — a distinguishable failure is an enumeration oracle",
			got, oidcErrorRedirect)
	}
	if resp.sessionIssued() {
		t.Error("a failed SSO login issued a session cookie")
	}
}

func TestOIDCCallback_Success(t *testing.T) {
	st := fake.New()
	p := &fakeOIDCProvider{identity: aliceOIDCIdentity()}
	ts := newOIDCServer(t, st, p, oidcTestCfg())

	stateCookie, state := startLogin(t, ts, nil)
	resp := callback(t, ts, url.Values{"code": {"auth-code-1"}, "state": {state}}, stateCookie)

	if resp.status != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.status)
	}
	if got := resp.location; got != oidcAppRoot {
		t.Errorf("Location = %q, want %q", got, oidcAppRoot)
	}
	if !resp.sessionIssued() {
		t.Fatal("callback issued no session cookie")
	}
	if c := resp.cookie(oidcStateCookie); c == nil || c.Value != "" {
		t.Error("callback did not clear the state cookie; a replayed callback would still find it")
	}

	u, err := st.GetUserByExternalID(t.Context(), store.AuthSourceOIDC, "sub-1")
	if err != nil {
		t.Fatalf("account was not provisioned: %v", err)
	}
	if u.Username != "alice" || u.AuthSource != store.AuthSourceOIDC || u.ExternalID != "sub-1" {
		t.Errorf("provisioned user = %+v, want username=alice auth_source=oidc external_id=sub-1", u)
	}
	if u.Role != "admin" {
		t.Errorf("role = %q, want admin from the group mapping", u.Role)
	}
	if p.lastVerifier == "" {
		t.Error("exchange was called without the PKCE verifier from the sealed state")
	}
}

func TestOIDCCallback_Failures(t *testing.T) {
	tests := []struct {
		name string
		// cfg, when set, adjusts the SSO configuration for this case.
		cfg func(*oidc.Config)
		// setup mutates the world for this case and returns the callback query
		// plus the cookies to send.
		setup func(t *testing.T, st store.Store, p *fakeOIDCProvider, ts oidcTestServer) (url.Values, []*http.Cookie)
		// check runs extra assertions after the generic failure assertions.
		check func(t *testing.T, st store.Store, p *fakeOIDCProvider)
	}{
		{
			name: "no state cookie",
			setup: func(_ *testing.T, _ store.Store, _ *fakeOIDCProvider, _ oidcTestServer) (url.Values, []*http.Cookie) {
				return url.Values{"code": {"c"}, "state": {"whatever"}}, nil
			},
			check: func(t *testing.T, _ store.Store, p *fakeOIDCProvider) {
				t.Helper()
				if p.exchangeCount() != 0 {
					t.Error("the code was exchanged despite no state cookie; state must be checked first")
				}
			},
		},
		{
			name: "state cookie not signed by this server",
			setup: func(_ *testing.T, _ store.Store, _ *fakeOIDCProvider, _ oidcTestServer) (url.Values, []*http.Cookie) {
				return url.Values{"code": {"c"}, "state": {"s"}},
					[]*http.Cookie{{Name: oidcStateCookie, Value: "forged.payload"}}
			},
			check: func(t *testing.T, _ store.Store, p *fakeOIDCProvider) {
				t.Helper()
				if p.exchangeCount() != 0 {
					t.Error("the code was exchanged against an unverified state cookie")
				}
			},
		},
		{
			name: "state parameter does not match the cookie",
			setup: func(t *testing.T, _ store.Store, _ *fakeOIDCProvider, ts oidcTestServer) (url.Values, []*http.Cookie) {
				t.Helper()
				cookie, _ := startLogin(t, ts, nil)
				return url.Values{"code": {"c"}, "state": {"attacker-chosen-state"}}, []*http.Cookie{cookie}
			},
			check: func(t *testing.T, _ store.Store, p *fakeOIDCProvider) {
				t.Helper()
				if p.exchangeCount() != 0 {
					t.Error("the code was exchanged with a mismatched state; this is the CSRF defense")
				}
			},
		},
		{
			name: "provider returned an error",
			setup: func(t *testing.T, _ store.Store, _ *fakeOIDCProvider, ts oidcTestServer) (url.Values, []*http.Cookie) {
				t.Helper()
				cookie, state := startLogin(t, ts, nil)
				return url.Values{
					"state":             {state},
					"error":             {"access_denied"},
					"error_description": {"the user said no"},
				}, []*http.Cookie{cookie}
			},
			check: func(t *testing.T, _ store.Store, p *fakeOIDCProvider) {
				t.Helper()
				if p.exchangeCount() != 0 {
					t.Error("a provider error was followed by a code exchange")
				}
			},
		},
		{
			name: "token exchange rejected",
			setup: func(t *testing.T, _ store.Store, p *fakeOIDCProvider, ts oidcTestServer) (url.Values, []*http.Cookie) {
				t.Helper()
				p.exchangeErr = oidc.ErrTokenInvalid
				cookie, state := startLogin(t, ts, nil)
				return url.Values{"code": {"c"}, "state": {state}}, []*http.Cookie{cookie}
			},
		},
		{
			name: "identity maps to no role and there is no default",
			cfg:  func(c *oidc.Config) { c.DefaultRole = "" },
			setup: func(t *testing.T, _ store.Store, p *fakeOIDCProvider, ts oidcTestServer) (url.Values, []*http.Cookie) {
				t.Helper()
				p.identity = oidc.Identity{Subject: "sub-9", Username: "nobody", Groups: []string{"guests"}}
				cookie, state := startLogin(t, ts, nil)
				return url.Values{"code": {"c"}, "state": {state}}, []*http.Cookie{cookie}
			},
			check: func(t *testing.T, st store.Store, _ *fakeOIDCProvider) {
				t.Helper()
				if _, err := st.GetUserByUsername(t.Context(), "nobody"); err == nil {
					t.Error("an account was provisioned for an identity that maps to no role")
				}
			},
		},
		{
			name: "locally disabled account",
			setup: func(t *testing.T, st store.Store, _ *fakeOIDCProvider, ts oidcTestServer) (url.Values, []*http.Cookie) {
				t.Helper()
				seedOIDCUser(t, st, store.User{Username: "alice", Role: "admin", ExternalID: "sub-1", Disabled: true})
				cookie, state := startLogin(t, ts, nil)
				return url.Values{"code": {"c"}, "state": {state}}, []*http.Cookie{cookie}
			},
		},
		{
			name: "username collides with a local account",
			setup: func(t *testing.T, st store.Store, _ *fakeOIDCProvider, ts oidcTestServer) (url.Values, []*http.Cookie) {
				t.Helper()
				seedAuthUser(t, st, "alice", "local-password", "admin")
				cookie, state := startLogin(t, ts, nil)
				return url.Values{"code": {"c"}, "state": {state}}, []*http.Cookie{cookie}
			},
			check: func(t *testing.T, st store.Store, _ *fakeOIDCProvider) {
				t.Helper()
				u, err := st.GetUserByUsername(t.Context(), "alice")
				if err != nil {
					t.Fatalf("the local account disappeared: %v", err)
				}
				if u.AuthSource != store.AuthSourceLocal || u.ExternalID != "" {
					t.Errorf("the local account was adopted by the SSO identity: %+v", u)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := fake.New()
			p := &fakeOIDCProvider{identity: aliceOIDCIdentity()}
			cfg := oidcTestCfg()
			if tt.cfg != nil {
				tt.cfg(&cfg)
			}
			ts := newOIDCServer(t, st, p, cfg)

			q, cookies := tt.setup(t, st, p, ts)
			resp := callback(t, ts, q, cookies...)

			assertGenericSSOFailure(t, resp)
			assertNoLeakedDetail(t, resp)
			if tt.check != nil {
				tt.check(t, st, p)
			}
		})
	}
}

// assertNoLeakedDetail asserts the browser-visible response carries no provider
// detail and nothing resembling a token.
func assertNoLeakedDetail(t *testing.T, resp ssoResponse) {
	t.Helper()
	// The Location header is part of what the browser (and any proxy log) sees,
	// so it is checked alongside the body.
	visible := resp.body + "\n" + resp.location
	for _, needle := range []string{
		"access_denied", "the user said no", // provider-supplied detail
		"id_token", "eyJ", "auth-code-1", // anything token- or code-shaped
		"disabled", "collide", "role", "sub-1", // internal refusal reasons
		"idp.example.com", // the provider's own identity
	} {
		if strings.Contains(strings.ToLower(visible), strings.ToLower(needle)) {
			t.Errorf("SSO failure response leaks %q:\n%s", needle, visible)
		}
	}
}

// TestOIDCCallback_ClearsStateCookieSoABrowserReplayFindsNothing pins exactly
// what it says and no more: the callback clears the state cookie, so a browser
// that honored the clearing Set-Cookie sends none on a second navigation to the
// same URL and is refused.
//
// It is NOT a server-side replay defense, and must not be read as one. An
// attacker holding both the callback URL and the captured cookie VALUE can
// present them again and, within oidc.StateTTL, sqi will accept the state — the
// server keeps no record of a state having been spent. What actually stops that
// replay is the identity provider refusing to redeem an authorization code a
// second time (OAuth 2.1 §4.1.3 requires single-use codes), which is outside
// this process. The server-side bound sqi does enforce is the TTL; see
// TestOIDCCallback_ExpiredStateIsRefused.
func TestOIDCCallback_ClearsStateCookieSoABrowserReplayFindsNothing(t *testing.T) {
	st := fake.New()
	p := &fakeOIDCProvider{identity: aliceOIDCIdentity()}
	ts := newOIDCServer(t, st, p, oidcTestCfg())

	stateCookie, state := startLogin(t, ts, nil)
	q := url.Values{"code": {"auth-code-1"}, "state": {state}}

	first := callback(t, ts, q, stateCookie)
	if first.location != oidcAppRoot {
		t.Fatalf("first callback did not succeed: %q", first.location)
	}
	if c := first.cookie(oidcStateCookie); c == nil || c.MaxAge >= 0 {
		t.Fatal("the callback must clear the state cookie so a replay finds nothing")
	}

	// A browser that honored the clearing Set-Cookie sends no state cookie on
	// the replay.
	replay := callback(t, ts, q)
	assertGenericSSOFailure(t, replay)
}

// TestOIDCCallback_ExpiredStateIsRefused is the refusal the server really does
// make on its own: a state cookie older than oidc.StateTTL is rejected even
// when the client presents it verbatim alongside a matching state parameter.
// This is the case a captured-cookie replay eventually runs into, and the one
// the cookie's MaxAge alone could never enforce — MaxAge binds a cooperating
// browser, not an attacker holding the cookie's value.
func TestOIDCCallback_ExpiredStateIsRefused(t *testing.T) {
	tests := []struct {
		name     string
		age      time.Duration
		wantFail bool
	}{
		{name: "within the TTL", age: oidc.StateTTL - time.Minute},
		{name: "past the TTL", age: oidc.StateTTL + time.Minute, wantFail: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &fakeOIDCProvider{identity: aliceOIDCIdentity()}
			ts := newOIDCServer(t, fake.New(), p, oidcTestCfg())

			// Seal a genuinely-signed state with a backdated issued-at: this is
			// the exact cookie the server itself minted `age` ago.
			fs, err := oidc.NewFlowState()
			if err != nil {
				t.Fatalf("NewFlowState: %v", err)
			}
			fs.IssuedAt = time.Now().Add(-tt.age).Unix()
			sealed, err := oidc.SealState(ts.key, fs)
			if err != nil {
				t.Fatalf("SealState: %v", err)
			}

			resp := callback(t, ts,
				url.Values{"code": {"auth-code-1"}, "state": {fs.State}},
				&http.Cookie{Name: oidcStateCookie, Value: sealed})

			if !tt.wantFail {
				if resp.location != oidcAppRoot || !resp.sessionIssued() {
					t.Fatalf("a state inside the TTL was refused: Location = %q", resp.location)
				}
				return
			}
			assertGenericSSOFailure(t, resp)
			assertNoLeakedDetail(t, resp)
			if n := p.exchangeCount(); n != 0 {
				t.Errorf("exchanges = %d, want 0 — an expired state must be refused before the code is redeemed", n)
			}
		})
	}
}

// TestOIDCCallback_IgnoresCallerSuppliedRedirect is the open-redirect guard:
// the destination is a constant on both the success and the failure path,
// whatever the request asks for.
func TestOIDCCallback_IgnoresCallerSuppliedRedirect(t *testing.T) {
	const evil = "https://evil.example.com/steal"

	t.Run("on success", func(t *testing.T) {
		st := fake.New()
		p := &fakeOIDCProvider{identity: aliceOIDCIdentity()}
		ts := newOIDCServer(t, st, p, oidcTestCfg())

		stateCookie, state := startLogin(t, ts, nil)
		resp := callback(t, ts, url.Values{
			"code": {"auth-code-1"}, "state": {state},
			"next": {evil}, "redirect_uri": {evil}, "return_to": {evil}, "RelayState": {evil},
		}, stateCookie)

		if got := resp.location; got != oidcAppRoot {
			t.Errorf("Location = %q, want the constant %q", got, oidcAppRoot)
		}
	})

	t.Run("on failure", func(t *testing.T) {
		st := fake.New()
		p := &fakeOIDCProvider{identity: aliceOIDCIdentity()}
		ts := newOIDCServer(t, st, p, oidcTestCfg())

		resp := callback(t, ts, url.Values{"code": {"c"}, "state": {"s"}, "next": {evil}})

		if got := resp.location; got != oidcErrorRedirect {
			t.Errorf("Location = %q, want the constant %q", got, oidcErrorRedirect)
		}
	})
}

// TestOIDCCallback_SessionIsUsable proves the minted session is a real one: it
// authenticates a subsequent API call as the provisioned account.
func TestOIDCCallback_SessionIsUsable(t *testing.T) {
	st := fake.New()
	p := &fakeOIDCProvider{identity: aliceOIDCIdentity()}
	ts := newOIDCServer(t, st, p, oidcTestCfg())

	stateCookie, state := startLogin(t, ts, nil)
	resp := callback(t, ts, url.Values{"code": {"c"}, "state": {state}}, stateCookie)

	sess := resp.cookie("sqi_session")
	if sess == nil {
		t.Fatal("no session cookie")
	}
	me := doRequest(t, http.MethodGet, ts.srv.URL+"/api/v1/auth/me", nil, sess)
	defer me.Body.Close()
	if me.StatusCode != http.StatusOK {
		t.Fatalf("GET /auth/me with the SSO session = %d, want 200", me.StatusCode)
	}
	body, err := io.ReadAll(me.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), `"alice"`) {
		t.Errorf("/auth/me did not report the SSO account: %s", body)
	}
}

// TestOIDCCallback_RoleResyncRespectsRoleSource covers the two role_source
// modes on a second login for an already-provisioned identity.
func TestOIDCCallback_RoleResyncRespectsRoleSource(t *testing.T) {
	tests := []struct {
		name       string
		roleSource string
		wantRole   string
	}{
		{name: "directory reclaims the role", roleSource: oidc.RoleSourceDirectory, wantRole: "admin"},
		{name: "local keeps the admin's edit", roleSource: oidc.RoleSourceLocal, wantRole: "user"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := fake.New()
			// An existing SSO account whose role was changed locally to "user",
			// while the claims still map to "admin".
			seedOIDCUser(t, st, store.User{Username: "alice", Role: "user", ExternalID: "sub-1"})

			p := &fakeOIDCProvider{identity: aliceOIDCIdentity()}
			cfg := oidcTestCfg()
			cfg.RoleSource = tt.roleSource
			ts := newOIDCServer(t, st, p, cfg)

			stateCookie, state := startLogin(t, ts, nil)
			resp := callback(t, ts, url.Values{"code": {"c"}, "state": {state}}, stateCookie)
			if !resp.sessionIssued() {
				t.Fatal("login failed")
			}

			u, err := st.GetUserByExternalID(t.Context(), store.AuthSourceOIDC, "sub-1")
			if err != nil {
				t.Fatalf("GetUserByExternalID: %v", err)
			}
			if u.Role != tt.wantRole {
				t.Errorf("role = %q, want %q", u.Role, tt.wantRole)
			}
		})
	}
}

// seedOIDCUser inserts an SSO-backed account directly, since seedAuthUser only
// ever creates local ones.
func seedOIDCUser(t *testing.T, st store.Store, u store.User) store.User {
	t.Helper()
	u.ID = uuid.NewString()
	u.AuthSource = store.AuthSourceOIDC
	if u.PasswordHash == "" {
		u.PasswordHash = externalPlaceholderHash
	}
	out, err := st.CreateUser(t.Context(), u)
	if err != nil {
		t.Fatalf("seedOIDCUser: %v", err)
	}
	return out
}
