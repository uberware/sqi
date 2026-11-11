// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package main

import (
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/loadtest/marker"
)

// startInGroup starts the no-op in a process group of its own, as a worker
// starts a task, and kills that group when the test ends.
func startInGroup(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	cmd.Env = append(os.Environ(), "SQILOAD_INCARNATION=0.0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start noop: %v", err)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() {
		syscall.Kill(-pgid, syscall.SIGKILL) //nolint:errcheck // ESRCH when the group already ended
	})
}

func TestNoop_SilentChildSurvivesParentKill(t *testing.T) {
	bin := noopBin(t)
	config, markers := writeConfig(t, marker.NoopClass{DurationMS: 3000, SilentChild: true})
	cmd := exec.CommandContext(t.Context(), bin, taskArgs(config)...)
	startInGroup(t, cmd)
	waitExec(t, markers, marker.RoleSilent, 2*time.Second)

	time.Sleep(500 * time.Millisecond)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill the task's main process: %v", err)
	}
	cmd.Wait() //nolint:errcheck // killed on purpose
	time.Sleep(1500 * time.Millisecond)

	silent, _ := execByRole(t, markers, marker.RoleSilent)
	if age := time.Since(silent.LastSeen); age > 500*time.Millisecond {
		t.Errorf("silent child last seen %s ago, 1.5s after its parent was killed; want it still alive", age)
	}
	if silent.End != nil {
		t.Errorf("silent child ended at %s, want it still running", silent.End)
	}
	main, _ := execByRole(t, markers, marker.RoleMain)
	if main.End != nil {
		t.Errorf("the killed main process recorded an end: %+v", main)
	}
}

func TestNoop_ChattyChildDiesOnBrokenPipe(t *testing.T) {
	bin := noopBin(t)
	config, markers := writeConfig(t, marker.NoopClass{DurationMS: 3000, ChattyChild: true})
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), bin, taskArgs(config)...)
	cmd.Stdout = w
	startInGroup(t, cmd)
	w.Close()
	go io.Copy(io.Discard, r) //nolint:errcheck // drains until the read end closes
	waitExec(t, markers, marker.RoleChatty, 2*time.Second)

	time.Sleep(500 * time.Millisecond)
	r.Close() // the dead worker's end of the task's stdout

	// Within 2s the chatty child has died of SIGPIPE: its LastSeen stops.
	time.Sleep(time.Second)
	before, _ := execByRole(t, markers, marker.RoleChatty)
	time.Sleep(600 * time.Millisecond)
	after, _ := execByRole(t, markers, marker.RoleChatty)
	if !after.LastSeen.Equal(before.LastSeen) {
		t.Errorf("chatty child's LastSeen advanced from %s to %s after its stdout broke", before.LastSeen, after.LastSeen)
	}
	cmd.Wait() //nolint:errcheck // the main process may die of the broken pipe too
	if chatty, _ := execByRole(t, markers, marker.RoleChatty); chatty.End != nil {
		t.Errorf("chatty child recorded an end %s; want it killed by SIGPIPE", chatty.End)
	}
}

// execByRole returns the one exec of role in dir.
func execByRole(t *testing.T, dir, role string) (marker.Exec, bool) {
	t.Helper()
	execs, _, err := marker.ReadAll(dir)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	var found []marker.Exec
	for _, e := range execs {
		if e.Role == role {
			found = append(found, e)
		}
	}
	if len(found) > 1 {
		t.Fatalf("%d %s execs, want at most one: %+v", len(found), role, found)
	}
	if len(found) == 0 {
		return marker.Exec{}, false
	}
	return found[0], true
}

// waitExec waits up to timeout for the exec of role to appear.
func waitExec(t *testing.T, dir, role string, timeout time.Duration) marker.Exec {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if e, ok := execByRole(t, dir, role); ok {
			return e
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no %s exec in %s after %s", role, dir, timeout)
	return marker.Exec{}
}
