// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

func TestWorkerCredential_CreateAndGet(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	enrolledAt := time.Now().UTC().Truncate(time.Second)

	c := store.WorkerCredential{
		ID:         "wc1",
		WorkerID:   "w1",
		PublicKey:  "pub1",
		Name:       "worker-one",
		EnrolledAt: enrolledAt,
	}
	created, err := s.CreateWorkerCredential(ctx, c)
	if err != nil {
		t.Fatalf("CreateWorkerCredential: %v", err)
	}
	if created.ID != c.ID {
		t.Errorf("ID: got %q, want %q", created.ID, c.ID)
	}
	if !created.EnrolledAt.Equal(enrolledAt) {
		t.Errorf("EnrolledAt: got %v, want %v", created.EnrolledAt, enrolledAt)
	}
	if created.LastSeenAt != nil {
		t.Errorf("LastSeenAt: got %v, want nil", created.LastSeenAt)
	}
	if created.RevokedAt != nil {
		t.Errorf("RevokedAt: got %v, want nil", created.RevokedAt)
	}

	got, err := s.GetActiveWorkerCredentialByWorkerID(ctx, "w1")
	if err != nil {
		t.Fatalf("GetActiveWorkerCredentialByWorkerID: %v", err)
	}
	if got.ID != c.ID || got.PublicKey != c.PublicKey || got.Name != c.Name {
		t.Errorf("got %+v, want fields matching %+v", got, c)
	}
}

