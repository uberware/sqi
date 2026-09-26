// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package winsvc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
)

type harness struct {
	h        *handler
	requests chan svc.ChangeRequest
	statuses chan svc.Status
	done     chan struct{}
	specific bool
	code     uint32
	trace    string
}

func newHarness(t *testing.T, fn func(context.Context) error) *harness {
	t.Helper()
	trace := filepath.Join(t.TempDir(), "trace.log")
	hs := &harness{
		requests: make(chan svc.ChangeRequest),
		statuses: make(chan svc.Status, 64),
		done:     make(chan struct{}),
		trace:    trace,
	}
	hs.h = &handler{
		name:            "default-name",
		workDir:         t.TempDir(),
		fn:              fn,
		checkpointEvery: 10 * time.Millisecond,
		tracePath:       func(string) string { return trace },
		chdir:           func(string) error { return nil },
	}
	return hs
}

func (hs *harness) start(args ...string) {
	go func() {
		hs.specific, hs.code = hs.h.Execute(args, hs.requests, hs.statuses)
		close(hs.done)
	}()
}

func (hs *harness) waitState(t *testing.T, want svc.State) svc.Status {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case s := <-hs.statuses:
			if s.State == want {
				return s
			}
		case <-deadline:
			t.Fatalf("never reported state %d", want)
		}
	}
}

func (hs *harness) wait(t *testing.T) {
	t.Helper()
	select {
	case <-hs.done:
	case <-time.After(5 * time.Second):
		t.Fatal("Execute did not return")
	}
}

func blockUntilDone(ctx context.Context) error { <-ctx.Done(); return nil }

func TestHandler_StopCancelsAndExitsZero(t *testing.T) {
	var reason string
	hs := newHarness(t, func(ctx context.Context) error {
		<-ctx.Done()
		reason = StopReason(ctx)
		return nil
	})
	hs.start("sqi-worker")
	running := hs.waitState(t, svc.Running)
	if running.Accepts&svc.AcceptStop == 0 || running.Accepts&svc.AcceptPreShutdown == 0 {
		t.Fatalf("Running accepts %v, want Stop|PreShutdown", running.Accepts)
	}
	hs.requests <- svc.ChangeRequest{Cmd: svc.Stop}
	hs.waitState(t, svc.StopPending)
	hs.wait(t)
	if hs.specific || hs.code != 0 {
		t.Fatalf("exit = (%v, %d), want (false, 0)", hs.specific, hs.code)
	}
	if reason != "service stop" {
		t.Fatalf("StopReason = %q", reason)
	}
}

func TestHandler_PreShutdownCancels(t *testing.T) {
	var reason string
	hs := newHarness(t, func(ctx context.Context) error { <-ctx.Done(); reason = StopReason(ctx); return nil })
	hs.start("sqi-server")
	hs.waitState(t, svc.Running)
	hs.requests <- svc.ChangeRequest{Cmd: svc.PreShutdown}
	hs.wait(t)
	if reason != "service preshutdown" {
		t.Fatalf("StopReason = %q", reason)
	}
}

func TestHandler_FnErrorExitsServiceSpecificAndTraces(t *testing.T) {
	hs := newHarness(t, func(context.Context) error { return errors.New("load config: bad yaml") })
	hs.start("sqi-worker")
	hs.wait(t)
	if !hs.specific || hs.code != 1 {
		t.Fatalf("exit = (%v, %d), want (true, 1)", hs.specific, hs.code)
	}
	b, err := os.ReadFile(hs.trace)
	if err != nil || !strings.Contains(string(b), "load config: bad yaml") {
		t.Fatalf("trace = %q, %v", b, err)
	}
	if hs.h.err == nil || hs.h.err.Error() != "load config: bad yaml" {
		t.Fatalf("handler recorded %v; Run must return fn's error", hs.h.err)
	}
}

func TestHandler_CheckpointsAdvanceDuringSlowDrain(t *testing.T) {
	release := make(chan struct{})
	hs := newHarness(t, func(ctx context.Context) error { <-ctx.Done(); <-release; return nil })
	hs.start("sqi-worker")
	hs.waitState(t, svc.Running)
	hs.requests <- svc.ChangeRequest{Cmd: svc.Stop}
	first := hs.waitState(t, svc.StopPending)
	var later svc.Status
	for later.CheckPoint <= first.CheckPoint+1 {
		later = hs.waitState(t, svc.StopPending)
	}
	if later.WaitHint == 0 {
		t.Fatal("StopPending without a WaitHint")
	}
	close(release)
	hs.wait(t)
}

