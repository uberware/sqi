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

func mkAPIKey(userID, hash string) store.APIKey {
	return store.APIKey{
		ID: uuid.NewString(), UserID: userID, Name: "laptop",
		Prefix: "sqi_abc123", TokenHash: hash, CreatedAt: time.Now().UTC(),
	}
}

func TestAPIKeyStore_CreateGetRevoke(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			u, err := st.CreateUser(ctx, mkUser("keyer"))
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			now := time.Now().UTC()
			k := mkAPIKey(u.ID, "hash-"+uuid.NewString())
			if _, err := st.CreateAPIKey(ctx, k); err != nil {
				t.Fatalf("CreateAPIKey: %v", err)
			}

			got, err := st.GetAPIKeyByTokenHash(ctx, k.TokenHash, now)
			if err != nil || got.UserID != u.ID {
				t.Fatalf("GetAPIKeyByTokenHash: %+v err=%v", got, err)
			}

			// Revoke (scoped to owner) → subsequent lookup is ErrNotFound.
			if err := st.RevokeAPIKey(ctx, k.ID, u.ID, now); err != nil {
				t.Fatalf("RevokeAPIKey: %v", err)
			}
			if _, err := st.GetAPIKeyByTokenHash(ctx, k.TokenHash, now); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("revoked key should be ErrNotFound, got %v", err)
			}
		})
	}
}

func TestAPIKeyStore_ExpiryAndScope(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			ua, err := st.CreateUser(ctx, mkUser("alice"))
			if err != nil {
				t.Fatalf("CreateUser alice: %v", err)
			}
			ub, err := st.CreateUser(ctx, mkUser("bob"))
			if err != nil {
				t.Fatalf("CreateUser bob: %v", err)
			}
			now := time.Now().UTC()

			exp := now.Add(time.Hour)
			ka := mkAPIKey(ua.ID, "hash-a")
			ka.ExpiresAt = &exp
			if _, err := st.CreateAPIKey(ctx, ka); err != nil {
				t.Fatalf("CreateAPIKey alice: %v", err)
			}

			// Expired key is not returned.
			if _, err := st.GetAPIKeyByTokenHash(ctx, ka.TokenHash, now.Add(2*time.Hour)); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("expired key should be ErrNotFound, got %v", err)
			}

			// Exact-boundary: a lookup at precisely ExpiresAt must be
			// treated as expired. Both backends use a strict comparison
			// (sqlite `expires_at > ?`, fake `ExpiresAt.After(now)`), so
			// equality means expired, not still-valid.
			if _, err := st.GetAPIKeyByTokenHash(ctx, ka.TokenHash, exp); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("key at exact ExpiresAt boundary should be ErrNotFound, got %v", err)
			}

			// Bob cannot revoke Alice's key.
			if err := st.RevokeAPIKey(ctx, ka.ID, ub.ID, now); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("cross-user revoke should be ErrNotFound, got %v", err)
			}

			// List is scoped and excludes revoked.
			keys, err := st.ListAPIKeysForUser(ctx, ua.ID)
			if err != nil || len(keys) != 1 {
				t.Fatalf("ListAPIKeysForUser alice: %d keys err=%v", len(keys), err)
			}
			bob, err := st.ListAPIKeysForUser(ctx, ub.ID)
			if err != nil {
				t.Fatalf("ListAPIKeysForUser bob: %v", err)
			}
			if len(bob) != 0 {
				t.Fatalf("bob should have no keys, got %d", len(bob))
			}
		})
	}
}

// TestAPIKeyStore_CreateAPIKey_DuplicateTokenHash verifies that creating a
// second API key with an already-used token_hash is rejected — SQLite via
// `token_hash TEXT NOT NULL UNIQUE`, the fake via an explicit check.
func TestAPIKeyStore_CreateAPIKey_DuplicateTokenHash(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			u, err := st.CreateUser(ctx, mkUser("dupkeyer"))
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			tokenHash := "dup-" + uuid.NewString()

			k1 := mkAPIKey(u.ID, tokenHash)
			if _, err := st.CreateAPIKey(ctx, k1); err != nil {
				t.Fatalf("CreateAPIKey(1): %v", err)
			}

			k2 := mkAPIKey(u.ID, tokenHash)
			if _, err := st.CreateAPIKey(ctx, k2); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("expected ErrConflict on duplicate token_hash, got %v", err)
			}
		})
	}
}

func TestAPIKeyStore_TouchLastUsed(t *testing.T) {
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			u, err := st.CreateUser(ctx, mkUser("toucher"))
			if err != nil {
				t.Fatalf("CreateUser toucher: %v", err)
			}
			now := time.Now().UTC()
			k := mkAPIKey(u.ID, "hash-touch")
			if _, err := st.CreateAPIKey(ctx, k); err != nil {
				t.Fatalf("CreateAPIKey: %v", err)
			}
			if err := st.TouchAPIKeyLastUsed(ctx, k.ID, now); err != nil {
				t.Fatalf("TouchAPIKeyLastUsed: %v", err)
			}
			got, err := st.GetAPIKeyByTokenHash(ctx, k.TokenHash, now)
			if err != nil || got.LastUsedAt == nil || !got.LastUsedAt.Equal(now) {
				t.Fatalf("last_used_at not set: %+v err=%v", got.LastUsedAt, err)
			}
		})
	}
}
