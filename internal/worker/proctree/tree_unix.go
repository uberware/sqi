// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package proctree

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// Tree is the POSIX counterpart of the Windows job object. Configure's
// Setpgid establishes containment before Start, so there is nothing to
// create here; the type gives callers one shape on both platforms.
type Tree struct{ proc *exec.Cmd }

// Configure places the command in its own process group so that the
// whole group (the task's process and every child it spawns) can be signaled
// together on timeout or cancellation. Without this, Kill/Notify reach only
// the direct child; a shell's grandchildren (for example a `sleep`) survive,
// keep the inherited stdout/stderr pipes open, and stall the caller's
// cmd.Wait() until they exit on their own.
func Configure(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// Supervise records cmd. The process group was configured before Start.
func Supervise(cmd *exec.Cmd) (*Tree, error) {
	return &Tree{proc: cmd}, nil
}

// Release is a no-op: processes still running in the task's process group
// when the task exits successfully are left running. internal/worker/staging's
// stage-out checks (openStageOutSource) allow for such a process. The Windows
// Release behaves the same.
func (*Tree) Release() error { return nil }

// Alive reports whether any process of the task's process group is still
// running. A group whose processes belong to another account (run-as-user)
// answers EPERM, which still means it has members.
func (t *Tree) Alive() bool {
	err := syscall.Kill(-t.proc.Process.Pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Notify sends SIGTERM to the task's process group, requesting graceful
// termination of the task and every child it spawned. It returns an error if
// the signal could not be delivered (for example, the process has already
// exited).
func (t *Tree) Notify() error {
	return signalGroup(t.proc.Process, syscall.SIGTERM)
}

// Kill sends SIGKILL to the task's process group, forcibly terminating the
// task and every child it spawned. It returns an error if the signal could not
// be delivered.
func (t *Tree) Kill() error {
	return signalGroup(t.proc.Process, syscall.SIGKILL)
}

// signalGroup delivers sig to the process group led by proc. The task is
// started as its own group leader (see Configure), so its group id
// equals its pid and a negative-pid kill reaches the leader plus every
// descendant, including a shell's orphaned `sleep` child that would otherwise
// keep the stdout/stderr pipes open and stall cmd.Wait().
//
// If the group kill fails (ESRCH: the group has already exited, or the process
// was not made a group leader), it falls back to signaling the single process,
// so it never targets an unrelated group.
func signalGroup(proc *os.Process, sig syscall.Signal) error {
	if err := syscall.Kill(-proc.Pid, sig); err == nil {
		return nil
	}
	return proc.Signal(sig)
}
