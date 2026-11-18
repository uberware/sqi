// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package isolation_test

import (
	"os/exec"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

// configureTestProcessGroup mirrors executor.configureProcessGroup, which on
// Windows sets CREATE_SUSPENDED in cmd's SysProcAttr.CreationFlags before
// calling isolation.Apply. Apply must keep that flag when it attaches the
// logon token, the same property the unix test proves for Setpgid. The
// command is never started.
func configureTestProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
}

// assertProcessGroupStillSet fails the test if Apply cleared CREATE_SUSPENDED.
func assertProcessGroupStillSet(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.CreationFlags&windows.CREATE_SUSPENDED == 0 {
		t.Error("Apply must preserve pre-existing SysProcAttr fields on Windows too")
	}
}
