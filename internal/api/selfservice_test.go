// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/auth"
	"github.com/uberware/sqi/internal/auth/ldap"
	"github.com/uberware/sqi/internal/auth/password"
	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// seedAPIKey creates an API key owned by userID and returns it.
func seedAPIKey(t *testing.T, st store.Store, userID, name string) store.APIKey {
	t.Helper()
	tok, err := password.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	k, err := st.CreateAPIKey(t.Context(), store.APIKey{
		ID:        uuid.NewString(),
		UserID:    userID,
		Name:      name,
		Prefix:    tok[:apiKeyPrefixLen],
		TokenHash: password.HashToken(tok),
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	return k
}

func newTestAuthHandler(t *testing.T, st store.Store) *authHandler {
	t.Helper()
	return newAuthHandler(st, slog.New(slog.DiscardHandler), time.Hour, "sqi_session", "false", nil, ldap.Config{})
}

// principalFor builds the context principal middleware.Auth would attach for u.
func principalFor(u store.User) auth.Principal {
	return auth.Principal{
		Subject:  u.ID,
		Username: u.Username,
		Kind:     auth.KindUser,
		Roles:    []string{u.Role},
	}
}

func TestChangePassword(t *testing.T) {
	t.Run("succeeds with the correct current password", func(t *testing.T) {
		st := fake.New()
		u := seedAuthUser(t, st, "alice", "old-password", "user")
		h := newTestAuthHandler(t, st)

		rr := doChangePassword(t, h, u, `{"current_password":"old-password","new_password":"new-password"}`)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204 (body %s)", rr.Code, rr.Body)
		}

		updated, err := st.GetUser(t.Context(), u.ID)
		if err != nil {
			t.Fatalf("GetUser: %v", err)
		}
		ok, err := password.Verify(updated.PasswordHash, "new-password")
		if err != nil || !ok {
			t.Fatalf("new password does not verify (ok=%v err=%v)", ok, err)
		}
		if len(rr.Result().Cookies()) == 0 {
			t.Fatal("expected a re-issued session cookie")
		}
	})

	t.Run("wrong current password is 403, not 401", func(t *testing.T) {
		st := fake.New()
		u := seedAuthUser(t, st, "alice", "old-password", "user")
		h := newTestAuthHandler(t, st)

		rr := doChangePassword(t, h, u, `{"current_password":"wrong","new_password":"new-password"}`)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (401 would trip the web login interceptor)", rr.Code)
		}

		unchanged, err := st.GetUser(t.Context(), u.ID)
		if err != nil {
			t.Fatalf("GetUser: %v", err)
		}
		stillOld, verr := password.Verify(unchanged.PasswordHash, "old-password")
		if verr != nil {
			t.Fatalf("Verify: %v", verr)
		}
		if !stillOld {
			t.Fatal("password must be unchanged after a failed attempt")
		}
	})

	t.Run("empty new password is 400", func(t *testing.T) {
		st := fake.New()
		u := seedAuthUser(t, st, "alice", "old-password", "user")
		h := newTestAuthHandler(t, st)

		rr := doChangePassword(t, h, u, `{"current_password":"old-password","new_password":""}`)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rr.Code)
		}
	})

	t.Run("other sessions are destroyed but the caller stays signed in", func(t *testing.T) {
		st := fake.New()
		u := seedAuthUser(t, st, "alice", "old-password", "user")
		now := time.Now().UTC()
		if _, err := st.CreateSession(t.Context(), store.Session{
			ID:        "other-session",
			TokenHash: password.HashToken("other-token"),
			UserID:    u.ID,
			ExpiresAt: now.Add(time.Hour),
			CreatedAt: now,
		}); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		h := newTestAuthHandler(t, st)

		rr := doChangePassword(t, h, u, `{"current_password":"old-password","new_password":"new-password"}`)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204 (body %s)", rr.Code, rr.Body)
		}

		if _, err := st.GetSessionByTokenHash(t.Context(), password.HashToken("other-token"), now); err == nil {
			t.Fatal("the other device's session should have been destroyed")
		}

		// The cookie handed back must resolve to a live session, or the caller
		// would be signed out by their own password change.
		cookies := rr.Result().Cookies()
		if len(cookies) == 0 {
			t.Fatal("expected a re-issued session cookie")
		}
		if _, err := st.GetSessionByTokenHash(t.Context(), password.HashToken(cookies[0].Value), now); err != nil {
			t.Fatalf("re-issued cookie does not resolve to a live session: %v", err)
		}
	})

	t.Run("API keys are deliberately not revoked", func(t *testing.T) {
		st := fake.New()
		u := seedAuthUser(t, st, "alice", "old-password", "user")
		seedAPIKey(t, st, u.ID, "ci-key")
		h := newTestAuthHandler(t, st)

		rr := doChangePassword(t, h, u, `{"current_password":"old-password","new_password":"new-password"}`)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", rr.Code)
		}

		keys, err := st.ListAPIKeysForUser(t.Context(), u.ID)
		if err != nil {
			t.Fatalf("ListAPIKeysForUser: %v", err)
		}
		if len(keys) != 1 {
			t.Fatalf("got %d keys, want the key to survive a password change", len(keys))
		}
	})

	t.Run("auth disabled is 409", func(t *testing.T) {
		st := fake.New()
		h := newTestAuthHandler(t, st)

		req := httptest.NewRequestWithContext(
			auth.NewContext(t.Context(), auth.Principal{Kind: auth.KindAnonymous}),
			http.MethodPut, "/auth/password",
			bytes.NewBufferString(`{"current_password":"x","new_password":"y"}`),
		)
		rr := httptest.NewRecorder()
		h.changePassword(rr, req)

		if rr.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (matches the /api-keys auth-off precedent)", rr.Code)
		}
	})
}

