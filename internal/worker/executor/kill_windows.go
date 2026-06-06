// SPDX-License-Identifier: AGPL-3.0-only

//go:build windows

package executor

import "os"

// sendTERM sends the closest Windows equivalent of SIGTERM to proc.
//
// Windows does not support POSIX signals; Process.Kill() calls TerminateProcess
// which is an immediate, non-graceful termination.  A proper graceful
// notification (e.g., posting WM_CLOSE or calling GenerateConsoleCtrlEvent)
// requires the Windows-specific cancellation path implemented in task 84.
// For now both sendTERM and sendKILL call Kill() so the timeout and shutdown
// paths still terminate the process rather than leaving it running.
func sendTERM(proc *os.Process) error {
	return proc.Kill()
}

// sendKILL forcibly terminates proc via TerminateProcess.
func sendKILL(proc *os.Process) error {
	return proc.Kill()
}
