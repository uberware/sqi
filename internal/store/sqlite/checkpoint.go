// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"log/slog"
	"time"
)

// Checkpoint runs a WAL checkpoint in TRUNCATE mode, which copies all
// committed WAL frames back into the main database file and then truncates
// the WAL to zero bytes. It returns the number of WAL log pages and the
// number successfully checkpointed.
//
// TRUNCATE mode is appropriate for the periodic background case: because the
// store uses a single connection (SetMaxOpenConns(1)), there are no concurrent
// readers that could block the checkpoint, so all frames will always be
// transferred in one call.
func (s *Store) Checkpoint(ctx context.Context) (walPages, checkpointed int, err error) {
	row := s.db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	var busy, wal, done int
	if err := row.Scan(&busy, &wal, &done); err != nil {
		return 0, 0, err
	}
	return wal, done, nil
}

// StartCheckpointer launches a background goroutine that calls [Checkpoint]
// every interval. The goroutine exits when ctx is canceled, running one final
// checkpoint before returning so the WAL is fully flushed before the store
// is closed.
//
// Errors are logged but do not stop the loop; transient checkpoint failures
// are not fatal. The caller does not need to wrap this in a goroutine — it
// manages its own.
func (s *Store) StartCheckpointer(ctx context.Context, interval time.Duration, logger *slog.Logger) {
	//nolint:gosec // G118: the goroutine uses ctx for normal ticks; context.Background is intentional for the shutdown checkpoint so it isn't rejected by the already-canceled ctx.
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		checkpoint := func(ctx context.Context) {
			wal, done, err := s.Checkpoint(ctx)
			if err != nil {
				logger.ErrorContext(ctx, "store: wal checkpoint failed", slog.Any("error", err))
				return
			}
			if wal > 0 {
				logger.InfoContext(
					ctx, "store: wal checkpoint",
					slog.Int("wal_pages", wal),
					slog.Int("checkpointed", done),
				)
			}
		}

		for {
			select {
			case <-ctx.Done():
				checkpoint(context.Background())
				return
			case <-ticker.C:
				checkpoint(ctx)
			}
		}
	}()
}
