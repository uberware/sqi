// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration && windows

package integration

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// setStoppableAttrs starts cmd in a new console process group, so
// [interruptProcess] can send that group CTRL_BREAK_EVENT without reaching
// this test process.
func setStoppableAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

// interruptProcess sends CTRL_BREAK_EVENT to p's console process group, which
// Go delivers to sqi-worker as os.Interrupt: Windows has no signal one process
// can send another. It fails when this process shares no console with p, and
// the caller then kills it instead.
func interruptProcess(p *os.Process) error {
	if p.Pid <= 0 {
		return errors.New("no process id") // group 0 would be every process on the console, this one included
	}
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(p.Pid))
}
