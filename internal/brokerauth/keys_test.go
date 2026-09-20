// SPDX-License-Identifier: AGPL-3.0-or-later

package brokerauth_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nats-io/nkeys"

	"github.com/uberware/sqi/internal/brokerauth"
	"github.com/uberware/sqi/internal/fsutil"
)

func TestGenerateSeed_RoundTrips(t *testing.T) {
	seed, pub, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed: %v", err)
	}
	if !strings.HasPrefix(pub, "U") {
		t.Errorf("public key %q is not a user nkey", pub)
	}
	got, err := brokerauth.PublicKeyFromSeed(seed)
	if err != nil {
		t.Fatalf("PublicKeyFromSeed: %v", err)
	}
	if got != pub {
		t.Errorf("PublicKeyFromSeed = %q, want %q", got, pub)
	}
}

func TestSaveSeed_WritesOwnerOnly(t *testing.T) {
	seed, _, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed: %v", err)
	}
	path := filepath.Join(t.TempDir(), "worker.nk")
	if err := brokerauth.SaveSeed(path, seed); err != nil {
		t.Fatalf("SaveSeed: %v", err)
	}
	assertSecretRestricted(t, path)
}

// assertSecretRestricted asserts that path holds key material only its owner
// can read.
//
// The POSIX mode is still asserted where it is meaningful. On Windows it is
// not: os.Chmod maps only to the read-only ATTRIBUTE and cannot deny read
// access to anyone, so `perm == 0o600` is unsatisfiable there however well the
// file is protected — fsutil.IsRestricted inspects the real DACL instead.
// Asserting the POSIX mode on both platforms is what let the nkey seed ship
// with no confidentiality on Windows while CI stayed green on Linux.
//
// The two sibling tests below still skip on Windows, and correctly so: they
// are about os.WriteFile's create-only mode semantics and LoadSeed's
// reader-side mode check, both of which are genuinely POSIX-specific
// questions rather than this one, which is about the property itself.
func assertSecretRestricted(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", path, perm)
		}
		return
	}
	restricted, err := fsutil.IsRestricted(path)
	if err != nil {
		t.Fatalf("IsRestricted %s: %v", path, err)
	}
	if !restricted {
		t.Errorf("%s is readable beyond its owner", path)
	}
}

// TestSaveSeed_RestrictsAnExistingPermissiveFile no longer skips on Windows:
// since fsutil.WriteSecret, SaveSeed restricts on both platforms, so
// overwriting a permissive leftover must produce a restricted file on both.
// Only the way the "permissive leftover" is CONSTRUCTED differs, and
// os.WriteFile at 0644 constructs it on either — a real mode on POSIX,
// inherited directory access on Windows.
func TestSaveSeed_RestrictsAnExistingPermissiveFile(t *testing.T) {
	seed, _, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed: %v", err)
	}
	path := filepath.Join(t.TempDir(), "worker.nk")

	// Simulate a pre-existing, more permissive file: key rotation over an
	// old seed, or a file an operator chmod'd to inspect. os.WriteFile only
	// applies its perm argument when it creates the file, so writing over an
	// existing 0644 file must not leave it 0644.
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatalf("seed pre-write: %v", err)
	}

	if err := brokerauth.SaveSeed(path, seed); err != nil {
		t.Fatalf("SaveSeed: %v", err)
	}
	assertSecretRestricted(t, path)

	// And the result must be loadable: a restriction the reader then refuses
	// would be worse than the permissive file it replaced.
	if _, err := brokerauth.LoadSeed(path); err != nil {
		t.Errorf("LoadSeed after SaveSeed replaced a permissive file: %v", err)
	}
}

func TestSaveSeed_LeavesNoTempFileBehind(t *testing.T) {
	seed, _, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "worker.nk")
	if err := brokerauth.SaveSeed(path, seed); err != nil {
		t.Fatalf("SaveSeed: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("directory contains %v, want exactly [%s]", names, filepath.Base(path))
	}
	if got := entries[0].Name(); got != filepath.Base(path) {
		t.Errorf("directory contains %q, want %q", got, filepath.Base(path))
	}
}

