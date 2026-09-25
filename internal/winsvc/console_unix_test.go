// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package winsvc

import (
	"context"
	"syscall"
	"testing"
	"time"
)

// TestRun_ConsoleSIGTERMCancelsContext pins the console path to today's
// serve/start behavior: SIGTERM cancels the context fn runs under.
func TestRun_ConsoleSIGTERMCancelsContext(t *testing.T) {
	err := Run("x", func(ctx context.Context) error {
		if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
			t.Error("SIGTERM did not cancel the context")
			return nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}
