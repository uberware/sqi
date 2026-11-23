// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package isolation

import (
	"os"
	"testing"
)

// The riskiest code in this package, dpapiProtect/dpapiUnprotect
// (unsafe.Slice over an OS-allocated buffer with a deferred LocalFree) and
// writeSecured/secureDir (real SetSecurityInfo calls), is otherwise covered
// only by
// TestIsolationWindows_CredentialRoundTrip
// (test/integration/isolation_windows_test.go), which needs
// SQI_TEST_ISOLATION_WINDOWS=1 and an elevation harness and therefore never
// runs on an ordinary `go test`. A DPAPI round trip with
// CRYPTPROTECT_LOCAL_MACHINE needs no special privilege — any local account
// can protect/unprotect its own machine-scope blobs — so that much runs
// unprivileged. A protected admin-only DACL is different: NTFS grants an
// object's owner no implicit access beyond what its DACL says (see
// adminOnlyDACL's doc in acl_windows.go for how secureDir/writeSecured
// handle that), so an unprivileged process cannot read a file Put has locked
// down. Each test's doc says how it executes these primitives without that
// step.

// TestDpapiRoundTrip executes dpapiProtect and dpapiUnprotect end to end
// rather than only asserting a call returned no error.
func TestDpapiRoundTrip(t *testing.T) {
	const want = "correct horse battery staple"

	blob, err := dpapiProtect([]byte(want))
	if err != nil {
		t.Fatalf("dpapiProtect: %v", err)
	}
	if len(blob) == 0 {
		t.Fatal("dpapiProtect returned an empty blob for non-empty plaintext")
	}

	got, err := dpapiUnprotect(blob)
	if err != nil {
		t.Fatalf("dpapiUnprotect: %v", err)
	}
	if string(got) != want {
		t.Errorf("round trip = %q, want %q", got, want)
	}
}

// TestDpapiProtect_EmptyInput exercises the len(x) > 0 guard in
// blobDataPointer via dpapiProtect's zero-length path: without the guard,
// `&plain[0]` on a nil slice panics before CryptProtectData is ever called.
// Protecting an empty secret still produces a real, non-empty DPAPI
// envelope (CryptProtectData rejects a NULL pbData even when cbData is 0,
// which the guard's fallback pointer avoids),
// so the full round trip back through dpapiUnprotect must still recover an
// empty plaintext with no error.
func TestDpapiProtect_EmptyInput(t *testing.T) {
	blob, err := dpapiProtect(nil)
	if err != nil {
		t.Fatalf("dpapiProtect(nil): %v", err)
	}
	if len(blob) == 0 {
		t.Fatal("dpapiProtect(nil) returned an empty blob; DPAPI envelopes always carry overhead")
	}

	got, err := dpapiUnprotect(blob)
	if err != nil {
		t.Fatalf("dpapiUnprotect: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("round trip of empty input = %q, want empty", got)
	}
}

// TestDpapiUnprotect_EmptyBlobDoesNotPanic exercises the same guard from the
// decrypt side directly. dpapiProtect never itself produces a zero-length
// blob (see above), so this path is reached only for a corrupted or
// truncated on-disk credential file; the guard's job is only to turn what
// would otherwise be a slice-index panic into a real, reportable error —
// CryptUnprotectData legitimately has nothing valid to decrypt from zero
// bytes, so an error here (not a panic, and not a false "empty secret") is
// the correct outcome.
func TestDpapiUnprotect_EmptyBlobDoesNotPanic(t *testing.T) {
	if _, err := dpapiUnprotect(nil); err == nil {
		t.Error("dpapiUnprotect(nil) = nil error, want a failure decrypting zero bytes")
	}
}

// The directory- and file-ACL shape Put() produces is asserted in
// TestIsolationWindows_CredentialDirectoryExcludesUnprivilegedTrustees
// (test/integration/isolation_windows_test.go), not here: adminOnlyDACL
// (acl_windows.go) grants the isolation directory to SYSTEM and
// Administrators only — no CREATOR OWNER placeholder standing in for whoever
// happens to create it — so an unelevated process cannot complete Put() at
// all: os.WriteFile against a
// SYSTEM+Administrators-only directory fails for anyone without an enabled
// Administrators SID in their token, which an ordinary `go test` run does
// not have. That is intended (Put's real caller is always an elevated
// Administrator, so nothing legitimate is lost), but it does mean the
// assertion belongs in the elevated tier alongside the rest of the isolation
// integration suite.

// TestFileStore_SecretDecryptsWhatPutWrites proves Secret() is wired
// correctly to dpapiUnprotect end to end: given a real DPAPI blob produced
// by the exact dpapiProtect call Put itself uses, written under Put's own
// naming scheme (store.path) at the location Put itself would write it,
// Secret() reads it back and decrypts it to the original secret.
//
// It does not call Put() first: an unprivileged Put() cannot complete,
// because adminOnlyDACL locks the directory to SYSTEM and Administrators
// (see the file-level doc comment above), and
// TestIsolationWindows_CredentialDirectoryExcludesUnprivilegedTrustees
// (test/integration/isolation_windows_test.go) checks in the elevated tier
// that Put restricts the file it writes to SYSTEM and Administrators, even
// against the account that created it (NTFS grants an object's owner no
// implicit access beyond what its DACL says). Reading a Put-produced file
// from this unprivileged process would need elevation, which this tier must
// not do, or access the ACL exists to deny. Writing the same bytes without
// Put's DACL tests Secret()'s read-and-decrypt path on its own; the ACL is
// tested elsewhere.
func TestFileStore_SecretDecryptsWhatPutWrites(t *testing.T) {
	dir := t.TempDir()
	store, ok := NewFileStore(dir).(*fileStore)
	if !ok {
		t.Fatal("NewFileStore on windows must return *fileStore")
	}
	const user = "render-svc"
	const want = "correct horse battery staple"

	blob, err := dpapiProtect([]byte(want))
	if err != nil {
		t.Fatalf("dpapiProtect: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(store.path(user), blob, 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", store.path(user), err)
	}

	got, err := store.Secret(user)
	if err != nil {
		t.Fatalf("Secret: %v", err)
	}
	if got != want {
		t.Errorf("Secret = %q, want %q", got, want)
	}
}
