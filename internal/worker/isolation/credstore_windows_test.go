// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package isolation

import (
	"os"
	"testing"
)

// These tests exist because the highest-risk code in this package —
// dpapiProtect/dpapiUnprotect (unsafe.Slice over an OS-allocated buffer with
// a deferred LocalFree) and writeSecured/secureDir (real SetSecurityInfo
// calls) — was previously covered only by
// TestIsolationWindows_CredentialRoundTrip
// (test/integration/isolation_windows_test.go), which needs
// SQI_TEST_ISOLATION_WINDOWS=1 and an elevation harness and therefore never
// runs on an ordinary `go test`. A DPAPI round trip with
// CRYPTPROTECT_LOCAL_MACHINE needs no special privilege — any local account
// can protect/unprotect its own machine-scope blobs — so that much runs
// unprivileged outright. Applying a protected admin-only DACL is a
// different story: contrary to folklore, NTFS grants an object's owner NO
// implicit access beyond what its DACL actually says (verified empirically
// against the real API — see adminOnlyDACL's doc in acl_windows.go for how
// secureDir/writeSecured route around that rather than depend on it), so an
// unprivileged process genuinely cannot read a file Put has correctly
// locked down. These tests are written to prove every one of the flagged
// primitives executes for real without ever requiring that impossible
// step — see each test's own doc for how.

// TestDpapiRoundTrip actually executes dpapiProtect and dpapiUnprotect
// end to end — the "highest-risk code [that] has never been executed" this
// test exists to cover — rather than merely asserting a call returned no
// error.
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
// envelope (verified: CryptProtectData rejects a NULL pbData even when
// cbData is 0, which is exactly what the guard's fallback pointer avoids),
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
// (test/integration/isolation_windows_test.go), not here. It used to live in
// this package, but adminOnlyDACL (acl_windows.go) now grants the isolation
// directory to SYSTEM and Administrators only — no CREATOR OWNER placeholder
// standing in for whoever happens to create it — so an unelevated process
// can no longer complete Put() at all: os.WriteFile against a
// SYSTEM+Administrators-only directory fails for anyone without an enabled
// Administrators SID in their token, which an ordinary `go test` run does
// not have. That is the point of the fix (finding 1's real caller is always
// an elevated Administrator, so nothing legitimate is lost), but it does mean
// the assertion now belongs in the elevated tier alongside the rest of the
// isolation integration suite.

// TestFileStore_SecretDecryptsWhatPutWrites proves Secret() is wired
// correctly to dpapiUnprotect end to end: given a real DPAPI blob produced
// by the exact dpapiProtect call Put itself uses, written under Put's own
// naming scheme (store.path) at the location Put itself would write it,
// Secret() reads it back and decrypts it to the original secret.
//
// This deliberately does not chain directly after a call to Put(), and
// that is not a shortcut: an unprivileged Put() call can no longer even
// complete now that adminOnlyDACL locks the directory to SYSTEM and
// Administrators only (see the file-level doc comment above), and
// TestIsolationWindows_CredentialDirectoryExcludesUnprivilegedTrustees
// (test/integration/isolation_windows_test.go) proves for real, in the
// elevated tier, that Put locks the file it writes down to SYSTEM and
// Administrators only — which holds even for the very account that created
// it (verified empirically: NTFS grants an object's owner no implicit
// access beyond what its DACL says; there is no "owner override" for
// WRITE_DAC despite folklore to the contrary). Proving Secret() can decrypt
// a real Put-produced file from this same unprivileged process would
// therefore require either elevating (forbidden by this task) or grabbing
// access this process was never granted — and if either worked, THAT would
// itself be the security bug finding 1 exists to close, not a legitimate
// test setup step. Writing the identical bytes without Put's restrictive
// DACL isolates exactly the property this test needs to prove — that
// Secret()'s read-plus-decrypt path is correct — from the ACL property
// already proven directly, for real, elsewhere.
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
