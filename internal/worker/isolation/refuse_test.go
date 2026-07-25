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

// TestRefusesUPNAccountForms proves the UPN spelling ("user@domain") is
// refused exactly like the "DOMAIN\user" spelling — LogonUserW requires the
// UPN form when logonUserOS calls it with a nil domain (see
// normalizeAccountName's doc), so a queue operator (or an attacker) writing
// "Administrator@corp.example.com" must be refused just as reliably as
// ".\Administrator" or bare "Administrator". Table-driven across UPN,
// "DOMAIN\", ".\", bare, and mixed-case spellings of the same account, all
// asserted with a nonzero uid so refusal can only come from the name match.
func TestRefusesUPNAccountForms(t *testing.T) {
	for _, name := range []string{
		"administrator",
		"Administrator",
		"ADMINISTRATOR",
		`.\Administrator`,
		`DOMAIN\Administrator`,
		"administrator@corp.example.com",
		"Administrator@corp.example.com",
		"ADMINISTRATOR@CORP.EXAMPLE.COM",
		"administrator@CORP.example.COM",
	} {
		t.Run(name, func(t *testing.T) {
			if err := isolation.CheckNotPrivileged(name, 1001); err == nil {
				t.Errorf("CheckNotPrivileged(%q, 1001) = nil, want refusal", name)
			}
		})
	}
}

// TestUPNFormDoesNotOverRefuse guards the flip side of
// TestRefusesUPNAccountForms: stripping an "@domain" suffix must not refuse
// an ordinary account merely because its name happens to be used at a
// domain — "render-svc@corp.example.com" must be treated exactly like
// "render-svc".
func TestUPNFormDoesNotOverRefuse(t *testing.T) {
	if err := isolation.CheckNotPrivileged("render-svc@corp.example.com", 1001); err != nil {
		t.Errorf("CheckNotPrivileged(render-svc@corp.example.com, 1001) = %v, want nil", err)
	}
}

// TestRefusesUPNGroupForms is TestRefusesUPNAccountForms's counterpart for
// CheckGroupNotPrivileged — the UPN suffix must be stripped on the group
// refusal path too, not just the account path.
func TestRefusesUPNGroupForms(t *testing.T) {
	for _, name := range []string{
		"admin",
		"Admin",
		"ADMIN",
		`.\admin`,
		`DOMAIN\admin`,
		"admin@corp.example.com",
		"Admin@corp.example.com",
		"ADMIN@CORP.EXAMPLE.COM",
	} {
		t.Run(name, func(t *testing.T) {
			if err := isolation.CheckGroupNotPrivileged(name, 999); err == nil {
				t.Errorf("CheckGroupNotPrivileged(%q, 999) = nil, want refusal", name)
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
