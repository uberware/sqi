// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/winsvc"
	workerconfig "github.com/uberware/sqi/internal/worker/config"
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

// TestWorkerConfigPaths_FromConfig pins that service install checks the
// paths the loaded configuration names, as written, log.file included and
// carried separately for the log directory.
func TestWorkerConfigPaths_FromConfig(t *testing.T) {
	t.Setenv("SQI_WORKER_NATS_TLS_CA_FILE", "")
	t.Setenv("SQI_WORKER_LOG_FILE", "")
	t.Setenv("SQI_WORKER_DATA_DIR", "")
	path := filepath.Join(t.TempDir(), "sqi-worker.yaml")
	body := "nats:\n  tls_ca_file: pki/ca.pem\nworker:\n  data_dir: data\n  session_dir: sessions\nlog:\n  file: logs/w.log\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := workerConfigPaths(path)
	if err != nil {
		t.Fatalf("workerConfigPaths: %v", err)
	}
	if got.LogFile != "logs/w.log" {
		t.Errorf("LogFile = %q, want logs/w.log", got.LogFile)
	}
	for _, want := range []winsvc.PathRef{
		{Key: "nats.tls_ca_file", Path: "pki/ca.pem"},
		{Key: "nats.credential_file", Path: filepath.Join("data", "worker.nk")},
		{Key: "worker.data_dir", Path: "data"},
		{Key: "worker.session_dir", Path: "sessions"},
		{Key: "log.file", Path: "logs/w.log"},
	} {
		if !slices.Contains(got.Paths, want) {
			t.Errorf("Paths lacks %q: %q", want, got.Paths)
		}
	}
	for _, p := range got.Paths {
		if p.Key == "worker.session_dir (default)" {
			t.Errorf("Paths names the default session root although worker.session_dir is set: %q", got.Paths)
		}
	}
}

// TestWorkerConfigPaths_DefaultSessionRoot pins that service install checks
// the session root the worker uses when worker.session_dir is unset:
// %ProgramData%\sqi\worker\sessions, computed as the worker computes it.
func TestWorkerConfigPaths_DefaultSessionRoot(t *testing.T) {
	t.Setenv("SQI_WORKER_SESSION_DIR", "")
	t.Setenv("SQI_WORKER_DATA_DIR", "")
	pd := t.TempDir()
	t.Setenv("ProgramData", pd)
	path := filepath.Join(t.TempDir(), "sqi-worker.yaml")
	if err := os.WriteFile(path, []byte("worker:\n  data_dir: data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := workerConfigPaths(path)
	if err != nil {
		t.Fatalf("workerConfigPaths: %v", err)
	}
	want := winsvc.PathRef{Key: "worker.session_dir (default)", Path: filepath.Join(pd, "sqi", "worker", "sessions")}
	if !slices.Contains(got.Paths, want) {
		t.Errorf("Paths lacks %q: %q", want, got.Paths)
	}
}

// TestInstall_RefusesUnsetWorkerDataDir pins that service install refuses a
// configuration that leaves worker.data_dir unset, naming the key and the
// way out, and accepts one that sets it, even to the default's value. The
// winsvc test of the same name pins that install returns this error before
// the password prompt and before any operation after the existence probe.
func TestInstall_RefusesUnsetWorkerDataDir(t *testing.T) {
	t.Setenv("SQI_WORKER_DATA_DIR", "")
	dir := t.TempDir()
	write := func(body string) string {
		path := filepath.Join(dir, "sqi-worker.yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	path := write("log:\n  level: info\n")
	_, err := workerConfigPaths(path)
	if err == nil {
		t.Fatal("workerConfigPaths accepted a configuration without worker.data_dir")
	}
	for _, s := range []string{path, "worker.data_dir", "data_dir: " + filepath.Join(dir, "worker"), "sqi-worker config init"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("refusal lacks %q:\n%v", s, err)
		}
	}
	def := workerconfig.Default().Worker.DataDir
	if _, err := workerConfigPaths(write("worker:\n  data_dir: '" + def + "'\n")); err != nil {
		t.Errorf("workerConfigPaths refused a data_dir equal to the default: %v", err)
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
