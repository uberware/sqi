// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package winsvc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/uberware/sqi/internal/fsutil/fsutiltest"
)

// These tests build every DACL themselves and trust the current user, so they
// pass unelevated: an unelevated process cannot make Administrators the owner
// of what it creates, and an elevated one usually does by default.

const (
	// sddlRX is Explorer's "Read & execute": FILE_GENERIC_READ|FILE_GENERIC_EXECUTE.
	sddlRX = "0x1200a9"
	// sddlProgramDataUsers is what a new C:\ProgramData folder grants
	// BUILTIN\Users: WD, AD, WEA and WA.
	sddlProgramDataUsers = "0x116"
	// sddlModify is Explorer's "Modify".
	sddlModify = "0x1301bf"
	// sddlWriteDAC is WRITE_DAC alone.
	sddlWriteDAC = "0x40000"
)

// testTrusted is the trusted set the tests pass to checkPaths: SYSTEM,
// Administrators, TrustedInstaller and the current user.
func testTrusted(t *testing.T) []*windows.SID {
	t.Helper()
	me, _ := currentAccount(t)
	return append(builtinTrusted(t), me)
}

// builtinTrusted is the trusted set without the current user.
func builtinTrusted(t *testing.T) []*windows.SID {
	t.Helper()
	ti, err := windows.StringToSid(trustedInstallerSID)
	if err != nil {
		t.Fatal(err)
	}
	return []*windows.SID{
		wellKnownSID(t, windows.WinLocalSystemSid),
		wellKnownSID(t, windows.WinBuiltinAdministratorsSid),
		ti,
	}
}

// trustedACEs is SDDL granting full control to every SID in testTrusted.
func trustedACEs(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, sid := range testTrusted(t) {
		fmt.Fprintf(&b, "(A;OICI;FA;;;%s)", sid)
	}
	return b.String()
}

// setSecurity gives path the protected DACL, and the owner when it names one,
// that sddl describes, through a handle to path itself.
func setSecurity(t *testing.T, path, sddl string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatalf("parse SDDL %q: %v", sddl, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("DACL of %q: %v", sddl, err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		t.Fatalf("owner of %q: %v", sddl, err)
	}
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	if owner != nil {
		info |= windows.OWNER_SECURITY_INFORMATION
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer windows.CloseHandle(h) //nolint:errcheck // test cleanup
	if err := windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT, info, owner, nil, dacl, nil); err != nil {
		t.Fatalf("set security of %s to %q: %v", path, sddl, err)
	}
}

// securedDir makes a new directory whose protected DACL is the trusted
// full-control grants plus extra (SDDL ACEs).
func securedDir(t *testing.T, extra string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "d")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	setSecurity(t, dir, "D:P"+trustedACEs(t)+extra)
	return dir
}

// checkOne runs checkPaths on one ref for path, trusting testTrusted.
func checkOne(t *testing.T, path string) error {
	t.Helper()
	return checkPaths([]PathRef{{Key: "config", Path: path}}, testTrusted(t))
}

// wantFailures asserts err is an *UntrustedPathsError and returns its
// failures.
func wantFailures(t *testing.T, err error) []PathFailure {
	t.Helper()
	var u *UntrustedPathsError
	if !errors.As(err, &u) {
		t.Fatalf("checkPaths = %v, want an *UntrustedPathsError", err)
	}
	return u.Failures
}

// wantOneFailure asserts err reports exactly one failure, judged at judged,
// whose reason contains every one of want.
func wantOneFailure(t *testing.T, err error, judged string, want ...string) PathFailure {
	t.Helper()
	failures := wantFailures(t, err)
	if len(failures) != 1 {
		t.Fatalf("checkPaths reported %d failures, want 1: %+v", len(failures), failures)
	}
	f := failures[0]
	if f.JudgedPath != judged {
		t.Errorf("JudgedPath = %q, want %q", f.JudgedPath, judged)
	}
	for _, w := range want {
		if !strings.Contains(f.Reason, w) {
			t.Errorf("Reason = %q, want it to contain %q", f.Reason, w)
		}
	}
	return f
}

