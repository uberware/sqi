// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package executor

import (
	"errors"
	"os/exec"
	"syscall"
)

// processTree is the POSIX counterpart of the Windows job object. Containment
// is already established BEFORE start, by configureProcessGroup's Setpgid, so
// there is nothing to create here — this type exists only so run.go and
// killAndWait have one shape on both platforms.
type processTree struct{ proc *exec.Cmd }

// superviseTree records cmd. The process group was configured before Start.
func superviseTree(cmd *exec.Cmd) (*processTree, error) {
	return &processTree{proc: cmd}, nil
}

// release is a no-op: nothing kills a task's process group on a successful
// exit here, which internal/worker/staging's TOCTOU analysis depends on. The
// Windows implementation goes out of its way to match this.
func (*processTree) release() error { return nil }

// alive reports whether any process of the task's process group is still
// running. A group whose processes belong to another account (run-as-user)
// answers EPERM, which still means it has members.
func (t *processTree) alive() bool {
	err := syscall.Kill(-t.proc.Process.Pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
