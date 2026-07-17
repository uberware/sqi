// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/uberware/sqi/internal/store"
)

func eqFold(a, b string) bool { return strings.EqualFold(a, b) }

// CreateUser implements [store.UserStore].
func (s *Store) CreateUser(_ context.Context, u store.User) (store.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ex := range s.users {
		if eqFold(ex.Username, u.Username) {
			return store.User{}, store.ErrConflict
		}
	}
	now := time.Now().UTC()
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
		if eqFold(u.Username, username) {
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
	slices.SortStableFunc(users, func(a, b store.User) int {
		return strings.Compare(a.Username, b.Username)
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