// accountNameOf is a well-known account's DOMAIN\name, or its bare name when
// it has no domain (Everyone).
func accountNameOf(t *testing.T, wk windows.WELL_KNOWN_SID_TYPE) string {
	t.Helper()
	name, domain, _, err := wellKnownSID(t, wk).LookupAccount("")
	if err != nil {
		t.Fatal(err)
	}
	if domain == "" {
		return name
	}
	return domain + `\` + name
}

func TestCheckPaths_PassesAProtectedDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := protectNewDir(dir, testTrusted(t)); err != nil {
		t.Fatal(err)
	}
	if err := checkOne(t, dir); err != nil {
		t.Errorf("checkPaths(<protected dir>) = %v, want nil", err)
	}
}

// The shape of a new folder in C:\Program Files.
func TestCheckPaths_PassesReadOnlyUsers(t *testing.T) {
	dir := securedDir(t, "(A;OICI;"+sddlRX+";;;BU)")
	if err := checkOne(t, dir); err != nil {
		t.Errorf("checkPaths(<Users RX>) = %v, want nil", err)
	}
}

// C:\ProgramData's CREATOR OWNER grant applies only to children, which nobody
// untrusted can create here.
func TestCheckPaths_IgnoresInheritOnlyCreatorOwner(t *testing.T) {
	dir := securedDir(t, "(A;OICIIO;FA;;;CO)")
	if err := checkOne(t, dir); err != nil {
		t.Errorf("checkPaths(<CREATOR OWNER inherit-only>) = %v, want nil", err)
	}
}

// The shape of a new folder in C:\ProgramData: the container-inherit grant is
// not inherit-only, so it applies to the folder itself.
func TestCheckPaths_FailsUsersWrite(t *testing.T) {
	dir := securedDir(t, "(A;CI;"+sddlProgramDataUsers+";;;BU)")
	f := wantOneFailure(t, checkOne(t, dir), dir,
		accountNameOf(t, windows.WinBuiltinUsersSid)+" can write (WD,AD,WEA,WA)")
	if f.Key != "config" || f.Path != dir {
		t.Errorf("failure = %+v, want key config and path %s", f, dir)
	}
}

func TestCheckPaths_FailsAuthenticatedUsersModify(t *testing.T) {
	dir := securedDir(t, "(A;OICI;"+sddlModify+";;;AU)")
	wantOneFailure(t, checkOne(t, dir), dir,
		accountNameOf(t, windows.WinAuthenticatedUserSid)+" can write (WD,AD,WEA,WA,D)")
}

func TestCheckPaths_FailsEveryoneWriteDAC(t *testing.T) {
	dir := securedDir(t, "(A;;"+sddlWriteDAC+";;;WD)")
	wantOneFailure(t, checkOne(t, dir), dir,
		accountNameOf(t, windows.WinWorldSid)+" can write (WDAC)")
}

func TestCheckPaths_FailsNullDACL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	setSecurity(t, dir, "D:NO_ACCESS_CONTROL")
	wantOneFailure(t, checkOne(t, dir), dir, "NULL DACL")
}

func TestCheckPaths_FailsUntrustedOwner(t *testing.T) {
	me, myName := currentAccount(t)
	dir := filepath.Join(t.TempDir(), "d")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	// Owned by the current user explicitly: elevated, the default owner
	// would be Administrators. Only the trusted set and the owner itself are
	// granted anything.
	setSecurity(t, dir, fmt.Sprintf("O:%sD:P%s", me, trustedACEs(t)))
	// The current user also owns the temp directory's ancestors, which fail
	// too; only the failure judged at dir is this test's.
	err := checkPaths([]PathRef{{Key: "config", Path: dir}}, builtinTrusted(t))
	var found bool
	for _, f := range wantFailures(t, err) {
		if f.JudgedPath == dir {
			found = true
			if !strings.Contains(f.Reason, "owner "+myName+" is not trusted") {
				t.Errorf("Reason = %q, want it to name the untrusted owner %s", f.Reason, myName)
			}
		}
	}
	if !found {
		t.Errorf("checkPaths = %v, want a failure judged at %s", err, dir)
	}
}

func TestCheckPaths_RefusesJunction(t *testing.T) {
	target := securedDir(t, "")
	link := filepath.Join(t.TempDir(), "link")
	fsutiltest.Junction(t, link, target)
	wantOneFailure(t, checkOne(t, link), link, "is a junction or symbolic link")
}

func TestCheckPaths_RefusesSymlink(t *testing.T) {
	dir := securedDir(t, "")
	target := filepath.Join(dir, "f")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	fsutiltest.SymlinkOrSkip(t, link, target)
	wantOneFailure(t, checkOne(t, link), link, "is a junction or symbolic link")
}

// A missing path below a junction is judged by the junction, which is
// refused rather than followed.
func TestCheckPaths_RefusesJunctionAncestor(t *testing.T) {
	target := securedDir(t, "")
	link := filepath.Join(t.TempDir(), "link")
	fsutiltest.Junction(t, link, target)
	missing := filepath.Join(link, "a", "b.txt")
	wantOneFailure(t, checkOne(t, missing), link, "is a junction or symbolic link")
}

// An existing path reached through a junction is refused too: whoever can
// retarget the junction decides what the path names.
func TestCheckPaths_RefusesPathThroughJunction(t *testing.T) {
	target := securedDir(t, "")
	if err := os.WriteFile(filepath.Join(target, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	fsutiltest.Junction(t, link, target)
	path := filepath.Join(link, "f")
	wantOneFailure(t, checkOne(t, path), link, "ancestor "+link+": is a junction or symbolic link")
}

func TestCheckPaths_MissingPathJudgedByAncestor(t *testing.T) {
	protected := securedDir(t, "")
	missing := filepath.Join(protected, "a", "b", "c.txt")
	judged, reasons, err := judgePath(missing, testTrusted(t))
	if err != nil || len(reasons) != 0 || judged != protected {
		t.Errorf("judgePath(%s) = %q, %q, %v; want %q, no reasons, nil", missing, judged, reasons, err, protected)
	}
	if err := checkOne(t, missing); err != nil {
		t.Errorf("checkPaths(<missing under protected>) = %v, want nil", err)
	}

	writable := securedDir(t, "(A;CI;"+sddlProgramDataUsers+";;;BU)")
	wantOneFailure(t, checkOne(t, filepath.Join(writable, "x")), writable, "can write")
}

func TestCheckPaths_ReportsEveryFailure(t *testing.T) {
	protected := securedDir(t, "")
	writable := securedDir(t, "(A;CI;"+sddlProgramDataUsers+";;;BU)")
	everyone := securedDir(t, "(A;;"+sddlWriteDAC+";;;WD)")
	refs := []PathRef{
		{Key: "config", Path: protected},
		{Key: "worker.data_dir", Path: writable},
		{Key: "nats.tls_ca_file", Path: everyone},
	}
	failures := wantFailures(t, checkPaths(refs, testTrusted(t)))
	if len(failures) != 2 {
		t.Fatalf("got %d failures, want 2: %+v", len(failures), failures)
	}
	for i, want := range refs[1:] {
		if failures[i].Key != want.Key || failures[i].Path != want.Path {
			t.Errorf("failure %d = %+v, want key %s path %s", i, failures[i], want.Key, want.Path)
		}
	}
	msg := (&UntrustedPathsError{Failures: failures}).Error()
	lines := strings.Split(msg, "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "worker.data_dir "+writable+": ") ||
		!strings.HasPrefix(lines[1], "nats.tls_ca_file "+everyone+": ") {
		t.Errorf("Error() = %q, want one \"<key> <path>: <reason>\" line per failure", msg)
	}
}

func TestCheckPaths_File(t *testing.T) {
	dir := securedDir(t, "")
	good := filepath.Join(dir, "good.yaml")
	bad := filepath.Join(dir, "bad.yaml")
	for _, f := range []string{good, bad} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	setSecurity(t, good, "D:P"+trustedACEs(t)+"(A;;"+sddlRX+";;;BU)")
	setSecurity(t, bad, "D:P"+trustedACEs(t)+"(A;;"+sddlModify+";;;BU)")
	if err := checkOne(t, good); err != nil {
		t.Errorf("checkPaths(<read-only file>) = %v, want nil", err)
	}
	wantOneFailure(t, checkOne(t, bad), bad, accountNameOf(t, windows.WinBuiltinUsersSid)+" can write")
}

func TestCheckPaths_RefusesRelativePath(t *testing.T) {
	err := checkPaths([]PathRef{{Key: "config", Path: `sqi\worker.yaml`}}, testTrusted(t))
	var u *UntrustedPathsError
	if err == nil || errors.As(err, &u) {
		t.Errorf("checkPaths(<relative>) = %v, want an error that is not an *UntrustedPathsError", err)
	}
}

// CheckPaths trusts the named account; "" trusts only the built-in
// principals, so a directory the current user owns or can write fails.
func TestCheckPaths_TrustsAccount(t *testing.T) {
	_, me := currentAccount(t)
	dir := securedDir(t, "")
	refs := []PathRef{{Key: "config", Path: dir}}
	if err := CheckPaths(refs, me); err != nil {
		t.Errorf("CheckPaths(<protected>, %s) = %v, want nil", me, err)
	}
	wantFailures(t, CheckPaths(refs, ""))
}

// A group named as the account would trust every member of it.
func TestCheckPaths_RefusesGroupAccount(t *testing.T) {
	dir := securedDir(t, "(A;CI;"+sddlProgramDataUsers+";;;BU)")
	users := accountNameOf(t, windows.WinBuiltinUsersSid)
	err := CheckPaths([]PathRef{{Key: "config", Path: dir}}, users)
	var u *UntrustedPathsError
	if err == nil || errors.As(err, &u) || !strings.Contains(err.Error(), "not a user account") {
		t.Errorf("CheckPaths(_, %s) = %v, want a not-a-user-account error", users, err)
	}
}

// TestJudgeSecurity_ACERules pins the ACE rules on in-memory descriptors,
// including shapes that cannot safely be put on a test directory (a deny for
// Users would lock the test out of its own temp directory).
func TestJudgeSecurity_ACERules(t *testing.T) {
	me, _ := currentAccount(t)
	owner := "O:" + me.String()
	base := trustedACEs(t)
	tests := []struct {
		name string
		sddl string
		want string // "" = passes; otherwise a substring of the reasons
	}{
		{"deny never fails", "D:P" + base + "(D;OICI;FA;;;BU)", ""},
		{"read-only grant passes", "D:P" + base + "(A;;" + sddlRX + ";;;BU)", ""},
		{"inherit-only write is ignored", "D:P" + base + "(A;OICIIO;FA;;;BU)", ""},
		{"generic write fails", "D:P" + base + "(A;;GW;;;BU)", "(GW)"},
		{"generic all fails", "D:P" + base + "(A;;GA;;;BU)", "(GA)"},
		{"maximum allowed fails", "D:P" + base + "(A;;0x2000000;;;BU)", "(MA)"},
		{"delete child fails", "D:P" + base + "(A;;0x40;;;BU)", "(DC)"},
		{"write owner fails", "D:P" + base + "(A;;0x80000;;;BU)", "(WO)"},
		{"delete fails", "D:P" + base + "(A;;SD;;;BU)", "(D)"},
		{"grants to one SID are merged", "D:P" + base + "(A;;0x2;;;BU)(A;;0x100;;;BU)", "can write (WD,WA)"},
		{
			"callback allow is judged",
			"D:P" + base + "(XA;;FA;;;BU;(Member_of {SID(BA)}))",
			"can write",
		},
		{
			"object allow fails closed",
			"D:P" + base + "(OA;;FA;bf967aba-0de6-11d0-a285-00aa003049e2;;BU)",
			"unsupported type 5",
		},
		{"NULL DACL fails", "D:NO_ACCESS_CONTROL", "NULL DACL"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString(owner + tc.sddl)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.sddl, err)
			}
			reasons, err := judgeSecurity(sd, testTrusted(t), objectRule)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Join(reasons, "; ")
			switch {
			case tc.want == "" && got != "":
				t.Errorf("reasons = %q, want none", got)
			case tc.want != "" && !strings.Contains(got, tc.want):
				t.Errorf("reasons = %q, want them to contain %q", got, tc.want)
			}
		})
	}
}

// fixtureFailures returns the failures in err judged below root: root (a
// t.TempDir) and the host's ancestors of it are not the test's to judge.
func fixtureFailures(t *testing.T, err error, root string) []PathFailure {
	t.Helper()
	var out []PathFailure
	for _, f := range wantFailures(t, err) {
		if strings.HasPrefix(f.JudgedPath, root+`\`) {
			out = append(out, f)
		}
	}
	return out
}

// underParent makes a protected directory holding a protected file, both
// trusted-only, inside parent, and returns their refs.
func underParent(t *testing.T, parent string) []PathRef {
	t.Helper()
	dir := filepath.Join(parent, "d")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	setSecurity(t, dir, "D:P"+trustedACEs(t))
	file := filepath.Join(dir, "worker.yaml")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	setSecurity(t, file, "D:P"+trustedACEs(t))
	return []PathRef{{Key: "config dir", Path: dir}, {Key: "config", Path: file}}
}

// wantAncestorFailures asserts err reports, for each of refs and nothing
// else, one failure judged at ancestor whose reason names it and contains
// want.
func wantAncestorFailures(t *testing.T, err error, refs []PathRef, ancestor, want string) {
	t.Helper()
	failures := wantFailures(t, err)
	if len(failures) != len(refs) {
		t.Fatalf("got %d failures, want %d: %+v", len(failures), len(refs), failures)
	}
	prefix := "ancestor " + ancestor + ": "
	for i, f := range failures {
		if f.Key != refs[i].Key || f.Path != refs[i].Path || f.JudgedPath != ancestor {
			t.Errorf("failure %d = %+v, want key %s path %s judged at %s", i, f, refs[i].Key, refs[i].Path, ancestor)
		}
		if !strings.HasPrefix(f.Reason, prefix) || !strings.Contains(f.Reason, want) {
			t.Errorf("failure %d reason = %q, want %q followed by something containing %q", i, f.Reason, prefix, want)
		}
	}
}

// An ancestor an untrusted principal can delete children of lets them move a
// protected directory aside and plant their own.
func TestCheckPaths_FailsAncestorDeleteChild(t *testing.T) {
	parent := securedDir(t, "(A;;0x40;;;BU)")
	refs := underParent(t, parent)
	wantAncestorFailures(t, checkPaths(refs, testTrusted(t)), refs, parent,
		accountNameOf(t, windows.WinBuiltinUsersSid)+" can replace it or its children (DC)")
}

// WRITE_DAC on an ancestor lets its holder grant themselves DC.
func TestCheckPaths_FailsAncestorWriteDAC(t *testing.T) {
	parent := securedDir(t, "(A;;"+sddlWriteDAC+";;;AU)")
	refs := underParent(t, parent)
	wantAncestorFailures(t, checkPaths(refs, testTrusted(t)), refs, parent,
		accountNameOf(t, windows.WinAuthenticatedUserSid)+" can replace it or its children (WDAC)")
}

// Adding files or folders to an ancestor cannot replace an existing child, so
// the C:\ProgramData shape passes as an ancestor.
func TestCheckPaths_PassesAncestorAddOnly(t *testing.T) {
	parent := securedDir(t, "(A;CI;"+sddlProgramDataUsers+";;;BU)")
	if err := checkPaths(underParent(t, parent), testTrusted(t)); err != nil {
		t.Errorf("checkPaths(<under an add-only parent>) = %v, want nil", err)
	}
}

func TestCheckPaths_FailsAncestorTwoLevelsUp(t *testing.T) {
	grandparent := securedDir(t, "(A;;0x40;;;BU)")
	parent := filepath.Join(grandparent, "p")
	if err := os.Mkdir(parent, 0o750); err != nil {
		t.Fatal(err)
	}
	setSecurity(t, parent, "D:P"+trustedACEs(t))
	refs := underParent(t, parent)
	wantAncestorFailures(t, checkPaths(refs, testTrusted(t)), refs, grandparent, "(DC)")
}

// An ancestor whose owner is untrusted fails: its owner can always rewrite
// its DACL. Making Administrators an owner needs an elevated token, so this
// trusts SYSTEM, TrustedInstaller and the current user, not Administrators,
// and gives the parent to Administrators.
func TestCheckPaths_FailsAncestorUntrustedOwner(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("making Administrators an owner needs an elevated token; TestJudgeSecurity_AncestorRules covers the rule")
	}
	me, _ := currentAccount(t)
	ti, err := windows.StringToSid(trustedInstallerSID)
	if err != nil {
		t.Fatal(err)
	}
	trusted := []*windows.SID{wellKnownSID(t, windows.WinLocalSystemSid), ti, me}
	var aces strings.Builder
	for _, sid := range trusted {
		fmt.Fprintf(&aces, "(A;OICI;FA;;;%s)", sid)
	}
	root := t.TempDir()
	parent := filepath.Join(root, "p")
	dir := filepath.Join(parent, "d")
	for _, d := range []string{parent, dir} {
		if err := os.Mkdir(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	setSecurity(t, parent, "O:BAD:P"+aces.String())
	setSecurity(t, dir, fmt.Sprintf("O:%sD:P%s", me, aces.String()))
	failures := fixtureFailures(t, checkPaths([]PathRef{{Key: "config dir", Path: dir}}, trusted), root)
	want := "ancestor " + parent + ": owner " + accountNameOf(t, windows.WinBuiltinAdministratorsSid) + " is not trusted"
	if len(failures) != 1 || failures[0].JudgedPath != parent || failures[0].Reason != want {
		t.Errorf("failures in the fixture = %+v, want one judged at %s with reason %q", failures, parent, want)
	}
}

// TestJudgeSecurity_AncestorRules pins the narrower rule an ancestor is
// judged by.
func TestJudgeSecurity_AncestorRules(t *testing.T) {
	me, _ := currentAccount(t)
	owned := "O:" + me.String() + "D:P" + trustedACEs(t)
	tests := []struct {
		name string
		sddl string
		want string // "" = passes; otherwise a substring of the reasons
	}{
		{"add file and folder pass", owned + "(A;;" + sddlProgramDataUsers + ";;;AU)", ""},
		{`C:\ shape passes`, owned + "(A;;0x4;;;AU)(A;OICIIO;" + sddlModify + ";;;AU)", ""},
		{"deny passes", owned + "(D;;FA;;;BU)", ""},
		{"delete child fails", owned + "(A;;0x40;;;BU)", "(DC)"},
		{"delete fails", owned + "(A;;SD;;;BU)", "(D)"},
		{"write owner fails", owned + "(A;;0x80000;;;BU)", "(WO)"},
		{"generic write fails", owned + "(A;;GW;;;BU)", "(GW)"},
		{"generic all fails", owned + "(A;;GA;;;BU)", "(GA)"},
		{"maximum allowed fails", owned + "(A;;0x2000000;;;BU)", "(MA)"},
		{"modify names only the replace rights", owned + "(A;;" + sddlModify + ";;;BU)", "children (D)"},
		{"untrusted owner fails", "O:BUD:P" + trustedACEs(t), "owner "},
		{"object allow fails closed", owned + "(OA;;FA;bf967aba-0de6-11d0-a285-00aa003049e2;;BU)", "unsupported type 5"},
		{"NULL DACL fails", "O:" + me.String() + "D:NO_ACCESS_CONTROL", "NULL DACL"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString(tc.sddl)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.sddl, err)
			}
			reasons, err := judgeSecurity(sd, testTrusted(t), ancestorRule)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Join(reasons, "; ")
			switch {
			case tc.want == "" && got != "":
				t.Errorf("reasons = %q, want none", got)
			case tc.want != "" && !strings.Contains(got, tc.want):
				t.Errorf("reasons = %q, want them to contain %q", got, tc.want)
			}
		})
	}
}
