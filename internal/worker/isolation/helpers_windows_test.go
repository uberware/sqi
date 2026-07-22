// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package isolation_test

import (
	"os/exec"
	"syscall"
	"testing"
)

// configureTestProcessGroup mirrors executor.configureProcessGroup, whose
// unix implementation sets Setpgid on cmd's SysProcAttr before calling
// isolation.Apply. Windows has no process-group concept and no equivalent
// SysProcAttr field, so HideWindow stands in as an arbitrary pre-existing
// field that Apply must not clobber when it attaches the logon token — the
// same property the unix test proves for Setpgid.
func configureTestProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
}

// assertProcessGroupStillSet fails the test if Apply cleared HideWindow.
func assertProcessGroupStillSet(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow {
		t.Error("Apply must preserve pre-existing SysProcAttr fields on Windows too")
	}
}
