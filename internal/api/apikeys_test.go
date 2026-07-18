// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Unit tests for the API-key REST handlers: create, list, revoke. All routes
// are mounted inside the middleware.Auth group and are self-scoped to the
// caller's principal, so tests authenticate via seedAuthUser + /auth/login,
// reusing the helpers defined in auth_test.go, and exercise the real router
// end to end rather than injecting a principal directly.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/health"
	"github.com/uberware/sqi/internal/metrics"
	"github.com/uberware/sqi/internal/store/fake"
)

// ── create / list ─────────────────────────────────────────────────────────────

func TestAPIKeys_CreateReturnsSecretOnce(t *testing.T) {
	st := fake.New()
	seedAuthUser(t, st, "alice", "hunter2!", "operator")
	srv := newAuthTestServer(t, st)
	cookie := loginCookie(t, srv, "alice", "hunter2!")

	resp := doRequest(t, http.MethodPost, srv.URL+"/api/v1/api-keys",
		map[string]any{"name": "laptop"}, cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	var created struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Prefix string `json:"prefix"`
		Secret string `json:"secret"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if !strings.HasPrefix(created.Secret, "sqi_") {
		t.Fatalf("secret = %q, want prefix sqi_", created.Secret)
	}
	if created.ID == "" {
		t.Error("id is empty")
	}
	if created.Name != "laptop" {
		t.Errorf("name = %v, want laptop", created.Name)
	}
	if created.Prefix == "" {
		t.Error("prefix is empty")
	}

	listResp := doRequest(t, http.MethodGet, srv.URL+"/api/v1/api-keys", nil, cookie)
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", listResp.StatusCode)
	}
	var listed []map[string]any
	if err := json.NewDecoder(listResp.Body).Decode(&listed); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("len(listed) = %d, want 1", len(listed))
	}
	if _, leaked := listed[0]["secret"]; leaked {
		t.Errorf("list response leaked secret: %v", listed[0])
	}
	if listed[0]["name"] != "laptop" {
		t.Errorf("listed name = %v, want laptop", listed[0]["name"])
	}
}

// ── revoke ────────────────────────────────────────────────────────────────────

func TestAPIKeys_RevokeInvalidatesAndScopes(t *testing.T) {
	st := fake.New()
	seedAuthUser(t, st, "alice", "hunter2!", "operator")
	seedAuthUser(t, st, "bob", "hunter2!", "operator")
	srv := newAuthTestServer(t, st)
	aliceCookie := loginCookie(t, srv, "alice", "hunter2!")
	bobCookie := loginCookie(t, srv, "bob", "hunter2!")

	createResp := doRequest(t, http.MethodPost, srv.URL+"/api/v1/api-keys",
		map[string]any{"name": "alice-key"}, aliceCookie)
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", createResp.StatusCode)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(createResp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.ID == "" {
		t.Fatal("created key id is empty")
	}
	id := created.ID

	// Bob is not the owner: revoking Alice's key must 404, not succeed.
	bobDelete := doRequest(t, http.MethodDelete, srv.URL+"/api/v1/api-keys/"+id, nil, bobCookie)
	defer bobDelete.Body.Close()
	if bobDelete.StatusCode != http.StatusNotFound {
		t.Fatalf("bob delete status = %d, want 404", bobDelete.StatusCode)
	}

	// Alice, the owner, can revoke it.
	aliceDelete := doRequest(t, http.MethodDelete, srv.URL+"/api/v1/api-keys/"+id, nil, aliceCookie)
	defer aliceDelete.Body.Close()
	if aliceDelete.StatusCode != http.StatusNoContent {
		t.Fatalf("alice delete status = %d, want 204", aliceDelete.StatusCode)
	}

	// The revoked key no longer appears in Alice's list.
	listResp := doRequest(t, http.MethodGet, srv.URL+"/api/v1/api-keys", nil, aliceCookie)
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", listResp.StatusCode)
	}
	var listed []map[string]any
	if err := json.NewDecoder(listResp.Body).Decode(&listed); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	for _, k := range listed {
		if k["id"] == id {
			t.Fatalf("revoked key %s still present in list: %v", id, listed)
		}
	}
}

// ── auth-off inert ───────────────────────────────────────────────────────────

// TestAPIKeys_AuthOffInert verifies that with auth disabled (the anonymous
// principal in play), issuing an API key is rejected rather than minting a
// key for an empty/anonymous subject.
func TestAPIKeys_AuthOffInert(t *testing.T) {
	st := fake.New()
	r := NewRouter(
		Config{DisableRateLimit: true, AuthEnabled: false},
		Deps{Store: st, Auth: auth.Anonymous()},
		newTestLogger(), metrics.New(), health.NewRegistry(),
	)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	resp := doRequest(t, http.MethodPost, srv.URL+"/api/v1/api-keys",
		map[string]any{"name": "laptop"}, nil)
	defer resp.Body.Close()
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Fatalf("status = %d, want 4xx (auth-off must not mint a key)", resp.StatusCode)
	}
}
