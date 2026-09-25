// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package winsvc

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// fakeService is a scripted controller. Control(Stop) behaves like the SCM's
// ControlService: NOT_ACTIVE when stopped, CANNOT_ACCEPT_CTRL (with the
// status) while start- or stop-pending, else accepted — the service then
// moves to StopPending and through afterStop. Each Query reports the current
// state, then advances along next.
type fakeService struct {
	state     svc.State
	next      []svc.State
	afterStop []svc.State
	ctlErr    error     // returned by every Control when set …
	ctlState  svc.State // … with this status
	stops     int
}

func (f *fakeService) Control(c svc.Cmd) (svc.Status, error) {
	if c != svc.Stop {
		return svc.Status{}, fmt.Errorf("unexpected control %d", c)
	}
	f.stops++
	st := svc.Status{State: f.state}
	switch {
	case f.ctlErr != nil:
		return svc.Status{State: f.ctlState}, f.ctlErr
	case f.state == svc.Stopped:
		return st, windows.ERROR_SERVICE_NOT_ACTIVE
	case f.state == svc.StartPending || f.state == svc.StopPending:
		return st, windows.ERROR_SERVICE_CANNOT_ACCEPT_CTRL
	}
	f.state, f.next = svc.StopPending, f.afterStop
	return svc.Status{State: svc.StopPending}, nil
}

func (f *fakeService) Query() (svc.Status, error) {
	st := svc.Status{State: f.state}
	if len(f.next) > 0 {
		f.state, f.next = f.next[0], f.next[1:]
	}
	return st, nil
}

func TestStopAndWait(t *testing.T) {
	const wait = 5 * time.Second
	for _, tc := range []struct {
		name      string
		svc       *fakeService
		wantStops int
	}{
		{"already stopped", &fakeService{state: svc.Stopped}, 1},
		{"running", &fakeService{state: svc.Running, afterStop: []svc.State{svc.Stopped}}, 1},
		{"stop pending on arrival", &fakeService{state: svc.StopPending, next: []svc.State{svc.StopPending, svc.Stopped}}, 1},
		{
			"start pending then running",
			&fakeService{
				state: svc.StartPending, next: []svc.State{svc.StartPending, svc.Running},
				afterStop: []svc.State{svc.Stopped},
			},
			2,
		},
		{"start pending then stopped on its own", &fakeService{state: svc.StartPending, next: []svc.State{svc.Stopped}}, 1},
		{
			// It stopped between the caller's Query and this Control.
			"stopped before the control landed",
			&fakeService{state: svc.Stopped, ctlErr: windows.ERROR_SERVICE_CANNOT_ACCEPT_CTRL, ctlState: svc.Stopped},
			1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := stopAndWait(tc.svc, "sqi-test", wait)
			if err != nil {
				t.Fatalf("stopAndWait = %v", err)
			}
			if st.State != svc.Stopped {
				t.Errorf("final state = %s, want stopped", stateString(st.State))
			}
			if tc.svc.stops != tc.wantStops {
				t.Errorf("sent %d stop controls, want %d", tc.svc.stops, tc.wantStops)
			}
		})
	}
}

func TestStopAndWait_NeverStopsIsBounded(t *testing.T) {
	for _, tc := range []struct {
		name string
		svc  *fakeService
		want string
	}{
		{"stuck stopping", &fakeService{state: svc.Running}, "still stop pending"},
		{"stuck starting", &fakeService{state: svc.StartPending}, "still start pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const wait = 300 * time.Millisecond
			start := time.Now()
			_, err := stopAndWait(tc.svc, "sqi-test", wait)
			elapsed := time.Since(start)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("stopAndWait = %v, want an error containing %q", err, tc.want)
			}
			if limit := wait + pollInterval + 100*time.Millisecond; elapsed > limit {
				t.Errorf("took %v, want at most %v", elapsed, limit)
			}
		})
	}
}

func TestStopAndWait_OtherControlErrorFails(t *testing.T) {
	f := &fakeService{state: svc.Running, ctlErr: windows.ERROR_ACCESS_DENIED}
	if _, err := stopAndWait(f, "sqi-test", time.Second); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("stopAndWait = %v, want ERROR_ACCESS_DENIED", err)
	}
}

