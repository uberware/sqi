// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/uberware/sqi/internal/auth"
)

type stubAuthn struct {
	p   auth.Principal
	err error
}

func (s stubAuthn) Authenticate(*http.Request) (auth.Principal, error) { return s.p, s.err }

func TestChain_FirstSuccessWins(t *testing.T) {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	key := stubAuthn{p: auth.Principal{Subject: "key", Kind: auth.KindAPIKey}}
	sess := stubAuthn{p: auth.Principal{Subject: "sess", Kind: auth.KindUser}}
	c := auth.Chain(key, sess)
	p, err := c.Authenticate(r)
	if err != nil || p.Subject != "key" {
		t.Fatalf("want key principal, got %+v err=%v", p, err)
	}
}

func TestChain_FallsThroughOnError(t *testing.T) {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	key := stubAuthn{err: errors.New("no bearer")}
	sess := stubAuthn{p: auth.Principal{Subject: "sess", Kind: auth.KindUser}}
	c := auth.Chain(key, sess)
	p, err := c.Authenticate(r)
	if err != nil || p.Subject != "sess" {
		t.Fatalf("want session principal, got %+v err=%v", p, err)
	}
}

func TestChain_AllFail_ReturnsLastError(t *testing.T) {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	last := errors.New("no cookie")
	c := auth.Chain(stubAuthn{err: errors.New("no bearer")}, stubAuthn{err: last})
	if _, err := c.Authenticate(r); !errors.Is(err, last) {
		t.Fatalf("want last error, got %v", err)
	}
}
