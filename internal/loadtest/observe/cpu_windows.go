// SPDX-License-Identifier: AGPL-3.0-or-later

package observe

import (
	"fmt"
	"syscall"
	"time"
)

// processCPUTime is the CPU time, user plus kernel, this process has used.
func processCPUTime() (time.Duration, error) {
	var creation, exit, kernel, user syscall.Filetime
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0, fmt.Errorf("observe: current process: %w", err)
	}
	if err := syscall.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return 0, fmt.Errorf("observe: GetProcessTimes: %w", err)
	}
	// FILETIME counts 100 ns intervals.
	ticks := func(f syscall.Filetime) int64 { return int64(f.HighDateTime)<<32 | int64(f.LowDateTime) }
	return time.Duration(ticks(kernel)+ticks(user)) * 100, nil
}
