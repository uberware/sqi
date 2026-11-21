// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux || darwin

package proctree

import (
	"context"
	"errors"
	"log/slog"
	"syscall"
	"time"
)

// Sweep timing: the sweep rescans for marked processes for sweepDeadline,
// every sweepPoll, and gives a scan in progress sweepSlack more, so it waits
// at most 5 seconds in all.
const (
	sweepDeadline = 4 * time.Second
	sweepSlack    = time.Second
	sweepPoll     = 50 * time.Millisecond
)

// Sweep kills every process a previous worker on this data directory left
// running: each process whose EnvVar carries a token one of the ledger's
// records names. It then deletes every record. Run it before the worker
// registers, since the registration is what reclaims (and re-runs) the dead
// worker's tasks, and only while holding the data directory's lock, so that a
// live worker's trees are never swept. It waits at most 5 seconds. Only an
// unreadable ledger directory is returned as an error; everything else is
// logged. A nil ledger does nothing.
func (l *Ledger) Sweep(ctx context.Context, logger *slog.Logger) error {
	return l.sweepWith(ctx, logger, systemOps())
}

// killProcess sends SIGKILL to pid alone. A process already gone is not an
// error.
func killProcess(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// envToken returns EnvVar's value from a NUL-separated environment block, or
// "".
func envToken(env []string) string {
	const prefix = EnvVar + "="
	for _, kv := range env {
		if len(kv) > len(prefix) && kv[:len(prefix)] == prefix {
			return kv[len(prefix):]
		}
	}
	return ""
}
