// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// adminKeysFixture stands up a router with an admin caller (logged in) plus a
// separate target user. These routes are driven through the real router rather
// than the handler directly: the permission gate lives on the route group, so
// calling the handler would skip the very check under test.
type adminKeysFixture struct {
	st     store.Store
	srv    *httptest.Server
	cookie *http.Cookie
	target store.User
}

func newAdminKeysFixture(t *testing.T, callerRole string) adminKeysFixture {
	t.Helper()
	st := fake.New()
	seedAuthUser(t, st, "b3-caller", "caller-pw", callerRole)
	target := seedAuthUser(t, st, "alice", "alice-pw", "user")

	srv := httptest.NewServer(authRouter(st))
	t.Cleanup(srv.Close)

	return adminKeysFixture{
		st:     st,
		srv:    srv,
		cookie: loginCookie(t, srv, "b3-caller", "caller-pw"),
		target: target,
	}
}

func (f adminKeysFixture) keysURL() string {
	return f.srv.URL + "/api/v1/users/" + f.target.ID + "/api-keys"
}

func TestAdminAPIKeys(t *testing.T) {
	t.Run("admin lists another user's keys", func(t *testing.T) {
		f := newAdminKeysFixture(t, "admin")
		seedAPIKey(t, f.st, f.target.ID, "alice-key")

		resp := doRequest(t, http.MethodGet, f.keysURL(), nil, f.cookie)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}

		body, err := readAll(resp)
		if err != nil {
			t.Fatalf("readAll: %v", err)
		}
		var keys []apiKeyResponse
		if err := json.Unmarshal(body, &keys); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(keys) != 1 || keys[0].Name != "alice-key" {
			t.Fatalf("keys = %+v, want one named alice-key", keys)
		}
	})

	t.Run("list never leaks a secret or a token hash", func(t *testing.T) {
		f := newAdminKeysFixture(t, "admin")
		seedAPIKey(t, f.st, f.target.ID, "alice-key")

		resp := doRequest(t, http.MethodGet, f.keysURL(), nil, f.cookie)
		defer resp.Body.Close()
		body, err := readAll(resp)
		if err != nil {
			t.Fatalf("readAll: %v", err)
		}
		for _, forbidden := range []string{"secret", "token_hash", "TokenHash"} {
			if bytes.Contains(body, []byte(forbidden)) {
				t.Fatalf("list response must never contain %q: %s", forbidden, body)
			}
		}
	})

	t.Run("empty list is an empty array, not null", func(t *testing.T) {
		f := newAdminKeysFixture(t, "admin")

		resp := doRequest(t, http.MethodGet, f.keysURL(), nil, f.cookie)
		defer resp.Body.Close()
		body, err := readAll(resp)
		if err != nil {
			t.Fatalf("readAll: %v", err)
		}
		if got := bytes.TrimSpace(body); !bytes.Equal(got, []byte("[]")) {
			t.Fatalf("body = %s, want []", got)
		}
	})

	t.Run("admin revokes another user's key", func(t *testing.T) {
		f := newAdminKeysFixture(t, "admin")
		key := seedAPIKey(t, f.st, f.target.ID, "alice-key")

		resp := doRequest(t, http.MethodDelete, f.keysURL()+"/"+key.ID, nil, f.cookie)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", resp.StatusCode)
		}

		remaining, err := f.st.ListAPIKeysForUser(t.Context(), f.target.ID)
		if err != nil {
			t.Fatalf("ListAPIKeysForUser: %v", err)
		}
		if len(remaining) != 0 {
			t.Fatalf("key should be revoked, got %d remaining", len(remaining))
		}
	})

	// RevokeAPIKey is owner-scoped, so a key id that belongs to someone else
	// simply is not found for this user. That existing scoping IS the
	// authorization check — there is no separate ownership branch to forget.
	t.Run("a key belonging to a different user is 404", func(t *testing.T) {
		f := newAdminKeysFixture(t, "admin")
		bob := seedAuthUser(t, f.st, "bob", "bob-pw", "user")
		bobKey := seedAPIKey(t, f.st, bob.ID, "bob-key")

		resp := doRequest(t, http.MethodDelete, f.keysURL()+"/"+bobKey.ID, nil, f.cookie)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}

		// And Bob's key must still be there — a cross-owner revoke that
		// returned 404 but deleted anyway would be the worst outcome.
		remaining, err := f.st.ListAPIKeysForUser(t.Context(), bob.ID)
		if err != nil {
			t.Fatalf("ListAPIKeysForUser: %v", err)
		}
		if len(remaining) != 1 {
			t.Fatalf("bob's key count = %d, want it untouched at 1", len(remaining))
		}
	})

	t.Run("revoking twice is 404 the second time", func(t *testing.T) {
		f := newAdminKeysFixture(t, "admin")
		key := seedAPIKey(t, f.st, f.target.ID, "alice-key")

		first := doRequest(t, http.MethodDelete, f.keysURL()+"/"+key.ID, nil, f.cookie)
		first.Body.Close()
		if first.StatusCode != http.StatusNoContent {
			t.Fatalf("first revoke status = %d, want 204", first.StatusCode)
		}

		second := doRequest(t, http.MethodDelete, f.keysURL()+"/"+key.ID, nil, f.cookie)
		defer second.Body.Close()
		if second.StatusCode != http.StatusNotFound {
			t.Fatalf("second revoke status = %d, want 404", second.StatusCode)
		}
	})

	// 200 [] for an unknown id would tell an admin "this account has no keys"
	// when the real answer is "no such account" — the wrong reply to act on
	// during a credential-revocation incident.
	t.Run("an unknown user id is 404, not an empty list", func(t *testing.T) {
		f := newAdminKeysFixture(t, "admin")

		resp := doRequest(t, http.MethodGet,
			f.srv.URL+"/api/v1/users/no-such-user/api-keys", nil, f.cookie)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("non-admin roles are forbidden", func(t *testing.T) {
		for _, role := range []string{"user", "operator", "read-only"} {
			t.Run(role, func(t *testing.T) {
				f := newAdminKeysFixture(t, role)
				key := seedAPIKey(t, f.st, f.target.ID, "alice-key")

				list := doRequest(t, http.MethodGet, f.keysURL(), nil, f.cookie)
				list.Body.Close()
				if list.StatusCode != http.StatusForbidden {
					t.Fatalf("role %s: GET status = %d, want 403", role, list.StatusCode)
				}

				del := doRequest(t, http.MethodDelete, f.keysURL()+"/"+key.ID, nil, f.cookie)
				del.Body.Close()
				if del.StatusCode != http.StatusForbidden {
					t.Fatalf("role %s: DELETE status = %d, want 403", role, del.StatusCode)
				}

				// A refused revoke must not have taken effect.
				remaining, err := f.st.ListAPIKeysForUser(t.Context(), f.target.ID)
				if err != nil {
					t.Fatalf("ListAPIKeysForUser: %v", err)
				}
				if len(remaining) != 1 {
					t.Fatalf("role %s: key count = %d, want it untouched at 1", role, len(remaining))
				}
			})
		}
	})
}