func TestHandler_StopDuringSlowStartup(t *testing.T) {
	// fn models a worker still blocked in NATS connect: it only returns once
	// its ctx is canceled.
	hs := newHarness(t, blockUntilDone)
	hs.start("sqi-worker")
	hs.waitState(t, svc.Running)
	hs.requests <- svc.ChangeRequest{Cmd: svc.Stop}
	hs.wait(t)
	if hs.specific {
		t.Fatal("a stop during startup reported a failure")
	}
}

func TestHandler_InterrogateAnsweredWhileRunningAndStopping(t *testing.T) {
	release := make(chan struct{})
	hs := newHarness(t, func(ctx context.Context) error { <-ctx.Done(); <-release; return nil })
	hs.start("sqi-worker")
	hs.waitState(t, svc.Running)
	hs.requests <- svc.ChangeRequest{Cmd: svc.Interrogate, CurrentStatus: svc.Status{State: svc.Running}}
	hs.waitState(t, svc.Running)
	hs.requests <- svc.ChangeRequest{Cmd: svc.Stop}
	hs.waitState(t, svc.StopPending)
	// Must not block: the SCM's control thread is waiting on this send.
	sent := make(chan struct{})
	go func() {
		hs.requests <- svc.ChangeRequest{Cmd: svc.Interrogate, CurrentStatus: svc.Status{State: svc.StopPending}}
		close(sent)
	}()
	select {
	case <-sent:
	case <-time.After(2 * time.Second):
		t.Fatal("Interrogate not read while stopping")
	}
	close(release)
	hs.wait(t)
}

func TestHandler_ServiceNameFromArgs(t *testing.T) {
	var name string
	hs := newHarness(t, func(ctx context.Context) error { name = ServiceName(ctx); return nil })
	hs.start("sqi-worker-2")
	hs.wait(t)
	if name != "sqi-worker-2" {
		t.Fatalf("ServiceName = %q, want the installed name from args[0]", name)
	}
}

func TestHandler_ChdirFailureIsReported(t *testing.T) {
	called := false
	hs := newHarness(t, func(context.Context) error { called = true; return nil })
	hs.h.chdir = func(string) error { return errors.New("access denied") }
	hs.start("sqi-worker")
	hs.wait(t)
	if called {
		t.Fatal("fn ran despite the working-directory failure")
	}
	if !hs.specific || hs.code != 1 {
		t.Fatalf("exit = (%v, %d)", hs.specific, hs.code)
	}
	b, err := os.ReadFile(hs.trace)
	if err != nil || !strings.Contains(string(b), "access denied") {
		t.Fatalf("trace = %q, %v", b, err)
	}
}

// blockedTracePath returns a trace path whose directory cannot be created,
// because a regular file already sits where the directory would go.
func blockedTracePath(t *testing.T) string {
	t.Helper()
	blocker := filepath.Join(t.TempDir(), "logs")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(blocker, "sqi-worker.log")
}

// TestHandler_TraceFallsBackToWorkDir pins that a failure whose default log
// directory cannot be created (refused, or blocked) still leaves a line: in
// <service-name>.trace.log in the service's working directory, with the reason
// the default log could not take it.
func TestHandler_TraceFallsBackToWorkDir(t *testing.T) {
	hs := newHarness(t, func(context.Context) error { return errors.New("load config: bad yaml") })
	primary := blockedTracePath(t)
	hs.h.tracePath = func(string) string { return primary }
	hs.start("sqi-worker")
	hs.wait(t)
	if !hs.specific || hs.code != 1 {
		t.Fatalf("exit = (%v, %d), want (true, 1)", hs.specific, hs.code)
	}
	b, err := os.ReadFile(filepath.Join(hs.h.workDir, "sqi-worker.trace.log"))
	if err != nil {
		t.Fatalf("no fallback trace in the working directory: %v", err)
	}
	for _, want := range []string{`"msg":"service exited with error"`, `"error":"load config: bad yaml"`, `"log_error":`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("fallback trace %q missing %s", b, want)
		}
	}
	if hs.h.err == nil || hs.h.err.Error() != "load config: bad yaml" {
		t.Fatalf("handler recorded %v; Run must return fn's error", hs.h.err)
	}
}

// TestHandler_TraceUsesDefaultLogWhenWritable pins that the fallback is only a
// fallback: a writable default log takes the line, and nothing is written to
// the working directory.
func TestHandler_TraceUsesDefaultLogWhenWritable(t *testing.T) {
	hs := newHarness(t, func(context.Context) error { return errors.New("load config: bad yaml") })
	hs.start("sqi-worker")
	hs.wait(t)
	if b, err := os.ReadFile(hs.trace); err != nil || !strings.Contains(string(b), "load config: bad yaml") {
		t.Fatalf("default trace = %q, %v", b, err)
	}
	if _, err := os.Lstat(filepath.Join(hs.h.workDir, "sqi-worker.trace.log")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a fallback trace was written although the default log was writable (lstat: %v)", err)
	}
}

