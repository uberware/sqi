// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
)

func TestSessionStore_IssueVerifyRevoke(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			u, err := st.CreateUser(ctx, mkUser("user"))
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			now := time.Now().UTC()

			sess := store.Session{
				ID: uuid.NewString(), TokenHash: "hash-" + uuid.NewString(),
				UserID: u.ID, ExpiresAt: now.Add(time.Hour), CreatedAt: now,
			}
			if _, err := st.CreateSession(ctx, sess); err != nil {
				t.Fatalf("CreateSession: %v", err)
			}

			got, err := st.GetSessionByTokenHash(ctx, sess.TokenHash, now)
			if err != nil || got.UserID != u.ID {
				t.Fatalf("GetSessionByTokenHash: %+v err=%v", got, err)
			}

			// Expired session is not returned.
			if _, err := st.GetSessionByTokenHash(ctx, sess.TokenHash, now.Add(2*time.Hour)); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("expired session should be ErrNotFound, got %v", err)
			}

			// Exact-boundary: a lookup at precisely ExpiresAt must be
			// treated as expired. Both backends use a strict comparison
			// (sqlite `expires_at > ?`, fake `ExpiresAt.After(now)`), so
			// equality means expired, not still-valid.
			if _, err := st.GetSessionByTokenHash(ctx, sess.TokenHash, sess.ExpiresAt); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("session at exact ExpiresAt boundary should be ErrNotFound, got %v", err)
			}

			if err := st.DeleteSession(ctx, sess.ID); err != nil {
				t.Fatalf("DeleteSession: %v", err)
			}
			if _, err := st.GetSessionByTokenHash(ctx, sess.TokenHash, now); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("revoked session should be ErrNotFound, got %v", err)
			}
		})
	}
}

// TestSessionStore_CreateSession_OrphanUser verifies that creating a session
// whose user_id references no user is rejected — SQLite via the `REFERENCES
// users(id)` foreign key (PRAGMA foreign_keys=ON), the fake via an explicit
// existence check. Both must surface store.ErrConflict (the FOREIGN KEY
// violation is mapped to that sentinel in sqlite/scan.go).
func TestSessionStore_CreateSession_OrphanUser(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC()
			sess := store.Session{
				ID: uuid.NewString(), TokenHash: "orphan-" + uuid.NewString(),
				UserID:    uuid.NewString(), // no such user exists
				ExpiresAt: now.Add(time.Hour), CreatedAt: now,
			}
			if _, err := st.CreateSession(ctx, sess); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("expected ErrConflict creating session for nonexistent user, got %v", err)
			}
		})
	}
}

// TestSessionStore_CreateSession_DuplicateTokenHash verifies that creating a
// second session with an already-used token_hash is rejected — SQLite via
// `token_hash TEXT NOT NULL UNIQUE`, the fake via an explicit check.
func TestSessionStore_CreateSession_DuplicateTokenHash(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			u, err := st.CreateUser(ctx, mkUser("user"))
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			now := time.Now().UTC()
			tokenHash := "dup-" + uuid.NewString()

			sess1 := store.Session{ID: uuid.NewString(), TokenHash: tokenHash, UserID: u.ID, ExpiresAt: now.Add(time.Hour), CreatedAt: now}
			if _, err := st.CreateSession(ctx, sess1); err != nil {
				t.Fatalf("CreateSession(1): %v", err)
			}

			sess2 := store.Session{ID: uuid.NewString(), TokenHash: tokenHash, UserID: u.ID, ExpiresAt: now.Add(time.Hour), CreatedAt: now}
			if _, err := st.CreateSession(ctx, sess2); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("expected ErrConflict on duplicate token_hash, got %v", err)
			}
		})
	}
}

func TestSessionStore_DeleteForUserAndExpired(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			u, err := st.CreateUser(ctx, mkUser("user"))
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			now := time.Now().UTC()
			live := store.Session{ID: uuid.NewString(), TokenHash: "live", UserID: u.ID, ExpiresAt: now.Add(time.Hour), CreatedAt: now}
			dead := store.Session{ID: uuid.NewString(), TokenHash: "dead", UserID: u.ID, ExpiresAt: now.Add(-time.Hour), CreatedAt: now}
			if _, err := st.CreateSession(ctx, live); err != nil {
				t.Fatalf("CreateSession(live): %v", err)
			}
			if _, err := st.CreateSession(ctx, dead); err != nil {
				t.Fatalf("CreateSession(dead): %v", err)
			}

			n, err := st.DeleteExpiredSessions(ctx, now)
			if err != nil || n != 1 {
				t.Fatalf("DeleteExpiredSessions: n=%d err=%v", n, err)
			}
			if err := st.DeleteSessionsForUser(ctx, u.ID); err != nil {
				t.Fatalf("DeleteSessionsForUser: %v", err)
			}
			if _, err := st.GetSessionByTokenHash(ctx, "live", now); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("session should be gone after DeleteSessionsForUser, got %v", err)
			}
		})
	}
}

