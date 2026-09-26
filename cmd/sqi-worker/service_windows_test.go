// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/winsvc"
)

// TestWorkerDrainTimeout_FromConfig pins that the drain timeout a service is
// installed with covers the whole worker drain: the configured grace period,
// then the SIGTERM→SIGKILL window (a third of it) for tasks still running.
func TestWorkerDrainTimeout_FromConfig(t *testing.T) {
	t.Setenv("SQI_WORKER_SHUTDOWN_GRACE_PERIOD", "")
	path := filepath.Join(t.TempDir(), "sqi-worker.yaml")
	if err := os.WriteFile(path, []byte("worker:\n  shutdown_grace_period: 90s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := workerDrainTimeout(path)
	if err != nil || got != 120*time.Second {
		t.Fatalf("workerDrainTimeout = %v, %v; want 120s (90s grace + 30s kill window)", got, err)
	}
}

// TestWorkerDrainBound_CoversGraceAndKillWindow pins the relationship between
// the service's drain bound and the executor's kill window: both come from
// shutdownKillGrace, the window is a third of the grace period, and at the
// default 30 s grace the PreShutdown timeout is 55 s (30 + 10 + 15), which the
// Windows service docs quote.
func TestWorkerDrainBound_CoversGraceAndKillWindow(t *testing.T) {
	for _, grace := range []time.Duration{30 * time.Second, 45 * time.Second, 90 * time.Second, 10 * time.Minute} {
		kill := shutdownKillGrace(grace)
		if kill != grace/3 {
			t.Errorf("shutdownKillGrace(%v) = %v, want a third of the grace period", grace, kill)
		}
		if got := workerDrainBound(grace); got != grace+kill {
			t.Errorf("workerDrainBound(%v) = %v, want the grace period plus the kill window, %v", grace, got, grace+kill)
		}
	}
	if got := workerDrainBound(30*time.Second) + winsvc.ShutdownMargin; got != 55*time.Second {
		t.Errorf("PreShutdown timeout at the default grace period = %v, want 55s", got)
	}
}

func TestWorkerServiceCommand_Registered(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"service", "install"})
	if err != nil || cmd.Name() != "install" {
		t.Fatalf("service install not registered: %v", err)
	}
	for _, f := range []string{"name", "display-name", "user", "start"} {
		if cmd.Flags().Lookup(f) == nil {
			t.Errorf("service install lacks --%s", f)
		}
	}
	if cmd.Flag("config") == nil {
		t.Error("service install cannot see the root --config flag")
	}
	if cmd.LocalNonPersistentFlags().Lookup("config") != nil {
		t.Error("service install declares its own --config, shadowing the root flag")
	}
}
