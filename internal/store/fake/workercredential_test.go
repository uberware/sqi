// SPDX-License-Identifier: AGPL-3.0-or-later

package fake_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

// TestWorkerCredential_RevokeActiveAfterRotation exercises
// RevokeWorkerCredential against a worker that has one revoked row and one
// active row for the same worker_id — the state a key rotation (revoke, then
// re-enroll with a new key) leaves behind, and a state that only became
// legal once worker_id uniqueness was scoped to active rows.
//
// Go's map iteration order is randomized. A version of RevokeWorkerCredential
// that stops at the FIRST row matching the worker ID, rather than scanning
// for one that is also active, fails non-deterministically here: whenever
// iteration reaches the already-revoked row before the active one, it
// returns ErrNotFound for what is otherwise a completely legitimate revoke.
//
// The test seeds a pile of unrelated "noise" workers so the map has enough
// entries for order to matter, and repeats the whole scenario across many
// subtests (fresh store, fresh map, fresh iteration order each time) so a
// fix that only happens to work by accident of one run's order cannot pass
// by luck.
func TestWorkerCredential_RevokeActiveAfterRotation(t *testing.T) {
	for run := range 50 {
		t.Run(fmt.Sprintf("run-%d", run), func(t *testing.T) {
			s := fake.New()
			defer s.Close()
			ctx := context.Background()
			now := time.Now().UTC()

			// Noise: several other fully-revoked workers, planted before and
			// after the worker under test, so a worker_id match with no
			// revoked_at filter has other rows it could land on first.
			for i := range 20 {
				wid := fmt.Sprintf("noise-%d", i)
				if _, err := s.CreateWorkerCredential(ctx, store.WorkerCredential{
					ID: "noise-wc-" + wid, WorkerID: wid, PublicKey: "noise-pub-" + wid, EnrolledAt: now,
				}); err != nil {
					t.Fatalf("CreateWorkerCredential (noise %d): %v", i, err)
				}
			}

			if _, err := s.CreateWorkerCredential(ctx, store.WorkerCredential{
				ID: "wc1", WorkerID: "w1", PublicKey: "pub1", EnrolledAt: now,
			}); err != nil {
				t.Fatalf("CreateWorkerCredential (first): %v", err)
			}
			if err := s.RevokeWorkerCredential(ctx, "w1", now); err != nil {
				t.Fatalf("RevokeWorkerCredential (first): %v", err)
			}
			if _, err := s.CreateWorkerCredential(ctx, store.WorkerCredential{
				ID: "wc2", WorkerID: "w1", PublicKey: "pub2", EnrolledAt: now,
			}); err != nil {
				t.Fatalf("CreateWorkerCredential (rotated): %v", err)
			}

			// w1 now has one revoked row (wc1) and one active row (wc2).
			// Revoking the active credential must succeed regardless of
			// which row the map iteration reaches first.
			if err := s.RevokeWorkerCredential(ctx, "w1", now.Add(time.Minute)); err != nil {
				t.Fatalf("RevokeWorkerCredential (active credential, post-rotation): %v", err)
			}

			if _, err := s.GetActiveWorkerCredentialByWorkerID(ctx, "w1"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("expected ErrNotFound after revoking the only active credential, got %v", err)
			}
		})
	}
}

// TestWorkerCredential_GetActive_ReturnsActiveRowAfterRotation verifies that
// GetActiveWorkerCredentialByWorkerID resolves the ambiguity a rotation
// creates: it must return the active (rotated) row, never the revoked one.
func TestWorkerCredential_GetActive_ReturnsActiveRowAfterRotation(t *testing.T) {
	s := fake.New()
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC()

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

	got, err := s.GetActiveWorkerCredentialByWorkerID(ctx, "w1")
	if err != nil {
		t.Fatalf("GetActiveWorkerCredentialByWorkerID: %v", err)
	}
	if got.PublicKey != "pub2" {
		t.Errorf("PublicKey = %q, want %q (the active, rotated key)", got.PublicKey, "pub2")
	}
	if got.RevokedAt != nil {
		t.Errorf("RevokedAt = %v, want nil", got.RevokedAt)
	}
}

// TestWorkerCredential_GetActive_RevokedOnlyReturnsNotFound verifies that a
// worker whose only credential has been revoked (no rotation) reports no
// active credential, mirroring the SQLite backend's contract exactly.
func TestWorkerCredential_GetActive_RevokedOnlyReturnsNotFound(t *testing.T) {
	s := fake.New()
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC()

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

// TestWorkerCredential_DuplicateWorkerID mirrors
// sqlite_test.TestWorkerCredential_DuplicateWorkerID: a worker ID with an
// existing ACTIVE credential cannot be double-enrolled. The fake had no
// direct test of CreateWorkerCredential's own conflict logic before this —
// it was only ever exercised indirectly through the rotation tests above.
func TestWorkerCredential_DuplicateWorkerID(t *testing.T) {
	s := fake.New()
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC()

	if _, err := s.CreateWorkerCredential(ctx, store.WorkerCredential{
		ID: "wc1", WorkerID: "w1", PublicKey: "pub1", EnrolledAt: now,
	}); err != nil {
		t.Fatalf("CreateWorkerCredential: %v", err)
	}
	_, err := s.CreateWorkerCredential(ctx, store.WorkerCredential{
		ID: "wc2", WorkerID: "w1", PublicKey: "pub2", EnrolledAt: now,
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("expected ErrConflict for duplicate active worker_id, got %v", err)
	}
}

// TestWorkerCredential_DuplicatePublicKey mirrors
// sqlite_test.TestWorkerCredential_DuplicatePublicKey: two different
// workers can never share a public key, active-vs-active.
func TestWorkerCredential_DuplicatePublicKey(t *testing.T) {
	s := fake.New()
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC()

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

// TestWorkerCredential_RevokedPublicKeyStillUnique mirrors
// sqlite_test.TestWorkerCredential_RevokedPublicKeyStillUnique. The whole
// point of leaving public_key globally UNIQUE (not scoped to active rows the
// way worker_id now is) is that a rotated-away key must never become usable
// again — this pins that in the fake as well as SQLite, so public_key
// uniqueness can never silently regress to active-rows-only scoping without
// a test failing in both backends. If it did regress, a revoked credential's
// key would become available for reuse, defeating the reason rotation is
// safe in the first place.
func TestWorkerCredential_RevokedPublicKeyStillUnique(t *testing.T) {
	s := fake.New()
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC()

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

// TestWorkerCredential_ListActiveAfterRotationHasExactlyOneRow mirrors
// sqlite_test.TestWorkerCredential_ListActiveAfterRotationHasExactlyOneRow:
// after enroll, revoke, re-enroll, ListActiveWorkerCredentials must return
// exactly one row for the rotated worker — never zero (the rotation
// silently failing) and never two. Two simultaneously-active rows for one
// worker_id would mean the broker's authorized-key set (which this method
// feeds) accepts both the old and the new key for that worker at once,
// which is not a rotation at all.
func TestWorkerCredential_ListActiveAfterRotationHasExactlyOneRow(t *testing.T) {
	s := fake.New()
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC()

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