// TestSessionStore_CreateSession_DuplicateID pins the fake to SQLite's
// `id TEXT PRIMARY KEY`: re-using a session ID is a conflict, not a silent
// overwrite of the existing session.
func TestSessionStore_CreateSession_DuplicateID(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			u, err := st.CreateUser(ctx, mkUser("user"))
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			now := time.Now().UTC()
			id := uuid.NewString()
			first := store.Session{
				ID: id, TokenHash: "hash-" + uuid.NewString(),
				UserID: u.ID, ExpiresAt: now.Add(time.Hour), CreatedAt: now,
			}
			if _, err := st.CreateSession(ctx, first); err != nil {
				t.Fatalf("CreateSession (first): %v", err)
			}

			dup := first
			dup.TokenHash = "hash-" + uuid.NewString() // unique token, same ID
			if _, err := st.CreateSession(ctx, dup); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("duplicate session ID should be ErrConflict, got %v", err)
			}

			// The original session must survive the rejected insert.
			if _, err := st.GetSessionByTokenHash(ctx, first.TokenHash, now); err != nil {
				t.Fatalf("original session should still resolve: %v", err)
			}
		})
	}
}

// TestSessionStore_GetSessionUserByTokenHash covers the joined lookup used on
// every cookie-authenticated request. It runs against both backends so the
// fake cannot drift from SQLite's JOIN semantics.
func TestSessionStore_GetSessionUserByTokenHash(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			u, err := st.CreateUser(ctx, mkUser("operator"))
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			now := time.Now().UTC()
			hash := "hash-" + uuid.NewString()
			if _, err := st.CreateSession(ctx, store.Session{
				ID: uuid.NewString(), TokenHash: hash, UserID: u.ID,
				ExpiresAt: now.Add(time.Hour), CreatedAt: now,
			}); err != nil {
				t.Fatalf("CreateSession: %v", err)
			}

			// Every user column must survive the join — the SQLite select
			// lists them positionally, so a reordering would silently swap
			// fields rather than fail.
			got, err := st.GetSessionUserByTokenHash(ctx, hash, now)
			if err != nil {
				t.Fatalf("GetSessionUserByTokenHash: %v", err)
			}
			if got.ID != u.ID || got.Username != u.Username || got.Role != u.Role {
				t.Fatalf("user = %+v, want id/username/role of %+v", got, u)
			}
			if got.DisplayName != u.DisplayName || got.PasswordHash != u.PasswordHash {
				t.Fatalf("display_name/password_hash did not survive the join: %+v", got)
			}
			if got.Disabled {
				t.Error("Disabled = true, want false")
			}
			if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
				t.Errorf("timestamps did not survive the join: created=%v updated=%v",
					got.CreatedAt, got.UpdatedAt)
			}

			// An expired session resolves to nobody, matching
			// GetSessionByTokenHash's own expiry rule.
			if _, err := st.GetSessionUserByTokenHash(ctx, hash, now.Add(2*time.Hour)); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("expired session: err = %v, want ErrNotFound", err)
			}
			if _, err := st.GetSessionUserByTokenHash(ctx, "no-such-hash", now); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("unknown hash: err = %v, want ErrNotFound", err)
			}

			// A disabled user is still returned — the authenticator, not the
			// store, decides what disabled means.
			u.Disabled = true
			if _, err := st.UpdateUser(ctx, u); err != nil {
				t.Fatalf("UpdateUser: %v", err)
			}
			got, err = st.GetSessionUserByTokenHash(ctx, hash, now)
			if err != nil {
				t.Fatalf("GetSessionUserByTokenHash after disable: %v", err)
			}
			if !got.Disabled {
				t.Error("Disabled = false, want true (the store must report it, not filter it)")
			}
		})
	}
}

// TestSessionStore_GetSessionUserByTokenHash_DeletedUser pins the join's
// behavior when the user row is gone: the session must resolve to nobody
// rather than to a zero-valued user.
func TestSessionStore_GetSessionUserByTokenHash_DeletedUser(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			u, err := st.CreateUser(ctx, mkUser("user"))
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			now := time.Now().UTC()
			hash := "hash-" + uuid.NewString()
			if _, err := st.CreateSession(ctx, store.Session{
				ID: uuid.NewString(), TokenHash: hash, UserID: u.ID,
				ExpiresAt: now.Add(time.Hour), CreatedAt: now,
			}); err != nil {
				t.Fatalf("CreateSession: %v", err)
			}
			if err := st.DeleteUser(ctx, u.ID); err != nil {
				t.Fatalf("DeleteUser: %v", err)
			}

			if _, err := st.GetSessionUserByTokenHash(ctx, hash, now); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("err = %v, want ErrNotFound after the user row is gone", err)
			}
		})
	}
}
