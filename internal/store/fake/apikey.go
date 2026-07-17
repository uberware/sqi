// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"context"
	"sort"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// CreateAPIKey implements [store.APIKeyStore].
func (s *Store) CreateAPIKey(_ context.Context, k store.APIKey) (store.APIKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.apiKeys[k.ID]; ok {
		return store.APIKey{}, store.ErrConflict
	}
	// Mirror SQLite's FK to users(id).
	if _, ok := s.users[k.UserID]; !ok {
		return store.APIKey{}, store.ErrConflict
	}
	// Mirror SQLite's token_hash UNIQUE.
	for _, ex := range s.apiKeys {
		if ex.TokenHash == k.TokenHash {
			return store.APIKey{}, store.ErrConflict
		}
	}
	s.apiKeys[k.ID] = k
	return k, nil
}

// GetAPIKeyByTokenHash implements [store.APIKeyStore].
func (s *Store) GetAPIKeyByTokenHash(_ context.Context, tokenHash string, now time.Time) (store.APIKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.apiKeys {
		if k.TokenHash != tokenHash {
			continue
		}
		if k.RevokedAt != nil {
			return store.APIKey{}, store.ErrNotFound
		}
		if k.ExpiresAt != nil && !k.ExpiresAt.After(now) {
			return store.APIKey{}, store.ErrNotFound
		}
		return k, nil
	}
	return store.APIKey{}, store.ErrNotFound
}

// ListAPIKeysForUser implements [store.APIKeyStore].
func (s *Store) ListAPIKeysForUser(_ context.Context, userID string) ([]store.APIKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.APIKey
	for _, k := range s.apiKeys {
		if k.UserID == userID && k.RevokedAt == nil {
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// RevokeAPIKey implements [store.APIKeyStore].
func (s *Store) RevokeAPIKey(_ context.Context, id, userID string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.apiKeys[id]
	if !ok || k.UserID != userID {
		return store.ErrNotFound
	}
	k.RevokedAt = &now
	s.apiKeys[id] = k
	return nil
}

// TouchAPIKeyLastUsed implements [store.APIKeyStore].
func (s *Store) TouchAPIKeyLastUsed(_ context.Context, id string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.apiKeys[id]
	if !ok {
		return store.ErrNotFound
	}
	k.LastUsedAt = &now
	s.apiKeys[id] = k
	return nil
}
