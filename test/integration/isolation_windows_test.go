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
	"strings"
	"testing"

	"golang.org/x/sys/windows"
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
