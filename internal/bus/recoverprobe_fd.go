// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux || darwin

package bus

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
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
	// The list includes the descriptor the read itself held, closed since;
	// counting it errs by one toward waiting.
	open := make([]int, 0, len(entries))
	for _, e := range entries {
		fd, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		open = append(open, fd)
	}
	return fdHeadroomFrom(open, lim.Cur)
}
