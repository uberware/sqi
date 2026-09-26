// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package winsvc

import "os"

// createLogDir creates a missing log directory; the mode keeps it closed to
// other users.
func createLogDir(dir string) error {
	return os.MkdirAll(dir, 0o750)
}
