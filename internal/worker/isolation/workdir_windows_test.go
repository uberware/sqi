// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package isolation

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// currentUsername resolves the bare account name of the process this test is
// running under, straight from the process token — exactly how lookupUserSID
// expects to be asked: LookupSID with an empty system string searches the
// local account database for an unqualified name, so a domain-qualified
// result would not round-trip. Building a *Credential naming this account
// (rather than nil, or an account the process can't touch) is what lets
// WriteFileFchown's ACL path genuinely execute on an ordinary developer
// machine: lookupUserSID can resolve any real local account, and the process
// account always qualifies.
func currentUsername(t *testing.T) string {
	t.Helper()
	tok := windows.GetCurrentProcessToken()
	tu, err := tok.GetTokenUser()
	if err != nil {
		t.Fatalf("GetTokenUser: %v", err)
	}
	account, _, _, err := tu.User.Sid.LookupAccount("")
	if err != nil {
		t.Fatalf("LookupAccount: %v", err)
	}
	return account
}

// TestSecureWorkDir_NilCredentialIsNoOp proves the pre-isolation path (no
// queue-configured run-as-user, so resolveCredential never calls Resolve) is
// completely unaffected: SecureWorkDir must stay a no-op for a nil credential
// exactly as it always has.
func TestSecureWorkDir_NilCredentialIsNoOp(t *testing.T) {
	if err := SecureWorkDir(t.TempDir(), nil); err != nil {
		t.Errorf("SecureWorkDir(nil) = %v, want nil", err)
	}
}

// TestChownRecursive_NilCredentialIsNoOp mirrors the above for ChownRecursive.
func TestChownRecursive_NilCredentialIsNoOp(t *testing.T) {
	if err := ChownRecursive(t.TempDir(), nil); err != nil {
		t.Errorf("ChownRecursive(nil) = %v, want nil", err)
	}
}

// TestSecureWorkDir_UnknownAccountFails proves a credential naming an account
// Windows cannot resolve produces an error rather than silently leaving the
// directory with its inherited ACL. Silent success here would be the worst
// possible outcome: the session would look isolated and be world-readable.
func TestSecureWorkDir_UnknownAccountFails(t *testing.T) {
	cred := &Credential{User: "sqi-no-such-account-9f3a"}

	err := SecureWorkDir(t.TempDir(), cred)

	if err == nil {
		t.Fatal("SecureWorkDir with an unresolvable account = nil, want an error")
	}
}

// TestValidateTraversable_NoOpWithReason proves ValidateTraversable does not
// block on Windows. Unlike the previous revision, this is no longer because
// the feature is unimplemented: Windows grants Bypass traverse checking
// (SeChangeNotifyPrivilege) to Everyone by default, which skips ancestor
// checks entirely, so there is no POSIX-style traversable bit for a
// path-based check to inspect. The equivalent guarantee is verified against
// the TARGET'S TOKEN in Resolve instead.
func TestValidateTraversable_NoOpWithReason(t *testing.T) {
	if err := ValidateTraversable(`C:\some\narrow\path`, `C:\another\one`); err != nil {
		t.Errorf("ValidateTraversable = %v, want nil", err)
	}
}

// TestWriteFileFchown_RefusesToWriteThroughAPreexistingEntry proves the
// remove-then-O_EXCL-create shape survives the ACL work: a second legitimate
// write of the same embedded-file name succeeds (OpenJD last-wins), and the
// file is replaced rather than written through.
func TestWriteFileFchown_RefusesToWriteThroughAPreexistingEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run")
	if err := WriteFileFchown(path, []byte("first"), 0o600, nil); err != nil {
		t.Fatalf("first write: %v", err)
	}

	if err := WriteFileFchown(path, []byte("second"), 0o600, nil); err != nil {
		t.Fatalf("second write: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "second" {
		t.Errorf("content = %q, want %q (last-wins)", got, "second")
	}
}

// assertSecured fails the test unless path carries EXACTLY the DACL
// secureDACL builds: protected (no inherited ACEs survive), and exactly the
// three trustees (target account, SYSTEM, Administrators) — the same
// structural check TestSecureDACL_GrantsExactlyThreeTrustees makes against
// the ACL in isolation, applied here to what actually landed on disk. It
// reads the security descriptor straight from the filesystem via
// GetNamedSecurityInfo, independently of the openForACL/applyProtectedDACL
// primitives production code used to write it, so a bug in those primitives
// can't hide from this assertion the way it could if the test reused the
// same code path to verify itself.
//
// Checking the PROTECTED control bit (not just "sid is present somewhere in
// the DACL") is what makes this distinguish "the code under test actually
// secured this entry" from "this entry merely inherited an ACL that happens
// to already grant sid" — the latter is a real risk whenever cred names the
// account the test itself runs as: that account also created path's parent
// directory and, by ordinary Windows inheritance, may already appear on its
// ACL (e.g. via a CREATOR-OWNER-derived ACE) with no help from the code under
// test at all. A fresh file's inherited ACL is never PROTECTED, so that bit
// alone would already fail before either fix lands a hand on the entry.
func assertSecured(t *testing.T, path string, sid *windows.SID) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo(%q): %v", path, err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatalf("Control(%q): %v", path, err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Errorf("%q DACL is not PROTECTED; it still carries whatever it inherited instead of the applied session ACL", path)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("DACL(%q): %v", path, err)
	}
	if got := aceCount(t, acl); got != 3 {
		t.Errorf("%q ACL has %d ACEs, want exactly 3 (target, SYSTEM, Administrators)", path, got)
	}
	if !hasSID(t, acl, sid) {
		t.Errorf("%q ACL does not grant the target account", path)
	}
}