// doChangePassword issues the request as the given user.
func doChangePassword(t *testing.T, h *authHandler, u store.User, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(
		auth.NewContext(t.Context(), principalFor(u)),
		http.MethodPut, "/auth/password", bytes.NewBufferString(body),
	)
	rr := httptest.NewRecorder()
	h.changePassword(rr, req)
	return rr
}

func TestUpdateMe(t *testing.T) {
	t.Run("updates the display name", func(t *testing.T) {
		st := fake.New()
		u := seedAuthUser(t, st, "alice", "pw", "user")
		h := newTestAuthHandler(t, st)

		rr := doUpdateMe(t, h, u, `{"display_name":"Alice A."}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body)
		}

		updated, err := st.GetUser(t.Context(), u.ID)
		if err != nil {
			t.Fatalf("GetUser: %v", err)
		}
		if updated.DisplayName != "Alice A." {
			t.Fatalf("DisplayName = %q, want %q", updated.DisplayName, "Alice A.")
		}

		var body principalResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.DisplayName != "Alice A." {
			t.Fatalf("response DisplayName = %q, want the updated value", body.DisplayName)
		}
	})

	// The highest-value test in B3: a self-service route that can reach `role`
	// is a privilege-escalation hole. `role`, `disabled`, and `username` are
	// absent from the request struct, so a body carrying them must be inert.
	t.Run("cannot escalate role, rename, or re-enable the account", func(t *testing.T) {
		st := fake.New()
		u := seedAuthUser(t, st, "alice", "pw", "user")
		h := newTestAuthHandler(t, st)

		rr := doUpdateMe(t, h, u,
			`{"display_name":"Alice","role":"admin","disabled":false,"username":"root"}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body)
		}

		updated, err := st.GetUser(t.Context(), u.ID)
		if err != nil {
			t.Fatalf("GetUser: %v", err)
		}
		if updated.Role != "user" {
			t.Fatalf("Role = %q, want it untouched at %q", updated.Role, "user")
		}
		if updated.Username != "alice" {
			t.Fatalf("Username = %q, want it untouched", updated.Username)
		}
	})

	// A disabled account must not be able to un-disable itself. Guarding the
	// field is not enough if the round-trip through UpdateUser resets it.
	t.Run("a disabled account stays disabled", func(t *testing.T) {
		st := fake.New()
		u := seedAuthUser(t, st, "alice", "pw", "user")
		u.Disabled = true
		if _, err := st.UpdateUser(t.Context(), u); err != nil {
			t.Fatalf("UpdateUser: %v", err)
		}
		h := newTestAuthHandler(t, st)

		if rr := doUpdateMe(t, h, u, `{"display_name":"Alice","disabled":false}`); rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body)
		}

		updated, err := st.GetUser(t.Context(), u.ID)
		if err != nil {
			t.Fatalf("GetUser: %v", err)
		}
		if !updated.Disabled {
			t.Fatal("Disabled = false, want the account to stay disabled")
		}
	})

	// The reviewer's race is a read-modify-write window that cannot be opened
	// deterministically from outside the handler, so this pins the property
	// that closes it instead: updateMe must reach the store ONLY through the
	// single-column SetUserDisplayName. UpdateUser writes display_name, role,
	// and disabled together, so any call to it here is a route by which a
	// concurrent admin demotion could be reverted.
	t.Run("writes only the display-name column, never a whole-row update", func(t *testing.T) {
		spy := &userWriteSpy{Store: fake.New()}
		u := seedAuthUser(t, spy, "alice", "pw", "admin")
		h := newTestAuthHandler(t, spy)

		if rr := doUpdateMe(t, h, u, `{"display_name":"Alice A."}`); rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body)
		}

		if spy.updateUserCalls != 0 {
			t.Errorf("UpdateUser called %d time(s); updateMe must not write role/disabled",
				spy.updateUserCalls)
		}
		if spy.setDisplayNameCalls != 1 {
			t.Errorf("SetUserDisplayName called %d time(s), want 1", spy.setDisplayNameCalls)
		}

		after, err := spy.GetUser(t.Context(), u.ID)
		if err != nil {
			t.Fatalf("GetUser: %v", err)
		}
		if after.DisplayName != "Alice A." || after.Role != "admin" || after.Disabled {
			t.Errorf("user = %+v, want only display_name changed", after)
		}
	})

	t.Run("auth disabled is 409", func(t *testing.T) {
		st := fake.New()
		h := newTestAuthHandler(t, st)

		req := httptest.NewRequestWithContext(
			auth.NewContext(t.Context(), auth.Principal{Kind: auth.KindAnonymous}),
			http.MethodPatch, "/auth/me", bytes.NewBufferString(`{"display_name":"x"}`),
		)
		rr := httptest.NewRecorder()
		h.updateMe(rr, req)

		if rr.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rr.Code)
		}
	})
}

