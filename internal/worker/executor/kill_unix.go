// SPDX-License-Identifier: AGPL-3.0-only

//go:build unix

package executor

import (
	"os"
	"syscall"
)

// sendTERM sends SIGTERM to proc, requesting graceful termination.
// Returns nil on success or an error if the signal could not be delivered
// (e.g., the process has already exited).
func sendTERM(proc *os.Process) error {
	return proc.Signal(syscall.SIGTERM)
}

// sendKILL sends SIGKILL to proc, forcibly terminating it.
// Returns nil on success or an error if the signal could not be delivered.
func sendKILL(proc *os.Process) error {
	return proc.Kill()
}
