// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

// Package proctest helps the load harness's tests check on the processes a
// run started. It is importable only from under internal/loadtest.
package proctest

import (
	"errors"
	"syscall"
)

// Alive reports whether a process with this id exists. A process its parent
// has reaped no longer does.
func Alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
