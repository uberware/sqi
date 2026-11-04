// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package main

import (
	"bytes"
	"syscall"
	"testing"
	"time"
)

// TestInterruptible sends the process itself two stop signals: the first
// cancels the run's context, the second closes the abort channel.
func TestInterruptible(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			var stderr bytes.Buffer
			a := &app{stderr: &stderr}
			ctx, abort, stop := a.interruptible(t.Context())
			defer stop()
			if err := syscall.Kill(syscall.Getpid(), sig); err != nil {
				t.Fatal(err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("the first signal did not cancel the context")
			}
			select {
			case <-abort:
				t.Fatal("the first signal closed abort")
			default:
			}
			if err := syscall.Kill(syscall.Getpid(), sig); err != nil {
				t.Fatal(err)
			}
			select {
			case <-abort:
			case <-time.After(5 * time.Second):
				t.Fatal("the second signal did not close abort")
			}
		})
	}
}
