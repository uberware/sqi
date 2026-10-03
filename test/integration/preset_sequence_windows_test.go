// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package integration

import "testing"

// launcherForwardsSignalExit has nothing to prove on Windows: the executor
// cancels a task by terminating its whole job object (kill_windows.go), which
// no handler can observe, so there is no signal for the launcher to survive.
func launcherForwardsSignalExit(t *testing.T, _, _ string) {
	t.Helper()
	t.Skip("POSIX process-group signaling only; Windows terminates the job object")
}
