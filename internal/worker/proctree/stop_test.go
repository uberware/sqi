// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package proctree

import (
	"bufio"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestStop_KillsAfterPeriodWhenTreeOutlivesNotify: a group member that
// ignores SIGTERM is killed once the notify period has passed.
func TestStop_KillsAfterPeriodWhenTreeOutlivesNotify(t *testing.T) {
	// The leader exits on TERM; its child ignores it, so the group outlives
	// the notify and only the kill after the period ends it. Each process sets
	// its trap before the line the test waits for is printed, and the child
	// prints that line itself: announcing the child's pid from the parent
	// ($!) races the child's own trap, and a TERM that wins the race kills the
	// whole group early. An ignored signal stays ignored across exec.
	cmd := exec.CommandContext(t.Context(), "sh", "-c", childIgnoresTermScript)
	Configure(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	br := bufio.NewReader(stdout)
	line, err := br.ReadString('\n')
	child, convErr := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(line), "child="))
	if err != nil || convErr != nil {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) //nolint:errcheck // best-effort cleanup before failing
		cmd.Wait()                                      //nolint:errcheck // reaps the killed leader
		t.Fatalf("child line = %q, err = %v", line, errors.Join(err, convErr))
	}
	// The pipe reaches EOF once the whole group is gone; the leader is reaped
	// after that.
	waited := make(chan struct{})
	go func() {
		io.Copy(io.Discard, br) //nolint:errcheck // drains the output to EOF; a read error ends it too
		cmd.Wait()              //nolint:errcheck // reaps the leader; its exit status is not under test
		close(waited)
	}()
	t.Cleanup(func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) //nolint:errcheck // best-effort cleanup if an assertion fails first
		syscall.Kill(child, syscall.SIGKILL)            //nolint:errcheck // the child is in the group unless it escaped it
		<-waited
	})

	tree, err := Supervise(cmd)
	if err != nil {
		t.Fatalf("Supervise: %v", err)
	}
	start := time.Now()
	if err := tree.Stop(true, time.Second); err != nil {
		t.Errorf("Stop: %v", err)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Errorf("Stop returned after %v, before the 1s notify period", elapsed)
	}
	if !childGone(child, 5*time.Second) {
		t.Errorf("child %d that ignored SIGTERM survived Stop", child)
	}
}

// TestStop_KillOfEmptyTreeIsNotAnError: stopping a tree whose processes have
// all exited succeeds, although there is nothing left to signal.
func TestStop_KillOfEmptyTreeIsNotAnError(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "true")
	Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	tree, err := Supervise(cmd)
	if err != nil {
		t.Fatalf("Supervise: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if err := tree.Stop(false, 0); err != nil {
		t.Errorf("Stop of an exited, reaped tree = %v, want nil", err)
	}
}

// childIgnoresTermScript is a group whose leader exits on SIGTERM and whose
// child ignores it, printing "child=<pid>" once both traps are in place.
const childIgnoresTermScript = `trap 'exit 0' TERM; sh -c 'trap "" TERM; echo child=$$; exec sleep 300' & while :; do sleep 0.1; done`

// childGone reports whether pid no longer exists within d.
func childGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}
