// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/middleware"
)

// stubAuthenticator returns a fixed principal/error for tests.
type stubAuthenticator struct {
	p   auth.Principal
	err error
}

func (s stubAuthenticator) Authenticate(*http.Request) (auth.Principal, error) {
	return s.p, s.err
}

func TestAuth_FailingAuthenticator_Returns401AndSkipsHandler(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })
	h := middleware.Auth(stubAuthenticator{err: errors.New("bad token")}, discardLogger())(next)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/jobs", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	if called {
		t.Error("wrapped handler ran on auth failure, want skipped")
	}
}

func TestAuth_SuccessfulAuthenticator_InjectsPrincipal(t *testing.T) {
	var got auth.Principal
	var ok bool
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, ok = auth.FromContext(r.Context())
	})
	want := auth.Principal{Subject: "u1", Kind: auth.KindUser}
	h := middleware.Auth(stubAuthenticator{p: want}, discardLogger())(next)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/jobs", nil))

	if !ok {
		t.Fatal("principal not found in context")
	}
	if got.Subject != want.Subject {
		t.Errorf("Subject = %q, want %q", got.Subject, want.Subject)
	}
}

func TestAuth_NilAuthenticator_TreatedAsAnonymousSuperuser(t *testing.T) {
	var got auth.Principal
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, _ = auth.FromContext(r.Context())
	})
	h := middleware.Auth(nil, discardLogger())(next)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/jobs", nil))

	if rec.Code == http.StatusUnauthorized {
		t.Fatal("nil authenticator produced 401, want anonymous passthrough")
	}
	if !got.Superuser {
		t.Error("anonymous principal Superuser = false, want true")
	}
}
