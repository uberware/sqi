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
	"github.com/uberware/sqi/internal/worker/fmtres"
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

// wantSetupError fails the test unless Dispatch returned an error within d.
func wantSetupError(t *testing.T, result chan error, d time.Duration) {
	t.Helper()
	if err := awaitDispatch(t, result, d); err == nil {
		t.Error("Dispatch returned nil; want the stopped entry's error")
	}
}

// wantWorkerShutdown fails the test unless the last status published is
// "failed" with [protocol.MessageWorkerShutdown].
func wantWorkerShutdown(t *testing.T, nc *stubNATS) {
	t.Helper()
	msgs := nc.statusMsgs()
	if len(msgs) == 0 {
		t.Fatal("no status was published")
	}
	if last := msgs[len(msgs)-1]; last.Status != "failed" || last.Message != protocol.MessageWorkerShutdown {
		t.Errorf("terminal status = %q (%q), want failed (%q)", last.Status, last.Message, protocol.MessageWorkerShutdown)
	}
}

// drainWithin runs DrainAndShutdown(grace) and fails the test unless it
// returns within 15s.
func drainWithin(t *testing.T, exec *executor.Executor, grace time.Duration) (completed, killed int) {
	t.Helper()
	type drained struct{ completed, killed int }
	done := make(chan drained, 1)
	go func() {
		c, k := exec.DrainAndShutdown(grace)
		done <- drained{c, k}
	}()
	select {
	case got := <-done:
		return got.completed, got.killed
	case <-time.After(15 * time.Second):
		t.Fatal("DrainAndShutdown did not return within 15s")
		return 0, 0
	}
}

// twoEnvAssign is an assignment entering A (onEnter, onExit) then B, whose
// onEnter blocks. It returns the paths A's onExit and B's onEnter create.
func twoEnvAssign(t *testing.T) (msg *protocol.AssignMsg, aOut, bIn string) {
	t.Helper()
	dir := t.TempDir()
	aIn := filepath.Join(dir, "a-in")
	aOut, bIn = filepath.Join(dir, "a-out"), filepath.Join(dir, "b-in")
	msg = setupAssign(
		touchEnv("A", touchAction(aIn, ""), touchAction(aOut, "")),
		touchEnv("B", touchAction(bIn, "300s"), nil),
	)
	return msg, aOut, bIn
}

// TestDispatch_CancelDuringOnEnterReportsCanceled: a cancel that arrives while
// an environment is being entered stops the entry, and the task is reported
// canceled.
func TestDispatch_CancelDuringOnEnterReportsCanceled(t *testing.T) {
	exec, nc, _ := newTestExecutor(t, nil)
	started := filepath.Join(t.TempDir(), "started")
	msg := setupAssign(touchEnv("A", touchAction(started, "300s"), nil))

	held := watchHeld(nc, exec, msg.AttemptID)

	result := dispatchAsync(t, exec, msg)
	waitForMarker(t, started, 10*time.Second)
	if !slices.Contains(exec.ActiveAttemptIDs(), msg.AttemptID) {
		t.Fatal("the attempt is not held while its environment is being entered")
	}
	if !exec.Cancel(msg.TaskID, msg.AttemptID) {
		t.Fatal("Cancel did not find the task whose environment is being entered")
	}
	wantSetupError(t, result, 10*time.Second)
	if got := nc.statuses(); !slices.Equal(got, []string{"canceled"}) {
		t.Fatalf("statuses = %v, want [canceled]", got)
	}
	// Held at the terminal publish, released by the time Dispatch returns.
	held.require(t)
	if ids := exec.ActiveAttemptIDs(); len(ids) != 0 {
		t.Fatalf("held after Dispatch returned = %v, want none", ids)
	}
}

