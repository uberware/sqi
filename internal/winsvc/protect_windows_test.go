// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package winsvc

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// requireElevated skips a test of PrepareConfigDir or PrepareDataDir in an
// unelevated process: both make BUILTIN\Administrators the owner of what they
// create, which only an elevated token may assign, and their callers are
// elevated.
func requireElevated(t *testing.T) {
	t.Helper()
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("needs an elevated process: an unelevated one cannot make Administrators an owner")
	}
}

// ownerOf reads path's owner through a handle to path itself.
func ownerOf(t *testing.T, path string) *windows.SID {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("read owner of %s: %v", path, err)
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		t.Fatalf("owner of %s: %v (owner %v)", path, err, owner)
	}
	return owner
}

// defaultOwner is the owner the process token gives what it creates when no
// owner is specified (TokenOwner).
func defaultOwner(t *testing.T) *windows.SID {
	t.Helper()
	token := windows.GetCurrentProcessToken()
	var n uint32
	// The first call only reports the size needed.
	windows.GetTokenInformation(token, windows.TokenOwner, nil, 0, &n) //nolint:errcheck // ERROR_INSUFFICIENT_BUFFER is expected
	if n == 0 {
		t.Fatal("GetTokenInformation(TokenOwner) reported no size")
	}
	buf := make([]byte, n)
	if err := windows.GetTokenInformation(token, windows.TokenOwner, &buf[0], n, &n); err != nil {
		t.Fatalf("GetTokenInformation(TokenOwner): %v", err)
	}
	// TOKEN_OWNER (winnt.h) is one SID pointer, into buf.
	owner, err := (*struct{ Owner *windows.SID })(unsafe.Pointer(&buf[0])).Owner.Copy()
	if err != nil {
		t.Fatal(err)
	}
	return owner
}

func assertOwner(t *testing.T, path string, want *windows.SID) {
	t.Helper()
	if got := ownerOf(t, path); !got.Equals(want) {
		t.Errorf("owner of %s = %s, want %s", path, accountName(got), accountName(want))
	}
}

// assertOnlyAdmins checks entries are exactly one inheritable full-control
// ACE each for SYSTEM and Administrators, plus the ACEs in extra.
func assertOnlyAdmins(t *testing.T, entries []aceInfo, extra ...aceInfo) {
	t.Helper()
	system := wellKnownSID(t, windows.WinLocalSystemSid)
	admins := wellKnownSID(t, windows.WinBuiltinAdministratorsSid)
	assertFullControl(t, entries, system)
	assertFullControl(t, entries, admins)
	for _, want := range extra {
		got := allowedFor(entries, want.sid)
		if len(got) != 1 || got[0].mask != want.mask || !inheritable(got[0]) {
			t.Errorf("want one inheritable ACE granting %#x to %s, got %+v", want.mask, accountName(want.sid), got)
		}
	}
	if want := 2 + len(extra); len(entries) != want {
		t.Errorf("DACL has %d entries, want %d: %+v", len(entries), want, entries)
	}
}

// TestPrepareConfigDir_CreatesProtected pins that a missing configuration
// directory, and a missing parent of it, are created owned by Administrators
// with a protected DACL of SYSTEM and Administrators only — whatever owner the
// creating shell's token would give by default (an elevated process started
// from Git Bash owns what it creates).
func TestPrepareConfigDir_CreatesProtected(t *testing.T) {
	requireElevated(t)
	dir := filepath.Join(t.TempDir(), "parent", "sqi")
	if err := prepareConfigDir(dir, nil, testTrusted(t)); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Dir(dir), dir} {
		acl, protected := daclOf(t, p)
		if !protected {
			t.Errorf("%s has an unprotected DACL", p)
		}
		assertOnlyAdmins(t, aclEntries(t, acl))
		assertOwner(t, p, wellKnownSID(t, windows.WinBuiltinAdministratorsSid))
	}
	if err := checkOne(t, dir); err != nil {
		t.Errorf("the check refuses the directory PrepareConfigDir created: %v", err)
	}
}

// TestPrepareConfigDir_GrantsAccountRead pins the --user grant: a created
// directory carries one extra inheritable read-and-execute ACE for the
// account; an existing directory that passes the check gets that ACE added
// and keeps everything else.
func TestPrepareConfigDir_GrantsAccountRead(t *testing.T) {
	requireElevated(t)
	me, _ := currentAccount(t)
	rx := aceInfo{sid: me, mask: readExecuteAccess}

	t.Run("created", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "sqi-render")
		if err := prepareConfigDir(dir, me, testTrusted(t)); err != nil {
			t.Fatal(err)
		}
		acl, protected := daclOf(t, dir)
		if !protected {
			t.Error("an unprotected DACL")
		}
		assertOnlyAdmins(t, aclEntries(t, acl), rx)
	})

	t.Run("existing", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "sqi-render")
		if err := os.Mkdir(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		setSecurity(t, dir, "O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
		if err := prepareConfigDir(dir, me, testTrusted(t)); err != nil {
			t.Fatal(err)
		}
		acl, protected := daclOf(t, dir)
		if !protected {
			t.Error("the grant unprotected the DACL")
		}
		assertOnlyAdmins(t, aclEntries(t, acl), rx)
	})
}

