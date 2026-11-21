// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux || darwin

package proctree

import (
	"bufio"
	"errors"
	"log/slog"
	"os"
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
	tok := NewToken()
	// A background child, a child that leaves the group with setsid, and the
	// shell itself. The echo after both are started means all three exist
	// before the sweep.
	cmd := exec.CommandContext(t.Context(), "sh", "-c", "sleep 300 & setsid sleep 300 & echo started; wait")
	Configure(cmd)
	cmd.Env = MarkEnv(nil, tok)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) //nolint:errcheck // cleanup after a failure
		cmd.Wait()                                      //nolint:errcheck // reaps the shell
	})
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || strings.TrimSpace(line) != "started" {
		t.Fatalf("marker read = %q, %v", line, err)
	}
	tree, err := Supervise(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.Track(l, tok, Label{Kind: "task", AttemptID: "a1"}); err != nil {
		t.Fatal(err)
	}

	var pids []int
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if pids = pidsWithToken(t, tok); len(pids) >= 3 {
			break
		}
	}
	if len(pids) < 3 {
		t.Fatalf("found %d marked processes %v, want 3 (shell, sleep, setsid sleep)", len(pids), pids)
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
	ents, err := os.ReadDir(l.dir)
	if err != nil || len(ents) != 0 {
		t.Errorf("ledger after sweep: %d entries, err %v; want empty", len(ents), err)
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
	if _, err := l.write(record{Token: NewToken(), PGID: cmd.Process.Pid, BootID: l.bootID, Label: Label{Kind: "task"}, StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := l.Sweep(t.Context(), slog.New(&logCapture{})); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if goneOrZombie(cmd.Process.Pid) {
		t.Error("the sweep killed an unmarked process in a recorded group")
	}
}
