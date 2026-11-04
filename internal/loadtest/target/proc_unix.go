// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package target

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// platformProc holds nothing on Unix: the process group is the process's id.
type platformProc struct{}

func (platformProc) release() {}

// setProcAttrs puts the child in its own process group, so a Ctrl-C at the
// terminal reaches only the harness, which then stops its children in order,
// and so a kill can take the whole group. On Linux the child is also killed
// if the harness dies first.
func setProcAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	setParentDeathSignal(cmd.SysProcAttr)
}

func supervise(*os.Process) (platformProc, error) { return platformProc{}, nil }

// interrupt sends SIGINT, which sqi-server and sqi-worker treat as a request
// to shut down cleanly.
func interrupt(p *os.Process) error { return p.Signal(os.Interrupt) }

// killTree kills the process's whole group, which takes any child it started
// along with it.
func killTree(p *os.Process, _ platformProc) error {
	err := syscall.Kill(-p.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return p.Kill()
	}
	return err
}

// processAlive reports whether a process with this id exists. A process the
// harness has reaped no longer does.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
