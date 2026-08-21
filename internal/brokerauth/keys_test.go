// SPDX-License-Identifier: AGPL-3.0-or-later

package brokerauth_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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
