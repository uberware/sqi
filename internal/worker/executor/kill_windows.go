// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

// Windows-compatible process termination.
//
// Windows does not support POSIX signals, so the OpenJD Action.cancelation modes
// are approximated:
//
//   - TERMINATE (the spec default): killAndWait calls sendKILL directly, which
//     uses "taskkill /F /T /PID" (TerminateProcess equivalent) to force-terminate
//     the process tree immediately — the closest Windows equivalent of SIGKILL.
//   - NOTIFY_THEN_TERMINATE: killAndWait calls sendTERM first.  There is no true
//     SIGTERM on Windows; sendTERM uses "taskkill /T /PID" (without /F) as a
//     best-effort graceful request that lets GUI processes handle WM_CLOSE.
//     Console processes that do not handle it are killed by the OS on a
//     best-effort basis.  After the notify period, killAndWait escalates to
//     sendKILL.  This is an approximation: a console process may not receive a
//     graceful-shutdown notification equivalent to SIGTERM.
//
// Both functions fall back to proc.Kill() (TerminateProcess syscall) if
// taskkill is unavailable or returns an error, ensuring the process is always
// terminated.

package executor

import (
	"os"
	"os/exec"
	"strconv"
)

// configureProcessGroup is a no-op on Windows: sendKILL/sendTERM already
// terminate the whole process tree via "taskkill /T", so no special
// process-group setup is required at launch.
func configureProcessGroup(*exec.Cmd) {}

// sendTERM attempts to terminate proc's process tree on Windows using
// "taskkill /T /PID <pid>".  Falls back to proc.Kill() (TerminateProcess —
// an immediate hard kill, not a graceful termination window) on failure.
func sendTERM(proc *os.Process) error {
	pid := strconv.Itoa(proc.Pid)
	if err := exec.Command("taskkill", "/T", "/PID", pid).Run(); err != nil { //nolint:gosec // pid is an integer, not user input
		// taskkill /T failed: proc.Kill() terminates the process immediately,
		// skipping the NOTIFY_THEN_TERMINATE grace window entirely.
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
