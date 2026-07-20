// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// asciiEqFold reports whether a and b are equal under ASCII-only
// case-folding. This deliberately mirrors SQLite's `COLLATE NOCASE`, which
// folds only ASCII letters ('A'-'Z') and leaves non-ASCII code points
// (e.g. Greek "Δ"/"δ") untouched — unlike strings.EqualFold, which is
// Unicode-aware and would incorrectly treat "Δelta" and "δelta" as the same
// username.
func asciiEqFold(a, b string) bool {
	return asciiToLower(a) == asciiToLower(b)
}

// asciiToLower lowercases only ASCII letters, leaving all other bytes/runes
// untouched — matching SQLite's ASCII-only NOCASE collation.
func asciiToLower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// CreateUser implements [store.UserStore].
func (s *Store) CreateUser(_ context.Context, u store.User) (store.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[u.ID]; ok {
		return store.User{}, store.ErrConflict
	}
	for _, ex := range s.users {
		if asciiEqFold(ex.Username, u.Username) {
			return store.User{}, store.ErrConflict
		}
	}
	now := time.Now().UTC()
	// Mirror the SQLite column DEFAULT. Without this the fake keeps "" while
	// SQLite yields "local", and the login path branches on exactly this
	// field — so the two stores would disagree about which backend verifies
	// an account.
	if u.AuthSource == "" {
		u.AuthSource = store.AuthSourceLocal
	}
	u.CreatedAt, u.UpdatedAt = now, now
	s.users[u.ID] = u
	return u, nil
}

// GetUser implements [store.UserStore].
func (s *Store) GetUser(_ context.Context, id string) (store.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return store.User{}, store.ErrNotFound
	}
	return u, nil
}

// GetUserByUsername implements [store.UserStore].
func (s *Store) GetUserByUsername(_ context.Context, username string) (store.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.users {
		if asciiEqFold(u.Username, username) {
			return u, nil
		}
	}
	return store.User{}, store.ErrNotFound
}

// ListUsers implements [store.UserStore].
func (s *Store) ListUsers(_ context.Context) ([]store.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	users := make([]store.User, 0, len(s.users))
	for _, u := range s.users {
		users = append(users, u)
	}
	// Case-insensitive to match SQLite's `ORDER BY username` over the
	// COLLATE NOCASE column (see sqlListUsers in internal/store/sqlite/user.go).
	// ASCII-only fold, mirroring SQLite's NOCASE collation.
	slices.SortStableFunc(users, func(a, b store.User) int {
		return strings.Compare(asciiToLower(a.Username), asciiToLower(b.Username))
	})
	return users, nil
}

// UpdateUser implements [store.UserStore].
func (s *Store) UpdateUser(_ context.Context, u store.User) (store.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ex, ok := s.users[u.ID]
	if !ok {
		return store.User{}, store.ErrNotFound
	}
	ex.DisplayName = u.DisplayName
	ex.Role = u.Role
	ex.Disabled = u.Disabled
	ex.UpdatedAt = time.Now().UTC()
	s.users[u.ID] = ex
	return ex, nil
}

// SetUserPassword implements [store.UserStore].
func (s *Store) SetUserPassword(_ context.Context, id, passwordHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return store.ErrNotFound
	}
	u.PasswordHash = passwordHash
	u.UpdatedAt = time.Now().UTC()
	s.users[id] = u
	return nil
}

// SetUserPasswordAndEvictSessions implements [store.UserStore]. The whole
// operation runs under one lock, mirroring SQLite's transaction.
func (s *Store) SetUserPasswordAndEvictSessions(_ context.Context, id, passwordHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return store.ErrNotFound
	}
	u.PasswordHash = passwordHash
	u.UpdatedAt = time.Now().UTC()
	s.users[id] = u
	for sid, sess := range s.sessions {
		if sess.UserID == id {
			delete(s.sessions, sid)
		}
	}
	return nil
}

// SetUserDisplayName implements [store.UserStore].
func (s *Store) SetUserDisplayName(_ context.Context, id, displayName string) (store.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return store.User{}, store.ErrNotFound
	}
	u.DisplayName = displayName
	u.UpdatedAt = time.Now().UTC()
	s.users[id] = u
	return u, nil
}

// DeleteUser implements [store.UserStore].
func (s *Store) DeleteUser(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[id]; !ok {
		return store.ErrNotFound
	}
	delete(s.users, id)
	// Cascade sessions (mirrors the SQLite ON DELETE CASCADE).
	for sid, sess := range s.sessions {
		if sess.UserID == id {
			delete(s.sessions, sid)
		}
	}
	return nil
}

// CountUsers implements [store.UserStore].
func (s *Store) CountUsers(_ context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.users), nil
}

// CountAdmins implements [store.UserStore].
func (s *Store) CountAdmins(_ context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, u := range s.users {
		if u.Role == "admin" && !u.Disabled {
			n++
		}
	}
	return n, nil
}
