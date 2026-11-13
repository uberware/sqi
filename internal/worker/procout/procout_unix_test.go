// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package procout

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startGroup starts script under sh as the leader of its own process group,
// as the executor starts a task, and kills the group when the test ends.
func startGroup(t *testing.T, script string) *Output {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := Start(cmd)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() {
		syscall.Kill(-pgid, syscall.SIGKILL) //nolint:errcheck // best-effort cleanup of the test's own processes
	})
	return out
}

// groupAlive reports whether any process of out's process group is running.
func groupAlive(out *Output) func() bool {
	pgid := out.cmd.Process.Pid
	return func() bool {
		err := syscall.Kill(-pgid, 0)
		return err == nil || errors.Is(err, syscall.EPERM)
	}
}

// killChild kills the process a script reported as "child=<pid>".
func killChild(lines []string) {
	for _, l := range lines {
		if pid, ok := strings.CutPrefix(l, "child="); ok {
			if n, err := strconv.Atoi(pid); err == nil {
				syscall.Kill(n, syscall.SIGKILL) //nolint:errcheck // best-effort cleanup of the test's own sleep
			}
		}
	}
}

// TestWait_WithoutAGroup covers a process whose output closes when it exits
// and one that leaves a descendant holding it open, with no process group
// tracked. Either way Wait returns soon after the process exits, with its
// exit status, and the output written before the exit is read; stillOpen
// reports the held output whatever the exit status.
func TestWait_WithoutAGroup(t *testing.T) {
	shortDrain(t, 200*time.Millisecond)

	for _, tc := range []struct {
		name          string
		script        string
		wantExit      int
		wantStillOpen bool
	}{
		{name: "output closes on exit", script: "echo before exit; exit 0"},
		{name: "failed exit", script: "echo before exit; exit 3", wantExit: 3},
		{name: "descendant holds the output", script: "sleep 60 & echo child=$!; echo before exit; exit 0", wantStillOpen: true},
		{name: "descendant holds the output after a failed exit", script: "sleep 60 & echo child=$!; echo before exit; exit 3", wantExit: 3, wantStillOpen: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Start(exec.CommandContext(t.Context(), "sh", "-c", tc.script))
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			lines := readAll(out)

			start := time.Now()
			stillOpen, err := out.Wait(nil)
			elapsed := time.Since(start)
			got := <-lines
			killChild(got)

			if elapsed > 10*time.Second {
				t.Fatalf("Wait took %v: it waited for the descendant, not for the process", elapsed)
			}
			if stillOpen != tc.wantStillOpen {
				t.Errorf("stillOpen = %v, want %v", stillOpen, tc.wantStillOpen)
			}
			var exitErr *exec.ExitError
			switch {
			case tc.wantExit == 0 && err != nil:
				t.Errorf("Wait error = %v, want nil", err)
			case tc.wantExit != 0 && (!errors.As(err, &exitErr) || exitErr.ExitCode() != tc.wantExit):
				t.Errorf("Wait error = %v, want exit status %d", err, tc.wantExit)
			}
			if !strings.Contains(strings.Join(got, "\n"), "before exit") {
				t.Errorf("output %q lost the line written before the exit", got)
			}
		})
	}
}

// TestWait_WaitsForTheGroup: a child left running in the process's own group,
// a background job the script did not wait for, is waited for and its output
// read, as before the drain existed. Only a holder outside the group is cut
// off after DrainDelay.
func TestWait_WaitsForTheGroup(t *testing.T) {
	shortDrain(t, 200*time.Millisecond)
	out := startGroup(t, "(sleep 1; echo later) & echo first; exit 0")
	lines := readAll(out)

	stillOpen, err := out.Wait(groupAlive(out))
	got := <-lines

	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if stillOpen {
		t.Error("stillOpen = true, want false: the group's own child closed the output")
	}
	if !strings.Contains(strings.Join(got, "\n"), "later") {
		t.Errorf("output %q lost the line the group's child wrote after the exit", got)
	}
}

// TestWait_DoesNotKillTheHolder: once the drain delay has passed, the output
// is no longer delivered but is still read, so a process holding it, such as
// a service an onEnter action starts, is not killed by SIGPIPE on its next
// write.
func TestWait_DoesNotKillTheHolder(t *testing.T) {
	shortDrain(t, 100*time.Millisecond)
	marker := filepath.Join(t.TempDir(), "marker")
	out := startGroup(t, "(sleep 0.5; echo later; touch '"+marker+"') & echo first; exit 0")
	lines := readAll(out)

	stillOpen, err := out.Wait(nil)
	<-lines

	if err != nil || !stillOpen {
		t.Fatalf("Wait = %v, %v; want the output still open and no error", stillOpen, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the holder never got past its write after the drain: it was killed")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestWait_UnreadOutputDoesNotHang: a caller that kills the process before it
// starts reading, as the executor does when it cannot supervise a started
// task, must not hang in Wait. Discard reads the output for it; without it
// Wait still returns once DrainDelay has passed.
func TestWait_UnreadOutputDoesNotHang(t *testing.T) {
	shortDrain(t, 200*time.Millisecond)

	for _, tc := range []struct {
		name    string
		discard bool
	}{
		{name: "discarded", discard: true},
		{name: "never read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := startGroup(t, "echo written; sleep 30")
			time.Sleep(100 * time.Millisecond) // let it write before the kill
			if err := out.cmd.Process.Kill(); err != nil {
				t.Fatalf("Kill: %v", err)
			}
			if tc.discard {
				out.Discard()
			}

			done := make(chan struct{})
			go func() {
				out.Wait(nil) //nolint:errcheck // only the return matters
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("Wait hung on output nothing read")
			}
		})
	}
}

func TestStart_ReturnsTheStartError(t *testing.T) {
	if _, err := Start(exec.CommandContext(t.Context(), "/nonexistent/sqi-procout-test")); err == nil {
		t.Fatal("Start of a missing program succeeded")
	}
}
