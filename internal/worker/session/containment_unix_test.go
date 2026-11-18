// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/worker/procout"
	"github.com/uberware/sqi/internal/worker/protocol"
)

// lowerDrainDelay shortens procout.DrainDelay for the test, so an action whose
// descendant survives it returns quickly enough to be measured.
func lowerDrainDelay(t *testing.T) {
	t.Helper()
	setDrainDelay(t, 200*time.Millisecond)
}

// setDrainDelay sets procout.DrainDelay to d for the test.
func setDrainDelay(t *testing.T, d time.Duration) {
	t.Helper()
	orig := procout.DrainDelay
	procout.DrainDelay = d
	t.Cleanup(func() { procout.DrainDelay = orig })
}

// childRecorder is a runAction line handler that records each `child=<pid>`
// line and kills every recorded process when the test ends.
type childRecorder struct {
	mu   sync.Mutex
	pids []int
	seen chan int
}

func newChildRecorder(t *testing.T) *childRecorder {
	t.Helper()
	r := &childRecorder{seen: make(chan int, 16)}
	t.Cleanup(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, pid := range r.pids {
			syscall.Kill(pid, syscall.SIGKILL) //nolint:errcheck // best-effort cleanup; the process is usually already gone
		}
	})
	return r
}

func (r *childRecorder) handle(_, line string) string {
	if s, ok := strings.CutPrefix(line, "child="); ok {
		if pid, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			r.mu.Lock()
			r.pids = append(r.pids, pid)
			r.mu.Unlock()
			select {
			case r.seen <- pid:
			default:
			}
		}
	}
	return line
}

// pid returns the one child the action reported.
func (r *childRecorder) pid(t *testing.T) int {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.pids) != 1 {
		t.Fatalf("the action reported children %v, want exactly one", r.pids)
	}
	return r.pids[0]
}

// processGone reports whether pid no longer exists within d.
func processGone(pid int, d time.Duration) bool {
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

// runActionAsync runs action through runAction in the background and returns
// the channel its error arrives on.
func runActionAsync(ctx context.Context, t *testing.T, action *protocol.Action, handler func(stream, line string) string) <-chan error {
	t.Helper()
	dir := t.TempDir()
	result := make(chan error, 1)
	go func() { result <- runAction(ctx, action, dir, nil, nil, nopLogger(), handler) }()
	return result
}

// awaitRunAction waits up to d for the action's result.
func awaitRunAction(t *testing.T, result <-chan error, d time.Duration) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(d):
		t.Fatalf("runAction did not return within %v", d)
		return nil
	}
}

// shAction is an environment action running script under sh -c.
func shAction(script string, timeout int, cancelation *protocol.CancelationMethod) *protocol.Action {
	return &protocol.Action{Command: "sh", Args: []string{"-c", script}, TimeoutSeconds: timeout, Cancelation: cancelation}
}

// wantTimedOut fails the test unless err reports the action's 1s timeout, and
// not a cancel.
func wantTimedOut(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "timed out after 1s") {
		t.Errorf("runAction error = %v, want one saying it timed out after 1s", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("runAction error = %v wraps context.Canceled; a timeout is not a cancel", err)
	}
}

const backgroundSleep = "sleep 300 & echo child=$!; wait"

// TestRunAction_TimeoutKillsTheWholeTree: an action's own timeout stops every
// process it started, not only the one it ran, and says it timed out.
func TestRunAction_TimeoutKillsTheWholeTree(t *testing.T) {
	lowerDrainDelay(t)
	rec := newChildRecorder(t)
	err := awaitRunAction(t, runActionAsync(context.Background(), t, shAction(backgroundSleep, 1, nil), rec.handle), 10*time.Second)
	wantTimedOut(t, err)
	if pid := rec.pid(t); !processGone(pid, 5*time.Second) {
		t.Errorf("background child %d survived the action's timeout", pid)
	}
}

