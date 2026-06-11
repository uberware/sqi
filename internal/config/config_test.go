// SPDX-License-Identifier: AGPL-3.0-or-later

package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/config"
)

// ── DefaultConfig ─────────────────────────────────────────────────────────────

func TestDefaultConfig(t *testing.T) {
	cfg := config.DefaultConfig()

	check := func(field, got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("%s: got %q, want %q", field, got, want)
		}
	}
	checkInt := func(field string, got, want int) {
		t.Helper()
		if got != want {
			t.Errorf("%s: got %d, want %d", field, got, want)
		}
	}
	checkDur := func(field string, got, want time.Duration) {
		t.Helper()
		if got != want {
			t.Errorf("%s: got %s, want %s", field, got, want)
		}
	}

	check("http.addr", cfg.HTTP.Addr, "0.0.0.0:8080")
	check("nats.addr", cfg.NATS.Addr, "0.0.0.0:4222")
	check("nats.data_dir", cfg.NATS.DataDir, "data/nats")
	checkInt("nats.max_store_mb", cfg.NATS.MaxStoreMB, 1024)
	check("store.sqlite_path", cfg.Store.SQLitePath, "sqi.db")
	check("log.level", cfg.Log.Level, "info")
	check("log.format", cfg.Log.Format, "json")
	checkDur("scheduler.heartbeat_timeout", cfg.Scheduler.HeartbeatTimeout, 30*time.Second)
	checkDur("scheduler.tick_interval", cfg.Scheduler.TickInterval, 500*time.Millisecond)
	checkInt("scheduler.max_tasks_per_worker", cfg.Scheduler.MaxTasksPerWorker, 1)
	if !cfg.Discovery.Enabled {
		t.Error("discovery.enabled: got false, want true")
	}
	check("discovery.instance_name", cfg.Discovery.InstanceName, "sqi-server")
}

// ── Load: defaults when no file ───────────────────────────────────────────────

