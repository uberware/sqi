// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package isolation

import (
	"os/exec"
	"syscall"
)

// apply attaches the logon token to cmd, preserving any SysProcAttr fields
// the caller already set. os/exec's windows implementation switches to
// calling CreateProcessAsUser internally whenever SysProcAttr.Token is
// non-zero — that field is the entire integration point; no other Win32 call
// is needed here to make the child process run as cred's identity.
func apply(cmd *exec.Cmd, cred *Credential) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Token = syscall.Token(cred.token)
	return nil
}