func TestWorkerCredential_GetNotFound(t *testing.T) {
	s := openTestStore(t)
	_, err := s.GetActiveWorkerCredentialByWorkerID(context.Background(), "nope")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestWorkerCredential_DuplicateWorkerID(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	if _, err := s.CreateWorkerCredential(ctx, store.WorkerCredential{
		ID: "wc1", WorkerID: "w1", PublicKey: "pub1", EnrolledAt: now,
	}); err != nil {
		t.Fatalf("CreateWorkerCredential: %v", err)
	}
	_, err := s.CreateWorkerCredential(ctx, store.WorkerCredential{
		ID: "wc2", WorkerID: "w1", PublicKey: "pub2", EnrolledAt: now,
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("expected ErrConflict for duplicate worker_id, got %v", err)
	}
}

func TestWorkerCredential_DuplicatePublicKey(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	if _, err := s.CreateWorkerCredential(ctx, store.WorkerCredential{
		ID: "wc1", WorkerID: "w1", PublicKey: "pub1", EnrolledAt: now,
	}); err != nil {
		t.Fatalf("CreateWorkerCredential: %v", err)
	}
	_, err := s.CreateWorkerCredential(ctx, store.WorkerCredential{
		ID: "wc2", WorkerID: "w2", PublicKey: "pub1", EnrolledAt: now,
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("expected ErrConflict for duplicate public_key, got %v", err)
	}
}

// TestWorkerCredential_RevokedPublicKeyStillUnique verifies that public_key
// uniqueness is untouched by the worker_id fix: a key that belonged to a
// now-revoked credential can never be reused, by any worker.
func TestWorkerCredential_RevokedPublicKeyStillUnique(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	if _, err := s.CreateWorkerCredential(ctx, store.WorkerCredential{
		ID: "wc1", WorkerID: "w1", PublicKey: "pub1", EnrolledAt: now,
	}); err != nil {
		t.Fatalf("CreateWorkerCredential: %v", err)
	}
	if err := s.RevokeWorkerCredential(ctx, "w1", now); err != nil {
		t.Fatalf("RevokeWorkerCredential: %v", err)
	}

	_, err := s.CreateWorkerCredential(ctx, store.WorkerCredential{
		ID: "wc2", WorkerID: "w2", PublicKey: "pub1", EnrolledAt: now,
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("expected ErrConflict reusing a revoked credential's public_key, got %v", err)
	}
}

func TestWorkerCredential_ListActiveOmitsRevoked(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	if _, err := s.CreateWorkerCredential(ctx, store.WorkerCredential{
		ID: "wc1", WorkerID: "w1", PublicKey: "pub1", EnrolledAt: now,
	}); err != nil {
		t.Fatalf("CreateWorkerCredential w1: %v", err)
	}
	if _, err := s.CreateWorkerCredential(ctx, store.WorkerCredential{
		ID: "wc2", WorkerID: "w2", PublicKey: "pub2", EnrolledAt: now,
	}); err != nil {
		t.Fatalf("CreateWorkerCredential w2: %v", err)
	}
	if err := s.RevokeWorkerCredential(ctx, "w2", now); err != nil {
		t.Fatalf("RevokeWorkerCredential: %v", err)
	}

	active, err := s.ListActiveWorkerCredentials(ctx)
	if err != nil {
		t.Fatalf("ListActiveWorkerCredentials: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("want 1 active credential, got %d", len(active))
	}
	if active[0].WorkerID != "w1" {
		t.Errorf("active[0].WorkerID: got %q, want %q", active[0].WorkerID, "w1")
	}
}

// TestWorkerCredential_ListActiveAfterRotationHasExactlyOneRow verifies that
// after a worker is enrolled, revoked, and re-enrolled with a new key,
// exactly one row is active — never zero (the rotation silently failing) and
// never two (both keys accepted by the broker at once).
func TestWorkerCredential_ListActiveAfterRotationHasExactlyOneRow(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	if _, err := s.CreateWorkerCredential(ctx, store.WorkerCredential{
		ID: "wc1", WorkerID: "w1", PublicKey: "pub1", EnrolledAt: now,
	}); err != nil {
		t.Fatalf("CreateWorkerCredential (first): %v", err)
	}
	if err := s.RevokeWorkerCredential(ctx, "w1", now); err != nil {
		t.Fatalf("RevokeWorkerCredential: %v", err)
	}
	if _, err := s.CreateWorkerCredential(ctx, store.WorkerCredential{
		ID: "wc2", WorkerID: "w1", PublicKey: "pub2", EnrolledAt: now,
	}); err != nil {
		t.Fatalf("CreateWorkerCredential (rotated): %v", err)
	}

	active, err := s.ListActiveWorkerCredentials(ctx)
	if err != nil {
		t.Fatalf("ListActiveWorkerCredentials: %v", err)
	}
	var forW1 []store.WorkerCredential
	for _, c := range active {
		if c.WorkerID == "w1" {
			forW1 = append(forW1, c)
		}
	}
	if len(forW1) != 1 {
		t.Fatalf("want exactly 1 active credential for w1 after rotation, got %d", len(forW1))
	}
	if forW1[0].PublicKey != "pub2" {
		t.Errorf("active credential PublicKey = %q, want %q (the rotated key)", forW1[0].PublicKey, "pub2")
	}
}

// TestWorkerCredential_RotateAfterRevoke verifies that revocation is not a
// one-way door: once a worker's credential is revoked, that same worker ID
// can enroll again with a new public key, and both rows persist — the
// revoked one keeping its revoked_at, the new one active.
func TestWorkerCredential_RotateAfterRevoke(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	if _, err := s.CreateWorkerCredential(ctx, store.WorkerCredential{
		ID: "wc1", WorkerID: "w1", PublicKey: "pub1", EnrolledAt: now,
	}); err != nil {
		t.Fatalf("CreateWorkerCredential (first): %v", err)
	}
	if err := s.RevokeWorkerCredential(ctx, "w1", now); err != nil {
		t.Fatalf("RevokeWorkerCredential: %v", err)
	}

	created, err := s.CreateWorkerCredential(ctx, store.WorkerCredential{
		ID: "wc2", WorkerID: "w1", PublicKey: "pub2", EnrolledAt: now,
	})
	if err != nil {
		t.Fatalf("CreateWorkerCredential (rotated): %v", err)
	}
	if created.RevokedAt != nil {
		t.Errorf("rotated credential RevokedAt = %v, want nil", created.RevokedAt)
	}

	// GetActiveWorkerCredentialByWorkerID's whole contract is to resolve this
	// ambiguity: after a rotation there are two rows for w1, and it must
	// return the active one (pub2), never the revoked one (pub1).
	got, err := s.GetActiveWorkerCredentialByWorkerID(ctx, "w1")
	if err != nil {
		t.Fatalf("GetActiveWorkerCredentialByWorkerID: %v", err)
	}
	if got.WorkerID != "w1" {
		t.Errorf("GetActiveWorkerCredentialByWorkerID WorkerID = %q, want %q", got.WorkerID, "w1")
	}
	if got.PublicKey != "pub2" {
		t.Errorf("GetActiveWorkerCredentialByWorkerID PublicKey = %q, want %q (the active, rotated key)", got.PublicKey, "pub2")
	}
	if got.RevokedAt != nil {
		t.Errorf("GetActiveWorkerCredentialByWorkerID RevokedAt = %v, want nil", got.RevokedAt)
	}
}

// TestWorkerCredential_GetActiveOnly_RevokedOnlyReturnsNotFound verifies that
// a worker whose only credential has been revoked (no rotation yet) is
// reported as having no active credential — GetActiveWorkerCredentialByWorkerID
// must never hand back a revoked row.
func TestWorkerCredential_GetActiveOnly_RevokedOnlyReturnsNotFound(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	if _, err := s.CreateWorkerCredential(ctx, store.WorkerCredential{
		ID: "wc1", WorkerID: "w1", PublicKey: "pub1", EnrolledAt: now,
	}); err != nil {
		t.Fatalf("CreateWorkerCredential: %v", err)
	}
	if err := s.RevokeWorkerCredential(ctx, "w1", now); err != nil {
		t.Fatalf("RevokeWorkerCredential: %v", err)
	}

	_, err := s.GetActiveWorkerCredentialByWorkerID(ctx, "w1")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound for a worker with only a revoked credential, got %v", err)
	}
}

func TestWorkerCredential_RevokeNotFound(t *testing.T) {
	s := openTestStore(t)
	err := s.RevokeWorkerCredential(context.Background(), "nope", time.Now().UTC())
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestWorkerJoinToken_CreateAndGetByHash(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	expiresAt := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	createdAt := time.Now().UTC().Truncate(time.Second)

	tok := store.WorkerJoinToken{
		ID:        "jt1",
		TokenHash: "hash1",
		Prefix:    "sqiwjt_abcd",
		Name:      "farm-join",
		ExpiresAt: expiresAt,
		CreatedBy: "u1",
		CreatedAt: createdAt,
	}
	created, err := s.CreateWorkerJoinToken(ctx, tok)
	if err != nil {
		t.Fatalf("CreateWorkerJoinToken: %v", err)
	}
	if created.UsedAt != nil {
		t.Errorf("UsedAt: got %v, want nil", created.UsedAt)
	}

	got, err := s.GetWorkerJoinTokenByHash(ctx, "hash1")
	if err != nil {
		t.Fatalf("GetWorkerJoinTokenByHash: %v", err)
	}
	if got.ID != tok.ID || got.Prefix != tok.Prefix || got.Name != tok.Name || got.CreatedBy != tok.CreatedBy {
		t.Errorf("got %+v, want fields matching %+v", got, tok)
	}
	if !got.ExpiresAt.Equal(expiresAt) {
		t.Errorf("ExpiresAt: got %v, want %v", got.ExpiresAt, expiresAt)
	}
}

func TestWorkerJoinToken_MarkUsed(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	if _, err := s.CreateWorkerJoinToken(ctx, store.WorkerJoinToken{
		ID:        "jt1",
		TokenHash: "hash1",
		Prefix:    "sqiwjt_abcd",
		ExpiresAt: now.Add(time.Hour),
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("CreateWorkerJoinToken: %v", err)
	}

	usedAt := now.Add(time.Minute)
	if err := s.MarkWorkerJoinTokenUsed(ctx, "jt1", usedAt); err != nil {
		t.Fatalf("MarkWorkerJoinTokenUsed: %v", err)
	}

	got, err := s.GetWorkerJoinTokenByHash(ctx, "hash1")
	if err != nil {
		t.Fatalf("GetWorkerJoinTokenByHash: %v", err)
	}
	if got.UsedAt == nil {
		t.Fatal("UsedAt: got nil, want set")
	}
	if !got.UsedAt.Equal(usedAt) {
		t.Errorf("UsedAt: got %v, want %v", *got.UsedAt, usedAt)
	}
}

func TestWorkerJoinToken_MarkUsedNotFound(t *testing.T) {
	s := openTestStore(t)
	err := s.MarkWorkerJoinTokenUsed(context.Background(), "nope", time.Now().UTC())
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestWorkerJoinToken_GetByHashNotFound(t *testing.T) {
	s := openTestStore(t)
	_, err := s.GetWorkerJoinTokenByHash(context.Background(), "nope")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}
