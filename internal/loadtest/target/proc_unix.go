// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package target

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// platformProc holds the child's process group, whose id is the child's pid.
type platformProc struct {
	pgid int
}

// release kills whatever is left in the child's process group, as closing
// the job does on Windows: after a stop, or a kill of the child alone, that
// can only be something the child started and left behind. A group that is
// already empty is not an error.
//
// Once the group is empty its id can be reused by an unrelated process, which
// a later release would then signal; the harness calls release once, when it
// is done with the process.
func (p platformProc) release() {
	if p.pgid > 0 {
		syscall.Kill(-p.pgid, syscall.SIGKILL) //nolint:errcheck // ESRCH (nothing left) is the usual answer; nothing to do on any other
	}
}

// setProcAttrs puts the child in its own process group, so a Ctrl-C at the
// terminal reaches only the harness, which then stops its children in order,
// and so a kill can take the whole group. On Linux the child is also killed
// if the harness dies first.
func setProcAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	setParentDeathSignal(cmd.SysProcAttr)
}

func supervise(p *os.Process) (platformProc, error) { return platformProc{pgid: p.Pid}, nil }

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
