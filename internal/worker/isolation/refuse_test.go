// SPDX-License-Identifier: AGPL-3.0-or-later

package isolation_test

import (
	"testing"

	"github.com/uberware/sqi/internal/worker/isolation"
)

func TestRefusesPrivilegedAccounts(t *testing.T) {
	for _, name := range []string{
		"root", "ROOT", "Administrator", "administrator", "SYSTEM", "system",
		"LocalSystem", "NT AUTHORITY\\SYSTEM",
	} {
		t.Run(name, func(t *testing.T) {
			if err := isolation.CheckNotPrivileged(name, 0); err == nil {
				t.Errorf("CheckNotPrivileged(%q) = nil, want refusal", name)
			}
		})
	}
}

func TestRefusesUIDZero(t *testing.T) {
	if err := isolation.CheckNotPrivileged("someuser", 0); err == nil {
		t.Error("uid 0 must be refused regardless of the account name")
	}
}

func TestAllowsOrdinaryAccount(t *testing.T) {
	if err := isolation.CheckNotPrivileged("render-svc", 1001); err != nil {
		t.Errorf("CheckNotPrivileged(render-svc, 1001) = %v, want nil", err)
	}
}

// TestRefusesWindowsPrincipalForms passes a nonzero uid so refusal can only
// come from the name match itself, not the uid-0 backstop — proving the
// normalization (prefix-stripping, whitespace-collapsing) actually catches
// these forms rather than piggybacking on a check that won't exist on
// Windows.
func TestRefusesWindowsPrincipalForms(t *testing.T) {
	for _, name := range []string{
		`.\Administrator`, `DOMAIN\Administrator`, `BUILTIN\Administrators`,
		`NT AUTHORITY\LocalSystem`, `NT AUTHORITY\SYSTEM`,
		"NETWORK SERVICE", "LOCAL SERVICE",
	} {
		t.Run(name, func(t *testing.T) {
			if err := isolation.CheckNotPrivileged(name, 1001); err == nil {
				t.Errorf("CheckNotPrivileged(%q, 1001) = nil, want refusal", name)
			}
		})
	}
}

func TestRefusesPrivilegedGroups(t *testing.T) {
	for _, name := range []string{
		"root", "ROOT", "wheel", "Wheel", "admin", "Admin", "sudo", "sudoers",
		"adm", "docker", "Docker", "disk", "shadow", "staff", "administrators",
		"Administrators",
	} {
		t.Run(name, func(t *testing.T) {
			if err := isolation.CheckGroupNotPrivileged(name, 999); err == nil {
				t.Errorf("CheckGroupNotPrivileged(%q, 999) = nil, want refusal", name)
			}
		})
	}
}

func TestRefusesGIDZero(t *testing.T) {
	if err := isolation.CheckGroupNotPrivileged("rendergroup", 0); err == nil {
		t.Error("gid 0 must be refused regardless of group name")
	}
}

func TestAllowsOrdinaryGroup(t *testing.T) {
	if err := isolation.CheckGroupNotPrivileged("render", 2001); err != nil {
		t.Errorf("CheckGroupNotPrivileged(render, 2001) = %v, want nil", err)
	}
}
