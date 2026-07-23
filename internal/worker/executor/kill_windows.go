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
//
// exec.Command is used intentionally instead of exec.CommandContext (noctx),
// the same reasoning run.go records for the task process itself: sendTERM and
// sendKILL are called by killAndWait *as* the mechanism that implements the
// OpenJD cancelation policy, typically only after the caller's own ctx is
// already canceled or timed out — that is precisely why we are killing the
// process. Binding these short, synchronous taskkill invocations to that same
// context would be actively wrong: os/exec kills a command as soon as its
// context is done, so an already-canceled ctx could abort taskkill before it
// signals the target, and this function would never learn whether taskkill
// succeeded, defeating the proc.Kill() fallback below. There is no other
// context in scope that would be more appropriate to use here, so these
// helpers deliberately run unbound by any context. (gosec's G204 command-
// injection check only inspects the command name, "taskkill" — a literal, not
// a variable — so it does not fire on either call below; no gosec suppression
// is needed here.)
func sendTERM(proc *os.Process) error {
	pid := strconv.Itoa(proc.Pid)
	if err := exec.Command("taskkill", "/T", "/PID", pid).Run(); err != nil { //nolint:noctx // pid is an integer, not user input; context deliberately not threaded, see doc comment above
		// taskkill /T failed: proc.Kill() terminates the process immediately,
		// skipping the NOTIFY_THEN_TERMINATE grace window entirely.
		return proc.Kill()
	}
	return nil
}

// sendKILL force-terminates proc and all child processes using
// "taskkill /F /T /PID <pid>".  Falls back to proc.Kill() on failure.
//
// See sendTERM's doc comment for why exec.Command (not exec.CommandContext)
// is used deliberately here too.
func sendKILL(proc *os.Process) error {
	pid := strconv.Itoa(proc.Pid)
	if err := exec.Command("taskkill", "/F", "/T", "/PID", pid).Run(); err != nil { //nolint:noctx // pid is an integer, not user input; context deliberately not threaded, see sendTERM's doc comment
		return proc.Kill()
	}
	return nil
}
