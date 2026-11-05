// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package observe

import (
	"fmt"
	"syscall"
	"time"
)

// processCPUTime is the CPU time, user plus system, this process has used.
func processCPUTime() (time.Duration, error) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, fmt.Errorf("observe: getrusage: %w", err)
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano()), nil
}
