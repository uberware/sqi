// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package session

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/worker/procout"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// startsDetachedGrandchild writes a leader.bat that starts grandchild.bat in
// the background with "start /b" and then pings for leaderPings, and returns
// the environment action that runs it. grandchild.bat pings for pingSeconds
// and then writes marker. The grandchild is a separate process the leader
// does not wait for, so once the leader is gone it is orphaned: the case
// "taskkill /T" cannot reach and a job object can.
//
// The scripts are .bat files, not inline "cmd /c" one-liners, because
// os/exec quotes arguments the CommandLineToArgvW way, which cmd.exe does not
// parse; the paths are passed unquoted (TempDir paths contain no spaces).
//
// Any process still running from the scripts' directory is killed when the
// test ends.
func startsDetachedGrandchild(t *testing.T, pingSeconds, leaderPings int) (action *protocol.Action, marker string) {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { killProcessesUnder(t, dir) })
	marker = filepath.Join(dir, "marker.txt")
	grandchild := filepath.Join(dir, "grandchild.bat")
	grandchildScript := "@echo off\r\nping -n " + strconv.Itoa(pingSeconds) + " 127.0.0.1 > nul\r\necho alive > \"" + marker + "\"\r\n"
	if err := os.WriteFile(grandchild, []byte(grandchildScript), 0o700); err != nil {
		t.Fatalf("write grandchild.bat: %v", err)
	}
	leader := filepath.Join(dir, "leader.bat")
	leaderScript := "@echo off\r\nstart /b " + grandchild + "\r\nping -n " + strconv.Itoa(leaderPings) + " 127.0.0.1 > nul\r\n"
	if err := os.WriteFile(leader, []byte(leaderScript), 0o700); err != nil {
		t.Fatalf("write leader.bat: %v", err)
	}
	return &protocol.Action{Command: "cmd", Args: []string{"/c", leader}}, marker
}

// killProcessesUnder force-kills, with their trees, the processes whose
// command line names dir: the test's batch files, if an assertion failed
// before the action's own stop reached them.
func killProcessesUnder(t *testing.T, dir string) {
	t.Helper()
	script := "Get-CimInstance Win32_Process | Where-Object { $_.ProcessId -ne $PID -and $_.CommandLine -like '*" + dir + "*' } | " +
		"ForEach-Object { taskkill /F /T /PID $_.ProcessId 2>&1 | Out-Null }; exit 0"
	if out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput(); err != nil { //nolint:noctx // cleanup runs after the test context is done
		t.Logf("cleanup of %s: %v: %s", dir, err, out)
	}
}

// lowerWindowsDrainDelay shortens procout.DrainDelay for the test, so an
// action whose grandchild holds its output returns quickly.
func lowerWindowsDrainDelay(t *testing.T) {
	t.Helper()
	orig := procout.DrainDelay
	procout.DrainDelay = 200 * time.Millisecond
	t.Cleanup(func() { procout.DrainDelay = orig })
}

// runActionWithin runs action and fails the test unless it returns within d.
func runActionWithin(t *testing.T, action *protocol.Action, d time.Duration) (elapsed time.Duration, err error) {
	t.Helper()
	dir := t.TempDir()
	result := make(chan error, 1)
	start := time.Now()
	go func() { result <- runAction(context.Background(), action, dir, nil, nil, nopLogger(), nil) }()
	select {
	case err = <-result:
		return time.Since(start), err
	case <-time.After(d):
		t.Fatalf("runAction did not return within %v", d)
		return 0, nil
	}
}

// wantTimedOutAfter1s fails the test unless err reports the action's 1s
// timeout, and not a cancel.
func wantTimedOutAfter1s(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "timed out after 1s") {
		t.Errorf("runAction error = %v, want one saying it timed out after 1s", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("runAction error = %v wraps context.Canceled; a timeout is not a cancel", err)
	}
}

// wantMarkerAbsentAfter fails the test if marker appears within d: the
// grandchild that writes it survived.
func wantMarkerAbsentAfter(t *testing.T, marker string, d time.Duration) {
	t.Helper()
	time.Sleep(d)
	if _, err := os.Stat(marker); err == nil {
		t.Error("the orphaned grandchild survived the stop and wrote its marker")
	}
}

// TestRunAction_TimeoutKillsDetachedGrandchild: an action's timeout stops a
// grandchild it started detached, not only the process it ran.
func TestRunAction_TimeoutKillsDetachedGrandchild(t *testing.T) {
	lowerWindowsDrainDelay(t)
	action, marker := startsDetachedGrandchild(t, 5, 60)
	action.TimeoutSeconds = 1
	_, err := runActionWithin(t, action, 10*time.Second)
	wantTimedOutAfter1s(t, err)
	wantMarkerAbsentAfter(t, marker, 7*time.Second)
}

// TestRunAction_NormalExitLeavesDetachedGrandchild: an action that exits on
// its own leaves running what it started, such as a service an onEnter
// starts for the session's tasks.
func TestRunAction_NormalExitLeavesDetachedGrandchild(t *testing.T) {
	lowerWindowsDrainDelay(t)
	action, marker := startsDetachedGrandchild(t, 3, 1)
	if _, err := runActionWithin(t, action, 10*time.Second); err != nil {
		t.Fatalf("runAction: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the grandchild never wrote its marker: the action's normal exit killed it")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestRunAction_NotifyFallsThroughToKill: "taskkill /T" without /F cannot
// end a console process, so a NOTIFY_THEN_TERMINATE stop kills the tree at
// once rather than waiting out the notify period.
func TestRunAction_NotifyFallsThroughToKill(t *testing.T) {
	lowerWindowsDrainDelay(t)
	action, marker := startsDetachedGrandchild(t, 5, 60)
	action.TimeoutSeconds = 1
	action.Cancelation = &protocol.CancelationMethod{Mode: "NOTIFY_THEN_TERMINATE", NotifyPeriodSeconds: 30}
	elapsed, err := runActionWithin(t, action, 40*time.Second)
	if elapsed >= 20*time.Second {
		t.Errorf("runAction took %v: it waited out the 30s notify period", elapsed)
	}
	wantTimedOutAfter1s(t, err)
	wantMarkerAbsentAfter(t, marker, 7*time.Second)
}