// TestWriteFileFchown_AppliesACLForRealCredential proves the ACL apply step
// actually succeeds when it is invoked for real, not merely that
// WriteFileFchown returns nil.
//
// Before the fix, the create went through os.OpenFile, which on Windows only
// ever requests GENERIC_WRITE; that carries READ_CONTROL but not WRITE_DAC,
// and Windows fixes a handle's access rights at open time, so
// applyProtectedDACL failed ERROR_ACCESS_DENIED on the still-open descriptor
// every single time cred was non-nil — WriteFileFchown returned that error
// instead of nil. Both pre-existing WriteFileFchown tests above pass nil,
// which is exactly why this went unnoticed. This test uses a credential
// naming the account the test process is already running as: lookupUserSID
// resolves any real local account, and the process account always qualifies,
// so the ACL path genuinely executes on an ordinary developer machine with no
// fixture account required.
func TestWriteFileFchown_AppliesACLForRealCredential(t *testing.T) {
	user := currentUsername(t)
	cred := &Credential{User: user}
	path := filepath.Join(t.TempDir(), "run")

	if err := WriteFileFchown(path, []byte("payload"), 0o600, cred); err != nil {
		t.Fatalf("WriteFileFchown with a real credential: %v", err)
	}

	sid, err := lookupUserSID(user)
	if err != nil {
		t.Fatalf("lookupUserSID(%q): %v", user, err)
	}
	assertSecured(t, path, sid)
}

// TestChownRecursive_SecuresSiblingsAfterAReparsePoint proves every ordinary
// entry in root still gets ChownRecursive's DACL even when a reparse point
// sorts lexically before it.
//
// The reviewer's own reproduction used a real NTFS symlink
// (IO_REPARSE_TAG_SYMLINK) — what production code actually encounters — but
// creating one needs SeCreateSymbolicLinkPrivilege (or Developer Mode), and
// this session, which must not attempt to elevate, holds neither: verified
// directly rather than assumed — both os.Symlink and `mklink` (no /j) fail
// here with "A required privilege is not held by the client", and the
// Developer Mode registry key
// (HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\AppModelUnlock\
// AllowDevelopmentWithoutDevLicense) does not exist on this host at all. So
// this test cannot drive the defect through an actual symlink.
//
// A directory JUNCTION (IO_REPARSE_TAG_MOUNT_POINT, `mklink /j`) needs no
// privilege at all — but is exactly what the already-committed
// TestIsolationWindows_ChownRecursiveDoesNotFollowJunction
// (test/integration/isolation_windows_test.go) already uses, and Go 1.23+
// reports a junction as fs.ModeIrregular, not fs.ModeSymlink
// (os/types_windows.go's isReparseTagNameSurrogate/mode split from the
// pre-1.23 behavior): an unadorned junction never reaches the
// ModeSymlink-gated branch the defect lived in, so a plain junction test
// would pass against the buggy code exactly as it does against the fixed
// code, proving nothing — precisely how this shipped uncaught.
//
// GODEBUG=winsymlink=0 restores the pre-Go-1.23 compatibility mode, in which
// a junction DirEntry reports fs.ModeSymlink exactly like a real symlink does
// by default (checked directly against os.ReadDir before writing this test).
// That is the SAME DirEntry.Type() bit the buggy branch keyed off, so setting
// it drives the actual conditional the defect lived in, through the real
// ChownRecursive function and a real filepath.WalkDir, using only a
// filesystem object any unprivileged account can create — the closest this
// session can get to the reviewer's repro without elevating.
func TestChownRecursive_SecuresSiblingsAfterAReparsePoint(t *testing.T) {
	t.Setenv("GODEBUG", "winsymlink=0")

	root := t.TempDir()
	target := filepath.Join(root, "link_target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", target, err)
	}
	// Named to sort lexically BEFORE the ordinary entry below: the bug's
	// early loop break (see ChownRecursive's own doc) abandons every sibling
	// sorting AFTER the reparse point, so the reparse point must come first.
	reparse := filepath.Join(root, "aaa_reparse")
	if out, err := exec.CommandContext(context.Background(), "cmd", "/c", "mklink", "/j", reparse, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /j %q %q: %v: %s", reparse, target, err, out)
	}
	after := filepath.Join(root, "zzz_after")
	if err := os.WriteFile(after, []byte("ordinary"), 0o600); err != nil {
		t.Fatalf("write %q: %v", after, err)
	}

	user := currentUsername(t)
	cred := &Credential{User: user}

	if err := ChownRecursive(root, cred); err != nil {
		t.Fatalf("ChownRecursive: %v", err)
	}

	sid, err := lookupUserSID(user)
	if err != nil {
		t.Fatalf("lookupUserSID(%q): %v", user, err)
	}
	assertSecured(t, after, sid)
}