// TestPrepareConfigDir_RefusesUntrustedExisting pins that an existing
// directory the check refuses is reported and left exactly as it was: no
// account ACE is added to a directory someone else controls.
func TestPrepareConfigDir_RefusesUntrustedExisting(t *testing.T) {
	requireElevated(t)
	dir := securedDir(t, "(A;OICI;"+sddlProgramDataUsers+";;;BU)")
	before := daclBytes(t, dir)
	beforeOwner := ownerOf(t, dir)
	reader := wellKnownSID(t, windows.WinNetworkServiceSid)
	err := prepareConfigDir(dir, reader, testTrusted(t))
	if _, ok := errors.AsType[*UntrustedPathsError](err); !ok {
		t.Fatalf("prepareConfigDir(<Users-writable dir>) = %v, want an *UntrustedPathsError", err)
	}
	if after := daclBytes(t, dir); after != before {
		t.Errorf("prepareConfigDir changed a refused directory's DACL:\nbefore %s\nafter  %s", before, after)
	}
	assertOwner(t, dir, beforeOwner)
}

// TestPrepareConfigDir_RemovesWhatItCreatedWhenCheckFails pins that a new
// directory whose ancestors fail the check is removed, not left for a later
// run to find and accept as existing.
func TestPrepareConfigDir_RemovesWhatItCreatedWhenCheckFails(t *testing.T) {
	requireElevated(t)
	root := t.TempDir()
	dir := filepath.Join(root, "parent", "sqi")
	// Without the current user trusted, the temp directory's ancestors (owned
	// by the current user) fail.
	err := prepareConfigDir(dir, nil, builtinTrusted(t))
	if _, ok := errors.AsType[*UntrustedPathsError](err); !ok {
		t.Fatalf("prepareConfigDir under an untrusted ancestor = %v, want an *UntrustedPathsError", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "parent")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the directories prepareConfigDir created were left behind (lstat: %v)", err)
	}
}

// TestPrepareConfigDir_RefusesExistingFile pins that a file where the
// directory should be is an error, not success.
func TestPrepareConfigDir_RefusesExistingFile(t *testing.T) {
	requireElevated(t)
	path := filepath.Join(securedDir(t, ""), "sqi")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareConfigDir(path, nil, testTrusted(t)); err == nil {
		t.Error("prepareConfigDir(<file>) = nil, want an error")
	}
}

func TestPrepareConfigDir_RefusesRelativePath(t *testing.T) {
	if err := PrepareConfigDir(`relative\sqi`, ""); err == nil {
		t.Error(`PrepareConfigDir("relative\sqi") = nil, want an error`)
	}
}

// TestPrepareDataDir_GrantsModify pins the --user data directory: created
// under the configuration directory owned by Administrators, inheriting its
// DACL, plus one explicit inheritable Modify ACE for the account; an existing
// one just gains the ACE.
func TestPrepareDataDir_GrantsModify(t *testing.T) {
	requireElevated(t)
	me, name := currentAccount(t)
	explicitModify := func(entries []aceInfo) int {
		n := 0
		for _, e := range allowedFor(entries, me) {
			if e.flags&windows.INHERITED_ACE == 0 && e.mask == fileModifyAccess && inheritable(e) {
				n++
			}
		}
		return n
	}
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "created", true: "existing"}[existing], func(t *testing.T) {
			parent := securedDir(t, "")
			dir := filepath.Join(parent, "worker")
			if existing {
				if err := os.Mkdir(dir, 0o750); err != nil {
					t.Fatal(err)
				}
			}
			if err := PrepareDataDir(dir, name); err != nil {
				t.Fatal(err)
			}
			acl, _ := daclOf(t, dir)
			entries := aclEntries(t, acl)
			if n := explicitModify(entries); n != 1 {
				t.Errorf("want one explicit inheritable Modify ACE for %s, got %d in %+v", name, n, entries)
			}
			if existing {
				return
			}
			assertOwner(t, dir, wellKnownSID(t, windows.WinBuiltinAdministratorsSid))
			assertFullControl(t, entries, wellKnownSID(t, windows.WinLocalSystemSid)) // inherited
		})
	}
}

