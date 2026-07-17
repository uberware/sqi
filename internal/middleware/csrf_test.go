// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware_test

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/uberware/sqi/internal/middleware"
)

// csrfHandler builds a CSRF-guarded 200 OK handler with a fixed cookie name
// and an explicit allow-listed origin, matching the shape of the real router
// wiring (CookieName from deps.CookieName, AllowedOrigins from cfg.CORSOrigins).
func csrfHandler(t *testing.T) http.Handler {
	t.Helper()
	mw := middleware.CSRF(middleware.CSRFConfig{
		CookieName:     "sqi_session",
		AllowedOrigins: []string{"https://farm.example"},
	}, discardLogger())
	return mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
}

func TestCSRF(t *testing.T) {
	tests := []struct {
		name           string
		method         string
		withCookie     bool
		origin         string
		referer        string
		tls            bool
		forwardedProto string
		wantStatus     int
	}{
		{
			name:       "foreign origin on cookie-authenticated POST is rejected",
			method:     http.MethodPost,
			withCookie: true,
			origin:     "https://evil.example",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "allow-listed origin passes",
			method:     http.MethodPost,
			withCookie: true,
			origin:     "https://farm.example",
			wantStatus: http.StatusOK,
		},
		{
			name:       "same-origin passes",
			method:     http.MethodPost,
			withCookie: true,
			origin:     "http://example.com",
			wantStatus: http.StatusOK,
		},
		{
			name:       "GET is never checked, even with a foreign origin",
			method:     http.MethodGet,
			withCookie: true,
			origin:     "https://evil.example",
			wantStatus: http.StatusOK,
		},
		{
			name:       "POST with no cookie is exempt (not cookie-authenticated)",
			method:     http.MethodPost,
			withCookie: false,
			origin:     "https://evil.example",
			wantStatus: http.StatusOK,
		},
		{
			name:       "cookie-bearing POST with no Origin or Referer is rejected",
			method:     http.MethodPost,
			withCookie: true,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "Referer fallback: allow-listed referer passes",
			method:     http.MethodPost,
			withCookie: true,
			referer:    "https://farm.example/some/page",
			wantStatus: http.StatusOK,
		},
		{
			name:       "Referer fallback: foreign referer is rejected",
			method:     http.MethodPost,
			withCookie: true,
			referer:    "https://evil.example/some/page",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "http Origin against a TLS request is rejected (scheme mismatch)",
			method:     http.MethodPost,
			withCookie: true,
			origin:     "http://example.com",
			tls:        true,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "https Origin against a TLS request passes",
			method:     http.MethodPost,
			withCookie: true,
			origin:     "https://example.com",
			tls:        true,
			wantStatus: http.StatusOK,
		},
		{
			name:           "https Origin against an X-Forwarded-Proto: https request passes",
			method:         http.MethodPost,
			withCookie:     true,
			origin:         "https://example.com",
			forwardedProto: "https",
			wantStatus:     http.StatusOK,
		},
		{
			name:       "Origin: null is rejected",
			method:     http.MethodPost,
			withCookie: true,
			origin:     "null",
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := csrfHandler(t)
			r := httptest.NewRequestWithContext(context.Background(), tt.method, "http://example.com/api/v1/users", nil)
			if tt.tls {
				r.TLS = &tls.ConnectionState{}
			}
			if tt.withCookie {
				r.AddCookie(&http.Cookie{Name: "sqi_session", Value: "tok"})
			}
			if tt.origin != "" {
				r.Header.Set("Origin", tt.origin)
			}
			if tt.referer != "" {
				r.Header.Set("Referer", tt.referer)
			}
			if tt.forwardedProto != "" {
				r.Header.Set("X-Forwarded-Proto", tt.forwardedProto)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, tt.wantStatus)
			}
		})
	}
}
