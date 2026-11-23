// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package isolation

import (
	"errors"
	"os/exec"
	"syscall"
)

// errZeroToken is returned by apply when cred carries a zero token handle.
// Every path that produces a *Credential (logonUserOS, newFakeCredential) sets
// a non-zero token, but Credential.Close zeroes it, so a credential used after
// Close reaches this. A zero Token makes os/exec's windows implementation take
// the plain CreateProcess branch instead of CreateProcessAsUser
// (syscall.StartProcess treats Token == 0 as "not set"), so without this
// guard the child would run as the worker while apply reports success.
var errZeroToken = errors.New("isolation: credential carries a zero token; refusing to start an unisolated process")

// apply attaches the logon token to cmd, preserving any SysProcAttr fields
// the caller already set. os/exec's windows implementation switches to
// calling CreateProcessAsUser internally whenever SysProcAttr.Token is
// non-zero; no other Win32 call is needed here to make the child process run
// as cred's identity.
func apply(cmd *exec.Cmd, cred *Credential) error {
	if cred.token == 0 {
		return errZeroToken
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Token = syscall.Token(cred.token)
	return nil
}
