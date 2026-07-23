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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"

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
