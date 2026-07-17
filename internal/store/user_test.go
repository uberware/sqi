// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
	"github.com/uberware/sqi/internal/store/sqlite"
)

func newStores(t *testing.T) map[string]store.Store {
	t.Helper()
	db := t.TempDir() + "/test.db"
	sq, err := sqlite.Open(context.Background(), db, sqlite.DefaultOptions())
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = sq.Close() })
	return map[string]store.Store{"sqlite": sq, "fake": fake.New()}
}

func mkUser(role string) store.User {
	return store.User{
		ID: uuid.NewString(), Username: "alice-" + uuid.NewString()[:8],
		DisplayName: "Alice", PasswordHash: "$argon2id$stub", Role: role,
	}
}

func TestUserStore_CRUD(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			u := mkUser("admin")

			created, err := st.CreateUser(ctx, u)
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			if created.CreatedAt.IsZero() {
				t.Fatal("CreatedAt not set")
			}

			got, err := st.GetUser(ctx, u.ID)
			if err != nil || got.Username != u.Username {
				t.Fatalf("GetUser: %+v err=%v", got, err)
			}

			byName, err := st.GetUserByUsername(ctx, u.Username)
			if err != nil || byName.ID != u.ID {
				t.Fatalf("GetUserByUsername: %+v err=%v", byName, err)
			}

			// Case-insensitive username lookup + uniqueness.
			if _, err := st.GetUserByUsername(ctx, upper(u.Username)); err != nil {
				t.Fatalf("case-insensitive lookup failed: %v", err)
			}
			dup := mkUser("user")
			dup.Username = upper(u.Username)
			if _, err := st.CreateUser(ctx, dup); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("expected ErrConflict on duplicate username, got %v", err)
			}

			got.DisplayName = "Alice B"
			got.Role = "operator"
			got.Disabled = true
			upd, err := st.UpdateUser(ctx, got)
			if err != nil || upd.DisplayName != "Alice B" || upd.Role != "operator" || !upd.Disabled {
				t.Fatalf("UpdateUser: %+v err=%v", upd, err)
			}

			if err := st.SetUserPassword(ctx, u.ID, "$argon2id$new"); err != nil {
				t.Fatalf("SetUserPassword: %v", err)
			}
			after, err := st.GetUser(ctx, u.ID)
			if err != nil {
				t.Fatalf("GetUser: %v", err)
			}
			if after.PasswordHash != "$argon2id$new" {
				t.Fatalf("password not updated: %q", after.PasswordHash)
			}

			n, err := st.CountUsers(ctx)
			if err != nil || n != 1 {
				t.Fatalf("CountUsers: %d err=%v", n, err)
			}

			if err := st.DeleteUser(ctx, u.ID); err != nil {
				t.Fatalf("DeleteUser: %v", err)
			}
			if _, err := st.GetUser(ctx, u.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("expected ErrNotFound after delete, got %v", err)
			}
		})
	}
}

// TestUserStore_DeleteCascadesSessions verifies that deleting a user removes
// their sessions too — SQLite via the `ON DELETE CASCADE` foreign key
// (requires PRAGMA foreign_keys=ON, set in sqlite.Open), the fake via its
// manual mirror in DeleteUser.
func TestUserStore_DeleteCascadesSessions(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			u, err := st.CreateUser(ctx, mkUser("user"))
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			now := time.Now().UTC()
			sess := store.Session{
				ID: uuid.NewString(), TokenHash: "cascade-" + uuid.NewString(),
				UserID: u.ID, ExpiresAt: now.Add(time.Hour), CreatedAt: now,
			}
			if _, err := st.CreateSession(ctx, sess); err != nil {
				t.Fatalf("CreateSession: %v", err)
			}

			if err := st.DeleteUser(ctx, u.ID); err != nil {
				t.Fatalf("DeleteUser: %v", err)
			}

			if _, err := st.GetSessionByTokenHash(ctx, sess.TokenHash, now); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("expected session to be cascade-deleted with its user, got %v", err)
			}
		})
	}
}

func upper(s string) string {
	// simple ASCII upper for the test username
	b := []byte(s)
	for i := range b {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 32
		}
	}
	return string(b)
}
