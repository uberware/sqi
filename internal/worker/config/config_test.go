// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ── Default() ────────────────────────────────────────────────────────────────

func TestDefault_SanityValues(t *testing.T) {
	cfg := Default()

	if cfg.Worker.MaxConcurrentTasks < 1 {
		t.Errorf("max_concurrent_tasks default %d < 1", cfg.Worker.MaxConcurrentTasks)
	}
	if cfg.Worker.HeartbeatInterval <= 0 {
		t.Errorf("heartbeat_interval default %v is not positive", cfg.Worker.HeartbeatInterval)
	}
	if cfg.Worker.ShutdownGracePeriod <= 0 {
		t.Errorf("shutdown_grace_period default %v is not positive", cfg.Worker.ShutdownGracePeriod)
	}
	if cfg.Log.Level == "" {
		t.Error("log.level default is empty")
	}
	if cfg.Log.Format == "" {
		t.Error("log.format default is empty")
	}
	if cfg.Metrics.Addr == "" {
		t.Error("metrics.addr default is empty")
	}
	if cfg.Discovery.MDNSTimeout <= 0 {
		t.Errorf("discovery.mdns_timeout default %v is not positive", cfg.Discovery.MDNSTimeout)
	}
	// nats.url should be empty by default — server address must be supplied.
	if cfg.NATS.URL != "" {
		t.Errorf("nats.url default should be empty, got %q", cfg.NATS.URL)
	}
}

// ── Load — file layer ─────────────────────────────────────────────────────────

