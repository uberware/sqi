// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

// Package proctest helps the load harness's tests check on the processes a
// run started. It is importable only from under internal/loadtest.
package proctest

import (
	"errors"

	"golang.org/x/sys/windows"
)

// Alive reports whether a process with this id is still running.
func Alive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid)) //nolint:gosec // G115: PIDs fit in uint32
	if err != nil {
		// No such process is ERROR_INVALID_PARAMETER; one we may not open
		// still exists.
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h) //nolint:errcheck // done with it either way
	ev, err := windows.WaitForSingleObject(h, 0)
	return err == nil && ev == uint32(windows.WAIT_TIMEOUT)
}
