// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package target

import (
	"errors"
	"fmt"
	"syscall"
)

// stopProcess sends SIGSTOP to the process alone.
func stopProcess(pid int) error { return syscall.Kill(pid, syscall.SIGSTOP) }

// stopGroup sends SIGSTOP to every process in the group.
func stopGroup(pgid int) error { return signalGroup(pgid, syscall.SIGSTOP) }

// contProcess sends SIGCONT to the process alone.
func contProcess(pid int) error { return syscall.Kill(pid, syscall.SIGCONT) }

// contGroup sends SIGCONT to every process in the group.
func contGroup(pgid int) error { return signalGroup(pgid, syscall.SIGCONT) }

// killGroup sends SIGKILL to every process in the group.
func killGroup(pgid int) error { return signalGroup(pgid, syscall.SIGKILL) }

// signalGroup signals every process in group pgid, refusing a group that is
// not safe to signal (see safeGroup).
func signalGroup(pgid int, sig syscall.Signal) error {
	if !safeGroup(pgid) {
		return fmt.Errorf("%w: %d", errUnsafeGroup, pgid)
	}
	return syscall.Kill(-pgid, sig)
}

// ownGroup is the harness's own process group.
func ownGroup() int { return syscall.Getpgrp() }

// safeGroup reports whether a group id read from a marker may be signaled:
// kill(-0) is the harness's own group, kill(-1) every process the user may
// signal, and the harness's own group would take the harness with it.
func safeGroup(pgid int) bool { return pgid > 1 && pgid != ownGroup() }

// groupGone reports whether a group signal failed because nothing is left in
// the group.
func groupGone(err error) bool { return errors.Is(err, syscall.ESRCH) }
