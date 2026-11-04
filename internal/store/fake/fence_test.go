// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"errors"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// The completeness of the fence across every mutator, on this store and the
// SQL ones, is internal/store's TestParity_Fence_EveryMutator*; these pin the
// fake's own rule.

func TestFenceLocked_Epochs(t *testing.T) {
	s := New()
	bg := t.Context()
	if _, ok, err := s.AcquireLeadership(bg, "a", time.Hour); err != nil || !ok {
		t.Fatalf("AcquireLeadership: ok=%v err=%v", ok, err)
	}
	tests := []struct {
		name string
		ctx  func() (epoch int64, carries bool)
		want error
	}{
		{"no epoch", func() (int64, bool) { return 0, false }, nil},
		{"the lease's epoch", func() (int64, bool) { return 1, true }, nil},
		{"a stale epoch", func() (int64, bool) { return 0, true }, store.ErrFenced},
		{"a future epoch", func() (int64, bool) { return 2, true }, store.ErrFenced},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := bg
			if epoch, ok := tt.ctx(); ok {
				ctx = store.WithLeaderEpoch(bg, epoch)
			}
			_, err := s.CreateFarm(ctx, store.Farm{ID: tt.name, Name: tt.name})
			if !errors.Is(err, tt.want) {
				t.Fatalf("CreateFarm err = %v, want %v", err, tt.want)
			}
			_, getErr := s.GetFarm(bg, tt.name)
			if tt.want != nil && !errors.Is(getErr, store.ErrNotFound) {
				t.Fatalf("a fenced write left a farm behind: GetFarm err = %v", getErr)
			}
			if tt.want == nil && getErr != nil {
				t.Fatalf("an allowed write is missing: GetFarm err = %v", getErr)
			}
		})
	}
}

// TestFenceLocked_FollowsTheLease: the epoch a write is checked against is the
// lease's current one, so a takeover fences the previous leader at once, and a
// released lease keeps its epoch (a deposed server stays fenced).
func TestFenceLocked_FollowsTheLease(t *testing.T) {
	s := New()
	s.SetClock(func() int64 { return 1_000_000 })
	bg := t.Context()
	farm := func(epoch int64, id string) error {
		_, err := s.CreateFarm(store.WithLeaderEpoch(bg, epoch), store.Farm{ID: id, Name: id})
		return err
	}
	// No lease has ever been taken: no epoch is current.
	if err := farm(1, "never"); !errors.Is(err, store.ErrFenced) {
		t.Fatalf("before any lease: err = %v, want ErrFenced", err)
	}
	if _, ok, err := s.AcquireLeadership(bg, "a", time.Second); err != nil || !ok {
		t.Fatalf("a did not acquire: ok=%v err=%v", ok, err)
	}
	if err := farm(1, "a1"); err != nil {
		t.Fatalf("a at epoch 1: %v", err)
	}
	if err := s.ReleaseLeadership(bg, "a", 1); err != nil {
		t.Fatal(err)
	}
	if err := farm(1, "a2"); err != nil {
		t.Fatalf("a after releasing, before anyone else leads: %v", err)
	}
	if _, ok, err := s.AcquireLeadership(bg, "b", time.Second); err != nil || !ok {
		t.Fatalf("b did not acquire: ok=%v err=%v", ok, err)
	}
	if err := farm(1, "a3"); !errors.Is(err, store.ErrFenced) {
		t.Fatalf("a after b took over: err = %v, want ErrFenced", err)
	}
	if err := farm(2, "b1"); err != nil {
		t.Fatalf("b at epoch 2: %v", err)
	}
}
