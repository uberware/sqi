// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration && windows

// Package integration's Windows run-as-user isolation suite.
//
// Two tiers, split by test-name prefix because they need different
// privileges:
//
//   - TestIsolationWindows_*       elevated Administrator is enough
//   - TestIsolationWindowsSystem_* must run as SYSTEM
//
// CreateProcessAsUser (which os/exec invokes whenever SysProcAttr.Token is
// set) requires SeAssignPrimaryTokenPrivilege. By default only LocalSystem
// and the LOCAL/NETWORK SERVICE accounts hold it; an elevated Administrator
// does not. A test asserting that a child ran as the target user therefore
// belongs in the System tier, which scripts/test-isolation-windows.ps1 runs
// via a scheduled task with /ru SYSTEM.
//
// Everything else belongs in the fast tier: ACL shape, credential storage,
// and real impersonated access checks (LogonUserW needs no privilege for a
// local account with a password; ImpersonateLoggedOnUser needs only
// SeImpersonatePrivilege, which administrators hold).
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/uberware/sqi/internal/fsutil/fsutiltest"
	workerconfig "github.com/uberware/sqi/internal/worker/config"
	"github.com/uberware/sqi/internal/worker/envutil"
	"github.com/uberware/sqi/internal/worker/executor"
	"github.com/uberware/sqi/internal/worker/fmtres"
	"github.com/uberware/sqi/internal/worker/isolation"
	"github.com/uberware/sqi/internal/worker/metrics"
	"github.com/uberware/sqi/internal/worker/protocol"
	"github.com/uberware/sqi/internal/worker/session"
	"github.com/uberware/sqi/internal/worker/staging"
	"github.com/uberware/sqi/internal/worker/status"
)

// requireHarness skips unless the PowerShell harness set up the throwaway
// accounts. A bare `make test-integration` does not create OS accounts.
func requireHarness(t *testing.T) {
	t.Helper()
	if os.Getenv("SQI_TEST_ISOLATION_WINDOWS") != "1" {
		t.Skip("SQI_TEST_ISOLATION_WINDOWS != 1; run via make test-isolation-windows")
	}
}

