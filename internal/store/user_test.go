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
			// UpdateUser is scoped to display_name/role/disabled — guard
			// against a future regression touching username or password.
			if upd.Username != u.Username {
				t.Fatalf("UpdateUser must not change Username: got %q, want %q", upd.Username, u.Username)
			}
			if upd.PasswordHash != u.PasswordHash {
				t.Fatalf("UpdateUser must not change PasswordHash: got %q, want %q", upd.PasswordHash, u.PasswordHash)
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

// TestCountAdmins verifies CountAdmins counts only enabled accounts with
// role "admin" — disabled admins and non-admins are excluded.
func TestCountAdmins(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()

			if n, err := st.CountAdmins(ctx); err != nil || n != 0 {
				t.Fatalf("empty store: CountAdmins = %d, %v; want 0, nil", n, err)
			}

			active := mkUser("admin")
			if _, err := st.CreateUser(ctx, active); err != nil {
				t.Fatalf("CreateUser(active admin): %v", err)
			}

			disabledAdmin := mkUser("admin")
			disabledAdmin.Username = "bob-" + uuid.NewString()[:8]
			disabledAdmin.Disabled = true
			if _, err := st.CreateUser(ctx, disabledAdmin); err != nil {
				t.Fatalf("CreateUser(disabled admin): %v", err)
			}

			operator := mkUser("operator")
			operator.Username = "carol-" + uuid.NewString()[:8]
			if _, err := st.CreateUser(ctx, operator); err != nil {
				t.Fatalf("CreateUser(operator): %v", err)
			}

			if n, err := st.CountAdmins(ctx); err != nil || n != 1 {
				t.Fatalf("CountAdmins = %d, %v; want 1, nil", n, err)
			}
		})
	}
}

// TestUserStore_ListUsers verifies ListUsers returns users ordered
// case-insensitively by username — matching SQLite's `ORDER BY username`
// over the `username` column, which is declared COLLATE NOCASE (see
// sqlListUsers in internal/store/sqlite/user.go). The fake store must sort
// the same way; a case-sensitive ASCII sort would put "Zeta" before "bob"
// (since 'Z' < 'b' in ASCII), diverging from SQLite.
func TestUserStore_ListUsers(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			for _, username := range []string{"Zeta", "bob", "Alice"} {
				u := mkUser("user")
				u.Username = username
				if _, err := st.CreateUser(ctx, u); err != nil {
					t.Fatalf("CreateUser(%q): %v", username, err)
				}
			}

			users, err := st.ListUsers(ctx)
			if err != nil {
				t.Fatalf("ListUsers: %v", err)
			}
			if len(users) != 3 {
				t.Fatalf("ListUsers: got %d users, want 3", len(users))
			}
			got := []string{users[0].Username, users[1].Username, users[2].Username}
			want := []string{"Alice", "bob", "Zeta"}
			if got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
				t.Fatalf("ListUsers order = %v, want %v (case-insensitive sort)", got, want)
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

// TestUserStore_CreateUser_DuplicateID verifies that creating a user whose ID
// collides with an existing user is rejected — SQLite via `id TEXT PRIMARY
// KEY`, the fake via an explicit check. Without this check the fake would
// silently overwrite the existing user's password hash and role instead of
// returning store.ErrConflict like SQLite does.
func TestUserStore_CreateUser_DuplicateID(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			u := mkUser("admin")
			if _, err := st.CreateUser(ctx, u); err != nil {
				t.Fatalf("CreateUser: %v", err)
			}

			dup := mkUser("user")
			dup.ID = u.ID // same ID, different username/role/password.
			if _, err := st.CreateUser(ctx, dup); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("expected ErrConflict on duplicate ID, got %v", err)
			}

			// The original user must be unchanged, not overwritten.
			got, err := st.GetUser(ctx, u.ID)
			if err != nil {
				t.Fatalf("GetUser: %v", err)
			}
			if got.Username != u.Username || got.PasswordHash != u.PasswordHash || got.Role != u.Role {
				t.Fatalf("duplicate-ID create must not overwrite existing user: got %+v, want %+v", got, u)
			}
		})
	}
}

