// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
)

// SchemaVersion returns the highest applied goose version, or 0 when the
// database has never been migrated. It creates nothing.
func SchemaVersion(ctx context.Context, db *sql.DB) (int64, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'goose_db_version'`).Scan(&n); err != nil {
		return 0, fmt.Errorf("sqlite: read schema version: %w", err)
	}
	if n == 0 {
		return 0, nil
	}
	var v sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT MAX(version_id) FROM goose_db_version WHERE is_applied`).Scan(&v); err != nil {
		return 0, fmt.Errorf("sqlite: read schema version: %w", err)
	}
	return v.Int64, nil
}