// currentAccountName returns the account this process is running as, in
// DOMAIN\user form.
func currentAccountName(t *testing.T) string {
	t.Helper()
	tok := windows.GetCurrentProcessToken()
	user, err := tok.GetTokenUser()
	if err != nil {
		t.Fatalf("GetTokenUser: %v", err)
	}
	account, domain, _, err := user.User.Sid.LookupAccount("")
	if err != nil {
		t.Fatalf("LookupAccount: %v", err)
	}
	return domain + `\` + account
}

// TestIsolationWindowsSystem_RunsAsSystem is the harness's own smoke test. If
// the scheduled task ran the binary as the invoking administrator instead of
// SYSTEM, every other System-tier assertion would fail for the wrong reason,
// or a privilege-dependent test could be skipped and read as a pass. This
// asserts the premise directly.
func TestIsolationWindowsSystem_RunsAsSystem(t *testing.T) {
	requireHarness(t)

	got := currentAccountName(t)

	if !strings.EqualFold(got, `NT AUTHORITY\SYSTEM`) {
		t.Fatalf("running as %q, want NT AUTHORITY\\SYSTEM; the scheduled task did not run as SYSTEM", got)
	}
}

// TestIsolationWindows_CredentialRoundTrip checks that an operator-provisioned
// secret is readable by the worker: encrypt with the machine key, write with
// a locked-down ACL, read it back. It runs in the admin tier because DPAPI
// needs no special privilege.
func TestIsolationWindows_CredentialRoundTrip(t *testing.T) {
	requireHarness(t)
	dir := t.TempDir()
	user := os.Getenv("SQI_TEST_ISOLATION_USER_A")
	want := os.Getenv("SQI_TEST_ISOLATION_PASS_A")

	store := isolation.NewFileStore(dir)
	putter, ok := store.(interface {
		Put(user, secret string) error
	})
	if !ok {
		t.Fatal("windows file store must expose Put")
	}
	if err := putter.Put(user, want); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := store.Secret(user)
	if err != nil {
		t.Fatalf("Secret: %v", err)
	}
	if got != want {
		t.Errorf("Secret = %q, want %q", got, want)
	}
}

// TestIsolationWindows_CredentialMissingIsActionable checks that an operator
// who never ran set-credential gets a message naming the fix, not "empty
// secret".
func TestIsolationWindows_CredentialMissingIsActionable(t *testing.T) {
	requireHarness(t)

	_, err := isolation.NewFileStore(t.TempDir()).Secret("never-provisioned")

	if err == nil {
		t.Fatal("Secret for an unprovisioned account = nil error, want a failure")
	}
	if !strings.Contains(err.Error(), "set-credential") {
		t.Errorf("Secret error = %q, want it to name the set-credential fix", err.Error())
	}
}

// TestIsolationWindows_CredentialDirectoryExcludesUnprivilegedTrustees checks
// that after Put, the credential directory and the file it wrote both carry a
// protected DACL that excludes every unprivileged trustee, BUILTIN\Users
// above all, which a ProgramData-rooted parent tree would otherwise grant. It
// reads the on-disk DACL back with windows.GetNamedSecurityInfo, independently
// of the openForACL/applyProtectedDACL primitives Put uses to write it.
//
// This runs in the elevated tier, not alongside the package's unit tests
// (internal/worker/isolation/credstore_windows_test.go), because an
// unelevated process cannot complete Put: adminOnlyDACL (acl_windows.go)
// grants the credential directory to SYSTEM and Administrators only, with no
// CREATOR OWNER placeholder for the creator. The caller of set-credential is
// an elevated Administrator, so this makes running from an elevated shell an
// enforced requirement.
func TestIsolationWindows_CredentialDirectoryExcludesUnprivilegedTrustees(t *testing.T) {
	requireHarness(t)
	dir := t.TempDir()
	store, ok := isolation.NewFileStore(dir).(interface {
		Put(user, secret string) error
	})
	if !ok {
		t.Fatal("windows file store must expose Put")
	}
	const user = "render-svc"

	if err := store.Put(user, "secret"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Put's on-disk filename is a hex encoding of the normalized account
	// name (credFileName, internal/worker/isolation/credstore.go). The test
	// finds the file by listing the directory instead of depending on that
	// unexported naming scheme.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	if len(entries) != 1 {
		t.Fatalf("credential directory has %d entries after one Put, want exactly 1", len(entries))
	}
	filePath := filepath.Join(dir, entries[0].Name())

	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatalf("CreateWellKnownSid(system): %v", err)
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatalf("CreateWellKnownSid(admins): %v", err)
	}
	users, err := windows.CreateWellKnownSid(windows.WinBuiltinUsersSid)
	if err != nil {
		t.Fatalf("CreateWellKnownSid(users): %v", err)
	}

	t.Run("directory", func(t *testing.T) {
		acl := readDACL(t, dir)

		// 2: one ACE per trustee, SYSTEM and Administrators, and nothing
		// else: no CREATOR OWNER, no direct grant for the account running
		// Put.
		//
		// SetSecurityInfo splits an inheritable ACE applied to a container
		// into a direct ACE plus a separate INHERIT_ONLY one, but only for an
		// ACE carrying GENERIC bits, which are re-mapped per child type.
		// explicitFullControl grants the already-mapped FILE_ALL_ACCESS (see
		// isolation.fileAllAccess), so no split happens and the applied ACL
		// matches the one the package builds. A split ACL would grant the same
		// two trustees, but counting ACEs would no longer show that nothing
		// else is granted.
		if got := aceCount(t, acl); got != 2 {
			t.Errorf("directory ACL has %d ACEs, want exactly 2 (SYSTEM, Administrators)", got)
		}
		if !hasSID(t, acl, system) {
			t.Error("directory ACL must grant SYSTEM, or the worker service cannot re-secure it")
		}
		if !hasSID(t, acl, admins) {
			t.Error("directory ACL must grant Administrators, or an operator is locked out")
		}
		if hasSID(t, acl, users) {
			t.Error("directory ACL must not grant BUILTIN\\Users; that would disclose the stored credential to every local user")
		}
	})

	t.Run("file", func(t *testing.T) {
		acl := readDACL(t, filePath)

		if got := aceCount(t, acl); got != 2 {
			t.Errorf("file ACL has %d ACEs, want exactly 2 (SYSTEM, Administrators)", got)
		}
		if !hasSID(t, acl, system) {
			t.Error("file ACL must grant SYSTEM, or the worker service cannot read the credential it needs")
		}
		if !hasSID(t, acl, admins) {
			t.Error("file ACL must grant Administrators, or an operator is locked out")
		}
		if hasSID(t, acl, users) {
			t.Error("file ACL must not grant BUILTIN\\Users; that would disclose the stored credential to every local user")
		}
	})
}

// readDACL fetches path's discretionary ACL from the filesystem via
// GetNamedSecurityInfo.
func readDACL(t *testing.T, path string) *windows.ACL {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo(%q): %v", path, err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("SECURITY_DESCRIPTOR.DACL(%q): %v", path, err)
	}
	return acl
}

// aceCount reports how many entries acl carries.
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

// harnessStore builds a CredentialStore seeded with whichever harness
// accounts the environment provides (SQI_TEST_ISOLATION_USER_A/PASS_A and
// the _B pair), so resolveHarnessCredential can resolve either one through
// the real logon_user Provider. An account whose USER env var is unset is
// not seeded, so a caller that needs one account still gets a usable store.
func harnessStore(t *testing.T) isolation.CredentialStore {
	t.Helper()
	store := isolation.NewFileStore(t.TempDir())
	putter, ok := store.(interface {
		Put(user, secret string) error
	})
	if !ok {
		t.Fatal("windows file store must expose Put")
	}
	for _, pair := range [][2]string{
		{"SQI_TEST_ISOLATION_USER_A", "SQI_TEST_ISOLATION_PASS_A"},
		{"SQI_TEST_ISOLATION_USER_B", "SQI_TEST_ISOLATION_PASS_B"},
	} {
		user := os.Getenv(pair[0])
		if user == "" {
			continue
		}
		if err := putter.Put(user, os.Getenv(pair[1])); err != nil {
			t.Fatalf("Put(%s): %v", user, err)
		}
	}
	return store
}

// resolveHarnessCredential resolves a genuine *isolation.Credential for user
// via the real logon_user Provider, backed by harnessStore.
// internal/worker/isolation exports no test-only API, so the assertions below
// call the production SecureWorkDir/ChownRecursive with a credential obtained
// through Resolve, as a real worker obtains one, not a hand-built value.
func resolveHarnessCredential(t *testing.T, user string) *isolation.Credential {
	t.Helper()
	store := harnessStore(t)
	p, err := isolation.NewProvider(isolation.Config{Provider: "logon_user", CredentialStore: store})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	cred, err := p.Resolve(context.Background(), isolation.Spec{User: user})
	if err != nil {
		t.Fatalf("Resolve(%s): %v", user, err)
	}
	return cred
}

// golang.org/x/sys/windows has no wrapper for advapi32.dll's
// LogonUserW or ImpersonateLoggedOnUser; it wraps RevertToSelf and
// ImpersonateSelf only. This package cannot reach the unexported logonUserW
// seam in internal/worker/isolation, so both calls are declared here, the way
// golang.org/x/sys/windows declares the Win32 calls it wraps.
var (
	modAdvapi32                 = windows.NewLazySystemDLL("advapi32.dll")
	procLogonUserW              = modAdvapi32.NewProc("LogonUserW")
	procImpersonateLoggedOnUser = modAdvapi32.NewProc("ImpersonateLoggedOnUser")
)

// impersonateLoggedOnUser wraps the raw ImpersonateLoggedOnUser syscall.
func impersonateLoggedOnUser(tok windows.Token) error {
	r1, _, e1 := procImpersonateLoggedOnUser.Call(uintptr(tok))
	if r1 == 0 {
		if e1 != nil {
			return e1
		}
		return errors.New("ImpersonateLoggedOnUser: failed with no error code")
	}
	return nil
}

// logonUserW wraps the raw LogonUserW syscall. logonType/logonProvider match
// the LOGON32_LOGON_BATCH / LOGON32_PROVIDER_DEFAULT values
// internal/worker/isolation's logonUserOS uses, so impersonate below
// authenticates the harness accounts as a real worker does.
func logonUserW(username, domain, password *uint16, logonType, logonProvider uint32) (windows.Token, error) {
	var token windows.Token
	r1, _, e1 := procLogonUserW.Call(
		uintptr(unsafe.Pointer(username)),
		uintptr(unsafe.Pointer(domain)),
		uintptr(unsafe.Pointer(password)),
		uintptr(logonType),
		uintptr(logonProvider),
		uintptr(unsafe.Pointer(&token)),
	)
	if r1 == 0 {
		if e1 != nil {
			return 0, e1
		}
		return 0, errors.New("LogonUserW: failed with no error code")
	}
	return token, nil
}

// impersonate logs on as user and runs fn under that identity. LogonUserW
// needs no privilege for a local account with a password, and
// ImpersonateLoggedOnUser needs only SeImpersonatePrivilege, which
// administrators hold, so these ACL assertions run in the fast tier instead
// of behind the scheduled task.
func impersonate(t *testing.T, user, pass string, fn func()) {
	t.Helper()
	u, err := windows.UTF16PtrFromString(user)
	if err != nil {
		t.Fatalf("encode user: %v", err)
	}
	p, err := windows.UTF16PtrFromString(pass)
	if err != nil {
		t.Fatalf("encode pass: %v", err)
	}
	// LOGON32_LOGON_BATCH = 4, LOGON32_PROVIDER_DEFAULT = 0.
	tok, err := logonUserW(u, nil, p, 4, 0)
	if err != nil {
		t.Fatalf("LogonUser(%s): %v", user, err)
	}
	defer tok.Close()

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := impersonateLoggedOnUser(tok); err != nil {
		t.Fatalf("ImpersonateLoggedOnUser: %v", err)
	}
	defer windows.RevertToSelf() //nolint:errcheck // test cleanup

	fn()
}

// TestIsolationWindows_SessionDirACLIsProtected checks that the ACL is applied
// as well as built. secureDACL's unit test covers the three entries; it cannot
// show that applyProtectedDACL stripped inheritance on a real directory.
// Without the protected flag the session would keep whatever its parent
// grants, typically BUILTIN\Users, and the three explicit entries would be in
// addition to those, leaving every session readable by other local users.
func TestIsolationWindows_SessionDirACLIsProtected(t *testing.T) {
	requireHarness(t)
	user := os.Getenv("SQI_TEST_ISOLATION_USER_A")

	dir := t.TempDir()
	cred := resolveHarnessCredential(t, user)
	defer cred.Close()
	if err := isolation.SecureWorkDir(dir, cred); err != nil {
		t.Fatalf("SecureWorkDir: %v", err)
	}

	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo: %v", err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatalf("Control: %v", err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Error("session directory DACL is not PROTECTED; it still inherits ACEs from its parent")
	}

	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("DACL: %v", err)
	}
	// golang.org/x/sys/windows has no GetAclInformation wrapper; windows.ACL
	// exports AceCount as a struct field, which aceCount reads.
	if got := aceCount(t, dacl); got != 3 {
		t.Errorf("applied DACL has %d ACEs, want exactly 3 (%s, SYSTEM, Administrators)", got, user)
	}
}

// TestIsolationWindows_SecondAccountCannotOpenSessionDir is the real access
// check the structural DACL test cannot make: account B, impersonated, must be
// denied a session directory secured for account A.
func TestIsolationWindows_SecondAccountCannotOpenSessionDir(t *testing.T) {
	requireHarness(t)
	userA := os.Getenv("SQI_TEST_ISOLATION_USER_A")
	userB := os.Getenv("SQI_TEST_ISOLATION_USER_B")
	passB := os.Getenv("SQI_TEST_ISOLATION_PASS_B")

	dir := t.TempDir()
	credA := resolveHarnessCredential(t, userA)
	defer credA.Close()
	if err := isolation.SecureWorkDir(dir, credA); err != nil {
		t.Fatalf("SecureWorkDir: %v", err)
	}

	impersonate(t, userB, passB, func() {
		if _, err := os.ReadDir(dir); err == nil {
			t.Errorf("account %s could read a session directory secured for %s", userB, userA)
		}
	})
}

// TestIsolationWindows_CredentialFileNotReadableByRunAsAccount checks the
// claim in the credential store's doc: machine-scope DPAPI is decryptable by
// anything on this host that can read the file, so the file ACL is the
// boundary. A task must not be able to recover the password of the account it
// runs as.
func TestIsolationWindows_CredentialFileNotReadableByRunAsAccount(t *testing.T) {
	requireHarness(t)
	userA := os.Getenv("SQI_TEST_ISOLATION_USER_A")
	passA := os.Getenv("SQI_TEST_ISOLATION_PASS_A")

	dir := t.TempDir()
	store := isolation.NewFileStore(dir)
	putter, ok := store.(interface {
		Put(user, secret string) error
	})
	if !ok {
		t.Fatal("windows file store must expose Put")
	}
	if err := putter.Put(userA, passA); err != nil {
		t.Fatalf("Put: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("ReadDir(%q) = %v entries, %v; want exactly 1", dir, len(entries), err)
	}
	blob := filepath.Join(dir, entries[0].Name())

	impersonate(t, userA, passA, func() {
		if _, err := os.ReadFile(blob); err == nil {
			t.Errorf("account %s could read its own stored credential at %s", userA, blob)
		}
	})
}

// TestIsolationWindows_ChownRecursiveDoesNotFollowJunction checks the
// reparse-point guard. If a task plants a junction inside its session
// directory pointing at a directory it does not own, sqi must not grant the
// task's account full control of the target.
func TestIsolationWindows_ChownRecursiveDoesNotFollowJunction(t *testing.T) {
	requireHarness(t)
	userA := os.Getenv("SQI_TEST_ISOLATION_USER_A")
	passA := os.Getenv("SQI_TEST_ISOLATION_PASS_A")

	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("not for the task"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	link := filepath.Join(root, "escape")
	fsutiltest.Junction(t, link, outside)

	credA := resolveHarnessCredential(t, userA)
	defer credA.Close()
	if err := isolation.ChownRecursive(root, credA); err != nil {
		t.Fatalf("ChownRecursive: %v", err)
	}

	impersonate(t, userA, passA, func() {
		if _, err := os.ReadFile(secret); err == nil {
			t.Errorf("ChownRecursive followed a junction and granted %s access to %s", userA, secret)
		}
	})
}

// TestIsolationWindows_CapableRejectsPlainAdmin checks that Capable() tests
// the privilege. An elevated Administrator does not hold
// SeAssignPrimaryTokenPrivilege, so Capable() must refuse here; the System
// tier's counterpart test checks that it accepts when the privilege is
// present. A Capable() that always returned nil would pass only one half.
func TestIsolationWindows_CapableRejectsPlainAdmin(t *testing.T) {
	requireHarness(t)

	p, err := isolation.NewProvider(isolation.Config{Provider: "logon_user"})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	if err := p.Capable(); err == nil {
		t.Error("Capable() = nil as a plain administrator, want ErrNotCapable; administrators do not hold SeAssignPrimaryTokenPrivilege")
	}
}

// TestIsolationWindowsSystem_ProfileEnvPointsAtTargetUser checks that the
// loaded profile is real: the child's USERPROFILE resolves under the target
// account's own directory, not the daemon's. Without LoadUserProfile it would
// be SYSTEM's profile under System32\config\systemprofile, and every DCC
// would write its preferences there.
func TestIsolationWindowsSystem_ProfileEnvPointsAtTargetUser(t *testing.T) {
	requireHarness(t)
	user := os.Getenv("SQI_TEST_ISOLATION_USER_A")

	p, err := isolation.NewProvider(isolation.Config{
		Provider:        "logon_user",
		CredentialStore: harnessStore(t),
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	cred, err := p.Resolve(context.Background(), isolation.Spec{User: user})
	if err != nil {
		t.Fatalf("Resolve(%s): %v", user, err)
	}
	defer cred.Close() // test cleanup

	if cred.Home == "" {
		t.Fatal("Credential.Home is empty after LoadUserProfile; the profile was not created")
	}
	if !strings.Contains(strings.ToLower(cred.Home), strings.ToLower(user)) {
		t.Errorf("Credential.Home = %q, want a path under %s's own profile", cred.Home, user)
	}
	if _, err := os.Stat(cred.Home); err != nil {
		t.Errorf("profile directory %q does not exist: %v", cred.Home, err)
	}
}

// TestIsolationWindowsSystem_Capable is the System-tier half of
// TestIsolationWindows_CapableRejectsPlainAdmin: as SYSTEM, the worker holds
// SeAssignPrimaryTokenPrivilege, so Capable() must accept.
func TestIsolationWindowsSystem_Capable(t *testing.T) {
	requireHarness(t)

	p, err := isolation.NewProvider(isolation.Config{Provider: "logon_user"})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	if err := p.Capable(); err != nil {
		t.Errorf("Capable() as SYSTEM = %v, want nil", err)
	}
}

// TestIsolationWindowsSystem_ChildRunsAsTargetUser is the assertion no fake
// can make: the process started under the target account's identity.
//
// This reads identity from the child's token (whoami), not from
// `cmd /c echo %USERNAME%`. cmd.Env is nil here, so exec.Cmd passes
// os.Environ() as a non-NULL lpEnvironment to CreateProcessAsUser, which does
// not rewrite a caller-supplied environment block (MSDN tells callers to build
// one with CreateEnvironmentBlock). cmd.exe would therefore expand
// %USERNAME% from the parent's block, not the child's identity. whoami reads
// the process token with GetTokenInformation, so it shows that the token
// switch took effect. Under the SYSTEM-tier harness the parent's USERNAME is
// `<COMPUTERNAME>$` or `SYSTEM`, so a %USERNAME% check would fail, and a pass
// would say nothing about the token.
func TestIsolationWindowsSystem_ChildRunsAsTargetUser(t *testing.T) {
	requireHarness(t)
	user := os.Getenv("SQI_TEST_ISOLATION_USER_A")

	cred := resolveHarnessCredential(t, user)
	defer cred.Close() // test cleanup

	cmd := exec.CommandContext(context.Background(), "whoami")
	if err := isolation.Apply(cmd, cred); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run as %s: %v", user, err)
	}

	// whoami prints "DOMAIN\user" (or "COMPUTERNAME\user" for a local
	// account). Keep only the account part, because user, from
	// SQI_TEST_ISOLATION_USER_A, is unqualified.
	got := strings.TrimSpace(string(out))
	if i := strings.LastIndex(got, `\`); i != -1 {
		got = got[i+1:]
	}
	if !strings.EqualFold(got, user) {
		t.Errorf("child reported identity %q, want %q", got, user)
	}
}

// TestIsolationWindowsSystem_ChildCanWriteSessionDir catches what the
// structural ACL test cannot: an ACL that is correct in shape but too tight in
// practice, leaving the task unable to write its own scratch.
func TestIsolationWindowsSystem_ChildCanWriteSessionDir(t *testing.T) {
	requireHarness(t)
	user := os.Getenv("SQI_TEST_ISOLATION_USER_A")

	dir := t.TempDir()
	cred := resolveHarnessCredential(t, user)
	defer cred.Close() // test cleanup
	if err := isolation.SecureWorkDir(dir, cred); err != nil {
		t.Fatalf("SecureWorkDir: %v", err)
	}

	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "echo written > out.txt")
	cmd.Dir = dir
	if err := isolation.Apply(cmd, cred); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("task could not write its own session directory: %v: %s", err, out)
	}

	if _, err := os.Stat(filepath.Join(dir, "out.txt")); err != nil {
		t.Errorf("expected file was not written: %v", err)
	}
}

// TestIsolationWindowsSystem_DaemonEnvNotInherited checks that the environment
// policy holds across a real process boundary: a daemon secret must not reach
// job code, while an operator-allowlisted license variable must.
func TestIsolationWindowsSystem_DaemonEnvNotInherited(t *testing.T) {
	requireHarness(t)
	user := os.Getenv("SQI_TEST_ISOLATION_USER_A")
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAsecretdaemonvalue")
	t.Setenv("FOUNDRY_LICENSE", "4101@licserver")

	base := envutil.BaseEnv(envutil.Policy{Enabled: true, Allowlist: []string{"*_LICENSE"}})

	if _, leaked := base["AWS_ACCESS_KEY_ID"]; leaked {
		t.Error("daemon AWS_ACCESS_KEY_ID reached the task environment")
	}
	if _, present := base["FOUNDRY_LICENSE"]; !present {
		t.Error("allowlisted FOUNDRY_LICENSE did not reach the task environment")
	}
	cred := resolveHarnessCredential(t, user)
	defer cred.Close() // test cleanup

	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "echo [%AWS_ACCESS_KEY_ID%]")
	cmd.Env = envutil.BuildFromBase(base, nil, nil)
	if err := isolation.Apply(cmd, cred); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(string(out), "AKIAsecret") {
		t.Errorf("child saw the daemon's AWS key: %s", out)
	}
}

// TestIsolationWindowsSystem_CrossUserSessionDirDenied repeats the admin
// tier's impersonated check with real processes, as a worker runs them.
func TestIsolationWindowsSystem_CrossUserSessionDirDenied(t *testing.T) {
	requireHarness(t)
	userA := os.Getenv("SQI_TEST_ISOLATION_USER_A")
	userB := os.Getenv("SQI_TEST_ISOLATION_USER_B")

	dir := t.TempDir()
	credA := resolveHarnessCredential(t, userA)
	defer credA.Close() // test cleanup
	if err := isolation.SecureWorkDir(dir, credA); err != nil {
		t.Fatalf("SecureWorkDir: %v", err)
	}
	credB := resolveHarnessCredential(t, userB)
	defer credB.Close() // test cleanup

	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "dir "+dir)
	if err := isolation.Apply(cmd, credB); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if err := cmd.Run(); err == nil {
		t.Errorf("a process running as %s listed a session directory secured for %s", userB, userA)
	}
}

// TestIsolationWindowsSystem_NoIsolationUnchanged pins the no-isolation
// default: with no credential, a task runs as the daemon with the full
// inherited environment.
func TestIsolationWindowsSystem_NoIsolationUnchanged(t *testing.T) {
	requireHarness(t)
	t.Setenv("SQI_REGRESSION_MARKER", "inherited")

	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "echo %SQI_REGRESSION_MARKER%")
	if err := isolation.Apply(cmd, nil); err != nil {
		t.Fatalf("Apply(nil): %v", err)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := strings.TrimSpace(string(out)); got != "inherited" {
		t.Errorf("unisolated task saw %q, want the daemon's full environment", got)
	}
}

// TestIsolationWindowsSystem_StageOutRefusesPlantedJunction tests stage-out's
// junction refusal end to end: a real task, running as a second local account
// under a real logon token, plants the junction, and the daemon refuses.
//
// This is a System-tier test because planting the junction as the target user
// needs CreateProcessAsUser, hence SeAssignPrimaryTokenPrivilege, which an
// elevated administrator does not hold.
//
// The unit tests in internal/worker/staging cover the refusal; this test
// covers their premise, that an isolated task has write access to its own
// scratch subdirectory and can perform this swap. Without run-as-user
// isolation the task already runs as the daemon's account and gains nothing
// from the swap, so it matters only with isolation enabled.
func TestIsolationWindowsSystem_StageOutRefusesPlantedJunction(t *testing.T) {
	requireHarness(t)
	user := os.Getenv("SQI_TEST_ISOLATION_USER_A")

	cred := resolveHarnessCredential(t, user)
	defer cred.Close() // test cleanup

	scratch := t.TempDir()
	// "builtin" duplicates staging's unexported builtinSentinel (staging.go),
	// which this package cannot import. If the sentinel changes, this literal
	// becomes a sync_command template and stage-out would try to execute it.
	// The "outside scratch" assertion at the bottom checks the reason StageOut
	// refused, so a mismatch fails there with the wrong error instead of
	// passing as a containment refusal.
	s := staging.New(scratch, "builtin", false, slog.New(slog.DiscardHandler))

	outOrig := filepath.Join(t.TempDir(), "render.exr")
	entries := []protocol.StageEntry{
		{Path: outOrig, Direction: "OUT", ObjectType: "FILE"},
	}
	_, scratchDir, err := s.StageIn(context.Background(), "job1", "att1", entries, cred)
	if err != nil {
		t.Fatalf("StageIn: %v", err)
	}

	secretDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(secretDir, "render.exr"), []byte("daemon-only-contents"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The task, not the test process, removes its own scratch subdirectory
	// and junctions it at the secret directory. This must succeed: if the
	// isolated account could not do it, the refusal below would prove nothing.
	stagedDir := filepath.Join(scratchDir, "0")

	// The script is delivered through SysProcAttr.CmdLine, not exec.Cmd.Args,
	// and each path is quoted.
	//
	// os/exec builds a Windows command line by escaping each Arg with
	// CommandLineToArgvW rules, so an embedded `"` becomes `\"`. cmd.exe does
	// not understand that escape: it hands rmdir the literal `\"C:\...\0\"`,
	// which is not a valid path, `&&` short-circuits, and the plant step
	// fails with "The filename, directory name, or volume label syntax is
	// incorrect", unrelated to the property under test. This is the documented
	// os/exec exception for cmd.exe.
	//
	// Without quotes (as the other cmd /c calls in this file use, e.g.
	// "dir "+dir) the command works only while no path contains a space. These
	// are t.TempDir() paths rooted at os.TempDir(), which is SYSTEM's %TEMP%
	// under this tier, normally C:\Windows\TEMP but a host setting. Quoting
	// keeps a space from failing the plant step for a reason unrelated to
	// containment.
	//
	// cmd's /S strips exactly the first and last quote after /C and passes the
	// rest through verbatim, instead of cmd's conditional rule, which applies
	// only when there are exactly two quotes and none of & < > ( ) @ ^ |
	// between them. This string has more quotes. CmdLine includes the program
	// name, and exec ignores Args entirely when it is set.
	//
	// isolation.Apply sets only SysProcAttr.Token, allocating the struct when
	// it is nil, so a caller-set CmdLine survives it (see
	// isolation/apply_windows.go).
	cmd := exec.CommandContext(context.Background(), "cmd")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine: `cmd /s /c "rmdir /s /q "` + stagedDir + `" && mklink /J "` + stagedDir + `" "` + secretDir + `""`,
	}
	if err := isolation.Apply(cmd, cred); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the isolated task could not plant the junction, so the "+
			"refusal below is not tested: %v: %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(stagedDir, "render.exr")); err != nil {
		t.Fatalf("junction is not live: %v", err)
	}

	// Check the reason for the refusal as well as that StageOut failed: a
	// denied os.OpenRoot, a sharing violation or a malformed path also yield
	// an error and say nothing about containment. errStageOutEscape is
	// unexported and unreachable from this package, so match the
	// operator-facing message, as the unit-tier test
	// TestStageOut_RefusesJunctionedScratchSubdir does alongside its errors.Is
	// check.
	stageOutErr := s.StageOut(context.Background(), scratchDir, entries)
	if stageOutErr == nil {
		t.Error("StageOut accepted a source reached through a task-planted junction")
	} else if !strings.Contains(stageOutErr.Error(), "outside scratch") {
		t.Errorf("StageOut failed for the wrong reason: %v; want the containment "+
			"refusal naming the scratch boundary, not an incidental access failure", stageOutErr)
	}
	if _, err := os.Stat(outOrig); err == nil {
		t.Fatal("outOrig exists: a task running as a separate account made the " +
			"elevated daemon copy a file from outside its scratch directory")
	}
}

