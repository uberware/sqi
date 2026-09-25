// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package winsvc

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// These tests cover only what runs unelevated against no real service: the
// pure helpers and the log-directory ACL logic on temp directories the test
// owns. Install/Start/Stop/Status/Uninstall, GrantServiceLogonRight and
// ValidateCredentials are exercised against the real SCM by the winservice
// suite (test/winservice), which needs an elevated shell.

func TestStateString(t *testing.T) {
	for st, want := range map[svc.State]string{
		svc.Stopped: "stopped", svc.StartPending: "start pending", svc.StopPending: "stop pending",
		svc.Running: "running", svc.Paused: "paused", svc.State(99): "unknown (99)",
	} {
		if got := stateString(st); got != want {
			t.Errorf("stateString(%d) = %q, want %q", st, got, want)
		}
	}
}

func TestStartTypeString(t *testing.T) {
	if got := startTypeString(mgr.StartAutomatic, true); got != "automatic (delayed)" {
		t.Errorf("got %q", got)
	}
	if got := startTypeString(mgr.StartManual, false); got != "manual" {
		t.Errorf("got %q", got)
	}
}

func TestLookupName(t *testing.T) {
	host := computerName(t)
	if got := lookupName(`.\render`); got != host+`\render` {
		t.Errorf(`lookupName(.\render) = %q`, got)
	}
	if got := lookupName(`STUDIO\render`); got != `STUDIO\render` {
		t.Errorf("got %q", got)
	}
}

func TestSplitAccount(t *testing.T) {
	for in, want := range map[string][2]string{
		`.\render`:          {".", "render"},
		`STUDIO\render`:     {"STUDIO", "render"},
		"render@studio.lan": {"", "render@studio.lan"},
	} {
		d, u := splitAccount(in)
		if d != want[0] || u != want[1] {
			t.Errorf("splitAccount(%q) = %q,%q want %q", in, d, u, want)
		}
	}
}

func TestRequireElevated_MatchesProcessToken(t *testing.T) {
	err := RequireElevated()
	if windows.GetCurrentProcessToken().IsElevated() {
		if err != nil {
			t.Fatalf("RequireElevated in an elevated process = %v, want nil", err)
		}
		return
	}
	if !errors.Is(err, ErrNotElevated) {
		t.Fatalf("RequireElevated in an unelevated process = %v, want ErrNotElevated", err)
	}
}

// TestEnsureLogDir_GrantsAccountOnExistingProtectedDir pins plan
// clarification 4: a LocalSystem install creates the protected directory, and
// a later --user install must add its account to it without widening it.
func TestEnsureLogDir_GrantsAccountOnExistingProtectedDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	if err := EnsureLogDir(dir, ""); err != nil { // the earlier LocalSystem install
		t.Fatal(err)
	}
	if _, protected := daclOf(t, dir); !protected {
		t.Fatal("a new log directory's DACL is not protected")
	}
	me, account := currentAccount(t)
	if err := EnsureLogDir(dir, account); err != nil {
		t.Fatal(err)
	}

	acl, protected := daclOf(t, dir)
	if !protected {
		t.Error("the grant unprotected the DACL; the directory would inherit ProgramData's Users access")
	}
	entries := aclEntries(t, acl)
	mine := allowedFor(entries, me)
	if len(mine) != 1 {
		t.Fatalf("want exactly one ACE for %s, got %d: %+v", account, len(mine), entries)
	}
	if mine[0].mask != fileModifyAccess {
		t.Errorf("account ACE mask = %#x, want Modify %#x (not full control)", mine[0].mask, fileModifyAccess)
	}
	if !inheritable(mine[0]) {
		t.Errorf("account ACE flags = %#x, want object+container inherit and not inherit-only", mine[0].flags)
	}
	for _, wk := range []windows.WELL_KNOWN_SID_TYPE{windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid} {
		assertFullControl(t, entries, wellKnownSID(t, wk))
	}
	if users := allowedFor(entries, wellKnownSID(t, windows.WinBuiltinUsersSid)); len(users) != 0 {
		t.Errorf("BUILTIN\\Users was granted access: %+v", users)
	}
}

// TestEnsureLogDir_CreatesProtectedDirForAccount pins spec §2 for a new
// directory: a protected DACL with full control for SYSTEM, Administrators
// and the service account, and nobody else.
func TestEnsureLogDir_CreatesProtectedDirForAccount(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sqi", "logs")
	me, account := currentAccount(t)
	if err := EnsureLogDir(dir, account); err != nil {
		t.Fatal(err)
	}
	acl, protected := daclOf(t, dir)
	if !protected {
		t.Error("a new log directory's DACL is not protected")
	}
	assertOnlyFullControl(t, aclEntries(t, acl), me)
}