// TestHandler_NoTraceFallbackWithoutWorkDir pins that the fallback is used only
// once the service has changed into its working directory: after a failed
// chdir that directory may not exist, or may not be one the service should
// write to.
func TestHandler_NoTraceFallbackWithoutWorkDir(t *testing.T) {
	hs := newHarness(t, func(context.Context) error { return nil })
	primary := blockedTracePath(t)
	hs.h.tracePath = func(string) string { return primary }
	hs.h.chdir = func(string) error { return errors.New("access denied") }
	hs.start("sqi-worker")
	hs.wait(t)
	if !hs.specific || hs.code != 1 {
		t.Fatalf("exit = (%v, %d), want (true, 1)", hs.specific, hs.code)
	}
	if _, err := os.Lstat(filepath.Join(hs.h.workDir, "sqi-worker.trace.log")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a fallback trace was written to a working directory the service never entered (lstat: %v)", err)
	}
}

// A Stop that lands while fn is still booting makes fn return an error that
// wraps context.Canceled (its blocking dial, discovery or registration saw the
// canceled ctx). That is the operator's stop, not a failure: reporting it as
// service-specific exit code 1 would let the SCM's recovery actions restart a
// service that was just stopped.
func TestHandler_CanceledErrorAfterStopIsCleanExit(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cmd        svc.Cmd
		wantReason string
	}{
		{"stop", svc.Stop, "service stop"},
		{"preshutdown", svc.PreShutdown, "service preshutdown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reason string
			hs := newHarness(t, func(ctx context.Context) error {
				<-ctx.Done()
				reason = StopReason(ctx)
				return fmt.Errorf("discovery: %w", context.Canceled)
			})
			hs.start("sqi-worker")
			hs.waitState(t, svc.Running)
			hs.requests <- svc.ChangeRequest{Cmd: tc.cmd}
			hs.wait(t)
			if hs.specific || hs.code != 0 {
				t.Fatalf("exit = (%v, %d), want (false, 0)", hs.specific, hs.code)
			}
			if b, err := os.ReadFile(hs.trace); err == nil && len(b) != 0 {
				t.Fatalf("a stop-initiated cancel wrote a failure trace: %q", b)
			} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("reading trace: %v", err)
			}
			if hs.h.err != nil {
				t.Fatalf("handler recorded %v; Run must return nil for a clean stop", hs.h.err)
			}
			if reason != tc.wantReason {
				t.Fatalf("StopReason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}

// Without a stop request, a context.Canceled from fn is fn's own doing (it
// canceled a ctx it derived) and remains a failure.
func TestHandler_CanceledErrorWithoutStopStillFails(t *testing.T) {
	hs := newHarness(t, func(context.Context) error { return context.Canceled })
	hs.start("sqi-worker")
	hs.wait(t)
	if !hs.specific || hs.code != 1 {
		t.Fatalf("exit = (%v, %d), want (true, 1)", hs.specific, hs.code)
	}
	b, err := os.ReadFile(hs.trace)
	if err != nil || !strings.Contains(string(b), context.Canceled.Error()) {
		t.Fatalf("trace = %q, %v", b, err)
	}
	if !errors.Is(hs.h.err, context.Canceled) {
		t.Fatalf("handler recorded %v; Run must return fn's error", hs.h.err)
	}
}

// Only a canceled error after a stop is forgiven: a drain that failed or timed
// out is a real failure the SCM must see.
func TestHandler_OtherErrorAfterStopStillFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"plain", errors.New("shutdown timeout")},
		{"deadline", fmt.Errorf("drain: %w", context.DeadlineExceeded)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hs := newHarness(t, func(ctx context.Context) error { <-ctx.Done(); return tc.err })
			hs.start("sqi-worker")
			hs.waitState(t, svc.Running)
			hs.requests <- svc.ChangeRequest{Cmd: svc.Stop}
			hs.wait(t)
			if !hs.specific || hs.code != 1 {
				t.Fatalf("exit = (%v, %d), want (true, 1)", hs.specific, hs.code)
			}
			b, err := os.ReadFile(hs.trace)
			if err != nil || !strings.Contains(string(b), tc.err.Error()) {
				t.Fatalf("trace = %q, %v", b, err)
			}
			if !errors.Is(hs.h.err, tc.err) {
				t.Fatalf("handler recorded %v; Run must return fn's error", hs.h.err)
			}
		})
	}
}