func TestLoad_NoFile_ReturnsDefaults(t *testing.T) {
	// Run from a temp dir so no sqi-server.yaml is accidentally found.
	t.Chdir(t.TempDir())

	cfg, err := config.Load("", config.FlagOverrides{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.HTTP.Addr != "0.0.0.0:8080" {
		t.Errorf("http.addr: got %q, want default", cfg.HTTP.Addr)
	}
}

// ── Load: explicit missing file is an error ───────────────────────────────────

func TestLoad_ExplicitMissingFile_Errors(t *testing.T) {
	_, err := config.Load("/no/such/file.yaml", config.FlagOverrides{})
	if err == nil {
		t.Fatal("expected error for missing explicit config file, got nil")
	}
}

// ── Load: YAML file merge ─────────────────────────────────────────────────────

func TestLoad_FileOverridesDefaults(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "sqi-server.yaml")
	yaml := `
http:
  addr: "127.0.0.1:9090"
log:
  level: "debug"
  format: "text"
nats:
  max_store_mb: 512
scheduler:
  heartbeat_timeout: "1m"
  max_tasks_per_worker: 4
discovery:
  enabled: false
  instance_name: "test-node"
`
	if err := os.WriteFile(f, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(f, config.FlagOverrides{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.HTTP.Addr != "127.0.0.1:9090" {
		t.Errorf("http.addr: got %q", cfg.HTTP.Addr)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("log.level: got %q", cfg.Log.Level)
	}
	if cfg.Log.Format != "text" {
		t.Errorf("log.format: got %q", cfg.Log.Format)
	}
	if cfg.NATS.MaxStoreMB != 512 {
		t.Errorf("nats.max_store_mb: got %d", cfg.NATS.MaxStoreMB)
	}
	if cfg.Scheduler.HeartbeatTimeout != time.Minute {
		t.Errorf("scheduler.heartbeat_timeout: got %s", cfg.Scheduler.HeartbeatTimeout)
	}
	if cfg.Scheduler.MaxTasksPerWorker != 4 {
		t.Errorf("scheduler.max_tasks_per_worker: got %d", cfg.Scheduler.MaxTasksPerWorker)
	}
	if cfg.Discovery.Enabled {
		t.Error("discovery.enabled: got true, want false")
	}
	if cfg.Discovery.InstanceName != "test-node" {
		t.Errorf("discovery.instance_name: got %q", cfg.Discovery.InstanceName)
	}
	// Unset fields should keep defaults.
	if cfg.NATS.Addr != "0.0.0.0:4222" {
		t.Errorf("nats.addr: expected default, got %q", cfg.NATS.Addr)
	}
}

// ── Load: env var override ────────────────────────────────────────────────────

func TestLoad_EnvOverridesDefaults(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("SQI_HTTP_ADDR", "0.0.0.0:7777")
	t.Setenv("SQI_LOG_LEVEL", "warn")
	t.Setenv("SQI_NATS_MAX_STORE_MB", "256")
	t.Setenv("SQI_SCHEDULER_TICK_INTERVAL", "2s")
	t.Setenv("SQI_DISCOVERY_ENABLED", "false")

	cfg, err := config.Load("", config.FlagOverrides{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.HTTP.Addr != "0.0.0.0:7777" {
		t.Errorf("http.addr: got %q", cfg.HTTP.Addr)
	}
	if cfg.Log.Level != "warn" {
		t.Errorf("log.level: got %q", cfg.Log.Level)
	}
	if cfg.NATS.MaxStoreMB != 256 {
		t.Errorf("nats.max_store_mb: got %d", cfg.NATS.MaxStoreMB)
	}
	if cfg.Scheduler.TickInterval != 2*time.Second {
		t.Errorf("scheduler.tick_interval: got %s", cfg.Scheduler.TickInterval)
	}
	if cfg.Discovery.Enabled {
		t.Error("discovery.enabled: got true, want false")
	}
}

// ── Load: flag override wins over env ────────────────────────────────────────

func TestLoad_FlagWinsOverEnv(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("SQI_LOG_LEVEL", "warn")
	t.Setenv("SQI_HTTP_ADDR", "0.0.0.0:9000")

	cfg, err := config.Load("", config.FlagOverrides{
		LogLevel: "error",
		HTTPAddr: "127.0.0.1:8888",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Log.Level != "error" {
		t.Errorf("log.level: flag should win; got %q", cfg.Log.Level)
	}
	if cfg.HTTP.Addr != "127.0.0.1:8888" {
		t.Errorf("http.addr: flag should win; got %q", cfg.HTTP.Addr)
	}
}

// ── Load: precedence chain (file < env < flag) ────────────────────────────────

func TestLoad_PrecedenceChain(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "cfg.yaml")
	if err := os.WriteFile(f, []byte("log:\n  level: debug\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SQI_LOG_LEVEL", "warn") // env beats file

	cfg, err := config.Load(f, config.FlagOverrides{LogLevel: "error"}) // flag beats env
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Log.Level != "error" {
		t.Errorf("log.level: expected flag value 'error', got %q", cfg.Log.Level)
	}
}

// ── Validate: valid default config passes ────────────────────────────────────

func TestValidate_DefaultConfigPasses(t *testing.T) {
	errs := config.Validate(config.DefaultConfig())
	if len(errs) != 0 {
		t.Errorf("expected no errors for default config, got %d: %v", len(errs), errs)
	}
}

// ── Validate: catches all bad fields in one pass ──────────────────────────────

func TestValidate_ReturnsAllErrors(t *testing.T) {
	bad := config.Config{} // zero value — everything invalid

	errs := config.Validate(bad)
	if len(errs) == 0 {
		t.Fatal("expected validation errors for zero config, got none")
	}

	fields := make(map[string]bool, len(errs))
	for _, e := range errs {
		fields[e.Field] = true
	}

	required := []string{
		"http.addr",
		"nats.addr",
		"nats.data_dir",
		"nats.max_store_mb",
		"store.sqlite_path",
		"log.level",
		"log.format",
		"scheduler.heartbeat_timeout",
		"scheduler.tick_interval",
		"scheduler.max_tasks_per_worker",
		"discovery.instance_name",
	}
	for _, f := range required {
		if !fields[f] {
			t.Errorf("expected validation error for field %q, not found in: %v", f, errs)
		}
	}
}

// ── Validate: individual field errors ────────────────────────────────────────

func TestValidate_FieldErrors(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(*config.Config)
		wantField string
	}{
		{
			name:      "bad http addr",
			mutate:    func(c *config.Config) { c.HTTP.Addr = "not-an-addr" },
			wantField: "http.addr",
		},
		{
			name:      "bad nats addr",
			mutate:    func(c *config.Config) { c.NATS.Addr = "not-an-addr" },
			wantField: "nats.addr",
		},
		{
			name:      "empty nats data dir",
			mutate:    func(c *config.Config) { c.NATS.DataDir = "" },
			wantField: "nats.data_dir",
		},
		{
			name:      "zero nats max store mb",
			mutate:    func(c *config.Config) { c.NATS.MaxStoreMB = 0 },
			wantField: "nats.max_store_mb",
		},
		{
			name:      "empty sqlite path",
			mutate:    func(c *config.Config) { c.Store.SQLitePath = "" },
			wantField: "store.sqlite_path",
		},
		{
			name:      "bad log level",
			mutate:    func(c *config.Config) { c.Log.Level = "verbose" },
			wantField: "log.level",
		},
		{
			name:      "bad log format",
			mutate:    func(c *config.Config) { c.Log.Format = "xml" },
			wantField: "log.format",
		},
		{
			name:      "zero heartbeat timeout",
			mutate:    func(c *config.Config) { c.Scheduler.HeartbeatTimeout = 0 },
			wantField: "scheduler.heartbeat_timeout",
		},
		{
			name:      "zero tick interval",
			mutate:    func(c *config.Config) { c.Scheduler.TickInterval = 0 },
			wantField: "scheduler.tick_interval",
		},
		{
			name:      "max tasks per worker zero",
			mutate:    func(c *config.Config) { c.Scheduler.MaxTasksPerWorker = 0 },
			wantField: "scheduler.max_tasks_per_worker",
		},
		{
			name:      "empty discovery instance name",
			mutate:    func(c *config.Config) { c.Discovery.InstanceName = "" },
			wantField: "discovery.instance_name",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			tc.mutate(&cfg)
			errs := config.Validate(cfg)
			found := false
			for _, e := range errs {
				if e.Field == tc.wantField {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected validation error for field %q; got: %v", tc.wantField, errs)
			}
		})
	}
}

// ── Load: empty FlagOverrides does not clobber env ───────────────────────────
// Regression: flag defaults must not shadow env vars. Callers should only
// populate FlagOverrides for flags the user explicitly set.

func TestLoad_EmptyFlagsDoNotClobberEnv(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("SQI_LOG_LEVEL", "debug")

	// Pass zero FlagOverrides — simulates no flags explicitly set.
	cfg, err := config.Load("", config.FlagOverrides{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("log.level: env var should win over empty flag override; got %q", cfg.Log.Level)
	}
}

// ── Validate: invalid YAML file returns error ────────────────────────────────

func TestLoad_MalformedYAML_Errors(t *testing.T) {
	f := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(f, []byte(":\tnot: valid: yaml:::"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := config.Load(f, config.FlagOverrides{})
	if err == nil {
		t.Fatal("expected error for malformed YAML, got nil")
	}
}
