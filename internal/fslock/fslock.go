// SPDX-License-Identifier: AGPL-3.0-or-later

// Package fslock takes an exclusive, non-blocking operating-system lock on a
// file. The lock, not the file's existence, is the guard: the OS drops it when
// the holder exits however it exits, so a crashed process leaves no stale lock,
// and the file is never deleted (deleting a lock file races a process about to
// open it).
//
// POSIX uses flock(2), which conflicts per open file description; Windows uses
// LockFileEx, which conflicts per handle. Either way two Acquire calls in one
// process conflict, which is what a CLI checking for a running server needs.
// Locks on network filesystems are only as reliable as the filesystem's lock
// emulation.
package fslock

import (
	"errors"
	"fmt"
	"os"
)

// ErrHeld means another process (or another Acquire in this one) holds the lock.
var ErrHeld = errors.New("fslock: lock is held")

// Lock is a held lock. Release it exactly once.
type Lock struct {
	f    *os.File
	path string
}

// Acquire creates path if absent (owner-only) and locks it exclusively,
// failing at once with [ErrHeld] when it is already locked. The caller must
// keep the returned *Lock reachable until Release: an unreachable Lock's file
// is closed by its finalizer, which unlocks it.
func Acquire(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("fslock: open %s: %w", path, err)
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		if errors.Is(err, ErrHeld) {
			return nil, err
		}
		return nil, fmt.Errorf("fslock: lock %s: %w", path, err)
	}
	return &Lock{f: f, path: path}, nil
}

// Path is the locked file.
func (l *Lock) Path() string { return l.path }

// Release unlocks and closes the file. The file stays on disk.
func (l *Lock) Release() error {
	return errors.Join(unlockFile(l.f), l.f.Close())
}