func doUpdateMe(t *testing.T, h *authHandler, u store.User, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(
		auth.NewContext(t.Context(), principalFor(u)),
		http.MethodPatch, "/auth/me", bytes.NewBufferString(body),
	)
	rr := httptest.NewRecorder()
	h.updateMe(rr, req)
	return rr
}

// userWriteSpy counts which user-write path a handler takes. It exists to pin
// updateMe to the single-column update: the distinction matters for
// correctness (UpdateUser also writes role and disabled) but is invisible in
// the resulting row, so it cannot be asserted on state alone.
type userWriteSpy struct {
	store.Store

	updateUserCalls     int
	setDisplayNameCalls int
}

func (s *userWriteSpy) UpdateUser(ctx context.Context, u store.User) (store.User, error) {
	s.updateUserCalls++
	return s.Store.UpdateUser(ctx, u)
}

func (s *userWriteSpy) SetUserDisplayName(ctx context.Context, id, displayName string) (store.User, error) {
	s.setDisplayNameCalls++
	return s.Store.SetUserDisplayName(ctx, id, displayName)
}

// failingPasswordStore makes the atomic password+eviction write fail.
type failingPasswordStore struct {
	store.Store
}

func (failingPasswordStore) SetUserPasswordAndEvictSessions(context.Context, string, string) error {
	return errors.New("store unavailable")
}

// TestChangePassword_EvictionFailureIsReported pins the contract the UI
// depends on: the success response claims other devices were signed out, so a
// store failure must surface as an error rather than a 204. Because the write
// is atomic, a 500 also means the password itself is unchanged and the caller
// can safely retry.
func TestChangePassword_EvictionFailureIsReported(t *testing.T) {
	base := fake.New()
	u := seedAuthUser(t, base, "alice", "old-password", "user")
	h := newTestAuthHandler(t, failingPasswordStore{Store: base})

	rr := doChangePassword(t, h, u, `{"current_password":"old-password","new_password":"new-password"}`)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 when the password/eviction write fails", rr.Code)
	}
	if len(rr.Result().Cookies()) != 0 {
		t.Error("a failed change must not re-issue a session cookie")
	}

	// Atomicity: the old password must still work.
	unchanged, err := base.GetUser(t.Context(), u.ID)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	stillOld, verr := password.Verify(unchanged.PasswordHash, "old-password")
	if verr != nil || !stillOld {
		t.Fatalf("password changed despite the reported failure (ok=%v err=%v)", stillOld, verr)
	}
}
