// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/uberware/sqi/internal/auth"
)

func TestAnonymous_ReturnsSuperuserPrincipal(t *testing.T) {
	r := httptest.NewRequestWithContext(context.Background(), "GET", "/", nil)
	p, err := auth.Anonymous().Authenticate(r)
	if err != nil {
		t.Fatalf("Anonymous().Authenticate: unexpected error: %v", err)
	}
	if p.Kind != auth.KindAnonymous {
		t.Errorf("Kind = %q, want %q", p.Kind, auth.KindAnonymous)
	}
	if !p.Superuser {
		t.Error("Superuser = false, want true")
	}
}

func TestPrincipalContext_RoundTrip(t *testing.T) {
	want := auth.Principal{Subject: "u1", Kind: auth.KindUser, Roles: []string{"admin"}}
	ctx := auth.NewContext(context.Background(), want)

	got, ok := auth.FromContext(ctx)
	if !ok {
		t.Fatal("FromContext: ok = false, want true")
	}
	if got.Subject != want.Subject || got.Kind != want.Kind {
		t.Errorf("FromContext = %+v, want %+v", got, want)
	}
}

func TestPrincipalContext_Missing(t *testing.T) {
	if _, ok := auth.FromContext(context.Background()); ok {
		t.Error("FromContext on empty context: ok = true, want false")
	}
}
