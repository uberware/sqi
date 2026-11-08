// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"context"
	"sort"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// CreateWorkerCredential implements [store.WorkerCredentialStore].
func (s *Store) CreateWorkerCredential(ctx context.Context, c store.WorkerCredential) (store.WorkerCredential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fenceLocked(ctx); err != nil {
		return store.WorkerCredential{}, err
	}
	if _, ok := s.workerCredentials[c.ID]; ok {
		return store.WorkerCredential{}, store.ErrConflict
	}
	for _, ex := range s.workerCredentials {
		// Mirror SQLite's constraints: public_key is UNIQUE across every row,
		// active or revoked, but worker_id is only unique among ACTIVE rows
		// (worker_credentials_active, a partial index) — a revoked row must
		// never block the same worker ID from enrolling again with a new key.
		if ex.PublicKey == c.PublicKey {
			return store.WorkerCredential{}, store.ErrConflict
		}
		if ex.WorkerID == c.WorkerID && ex.RevokedAt == nil {
			return store.WorkerCredential{}, store.ErrConflict
		}
	}
	s.workerCredentials[c.ID] = c
	return c, nil
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

// GetActiveWorkerCredentialByPublicKey implements [store.WorkerCredentialStore].
func (s *Store) GetActiveWorkerCredentialByPublicKey(_ context.Context, publicKey string) (store.WorkerCredential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.workerCredentials {
		if c.PublicKey == publicKey && c.RevokedAt == nil {
			return c, nil
		}
	}
	return store.WorkerCredential{}, store.ErrNotFound
}

// RevokeWorkerCredential implements [store.WorkerCredentialStore].
func (s *Store) RevokeWorkerCredential(ctx context.Context, workerID string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fenceLocked(ctx); err != nil {
		return err
	}
	// Scan the WHOLE map before giving up. Go's map iteration order is
	// randomized, and after a key rotation a worker can legitimately have
	// both a revoked row and an active one for the same worker_id — mirror
	// SQLite's "UPDATE ... WHERE worker_id = ? AND revoked_at IS NULL",
	// which matches by predicate regardless of row count, rather than
	// stopping at the first row this map happens to yield. Returning
	// ErrNotFound on hitting a revoked row before the active one would make
	// a legitimate revoke fail non-deterministically.
	for id, c := range s.workerCredentials {
		if c.WorkerID != workerID || c.RevokedAt != nil {
			continue
		}
		c.RevokedAt = &at
		s.workerCredentials[id] = c
		return nil
	}
	return store.ErrNotFound
}

// TouchWorkerCredential implements [store.WorkerCredentialStore].
func (s *Store) TouchWorkerCredential(ctx context.Context, workerID string, at time.Time) (store.WorkerCredential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fenceLocked(ctx); err != nil {
		return store.WorkerCredential{}, err
	}
	// Scan the whole map rather than stopping at the first match, for the
	// same reason RevokeWorkerCredential does: a worker can have both a
	// revoked row and an active one after a key rotation, and map iteration
	// order is randomized.
	for id, c := range s.workerCredentials {
		if c.WorkerID != workerID || c.RevokedAt != nil {
			continue
		}
		c.LastSeenAt = &at
		s.workerCredentials[id] = c
		return c, nil
	}
	return store.WorkerCredential{}, store.ErrNotFound
}

// CreateWorkerJoinToken implements [store.WorkerCredentialStore].
func (s *Store) CreateWorkerJoinToken(ctx context.Context, t store.WorkerJoinToken) (store.WorkerJoinToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fenceLocked(ctx); err != nil {
		return store.WorkerJoinToken{}, err
	}
	if _, ok := s.workerJoinTokens[t.ID]; ok {
		return store.WorkerJoinToken{}, store.ErrConflict
	}
	for _, ex := range s.workerJoinTokens {
		if ex.TokenHash == t.TokenHash {
			return store.WorkerJoinToken{}, store.ErrConflict
		}
	}
	// The SQL stores refuse a token for a group that does not exist through the
	// foreign key.
	if t.CapacityGroupID != "" {
		if _, ok := s.capacityGroups[t.CapacityGroupID]; !ok {
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
func (s *Store) MarkWorkerJoinTokenUsed(ctx context.Context, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fenceLocked(ctx); err != nil {
		return err
	}
	t, ok := s.workerJoinTokens[id]
	if !ok {
		return store.ErrNotFound
	}
	t.UsedAt = &at
	s.workerJoinTokens[id] = t
	return nil
}

// ListCapacityGroupTokens implements [store.WorkerCredentialStore].
func (s *Store) ListCapacityGroupTokens(_ context.Context, groupID string) ([]store.WorkerJoinToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.WorkerJoinToken
	for _, t := range s.workerJoinTokens {
		if t.CapacityGroupID == groupID {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// RevokeWorkerJoinToken implements [store.WorkerCredentialStore].
func (s *Store) RevokeWorkerJoinToken(ctx context.Context, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fenceLocked(ctx); err != nil {
		return err
	}
	t, ok := s.workerJoinTokens[id]
	if !ok {
		return store.ErrNotFound
	}
	if t.RevokedAt == nil {
		t.RevokedAt = &at
		s.workerJoinTokens[id] = t
	}
	return nil
}

// RedeemWorkerJoinToken implements [store.WorkerCredentialStore].
//
// Mirrors SQLite's transaction: the token claim and the credential creation
// happen under ONE hold of the mutex, so a caller never observes the token
// consumed without the credential existing, or the reverse. An unknown,
// expired or already-claimed token is store.ErrNotFound with cred left
// uncreated; a cred that collides with an existing worker_id or public_key
// is store.ErrConflict with the token left unclaimed — mirroring SQLite's
// rollback, since nothing here is written until both checks pass.
func (s *Store) RedeemWorkerJoinToken(ctx context.Context, hash string, now time.Time, cred store.WorkerCredential) (store.WorkerCredential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fenceLocked(ctx); err != nil {
		return store.WorkerCredential{}, err
	}

	var tokID string
	found := false
	for id, t := range s.workerJoinTokens {
		if t.TokenHash != hash || t.UsedAt != nil || t.RevokedAt != nil {
			continue
		}
		// Strictly after, matching SQL's "expires_at > ?": a token whose
		// expiry is exactly now is expired.
		if !t.ExpiresAt.After(now) {
			continue
		}
		tokID = id
		found = true
		break
	}
	if !found {
		return store.WorkerCredential{}, store.ErrNotFound
	}

	// Same conflict rules as CreateWorkerCredential, inlined rather than
	// called: that method takes s.mu itself, and this method already holds
	// it for the whole claim-and-create span.
	if _, ok := s.workerCredentials[cred.ID]; ok {
		return store.WorkerCredential{}, store.ErrConflict
	}
	for _, ex := range s.workerCredentials {
		if ex.PublicKey == cred.PublicKey {
			return store.WorkerCredential{}, store.ErrConflict
		}
		if ex.WorkerID == cred.WorkerID && ex.RevokedAt == nil {
			return store.WorkerCredential{}, store.ErrConflict
		}
	}

	tok := s.workerJoinTokens[tokID]
	at := now
	tok.UsedAt = &at
	s.workerJoinTokens[tokID] = tok

	s.workerCredentials[cred.ID] = cred
	return cred, nil
}
