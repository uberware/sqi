// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package executor

import (
	"context"
	"errors"
	"log/slog"
	"os"
)

// IsRunningAsRoot returns true when the effective user ID of the worker
// process is 0 (root) on Linux or macOS.
//
// Exported (not just an internal CheckRootUser detail) because
// cmd/sqi-worker also needs it to decide the default run-as-user session
// root: /var/lib/sqi-worker-sessions when root (its ancestors are already
// traversable), falling back to the pre-split location under
// worker.data_dir otherwise. See cmd/sqi-worker's effectiveSessionRoot.
func IsRunningAsRoot() bool {
	return os.Geteuid() == 0
}

// CheckRootUser warns and returns an error if the worker is running as the
// root user on Linux/macOS and allowRoot is false (see
// docs/worker-configuration.md, "worker.allow_root").
//
// Call this at worker startup, before constructing an Executor.  If the
// function returns a non-nil error the caller should treat it as fatal and
// exit with a non-zero code.
func CheckRootUser(allowRoot bool, logger *slog.Logger) error {
	if !IsRunningAsRoot() {
		return nil
	}
	if allowRoot {
		logger.WarnContext(
			context.Background(),
			"executor: worker is running as root — allowed by allow_root configuration; "+
				"executing render processes as root is a security risk",
		)
		return nil
	}
	return errors.New(
		"worker process is running as root (UID 0), which is a security risk; " +
			"set allow_root: true in worker configuration or pass --allow-root to override",
	)
}
