// SPDX-License-Identifier: AGPL-3.0-or-later

package restore

import (
	"context"
	"errors"
	"fmt"

	"github.com/uberware/sqi/internal/store/migrations"
	"github.com/uberware/sqi/internal/store/postgres"
)

// placePostgres checks pg_restore against the server, checks the archive
// (pg_restore reads it without connecting), requires an empty target, and
// loads it. The single-server lock Run holds is an advisory lock on its own
// connection; pg_restore runs in a separate session and is not blocked by it.
//
// Only an archive pg_restore cannot read is ErrNotSQIBackup: a missing or too
// old pg_restore, or a canceled restore, says nothing about the backup.
func placePostgres(ctx context.Context, o Options) (int64, error) {
	if err := postgres.CheckRestoreTool(ctx, o.PostgresDSN); err != nil {
		return 0, fmt.Errorf("restore: %w", err)
	}
	v, err := postgres.ArchiveSchemaVersion(ctx, o.Backup)
	if errors.Is(err, postgres.ErrUnreadableArchive) {
		return 0, fmt.Errorf("%w: %w", ErrNotSQIBackup, err)
	}
	if err != nil {
		return 0, fmt.Errorf("restore: %w", err)
	}
	if v == 0 {
		return 0, fmt.Errorf("%w: the archive has no sqi schema", ErrNotSQIBackup)
	}
	if err := migrations.CheckVersion("postgres", v); err != nil {
		return v, err
	}
	if err := postgres.RequireEmptyDatabase(ctx, o.PostgresDSN); err != nil {
		return v, fmt.Errorf("restore: %w", err)
	}
	return v, postgres.Restore(ctx, o.PostgresDSN, o.Backup, o.Logger)
}

// requireInPlacePostgres refuses --in-place on a database with no sqi schema:
// opening it with AutoMigrate would create one and report a restore that
// restored nothing.
func requireInPlacePostgres(ctx context.Context, o Options) error {
	if err := postgres.RequireMigratedDatabase(ctx, o.PostgresDSN); err != nil {
		return fmt.Errorf("restore: --in-place needs an existing sqi database: %w", err)
	}
	return nil
}