// TestRunAction_CancelKillsTheWholeTree: canceling the session's context
// stops every process the action started.
func TestRunAction_CancelKillsTheWholeTree(t *testing.T) {
	lowerDrainDelay(t)
	rec := newChildRecorder(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := runActionAsync(ctx, t, shAction(backgroundSleep, 0, nil), rec.handle)
	select {
	case <-rec.seen:
	case <-time.After(10 * time.Second):
		t.Fatal("the action never reported its child")
	}
	time.Sleep(500 * time.Millisecond)
	cancel()
	err := awaitRunAction(t, result, 10*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("runAction error = %v, want one wrapping context.Canceled", err)
	}
	if pid := rec.pid(t); !processGone(pid, 5*time.Second) {
		t.Errorf("background child %d survived the cancel", pid)
	}
}

// TestRunAction_NotifyThenTerminate: an action whose cancelation is
// NOTIFY_THEN_TERMINATE gets SIGTERM across its whole tree, and SIGKILL once
// the notify period has passed with any of it still running.
func TestRunAction_NotifyThenTerminate(t *testing.T) {
	notifyFor := func(seconds int) *protocol.CancelationMethod {
		return &protocol.CancelationMethod{Mode: "NOTIFY_THEN_TERMINATE", NotifyPeriodSeconds: seconds}
	}

	t.Run("exits on notify", func(t *testing.T) {
		lowerDrainDelay(t)
		marker := filepath.Join(t.TempDir(), "marker")
		script := "trap 'touch \"" + marker + "\"; exit 0' TERM; echo ready; while :; do sleep 0.1; done"
		start := time.Now()
		err := awaitRunAction(t, runActionAsync(context.Background(), t, shAction(script, 1, notifyFor(10)), nil), 10*time.Second)
		wantTimedOut(t, err)
		if elapsed := time.Since(start); elapsed >= 5*time.Second {
			t.Errorf("runAction took %v: it waited out the notify period after the action exited", elapsed)
		}
		if _, statErr := os.Stat(marker); statErr != nil {
			t.Errorf("the action's TERM trap never ran: %v", statErr)
		}
	})

	t.Run("ignores notify", func(t *testing.T) {
		lowerDrainDelay(t)
		rec := newChildRecorder(t)
		script := "trap '' TERM; echo child=$$; while :; do sleep 0.1; done"
		start := time.Now()
		err := awaitRunAction(t, runActionAsync(context.Background(), t, shAction(script, 1, notifyFor(1)), rec.handle), 15*time.Second)
		elapsed := time.Since(start)
		wantTimedOut(t, err)
		if elapsed < 2*time.Second || elapsed >= 10*time.Second {
			t.Errorf("runAction took %v, want the 1s timeout plus the 1s notify period (2s to 10s)", elapsed)
		}
		if pid := rec.pid(t); !processGone(pid, 5*time.Second) {
			t.Errorf("action %d that ignored SIGTERM survived the notify period", pid)
		}
	})

	t.Run("leader exits, child ignores TERM", func(t *testing.T) {
		lowerDrainDelay(t)
		rec := newChildRecorder(t)
		script := "(trap '' TERM; exec sleep 300) & echo child=$!; trap 'exit 0' TERM; while :; do sleep 0.1; done"
		err := awaitRunAction(t, runActionAsync(context.Background(), t, shAction(script, 1, notifyFor(2)), rec.handle), 15*time.Second)
		wantTimedOut(t, err)
		if pid := rec.pid(t); !processGone(pid, 5*time.Second) {
			t.Errorf("child %d that ignored SIGTERM survived its leader's exit and the notify period", pid)
		}
	})
}

// backgroundService leaves a process holding the action's output after the
// action itself exits 0, as an onEnter that starts a service does.
const backgroundService = "(exec sleep 300) & echo child=$!; exit 0"

// wantServiceRunning fails the test unless the action succeeded and the
// service it left running is still alive.
func wantServiceRunning(t *testing.T, err error, rec *childRecorder) {
	t.Helper()
	if err != nil {
		t.Errorf("runAction error = %v, want nil: the action exited 0 before any stop", err)
	}
	if pid := rec.pid(t); syscall.Kill(pid, 0) != nil {
		t.Errorf("service %d the action left running was killed after the action exited", pid)
	}
}

// TestRunAction_ExitBeforeTimeoutLeavesServiceRunning: an action that exits
// before its timeout is not stopped when the timeout passes while its output
// is still draining, so the service it started keeps running and the action
// succeeds.
func TestRunAction_ExitBeforeTimeoutLeavesServiceRunning(t *testing.T) {
	setDrainDelay(t, 3*time.Second)
	rec := newChildRecorder(t)
	err := awaitRunAction(t, runActionAsync(context.Background(), t, shAction(backgroundService, 1, nil), rec.handle), 10*time.Second)
	wantServiceRunning(t, err, rec)
}

// TestRunAction_CancelAfterExitLeavesServiceRunning: a cancel that arrives
// after the action exited, while its output is still draining, stops
// nothing.
func TestRunAction_CancelAfterExitLeavesServiceRunning(t *testing.T) {
	setDrainDelay(t, 3*time.Second)
	rec := newChildRecorder(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := runActionAsync(ctx, t, shAction(backgroundService, 0, nil), rec.handle)
	select {
	case <-rec.seen:
	case <-time.After(10 * time.Second):
		t.Fatal("the action never reported its child")
	}
	time.Sleep(time.Second)
	cancel()
	wantServiceRunning(t, awaitRunAction(t, result, 10*time.Second), rec)
}

// TestRunAction_CanceledContextStartsNothing: an action whose session is
// already canceled is not started at all.
func TestRunAction_CanceledContextStartsNothing(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "marker")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runAction(ctx, shAction("touch \""+marker+"\"", 0, nil), t.TempDir(), nil, nil, nopLogger(), nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("runAction error = %v, want one wrapping context.Canceled", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("the action ran although its context was already canceled")
	}
}
