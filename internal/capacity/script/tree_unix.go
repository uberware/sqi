// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package script

import (
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
)

// startTree starts cmd in a process group of its own and makes its
// cancellation kill the whole group, so a program the script started (aws,
// gcloud) dies with it rather than finishing an action sqi has given up on.
// release frees what it holds once cmd has been waited for; on POSIX it holds
// nothing.
func startTree(cmd *exec.Cmd, _ *slog.Logger) (release func(), err error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return func() {}, cmd.Start()
}
