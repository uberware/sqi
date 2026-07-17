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

			if err := st.DeleteSession(ctx, sess.ID); err != nil {
				t.Fatalf("DeleteSession: %v", err)
			}
			if _, err := st.GetSessionByTokenHash(ctx, sess.TokenHash, now); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("revoked session should be ErrNotFound, got %v", err)
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
