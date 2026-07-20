// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// Unit tests for the user-admin REST handlers: create, list, get, update,
// setPassword, delete. All routes are mounted inside the middleware.Auth
// group, so every test authenticates first via seedAuthUser + /auth/login,
// reusing the helpers defined in auth_test.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/auth/ldap"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// ── create ────────────────────────────────────────────────────────────────────

func TestCreateUser_OmitsSecret(t *testing.T) {
	st := fake.New()
	seedAuthUser(t, st, "admin", "hunter2!", "admin")
	srv := newAuthTestServer(t, st)
	cookie := loginCookie(t, srv, "admin", "hunter2!")

	body := map[string]any{
		"username": "carol", "password": "pw-carol-1", "role": "user",
	}
	resp := doRequest(t, http.MethodPost, srv.URL+"/api/v1/users", body, cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	raw, err := readAll(resp)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	assertNoSecretLeak(t, raw)

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["role"] != "user" {
		t.Fatalf("role = %v, want user", got["role"])
	}
	if got["username"] != "carol" {
		t.Fatalf("username = %v, want carol", got["username"])
	}
	if got["id"] == "" || got["id"] == nil {
		t.Fatal("id is empty")
	}
}

func TestCreateUser_DefaultsRoleToUser(t *testing.T) {
	st := fake.New()
	seedAuthUser(t, st, "admin", "hunter2!", "admin")
	srv := newAuthTestServer(t, st)
	cookie := loginCookie(t, srv, "admin", "hunter2!")

	body := map[string]any{"username": "dave", "password": "pw-dave-12"}
	resp := doRequest(t, http.MethodPost, srv.URL+"/api/v1/users", body, cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["role"] != "user" {
		t.Fatalf("role = %v, want default user", got["role"])
	}
}

func TestCreateUser_DuplicateUsernameConflict(t *testing.T) {
	st := fake.New()
	seedAuthUser(t, st, "admin", "hunter2!", "admin")
	seedAuthUser(t, st, "erin", "whatever1", "user")
	srv := newAuthTestServer(t, st)
	cookie := loginCookie(t, srv, "admin", "hunter2!")

	body := map[string]any{"username": "erin", "password": "another-pw1", "role": "user"}
	resp := doRequest(t, http.MethodPost, srv.URL+"/api/v1/users", body, cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
}

func TestCreateUser_MissingFieldsRejected(t *testing.T) {
	st := fake.New()
	seedAuthUser(t, st, "admin", "hunter2!", "admin")
	srv := newAuthTestServer(t, st)
	cookie := loginCookie(t, srv, "admin", "hunter2!")

	tests := []struct {
		name string
		body map[string]any
	}{
		{"missing username", map[string]any{"password": "pw-123456"}},
		{"missing password", map[string]any{"username": "frank"}},
		{"invalid role", map[string]any{"username": "frank", "password": "pw-123456", "role": "superuser"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := doRequest(t, http.MethodPost, srv.URL+"/api/v1/users", tt.body, cookie)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}

func TestCreateUser_Unauthenticated(t *testing.T) {
	st := fake.New()
	srv := newAuthTestServer(t, st)

	body := map[string]any{"username": "carol", "password": "pw-carol-1"}
	resp := doRequest(t, http.MethodPost, srv.URL+"/api/v1/users", body, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// ── list ──────────────────────────────────────────────────────────────────────

func TestListUsers_OmitsSecret(t *testing.T) {
	st := fake.New()
	seedAuthUser(t, st, "admin", "hunter2!", "admin")
	seedAuthUser(t, st, "gina", "gina-pw-123", "user")
	srv := newAuthTestServer(t, st)
	cookie := loginCookie(t, srv, "admin", "hunter2!")

	resp := doRequest(t, http.MethodGet, srv.URL+"/api/v1/users", nil, cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	raw, err := readAll(resp)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	assertNoSecretLeak(t, raw)

	var got []map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(users) = %d, want 2", len(got))
	}
}

// ── get ───────────────────────────────────────────────────────────────────────

func TestGetUser_RoundTrip(t *testing.T) {
	st := fake.New()
	seedAuthUser(t, st, "admin", "hunter2!", "admin")
	target := seedAuthUser(t, st, "henry", "henry-pw-123", "user")
	srv := newAuthTestServer(t, st)
	cookie := loginCookie(t, srv, "admin", "hunter2!")

	resp := doRequest(t, http.MethodGet, srv.URL+"/api/v1/users/"+target.ID, nil, cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["username"] != "henry" {
		t.Fatalf("username = %v, want henry", got["username"])
	}
}

func TestGetUser_NotFound(t *testing.T) {
	st := fake.New()
	seedAuthUser(t, st, "admin", "hunter2!", "admin")
	srv := newAuthTestServer(t, st)
	cookie := loginCookie(t, srv, "admin", "hunter2!")

	resp := doRequest(t, http.MethodGet, srv.URL+"/api/v1/users/does-not-exist", nil, cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// ── update ────────────────────────────────────────────────────────────────────

func TestUpdateUser_ChangesDisplayNameRoleDisabled(t *testing.T) {
	st := fake.New()
	seedAuthUser(t, st, "admin", "hunter2!", "admin")
	target := seedAuthUser(t, st, "ivy", "ivy-pw-1234", "user")
	srv := newAuthTestServer(t, st)
	cookie := loginCookie(t, srv, "admin", "hunter2!")

	body := map[string]any{"display_name": "Ivy Admin", "role": "operator", "disabled": true}
	resp := doRequest(t, http.MethodPatch, srv.URL+"/api/v1/users/"+target.ID, body, cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["display_name"] != "Ivy Admin" {
		t.Fatalf("display_name = %v, want Ivy Admin", got["display_name"])
	}
	if got["role"] != "operator" {
		t.Fatalf("role = %v, want operator", got["role"])
	}
	if got["disabled"] != true {
		t.Fatalf("disabled = %v, want true", got["disabled"])
	}
}

// TestUpdateUser_PartialUpdatePreservesOtherFields guards PATCH semantics:
// a field absent from the request body must be left unchanged, not zeroed.
// Sending {"disabled": true} alone must not blank out an existing
// display_name or reset role to its zero value.
func TestUpdateUser_PartialUpdatePreservesOtherFields(t *testing.T) {
	st := fake.New()
	seedAuthUser(t, st, "admin", "hunter2!", "admin")
	target := seedAuthUser(t, st, "nora", "nora-pw-1234", "user")
	if _, err := st.UpdateUser(t.Context(), store.User{
		ID: target.ID, DisplayName: "Nora Original", Role: "operator", Disabled: false,
	}); err != nil {
		t.Fatalf("seed baseline: %v", err)
	}
	srv := newAuthTestServer(t, st)
	cookie := loginCookie(t, srv, "admin", "hunter2!")

	resp := doRequest(t, http.MethodPatch, srv.URL+"/api/v1/users/"+target.ID,
		map[string]any{"disabled": true}, cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["disabled"] != true {
		t.Fatalf("disabled = %v, want true", got["disabled"])
	}
	if got["display_name"] != "Nora Original" {
		t.Fatalf("display_name = %v, want unchanged %q (absent field must not be zeroed)", got["display_name"], "Nora Original")
	}
	if got["role"] != "operator" {
		t.Fatalf("role = %v, want unchanged \"operator\" (absent field must not be zeroed)", got["role"])
	}
}

func TestUpdateUser_InvalidRoleRejected(t *testing.T) {
	st := fake.New()
	seedAuthUser(t, st, "admin", "hunter2!", "admin")
	target := seedAuthUser(t, st, "jack", "jack-pw-1234", "user")
	srv := newAuthTestServer(t, st)
	cookie := loginCookie(t, srv, "admin", "hunter2!")

	body := map[string]any{"role": "superuser"}
	resp := doRequest(t, http.MethodPatch, srv.URL+"/api/v1/users/"+target.ID, body, cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestUpdateUser_NotFound(t *testing.T) {
	st := fake.New()
	seedAuthUser(t, st, "admin", "hunter2!", "admin")
	srv := newAuthTestServer(t, st)
	cookie := loginCookie(t, srv, "admin", "hunter2!")

	body := map[string]any{"display_name": "Nobody"}
	resp := doRequest(t, http.MethodPatch, srv.URL+"/api/v1/users/does-not-exist", body, cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// ── setPassword ───────────────────────────────────────────────────────────────

func TestSetPassword_AllowsLoginWithNewPassword(t *testing.T) {
	st := fake.New()
	seedAuthUser(t, st, "admin", "hunter2!", "admin")
	target := seedAuthUser(t, st, "kate", "old-pw-1234", "user")
	srv := newAuthTestServer(t, st)
	cookie := loginCookie(t, srv, "admin", "hunter2!")

	body := map[string]any{"password": "new-pw-56789"}
	resp := doRequest(t, http.MethodPut, srv.URL+"/api/v1/users/"+target.ID+"/password", body, cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}

	loginResp := doRequest(t, http.MethodPost, srv.URL+"/api/v1/auth/login",
		map[string]string{"username": "kate", "password": "new-pw-56789"}, nil)
	defer loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("login with new password status = %d, want 200", loginResp.StatusCode)
	}

	oldLoginResp := doRequest(t, http.MethodPost, srv.URL+"/api/v1/auth/login",
		map[string]string{"username": "kate", "password": "old-pw-1234"}, nil)
	defer oldLoginResp.Body.Close()
	if oldLoginResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("login with old password status = %d, want 401", oldLoginResp.StatusCode)
	}
}

func TestSetPassword_EmptyRejected(t *testing.T) {
	st := fake.New()
	seedAuthUser(t, st, "admin", "hunter2!", "admin")
	target := seedAuthUser(t, st, "leo", "leo-pw-1234", "user")
	srv := newAuthTestServer(t, st)
	cookie := loginCookie(t, srv, "admin", "hunter2!")

	body := map[string]any{"password": ""}
	resp := doRequest(t, http.MethodPut, srv.URL+"/api/v1/users/"+target.ID+"/password", body, cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestSetPassword_NotFound(t *testing.T) {
	st := fake.New()
	seedAuthUser(t, st, "admin", "hunter2!", "admin")
	srv := newAuthTestServer(t, st)
	cookie := loginCookie(t, srv, "admin", "hunter2!")

	body := map[string]any{"password": "new-pw-56789"}
	resp := doRequest(t, http.MethodPut, srv.URL+"/api/v1/users/does-not-exist/password", body, cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// ── delete ────────────────────────────────────────────────────────────────────

func TestDeleteUser_RemovesUser(t *testing.T) {
	st := fake.New()
	seedAuthUser(t, st, "admin", "hunter2!", "admin")
	target := seedAuthUser(t, st, "mia", "mia-pw-1234", "user")
	srv := newAuthTestServer(t, st)
	cookie := loginCookie(t, srv, "admin", "hunter2!")

	resp := doRequest(t, http.MethodDelete, srv.URL+"/api/v1/users/"+target.ID, nil, cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}

	getResp := doRequest(t, http.MethodGet, srv.URL+"/api/v1/users/"+target.ID, nil, cookie)
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete status = %d, want 404", getResp.StatusCode)
	}
}

func TestDeleteUser_NotFound(t *testing.T) {
	st := fake.New()
	seedAuthUser(t, st, "admin", "hunter2!", "admin")
	srv := newAuthTestServer(t, st)
	cookie := loginCookie(t, srv, "admin", "hunter2!")

	resp := doRequest(t, http.MethodDelete, srv.URL+"/api/v1/users/does-not-exist", nil, cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// ── last-admin lockout guard ─────────────────────────────────────────────────

func TestDeleteUser_LastAdminRejected(t *testing.T) {
	st := fake.New()
	admin := seedAuthUser(t, st, "root", "hunter2!", "admin")
	srv := newAuthTestServer(t, st)
	cookie := loginCookie(t, srv, "root", "hunter2!")

	resp := doRequest(t, http.MethodDelete, srv.URL+"/api/v1/users/"+admin.ID, nil, cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete last admin: status = %d, want 409", resp.StatusCode)
	}
}

func TestDeleteUser_NonLastAdminSucceeds(t *testing.T) {
	st := fake.New()
	seedAuthUser(t, st, "root", "hunter2!", "admin")
	second := seedAuthUser(t, st, "root2", "hunter2!", "admin")
	srv := newAuthTestServer(t, st)
	cookie := loginCookie(t, srv, "root", "hunter2!")

	resp := doRequest(t, http.MethodDelete, srv.URL+"/api/v1/users/"+second.ID, nil, cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete non-last admin: status = %d, want 204", resp.StatusCode)
	}
}

func TestUpdateUser_DemoteLastAdminRejected(t *testing.T) {
	st := fake.New()
	admin := seedAuthUser(t, st, "root", "hunter2!", "admin")
	srv := newAuthTestServer(t, st)
	cookie := loginCookie(t, srv, "root", "hunter2!")

	body := map[string]any{"role": "operator"}
	resp := doRequest(t, http.MethodPatch, srv.URL+"/api/v1/users/"+admin.ID, body, cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("demote last admin: status = %d, want 409", resp.StatusCode)
	}
}

func TestUpdateUser_DisableLastAdminRejected(t *testing.T) {
	st := fake.New()
	admin := seedAuthUser(t, st, "root", "hunter2!", "admin")
	srv := newAuthTestServer(t, st)
	cookie := loginCookie(t, srv, "root", "hunter2!")

	body := map[string]any{"disabled": true}
	resp := doRequest(t, http.MethodPatch, srv.URL+"/api/v1/users/"+admin.ID, body, cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("disable last admin: status = %d, want 409", resp.StatusCode)
	}
}

// ── auth_source enforcement ──────────────────────────────────────────────────

// In directory mode the role column is owned by the group mapping, so the
// API must refuse an edit rather than accept one that silently reverts at the
// user's next login.
func TestUsers_RoleEditRejectedForLDAPUserInDirectoryMode(t *testing.T) {
	st := fake.New()
	ctx := context.Background()
	target, err := st.CreateUser(ctx, store.User{
		ID: uuid.NewString(), Username: "alice", Role: "user",
		AuthSource: store.AuthSourceLDAP, PasswordHash: "!ldap",
	})
	if err != nil {
		t.Fatal(err)
	}
	seedAuthUser(t, st, "root", "hunter2!", "admin")
	srv := newLDAPServer(t, st, nil, ldap.Config{RoleSource: ldap.RoleSourceDirectory})
	cookie := loginCookie(t, srv, "root", "hunter2!")

	resp := doRequest(t, http.MethodPatch, srv.URL+"/api/v1/users/"+target.ID,
		map[string]any{"role": "admin"}, cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		raw := mustReadAll(t, resp)
		t.Fatalf("got %d, want 409: %s", resp.StatusCode, raw)
	}
	after, err := st.GetUser(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Role != "user" {
		t.Errorf("role changed despite 409: %q", after.Role)
	}
}

// Non-role fields stay editable — an admin must still be able to disable a
// directory account, which is the local override that works during an outage.
func TestUsers_DisableAllowedForLDAPUserInDirectoryMode(t *testing.T) {
	st := fake.New()
	ctx := context.Background()
	target, err := st.CreateUser(ctx, store.User{
		ID: uuid.NewString(), Username: "alice", Role: "user",
		AuthSource: store.AuthSourceLDAP, PasswordHash: "!ldap",
	})
	if err != nil {
		t.Fatal(err)
	}
	seedAuthUser(t, st, "root", "hunter2!", "admin")
	srv := newLDAPServer(t, st, nil, ldap.Config{RoleSource: ldap.RoleSourceDirectory})
	cookie := loginCookie(t, srv, "root", "hunter2!")

	resp := doRequest(t, http.MethodPatch, srv.URL+"/api/v1/users/"+target.ID,
		map[string]any{"disabled": true}, cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw := mustReadAll(t, resp)
		t.Fatalf("got %d, want 200: %s", resp.StatusCode, raw)
	}
	after, err := st.GetUser(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Disabled {
		t.Error("disabled flag not applied")
	}
}

func TestUsers_RoleEditAllowedForLDAPUserInLocalMode(t *testing.T) {
	st := fake.New()
	ctx := context.Background()
	target, err := st.CreateUser(ctx, store.User{
		ID: uuid.NewString(), Username: "alice", Role: "user",
		AuthSource: store.AuthSourceLDAP, PasswordHash: "!ldap",
	})
	if err != nil {
		t.Fatal(err)
	}
	seedAuthUser(t, st, "root", "hunter2!", "admin")
	srv := newLDAPServer(t, st, nil, ldap.Config{RoleSource: ldap.RoleSourceLocal})
	cookie := loginCookie(t, srv, "root", "hunter2!")

	resp := doRequest(t, http.MethodPatch, srv.URL+"/api/v1/users/"+target.ID,
		map[string]any{"role": "admin"}, cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw := mustReadAll(t, resp)
		t.Fatalf("got %d, want 200: %s", resp.StatusCode, raw)
	}
	after, err := st.GetUser(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Role != "admin" {
		t.Errorf("role: got %q, want admin", after.Role)
	}
}

// Role edits on LOCAL accounts are unaffected in either mode.
func TestUsers_RoleEditAlwaysAllowedForLocalUser(t *testing.T) {
	for _, mode := range []string{ldap.RoleSourceDirectory, ldap.RoleSourceLocal} {
		t.Run(mode, func(t *testing.T) {
			st := fake.New()
			seedAuthUser(t, st, "root", "hunter2!", "admin")
			target := seedAuthUser(t, st, "bob", "bob-pw-1234", "user")
			srv := newLDAPServer(t, st, nil, ldap.Config{RoleSource: mode})
			cookie := loginCookie(t, srv, "root", "hunter2!")

			resp := doRequest(t, http.MethodPatch, srv.URL+"/api/v1/users/"+target.ID,
				map[string]any{"role": "operator"}, cookie)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				raw := mustReadAll(t, resp)
				t.Fatalf("got %d, want 200: %s", resp.StatusCode, raw)
			}
		})
	}
}

// There is no local password to set on a directory account; a 409 says so
// instead of pretending to succeed.
func TestUsers_SetPasswordRejectedForLDAPUser(t *testing.T) {
	st := fake.New()
	ctx := context.Background()
	target, err := st.CreateUser(ctx, store.User{
		ID: uuid.NewString(), Username: "alice", Role: "user",
		AuthSource: store.AuthSourceLDAP, PasswordHash: "!ldap",
	})
	if err != nil {
		t.Fatal(err)
	}
	seedAuthUser(t, st, "root", "hunter2!", "admin")
	srv := newLDAPServer(t, st, nil, ldap.Config{RoleSource: ldap.RoleSourceDirectory})
	cookie := loginCookie(t, srv, "root", "hunter2!")

	resp := doRequest(t, http.MethodPut, srv.URL+"/api/v1/users/"+target.ID+"/password",
		map[string]any{"password": "newpass1234"}, cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		raw := mustReadAll(t, resp)
		t.Fatalf("got %d, want 409: %s", resp.StatusCode, raw)
	}
}

// POST /users always creates a local account: directory accounts arrive
// through JIT provisioning, never through the admin form.
func TestUsers_CreateAlwaysLocal(t *testing.T) {
	st := fake.New()
	seedAuthUser(t, st, "root", "hunter2!", "admin")
	srv := newLDAPServer(t, st, nil, ldap.Config{RoleSource: ldap.RoleSourceDirectory})
	cookie := loginCookie(t, srv, "root", "hunter2!")

	resp := doRequest(t, http.MethodPost, srv.URL+"/api/v1/users",
		map[string]any{
			"username": "carol", "password": "pw-carol-1", "role": "user",
			"auth_source": "ldap",
		}, cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw := mustReadAll(t, resp)
		t.Fatalf("got %d, want 201: %s", resp.StatusCode, raw)
	}
	u, err := st.GetUserByUsername(context.Background(), "carol")
	if err != nil {
		t.Fatal(err)
	}
	if u.AuthSource != store.AuthSourceLocal {
		t.Errorf("AuthSource: got %q, want local", u.AuthSource)
	}
}

// The wire format exposes auth_source so the UI can explain why an account
// behaves differently.
func TestUsers_ResponseIncludesAuthSource(t *testing.T) {
	st := fake.New()
	if _, err := st.CreateUser(context.Background(), store.User{
		ID: uuid.NewString(), Username: "alice", Role: "user",
		AuthSource: store.AuthSourceLDAP, PasswordHash: "!ldap",
	}); err != nil {
		t.Fatal(err)
	}
	seedAuthUser(t, st, "root", "hunter2!", "admin")
	srv := newLDAPServer(t, st, nil, ldap.Config{RoleSource: ldap.RoleSourceDirectory})
	cookie := loginCookie(t, srv, "root", "hunter2!")

	resp := doRequest(t, http.MethodGet, srv.URL+"/api/v1/users", nil, cookie)
	defer resp.Body.Close()
	raw, err := readAll(resp)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(raw), `"auth_source":"ldap"`) {
		t.Errorf("auth_source missing from list response: %s", raw)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// loginCookie logs username/pw in against srv and returns the resulting
// session cookie, failing the test if login does not succeed.
func loginCookie(t *testing.T, srv *httptest.Server, username, pw string) *http.Cookie {
	t.Helper()
	resp := doRequest(t, http.MethodPost, srv.URL+"/api/v1/auth/login",
		map[string]string{"username": username, "password": pw}, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login(%s) status = %d, want 200", username, resp.StatusCode)
	}
	cookie := sessionCookie(resp)
	if cookie == nil {
		t.Fatalf("login(%s) did not set a cookie", username)
	}
	return cookie
}

// readAll reads and returns resp's full body as raw bytes, for tests that
// need to assert on the exact marshaled JSON rather than a decoded struct
// (e.g. checking that no password_hash key/value leaked).
func readAll(resp *http.Response) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// mustReadAll reads resp's full body, failing the test on a read error. For
// tests that only need the body to enrich a t.Fatalf message on an
// already-failing status code.
func mustReadAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	raw, err := readAll(resp)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return raw
}

// assertNoSecretLeak fails the test if raw (a raw HTTP response body) contains
// the password_hash JSON key, or any known password-hash-shaped value
// (argon2id encoded hashes always contain this prefix), asserted on the
// marshaled bytes directly rather than a decoded struct — a decode-based
// check would miss a leak if the DTO used a different key name.
func assertNoSecretLeak(t *testing.T, raw []byte) {
	t.Helper()
	s := string(raw)
	if strings.Contains(s, "password_hash") {
		t.Fatalf("response leaked password_hash key: %s", s)
	}
	if strings.Contains(s, "PasswordHash") {
		t.Fatalf("response leaked PasswordHash key: %s", s)
	}
	if strings.Contains(s, "$argon2id$") {
		t.Fatalf("response leaked an argon2id hash value: %s", s)
	}
}
