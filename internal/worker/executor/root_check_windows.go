// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package executor

import "log/slog"

// isRunningAsRoot always returns false on Windows.  Administrator privilege
// detection on Windows requires checking group membership via the Windows
// security APIs, which is out of scope for Phase 1.  The root-user check is
// a Linux/macOS concern (see docs/worker-configuration.md, "worker.allow_root").
func isRunningAsRoot() bool {
	return false
}

// CheckRootUser is a no-op on Windows and always returns nil.
func CheckRootUser(_ bool, _ *slog.Logger) error {
	return nil
}
