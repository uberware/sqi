// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux

package target

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// fdLimit is a process's RLIMIT_NOFILE.
type fdLimit struct {
	soft, hard uint64
}

// limitFDs lowers pid's soft RLIMIT_NOFILE so that it can open headroom more
// files, leaves the hard limit and returns the open count and the limit it
// replaced. Lowering the soft limit of one's own child needs no privilege, and
// the unchanged hard limit lets restoreFDs raise it again.
func limitFDs(pid, headroom int) (open int, old fdLimit, err error) {
	if headroom < 0 {
		return 0, fdLimit{}, fmt.Errorf("target: the open-file headroom is %d, want at least 0", headroom)
	}
	entries, err := os.ReadDir("/proc/" + strconv.Itoa(pid) + "/fd")
	if err != nil {
		return 0, fdLimit{}, fmt.Errorf("target: count the open files of process %d: %w", pid, err)
	}
	used := make(map[int]bool, len(entries))
	for _, e := range entries {
		if fd, err := strconv.Atoi(e.Name()); err == nil {
			used[fd] = true
		}
	}
	open = len(used)
	var cur unix.Rlimit
	if err := unix.Prlimit(pid, unix.RLIMIT_NOFILE, nil, &cur); err != nil {
		return 0, fdLimit{}, fmt.Errorf("target: read the open-file limit of process %d: %w", pid, err)
	}
	soft := softLimitFor(used, headroom)
	if soft > cur.Max {
		return 0, fdLimit{}, fmt.Errorf("target: %d more open files need a limit of %d, over process %d's hard limit of %d", headroom, soft, pid, cur.Max)
	}
	lowered := unix.Rlimit{Cur: soft, Max: cur.Max}
	if err := unix.Prlimit(pid, unix.RLIMIT_NOFILE, &lowered, nil); err != nil {
		return 0, fdLimit{}, fmt.Errorf("target: lower the open-file limit of process %d: %w", pid, err)
	}
	return open, fdLimit{soft: cur.Cur, hard: cur.Max}, nil
}

// softLimitFor is the lowest RLIMIT_NOFILE soft limit that leaves exactly
// headroom free descriptor numbers below it, given the descriptors in use. The
// limit bounds descriptor numbers, not how many are open, so a gap or a
// high-numbered descriptor moves it away from len(used)+headroom.
func softLimitFor(used map[int]bool, headroom int) uint64 {
	free := 0
	for fd := 0; ; fd++ {
		if used[fd] {
			continue
		}
		if free == headroom {
			return uint64(fd)
		}
		free++
	}
}

// restoreFDs puts back the limit limitFDs replaced. A process that is gone has
// nothing to restore, which is not an error.
func restoreFDs(pid int, old fdLimit) error {
	l := unix.Rlimit{Cur: old.soft, Max: old.hard}
	if err := unix.Prlimit(pid, unix.RLIMIT_NOFILE, &l, nil); err != nil && !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("target: restore the open-file limit of process %d: %w", pid, err)
	}
	return nil
}
