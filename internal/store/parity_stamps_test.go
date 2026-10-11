// SPDX-License-Identifier: AGPL-3.0-or-later

package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
)

// TestParity_CreateStampsTimestamps checks that each Create stamps CreatedAt
// and UpdatedAt with the store's own clock, ignoring what the caller passed,
// as SQLite does.
func TestParity_CreateStampsTimestamps(t *testing.T) {
	// A time well before any real stamp: a Create that kept the caller's value
	// would hand it straight back.
	sentinel := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	for name, st := range newStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			farm, err := st.CreateFarm(ctx, store.Farm{ID: uuid.NewString(), Name: uuid.NewString(), CreatedAt: sentinel, UpdatedAt: sentinel})
			if err != nil {
				t.Fatalf("CreateFarm: %v", err)
			}
			queue, err := st.CreateQueue(ctx, store.Queue{ID: uuid.NewString(), FarmID: farm.ID, Name: uuid.NewString(), CreatedAt: sentinel, UpdatedAt: sentinel})
			if err != nil {
				t.Fatalf("CreateQueue: %v", err)
			}
			cl, err := st.CreateComputeLocation(ctx, store.ComputeLocation{ID: uuid.NewString(), Name: uuid.NewString(), CreatedAt: sentinel, UpdatedAt: sentinel})
			if err != nil {
				t.Fatalf("CreateComputeLocation: %v", err)
			}
			sl, err := st.CreateStorageLocation(ctx, store.StorageLocation{
				ID: uuid.NewString(), Name: uuid.NewString(), Type: store.StorageLocationTypeFilesystem, CreatedAt: sentinel, UpdatedAt: sentinel,
			})
			if err != nil {
				t.Fatalf("CreateStorageLocation: %v", err)
			}
			pool, err := st.CreateUsagePool(ctx, store.UsagePool{ID: uuid.NewString(), Name: uuid.NewString(), MaxConcurrent: 1, CreatedAt: sentinel, UpdatedAt: sentinel})
			if err != nil {
				t.Fatalf("CreateUsagePool: %v", err)
			}
			prod := mkProduct(uuid.NewString())
			prod.CreatedAt, prod.UpdatedAt = sentinel, sentinel
			product, err := st.CreateProduct(ctx, prod)
			if err != nil {
				t.Fatalf("CreateProduct: %v", err)
			}
			for what, at := range map[string][2]time.Time{
				"farm":             {farm.CreatedAt, farm.UpdatedAt},
				"queue":            {queue.CreatedAt, queue.UpdatedAt},
				"compute location": {cl.CreatedAt, cl.UpdatedAt},
				"storage location": {sl.CreatedAt, sl.UpdatedAt},
				"usage pool":       {pool.CreatedAt, pool.UpdatedAt},
				"product":          {product.CreatedAt, product.UpdatedAt},
			} {
				if !at[0].After(sentinel) || !at[1].After(sentinel) {
					t.Errorf("%s: CreatedAt = %v, UpdatedAt = %v; want both stamped by the store, not the caller's %v", what, at[0], at[1], sentinel)
				}
				if !at[0].Equal(at[1]) {
					t.Errorf("%s: CreatedAt = %v, UpdatedAt = %v; want equal on creation", what, at[0], at[1])
				}
			}
		})
	}
}
