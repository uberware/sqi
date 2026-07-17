// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Unit tests for the auth REST handlers: login, logout, me. Login is
// reachable unauthenticated (mounted outside the middleware.Auth group);
// logout and me require a valid session cookie.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/auth/password"
	"github.com/uberware/sqi/internal/auth/session"
	"github.com/uberware/sqi/internal/health"
	"github.com/uberware/sqi/internal/metrics"
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
	r := NewRouter(
		Config{DisableRateLimit: true, AuthEnabled: true},
		Deps{
			Store:        st,
			Auth:         session.New(st, "sqi_session", nil),
			SessionTTL:   time.Hour,
			CookieName:   "sqi_session",
			CookieSecure: "false",
		},
		newTestLogger(), metrics.New(), health.NewRegistry(),
	)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// seedAuthUser inserts a user with an argon2id-hashed password directly via
// the store, bypassing the (not-yet-implemented) user admin endpoints.
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
func doRequest(t *testing.T, method, url string, body any, cookie *http.Cookie) *http.Response {
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
	}
	resp, err := http.DefaultClient.Do(req)
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
	if logoutResp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204", logoutResp.StatusCode)
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

func TestLogout_NoCookieStillSucceeds(t *testing.T) {
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
