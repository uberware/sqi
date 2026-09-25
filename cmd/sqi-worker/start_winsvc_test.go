// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestShutdownTrigger(t *testing.T) {
	sig := make(chan os.Signal, 1)
	sig <- syscall.SIGTERM
	if got := shutdownTrigger(context.Background(), sig); got != syscall.SIGTERM.String() {
		t.Errorf("signal trigger = %q", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := shutdownTrigger(ctx, make(chan os.Signal)); got != context.Canceled.Error() {
		t.Errorf("fallback trigger = %q, want ctx.Err()", got)
	}
}

// A Windows service must not register for OS signals: the Go runtime maps
// CTRL_LOGOFF_EVENT and CTRL_SHUTDOWN_EVENT to SIGTERM whenever any channel is
// registered, and Windows sends those to services on every interactive logoff.
// A registered channel would buffer that stale SIGTERM and a later SCM Stop
// would be logged as "terminated" instead of "service stop".
func TestRegisterShutdownSignals(t *testing.T) {
	type notifyCall struct {
		c    chan<- os.Signal
		sigs []os.Signal
	}
	var notified []notifyCall
	var stopped []chan<- os.Signal
	notify := func(c chan<- os.Signal, sigs ...os.Signal) { notified = append(notified, notifyCall{c, sigs}) }
	stop := func(c chan<- os.Signal) { stopped = append(stopped, c) }

	t.Run("service registers nothing", func(t *testing.T) {
		notified, stopped = nil, nil
		sig, release := registerShutdownSignalsWith(true, notify, stop)
		if len(notified) != 0 {
			t.Fatalf("service mode called signal.Notify %d time(s), want 0", len(notified))
		}
		select {
		case s := <-sig:
			t.Fatalf("un-registered channel yielded %v", s)
		default:
		}
		release()
		if len(stopped) != 0 {
			t.Fatalf("service mode called signal.Stop %d time(s), want 0", len(stopped))
		}
		// shutdownTrigger must not block or panic on the un-registered channel.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if got := shutdownTrigger(ctx, sig); got != context.Canceled.Error() {
			t.Errorf("trigger = %q, want ctx.Err()", got)
		}
	})

	t.Run("console registers interrupt and SIGTERM and unregisters on release", func(t *testing.T) {
		notified, stopped = nil, nil
		sig, release := registerShutdownSignalsWith(false, notify, stop)
		if len(notified) != 1 {
			t.Fatalf("console mode called signal.Notify %d time(s), want 1", len(notified))
		}
		got := notified[0]
		if len(got.sigs) != 2 || got.sigs[0] != os.Interrupt || got.sigs[1] != syscall.SIGTERM {
			t.Fatalf("Notify signals = %v, want [interrupt terminated]", got.sigs)
		}
		if len(stopped) != 0 {
			t.Fatal("signal.Stop called before release")
		}
		release()
		if len(stopped) != 1 || stopped[0] != got.c {
			t.Fatalf("release did not stop the registered channel: %v", stopped)
		}
		// The channel handed to Notify is the one shutdownTrigger drains.
		got.c <- syscall.SIGTERM
		if trig := shutdownTrigger(context.Background(), sig); trig != syscall.SIGTERM.String() {
			t.Errorf("trigger = %q, want the signal name", trig)
		}
	})
}

// registerShutdownSignals is the production wrapper: the console path really
// registers with the runtime, and releasing it must not panic or hang.
func TestRegisterShutdownSignals_RealConsolePathReleases(t *testing.T) {
	sig, release := registerShutdownSignals(false)
	if sig == nil {
		t.Fatal("nil channel")
	}
	done := make(chan struct{})
	go func() { release(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("release did not return")
	}
}
