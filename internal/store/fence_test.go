// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/uberware/sqi/internal/store"
)

func TestLeaderEpoch_RoundTrips(t *testing.T) {
	for _, epoch := range []int64{0, 1, 42, 1 << 62} {
		got, ok := store.LeaderEpoch(store.WithLeaderEpoch(context.Background(), epoch))
		if !ok || got != epoch {
			t.Errorf("LeaderEpoch(WithLeaderEpoch(%d)) = (%d, %v), want (%d, true)", epoch, got, ok, epoch)
		}
	}
}

func TestLeaderEpoch_AbsentFromAPlainContext(t *testing.T) {
	if got, ok := store.LeaderEpoch(context.Background()); ok || got != 0 {
		t.Fatalf("LeaderEpoch(plain) = (%d, %v), want (0, false)", got, ok)
	}
}

// TestLeaderEpoch_InheritedAndOverridden: a context derived from a leadership
// context carries its epoch, and a later WithLeaderEpoch replaces it.
func TestLeaderEpoch_InheritedAndOverridden(t *testing.T) {
	type other struct{}
	ctx := context.WithValue(store.WithLeaderEpoch(context.Background(), 3), other{}, "x")
	if got, ok := store.LeaderEpoch(ctx); !ok || got != 3 {
		t.Fatalf("derived context: LeaderEpoch = (%d, %v), want (3, true)", got, ok)
	}
	if got, ok := store.LeaderEpoch(store.WithLeaderEpoch(ctx, 4)); !ok || got != 4 {
		t.Fatalf("overridden: LeaderEpoch = (%d, %v), want (4, true)", got, ok)
	}
}

func TestErrFenced_IsDistinctAndWrappable(t *testing.T) {
	wrapped := fmt.Errorf("store: park job: %w", store.ErrFenced)
	if !errors.Is(wrapped, store.ErrFenced) {
		t.Fatal("a wrapped ErrFenced does not match errors.Is")
	}
	for _, other := range []error{store.ErrNotFound, store.ErrConflict, store.ErrLastAdmin} {
		if errors.Is(wrapped, other) || errors.Is(other, store.ErrFenced) {
			t.Errorf("ErrFenced matches %v", other)
		}
	}
}
