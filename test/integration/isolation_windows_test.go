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
// and the LOCAL/NETWORK SERVICE accounts hold it — an elevated Administrator
// does NOT. So anything asserting that a child really ran as the target user
// belongs in the System tier, which scripts/test-isolation-windows.ps1 runs
// via a scheduled task with /ru SYSTEM.
//
// Everything else — ACL shape, credential storage, and even real impersonated
// access checks (LogonUserW needs no privilege for a local account with a
// password; ImpersonateLoggedOnUser needs only SeImpersonatePrivilege, which
// administrators hold) — belongs in the fast tier.
package integration

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/uberware/sqi/internal/worker/envutil"
	"github.com/uberware/sqi/internal/worker/isolation"
)

// requireHarness skips unless the PowerShell harness set up the throwaway
// accounts. A bare `make test-integration` on a developer box must never try
// to create OS accounts.
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
// the scheduled-task plumbing silently ran the binary as the invoking
// administrator instead of SYSTEM, every other System-tier assertion would
// fail for the wrong reason — or, worse, a privilege-dependent test could be
// skipped and read as a pass. This asserts the premise directly.
func TestIsolationWindowsSystem_RunsAsSystem(t *testing.T) {
	requireHarness(t)

	got := currentAccountName(t)

	if !strings.EqualFold(got, `NT AUTHORITY\SYSTEM`) {
		t.Fatalf("running as %q, want NT AUTHORITY\\SYSTEM — the scheduled task did not elevate to SYSTEM", got)
	}
}

// TestIsolationWindows_CredentialRoundTrip proves an operator-provisioned
// secret is readable by the worker: encrypt with the machine key, write with
// a locked-down ACL, read it back. Runs in the admin tier because DPAPI needs
// no special privilege.
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

// TestIsolationWindows_CredentialMissingIsActionable proves an operator who
// never ran set-credential gets a message naming the fix, not "empty secret".
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

// TestIsolationWindows_CredentialDirectoryExcludesUnprivilegedTrustees proves
// finding 1 is actually fixed: after Put, the credential directory and the
// file it wrote both carry a protected DACL that excludes every unprivileged
// trustee — BUILTIN\Users above all, which a ProgramData-rooted parent tree
// would otherwise grant. It reads the on-disk DACL back with
// windows.GetNamedSecurityInfo, independently of the
// openForACL/applyProtectedDACL primitives Put itself used to write it, so a
// bug in those primitives can't hide from this assertion the way it could if
// the test reused the same code path to verify itself.
//
// This runs in the elevated tier, not alongside the package's other unit
// tests (internal/worker/isolation/credstore_windows_test.go), because it no
// longer can: adminOnlyDACL (acl_windows.go) now grants the credential
// directory to SYSTEM and Administrators only — no CREATOR OWNER placeholder
// standing in for whoever happens to create it — so an unelevated process
// cannot complete Put() at all. That is deliberate: the documented,
// sanctioned caller of set-credential is always a genuinely elevated
// Administrator, so nothing legitimate is lost, and it is exactly what makes
// "run from an elevated shell" an enforced control rather than a
// documentation-only convention.
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
	// name (credFileName, internal/worker/isolation/credstore.go) — not
	// reproduced here, deliberately: this test discovers the file the same
	// way an operator would, by listing the directory Put just secured,
	// rather than depending on an unexported naming scheme this package
	// does not own.
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

		// 4, not 2: SetSecurityInfo canonicalizes each "this folder,
		// subfolders, and files" ACE applied to a container into a direct
		// ACE (applies to the directory itself) plus a separate
		// INHERIT_ONLY ACE (applies only to future children) — so SYSTEM
		// and Administrators each appear twice. Nothing else: no CREATOR
		// OWNER, no direct grant for whoever's Put call is actually
		// running — that is exactly what finding 1's fix removes.
		if got := aceCount(t, acl); got != 4 {
			t.Errorf("directory ACL has %d ACEs, want exactly 4 (SYSTEM and Administrators, each split direct+inherit-only)", got)
		}
		if !hasSID(t, acl, system) {
			t.Error("directory ACL must grant SYSTEM, or the worker service cannot re-secure it")
		}
		if !hasSID(t, acl, admins) {
			t.Error("directory ACL must grant Administrators, or an operator is locked out")
		}
		if hasSID(t, acl, users) {
			t.Error("directory ACL must not grant BUILTIN\\Users — that is the credential-disclosure finding 1 fixes")
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
			t.Error("file ACL must not grant BUILTIN\\Users — that is the credential-disclosure finding 1 fixes")
		}
	})
}

// readDACL fetches path's discretionary ACL straight from the filesystem via
// GetNamedSecurityInfo, the same Win32 call `icacls` itself is built on.
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

// aceCount reports how many access-allowed entries acl carries.
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
// simply not seeded — callers that only need one of the two accounts still
// get a usable store.
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
// via the real logon_user Provider, backed by harnessStore. This is
// deliberate — see the "No test-only exported API" note in this package's
// history: it lets the strongest assertions below call the production
// SecureWorkDir/ChownRecursive with a credential obtained exactly the way a
// real worker would (through Resolve), rather than a hand-built value that
// only Task 4's own code would ever construct.
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

