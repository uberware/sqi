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

// BackupPath creates a consistent, online copy of the database at dbPath in
// destPath using SQLite's VACUUM INTO statement, without opening the store, so
// it works on a schema this binary is too old for (the backup taken before
// rolling a binary back). The source database remains fully available for
// reads and writes during the operation.
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
func BackupPath(ctx context.Context, dbPath, destPath string) error {
	dsn, err := FileURI(dbPath, false)
	if err != nil {
		return fmt.Errorf("sqlite backup: %s: %w", dbPath, err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("sqlite backup: open %s: %w", dbPath, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
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
