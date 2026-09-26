// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package winsvc

import "context"

// IsService is always false off Windows.
func IsService() bool { return false }

// Run calls fn under a SIGINT/SIGTERM-canceled context. name and configPath
// only matter on Windows.
func Run(_ string, _ *string, fn func(ctx context.Context) error) error {
	return runConsole(fn)
}