func TestSaveSeed_NoTempFileSurvivesCreateFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission semantics")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory write permission")
	}

	seed, _, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed: %v", err)
	}
	dir := t.TempDir()
	// Pre-create the seed path with a normal save, then make the directory
	// read-only. A second SaveSeed then fails deterministically at
	// os.CreateTemp (it needs write permission on the directory to create
	// the temp file) before ever touching the existing seed.
	path := filepath.Join(dir, "worker.nk")
	if err := brokerauth.SaveSeed(path, seed); err != nil {
		t.Fatalf("SaveSeed (setup): %v", err)
	}

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("Chmod dir: %v", err)
	}
	//nolint:errcheck // best-effort restore so t.TempDir() can clean up even if the test fails earlier
	defer func() { _ = os.Chmod(dir, 0o700) }()

	seed2, _, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed: %v", err)
	}
	if err := brokerauth.SaveSeed(path, seed2); err == nil {
		t.Fatal("SaveSeed succeeded against a read-only directory; want error")
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("Chmod dir (restore): %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("directory contains %v after a failed save, want exactly [%s] (the untouched original)", names, filepath.Base(path))
	}
}

// TestLoadSeed_RejectsPermissiveSeed proves the reader-side guard on EVERY
// platform.
//
// It no longer skips on Windows. The old version wrote a restricted seed and
// then chmod'd it to 0644 to make it permissive, which is a POSIX-only move —
// os.Chmod on Windows toggles the read-only attribute and cannot widen read
// access to anybody, so there was no way to construct the bad state and the
// test simply skipped. Writing the seed with a plain os.WriteFile constructs it
// on both: 0644 on POSIX, and on Windows a file that inherits its directory's
// access instead of carrying a protected DACL. That is exactly the state every
// Windows seed was in before fsutil.WriteSecret existed.
func TestLoadSeed_RejectsPermissiveSeed(t *testing.T) {
	seed, _, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed: %v", err)
	}
	path := filepath.Join(t.TempDir(), "worker.nk")

	// Deliberately NOT SaveSeed: this is the pre-fix on-disk state, a valid
	// seed written with ordinary inherited access.
	if err := os.WriteFile(path, seed, 0o644); err != nil {
		t.Fatalf("seed pre-write: %v", err)
	}

	_, err = brokerauth.LoadSeed(path)
	if err == nil {
		t.Fatal("LoadSeed accepted a seed readable beyond its owner; want error")
	}
	if !strings.Contains(err.Error(), "readable beyond its owner") {
		t.Errorf("err = %v, want it to name the restriction failure", err)
	}

	// The same bytes must load once the file is properly restricted, so the
	// rejection is about ACCESS and not about the seed itself.
	if err := brokerauth.SaveSeed(path, seed); err != nil {
		t.Fatalf("SaveSeed: %v", err)
	}
	got, err := brokerauth.LoadSeed(path)
	if err != nil {
		t.Fatalf("LoadSeed after SaveSeed restricted it: %v", err)
	}
	if string(got) != string(seed) {
		t.Errorf("seed = %q, want %q", got, seed)
	}
}

func TestValidatePublicKey(t *testing.T) {
	userKP, err := nkeys.CreateUser()
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	validUser, err := userKP.PublicKey()
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}
	seed, err := userKP.Seed()
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}

	accountKP, err := nkeys.CreateAccount()
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	accountKey, err := accountKP.PublicKey()
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}

	// Corrupt the last character of an otherwise-valid user key so its
	// trailing CRC16 no longer checks out, without changing its length or
	// its "U" prefix.
	corrupted := []byte(validUser)
	if last := corrupted[len(corrupted)-1]; last == 'A' {
		corrupted[len(corrupted)-1] = 'B'
	} else {
		corrupted[len(corrupted)-1] = 'A'
	}

	tests := []struct {
		name    string
		pk      string
		wantErr bool
	}{
		{"valid generated user key", validUser, false},
		{"seed instead of a public key", string(seed), true},
		{"account key instead of a user key", accountKey, true},
		{"user key with corrupted CRC", string(corrupted), true},
		{"empty string", "", true},
		{"U-prefixed but not valid base32", "U!!!not-base32!!!", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := brokerauth.ValidatePublicKey(tt.pk)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidatePublicKey(%q) error = %v, wantErr %v", tt.pk, err, tt.wantErr)
			}
		})
	}
}
