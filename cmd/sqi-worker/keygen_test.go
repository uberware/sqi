// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	workerconfig "github.com/uberware/sqi/internal/worker/config"
)

// writeWorkerConfigFile writes a minimal sqi-worker config file setting
// worker.data_dir and returns its path.
func writeWorkerConfigFile(t *testing.T, dataDir string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sqi-worker.yaml")
	content := "worker:\n  data_dir: " + dataDir + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}
	return path
}

// captureStderr redirects os.Stderr to a pipe for the duration of fn, then
// returns everything written to it. keygen's overwrite warning is
// deliberately written to stderr (see runKeygen), so asserting on it needs
// this alongside captureStdout — both streams are written within the same
// call and must be captured together, not sequentially.
//
// Must NOT be called from parallel sub-tests — the redirect is process-wide,
// same caveat as captureStdout in main_test.go.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w

	fn()

	w.Close()
	os.Stderr = old

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("io.Copy from stderr pipe: %v", err)
	}
	r.Close()
	return buf.String()
}

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
	if !strings.Contains(out, "will not accept this credential until it restarts") {
		t.Errorf("output missing the running-server restart note; got:\n%s", out)
	}
	if !strings.Contains(out, "POST /api/v1/workers/enroll") {
		t.Errorf("output missing the join-token REST enrollment alternative; got:\n%s", out)
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
	var out string
	errOut := captureStderr(t, func() {
		out = captureStdout(t, func() {
			if err := Execute(); err != nil {
				t.Fatalf("forced keygen: unexpected error: %v", err)
			}
		})
	})
	if !strings.Contains(out, "Public key: U") {
		t.Errorf("forced keygen output missing public key line; got:\n%s", out)
	}

	// The overwrite warning belongs where an operator who already passed
	// --force will actually see it (stderr, on success) — not only in
	// --help text or in the refusal message shown when --force is absent.
	workerID, err := workerconfig.LoadOrCreateWorkerID(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateWorkerID: %v", err)
	}
	if !strings.Contains(errOut, "Warning: this replaced an existing seed") {
		t.Errorf("forced keygen stderr missing the overwrite warning; got:\n%s", errOut)
	}
	wantRevokeCmd := "sqi-server worker revoke " + workerID
	if !strings.Contains(errOut, wantRevokeCmd) {
		t.Errorf("forced keygen stderr missing the exact revoke command %q; got:\n%s", wantRevokeCmd, errOut)
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

// TestKeygenCmd_EmptyDataDir verifies that an explicitly-passed empty
// --data-dir (as opposed to the flag simply being omitted, which falls
// through to worker.data_dir) is rejected with a descriptive error. Routed
// through Execute() rather than calling runKeygen directly: with no cobra
// command, --data-dir cannot be told apart from "omitted", which would fall
// through to the real, config-resolved worker.data_dir instead of the empty
// value this test means to exercise.
func TestKeygenCmd_EmptyDataDir(t *testing.T) {
	prepareRoot([]string{"keygen", "--data-dir", ""})
	var runErr error
	_ = captureStdout(t, func() {
		runErr = Execute()
	})
	if runErr == nil {
		t.Fatal("expected an error for an empty --data-dir, got nil")
	}
	if !strings.Contains(runErr.Error(), "empty") {
		t.Errorf("error should mention 'empty'; got: %v", runErr)
	}
}

// TestKeygenCmd_DataDir_ExplicitFlagBeatsConfig verifies that --data-dir
// wins even when a config file names a different worker.data_dir.
func TestKeygenCmd_DataDir_ExplicitFlagBeatsConfig(t *testing.T) {
	explicitDir := filepath.Join(t.TempDir(), "explicit-data")
	configuredDir := filepath.Join(t.TempDir(), "configured-data")
	cfgPath := writeWorkerConfigFile(t, configuredDir)
	t.Cleanup(func() { persistentFlags.ConfigFile = "" })

	prepareRoot([]string{"keygen", "--config", cfgPath, "--data-dir", explicitDir})
	_ = captureStdout(t, func() {
		if err := Execute(); err != nil {
			t.Fatalf("keygen: unexpected error: %v", err)
		}
	})

	if _, err := os.Stat(filepath.Join(explicitDir, "worker.nk")); err != nil {
		t.Errorf("expected a seed under the explicit --data-dir %s: %v", explicitDir, err)
	}
	if _, err := os.Stat(configuredDir); err == nil {
		t.Errorf("keygen wrote under the configured worker.data_dir %s despite an explicit --data-dir", configuredDir)
	}
}

// TestKeygenCmd_DataDir_ConfigFileHonoredWhenFlagOmitted verifies that
// omitting --data-dir resolves worker.data_dir through the config layer
// rather than the platform default under the real home directory.
func TestKeygenCmd_DataDir_ConfigFileHonoredWhenFlagOmitted(t *testing.T) {
	withFlagUnchanged(t, keygenCmd.Flags(), "data-dir")

	configuredDir := filepath.Join(t.TempDir(), "configured-data")
	cfgPath := writeWorkerConfigFile(t, configuredDir)
	t.Cleanup(func() { persistentFlags.ConfigFile = "" })

	prepareRoot([]string{"keygen", "--config", cfgPath})
	_ = captureStdout(t, func() {
		if err := Execute(); err != nil {
			t.Fatalf("keygen: unexpected error: %v", err)
		}
	})

	if _, err := os.Stat(filepath.Join(configuredDir, "worker.nk")); err != nil {
		t.Errorf("expected a seed under the configured worker.data_dir %s: %v", configuredDir, err)
	}
}

// TestKeygenCmd_ReportsNewVsExistingWorkerID verifies that keygen states
// plainly whether the worker ID it printed was loaded from an existing
// worker.id file or freshly generated — the one signal an operator rotating
// a key has that --data-dir/worker.data_dir points at the wrong directory.
func TestKeygenCmd_ReportsNewVsExistingWorkerID(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "worker-data")

	prepareRoot([]string{"keygen", "--data-dir", dataDir})
	firstOut := captureStdout(t, func() {
		if err := Execute(); err != nil {
			t.Fatalf("first keygen: unexpected error: %v", err)
		}
	})
	if !strings.Contains(firstOut, "newly generated") {
		t.Errorf("first run against an empty data dir should report a newly generated worker id; got:\n%s", firstOut)
	}
	if strings.Contains(firstOut, "existing, loaded from") {
		t.Errorf("first run must not claim an existing worker id; got:\n%s", firstOut)
	}

	prepareRoot([]string{"keygen", "--data-dir", dataDir, "--force"})
	secondOut := captureStdout(t, func() {
		if err := Execute(); err != nil {
			t.Fatalf("second keygen: unexpected error: %v", err)
		}
	})
	if !strings.Contains(secondOut, "existing, loaded from") {
		t.Errorf("second run against the same data dir should report the existing worker id; got:\n%s", secondOut)
	}
	if strings.Contains(secondOut, "newly generated") {
		t.Errorf("second run must not claim a newly generated worker id; got:\n%s", secondOut)
	}
}