// TestEnsureLogDir_KeepsUnprotectedDirUnprotected pins the other half of
// "the existing DACL, including its protection state, is kept".
func TestEnsureLogDir_KeepsUnprotectedDirUnprotected(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, protected := daclOf(t, dir); protected {
		t.Fatal("precondition: a plain new directory should inherit (unprotected DACL)")
	}
	me, account := currentAccount(t)
	if err := EnsureLogDir(dir, account); err != nil {
		t.Fatal(err)
	}
	acl, protected := daclOf(t, dir)
	if protected {
		t.Error("the grant protected a DACL that was inheriting")
	}
	var explicit []aceInfo
	for _, e := range allowedFor(aclEntries(t, acl), me) {
		if e.flags&windows.INHERITED_ACE == 0 {
			explicit = append(explicit, e)
		}
	}
	if len(explicit) != 1 || explicit[0].mask&fileModifyAccess != fileModifyAccess || !inheritable(explicit[0]) {
		t.Errorf("want one explicit inheritable ACE of at least Modify for %s, got %+v", account, explicit)
	}
}

// --- DACL inspection helpers. SDDL is deliberately not used: it renders
// well-known SIDs as aliases (the built-in Administrator account as LA), so a
// substring search for a SID string fails on a runner whose account is RID 500.

type aceInfo struct {
	sid   *windows.SID
	typ   uint8
	flags uint8
	mask  windows.ACCESS_MASK
}

func daclOf(t *testing.T, dir string) (acl *windows.ACL, protected bool) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("read DACL of %s: %v", dir, err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatalf("read control of %s: %v", dir, err)
	}
	acl, _, err = sd.DACL()
	if err != nil {
		t.Fatalf("read DACL of %s: %v", dir, err)
	}
	if acl == nil {
		t.Fatalf("%s has a NULL DACL (everyone has full control)", dir)
	}
	return acl, control&windows.SE_DACL_PROTECTED != 0
}

func aclEntries(t *testing.T, acl *windows.ACL) []aceInfo {
	t.Helper()
	out := make([]aceInfo, 0, acl.AceCount)
	for i := range uint32(acl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			t.Fatalf("GetAce(%d): %v", i, err)
		}
		out = append(out, aceInfo{
			sid:   (*windows.SID)(unsafe.Pointer(&ace.SidStart)),
			typ:   ace.Header.AceType,
			flags: ace.Header.AceFlags,
			mask:  ace.Mask,
		})
	}
	return out
}

// allowedFor returns the access-allowed entries for sid.
func allowedFor(entries []aceInfo, sid *windows.SID) []aceInfo {
	var out []aceInfo
	for _, e := range entries {
		if e.typ == windows.ACCESS_ALLOWED_ACE_TYPE && windows.EqualSid(e.sid, sid) {
			out = append(out, e)
		}
	}
	return out
}

func inheritable(e aceInfo) bool {
	const both = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
	return e.flags&both == both && e.flags&windows.INHERIT_ONLY_ACE == 0
}

func assertFullControl(t *testing.T, entries []aceInfo, sid *windows.SID) {
	t.Helper()
	got := allowedFor(entries, sid)
	if len(got) != 1 || got[0].mask != fileAllAccess || !inheritable(got[0]) {
		t.Errorf("want one inheritable full-control ACE for %s, got %+v", sid, got)
	}
}

// assertOnlyFullControl checks entries grant inheritable full control to
// SYSTEM, Administrators and account, and to no other trustee.
func assertOnlyFullControl(t *testing.T, entries []aceInfo, account *windows.SID) {
	t.Helper()
	want := []*windows.SID{
		wellKnownSID(t, windows.WinLocalSystemSid),
		wellKnownSID(t, windows.WinBuiltinAdministratorsSid),
		account,
	}
	for _, sid := range want {
		assertFullControl(t, entries, sid)
	}
	for _, e := range entries {
		known := false
		for _, sid := range want {
			known = known || windows.EqualSid(e.sid, sid)
		}
		if !known {
			t.Errorf("unexpected ACE for %s: %+v", e.sid, e)
		}
	}
	if users := allowedFor(entries, wellKnownSID(t, windows.WinBuiltinUsersSid)); len(users) != 0 {
		t.Errorf("BUILTIN\\Users was granted access: %+v", users)
	}
}

func wellKnownSID(t *testing.T, wk windows.WELL_KNOWN_SID_TYPE) *windows.SID {
	t.Helper()
	sid, err := windows.CreateWellKnownSid(wk)
	if err != nil {
		t.Fatalf("CreateWellKnownSid(%d): %v", wk, err)
	}
	return sid
}

// currentAccount returns the process user's SID and DOMAIN\name. Resolving the
// name only reads the account database.
func currentAccount(t *testing.T) (*windows.SID, string) {
	t.Helper()
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	name, domain, _, err := tu.User.Sid.LookupAccount("")
	if err != nil {
		t.Fatal(err)
	}
	return tu.User.Sid, domain + `\` + name
}

func computerName(t *testing.T) string {
	t.Helper()
	n, err := windowsComputerName()
	if err != nil {
		t.Fatal(err)
	}
	return n
}
