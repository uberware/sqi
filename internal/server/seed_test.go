// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"log/slog"
	"testing"

	"github.com/uberware/sqi/internal/store"
	"github.com/uberware/sqi/internal/store/fake"
)

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestSeedDefaults(t *testing.T) {
	t.Run("seeds a default farm and queue on an empty store", func(t *testing.T) {
		st := fake.New()
		if err := seedDefaults(context.Background(), st, Config{SeedDefaults: true}, testLogger()); err != nil {
			t.Fatalf("seedDefaults: %v", err)
		}

		farms, err := st.ListFarms(context.Background())
		if err != nil {
			t.Fatalf("ListFarms: %v", err)
		}
		if len(farms) != 1 {
			t.Fatalf("want 1 farm, got %d", len(farms))
		}
		if farms[0].Name != defaultFarmName {
			t.Errorf("farm name = %q, want %q", farms[0].Name, defaultFarmName)
		}

		queues, err := st.ListQueues(context.Background(), store.ListQueuesOptions{
			FarmID:     farms[0].ID,
			Pagination: store.Pagination{Limit: 10},
		})
		if err != nil {
			t.Fatalf("ListQueues: %v", err)
		}
		if len(queues.Items) != 1 {
			t.Fatalf("want 1 queue, got %d", len(queues.Items))
		}
		if queues.Items[0].Name != defaultQueueName {
			t.Errorf("queue name = %q, want %q", queues.Items[0].Name, defaultQueueName)
		}
	})

	t.Run("is a no-op when a non-default farm already exists", func(t *testing.T) {
		st := fake.New()
		if _, err := st.CreateFarm(context.Background(), store.Farm{ID: "f1", Name: "existing"}); err != nil {
			t.Fatalf("CreateFarm: %v", err)
		}

		if err := seedDefaults(context.Background(), st, Config{SeedDefaults: true}, testLogger()); err != nil {
			t.Fatalf("seedDefaults: %v", err)
		}

		farms, err := st.ListFarms(context.Background())
		if err != nil {
			t.Fatalf("ListFarms: %v", err)
		}
		if len(farms) != 1 {
			t.Fatalf("want 1 farm (no seeding), got %d", len(farms))
		}
	})

	t.Run("recovers partial seed when default farm exists but has no queue", func(t *testing.T) {
		st := fake.New()
		// Simulate a crash after CreateFarm but before CreateQueue.
		if _, err := st.CreateFarm(context.Background(), store.Farm{ID: "f-default", Name: defaultFarmName}); err != nil {
			t.Fatalf("CreateFarm: %v", err)
		}

		if err := seedDefaults(context.Background(), st, Config{SeedDefaults: true}, testLogger()); err != nil {
			t.Fatalf("seedDefaults: %v", err)
		}

		queues, err := st.ListQueues(context.Background(), store.ListQueuesOptions{
			FarmID:     "f-default",
			Pagination: store.Pagination{Limit: 10},
		})
		if err != nil {
			t.Fatalf("ListQueues: %v", err)
		}
		if len(queues.Items) != 1 {
			t.Fatalf("want 1 queue (recovery), got %d", len(queues.Items))
		}
		if queues.Items[0].Name != defaultQueueName {
			t.Errorf("queue name = %q, want %q", queues.Items[0].Name, defaultQueueName)
		}
	})

	t.Run("does nothing when disabled", func(t *testing.T) {
		st := fake.New()
		if err := seedDefaults(context.Background(), st, Config{SeedDefaults: false}, testLogger()); err != nil {
			t.Fatalf("seedDefaults: %v", err)
		}
		farms, err := st.ListFarms(context.Background())
		if err != nil {
			t.Fatalf("ListFarms: %v", err)
		}
		if len(farms) != 0 {
			t.Fatalf("want 0 farms when disabled, got %d", len(farms))
		}
	})
}
