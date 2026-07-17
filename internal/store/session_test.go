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
