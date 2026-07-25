// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package isolation

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// aceCount reports how many access-allowed entries acl carries.
// golang.org/x/sys/windows v0.47.0 has no GetAclInformation wrapper, but it
// doesn't need one: windows.ACL already exports AceCount as a plain struct
// field — the same field the module's own syscall_windows_test.go
// (getEntriesFromACL/TestGetACEsFromACL) reads directly, with no
// GetAclInformation call anywhere in that suite.
func aceCount(t *testing.T, acl *windows.ACL) int {
	t.Helper()
	return int(acl.AceCount)
}

// hasSID reports whether acl grants anything to sid.
func hasSID(t *testing.T, acl *windows.ACL, sid *windows.SID) bool {
	t.Helper()
	for i := range uint32(acl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			t.Fatalf("GetAce(%d): %v", i, err)
		}
		if (*windows.SID)(unsafe.Pointer(&ace.SidStart)).Equals(sid) {
			return true
		}
	}
	return false
}

// TestAdminOnlyDACL_ExcludesUnprivilegedTrustees proves the credential-file
// ACL admits nobody but SYSTEM and Administrators.
//
// This matters more than it looks. Machine-scope DPAPI is decryptable by
// anything on the host that can READ the file, so this ACL — not the
// encryption — is what stops a task from recovering the password of the
// account it runs as. An extra trustee here is a full credential disclosure.
func TestAdminOnlyDACL_ExcludesUnprivilegedTrustees(t *testing.T) {
	acl, err := adminOnlyDACL()
	if err != nil {
		t.Fatalf("adminOnlyDACL: %v", err)
	}

	if got := aceCount(t, acl); got != 2 {
		t.Errorf("adminOnlyDACL granted %d ACEs, want exactly 2 (SYSTEM, Administrators)", got)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatalf("CreateWellKnownSid(system): %v", err)
	}
	if !hasSID(t, acl, system) {
		t.Error("adminOnlyDACL must grant SYSTEM, or the worker service cannot read the credential it needs")
	}
	guests, err := windows.CreateWellKnownSid(windows.WinBuiltinGuestsSid)
	if err != nil {
		t.Fatalf("CreateWellKnownSid(guests): %v", err)
	}
	if hasSID(t, acl, guests) {
		t.Error("adminOnlyDACL must not grant an unprivileged trustee")
	}
}

// TestSecureDACL_GrantsExactlyThreeTrustees proves a session directory's ACL
// admits the target account, SYSTEM, and Administrators — and nobody else.
// No Users, no Everyone, no CREATOR OWNER. This is the Windows equivalent of
// POSIX chmod 0700, asserted structurally here so the expensive SYSTEM-tier
// tests only have to confirm that the structure has the effect it claims.
func TestSecureDACL_GrantsExactlyThreeTrustees(t *testing.T) {
	target, err := windows.CreateWellKnownSid(windows.WinBuiltinGuestsSid)
	if err != nil {
		t.Fatalf("CreateWellKnownSid: %v", err)
	}

	acl, err := secureDACL(target)
	if err != nil {
		t.Fatalf("secureDACL: %v", err)
	}

	if got := aceCount(t, acl); got != 3 {
		t.Errorf("secureDACL granted %d ACEs, want exactly 3 (target, SYSTEM, Administrators)", got)
	}
	if !hasSID(t, acl, target) {
		t.Error("secureDACL must grant the target account, or its own tasks cannot use the directory")
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatalf("CreateWellKnownSid(system): %v", err)
	}
	if !hasSID(t, acl, system) {
		t.Error("secureDACL must grant SYSTEM, or the daemon cannot clean the session up")
	}
}
