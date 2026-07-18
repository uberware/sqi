// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// End-to-end authorization tests over the real router. Each case logs in as a
// role and asserts the HTTP status the matrix predicts, or (for the auth-off
// case) exercises the router with auth disabled to confirm the gate is inert.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/health"
	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/store/fake"
)

func TestAuthz_MatrixOverRealRouter(t *testing.T) {
	cases := []struct {
		name, role, method, path string
		body                     any
		want                     int
	}{
		{"readonly cannot submit job", "read-only", http.MethodPost, "/api/v1/jobs", map[string]any{}, http.StatusForbidden},
		{"user can reach jobs read", "user", http.MethodGet, "/api/v1/jobs", nil, http.StatusOK},
		{"user cannot manage workers", "user", http.MethodPost, "/api/v1/workers/w1/disable", nil, http.StatusForbidden},
		{"operator can manage workers", "operator", http.MethodPost, "/api/v1/workers/w1/disable", nil, http.StatusNotFound}, // allowed → handler runs → 404 (no such worker)
		{"operator cannot list users", "operator", http.MethodGet, "/api/v1/users", nil, http.StatusForbidden},
		{"admin can list users", "admin", http.MethodGet, "/api/v1/users", nil, http.StatusOK},
		{"readonly cannot read diagnostics", "read-only", http.MethodGet, "/api/v1/diagnostics/logs", nil, http.StatusForbidden},
		{"readonly can read workers", "read-only", http.MethodGet, "/api/v1/workers", nil, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := fake.New()
			seedAuthUser(t, st, "u", "pw-secret-1", tc.role)
			srv := newAuthTestServer(t, st)
			cookie := loginCookie(t, srv, "u", "pw-secret-1")
			resp := doRequest(t, tc.method, srv.URL+tc.path, tc.body, cookie)
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("%s %s as %s: status = %d, want %d", tc.method, tc.path, tc.role, resp.StatusCode, tc.want)
			}
		})
	}
}

func TestAuthz_DisabledIsUnchanged(t *testing.T) {
	// Auth off: anonymous superuser passes every gate. A raw router with a nil
	// Auth (auth.Anonymous) must let a POST /jobs reach the handler (400/201,
	// never 403).
	deps := Deps{Store: fake.New(), Auth: auth.Anonymous(), CookieName: "sqi_session"}
	r := NewRouter(Config{DisableRateLimit: true}, deps, newTestLogger(), metrics.New(), health.NewRegistry())
	srv := httptest.NewServer(r)
	defer srv.Close()

	resp := doRequest(t, http.MethodPost, srv.URL+"/api/v1/jobs", map[string]any{}, nil)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		t.Fatal("auth-off POST /jobs returned 403; authorization must be inert when disabled")
	}
}
