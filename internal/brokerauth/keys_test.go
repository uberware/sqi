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
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}

func TestSaveSeed_FixesModeOfExistingFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits")
	}
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
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
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

func TestSaveSeed_NoTempFileSurvivesRenameFailure(t *testing.T) {
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

func TestLoadSeed_RejectsPermissiveMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits")
	}
	seed, _, err := brokerauth.GenerateSeed()
	if err != nil {
		t.Fatalf("GenerateSeed: %v", err)
	}
	path := filepath.Join(t.TempDir(), "worker.nk")
	if err := brokerauth.SaveSeed(path, seed); err != nil {
		t.Fatalf("SaveSeed: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	if _, err := brokerauth.LoadSeed(path); err == nil {
		t.Error("LoadSeed accepted a world-readable seed file; want error")
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
