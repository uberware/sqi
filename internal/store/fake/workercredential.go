// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"context"
	"sort"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// CreateWorkerCredential implements [store.WorkerCredentialStore].
func (s *Store) CreateWorkerCredential(_ context.Context, c store.WorkerCredential) (store.WorkerCredential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.workerCredentials[c.ID]; ok {
		return store.WorkerCredential{}, store.ErrConflict
	}
	for _, ex := range s.workerCredentials {
		// Mirror SQLite's worker_id and public_key UNIQUE constraints.
		if ex.WorkerID == c.WorkerID || ex.PublicKey == c.PublicKey {
			return store.WorkerCredential{}, store.ErrConflict
		}
	}
	s.workerCredentials[c.ID] = c
	return c, nil
}

// GetWorkerCredentialByWorkerID implements [store.WorkerCredentialStore].
func (s *Store) GetWorkerCredentialByWorkerID(_ context.Context, workerID string) (store.WorkerCredential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.workerCredentials {
		if c.WorkerID == workerID {
			return c, nil
		}
	}
	return store.WorkerCredential{}, store.ErrNotFound
}

// ListActiveWorkerCredentials implements [store.WorkerCredentialStore].
func (s *Store) ListActiveWorkerCredentials(_ context.Context) ([]store.WorkerCredential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.WorkerCredential
	for _, c := range s.workerCredentials {
		if c.RevokedAt == nil {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EnrolledAt.Before(out[j].EnrolledAt) })
	return out, nil
}

// RevokeWorkerCredential implements [store.WorkerCredentialStore].
func (s *Store) RevokeWorkerCredential(_ context.Context, workerID string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, c := range s.workerCredentials {
		if c.WorkerID != workerID {
			continue
		}
		if c.RevokedAt != nil {
			return store.ErrNotFound
		}
		c.RevokedAt = &at
		s.workerCredentials[id] = c
		return nil
	}
	return store.ErrNotFound
}

// CreateWorkerJoinToken implements [store.WorkerCredentialStore].
func (s *Store) CreateWorkerJoinToken(_ context.Context, t store.WorkerJoinToken) (store.WorkerJoinToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.workerJoinTokens[t.ID]; ok {
		return store.WorkerJoinToken{}, store.ErrConflict
	}
	for _, ex := range s.workerJoinTokens {
		if ex.TokenHash == t.TokenHash {
			return store.WorkerJoinToken{}, store.ErrConflict
		}
	}
	s.workerJoinTokens[t.ID] = t
	return t, nil
}

// GetWorkerJoinTokenByHash implements [store.WorkerCredentialStore].
func (s *Store) GetWorkerJoinTokenByHash(_ context.Context, hash string) (store.WorkerJoinToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.workerJoinTokens {
		if t.TokenHash == hash {
			return t, nil
		}
	}
	return store.WorkerJoinToken{}, store.ErrNotFound
}

// MarkWorkerJoinTokenUsed implements [store.WorkerCredentialStore].
func (s *Store) MarkWorkerJoinTokenUsed(_ context.Context, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.workerJoinTokens[id]
	if !ok {
		return store.ErrNotFound
	}
	t.UsedAt = &at
	s.workerJoinTokens[id] = t
	return nil
}
