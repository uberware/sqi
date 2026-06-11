// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"fmt"
)

// Backup creates a consistent, online copy of the database at destPath using
// SQLite's VACUUM INTO statement. The source database remains fully available
// for reads and writes during the operation.
//
// VACUUM INTO is the SQL-level equivalent of the SQLite C online backup API
// (sqlite3_backup_*): it produces a clean, fully checkpointed snapshot of the
// live database in a single atomic operation. The modernc.org/sqlite driver
// does not expose the C backup API directly, so VACUUM INTO is the idiomatic
// replacement.
//
// destPath must not already exist; SQLite will return an error if it does.
func (s *Store) Backup(ctx context.Context, destPath string) error {
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", destPath); err != nil {
		return fmt.Errorf("sqlite backup to %q: %w", destPath, err)
	}
	return nil
}
