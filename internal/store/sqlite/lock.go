// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/uberware/sqi/internal/fslock"
)

// ErrServerLockHeld means another sqi-server, or a command that must not run
// beside one (restore, store copy, migrate up), holds the database's lock file.
var ErrServerLockHeld = fslock.ErrHeld

// maxLinks bounds the dangling links ResolvePath follows, as Linux
// bounds a path lookup.
const maxLinks = 40

// LockPath is the lock file guarding the database at dbPath: the database
// file's own path, symlinks resolved, plus ".lock". Every symlinked name for
// one database, including a link to one not created yet, gives the same lock
// file, so two servers cannot each hold a lock on it. A hard link is a name
// the filesystem cannot tell from the original, and gets a lock file of its
// own.
func LockPath(dbPath string) (string, error) {
	p, err := ResolvePath(dbPath)
	if err != nil {
		return "", fmt.Errorf("sqlite: resolve %s: %w", dbPath, err)
	}
	return p + ".lock", nil
}

// AcquireLock takes the single-server lock for the database at dbPath.
func AcquireLock(dbPath string) (*fslock.Lock, error) {
	lockPath, err := LockPath(dbPath)
	if err != nil {
		return nil, err
	}
	l, err := fslock.Acquire(lockPath)
	if errors.Is(err, fslock.ErrHeld) {
		return nil, fmt.Errorf("another sqi-server is using %s (lock file %s is held); stop it first: %w",
			dbPath, lockPath, err)
	}
	return l, err
}

// ResolvePath is the absolute path, symlinks resolved, of the database file
// SQLite opens for path. When nothing exists there yet, a dangling link is
// followed to the file SQLite would create through it; otherwise only the
// directory is resolved. An in-memory database has no file and keeps its name.
//
// Commands resolve a configured path once, so the server and a backup or
// restore given another name for the database all work on one file: SQLite on
// Windows names the -wal and -shm files after the name it is given, and
// restore moves aside and replaces whatever file the path names.
func ResolvePath(path string) (string, error) {
	if !isSharedPath(path) {
		return path, nil
	}
	p, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	for range maxLinks {
		resolved, err := filepath.EvalSymlinks(p)
		if err == nil {
			return resolved, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		target, err := os.Readlink(p)
		if err != nil {
			return resolveDir(p)
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(p), target)
		}
		p = target
	}
	return "", fmt.Errorf("more than %d links", maxLinks)
}

// resolveDir resolves the directory of p, which does not exist yet. A missing
// directory leaves p as it is, for the caller's own open to report.
func resolveDir(p string) (string, error) {
	dir, err := filepath.EvalSymlinks(filepath.Dir(p))
	if errors.Is(err, fs.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, filepath.Base(p)), nil
}
