// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package isolation

import (
	"errors"
	"strings"
	"testing"
)

// TestSecureWorkDir_NilCredentialIsNoOp proves the pre-isolation path (no
// queue-configured run-as-user, so resolveCredential never calls Resolve) is
// completely unaffected by Windows isolation being unsupported: SecureWorkDir
// must stay a no-op for a nil credential exactly as it always has.
func TestSecureWorkDir_NilCredentialIsNoOp(t *testing.T) {
	if err := SecureWorkDir(`C:\does\not\matter`, nil); err != nil {
		t.Errorf("SecureWorkDir(nil) = %v, want nil", err)
	}
}

// TestSecureWorkDir_NonNilCredentialReportsACLNotImplemented proves the
// per-assignment path: once session.Manager.resolveCredential has already
// obtained a real credential via the logon_user provider's Resolve (which
// works — see provider_windows.go), SecureWorkDir is what actually refuses
// the request, deterministically and with the same operator-facing message
// Capable() uses at boot (see windowsIsolationUnsupportedMsg's own doc) —
// never something obscure about a work directory.
func TestSecureWorkDir_NonNilCredentialReportsACLNotImplemented(t *testing.T) {
	cred := newFakeCredential(FakeAccount{Home: `C:\Users\render-svc`})

	err := SecureWorkDir(`C:\does\not\matter`, cred)

	if !errors.Is(err, ErrNotCapable) {
		t.Fatalf("SecureWorkDir(non-nil cred) = %v, want ErrNotCapable", err)
	}
	if !strings.Contains(err.Error(), windowsIsolationUnsupportedMsg) {
		t.Errorf("SecureWorkDir(non-nil cred) = %q, want it to contain %q", err.Error(), windowsIsolationUnsupportedMsg)
	}
}

// TestChownRecursive_NilCredentialIsNoOp mirrors
// TestSecureWorkDir_NilCredentialIsNoOp for ChownRecursive.
func TestChownRecursive_NilCredentialIsNoOp(t *testing.T) {
	if err := ChownRecursive(`C:\does\not\matter`, nil); err != nil {
		t.Errorf("ChownRecursive(nil) = %v, want nil", err)
	}
}

// TestChownRecursive_NonNilCredentialReportsACLNotImplemented mirrors
// TestSecureWorkDir_NonNilCredentialReportsACLNotImplemented for
// ChownRecursive (used by internal/worker/staging for stage_locally scratch,
// not the session working directory, but backed by the same missing NTFS ACL
// support and so refusing with the same message).
func TestChownRecursive_NonNilCredentialReportsACLNotImplemented(t *testing.T) {
	cred := newFakeCredential(FakeAccount{Home: `C:\Users\render-svc`})

	err := ChownRecursive(`C:\does\not\matter`, cred)

	if !errors.Is(err, ErrNotCapable) {
		t.Fatalf("ChownRecursive(non-nil cred) = %v, want ErrNotCapable", err)
	}
	if !strings.Contains(err.Error(), windowsIsolationUnsupportedMsg) {
		t.Errorf("ChownRecursive(non-nil cred) = %q, want it to contain %q", err.Error(), windowsIsolationUnsupportedMsg)
	}
}

// TestValidateTraversable_AlwaysNoOp proves ValidateTraversable never blocks
// boot on Windows regardless of arguments: there is no POSIX-style ancestor
// traversable bit to check on NTFS, and a Windows worker that sets
// isolation.required is already refused earlier, by Capable() — see that
// function's own doc (provider_windows.go's capableOS) and
// TestCapableOS_AlwaysReportsACLNotImplemented.
func TestValidateTraversable_AlwaysNoOp(t *testing.T) {
	if err := ValidateTraversable(`C:\some\narrow\path`, `C:\another\one`); err != nil {
		t.Errorf("ValidateTraversable = %v, want nil (always a no-op on Windows)", err)
	}
}