// TestDispatch_RefusesAssignmentOnceWorkerIsStopping: an assignment dispatched
// after the worker was told to stop enters no environment and runs nothing; it
// is reported as a worker shutdown and released.
func TestDispatch_RefusesAssignmentOnceWorkerIsStopping(t *testing.T) {
	exec, nc, _ := newTestExecutor(t, nil)
	t.Cleanup(func() { exec.DrainAndShutdown(0) })
	entered := filepath.Join(t.TempDir(), "entered")
	msg := setupAssign(touchEnv("A", touchAction(entered, ""), nil))
	held := watchHeld(nc, exec, msg.AttemptID)
	exec.Hold(msg.AttemptID)

	stopping, stop := context.WithCancel(context.Background())
	stop()
	if err := exec.Dispatch(stopping, msg); err == nil {
		t.Fatal("Dispatch returned nil; want an error for a worker that is stopping")
	}

	// Dispatch is synchronous: an onEnter it started would have run by now.
	if _, err := os.Stat(entered); err == nil {
		t.Error("the environment's onEnter ran on a worker that is stopping")
	}
	if got := nc.statuses(); !slices.Equal(got, []string{"failed"}) {
		t.Fatalf("statuses = %v, want [failed]", got)
	}
	wantWorkerShutdown(t, nc)
	held.require(t)
	if ids := exec.ActiveAttemptIDs(); len(ids) != 0 {
		t.Errorf("held after Dispatch returned = %v, want none", ids)
	}
	if n := exec.ActiveTaskCount(); n != 0 {
		t.Errorf("ActiveTaskCount = %d, want 0", n)
	}
}

// cancelOnRegister is an output handler whose TaskLifecycleHook cancels the
// attempt as it is registered, which is after its session exists and before
// anything of the task is staged or started.
type cancelOnRegister struct {
	*captureOutput

	exec *executor.Executor
}

func (c *cancelOnRegister) RegisterTask(attemptID, taskID, _, _ string, _ context.CancelFunc) {
	c.exec.Cancel(taskID, attemptID)
}

func (*cancelOnRegister) Deregister(string) {}

func (*cancelOnRegister) TakeFailReason(string) (string, bool) { return "", false }

// TestDispatch_CancelAfterSetupStartsNoProcess: a cancel that lands after the
// environments are entered and before the task starts is reported canceled,
// and no attempt is made to start the task's process. The task's command does
// not exist, so an attempt to start it would be reported failed.
func TestDispatch_CancelAfterSetupStartsNoProcess(t *testing.T) {
	hook := &cancelOnRegister{captureOutput: &captureOutput{}}
	exec, nc, _ := newTestExecutorHandler(t, hook, fmtres.ExprLimits{})
	hook.exec = exec
	msg := setupAssign(touchEnv("A", nil, nil))
	msg.OnRun = &protocol.Action{Command: filepath.Join(t.TempDir(), "no-such-command")}

	result := dispatchAsync(t, exec, msg)
	if err := awaitDispatch(t, result, 10*time.Second); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if ids := waitForNoAttempts(exec, 10*time.Second); len(ids) != 0 {
		t.Fatalf("held after the task ended = %v, want none", ids)
	}
	if got := nc.statuses(); !slices.Equal(got, []string{"canceled"}) {
		t.Fatalf("statuses = %v, want [canceled]", got)
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

	if _, killed := drainWithin(t, exec, 500*time.Millisecond); killed != 1 {
		t.Errorf("DrainAndShutdown killed = %d, want 1", killed)
	}
	wantWorkerShutdown(t, nc) // published before the drain returned
	wantSetupError(t, result, 5*time.Second)
}

// TestDispatch_StoppedEntryRunsEarlierOnExit: when a cancel stops an
// environment being entered, the environments already entered are still
// exited.
func TestDispatch_StoppedEntryRunsEarlierOnExit(t *testing.T) {
	exec, _, _ := newTestExecutor(t, nil)
	msg, aOut, bIn := twoEnvAssign(t)

	result := dispatchAsync(t, exec, msg)
	waitForMarker(t, bIn, 10*time.Second)
	if !exec.Cancel(msg.TaskID, msg.AttemptID) {
		t.Fatal("Cancel did not find the task whose environment is being entered")
	}
	wantSetupError(t, result, 10*time.Second)
	if _, err := os.Stat(aOut); err != nil {
		t.Errorf("environment A's onExit did not run after B's entry was stopped: %v", err)
	}
}

// TestDispatch_DrainStopsEntryAndRunsEarlierOnExit: when the shutdown drain
// stops an environment being entered, the environments already entered are
// exited before the drain returns, and the task is reported as a worker
// shutdown.
func TestDispatch_DrainStopsEntryAndRunsEarlierOnExit(t *testing.T) {
	exec, nc, _ := newTestExecutor(t, nil)
	msg, aOut, bIn := twoEnvAssign(t)

	result := dispatchAsync(t, exec, msg)
	waitForMarker(t, bIn, 10*time.Second)

	drainWithin(t, exec, 500*time.Millisecond)
	if _, err := os.Stat(aOut); err != nil {
		t.Errorf("environment A's onExit had not run when the drain returned: %v", err)
	}
	wantWorkerShutdown(t, nc)
	wantSetupError(t, result, 5*time.Second)
}