func TestLoad_FileOverridesDefaults(t *testing.T) {
	yaml := `
nats:
  url: "nats://file-host:4222"
  reconnect_wait: "10s"
worker:
  name: "from-file"
  max_concurrent_tasks: 8
log:
  level: "debug"
  format: "text"
`
	f := writeTempYAML(t, yaml)

	cfg, err := Load(f, FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	assertStr(t, "nats.url", cfg.NATS.URL, "nats://file-host:4222")
	assertDuration(t, "nats.reconnect_wait", cfg.NATS.ReconnectWait, 10*time.Second)
	assertStr(t, "worker.name", cfg.Worker.Name, "from-file")
	assertInt(t, "worker.max_concurrent_tasks", cfg.Worker.MaxConcurrentTasks, 8)
	assertStr(t, "log.level", cfg.Log.Level, "debug")
	assertStr(t, "log.format", cfg.Log.Format, "text")
}

func TestLoad_ExplicitFileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/path/sqi-worker.yaml", FlagOverrides{})
	if err == nil {
		t.Error("expected error for missing explicit config file, got nil")
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	f := writeTempFile(t, "sqi-worker.yaml", []byte("nats: [invalid yaml}}"))
	_, err := Load(f, FlagOverrides{})
	if err == nil {
		t.Error("expected error for invalid YAML, got nil")
	}
}

// ── Load — env layer ──────────────────────────────────────────────────────────

func TestLoad_EnvOverridesFile(t *testing.T) {
	yaml := `
nats:
  url: "nats://file-host:4222"
worker:
  name: "from-file"
  max_concurrent_tasks: 2
`
	f := writeTempYAML(t, yaml)

	t.Setenv("SQI_WORKER_NATS_URL", "nats://env-host:4222")
	t.Setenv("SQI_WORKER_NAME", "from-env")
	t.Setenv("SQI_WORKER_MAX_CONCURRENT_TASKS", "6")

	cfg, err := Load(f, FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	assertStr(t, "nats.url", cfg.NATS.URL, "nats://env-host:4222")
	assertStr(t, "worker.name", cfg.Worker.Name, "from-env")
	assertInt(t, "worker.max_concurrent_tasks", cfg.Worker.MaxConcurrentTasks, 6)
}

func TestLoad_EnvOverridesDefaults(t *testing.T) {
	t.Setenv("SQI_WORKER_NATS_URL", "nats://env-only:4222")
	t.Setenv("SQI_WORKER_LOG_LEVEL", "warn")
	t.Setenv("SQI_WORKER_LOG_FORMAT", "text")
	t.Setenv("SQI_WORKER_HEARTBEAT_INTERVAL", "5s")
	t.Setenv("SQI_WORKER_SHUTDOWN_GRACE_PERIOD", "60s")
	t.Setenv("SQI_WORKER_METRICS_ADDR", "127.0.0.1:9999")
	t.Setenv("SQI_WORKER_METRICS_ENABLE_PPROF", "true")
	t.Setenv("SQI_WORKER_DISCOVERY_ENABLE_MDNS", "false")
	t.Setenv("SQI_WORKER_DISCOVERY_MDNS_TIMEOUT", "10s")
	t.Setenv("SQI_WORKER_ALLOW_ROOT", "true")

	cfg, err := Load("", FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	assertStr(t, "nats.url", cfg.NATS.URL, "nats://env-only:4222")
	assertStr(t, "log.level", cfg.Log.Level, "warn")
	assertStr(t, "log.format", cfg.Log.Format, "text")
	assertDuration(t, "heartbeat_interval", cfg.Worker.HeartbeatInterval, 5*time.Second)
	assertDuration(t, "shutdown_grace_period", cfg.Worker.ShutdownGracePeriod, 60*time.Second)
	assertStr(t, "metrics.addr", cfg.Metrics.Addr, "127.0.0.1:9999")
	if !cfg.Metrics.EnablePprof {
		t.Error("metrics.enable_pprof: expected true from env")
	}
	if cfg.Discovery.EnableMDNS {
		t.Error("discovery.enable_mdns: expected false from env")
	}
	assertDuration(t, "mdns_timeout", cfg.Discovery.MDNSTimeout, 10*time.Second)
	if !cfg.Worker.AllowRoot {
		t.Error("worker.allow_root: expected true from env")
	}
}

func TestLoad_CapabilityTagsEnv(t *testing.T) {
	t.Setenv("SQI_WORKER_CAPABILITY_TAGS", "maya-2025, arnold-7, gpu")
	t.Setenv("SQI_WORKER_NATS_URL", "nats://x:4222") // satisfy validation

	cfg, err := Load("", FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := []string{"maya-2025", "arnold-7", "gpu"}
	if len(cfg.Worker.CapabilityTags) != len(want) {
		t.Fatalf("capability_tags: got %v, want %v", cfg.Worker.CapabilityTags, want)
	}
	for i, v := range want {
		if cfg.Worker.CapabilityTags[i] != v {
			t.Errorf("capability_tags[%d]: got %q, want %q", i, cfg.Worker.CapabilityTags[i], v)
		}
	}
}

// ── Load — flag layer ─────────────────────────────────────────────────────────

func TestLoad_FlagOverridesEnvAndFile(t *testing.T) {
	yaml := `
log:
  level: "debug"
  format: "text"
`
	f := writeTempYAML(t, yaml)

	t.Setenv("SQI_WORKER_LOG_LEVEL", "warn")
	t.Setenv("SQI_WORKER_LOG_FORMAT", "text")

	flags := FlagOverrides{
		LogLevel:  "error",
		LogFormat: "json",
	}

	cfg, err := Load(f, flags)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	assertStr(t, "log.level", cfg.Log.Level, "error")
	assertStr(t, "log.format", cfg.Log.Format, "json")
}

func TestLoad_EmptyFlagsDoNotOverride(t *testing.T) {
	yaml := `
log:
  level: "debug"
  format: "text"
`
	f := writeTempYAML(t, yaml)

	cfg, err := Load(f, FlagOverrides{}) // empty flags — file values win
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	assertStr(t, "log.level", cfg.Log.Level, "debug")
	assertStr(t, "log.format", cfg.Log.Format, "text")
}

// ── Load — precedence summary ─────────────────────────────────────────────────
//
// flag > env > file > default
// Verified field: log.level

func TestLoad_Precedence_FlagWinsOverAll(t *testing.T) {
	// default: "info"
	// file:    "debug"
	// env:     "warn"
	// flag:    "error"  ← must win
	f := writeTempYAML(t, "log:\n  level: debug\n")
	t.Setenv("SQI_WORKER_LOG_LEVEL", "warn")

	cfg, err := Load(f, FlagOverrides{LogLevel: "error"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assertStr(t, "log.level", cfg.Log.Level, "error")
}

func TestLoad_Precedence_EnvWinsOverFileAndDefault(t *testing.T) {
	// file: "debug", env: "warn", no flag → env wins
	f := writeTempYAML(t, "log:\n  level: debug\n")
	t.Setenv("SQI_WORKER_LOG_LEVEL", "warn")

	cfg, err := Load(f, FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assertStr(t, "log.level", cfg.Log.Level, "warn")
}

func TestLoad_Precedence_FileWinsOverDefault(t *testing.T) {
	// default: "info", file: "debug", no env, no flag → file wins
	f := writeTempYAML(t, "log:\n  level: debug\n")

	cfg, err := Load(f, FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assertStr(t, "log.level", cfg.Log.Level, "debug")
}

// ── Validate — error paths ────────────────────────────────────────────────────

func TestValidate_ValidConfigNoErrors(t *testing.T) {
	cfg := Default()
	cfg.NATS.URL = "nats://localhost:4222"
	errs := Validate(cfg)
	if len(errs) != 0 {
		t.Errorf("expected no errors, got %v", errs)
	}
}

func TestValidate_ValidConfigMDNS(t *testing.T) {
	cfg := Default()
	// nats.url empty is OK when mDNS is enabled
	cfg.NATS.URL = ""
	cfg.Discovery.EnableMDNS = true
	errs := Validate(cfg)
	if len(errs) != 0 {
		t.Errorf("expected no errors with mdns enabled, got %v", errs)
	}
}

func TestValidate_MissingNATSURLWhenMDNSDisabled(t *testing.T) {
	cfg := Default()
	cfg.NATS.URL = ""
	cfg.Discovery.EnableMDNS = false

	errs := Validate(cfg)
	if !containsField(errs, "nats.url") {
		t.Errorf("expected nats.url error, got %v", errs)
	}
}

func TestValidate_ZeroMaxConcurrentTasks(t *testing.T) {
	cfg := Default()
	cfg.NATS.URL = "nats://localhost:4222"
	cfg.Worker.MaxConcurrentTasks = 0

	errs := Validate(cfg)
	if !containsField(errs, "worker.max_concurrent_tasks") {
		t.Errorf("expected max_concurrent_tasks error, got %v", errs)
	}
}

func TestValidate_NegativeMaxConcurrentTasks(t *testing.T) {
	cfg := Default()
	cfg.NATS.URL = "nats://localhost:4222"
	cfg.Worker.MaxConcurrentTasks = -1

	errs := Validate(cfg)
	if !containsField(errs, "worker.max_concurrent_tasks") {
		t.Errorf("expected max_concurrent_tasks error, got %v", errs)
	}
}

func TestValidate_EmptyDataDir(t *testing.T) {
	cfg := Default()
	cfg.NATS.URL = "nats://localhost:4222"
	cfg.Worker.DataDir = ""

	errs := Validate(cfg)
	if !containsField(errs, "worker.data_dir") {
		t.Errorf("expected data_dir error, got %v", errs)
	}
}

func TestValidate_ZeroHeartbeatInterval(t *testing.T) {
	cfg := Default()
	cfg.NATS.URL = "nats://localhost:4222"
	cfg.Worker.HeartbeatInterval = 0

	errs := Validate(cfg)
	if !containsField(errs, "worker.heartbeat_interval") {
		t.Errorf("expected heartbeat_interval error, got %v", errs)
	}
}

func TestValidate_NegativeShutdownGracePeriod(t *testing.T) {
	cfg := Default()
	cfg.NATS.URL = "nats://localhost:4222"
	cfg.Worker.ShutdownGracePeriod = -1

	errs := Validate(cfg)
	if !containsField(errs, "worker.shutdown_grace_period") {
		t.Errorf("expected shutdown_grace_period error, got %v", errs)
	}
}

func TestValidate_UnrecognizedLogLevel(t *testing.T) {
	cfg := Default()
	cfg.NATS.URL = "nats://localhost:4222"
	cfg.Log.Level = "verbose"

	errs := Validate(cfg)
	if !containsField(errs, "log.level") {
		t.Errorf("expected log.level error, got %v", errs)
	}
}

func TestValidate_UnrecognizedLogFormat(t *testing.T) {
	cfg := Default()
	cfg.NATS.URL = "nats://localhost:4222"
	cfg.Log.Format = "csv"

	errs := Validate(cfg)
	if !containsField(errs, "log.format") {
		t.Errorf("expected log.format error, got %v", errs)
	}
}

func TestValidate_MultipleErrors(t *testing.T) {
	cfg := Default()
	cfg.NATS.URL = ""
	cfg.Discovery.EnableMDNS = false
	cfg.Worker.MaxConcurrentTasks = 0
	cfg.Worker.DataDir = ""
	cfg.Log.Level = "bad"
	cfg.Log.Format = "bad"

	errs := Validate(cfg)
	if len(errs) < 4 {
		t.Errorf("expected at least 4 errors for broken config, got %d: %v", len(errs), errs)
	}
}

// ── Worker ID persistence ─────────────────────────────────────────────────────
// Core tests live in workerid_test.go. These tests exercise the integration
// between Load and the persistence layer.

func TestLoad_WorkerIDNotAffectedByLoad(t *testing.T) {
	// Load should not create the worker.id file — that happens in start.go.
	dir := t.TempDir()
	yaml := "worker:\n  data_dir: '" + dir + "'\n"
	f := writeTempYAML(t, yaml)

	if _, err := Load(f, FlagOverrides{}); err != nil {
		t.Fatalf("Load: %v", err)
	}

	idFile := filepath.Join(dir, workerIDFilename)
	if _, err := os.Stat(idFile); err == nil {
		t.Error("Load must not create worker.id — that is the start command's responsibility")
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func writeTempYAML(t *testing.T, content string) string {
	t.Helper()
	return writeTempFile(t, "sqi-worker.yaml", []byte(content))
}

func writeTempFile(t *testing.T, name string, content []byte) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return p
}

func containsField(errs []ValidationError, field string) bool {
	for _, e := range errs {
		if e.Field == field {
			return true
		}
	}
	return false
}

func assertStr(t *testing.T, label, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %q, want %q", label, got, want)
	}
}

func assertInt(t *testing.T, label string, got, want int) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %d, want %d", label, got, want)
	}
}

func assertDuration(t *testing.T, label string, got, want time.Duration) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %v, want %v", label, got, want)
	}
}
