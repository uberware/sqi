// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux || darwin

package proctree

import (
	"bufio"
	"errors"
	"log/slog"
	"os/exec"
	"regexp"
	"strconv"
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

// treePIDs is pidsWithToken plus, for processes whose environment the kernel
// withholds, the members of group pgid: the processes the sweep must find.
func treePIDs(t *testing.T, token string, pgid int) []int {
	t.Helper()
	all, err := listProcs()
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, pid := range all {
		info, err := inspectProc(pid)
		if err != nil || info.zombie {
			continue
		}
		if info.token == token || (info.envHidden && info.pgid == pgid) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// platformHidesEnv reports whether this host withholds the environment of the
// system binaries the sweep tests run (macOS 27 does), so a process that left
// its group cannot be matched by token and cannot be found at all.
func platformHidesEnv(t *testing.T) bool {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "sleep", "5")
	cmd.Env = MarkEnv(nil)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }() //nolint:errcheck // cleanup
	info, err := inspectProc(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	return info.token == "" && info.envHidden
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
	// setsid(1); it reports its pid once it has left the group, and its pid
	// survives the exec into sleep. The echo after both are started means all
	// three exist before the sweep.
	cmd := exec.CommandContext(t.Context(), "sh", "-c",
		"sleep 300 & perl -MPOSIX -e 'POSIX::setsid(); print STDOUT \"left $$\\n\"; $|=1; exec @ARGV' sleep 300 & echo started; wait")
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
	escaped := 0 // the child that left the group, once it has said so
	t.Cleanup(func() {
		// After a failure: the group, and the child that left it.
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) //nolint:errcheck // ESRCH when the sweep worked
		for _, pid := range pidsWithToken(t, envToken(cmd.Env)) {
			syscall.Kill(pid, syscall.SIGKILL) //nolint:errcheck // ESRCH when the sweep worked
		}
		if escaped > 0 { // unfindable by token where the environment is withheld
			syscall.Kill(escaped, syscall.SIGKILL) //nolint:errcheck // ESRCH when the sweep worked
		}
		cmd.Wait() //nolint:errcheck // reaps the shell
	})
	// Both lines mean every process exists and the perl child has already left
	// the group, so the set of group members cannot change under the test.
	rd := bufio.NewReader(stdout)
	for seen := map[string]bool{}; len(seen) < 2; {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("marker read = %q, %v", line, err)
		}
		line = strings.TrimSpace(line)
		switch {
		case line == "started":
			seen[line] = true
		case strings.HasPrefix(line, "left "):
			if escaped, err = strconv.Atoi(strings.TrimPrefix(line, "left ")); err != nil {
				t.Fatalf("bad pid in marker %q: %v", line, err)
			}
			seen["left"] = true
		default:
			t.Fatalf("unexpected marker %q", line)
		}
	}
	tree, err := Supervise(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.Track(l, Label{Kind: "task", AttemptID: "a1"}); err != nil {
		t.Fatal(err)
	}

	// Where the environment is withheld the process that left its group has
	// nothing to identify it: a sweep finds the other two.
	want := 3
	if platformHidesEnv(t) {
		want = 2
	}
	var pids []int
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if pids = treePIDs(t, tok, cmd.Process.Pid); len(pids) >= want {
			break
		}
	}
	if len(pids) < want {
		t.Fatalf("found %d tree processes %v, want %d (shell, sleep, and where the environment is readable the sleep that left the group)", len(pids), pids, want)
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
	// The record names the live leader but a different start time, which is how
	// a reused number looks: the group route must refuse it.
	putRecord(t, l, record{Token: newToken(), PGID: cmd.Process.Pid, LeaderStart: 1, Label: Label{Kind: "task"}})
	if err := l.Sweep(t.Context(), slog.New(&logCapture{})); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if goneOrZombie(cmd.Process.Pid) {
		t.Error("the sweep killed an unmarked process in a recorded group")
	}
}
