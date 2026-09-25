// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package winsvc

import (
	"context"
	"errors"
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
