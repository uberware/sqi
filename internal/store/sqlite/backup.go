// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/uberware/sqi/internal/fsutil"
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
// destPath must not already exist. The copy holds password hashes, API-key
// hashes and session tokens, so destPath is created first, empty and
// restricted to the writing user ([fsutil.CreateSecretExclusive]), and VACUUM
// INTO, which accepts an empty file, writes into it; left to SQLite, the file
// would get the process's default permissions. On failure the file is removed.
func (s *Store) Backup(ctx context.Context, destPath string) error {
	return vacuumInto(ctx, s.db, destPath)
}

// BackupPath backs up the database at dbPath without opening the store, so it
// works on a schema this binary is too old for — the backup taken before
// rolling a binary back.
func BackupPath(ctx context.Context, dbPath, destPath string) error {
	db, err := sql.Open("sqlite", fmt.Sprintf("%s?_pragma=busy_timeout(%d)", dbPath, busyTimeoutMS))
	if err != nil {
		return fmt.Errorf("sqlite backup: open %s: %w", dbPath, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	return vacuumInto(ctx, db, destPath)
}

// vacuumInto is Backup's body over any connection to the database.
func vacuumInto(ctx context.Context, db *sql.DB, destPath string) error {
	if err := fsutil.CreateSecretExclusive(destPath); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("sqlite backup to %q: destination already exists", destPath)
		}
		return fmt.Errorf("sqlite backup to %q: create destination: %w", destPath, err)
	}
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", destPath); err != nil {
		err = fmt.Errorf("sqlite backup to %q: %w", destPath, err)
		if rmErr := os.Remove(destPath); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("sqlite backup: remove partial copy %s: %w", destPath, rmErr))
		}
		return err
	}
	return nil
}