// advapi32.dll!LogonUserW has no wrapper in golang.org/x/sys/windows —
// checked against the version this module depends on (v0.47.0), the same way
// internal/worker/isolation/provider_windows.go already had to for the same
// call: neither LogonUserW nor a `LogonUser` Go wrapper is exposed there.
// This package cannot reach that package's unexported logonUserW seam (it is
// a different package, and that seam is deliberately not exported — see "No
// test-only exported API" above), so it is declared directly here instead,
// the same way golang.org/x/sys/windows's own generated zsyscall_windows.go
// declares every Win32 call it does wrap.
// advapi32.dll!ImpersonateLoggedOnUser is likewise unwrapped by
// golang.org/x/sys/windows v0.47.0 — that package wraps RevertToSelf and
// ImpersonateSelf, but not this call — so it is declared alongside
// LogonUserW below for the same reason.
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
// internal/worker/isolation's own logonUserOS uses, so impersonate below
// authenticates the harness accounts exactly the way a real worker would.
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
// administrators hold — which is what lets the strongest ACL assertions live
// in the fast tier instead of behind the scheduled task.
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

// TestIsolationWindows_SessionDirACLIsProtected proves the ACL was not just
// BUILT correctly but APPLIED correctly. secureDACL's unit test covers the
// three entries; what it cannot show is that applyProtectedDACL actually
// stripped inheritance on a real directory. Without the protected flag the
// session would keep whatever its parent grants — typically BUILTIN\Users —
// and the three explicit entries would be an addition rather than the whole
// story, leaving every session readable farm-wide while every structural test
// still passed.
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
	// golang.org/x/sys/windows v0.47.0 has no GetAclInformation wrapper (see
	// aceCount's own doc, below in this file) — windows.ACL already exports
	// AceCount as a plain struct field, so that is read directly instead.
	if got := aceCount(t, dacl); got != 3 {
		t.Errorf("applied DACL has %d ACEs, want exactly 3 (%s, SYSTEM, Administrators)", got, user)
	}
}

// TestIsolationWindows_SecondAccountCannotOpenSessionDir is the real access
// check the structural DACL test cannot make: account B, actually
// impersonated, must be denied a session directory secured for account A.
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

// TestIsolationWindows_CredentialFileNotReadableByRunAsAccount proves the
// claim the credential store's own doc makes: machine-scope DPAPI is
// decryptable by anything on this host that can READ the file, so the file
// ACL is the real boundary. A task must not be able to recover the password
// of the account it runs as.
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

// TestIsolationWindows_ChownRecursiveDoesNotFollowJunction proves the
// reparse-point guard. A task that plants a junction inside its session
// directory pointing at a directory it does not own must not have sqi grant
// its own account full control of the target.
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
	if out, err := exec.CommandContext(context.Background(), "cmd", "/c", "mklink", "/J", link, outside).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v: %s", err, out)
	}

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

// TestIsolationWindows_CapableRejectsPlainAdmin proves the privilege check is
// real rather than vacuous. An elevated Administrator does NOT hold
// SeAssignPrimaryTokenPrivilege, so Capable() must refuse here — and the
// System tier's counterpart test proves it accepts when the privilege IS
// present. Without both halves, a Capable() that returned nil unconditionally
// would look correct.
func TestIsolationWindows_CapableRejectsPlainAdmin(t *testing.T) {
	requireHarness(t)

	p, err := isolation.NewProvider(isolation.Config{Provider: "logon_user"})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	if err := p.Capable(); err == nil {
		t.Error("Capable() = nil as a plain administrator, want ErrNotCapable — administrators do not hold SeAssignPrimaryTokenPrivilege")
	}
}

// TestIsolationWindowsSystem_ProfileEnvPointsAtTargetUser proves the loaded
// profile is real: the child's USERPROFILE resolves under the target
// account's own directory, not the daemon's. Without LoadUserProfile this
// would be SYSTEM's profile under System32\config\systemprofile, and every
// DCC would write its preferences there.
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

// TestIsolationWindowsSystem_Capable is the other half of
// TestCapableOS_ReportsThePrivilegeCheck: as SYSTEM, the worker holds
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
// can make: the process really started under the target account's identity.
func TestIsolationWindowsSystem_ChildRunsAsTargetUser(t *testing.T) {
	requireHarness(t)
	user := os.Getenv("SQI_TEST_ISOLATION_USER_A")

	cred := resolveHarnessCredential(t, user)
	defer cred.Close() // test cleanup

	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "echo %USERNAME%")
	if err := isolation.Apply(cmd, cred); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run as %s: %v", user, err)
	}

	if got := strings.TrimSpace(string(out)); !strings.EqualFold(got, user) {
		t.Errorf("child reported USERNAME=%q, want %q", got, user)
	}
}

// TestIsolationWindowsSystem_ChildCanWriteSessionDir catches the failure the
// structural ACL test cannot: an ACL that is correct in shape but too tight
// in practice, leaving the task unable to write its own scratch.
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

// TestIsolationWindowsSystem_DaemonEnvNotInherited proves the environment
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
// tier's impersonated check as REAL PROCESSES, which is what actually ships.
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

// TestIsolationWindowsSystem_NoIsolationUnchanged is the regression invariant:
// with no credential, a task runs as the daemon with the full inherited
// environment, exactly as before this feature existed.
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
