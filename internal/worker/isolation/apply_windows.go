// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package isolation

import (
	"errors"
	"os/exec"
	"syscall"
)

// errZeroToken is returned by apply when cred carries a zero token handle.
// Not reachable today — every path that produces a *Credential (logonUserOS,
// newFakeCredential) sets a real, non-zero token — but Credential.Close now
// zeroes the token field, so any future reordering of the session lifecycle
// (e.g. a credential closed before its last use, or reused after Close) makes
// this reachable. Without this guard, a zero Token makes os/exec's windows
// implementation silently take the plain CreateProcess branch instead of
// CreateProcessAsUser (syscall.StartProcess treats Token == 0 as "not set"),
// so the child would run as the DAEMON while apply reports success — exactly
// the silent-fallback-to-daemon-identity failure this package exists to
// prevent.
var errZeroToken = errors.New("isolation: credential carries a zero token; refusing to start an unisolated process")

// apply attaches the logon token to cmd, preserving any SysProcAttr fields
// the caller already set. os/exec's windows implementation switches to
// calling CreateProcessAsUser internally whenever SysProcAttr.Token is
// non-zero — that field is the entire integration point; no other Win32 call
// is needed here to make the child process run as cred's identity.
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
