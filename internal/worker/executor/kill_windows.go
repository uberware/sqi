// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

// Windows-compatible process termination.
//
// Windows does not support POSIX signals, so the OpenJD Action.cancelation modes
// are approximated:
//
//   - TERMINATE (the spec default): killAndWait calls sendKILL directly, which
//     terminates the whole job object (TerminateJobObject) — the closest
//     Windows equivalent of SIGKILL, and one that reaps every process in the
//     tree including a grandchild orphaned before the call (see
//     procgroup_windows.go's superviseTree doc for why "taskkill /F /T"
//     cannot do that).
//   - NOTIFY_THEN_TERMINATE: killAndWait calls sendTERM first.  There is no true
//     SIGTERM on Windows; sendTERM uses "taskkill /T /PID" (without /F) as a
//     best-effort graceful request that lets GUI processes handle WM_CLOSE.
//     Console processes that do not handle it are killed by the OS on a
//     best-effort basis.  After the notify period, killAndWait escalates to
//     sendKILL.  This is an approximation: a console process may not receive a
//     graceful-shutdown notification equivalent to SIGTERM.
//
// sendTERM falls back to sendKILL (job-object terminate) if taskkill is
// unavailable or returns an error, ensuring the process is always terminated.
// sendKILL itself falls back to the old taskkill/proc.Kill() behavior if the
// job handle is somehow absent, so a failure to create the job never leaves a
// task unkillable.

package executor

import (
	"fmt"
	"os/exec"
	"strconv"

	"golang.org/x/sys/windows"
)

// configureProcessGroup is a no-op on Windows: containment is established
// after Start by superviseTree (procgroup_windows.go), because a job object
// can only be assigned to a process that already exists.
func configureProcessGroup(*exec.Cmd) {}

// sendTERM makes a best-effort graceful request via "taskkill /T" (without
// /F), which lets a GUI process handle WM_CLOSE. It deliberately does NOT
// terminate the job object: NOTIFY_THEN_TERMINATE's whole point is the grace
// window before the hard kill, and TerminateJobObject offers no such
// courtesy. killAndWait escalates to sendKILL after the notify period.
//
// exec.Command is used intentionally instead of exec.CommandContext (noctx),
// the same reasoning run.go records for the task process itself: sendTERM and
// sendKILL are called by killAndWait *as* the mechanism that implements the
// OpenJD cancelation policy, typically only after the caller's own ctx is
// already canceled or timed out — that is precisely why we are killing the
// process. Binding this short, synchronous taskkill invocation to that same
// context would be actively wrong: os/exec kills a command as soon as its
// context is done, so an already-canceled ctx could abort taskkill before it
// signals the target, and this function would never learn whether taskkill
// succeeded, defeating the sendKILL fallback below. There is no other context
// in scope that would be more appropriate to use here, so this helper
// deliberately runs unbound by any context. (gosec's G204 command-injection
// check only inspects the command name, "taskkill" — a literal, not a
// variable — so it does not fire on the call below; no gosec suppression is
// needed here.)
func (t *processTree) sendTERM() error {
	pid := strconv.Itoa(t.proc.Process.Pid)
	if err := exec.Command("taskkill", "/T", "/PID", pid).Run(); err != nil { //nolint:noctx // pid is an integer, not user input; context deliberately not threaded, see doc comment above
		return t.sendKILL()
	}
	return nil
}

// sendKILL force-terminates the entire job object, which reaps every process
// in the tree — including a grandchild orphaned before this call, which
// "taskkill /F /T" would miss (see superviseTree's doc). Falls back to the
// old taskkill path if the job handle is somehow absent, so a failure to
// create the job never leaves a task unkillable.
func (t *processTree) sendKILL() error {
	if t.job == 0 {
		pid := strconv.Itoa(t.proc.Process.Pid)
		if err := exec.Command("taskkill", "/F", "/T", "/PID", pid).Run(); err != nil { //nolint:noctx // pid is an integer, not user input; context deliberately not threaded, see sendTERM's doc comment
			return t.proc.Process.Kill()
		}
		return nil
	}
	if err := windows.TerminateJobObject(t.job, 1); err != nil {
		return fmt.Errorf("terminate job object: %w", err)
	}
	return nil
}
