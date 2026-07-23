// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package isolation

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// TestCredentialClose_ZeroTokenNoOp proves closing a Credential that never
// held a token (the shape a zero-value Credential has — e.g. one built by a
// logon seam that failed before producing anything usable) is a safe no-op,
// matching the contract every other platform's Credential.Close already
// satisfies. newFakeCredential no longer produces this shape itself (it
// carries fakeCredentialToken, precisely so a fake credential can still pass
// through apply's zero-token guard in tests — see that constant's doc), but
// the zero-token shape remains a real one Close must still handle.
func TestCredentialClose_ZeroTokenNoOp(t *testing.T) {
	c := &Credential{}
	if err := c.Close(); err != nil {
		t.Errorf("Close() on a Credential with no token = %v, want nil", err)
	}
}

// TestCredentialClose_IdempotentAfterRealClose is the Windows-specific half
// of the Close-idempotency contract stated in this task's brief: a second
// Close() call after a successful first one must be a no-op, never a
// double-release of the same handle value. windows.Token(0x1234) is not a
// real, currently-open handle, so the first Close() is expected to return a
// syscall error (ERROR_INVALID_HANDLE) from CloseHandle — that is fine and
// not the point of this test; what matters is that the token field is
// zeroed regardless, so the SECOND call takes the zero-token branch instead
// of calling CloseHandle again.
func TestCredentialClose_IdempotentAfterRealClose(t *testing.T) {
	c := &Credential{token: windows.Token(0x1234)}

	_ = c.Close() // first call: attempts a real CloseHandle; return value not asserted

	if c.token != 0 {
		t.Fatalf("token = %#x after first Close, want 0 (idempotency depends on this)", c.token)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close() = %v, want nil (must not re-invoke CloseHandle on a zeroed token)", err)
	}
}

// TestNewProvider_LogonUserIsDefault proves the Windows dispatch in
// newProvider treats both "" and "logon_user" as selecting the logon_user
// mechanism, matching workerconfig's own default (config.Default sets
// Isolation.Provider to "logon_user").
func TestNewProvider_LogonUserIsDefault(t *testing.T) {
	for _, providerName := range []string{"", "logon_user"} {
		p, err := newProvider(Config{Provider: providerName, CredentialStore: fakeStore{}})
		if err != nil {
			t.Fatalf("newProvider(%q): %v", providerName, err)
		}
		if _, ok := p.(*logonUserProvider); !ok {
			t.Errorf("newProvider(%q) = %T, want *logonUserProvider", providerName, p)
		}
	}
}

// TestNewProvider_S4URefusedNotYetImplemented proves selecting "s4u" — an
// unimplemented credential mechanism whose token carries no network
// credentials (see docs/worker-configuration.md, "Windows") — fails closed
// rather than silently falling back to logon_user.
func TestNewProvider_S4URefusedNotYetImplemented(t *testing.T) {
	if _, err := newProvider(Config{Provider: "s4u"}); err == nil {
		t.Error("newProvider(s4u) = nil error, want ErrNotCapable (not yet implemented)")
	}
}

// TestNewProvider_UnknownProviderRejected proves a typo'd provider name is
// rejected rather than silently treated as logon_user.
func TestNewProvider_UnknownProviderRejected(t *testing.T) {
	if _, err := newProvider(Config{Provider: "bogus"}); err == nil {
		t.Error("newProvider(bogus) = nil error, want a rejection")
	}
}

// TestCapableOS_ReportsThePrivilegeCheck proves Capable() now reflects the
// worker's ACTUAL privileges rather than refusing unconditionally.
//
// The two halves of this claim are asserted in different places on purpose:
// here, that a process WITHOUT SeAssignPrimaryTokenPrivilege is refused (the
// go test binary is not SYSTEM), and in the System tier
// (TestIsolationWindowsSystem_Capable) that a process WITH it is accepted. A
// Capable() that always returned nil would pass one and fail the other.
func TestCapableOS_ReportsThePrivilegeCheck(t *testing.T) {
	err := capableOS()

	if err == nil {
		t.Skip("this process holds SeAssignPrimaryTokenPrivilege; the refusal path is covered by the admin tier of make test-isolation-windows")
	}
	if !errors.Is(err, ErrNotCapable) {
		t.Errorf("capableOS() = %v, want an ErrNotCapable-family error", err)
	}
	if !strings.Contains(err.Error(), "LocalSystem") {
		t.Errorf("capableOS() = %q, want it to name the LocalSystem fix", err.Error())
	}
}