func TestCreateError_MapsServiceExists(t *testing.T) {
	if err := createError("sqi-worker", windows.ERROR_SERVICE_EXISTS); !errors.Is(err, ErrServiceExists) {
		t.Errorf("createError(ERROR_SERVICE_EXISTS) = %v, want ErrServiceExists", err)
	}
	err := createError("sqi-worker", windows.ERROR_ACCESS_DENIED)
	if errors.Is(err, ErrServiceExists) || !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Errorf("createError(ERROR_ACCESS_DENIED) = %v, want it wrapped and not ErrServiceExists", err)
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
// clarification 4: an earlier install creates the protected directory, and a
// later --user install must add its account to it without widening it.
//
// The earlier install runs as the test user, who stands in for the elevated
// Administrator that really creates the directory: the grant reads the
// directory's attributes through its handle, which an unelevated owner with no
// ACE of its own cannot do (the Administrators ACE is deny-only here). The
// later install's account is NetworkService, named via its SID so the test
// also passes on a localized Windows.
func TestEnsureLogDir_GrantsAccountOnExistingProtectedDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	me, earlier := currentAccount(t)
	if err := EnsureLogDir(dir, earlier); err != nil { // the earlier install
		t.Fatal(err)
	}
	if _, protected := daclOf(t, dir); !protected {
		t.Fatal("a new log directory's DACL is not protected")
	}
	later, account := wellKnownAccount(t, windows.WinNetworkServiceSid)
	if err := EnsureLogDir(dir, account); err != nil {
		t.Fatal(err)
	}

	acl, protected := daclOf(t, dir)
	if !protected {
		t.Error("the grant unprotected the DACL; the directory would inherit ProgramData's Users access")
	}
	entries := aclEntries(t, acl)
	granted := allowedFor(entries, later)
	if len(granted) != 1 {
		t.Fatalf("want exactly one ACE for %s, got %d: %+v", account, len(granted), entries)
	}
	if granted[0].mask != fileModifyAccess {
		t.Errorf("account ACE mask = %#x, want Modify %#x (not full control)", granted[0].mask, fileModifyAccess)
	}
	if !inheritable(granted[0]) {
		t.Errorf("account ACE flags = %#x, want object+container inherit and not inherit-only", granted[0].flags)
	}
	for _, sid := range []*windows.SID{
		wellKnownSID(t, windows.WinLocalSystemSid), wellKnownSID(t, windows.WinBuiltinAdministratorsSid), me,
	} {
		assertFullControl(t, entries, sid)
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

// TestEnsureLogDir_RefusesJunction pins the fix for a privilege-escalation
// chain: any local user can create %ProgramData%\sqi and plant `logs` as a
// junction (no privilege needed), and following it would hand the service
// account Modify on the junction's target. EnsureLogDir must refuse, and the
// target's DACL must not change by a single byte.
func TestEnsureLogDir_RefusesJunction(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "logs")
	mklinkJunction(t, link, target)
	before := daclBytes(t, target)
	_, account := currentAccount(t)

	err := EnsureLogDir(link, account)
	if !errors.Is(err, errReparsePoint) {
		t.Errorf("EnsureLogDir(junction) = %v, want errReparsePoint", err)
	}
	if after := daclBytes(t, target); after != before {
		t.Errorf("the junction target's DACL changed:\nbefore %s\nafter  %s", before, after)
	}
}

// junctionParent builds <root>\target (with a plain `logs` subdirectory when
// withLogs) and <root>\sqi as a junction to it, the way any local user can
// pre-position %ProgramData%\sqi. It returns <root>\sqi\logs and the real
// <root>\target\logs.
func junctionParent(t *testing.T, withLogs bool) (dir, targetLogs string) {
	t.Helper()
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	targetLogs = filepath.Join(target, "logs")
	if withLogs {
		if err := os.Mkdir(targetLogs, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	parent := filepath.Join(root, "sqi")
	mklinkJunction(t, parent, target)
	return filepath.Join(parent, "logs"), targetLogs
}

// TestEnsureLogDir_RefusesReparsePointParent pins that a junction one level
// up cannot redirect the grant: every host has C:\Windows\Logs, so a
// %ProgramData%\sqi junction to C:\Windows would otherwise hand the service
// account Modify on it.
func TestEnsureLogDir_RefusesReparsePointParent(t *testing.T) {
	dir, targetLogs := junctionParent(t, true)
	before := daclBytes(t, targetLogs)
	_, account := currentAccount(t)

	err := EnsureLogDir(dir, account)
	if !errors.Is(err, errReparsePoint) {
		t.Errorf("EnsureLogDir(<junction>\\logs) = %v, want errReparsePoint", err)
	}
	if after := daclBytes(t, targetLogs); after != before {
		t.Errorf("the junction target's logs DACL changed:\nbefore %s\nafter  %s", before, after)
	}
}

// TestEnsureLogDir_RefusesReparsePointParentOnCreate is the create path of the
// same redirection: nothing may be created inside the junction's target.
func TestEnsureLogDir_RefusesReparsePointParentOnCreate(t *testing.T) {
	dir, targetLogs := junctionParent(t, false)
	_, account := currentAccount(t)

	err := EnsureLogDir(dir, account)
	if !errors.Is(err, errReparsePoint) {
		t.Errorf("EnsureLogDir(<junction>\\logs) = %v, want errReparsePoint", err)
	}
	if _, statErr := os.Lstat(targetLogs); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("a logs directory was created in the junction's target (lstat: %v)", statErr)
	}
}

func TestEnsureLogDir_RefusesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, account := currentAccount(t)
	if err := EnsureLogDir(path, account); err == nil {
		t.Error("EnsureLogDir accepted a regular file as the log directory")
	}
}

// mklinkJunction creates link as a directory junction to target. Unlike a
// symbolic link, a junction needs no privilege (as in
// internal/worker/isolation's workdir_windows_test.go).
func mklinkJunction(t *testing.T, link, target string) {
	t.Helper()
	if out, err := exec.CommandContext(t.Context(), "cmd", "/c", "mklink", "/j", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /j %q %q: %v: %s", link, target, err, out)
	}
}

// daclBytes renders dir's DACL protection state and raw DACL bytes, for a
// byte-for-byte before/after comparison.
func daclBytes(t *testing.T, dir string) string {
	t.Helper()
	acl, protected := daclOf(t, dir)
	// ACL header: revision (1 byte), Sbz1 (1), AclSize (2) — x/sys does not export the size.
	size := *(*uint16)(unsafe.Add(unsafe.Pointer(acl), 2))
	raw := unsafe.Slice((*byte)(unsafe.Pointer(acl)), size)
	return fmt.Sprintf("protected=%v %x", protected, raw)
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

// wellKnownAccount returns a well-known account's SID and its (localized)
// DOMAIN\name.
func wellKnownAccount(t *testing.T, wk windows.WELL_KNOWN_SID_TYPE) (*windows.SID, string) {
	t.Helper()
	sid := wellKnownSID(t, wk)
	name, domain, _, err := sid.LookupAccount("")
	if err != nil {
		t.Fatal(err)
	}
	return sid, domain + `\` + name
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
