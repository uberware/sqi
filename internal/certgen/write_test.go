// SPDX-License-Identifier: AGPL-3.0-or-later

package certgen_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/certgen"
	"github.com/uberware/sqi/internal/fsutil"
)

func TestWriteCA_FileModes(t *testing.T) {
	dir := t.TempDir()
	ca, err := certgen.NewCA("sqi farm CA", time.Hour)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	if err := certgen.WriteCA(dir, ca); err != nil {
		t.Fatalf("WriteCA: %v", err)
	}

	assertPublicMode(t, filepath.Join(dir, "ca.crt"), 0o644)
	assertSecretRestricted(t, filepath.Join(dir, "ca.key"))
}

// assertSecretRestricted asserts that path holds a PRIVATE KEY only its owner
// can read.
//
// The POSIX mode is still asserted where it means something. On Windows it
// does not: os.Chmod maps only to the read-only ATTRIBUTE and cannot deny read
// access to anyone, so `perm == 0o600` is unsatisfiable there however well the
// key is protected — fsutil.IsRestricted inspects the real DACL instead.
// Asserting the POSIX mode on both platforms is exactly what let the farm CA
// key and every leaf key ship with no confidentiality on Windows while CI
// stayed green on Linux. See fsutil.WriteSecret for the full account.
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

// assertPublicMode asserts a CERTIFICATE's mode. A certificate is public by
// design — it goes to every peer in a handshake — so this is a tidiness check,
// not a security one, and it is POSIX-only for the same reason as above:
// Windows has no POSIX mode to assert.
func assertPublicMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode = %04o, want %04o", path, got, want)
	}
}

func TestWriteCA_RefusesToOverwriteExistingCA(t *testing.T) {
	dir := t.TempDir()
	first, err := certgen.NewCA("sqi farm CA", time.Hour)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	if err := certgen.WriteCA(dir, first); err != nil {
		t.Fatalf("WriteCA (first): %v", err)
	}
	original, err := os.ReadFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatalf("read ca.key: %v", err)
	}

	second, err := certgen.NewCA("sqi farm CA", time.Hour)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	err = certgen.WriteCA(dir, second)
	if !errors.Is(err, certgen.ErrCAExists) {
		t.Fatalf("WriteCA (second) error = %v, want ErrCAExists", err)
	}

	after, err := os.ReadFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatalf("re-read ca.key: %v", err)
	}
	if string(after) != string(original) {
		t.Error("ca.key was modified despite the refusal; a replaced farm CA invalidates every certificate issued from it")
	}
}

func TestWriteLeaf_FileModes(t *testing.T) {
	dir := t.TempDir()
	ca, err := certgen.NewCA("sqi farm CA", time.Hour)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	leaf, err := ca.NewServerCert([]string{"sqi.example"}, time.Hour)
	if err != nil {
		t.Fatalf("NewServerCert: %v", err)
	}
	if err := certgen.WriteLeaf(dir, "server", leaf); err != nil {
		t.Fatalf("WriteLeaf: %v", err)
	}

	assertPublicMode(t, filepath.Join(dir, "server.crt"), 0o644)
	assertSecretRestricted(t, filepath.Join(dir, "server.key"))
}