// TestMkdirProtected_CreatesSqiParentProtected pins that a missing
// %ProgramData%\sqi, created as the default log directory's parent, does not
// inherit ProgramData's ACL, which lets every local user create files in it.
// A privileged process creates it owned by Administrators for SYSTEM and
// Administrators only; an unprivileged one (a hand-registered service running
// as an ordinary account) owns it and adds its own account, so it can then
// create logs. The unprivileged branch is taken through the isPrivileged seam,
// since an elevated test process cannot drop its elevation.
func TestMkdirProtected_CreatesSqiParentProtected(t *testing.T) {
	me, _ := currentAccount(t)
	admins := wellKnownSID(t, windows.WinBuiltinAdministratorsSid)
	orig := isPrivileged
	for _, forceUnprivileged := range []bool{false, true} {
		t.Run(map[bool]string{false: "actual", true: "unprivileged"}[forceUnprivileged], func(t *testing.T) {
			if forceUnprivileged {
				isPrivileged = func() bool { return false }
				t.Cleanup(func() { isPrivileged = orig })
			}
			root := t.TempDir()
			t.Setenv("ProgramData", root)
			parent := filepath.Join(root, "sqi")
			if _, err := mkdirProtected(filepath.Join(parent, "logs"), me); err != nil {
				t.Fatal(err)
			}
			acl, protected := daclOf(t, parent)
			if !protected {
				t.Errorf("%s has an unprotected DACL", parent)
			}
			entries := aclEntries(t, acl)
			// Unprivileged, the owner is the token's default: the process
			// account for an ordinary token, but Administrators for an
			// elevated one started from PowerShell, which the forced branch
			// may be running in.
			wantOwner := defaultOwner(t)
			if isPrivileged() {
				assertOnlyAdmins(t, entries)
				wantOwner = admins
			} else {
				assertOnlyAdmins(t, entries, aceInfo{sid: me, mask: fileAllAccess})
			}
			assertOwner(t, parent, wantOwner)
			assertOwner(t, filepath.Join(parent, "logs"), wantOwner)
		})
	}
}

// TestMkdirProtected_OtherParentsInherit pins that only %ProgramData%\sqi is
// created protected; another missing parent inherits as before.
func TestMkdirProtected_OtherParentsInherit(t *testing.T) {
	me, _ := currentAccount(t)
	t.Setenv("ProgramData", filepath.Join(t.TempDir(), "elsewhere"))
	parent := filepath.Join(t.TempDir(), "sqi")
	if _, err := mkdirProtected(filepath.Join(parent, "logs"), me); err != nil {
		t.Fatal(err)
	}
	if _, protected := daclOf(t, parent); protected {
		t.Errorf("%s, not %%ProgramData%%\\sqi, was created protected", parent)
	}
}

// TestEnsureSqiDir_CreatesMissingAdminOnly pins that service install creates a
// missing %ProgramData%\sqi as config init does: owned by Administrators with
// a protected DACL of SYSTEM and Administrators only, so the check that
// follows judges it rather than %ProgramData%, which every user can write.
func TestEnsureSqiDir_CreatesMissingAdminOnly(t *testing.T) {
	requireElevated(t)
	pd := t.TempDir()
	t.Setenv("ProgramData", pd)
	if err := EnsureSqiDir(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(pd, "sqi")
	acl, protected := daclOf(t, dir)
	if !protected {
		t.Errorf("%s has an unprotected DACL", dir)
	}
	assertOnlyAdmins(t, aclEntries(t, acl))
	assertOwner(t, dir, wellKnownSID(t, windows.WinBuiltinAdministratorsSid))
}

// TestEnsureSqiDir_LeavesExistingAlone pins that an existing %ProgramData%\sqi
// is not changed in any way: its owner, group, DACL and contents stay as they
// were, for the check to judge.
func TestEnsureSqiDir_LeavesExistingAlone(t *testing.T) {
	pd := t.TempDir()
	t.Setenv("ProgramData", pd)
	dir := filepath.Join(pd, "sqi")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := securityOf(t, dir)
	if err := EnsureSqiDir(); err != nil {
		t.Fatal(err)
	}
	if after := securityOf(t, dir); after != before {
		t.Errorf("security changed:\n before %s\n after  %s", before, after)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "keep.txt" {
		t.Errorf("contents changed: %v, %v", entries, err)
	}
}

// securityOf is path's owner, group and DACL as SDDL.
func securityOf(t *testing.T, path string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("read security of %s: %v", path, err)
	}
	return sd.String()
}
