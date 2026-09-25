// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"os"
	"syscall"
	"testing"
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
