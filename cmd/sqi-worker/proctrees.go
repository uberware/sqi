// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/uberware/sqi/internal/fslock"
	"github.com/uberware/sqi/internal/worker/proctree"
)

// lockDataDir takes the exclusive lock on <dataDir>/worker.lock that this
// worker holds for its whole life. Two workers on one data directory share a
// worker ID, and the startup sweep would kill the other one's running tasks,
// so the second is refused.
func lockDataDir(dataDir string) (*fslock.Lock, error) {
	l, err := fslock.Acquire(filepath.Join(dataDir, "worker.lock"))
	if errors.Is(err, fslock.ErrHeld) {
		return nil, fmt.Errorf("another sqi-worker is already running with data_dir %s", dataDir)
	}
	if err != nil {
		return nil, fmt.Errorf("lock data_dir: %w", err)
	}
	return l, nil
}

// claimDataDir locks dataDir for this worker's life (lockDataDir), opens the
// process-tree ledger in <dataDir>/proctrees, and kills every tree a previous
// worker on this data directory left running when it was killed. It must run
// before the worker registers, because the registration reclaims those trees'
// tasks and they would run again beside the old processes. The returned
// release drops the lock; on an error the lock is not held.
func claimDataDir(ctx context.Context, dataDir string, logger *slog.Logger) (release func(), ledger *proctree.Ledger, err error) {
	lock, err := lockDataDir(dataDir)
	if err != nil {
		return nil, nil, err
	}
	release = func() {
		if err := lock.Release(); err != nil {
			logger.WarnContext(context.Background(), "release data_dir lock", slog.Any("error", err))
		}
	}
	ledger, err = proctree.OpenLedger(filepath.Join(dataDir, "proctrees"))
	if err == nil {
		err = ledger.Sweep(ctx, logger)
	}
	if err != nil {
		release()
		return nil, nil, err
	}
	return release, ledger, nil
}
