// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package executor

import "log/slog"

// IsRunningAsRoot always returns false on Windows: sqi does not detect
// Administrator privilege. The root-user check applies to Linux and macOS
// (see docs/worker-configuration.md, "worker.allow_root").
func IsRunningAsRoot() bool {
	return false
}

// CheckRootUser is a no-op on Windows and always returns nil.
func CheckRootUser(_ bool, _ *slog.Logger) error {
	return nil
}
