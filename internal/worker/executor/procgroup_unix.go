// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package executor

import (
	"errors"
	"os/exec"
	"syscall"
)

// processTree is the POSIX counterpart of the Windows job object. Containment
// is established before Start by configureProcessGroup's Setpgid, so there is
// nothing to create here; the type gives run.go and killAndWait one shape on
// both platforms.
type processTree struct{ proc *exec.Cmd }

// superviseTree records cmd. The process group was configured before Start.
func superviseTree(cmd *exec.Cmd) (*processTree, error) {
	return &processTree{proc: cmd}, nil
}

// release is a no-op: processes still running in the task's process group
// when the task exits successfully are left running. internal/worker/staging's
// stage-out checks (openStageOutSource) allow for such a process, and the
// Windows release matches this behavior.
func (*processTree) release() error { return nil }

// alive reports whether any process of the task's process group is still
// running. A group whose processes belong to another account (run-as-user)
// answers EPERM, which still means it has members.
func (t *processTree) alive() bool {
	err := syscall.Kill(-t.proc.Process.Pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
