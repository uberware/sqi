// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/uberware/sqi/internal/server"
	"github.com/uberware/sqi/internal/winsvc"
)

func TestServerDrainTimeout_IsShutdownTimeout(t *testing.T) {
	got, err := serverDrainTimeout("")
	if err != nil || got != server.ShutdownTimeout {
		t.Fatalf("serverDrainTimeout = %v, %v; want %v", got, err, server.ShutdownTimeout)
	}
}

// TestServerConfigPaths_FromConfig pins that service install checks the
// paths the loaded configuration names, as written, defaults included and
// log.file carried separately for the log directory.
func TestServerConfigPaths_FromConfig(t *testing.T) {
	t.Setenv("SQI_STORE_SQLITE_PATH", "")
	t.Setenv("SQI_LOG_FILE", "")
	t.Setenv("SQI_NATS_EMBEDDED_DATA_DIR", "")
	path := filepath.Join(t.TempDir(), "sqi-server.yaml")
	if err := os.WriteFile(path, []byte("log:\n  file: logs/s.log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := serverConfigPaths(path, "")
	if err != nil {
		t.Fatalf("serverConfigPaths: %v", err)
	}
	if got.LogFile != "logs/s.log" {
		t.Errorf("LogFile = %q, want logs/s.log", got.LogFile)
	}
	for _, want := range []winsvc.PathRef{
		{Key: "nats.embedded.data_dir", Path: "data/nats"},
		{Key: "store.sqlite_path", Path: "sqi.db"},
		{Key: "log.file", Path: "logs/s.log"},
	} {
		if !slices.Contains(got.Paths, want) {
			t.Errorf("Paths lacks %q: %q", want, got.Paths)
		}
	}
}

func TestServerServiceCommand_Registered(t *testing.T) {
	for _, sub := range []string{"install", "uninstall", "start", "stop", "status"} {
		if c, _, err := rootCmd.Find([]string{"service", sub}); err != nil || c.Name() != sub {
			t.Errorf("service %s not registered: %v", sub, err)
		}
	}
}