// TestUserStore_CreateUser_AsciiFoldOnly verifies that username uniqueness
// folds ASCII case only, matching SQLite's `COLLATE NOCASE` (which is
// ASCII-only, unlike Go's Unicode-aware strings.EqualFold). "Δelta" and
// "δelta" differ only by a non-ASCII Greek letter case, so both must be
// accepted as distinct usernames.
func TestUserStore_CreateUser_AsciiFoldOnly(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			u1 := mkUser("user")
			u1.Username = "Δelta"
			if _, err := st.CreateUser(ctx, u1); err != nil {
				t.Fatalf("CreateUser(Δelta): %v", err)
			}

			u2 := mkUser("user")
			u2.Username = "δelta"
			if _, err := st.CreateUser(ctx, u2); err != nil {
				t.Fatalf("CreateUser(δelta) should succeed since SQLite COLLATE NOCASE is ASCII-only: %v", err)
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

// TestUserStore_SetUserPasswordAndEvictSessions covers the atomic pairing the
// self-service password change depends on: the client is told other devices
// were signed out, so the two writes must not be separable.
func TestUserStore_SetUserPasswordAndEvictSessions(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			u, err := st.CreateUser(ctx, mkUser("user"))
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			other, err := st.CreateUser(ctx, mkUser("user"))
			if err != nil {
				t.Fatalf("CreateUser(other): %v", err)
			}
			now := time.Now().UTC()
			mkSession := func(owner store.User, hash string) {
				t.Helper()
				if _, err := st.CreateSession(ctx, store.Session{
					ID: uuid.NewString(), TokenHash: hash, UserID: owner.ID,
					ExpiresAt: now.Add(time.Hour), CreatedAt: now,
				}); err != nil {
					t.Fatalf("CreateSession: %v", err)
				}
			}
			mkSession(u, "h-a")
			mkSession(u, "h-b")
			mkSession(other, "h-other")

			if err := st.SetUserPasswordAndEvictSessions(ctx, u.ID, "$argon2id$new"); err != nil {
				t.Fatalf("SetUserPasswordAndEvictSessions: %v", err)
			}

			got, err := st.GetUser(ctx, u.ID)
			if err != nil {
				t.Fatalf("GetUser: %v", err)
			}
			if got.PasswordHash != "$argon2id$new" {
				t.Errorf("PasswordHash = %q, want the new hash", got.PasswordHash)
			}
			for _, h := range []string{"h-a", "h-b"} {
				if _, err := st.GetSessionByTokenHash(ctx, h, now); !errors.Is(err, store.ErrNotFound) {
					t.Errorf("session %s: err = %v, want ErrNotFound", h, err)
				}
			}
			// Another account's sessions must be untouched.
			if _, err := st.GetSessionByTokenHash(ctx, "h-other", now); err != nil {
				t.Errorf("another user's session was evicted: %v", err)
			}

			// An unknown id changes nothing and reports it.
			if err := st.SetUserPasswordAndEvictSessions(ctx, "no-such-user", "$argon2id$x"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("unknown id: err = %v, want ErrNotFound", err)
			}
		})
	}
}

// TestUserStore_SetUserDisplayName pins that the targeted update touches only
// the display name — the whole point of it existing beside UpdateUser.
func TestUserStore_SetUserDisplayName(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			u, err := st.CreateUser(ctx, mkUser("operator"))
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}

			updated, err := st.SetUserDisplayName(ctx, u.ID, "New Name")
			if err != nil {
				t.Fatalf("SetUserDisplayName: %v", err)
			}
			if updated.DisplayName != "New Name" {
				t.Errorf("DisplayName = %q, want %q", updated.DisplayName, "New Name")
			}
			if updated.Role != u.Role {
				t.Errorf("Role = %q, want it untouched at %q", updated.Role, u.Role)
			}
			if updated.Disabled != u.Disabled {
				t.Errorf("Disabled = %v, want it untouched at %v", updated.Disabled, u.Disabled)
			}
			if updated.Username != u.Username || updated.PasswordHash != u.PasswordHash {
				t.Errorf("username/password_hash changed: %+v", updated)
			}

			// The load-bearing property: an admin change that lands between a
			// caller's read and their save must survive the save.
			u.Role = "read-only"
			u.Disabled = true
			if _, err := st.UpdateUser(ctx, u); err != nil {
				t.Fatalf("UpdateUser (admin demote+disable): %v", err)
			}
			if _, err := st.SetUserDisplayName(ctx, u.ID, "Renamed Again"); err != nil {
				t.Fatalf("SetUserDisplayName: %v", err)
			}
			after, err := st.GetUser(ctx, u.ID)
			if err != nil {
				t.Fatalf("GetUser: %v", err)
			}
			if after.Role != "read-only" || !after.Disabled {
				t.Fatalf("a display-name save reverted the admin change: role=%q disabled=%v",
					after.Role, after.Disabled)
			}
			if after.DisplayName != "Renamed Again" {
				t.Errorf("DisplayName = %q, want %q", after.DisplayName, "Renamed Again")
			}

			if _, err := st.SetUserDisplayName(ctx, "no-such-user", "x"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("unknown id: err = %v, want ErrNotFound", err)
			}
		})
	}
}
