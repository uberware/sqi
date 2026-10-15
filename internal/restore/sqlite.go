// SPDX-License-Identifier: AGPL-3.0-or-later

package restore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/uberware/sqi/internal/fsutil"
	"github.com/uberware/sqi/internal/store/migrations"
	"github.com/uberware/sqi/internal/store/sqlite"
)

// placeSQLite stages the backup beside the live database, checks the staged
// copy (so the backup file itself is never opened), moves the live database
// and its sidecars aside, and renames the staged copy into place.
func placeSQLite(ctx context.Context, o Options, stamp string) (version int64, moved []string, err error) {
	staging := o.SQLitePath + ".restore-tmp"
	if err := refuseCollisions(o.Backup, o.SQLitePath, staging); err != nil {
		return 0, nil, err
	}
	if err := refuseBackupWAL(o.Backup); err != nil {
		return 0, nil, err
	}
	removeDB(staging) // a previous failed restore's leftovers are ours
	if err := copySecret(o.Backup, staging); err != nil {
		return 0, nil, err
	}
	if version, err = inspectSQLite(ctx, staging); err != nil {
		removeDB(staging)
		return 0, nil, err
	}
	for _, p := range []string{o.SQLitePath, o.SQLitePath + "-wal", o.SQLitePath + "-shm"} {
		if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		to, err := moveAside(p, stamp)
		if err != nil {
			return version, moved, fmt.Errorf("%w%s", err, previousAt(moved))
		}
		moved = append(moved, to)
	}
	if err := rename(staging, o.SQLitePath); err != nil {
		return version, moved, fmt.Errorf("restore: put the backup in place: %w; the verified copy of the backup "+
			"is at %s%s", err, staging, previousAt(moved))
	}
	return version, moved, nil
}

// refuseCollisions refuses a backup that is the live database (which restore
// would move aside, leaving nothing to put in its place) or the staging file
// (which restore clears before copying into it).
func refuseCollisions(backup, live, staging string) error {
	if same, err := samePath(backup, live); err != nil {
		return err
	} else if same {
		return fmt.Errorf("restore: %s is the live database, not a backup", backup)
	}
	if same, err := samePath(backup, staging); err != nil {
		return err
	} else if same {
		return fmt.Errorf("restore: %s is restore's own staging file; copy the backup elsewhere first", backup)
	}
	return nil
}

// refuseBackupWAL refuses a backup with a non-empty -wal file: restore copies
// only the database file, so any committed writes in the -wal would be lost,
// and the copy would still pass the integrity check.
func refuseBackupWAL(backup string) error {
	n, err := walSize(backup)
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("restore: %[1]s has a -wal file that is not empty (%[2]d bytes), so it may hold writes "+
			"that are not in %[1]s, and restore copies only the database file, so they would be lost: stop the "+
			"server using %[1]s "+
			"cleanly (which folds them in) and run restore again, or restore a backup made with "+
			"\"sqi-server backup\", which never has a -wal file", backup, n)
	}
	return nil
}

// inspectSQLite checks the staged copy is an intact sqi database no newer
// than this binary, and returns its schema version.
func inspectSQLite(ctx context.Context, path string) (int64, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return 0, fmt.Errorf("restore: open the backup: %w", err)
	}
	defer db.Close()
	var ok string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&ok); err != nil {
		return 0, fmt.Errorf("%w: integrity check failed: %w", ErrNotSQIBackup, err)
	}
	if ok != "ok" {
		return 0, fmt.Errorf("%w: integrity check failed: %s", ErrNotSQIBackup, ok)
	}
	v, err := sqlite.SchemaVersion(ctx, db)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrNotSQIBackup, err)
	}
	if v == 0 {
		return 0, fmt.Errorf("%w: it has no sqi schema", ErrNotSQIBackup)
	}
	if err := migrations.CheckVersion("sqlite", v); err != nil {
		return 0, fmt.Errorf("restore: %w", err)
	}
	return v, nil
}

// copySecret copies src to dst, which it creates exclusively and owner-only:
// the database holds password, API-key and session hashes.
func copySecret(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("restore: open backup %s: %w", src, err)
	}
	defer in.Close()
	if err := fsutil.CreateSecretExclusive(dst); err != nil {
		return fmt.Errorf("restore: create %s: %w", dst, err)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		removeDB(dst)
		return fmt.Errorf("restore: open %s: %w", dst, err)
	}
	_, cpErr := io.Copy(out, in)
	syncErr := out.Sync()
	closeErr := out.Close()
	if err := errors.Join(cpErr, syncErr, closeErr); err != nil {
		removeDB(dst)
		return fmt.Errorf("restore: copy backup to %s: %w", dst, err)
	}
	return nil
}

// removeDB removes a database file and its sidecars, ignoring absence.
func removeDB(p string) {
	for _, f := range []string{p, p + "-wal", p + "-shm", p + "-journal"} {
		_ = os.Remove(f)
	}
}

// samePath reports whether a and b name the same file. a must exist; a b that
// does not cannot collide with it.
func samePath(a, b string) (bool, error) {
	fa, err := os.Stat(a)
	if err != nil {
		return false, fmt.Errorf("restore: backup %s: %w", a, err)
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false, nil //nolint:nilerr // no file at b: nothing to collide with
	}
	if os.SameFile(fa, fb) {
		return true, nil
	}
	aa, errA := filepath.Abs(a)
	ab, errB := filepath.Abs(b)
	return errA == nil && errB == nil && aa == ab, nil
}
