// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestKeygenCmd_WritesSeedAndPrintsEnrollCommand verifies that "keygen"
// writes a 0600 seed file, prints the public key, and prints the exact
// "sqi-server worker enroll" command to run — but never prints the seed.
func TestKeygenCmd_WritesSeedAndPrintsEnrollCommand(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "worker-data")

	prepareRoot([]string{"keygen", "--data-dir", dataDir})
	out := captureStdout(t, func() {
		if err := Execute(); err != nil {
			t.Fatalf("Execute(keygen) error = %v", err)
		}
	})

	if !strings.Contains(out, "Public key: U") {
		t.Errorf("output missing public key line; got:\n%s", out)
	}
	if !strings.Contains(out, "sqi-server worker enroll --worker-id") {
		t.Errorf("output missing the enroll command; got:\n%s", out)
	}
	if !strings.Contains(out, "--public-key U") {
		t.Errorf("output missing --public-key flag; got:\n%s", out)
	}

	seedPath := filepath.Join(dataDir, "worker.nk")
	info, err := os.Stat(seedPath)
	if err != nil {
		t.Fatalf("stat seed file: %v", err)
	}

	seedBytes, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatalf("read seed file: %v", err)
	}
	if strings.Contains(out, string(seedBytes)) {
		t.Error("stdout output must never contain the seed bytes")
	}

	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("seed file mode = %o; want 0600", perm)
		}
	}
}

// TestKeygenCmd_RefusesToOverwriteWithoutForce verifies that a second
// keygen invocation against the same data directory fails without --force,
// and succeeds (overwriting the seed) with it.
func TestKeygenCmd_RefusesToOverwriteWithoutForce(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "worker-data")

	prepareRoot([]string{"keygen", "--data-dir", dataDir})
	_ = captureStdout(t, func() {
		if err := Execute(); err != nil {
			t.Fatalf("first keygen: unexpected error: %v", err)
		}
	})

	seedPath := filepath.Join(dataDir, "worker.nk")
	before, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatalf("read seed after first keygen: %v", err)
	}

	prepareRoot([]string{"keygen", "--data-dir", dataDir})
	var secondErr error
	_ = captureStdout(t, func() {
		secondErr = Execute()
	})
	if secondErr == nil {
		t.Fatal("expected keygen without --force to fail when a seed already exists")
	}

	after, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatalf("read seed after refused overwrite: %v", err)
	}
	if string(before) != string(after) {
		t.Error("seed file changed despite the overwrite being refused")
	}

	prepareRoot([]string{"keygen", "--data-dir", dataDir, "--force"})
	out := captureStdout(t, func() {
		if err := Execute(); err != nil {
			t.Fatalf("forced keygen: unexpected error: %v", err)
		}
	})
	if !strings.Contains(out, "Public key: U") {
		t.Errorf("forced keygen output missing public key line; got:\n%s", out)
	}

	forced, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatalf("read seed after forced overwrite: %v", err)
	}
	if string(before) == string(forced) {
		t.Error("seed file did not change after --force")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(seedPath)
		if err != nil {
			t.Fatalf("stat seed after forced overwrite: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("seed file mode after --force = %o; want 0600", perm)
		}
	}
}

// TestKeygenCmd_EmptyDataDir verifies the validation guard fires directly.
func TestKeygenCmd_EmptyDataDir(t *testing.T) {
	origDir := keygenFlags.DataDir
	t.Cleanup(func() { keygenFlags.DataDir = origDir })

	keygenFlags.DataDir = ""
	err := runKeygen(nil, nil)
	if err == nil {
		t.Fatal("expected an error for an empty --data-dir, got nil")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error should mention 'empty'; got: %v", err)
	}
}
