// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

// Task 84: Windows-compatible process termination.
//
// Windows does not support POSIX signals.  sendTERM uses "taskkill /T /PID"
// (terminate the process tree without /F, which can allow GUI processes to
// clean up; console processes that do not handle WM_CLOSE are killed anyway
// by the OS on a best-effort basis).  sendKILL uses "taskkill /F /T /PID"
// to force-terminate the process and all its children regardless of state.
//
// Both functions fall back to proc.Kill() (TerminateProcess syscall) if
// taskkill is unavailable or returns an error, ensuring the process is always
// terminated during the kill-grace-period window.

package executor

import (
	"os"
	"os/exec"
	"strconv"
)

// sendTERM attempts to terminate proc's process tree on Windows using
// "taskkill /T /PID <pid>".  Falls back to proc.Kill() (TerminateProcess —
// an immediate hard kill, not a graceful termination window) on failure.
func sendTERM(proc *os.Process) error {
	pid := strconv.Itoa(proc.Pid)
	if err := exec.Command("taskkill", "/T", "/PID", pid).Run(); err != nil { //nolint:gosec // pid is an integer, not user input
		return proc.Kill()
	}
	return nil
}

// sendKILL force-terminates proc and all child processes using
// "taskkill /F /T /PID <pid>".  Falls back to proc.Kill() on failure.
func sendKILL(proc *os.Process) error {
	pid := strconv.Itoa(proc.Pid)
	if err := exec.Command("taskkill", "/F", "/T", "/PID", pid).Run(); err != nil { //nolint:gosec // pid is an integer, not user input
		return proc.Kill()
	}
	return nil
}
