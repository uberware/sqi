// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package target

import (
	"os"
	"syscall"
)

// killOne sends SIGKILL to the process alone. Unlike killTree it leaves the
// rest of the process group, such as the tasks a worker started, running.
func killOne(p *os.Process) error { return p.Signal(syscall.SIGKILL) }

// stopProcess sends SIGSTOP to the process alone.
func stopProcess(pid int) error { return syscall.Kill(pid, syscall.SIGSTOP) }

// stopGroup sends SIGSTOP to every process in the group.
func stopGroup(pgid int) error { return syscall.Kill(-pgid, syscall.SIGSTOP) }

// contProcess sends SIGCONT to the process alone.
func contProcess(pid int) error { return syscall.Kill(pid, syscall.SIGCONT) }

// contGroup sends SIGCONT to every process in the group.
func contGroup(pgid int) error { return syscall.Kill(-pgid, syscall.SIGCONT) }
