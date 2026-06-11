// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/uberware/sqi/internal/store"
)

const (
	defaultFarmName  = "default"
	defaultQueueName = "default"
	// defaultQueuePriority is the mid-range default used elsewhere in the UI.
	defaultQueuePriority = 50
)

// seedDefaults creates a starter farm and queue the first time the server runs
// against an empty store. It is a no-op when any farm already exists or when
// disabled via config.
func seedDefaults(ctx context.Context, st store.Store, cfg Config, logger *slog.Logger) error {
	if !cfg.SeedDefaults {
		return nil
	}

	farms, err := st.ListFarms(ctx)
	if err != nil {
		return fmt.Errorf("seed: list farms: %w", err)
	}

	now := time.Now().UTC()

	// Find the default farm if it was already seeded; create it if not.
	var farmID string
	var foundDefaultFarm bool
	for _, f := range farms {
		if f.Name == defaultFarmName {
			farmID = f.ID
			foundDefaultFarm = true
			break
		}
	}
	if !foundDefaultFarm {
		if len(farms) > 0 {
			// User has their own farms; don't seed.
			return nil
		}
		farm, err := st.CreateFarm(ctx, store.Farm{
			ID:          uuid.NewString(),
			Name:        defaultFarmName,
			Description: "Default farm created on first startup",
			CreatedAt:   now,
			UpdatedAt:   now,
		})
		if err != nil {
			return fmt.Errorf("seed: create default farm: %w", err)
		}
		farmID = farm.ID
		logger.InfoContext(
			ctx, "seeded default farm",
			slog.String("farm_id", farmID),
			slog.String("farm_name", farm.Name),
		)
	}

	// Check whether the default queue already exists (guards against partial-seed
	// if the process was killed between CreateFarm and CreateQueue).
	queues, err := st.ListQueues(ctx, store.ListQueuesOptions{
		FarmID:     farmID,
		Pagination: store.Pagination{Limit: 1},
	})
	if err != nil {
		return fmt.Errorf("seed: list queues: %w", err)
	}
	if len(queues.Items) > 0 {
		return nil
	}

	queue, err := st.CreateQueue(ctx, store.Queue{
		ID:          uuid.NewString(),
		FarmID:      farmID,
		Name:        defaultQueueName,
		Description: "Default queue created on first startup",
		Priority:    defaultQueuePriority,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	if err != nil {
		return fmt.Errorf("seed: create default queue: %w", err)
	}

	logger.InfoContext(
		ctx, "seeded default queue",
		slog.String("farm_id", farmID),
		slog.String("queue_id", queue.ID),
		slog.String("queue_name", queue.Name),
	)
	return nil
}
