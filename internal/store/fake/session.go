// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"context"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// CreateSession implements [store.SessionStore].
func (s *Store) CreateSession(_ context.Context, sess store.Session) (store.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sess.ID] = sess
	return sess, nil
}

// GetSessionByTokenHash implements [store.SessionStore].
func (s *Store) GetSessionByTokenHash(_ context.Context, tokenHash string, now time.Time) (store.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		if sess.TokenHash == tokenHash && sess.ExpiresAt.After(now) {
			return sess, nil
		}
	}
	return store.Session{}, store.ErrNotFound
}

// DeleteSession implements [store.SessionStore].
func (s *Store) DeleteSession(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[id]; !ok {
		return store.ErrNotFound
	}
	delete(s.sessions, id)
	return nil
}

// DeleteSessionsForUser implements [store.SessionStore].
func (s *Store) DeleteSessionsForUser(_ context.Context, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sess := range s.sessions {
		if sess.UserID == userID {
			delete(s.sessions, id)
		}
	}
	return nil
}

// DeleteExpiredSessions implements [store.SessionStore].
func (s *Store) DeleteExpiredSessions(_ context.Context, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, sess := range s.sessions {
		if !sess.ExpiresAt.After(now) {
			delete(s.sessions, id)
			n++
		}
	}
	return n, nil
}
