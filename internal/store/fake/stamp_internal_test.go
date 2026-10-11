// SPDX-License-Identifier: AGPL-3.0-or-later

package fake

import (
	"testing"
	"time"

	"github.com/uberware/sqi/internal/store"
)

// TestUpdatesStampFromTheCreatesClock pins that an Update stamps UpdatedAt
// from the same clock as a Create. Creates stamp through stampLocked, which
// runs ahead of the wall clock whenever several rows share a coarse clock
// tick, so an Update that read time.Now() directly could return an UpdatedAt
// before the row's CreatedAt, which SQLite cannot do. The test pushes the
// store clock an hour ahead to make that case certain rather than a race
// against the host clock.
func TestUpdatesStampFromTheCreatesClock(t *testing.T) {
	s := New()
	ctx := t.Context()
	s.mu.Lock()
	s.lastStamp = time.Now().UTC().Add(time.Hour)
	s.mu.Unlock()

	farm := mustCreateFarm(t, s, "f1")
	farm.Description = "updated"
	farmUpd, err := s.UpdateFarm(ctx, farm)
	if err != nil {
		t.Fatalf("UpdateFarm: %v", err)
	}

	queue := mustCreateQueue(t, s, farm.ID, "q1", "q1")
	queue.Description = "updated"
	queueUpd, err := s.UpdateQueue(ctx, queue)
	if err != nil {
		t.Fatalf("UpdateQueue: %v", err)
	}

	cl, err := s.CreateComputeLocation(ctx, store.ComputeLocation{ID: "c1", Name: "c1"})
	if err != nil {
		t.Fatalf("CreateComputeLocation: %v", err)
	}
	cl.Description = "updated"
	clUpd, err := s.UpdateComputeLocation(ctx, cl)
	if err != nil {
		t.Fatalf("UpdateComputeLocation: %v", err)
	}

	sl, err := s.CreateStorageLocation(ctx, store.StorageLocation{ID: "s1", Name: "s1", Type: store.StorageLocationTypeFilesystem})
	if err != nil {
		t.Fatalf("CreateStorageLocation: %v", err)
	}
	sl.Description = "updated"
	slUpd, err := s.UpdateStorageLocation(ctx, sl)
	if err != nil {
		t.Fatalf("UpdateStorageLocation: %v", err)
	}

	pool := mustCreatePool(t, s, "p1", 1)
	pool.MaxConcurrent = 2
	poolUpd, err := s.UpdateUsagePool(ctx, pool)
	if err != nil {
		t.Fatalf("UpdateUsagePool: %v", err)
	}

	for what, at := range map[string][2]time.Time{
		"farm":             {farm.CreatedAt, farmUpd.UpdatedAt},
		"queue":            {queue.CreatedAt, queueUpd.UpdatedAt},
		"compute location": {cl.CreatedAt, clUpd.UpdatedAt},
		"storage location": {sl.CreatedAt, slUpd.UpdatedAt},
		"usage pool":       {pool.CreatedAt, poolUpd.UpdatedAt},
	} {
		if at[1].Before(at[0]) {
			t.Errorf("%s: UpdatedAt %v is before CreatedAt %v", what, at[1], at[0])
		}
	}
}
