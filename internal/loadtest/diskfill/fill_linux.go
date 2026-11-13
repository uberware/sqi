// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux

package diskfill

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// preallocate reserves size bytes for f with fallocate.
func preallocate(f *os.File, size int64) error {
	if err := unix.Fallocate(int(f.Fd()), 0, 0, size); err != nil {
		return fmt.Errorf("fallocate: %w", err)
	}
	return nil
}
