// SPDX-License-Identifier: AGPL-3.0-or-later

package isolation

import (
	"fmt"
	"strings"
)

// privilegedNames are accounts that must never be a run-as-user target. Not
// configurable: an operator-overridable list would defeat the point, since the
// whole risk is a queue pointing at a more privileged account than the daemon.
//
// Matching goes through normalizeAccountName, which strips a qualifying
// "DOMAIN\" / "HOST\" / ".\" prefix, so a single "administrator" entry here
// also catches ".\Administrator", "DOMAIN\Administrator" and the like — the
// list below only needs the unqualified forms plus the built-in Windows
// service accounts, whose display names carry an interior space
// ("NETWORK SERVICE", "LOCAL SERVICE") that normalization preserves.
var privilegedNames = map[string]bool{
	"root": true, "administrator": true, "administrators": true,
	"system": true, "localsystem": true,
	"network service": true, "local service": true,
}

// privilegedGroupNames are groups that must never be an explicit run-as-user
// target, for the same reason privilegedNames exists: granting one is an
// escalation, not containment. docker in particular is a one-step root
// escalation on any host running the daemon (a member can bind-mount the host
// root into a container); disk is raw block-device access; shadow reads
// password hashes; staff is the macOS admin-equivalent group; administrators
// is its Windows counterpart. Not configurable, for the same reason
// privilegedNames isn't.
var privilegedGroupNames = map[string]bool{
	"root": true, "wheel": true, "admin": true, "sudo": true, "sudoers": true,
	"adm": true, "docker": true, "disk": true, "shadow": true, "staff": true,
	"administrators": true,
}

// normalizeAccountName folds an account or group name into a comparable
// form: strip a leading ".\" / "HOST\" / "DOMAIN\" qualifier (Windows
// principals are commonly written that way — ".\Administrator",
// "BUILTIN\Administrators", "NT AUTHORITY\SYSTEM"), strip a trailing
// "@domain" UPN suffix (LogonUserW requires this exact spelling when its
// lpszDomain argument is NULL — see logonUserOS — so "Administrator@corp.
// example.com" is not an obscure form to guard against, it is the form the
// real logon path is called with), collapse runs of interior whitespace to a
// single space (so "NETWORK  SERVICE" and "NETWORK SERVICE" compare equal
// without losing the space that "NETWORKSERVICE" would), and lowercase.
//
// Only one of the two qualifier forms is stripped per name — a principal is
// legitimately either "DOMAIN\user" or "user@domain", never both — so
// stripping the "\" prefix first and then any "@" suffix on what remains
// handles each correctly without a bare "administrator" happening to also
// contain "@".
func normalizeAccountName(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.LastIndex(name, `\`); i != -1 {
		name = name[i+1:]
	}
	if i := strings.IndexByte(name, '@'); i != -1 {
		name = name[:i]
	}
	name = strings.Join(strings.Fields(name), " ")
	return strings.ToLower(name)
}

// CheckNotPrivileged rejects privileged targets by name and by uid. uid is
// ignored on Windows, where callers pass 1.
func CheckNotPrivileged(name string, uid uint32) error {
	if privilegedNames[normalizeAccountName(name)] {
		return fmt.Errorf("%w: %q", ErrRefusedPrivileged, name)
	}
	if uid == 0 {
		return fmt.Errorf("%w: %q resolves to uid 0", ErrRefusedPrivileged, name)
	}
	return nil
}

// CheckGroupNotPrivileged rejects an explicitly requested group the same way
// CheckNotPrivileged rejects a privileged account: gid 0 and a fixed set of
// privileged group names are refused outright, regardless of what a queue
// operator configured.
func CheckGroupNotPrivileged(name string, gid uint32) error {
	if privilegedGroupNames[normalizeAccountName(name)] {
		return fmt.Errorf("%w: group %q", ErrRefusedPrivileged, name)
	}
	if gid == 0 {
		return fmt.Errorf("%w: group %q resolves to gid 0", ErrRefusedPrivileged, name)
	}
	return nil
}
