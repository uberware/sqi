// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package integration

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// launcherForwardsSignalExit runs the launcher directly, in its own process
// group exactly as the POSIX executor starts a task (kill_unix.go), and signals
// the group with SIGTERM as cancellation does. The child traps SIGTERM and
// exits 42; the launcher must survive the same signal and exit 42 too, rather
// than dying first and losing the child's code.
func launcherForwardsSignalExit(t *testing.T, python, launcher string) {
	t.Helper()
	dir := t.TempDir()
	launcherPath := filepath.Join(dir, "launcher.py")
	scriptPath := filepath.Join(dir, "script.py")
	ready := filepath.Join(dir, "ready")
	script := "import signal, sys, time\n" +
		"signal.signal(signal.SIGTERM, lambda s, f: sys.exit(42))\n" +
		"open(r\"" + ready + "\", \"w\").close()\n" +
		"time.sleep(60)\n"
	for path, body := range map[string]string{launcherPath: launcher, scriptPath: script} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	cmd := exec.CommandContext(t.Context(), python, launcherPath, scriptPath, "1", "1", "1-1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start launcher: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill() //nolint:errcheck // best-effort cleanup of a test process
			t.Fatal("child never signaled readiness")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatalf("signal group: %v", err)
	}
	err := cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 42 {
		t.Fatalf("launcher exit = %v, want exit code 42 (the child's own SIGTERM handler)", err)
	}
}
