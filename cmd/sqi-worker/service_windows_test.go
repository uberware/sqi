// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWorkerDrainTimeout_FromConfig(t *testing.T) {
	t.Setenv("SQI_WORKER_SHUTDOWN_GRACE_PERIOD", "")
	path := filepath.Join(t.TempDir(), "sqi-worker.yaml")
	if err := os.WriteFile(path, []byte("worker:\n  shutdown_grace_period: 90s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := workerDrainTimeout(path)
	if err != nil || got != 90*time.Second {
		t.Fatalf("workerDrainTimeout = %v, %v; want 90s", got, err)
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
