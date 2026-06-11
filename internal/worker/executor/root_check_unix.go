// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package executor

import (
	"context"
	"errors"
	"log/slog"
	"os"
)

// isRunningAsRoot returns true when the effective user ID of the worker
// process is 0 (root) on Linux or macOS.
func isRunningAsRoot() bool {
	return os.Geteuid() == 0
}

// CheckRootUser warns and returns an error if the worker is running as the
// root user on Linux/macOS and allowRoot is false (task 57, sqi.md §18,
// open question 2).
//
// Call this at worker startup, before constructing an Executor.  If the
// function returns a non-nil error the caller should treat it as fatal and
// exit with a non-zero code.
func CheckRootUser(allowRoot bool, logger *slog.Logger) error {
	if !isRunningAsRoot() {
		return nil
	}
	if allowRoot {
		logger.WarnContext(
			context.Background(),
			"executor: worker is running as root — allowed by allow_root configuration; "+
				"executing render processes as root is a security risk (sqi.md §18)",
		)
		return nil
	}
	return errors.New(
		"worker process is running as root (UID 0), which is a security risk " +
			"(sqi.md §18, open question 2); " +
			"set allow_root: true in worker configuration or pass --allow-root to override",
	)
}
