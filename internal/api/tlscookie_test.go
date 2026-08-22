// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/uberware/sqi/internal/store/fake"
)

// doRequestWithClient is doRequest with a caller-supplied client. The shared
// helper uses http.DefaultClient, which cannot verify httptest's self-signed
// TLS certificate; httptest.Server.Client() can.
func doRequestWithClient(t *testing.T, client *http.Client, method, url string, body any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, url, bytes.NewReader(b))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

// The session cookie's Secure attribute is resolved by authHandler.secure from
// the three-valued auth.session.cookie_secure setting. "auto" — the default —
// reads r.TLS. That logic predates TLS support and was, until these tests,
// pinned by nothing that ran against a real TLS listener: every existing
// cookie test sets CookieSecure to "false" explicitly.
//
// Both directions matter. A Secure cookie issued over plaintext is silently
// dropped by the browser and login simply stops working, with no error
// anywhere — so "not Secure on plaintext" needs a test just as much as
// "Secure under TLS" does.

// loginCookieUnder starts srvFn's server against a router configured with the
// given cookie_secure mode, logs in, and returns the session cookie.
func loginCookieUnder(t *testing.T, tlsListener bool, mode string) *http.Cookie {
	t.Helper()
	st := fake.New()
	seedAuthUser(t, st, "alice", "hunter2!", "operator")

	router := authRouterWith(st, func(d *Deps) { d.CookieSecure = mode })

	var srv *httptest.Server
	if tlsListener {
		srv = httptest.NewTLSServer(router)
	} else {
		srv = httptest.NewServer(router)
	}
	t.Cleanup(srv.Close)

	client := srv.Client() // trusts httptest's own certificate when TLS
	resp := doRequestWithClient(t, client, http.MethodPost, srv.URL+"/api/v1/auth/login",
		map[string]string{"username": "alice", "password": "hunter2!"})
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", resp.StatusCode)
	}
	c := sessionCookie(resp)
	if c == nil {
		t.Fatal("login did not set a sqi_session cookie")
	}
	return c
}

func TestSessionCookie_SecureUnderTLS(t *testing.T) {
	if c := loginCookieUnder(t, true, "auto"); !c.Secure {
		t.Error("session cookie is not Secure on a TLS listener with cookie_secure=auto")
	}
}

func TestSessionCookie_NotSecureOnPlaintext(t *testing.T) {
	if c := loginCookieUnder(t, false, "auto"); c.Secure {
		t.Error("session cookie is Secure on a plaintext listener; browsers drop it and login breaks silently")
	}
}

func TestSessionCookie_ExplicitModesOverrideTransport(t *testing.T) {
	if c := loginCookieUnder(t, false, "true"); !c.Secure {
		t.Error("cookie_secure=true did not force Secure on a plaintext listener")
	}
	if c := loginCookieUnder(t, true, "false"); c.Secure {
		t.Error("cookie_secure=false did not suppress Secure on a TLS listener")
	}
}
