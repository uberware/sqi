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
// held a token (the shape newFakeCredential produces, and the shape a
// zero-value Credential has) is a safe no-op, matching the contract every
// other platform's Credential.Close already satisfies.
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

// TestNewProvider_S4URefusedNotYetImplemented proves selecting "s4u" (Task
// 10's mechanism) fails closed rather than silently falling back to
// logon_user.
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

// TestCapableOS_AlwaysReportsACLNotImplemented proves the REAL production
// Capable() seam (not a test-injected fake — see newProvider above, which is
// how a real worker actually constructs its Provider) always reports the
// shared, operator-facing windowsIsolationUnsupportedMsg, regardless of
// whether this test process's own token happens to hold every privilege
// hasRequiredPrivileges checks for. That determinism is the whole point: a
// Windows worker must never look capable just because CreateProcessAsUser's
// prerequisites are satisfied — session directories now get real NTFS ACLs
// (workdir_windows.go), but profile loading, job-object process reaping, and
// end-to-end verification are still missing, so isolation as a whole cannot
// work either way, and Capable() must say so plainly rather than reporting
// ready and failing later, confusingly, inside Manager.Create (see that
// message's own doc for the full rationale).
func TestCapableOS_AlwaysReportsACLNotImplemented(t *testing.T) {
	p, err := newProvider(Config{Provider: "logon_user", CredentialStore: fakeStore{}})
	if err != nil {
		t.Fatalf("newProvider: %v", err)
	}

	err = p.Capable()
	if !errors.Is(err, ErrNotCapable) {
		t.Fatalf("Capable() = %v, want ErrNotCapable", err)
	}
	if !strings.Contains(err.Error(), windowsIsolationUnsupportedMsg) {
		t.Errorf("Capable() = %q, want it to contain the shared operator-facing message %q",
			err.Error(), windowsIsolationUnsupportedMsg)
	}
}
