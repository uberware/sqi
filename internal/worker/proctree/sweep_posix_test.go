// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux || darwin

package proctree

import (
	"bufio"
	"errors"
	"log/slog"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// pidsWithToken lists the processes whose environment carries token, through
// the same process reader the sweep uses.
func pidsWithToken(t *testing.T, token string) []int {
	t.Helper()
	all, err := listProcs()
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, pid := range all {
		if info, err := inspectProc(pid); err == nil && !info.zombie && info.token == token {
			pids = append(pids, pid)
		}
	}
	return pids
}

// goneOrZombie reports whether pid has exited (or exited and awaits reaping).
func goneOrZombie(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	info, err := inspectProc(pid)
	return err != nil || info.zombie
}

func TestBootID_IsStableAndNonEmpty(t *testing.T) {
	a, err := bootID()
	if err != nil {
		t.Fatalf("bootID: %v", err)
	}
	b, err := bootID()
	if err != nil {
		t.Fatalf("bootID: %v", err)
	}
	if a == "" || a != b {
		t.Errorf("bootID = %q then %q, want one non-empty value", a, b)
	}
	if !regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`).MatchString(a) {
		t.Errorf("bootID = %q, want the kernel's boot UUID", a)
	}
}

func TestSweep_KillsARealOrphanedTree(t *testing.T) {
	l, err := OpenLedger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// A background child, a child that leaves the group with setsid(2), and
	// the shell itself. perl makes the setsid call because macOS ships no
	// setsid(1). The echo after both are started means all three exist before
	// the sweep.
	cmd := exec.CommandContext(t.Context(), "sh", "-c",
		"sleep 300 & perl -MPOSIX -e 'POSIX::setsid(); exec @ARGV' sleep 300 & echo started; wait")
	Configure(cmd)
	cmd.Env = MarkEnv(nil)
	tok := envToken(cmd.Env)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// After a failure: the group, and the child that left it.
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) //nolint:errcheck // ESRCH when the sweep worked
		for _, pid := range pidsWithToken(t, envToken(cmd.Env)) {
			syscall.Kill(pid, syscall.SIGKILL) //nolint:errcheck // ESRCH when the sweep worked
		}
		cmd.Wait() //nolint:errcheck // reaps the shell
	})
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || strings.TrimSpace(line) != "started" {
		t.Fatalf("marker read = %q, %v", line, err)
	}
	tree, err := Supervise(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.Track(l, Label{Kind: "task", AttemptID: "a1"}); err != nil {
		t.Fatal(err)
	}

	var pids []int
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if pids = pidsWithToken(t, tok); len(pids) >= 3 {
			break
		}
	}
	if len(pids) < 3 {
		t.Fatalf("found %d marked processes %v, want 3 (shell, sleep, the sleep that left the group)", len(pids), pids)
	}

	// No Release: this is a worker that died with the tree running.
	if err := l.Sweep(t.Context(), slog.New(&logCapture{})); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		alive := 0
		for _, pid := range pids {
			if !goneOrZombie(pid) {
				alive++
			}
		}
		if alive == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %v still alive 5s after the sweep", alive, pids)
		}
	}
	if left := ledgerFiles(t, l); len(left) != 0 {
		t.Errorf("ledger after sweep: %v; want empty", left)
	}
}

func TestSweep_LeavesUnmarkedGroupMembers(t *testing.T) {
	l, err := OpenLedger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "sleep", "300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill() //nolint:errcheck // cleanup
		cmd.Wait()         //nolint:errcheck // reaps it
	})
	// A record naming this process's group, as if the number had been reused.
	putRecord(t, l, record{Token: newToken(), PGID: cmd.Process.Pid, Label: Label{Kind: "task"}})
	if err := l.Sweep(t.Context(), slog.New(&logCapture{})); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if goneOrZombie(cmd.Process.Pid) {
		t.Error("the sweep killed an unmarked process in a recorded group")
	}
}
