// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"log/slog"
	"time"
)

// Checkpoint runs a WAL checkpoint in PASSIVE mode, copying whatever committed
// WAL frames it can back into the main database file without ever waiting. It
// returns the number of WAL log pages and the number successfully
// checkpointed, and is the mode the periodic checkpointer uses.
//
// IT MUST NOT BE TRUNCATE, and that is not a style preference. Reads run on
// their own connection pool (see the Concurrency section of the package doc),
// so a reader holding a WAL snapshot now blocks a truncating checkpoint —
// which invokes SQLite's busy handler and waits out the ENTIRE busy_timeout
// (5s) on the write connection before giving up, and still fails to truncate.
// Measured under four concurrent readers: busy=1, 119,888 of 134,522 frames,
// 5.019s. That would stall job submission, lease grants and every task-status
// write for five seconds on a five-minute timer — the same write stall the
// separate read pool exists to remove, reintroduced through the back door.
//
// PASSIVE never invokes the busy handler: it transfers what it can and returns.
// A reader simply means fewer frames move this tick and the next one takes
// them. Pinned by TestCheckpoint_DoesNotBlockOnAReader.
//
// The checkpoint runs on the write pool, which is still a single connection,
// so two checkpoints can never overlap.
func (s *Store) Checkpoint(ctx context.Context) (walPages, checkpointed int, err error) {
	return s.checkpoint(ctx, "PASSIVE")
}

// CheckpointTruncate runs a WAL checkpoint in TRUNCATE mode, which additionally
// truncates the WAL to zero bytes once every frame is transferred.
//
// It is for SHUTDOWN, where a WAL left at zero is worth waiting for and there
// is no next tick to finish the job. It can block for up to busy_timeout if a
// reader still holds a snapshot, which is acceptable once — and only once — on
// the way down. Do not call it on a timer; see [Store.Checkpoint].
func (s *Store) CheckpointTruncate(ctx context.Context) (walPages, checkpointed int, err error) {
	return s.checkpoint(ctx, "TRUNCATE")
}

// checkpoint issues PRAGMA wal_checkpoint in the given mode. The busy flag is
// deliberately not surfaced as an error — a checkpoint that could not move
// every frame is not a failure.
func (s *Store) checkpoint(ctx context.Context, mode string) (walPages, checkpointed int, err error) {
	// mode is a package-internal literal, never caller-supplied.
	row := s.db.QueryRowContext(ctx, "PRAGMA wal_checkpoint("+mode+")")
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

		checkpoint := func(ctx context.Context, truncate bool) {
			run := s.Checkpoint
			if truncate {
				run = s.CheckpointTruncate
			}
			wal, done, err := run(ctx)
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
				// Shutdown: truncate, so the WAL is left at zero. There is no
				// next tick to finish the job, and waiting once here is fine.
				checkpoint(context.Background(), true)
				return
			case <-ticker.C:
				// Periodic: PASSIVE only. See [Store.Checkpoint].
				checkpoint(ctx, false)
			}
		}
	}()
}
