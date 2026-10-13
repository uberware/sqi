// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"errors"
	"fmt"

	"github.com/uberware/sqi/internal/fslock"
)

// ErrServerLockHeld means another sqi-server, or a command that must not run
// beside one (restore, store copy, migrate up), holds the database's lock file.
var ErrServerLockHeld = fslock.ErrHeld

// LockPath is the lock file guarding the database at dbPath.
func LockPath(dbPath string) string { return dbPath + ".lock" }

// AcquireLock takes the single-server lock for the database at dbPath.
func AcquireLock(dbPath string) (*fslock.Lock, error) {
	l, err := fslock.Acquire(LockPath(dbPath))
	if errors.Is(err, fslock.ErrHeld) {
		return nil, fmt.Errorf("another sqi-server is using %s (lock file %s is held); stop it first: %w",
			dbPath, LockPath(dbPath), err)
	}
	return l, err
}
