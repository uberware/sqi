// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package isolation

import (
	"os/exec"
	"syscall"
)

// apply attaches the POSIX credential, preserving any SysProcAttr the caller
// already configured. configureProcessGroup sets Setpgid before this runs;
// replacing the struct would silently break whole-tree kill, and a privileged
// child that survives cancellation is the worst place for that bug.
func apply(cmd *exec.Cmd, cred *Credential) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Credential = cred.cred
	return nil
}
