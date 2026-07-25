// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package executor

import (
	"os"
	"os/exec"
	"syscall"
)

// configureProcessGroup places the command in its own process group so that the
// whole group — the task's process and every child it spawns — can be signaled
// together on timeout or cancellation. Without this, sendKILL/sendTERM reach
// only the direct child; a shell's grandchildren (e.g. a `sleep`) survive,
// keep the inherited stdout/stderr pipes open, and stall killAndWait's
// cmd.Wait() until they exit on their own.
func configureProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// sendTERM sends SIGTERM to the task's process group, requesting graceful
// termination of the task and every child it spawned. Returns nil on success
// or an error if the signal could not be delivered (e.g., the process has
// already exited).
func (t *processTree) sendTERM() error {
	return signalGroup(t.proc.Process, syscall.SIGTERM)
}

// sendKILL sends SIGKILL to the task's process group, forcibly terminating the
// task and every child it spawned. Returns nil on success or an error if the
// signal could not be delivered.
func (t *processTree) sendKILL() error {
	return signalGroup(t.proc.Process, syscall.SIGKILL)
}

// signalGroup delivers sig to the process group led by proc. The task is
// started as its own group leader (see configureProcessGroup), so its group id
// equals its pid and a negative-pid kill reaches the leader plus every
// descendant — killing, for example, a shell's orphaned `sleep` child that
// would otherwise keep the stdout/stderr pipes open and stall cmd.Wait().
//
// If the group kill fails (ESRCH — e.g. the group already fully exited, or the
// process was somehow not made a group leader), fall back to signaling the
// single process so termination still degrades safely to the old behavior and
// never targets an unrelated group.
func signalGroup(proc *os.Process, sig syscall.Signal) error {
	if err := syscall.Kill(-proc.Pid, sig); err == nil {
		return nil
	}
	return proc.Signal(sig)
}
