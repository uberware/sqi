// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux || darwin

package bus

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
)

// fdHeadroom reports whether this process's open file descriptors leave
// enough of its soft limit free for a broker restart.
func fdHeadroom() error {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return fmt.Errorf("read the file descriptor limit: %w", err)
	}
	dir := "/proc/self/fd"
	if runtime.GOOS == "darwin" {
		dir = "/dev/fd"
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("count open file descriptors: %w", err)
	}
	// The read held one descriptor of its own, which it has closed.
	open := uint64(max(len(entries)-1, 0))
	return fdHeadroomFrom(open, lim.Cur)
}
