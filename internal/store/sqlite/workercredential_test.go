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

	got, err := s.GetWorkerCredentialByWorkerID(ctx, "w1")
	if err != nil {
		t.Fatalf("GetWorkerCredentialByWorkerID: %v", err)
	}
	if got.ID != c.ID || got.PublicKey != c.PublicKey || got.Name != c.Name {
		t.Errorf("got %+v, want fields matching %+v", got, c)
	}
}

func TestWorkerCredential_GetNotFound(t *testing.T) {
	s := openTestStore(t)
	_, err := s.GetWorkerCredentialByWorkerID(context.Background(), "nope")
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
