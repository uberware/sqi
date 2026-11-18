// SPDX-License-Identifier: AGPL-3.0-or-later

package executor_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/worker/executor"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// touchAction runs the test binary in its "touch" mode: it creates marker,
// then sleeps for sleep (none when empty) and exits 0.
func touchAction(marker, sleep string) *protocol.Action {
	args := []string{"-test.run=^$", "marker=" + marker}
	if sleep != "" {
		args = append(args, "sleep="+sleep)
	}
	return &protocol.Action{Command: testBinary(), Args: args}
}

// touchEnv is an environment whose actions run the test binary's "touch"
// mode. The variable also reaches the task's OnRun, which then exits 0.
func touchEnv(name string, onEnter, onExit *protocol.Action) protocol.AssignEnvironment {
	return protocol.AssignEnvironment{
		Name:      name,
		Variables: map[string]string{"SQI_TEST_SUBPROCESS": "touch"},
		OnEnter:   onEnter,
		OnExit:    onExit,
	}
}

// setupAssign is an assignment entering envs, whose task exits 0 at once.
func setupAssign(envs ...protocol.AssignEnvironment) *protocol.AssignMsg {
	return &protocol.AssignMsg{
		Version:      protocol.ProtocolVersion,
		Type:         protocol.TypeAssign,
		TaskID:       "task-setup",
		AttemptID:    "attempt-setup",
		JobID:        "job-1",
		OnRun:        &protocol.Action{Command: testBinary(), Args: []string{"-test.run=^$"}},
		Environments: envs,
	}
}

// dispatchAsync runs Dispatch in the background and returns the channel its
// error arrives on. Whatever the test leaves running is stopped when it ends.
func dispatchAsync(t *testing.T, exec *executor.Executor, msg *protocol.AssignMsg) chan error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- exec.Dispatch(ctx, msg) }()
	t.Cleanup(func() {
		cancel()
		exec.DrainAndShutdown(0)
		select {
		case <-result:
		case <-time.After(15 * time.Second):
			t.Error("Dispatch still had not returned when the test ended")
		}
	})
	return result
}

// awaitDispatch waits up to d for Dispatch's error, and puts it back so
// dispatchAsync's cleanup does not wait for a second one.
func awaitDispatch(t *testing.T, result chan error, d time.Duration) error {
	t.Helper()
	select {
	case err := <-result:
		result <- err
		return err
	case <-time.After(d):
		t.Fatalf("Dispatch did not return within %v", d)
		return nil
	}
}

// waitForMarker waits up to d for the file at path to exist.
func waitForMarker(t *testing.T, path string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not appear within %v", filepath.Base(path), d)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestDispatch_CancelDuringOnEnterReportsCanceled: a cancel that arrives while
// an environment is being entered stops the entry, and the task is reported
// canceled.
func TestDispatch_CancelDuringOnEnterReportsCanceled(t *testing.T) {
	exec, nc, _ := newTestExecutor(t, nil)
	started := filepath.Join(t.TempDir(), "started")
	msg := setupAssign(touchEnv("A", touchAction(started, "300s"), nil))

	result := dispatchAsync(t, exec, msg)
	waitForMarker(t, started, 10*time.Second)
	if !exec.Cancel(msg.TaskID, msg.AttemptID) {
		t.Fatal("Cancel did not find the task whose environment is being entered")
	}
	if err := awaitDispatch(t, result, 10*time.Second); err == nil {
		t.Fatal("Dispatch returned nil; want the stopped entry's error")
	}
	if got := nc.statuses(); !slices.Equal(got, []string{"running", "canceled"}) {
		t.Fatalf("statuses = %v, want [running canceled]", got)
	}
}

// TestDispatch_DrainLetsOnEnterFinishWithinGrace: the shutdown drain waits for
// an environment being entered, and the task it then runs, to finish within
// the grace period.
func TestDispatch_DrainLetsOnEnterFinishWithinGrace(t *testing.T) {
	exec, nc, _ := newTestExecutor(t, nil)
	started := filepath.Join(t.TempDir(), "started")
	msg := setupAssign(touchEnv("A", touchAction(started, "1s"), nil))

	result := dispatchAsync(t, exec, msg)
	waitForMarker(t, started, 10*time.Second)
	completed, killed := exec.DrainAndShutdown(30 * time.Second)
	if completed != 1 || killed != 0 {
		t.Fatalf("DrainAndShutdown = (%d, %d), want (1, 0)", completed, killed)
	}
	if err := awaitDispatch(t, result, 5*time.Second); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if got := nc.statuses(); len(got) == 0 || got[len(got)-1] != "succeeded" {
		t.Fatalf("statuses = %v, want them to end succeeded", got)
	}
}

// TestDispatch_DrainStopsOnEnterAfterGrace: an environment entry still running
// when the drain's grace period ends is stopped, reported as a worker shutdown,
// and counted as killed, all before the drain returns.
func TestDispatch_DrainStopsOnEnterAfterGrace(t *testing.T) {
	exec, nc, _ := newTestExecutor(t, nil)
	started := filepath.Join(t.TempDir(), "started")
	msg := setupAssign(touchEnv("A", touchAction(started, "300s"), nil))

	result := dispatchAsync(t, exec, msg)
	waitForMarker(t, started, 10*time.Second)

	type drained struct{ completed, killed int }
	done := make(chan drained, 1)
	go func() {
		c, k := exec.DrainAndShutdown(500 * time.Millisecond)
		done <- drained{c, k}
	}()
	var got drained
	select {
	case got = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("DrainAndShutdown did not return within 15s")
	}
	if got.killed != 1 {
		t.Errorf("DrainAndShutdown killed = %d, want 1", got.killed)
	}

	msgs := nc.statusMsgs()
	if len(msgs) == 0 {
		t.Fatal("no status was published before DrainAndShutdown returned")
	}
	last := msgs[len(msgs)-1]
	if last.Status != "failed" || last.Message != protocol.MessageWorkerShutdown {
		t.Errorf("terminal status = %q (%q), want failed (%q)", last.Status, last.Message, protocol.MessageWorkerShutdown)
	}
	if err := awaitDispatch(t, result, 5*time.Second); err == nil {
		t.Error("Dispatch returned nil; want the stopped entry's error")
	}
}

// TestDispatch_StoppedEntryRunsEarlierOnExit: when a cancel stops an
// environment being entered, the environments already entered are still
// exited.
func TestDispatch_StoppedEntryRunsEarlierOnExit(t *testing.T) {
	exec, _, _ := newTestExecutor(t, nil)
	dir := t.TempDir()
	aIn, aOut, bIn := filepath.Join(dir, "a-in"), filepath.Join(dir, "a-out"), filepath.Join(dir, "b-in")
	msg := setupAssign(
		touchEnv("A", touchAction(aIn, ""), touchAction(aOut, "")),
		touchEnv("B", touchAction(bIn, "300s"), nil),
	)

	result := dispatchAsync(t, exec, msg)
	waitForMarker(t, bIn, 10*time.Second)
	if !exec.Cancel(msg.TaskID, msg.AttemptID) {
		t.Fatal("Cancel did not find the task whose environment is being entered")
	}
	if err := awaitDispatch(t, result, 10*time.Second); err == nil {
		t.Fatal("Dispatch returned nil; want the stopped entry's error")
	}
	if _, err := os.Stat(aOut); err != nil {
		t.Errorf("environment A's onExit did not run after B's entry was stopped: %v", err)
	}
}