// TestIsolationWindowsSystem_TaskStartsSuspendedAsTargetUser drives the
// executor's start path for an isolated task: the process is created
// suspended under the target account's logon token, assigned to its job
// object, and resumed by the worker, which refuses a process that was not
// suspended. The task fails if the suspended flag does not reach
// CreateProcessAsUser or the worker cannot resume a thread of another
// account's process. The other tests in this file start their processes
// without the executor.
func TestIsolationWindowsSystem_TaskStartsSuspendedAsTargetUser(t *testing.T) {
	requireHarness(t)
	user := os.Getenv("SQI_TEST_ISOLATION_USER_A")
	logger := slog.New(slog.DiscardHandler)

	provider, err := isolation.NewProvider(isolation.Config{Provider: "logon_user", CredentialStore: harnessStore(t)})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	mgr := session.NewManager(filepath.Join(t.TempDir(), "sessions"), false, provider,
		workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, logger)
	statuses := &statusRecorder{}
	lines := &lineRecorder{}
	ex := executor.New(status.New(statuses, status.Config{WorkerID: "w-isolation"}, logger),
		mgr, metrics.New(), lines, executor.Config{KillGracePeriod: time.Second}, logger)

	msg := &protocol.AssignMsg{
		Version:   protocol.ProtocolVersion,
		Type:      protocol.TypeAssign,
		JobID:     "job-isolated",
		TaskID:    "task-isolated",
		AttemptID: "attempt-1",
		OnRun:     &protocol.Action{Command: "whoami"},
		Isolation: &protocol.IsolationSpec{User: user},
	}
	if err := ex.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	// The session's cleanup (directory removal, profile unload) runs after
	// the terminal status, so wait for the task goroutine before the test's
	// temporary directories are removed.
	t.Cleanup(func() { ex.DrainAndShutdown(30 * time.Second) })
	got, reason := statuses.waitTerminal(t, time.Minute)
	if got != "succeeded" {
		t.Fatalf("task ended %q (%s), want succeeded; output: %q", got, reason, lines.all())
	}
	var identity string
	for _, l := range lines.all() {
		if s := strings.TrimSpace(l); s != "" {
			identity = s
		}
	}
	if i := strings.LastIndex(identity, `\`); i != -1 {
		identity = identity[i+1:]
	}
	if !strings.EqualFold(identity, user) {
		t.Errorf("task reported identity %q, want %q", identity, user)
	}
}

// TestIsolationWindowsSystem_EnvironmentActionStartsSuspendedAsTargetUser
// drives an environment's onEnter action through the executor for an isolated
// task. The action starts suspended under the target account's logon token,
// joins its job object and is resumed by the worker, as a task's process does.
// An environment action's output is not task output, so the action writes the
// account it ran as to a file in the session directory and the task prints it.
func TestIsolationWindowsSystem_EnvironmentActionStartsSuspendedAsTargetUser(t *testing.T) {
	requireHarness(t)
	user := os.Getenv("SQI_TEST_ISOLATION_USER_A")
	logger := slog.New(slog.DiscardHandler)

	provider, err := isolation.NewProvider(isolation.Config{Provider: "logon_user", CredentialStore: harnessStore(t)})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	mgr := session.NewManager(filepath.Join(t.TempDir(), "sessions"), false, provider,
		workerconfig.IsolationConfig{}, fmtres.ExprLimits{}, logger)
	statuses := &statusRecorder{}
	lines := &lineRecorder{}
	ex := executor.New(status.New(statuses, status.Config{WorkerID: "w-isolation"}, logger),
		mgr, metrics.New(), lines, executor.Config{KillGracePeriod: time.Second}, logger)

	msg := &protocol.AssignMsg{
		Version:   protocol.ProtocolVersion,
		Type:      protocol.TypeAssign,
		JobID:     "job-isolated-env",
		TaskID:    "task-isolated-env",
		AttemptID: "attempt-1",
		Environments: []protocol.AssignEnvironment{{
			Name: "identity-env",
			OnEnter: &protocol.Action{
				Command: "cmd",
				Args:    []string{"/c", "whoami > identity.txt"},
			},
		}},
		OnRun:     &protocol.Action{Command: "cmd", Args: []string{"/c", "type identity.txt"}},
		Isolation: &protocol.IsolationSpec{User: user},
	}
	if err := ex.Dispatch(context.Background(), msg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	// The session's cleanup (directory removal, profile unload) runs after
	// the terminal status, so wait for the task goroutine before the test's
	// temporary directories are removed.
	t.Cleanup(func() { ex.DrainAndShutdown(30 * time.Second) })
	got, reason := statuses.waitTerminal(t, time.Minute)
	if got != "succeeded" {
		t.Fatalf("task ended %q (%s), want succeeded; output: %q", got, reason, lines.all())
	}
	var identity string
	for _, l := range lines.all() {
		if s := strings.TrimSpace(l); s != "" {
			identity = s
		}
	}
	if i := strings.LastIndex(identity, `\`); i != -1 {
		identity = identity[i+1:]
	}
	if !strings.EqualFold(identity, user) {
		t.Errorf("environment action reported identity %q, want %q", identity, user)
	}
}

// statusRecorder is the NATS side of a [status.Publisher]: it keeps each
// status a task reports, with its message.
type statusRecorder struct {
	mu       sync.Mutex
	statuses []reportedStatus
}

type reportedStatus struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

func (r *statusRecorder) Publish(_ string, data []byte) error {
	var m reportedStatus
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	r.mu.Lock()
	r.statuses = append(r.statuses, m)
	r.mu.Unlock()
	return nil
}

// PublishAcked records a status report as Publish does and acknowledges it.
func (r *statusRecorder) PublishAcked(_ context.Context, subj string, data []byte, _ string) error {
	return r.Publish(subj, data)
}

// waitTerminal returns the first terminal status reported within timeout,
// and its message.
func (r *statusRecorder) waitTerminal(t *testing.T, timeout time.Duration) (status, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		for _, s := range r.statuses {
			if s.Status == "succeeded" || s.Status == "failed" || s.Status == "canceled" {
				r.mu.Unlock()
				return s.Status, s.Message
			}
		}
		r.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t.Fatalf("no terminal status within %s; saw %v", timeout, r.statuses)
	return "", ""
}

// lineRecorder is an [executor.OutputHandler] that keeps a task's output.
type lineRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *lineRecorder) HandleLine(_ context.Context, _, _, _, _, line string) {
	r.mu.Lock()
	r.lines = append(r.lines, line)
	r.mu.Unlock()
}

func (r *lineRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}
