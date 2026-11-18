// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package isolation

import (
	"os/exec"
	"syscall"
)

// apply attaches the POSIX credential, preserving any SysProcAttr the caller
// already configured. proctree.Configure sets Setpgid before this runs, and
// replacing the struct would drop it and break whole-tree kill.
func apply(cmd *exec.Cmd, cred *Credential) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Credential = cred.cred
	return nil
}
