// SPDX-License-Identifier: AGPL-3.0-or-later

package config_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

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
	checkDur("scheduler.offline_worker_retention", cfg.Scheduler.OfflineWorkerRetention, 24*time.Hour)
	if !cfg.Discovery.Enabled {
		t.Error("discovery.enabled: got false, want true")
	}
	check("discovery.instance_name", cfg.Discovery.InstanceName, "sqi-server")
	if !cfg.OpenJD.EnforceLimits {
		t.Error("openjd.enforce_limits: got false, want true")
	}
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

// TestLoad_EnforceLimitsFlagOverride verifies the --openjd-enforce-limits flag
// (a *bool override) wins over the SQI_OPENJD_ENFORCE_LIMITS env var, and that a
// nil override leaves the env/default value intact.
func TestLoad_EnforceLimitsFlagOverride(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("SQI_OPENJD_ENFORCE_LIMITS", "true")

	// Explicit false flag turns the (env-true) gate off.
	disabled := false
	cfg, err := config.Load("", config.FlagOverrides{EnforceLimits: &disabled})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.OpenJD.EnforceLimits {
		t.Error("openjd.enforce_limits: --openjd-enforce-limits=false should win over env=true")
	}

	// A nil override must NOT clobber the env value.
	cfg, err = config.Load("", config.FlagOverrides{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.OpenJD.EnforceLimits {
		t.Error("openjd.enforce_limits: nil flag override must leave env=true intact")
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
		"scheduler.default_max_attempts",
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

// ── OpenJD: defaults and overrides ───────────────────────────────────────────
//
// The EnforceLimits default (true) is asserted in TestDefaultConfig.

func TestLoad_OpenJD_FileOverride_EnforceLimitsFalse(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "sqi-server.yaml")
	yaml := "openjd:\n  enforce_limits: false\n"
	if err := os.WriteFile(f, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(f, config.FlagOverrides{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.OpenJD.EnforceLimits {
		t.Error("openjd.enforce_limits: file override to false not applied")
	}
}

func TestLoad_OpenJD_EnvOverride_EnforceLimitsFalse(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("SQI_OPENJD_ENFORCE_LIMITS", "false")
	cfg, err := config.Load("", config.FlagOverrides{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.OpenJD.EnforceLimits {
		t.Error("openjd.enforce_limits: env SQI_OPENJD_ENFORCE_LIMITS=false not applied")
	}
}

func TestLoad_OpenJD_EnvOverride_EnforceLimitsTrue(t *testing.T) {
	// The default is already true; this verifies an explicit "true" env var also works.
	t.Chdir(t.TempDir())
	t.Setenv("SQI_OPENJD_ENFORCE_LIMITS", "1")
	cfg, err := config.Load("", config.FlagOverrides{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.OpenJD.EnforceLimits {
		t.Error("openjd.enforce_limits: env SQI_OPENJD_ENFORCE_LIMITS=1 not applied")
	}
}

func TestLoad_OpenJD_DefaultUnchangedWhenNotInFile(t *testing.T) {
	// A config file that sets other fields should leave enforce_limits at its default.
	dir := t.TempDir()
	f := filepath.Join(dir, "sqi-server.yaml")
	yaml := "log:\n  level: debug\n"
	if err := os.WriteFile(f, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(f, config.FlagOverrides{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.OpenJD.EnforceLimits {
		t.Error("openjd.enforce_limits: should remain true when not specified in config file")
	}
}

// ── Diagnostics: defaults and env overrides ──────────────────────────────────

func TestLoad_DiagnosticsDefaults(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg, err := config.Load("", config.FlagOverrides{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Diagnostics.BufferSize <= 0 {
		t.Fatalf("buffer size default should be positive, got %d", cfg.Diagnostics.BufferSize)
	}
}

func TestLoad_DiagnosticsBufferSizeEnvOverride(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("SQI_DIAGNOSTICS_BUFFER_SIZE", "500")
	cfg, err := config.Load("", config.FlagOverrides{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Diagnostics.BufferSize != 500 {
		t.Fatalf("buffer size = %d", cfg.Diagnostics.BufferSize)
	}
}

func TestLoad_DiagnosticsBufferSizeZeroDisables(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("SQI_DIAGNOSTICS_BUFFER_SIZE", "0")
	cfg, err := config.Load("", config.FlagOverrides{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Diagnostics.BufferSize != 0 {
		t.Fatalf("buffer size should be 0 (disabled), got %d", cfg.Diagnostics.BufferSize)
	}
}

func TestValidate_DiagnosticsNegativeBufferSizeRejected(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Diagnostics.BufferSize = -1
	if errs := config.Validate(cfg); len(errs) == 0 {
		t.Fatal("negative buffer size should be a validation error")
	}
}

func TestValidate_DiagnosticsZeroBufferSizeAllowed(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Diagnostics.BufferSize = 0
	for _, e := range config.Validate(cfg) {
		if e.Field == "diagnostics.buffer_size" {
			t.Fatalf("zero buffer size (disabled) should be valid, got %v", e)
		}
	}
}

// ── Scheduler: job_retention defaults and env overrides ──────────────────────

func TestDefaultConfig_PresetLibraryURL(t *testing.T) {
	if got := config.DefaultConfig().PresetLibrary.URL; got != "https://uberware.github.io/sqi-presets/index.json" {
		t.Fatalf("unexpected default preset library URL: %q", got)
	}
}

func TestDefaultConfig_JobRetention(t *testing.T) {
	cfg := config.DefaultConfig()
	if cfg.Scheduler.JobRetention != 7*24*time.Hour {
		t.Fatalf("JobRetention default = %v, want 168h", cfg.Scheduler.JobRetention)
	}
	if cfg.Scheduler.JobRetentionIncludeFailed {
		t.Fatalf("JobRetentionIncludeFailed default = true, want false")
	}
}

func TestLoad_JobRetentionEnvOverride(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("SQI_SCHEDULER_JOB_RETENTION", "48h")
	t.Setenv("SQI_SCHEDULER_JOB_RETENTION_INCLUDE_FAILED", "true")
	cfg, err := config.Load("", config.FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Scheduler.JobRetention != 48*time.Hour {
		t.Fatalf("JobRetention = %v, want 48h", cfg.Scheduler.JobRetention)
	}
	if !cfg.Scheduler.JobRetentionIncludeFailed {
		t.Fatalf("JobRetentionIncludeFailed = false, want true")
	}
}

// ── Scheduler: unschedulable_grace defaults and env overrides ────────────────

func TestDefaultConfig_UnschedulableGrace(t *testing.T) {
	cfg := config.DefaultConfig()
	if cfg.Scheduler.UnschedulableGrace != 30*time.Second {
		t.Fatalf("UnschedulableGrace default = %v, want 30s", cfg.Scheduler.UnschedulableGrace)
	}
}

func TestLoad_UnschedulableGraceEnv(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("SQI_SCHEDULER_UNSCHEDULABLE_GRACE", "45s")
	cfg, err := config.Load("", config.FlagOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Scheduler.UnschedulableGrace != 45*time.Second {
		t.Errorf("got %v, want 45s", cfg.Scheduler.UnschedulableGrace)
	}
}

func TestValidate_UnschedulableGraceNegativeRejected(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Scheduler.UnschedulableGrace = -1 * time.Second
	if errs := config.Validate(cfg); len(errs) == 0 {
		t.Fatal("negative unschedulable_grace should be a validation error")
	}
}

func TestValidate_UnschedulableGraceZeroAllowed(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Scheduler.UnschedulableGrace = 0
	for _, e := range config.Validate(cfg) {
		if e.Field == "scheduler.unschedulable_grace" {
			t.Fatalf("zero unschedulable_grace (disabled) should be valid, got %v", e)
		}
	}
}

// ── Scheduler: retry-policy defaults (server-level backstop) ─────────────────
//
// default_max_attempts / retry_delay / default_failure_limit are the
// server-level fallback tier of the layered retry policy (Server -> Farm ->
// Queue -> Job). This task only adds the config plumbing; RetryPolicy itself
// is introduced in a later task.

func TestDefaultConfig_RetryDefaults(t *testing.T) {
	cfg := config.DefaultConfig()
	if cfg.Scheduler.DefaultMaxAttempts != 3 {
		t.Errorf("scheduler.default_max_attempts: got %d, want 3", cfg.Scheduler.DefaultMaxAttempts)
	}
	if cfg.Scheduler.RetryDelay != 30*time.Second {
		t.Errorf("scheduler.retry_delay: got %v, want 30s", cfg.Scheduler.RetryDelay)
	}
	if cfg.Scheduler.DefaultFailureLimit != 0 {
		t.Errorf("scheduler.default_failure_limit: got %d, want 0", cfg.Scheduler.DefaultFailureLimit)
	}
}

func TestLoad_RetryDefaultsEnvOverride(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("SQI_SCHEDULER_DEFAULT_MAX_ATTEMPTS", "5")
	t.Setenv("SQI_SCHEDULER_RETRY_DELAY", "10s")
	t.Setenv("SQI_SCHEDULER_DEFAULT_FAILURE_LIMIT", "2")

	cfg, err := config.Load("", config.FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Scheduler.DefaultMaxAttempts != 5 {
		t.Errorf("scheduler.default_max_attempts: got %d, want 5", cfg.Scheduler.DefaultMaxAttempts)
	}
	if cfg.Scheduler.RetryDelay != 10*time.Second {
		t.Errorf("scheduler.retry_delay: got %v, want 10s", cfg.Scheduler.RetryDelay)
	}
	if cfg.Scheduler.DefaultFailureLimit != 2 {
		t.Errorf("scheduler.default_failure_limit: got %d, want 2", cfg.Scheduler.DefaultFailureLimit)
	}
}

func TestLoad_RetryDefaultsFileOverride(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "sqi-server.yaml")
	yaml := "scheduler:\n  default_max_attempts: 6\n  retry_delay: \"1m\"\n  default_failure_limit: 3\n"
	if err := os.WriteFile(f, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(f, config.FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Scheduler.DefaultMaxAttempts != 6 {
		t.Errorf("scheduler.default_max_attempts: got %d, want 6", cfg.Scheduler.DefaultMaxAttempts)
	}
	if cfg.Scheduler.RetryDelay != time.Minute {
		t.Errorf("scheduler.retry_delay: got %v, want 1m", cfg.Scheduler.RetryDelay)
	}
	if cfg.Scheduler.DefaultFailureLimit != 3 {
		t.Errorf("scheduler.default_failure_limit: got %d, want 3", cfg.Scheduler.DefaultFailureLimit)
	}
}

func TestValidate_DefaultMaxAttemptsBelowOneRejected(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Scheduler.DefaultMaxAttempts = 0
	errs := config.Validate(cfg)
	found := false
	for _, e := range errs {
		if e.Field == "scheduler.default_max_attempts" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected validation error for scheduler.default_max_attempts, got: %v", errs)
	}
}

func TestValidate_RetryDelayNegativeRejected(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Scheduler.RetryDelay = -1 * time.Second
	errs := config.Validate(cfg)
	found := false
	for _, e := range errs {
		if e.Field == "scheduler.retry_delay" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected validation error for scheduler.retry_delay, got: %v", errs)
	}
}

func TestValidate_RetryDelayZeroAllowed(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Scheduler.RetryDelay = 0
	for _, e := range config.Validate(cfg) {
		if e.Field == "scheduler.retry_delay" {
			t.Fatalf("zero retry_delay (immediate) should be valid, got %v", e)
		}
	}
}

func TestValidate_DefaultFailureLimitNegativeRejected(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Scheduler.DefaultFailureLimit = -1
	errs := config.Validate(cfg)
	found := false
	for _, e := range errs {
		if e.Field == "scheduler.default_failure_limit" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected validation error for scheduler.default_failure_limit, got: %v", errs)
	}
}

func TestValidate_DefaultFailureLimitZeroAllowed(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Scheduler.DefaultFailureLimit = 0
	for _, e := range config.Validate(cfg) {
		if e.Field == "scheduler.default_failure_limit" {
			t.Fatalf("zero default_failure_limit (off) should be valid, got %v", e)
		}
	}
}

// ── Auth: opt-in gate (file, env, flag) ──────────────────────────────────────

func TestLoad_AuthEnabled_DefaultFalse(t *testing.T) {
	cfg := config.DefaultConfig()
	if cfg.Auth.Enabled {
		t.Error("default Auth.Enabled = true, want false")
	}
}

func TestLoad_AuthEnabled_EnvFlips(t *testing.T) {
	t.Setenv("SQI_AUTH_ENABLED", "true")
	cfg, err := config.Load("", config.FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Auth.Enabled {
		t.Error("Auth.Enabled = false after SQI_AUTH_ENABLED=true, want true")
	}
}

func TestLoad_AuthEnabled_FlagFlips(t *testing.T) {
	enabled := true
	cfg, err := config.Load("", config.FlagOverrides{AuthEnabled: &enabled})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Auth.Enabled {
		t.Error("Auth.Enabled = false after --auth-enabled=true, want true")
	}
}

func TestLoad_AuthEnabled_UnsetFlagDoesNotClobberEnv(t *testing.T) {
	t.Setenv("SQI_AUTH_ENABLED", "true")
	cfg, err := config.Load("", config.FlagOverrides{AuthEnabled: nil})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Auth.Enabled {
		t.Error("unset --auth-enabled clobbered SQI_AUTH_ENABLED=true")
	}
}

// ── Env parsing: malformed security-relevant values fail loud ────────────────

// A trailing space must be trimmed, not treated as a parse failure, so a
// well-meaning "true " still enables auth rather than silently reverting.
func TestLoad_AuthEnabled_TrailingSpaceEnables(t *testing.T) {
	t.Setenv("SQI_AUTH_ENABLED", "true ")
	cfg, err := config.Load("", config.FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Auth.Enabled {
		t.Error("Auth.Enabled = false after SQI_AUTH_ENABLED=\"true \", want true")
	}
}

// An unparseable non-empty bool must be a hard load error that names the key,
// not a silent fail-open to the (disabled) default.
func TestLoad_AuthEnabled_GarbageValueErrors(t *testing.T) {
	t.Setenv("SQI_AUTH_ENABLED", "enabled")
	_, err := config.Load("", config.FlagOverrides{})
	if err == nil {
		t.Fatal("Load: got nil error for SQI_AUTH_ENABLED=enabled, want an error")
	}
	if !strings.Contains(err.Error(), "SQI_AUTH_ENABLED") {
		t.Errorf("error %q does not name the offending env key SQI_AUTH_ENABLED", err)
	}
}

// An unparseable non-empty duration must likewise error and name the key.
func TestLoad_AuthSessionTTL_GarbageValueErrors(t *testing.T) {
	t.Setenv("SQI_AUTH_SESSION_TTL", "abc")
	_, err := config.Load("", config.FlagOverrides{})
	if err == nil {
		t.Fatal("Load: got nil error for SQI_AUTH_SESSION_TTL=abc, want an error")
	}
	if !strings.Contains(err.Error(), "SQI_AUTH_SESSION_TTL") {
		t.Errorf("error %q does not name the offending env key SQI_AUTH_SESSION_TTL", err)
	}
}

// An unparseable non-empty int must error and name the key.
func TestLoad_IntEnv_GarbageValueErrors(t *testing.T) {
	t.Setenv("SQI_NATS_MAX_STORE_MB", "lots")
	_, err := config.Load("", config.FlagOverrides{})
	if err == nil {
		t.Fatal("Load: got nil error for SQI_NATS_MAX_STORE_MB=lots, want an error")
	}
	if !strings.Contains(err.Error(), "SQI_NATS_MAX_STORE_MB") {
		t.Errorf("error %q does not name the offending env key SQI_NATS_MAX_STORE_MB", err)
	}
}

// An unset var stays a no-op: the default survives and Load succeeds.
func TestLoad_UnsetEnv_KeepsDefault(t *testing.T) {
	cfg, err := config.Load("", config.FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.Enabled {
		t.Error("Auth.Enabled = true with SQI_AUTH_ENABLED unset, want the false default")
	}
}

// Valid bool/int/duration values still parse and apply after the fail-loud
// change. Table-driven over the accepted bool tokens plus an int and a
// duration.
func TestLoad_ValidEnvValuesStillApply(t *testing.T) {
	boolTokens := []struct {
		value string
		want  bool
	}{
		{"1", true},
		{"true", true},
		{"YES", true},
		{"On", true},
		{"0", false},
		{"false", false},
		{"NO", false},
		{"Off", false},
	}
	for _, tt := range boolTokens {
		t.Run("bool_"+tt.value, func(t *testing.T) {
			t.Setenv("SQI_AUTH_ENABLED", tt.value)
			cfg, err := config.Load("", config.FlagOverrides{})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Auth.Enabled != tt.want {
				t.Errorf("Auth.Enabled = %v for %q, want %v", cfg.Auth.Enabled, tt.value, tt.want)
			}
		})
	}

	t.Run("int", func(t *testing.T) {
		t.Setenv("SQI_NATS_MAX_STORE_MB", "256")
		cfg, err := config.Load("", config.FlagOverrides{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.NATS.MaxStoreMB != 256 {
			t.Errorf("NATS.MaxStoreMB = %d, want 256", cfg.NATS.MaxStoreMB)
		}
	})

	t.Run("duration", func(t *testing.T) {
		t.Setenv("SQI_AUTH_SESSION_TTL", "48h")
		cfg, err := config.Load("", config.FlagOverrides{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Auth.Session.TTL != 48*time.Hour {
			t.Errorf("Auth.Session.TTL = %s, want 48h", cfg.Auth.Session.TTL)
		}
	})
}

// ── Auth: validate_job_owner (default, env, flag) ────────────────────────────

func TestAuthValidateJobOwnerDefaultsTrue(t *testing.T) {
	cfg := config.DefaultConfig()
	if !cfg.Auth.ValidateJobOwner {
		t.Error("Auth.ValidateJobOwner = false, want true by default")
	}
}

func TestAuthValidateJobOwnerFromEnv(t *testing.T) {
	t.Setenv("SQI_AUTH_VALIDATE_JOB_OWNER", "false")
	cfg, err := config.Load("", config.FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.ValidateJobOwner {
		t.Error("env did not disable ValidateJobOwner")
	}
}

// An unset *bool flag must not clobber a file/env value.
func TestAuthValidateJobOwnerFlagUnsetDoesNotClobber(t *testing.T) {
	t.Setenv("SQI_AUTH_VALIDATE_JOB_OWNER", "false")
	cfg, err := config.Load("", config.FlagOverrides{ValidateJobOwner: nil})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.ValidateJobOwner {
		t.Error("unset flag clobbered the env value")
	}
}

func TestAuthValidateJobOwnerFlagFlips(t *testing.T) {
	disabled := false
	cfg, err := config.Load("", config.FlagOverrides{ValidateJobOwner: &disabled})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.ValidateJobOwner {
		t.Error("Auth.ValidateJobOwner = true after --auth-validate-job-owner=false, want false")
	}
}

// ── Auth: session + bootstrap (defaults, file, env, validation) ──────────────

func TestAuthConfigDefaults(t *testing.T) {
	c := config.DefaultConfig()
	if c.Auth.Enabled {
		t.Fatal("auth must default disabled")
	}
	if c.Auth.Session.TTL != 168*time.Hour {
		t.Fatalf("session TTL default = %v, want 168h", c.Auth.Session.TTL)
	}
	if c.Auth.Session.CookieName != "sqi_session" {
		t.Fatalf("cookie name default = %q, want sqi_session", c.Auth.Session.CookieName)
	}
	if c.Auth.Session.CookieSecure != "auto" {
		t.Fatalf("cookie secure default = %q, want auto", c.Auth.Session.CookieSecure)
	}
	if c.Auth.Bootstrap.Username != "" || c.Auth.Bootstrap.Password != "" {
		t.Fatalf("bootstrap credentials default = %+v, want empty", c.Auth.Bootstrap)
	}
	if c.Auth.LDAP.Enabled {
		t.Fatal("ldap must default disabled")
	}
	if c.Auth.LDAP.Timeout != 10*time.Second {
		t.Fatalf("ldap timeout default = %v, want 10s", c.Auth.LDAP.Timeout)
	}
	if c.Auth.LDAP.UserFilter != "(sAMAccountName=%s)" {
		t.Fatalf("ldap user_filter default = %q, want (sAMAccountName=%%s)", c.Auth.LDAP.UserFilter)
	}
	if c.Auth.LDAP.UsernameAttr != "sAMAccountName" {
		t.Fatalf("ldap username_attr default = %q, want sAMAccountName", c.Auth.LDAP.UsernameAttr)
	}
	if c.Auth.LDAP.DisplayNameAttr != "displayName" {
		t.Fatalf("ldap display_name_attr default = %q, want displayName", c.Auth.LDAP.DisplayNameAttr)
	}
	// unique_id_attr deliberately has no default: "objectGUID" and
	// "entryUUID" are each wrong on the other server family, and a default
	// would let a deployment start with the wrong one rather than say so.
	if c.Auth.LDAP.UniqueIDAttr != "" {
		t.Fatalf("ldap unique_id_attr default = %q, want empty (it must be stated explicitly)",
			c.Auth.LDAP.UniqueIDAttr)
	}
	if c.Auth.LDAP.RoleSource != "directory" {
		t.Fatalf("ldap role_source default = %q, want directory", c.Auth.LDAP.RoleSource)
	}
	if c.Auth.LDAP.DefaultRole != "read-only" {
		t.Fatalf("ldap default_role default = %q, want read-only", c.Auth.LDAP.DefaultRole)
	}
	if c.Auth.LDAP.URL != "" || c.Auth.LDAP.BindDN != "" || c.Auth.LDAP.BindPassword != "" ||
		c.Auth.LDAP.BaseDN != "" || c.Auth.LDAP.UserDNTemplate != "" || c.Auth.LDAP.CAFile != "" {
		t.Fatalf("ldap unset string fields default = %+v, want all empty", c.Auth.LDAP)
	}
	if c.Auth.LDAP.StartTLS || c.Auth.LDAP.TLSSkipVerify || c.Auth.LDAP.NestedGroups {
		t.Fatalf("ldap unset bool fields default = %+v, want all false", c.Auth.LDAP)
	}
	if len(c.Auth.LDAP.RoleMap) != 0 {
		t.Fatalf("ldap role_map default = %+v, want empty", c.Auth.LDAP.RoleMap)
	}
}

func TestAuthEnvOverrides(t *testing.T) {
	t.Setenv("SQI_AUTH_ENABLED", "true")
	t.Setenv("SQI_AUTH_SESSION_TTL", "24h")
	t.Setenv("SQI_AUTH_SESSION_COOKIE_NAME", "custom_session")
	t.Setenv("SQI_AUTH_SESSION_COOKIE_SECURE", "false")
	t.Setenv("SQI_AUTH_BOOTSTRAP_USERNAME", "admin")
	t.Setenv("SQI_AUTH_BOOTSTRAP_PASSWORD", "s3cret")

	c, err := config.Load("", config.FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.Auth.Enabled {
		t.Error("Auth.Enabled: got false, want true")
	}
	if c.Auth.Session.TTL != 24*time.Hour {
		t.Errorf("Auth.Session.TTL: got %v, want 24h", c.Auth.Session.TTL)
	}
	if c.Auth.Session.CookieName != "custom_session" {
		t.Errorf("Auth.Session.CookieName: got %q, want custom_session", c.Auth.Session.CookieName)
	}
	if c.Auth.Session.CookieSecure != "false" {
		t.Errorf("Auth.Session.CookieSecure: got %q, want false", c.Auth.Session.CookieSecure)
	}
	if c.Auth.Bootstrap.Username != "admin" {
		t.Errorf("Auth.Bootstrap.Username: got %q, want admin", c.Auth.Bootstrap.Username)
	}
	if c.Auth.Bootstrap.Password != "s3cret" {
		t.Errorf("Auth.Bootstrap.Password: got %q, want s3cret", c.Auth.Bootstrap.Password)
	}
}

func TestLoad_AuthSessionFileOverride(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "sqi-server.yaml")
	yaml := `
auth:
  enabled: true
  session:
    ttl: "48h"
    cookie_name: "file_session"
    cookie_secure: "true"
`
	if err := os.WriteFile(f, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(f, config.FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Auth.Enabled {
		t.Error("auth.enabled: got false, want true")
	}
	if cfg.Auth.Session.TTL != 48*time.Hour {
		t.Errorf("auth.session.ttl: got %v, want 48h", cfg.Auth.Session.TTL)
	}
	if cfg.Auth.Session.CookieName != "file_session" {
		t.Errorf("auth.session.cookie_name: got %q, want file_session", cfg.Auth.Session.CookieName)
	}
	if cfg.Auth.Session.CookieSecure != "true" {
		t.Errorf("auth.session.cookie_secure: got %q, want true", cfg.Auth.Session.CookieSecure)
	}
}

func TestLoad_AuthBootstrapFileOverride(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "sqi-server.yaml")
	yaml := `
auth:
  bootstrap:
    username: "file-admin"
    password: "file-pass"
`
	if err := os.WriteFile(f, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(f, config.FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.Bootstrap.Username != "file-admin" {
		t.Errorf("auth.bootstrap.username: got %q, want file-admin", cfg.Auth.Bootstrap.Username)
	}
	if cfg.Auth.Bootstrap.Password != "file-pass" {
		t.Errorf("auth.bootstrap.password: got %q, want file-pass", cfg.Auth.Bootstrap.Password)
	}
	// Unset session fields must keep defaults (pointer-field layering).
	if cfg.Auth.Session.TTL != 168*time.Hour {
		t.Errorf("auth.session.ttl: expected default, got %v", cfg.Auth.Session.TTL)
	}
	if cfg.Auth.Session.CookieName != "sqi_session" {
		t.Errorf("auth.session.cookie_name: expected default, got %q", cfg.Auth.Session.CookieName)
	}
}

func TestValidate_AuthDisabled_NoErrorsEvenWithBadValues(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Auth.Enabled = false
	cfg.Auth.Session.TTL = 0
	cfg.Auth.Session.CookieSecure = "not-a-valid-value"
	cfg.Auth.Session.CookieName = ""

	errs := config.Validate(cfg)
	for _, e := range errs {
		if strings.HasPrefix(e.Field, "auth.") {
			t.Fatalf("auth disabled must not produce validation errors, got %v", e)
		}
	}
}

func TestValidate_AuthSessionCookieNameMustNotBeEmpty(t *testing.T) {
	tests := []struct {
		name       string
		cookieName string
		wantErr    bool
	}{
		{"default is valid", "sqi_session", false},
		{"custom name is valid", "custom_session", false},
		{"empty is invalid", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.Auth.Enabled = true
			cfg.Auth.Session.CookieName = tt.cookieName

			errs := config.Validate(cfg)
			hasErr := false
			for _, e := range errs {
				if e.Field == "auth.session.cookie_name" {
					hasErr = true
				}
			}
			if hasErr != tt.wantErr {
				t.Fatalf("cookie_name=%q: got error=%v, want %v (errs=%+v)", tt.cookieName, hasErr, tt.wantErr, errs)
			}
		})
	}
}

func TestValidate_AuthSessionTTLMustBePositive(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Auth.Enabled = true
	cfg.Auth.Session.TTL = 0

	errs := config.Validate(cfg)
	found := false
	for _, e := range errs {
		if e.Field == "auth.session.ttl" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected auth.session.ttl validation error, got %+v", errs)
	}
}

func TestValidate_AuthCookieSecure(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"auto is valid", "auto", false},
		{"true is valid", "true", false},
		{"false is valid", "false", false},
		{"empty is invalid", "", true},
		{"uppercase TRUE is invalid", "TRUE", true},
		{"garbage is invalid", "sometimes", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.Auth.Enabled = true
			cfg.Auth.Session.CookieSecure = tt.value

			errs := config.Validate(cfg)
			hasErr := false
			for _, e := range errs {
				if e.Field == "auth.session.cookie_secure" {
					hasErr = true
				}
			}
			if hasErr != tt.wantErr {
				t.Fatalf("cookie_secure=%q: got error=%v, want %v (errs=%+v)", tt.value, hasErr, tt.wantErr, errs)
			}
		})
	}
}

func TestValidate_AuthBootstrapPasswordNeverInErrorMessages(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Auth.Enabled = true
	cfg.Auth.Session.TTL = 0
	cfg.Auth.Session.CookieSecure = "invalid"
	cfg.Auth.Bootstrap.Username = "admin"
	cfg.Auth.Bootstrap.Password = "super-secret-value"

	errs := config.Validate(cfg)
	for _, e := range errs {
		if strings.Contains(e.Field, "super-secret-value") || strings.Contains(e.Message, "super-secret-value") {
			t.Fatalf("bootstrap password leaked into validation error: %+v", e)
		}
	}
}

// ── Auth: bootstrap password redaction on marshal ────────────────────────────

func TestMarshalYAML_BootstrapPasswordRedacted(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Auth.Bootstrap.Username = "admin"
	cfg.Auth.Bootstrap.Password = "hunter2"

	out, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}

	if strings.Contains(string(out), "hunter2") {
		t.Fatalf("marshaled config contains the plaintext bootstrap password:\n%s", out)
	}
	if !strings.Contains(string(out), "<redacted>") {
		t.Fatalf("marshaled config does not contain the <redacted> placeholder:\n%s", out)
	}
}

func TestMarshalYAML_BootstrapPasswordSentinelNeverAppears(t *testing.T) {
	const sentinel = "S3CRET-SENTINEL"

	cfg := config.DefaultConfig()
	cfg.Auth.Bootstrap.Username = "admin"
	cfg.Auth.Bootstrap.Password = sentinel

	out, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}

	if strings.Contains(string(out), sentinel) {
		t.Fatalf("marshaled config leaks the bootstrap password sentinel:\n%s", out)
	}
}

func TestMarshalYAML_BootstrapPasswordEmptyNotRedacted(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Auth.Bootstrap.Username = ""
	cfg.Auth.Bootstrap.Password = ""

	out, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}

	if strings.Contains(string(out), "<redacted>") {
		t.Fatalf("marshaled config redacts an empty bootstrap password:\n%s", out)
	}
}

func TestLoad_AuthBootstrapPasswordSurvivesRedactedMarshal(t *testing.T) {
	// Redaction on marshal must not affect the loaded (unmarshaled) value —
	// round-tripping a redacted dump is not a goal, but loading the real
	// config must still populate the real password.
	t.Setenv("SQI_AUTH_BOOTSTRAP_USERNAME", "admin")
	t.Setenv("SQI_AUTH_BOOTSTRAP_PASSWORD", "s3cret")

	cfg, err := config.Load("", config.FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.Bootstrap.Password != "s3cret" {
		t.Errorf("Auth.Bootstrap.Password: got %q, want s3cret", cfg.Auth.Bootstrap.Password)
	}

	// Marshaling the loaded config must still redact.
	out, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	if strings.Contains(string(out), "s3cret") {
		t.Fatalf("marshaled loaded config leaks bootstrap password:\n%s", out)
	}
}

func TestMarshalYAML_LDAPBindPasswordRedacted(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Auth.LDAP.BindDN = "cn=svc,dc=example,dc=com"
	cfg.Auth.LDAP.BindPassword = "hunter2"

	out, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}

	if strings.Contains(string(out), "hunter2") {
		t.Fatalf("marshaled config contains the plaintext LDAP bind password:\n%s", out)
	}
	if !strings.Contains(string(out), "<redacted>") {
		t.Fatalf("marshaled config does not contain the <redacted> placeholder:\n%s", out)
	}
}

func TestMarshalYAML_LDAPBindPasswordSentinelNeverAppears(t *testing.T) {
	const sentinel = "S3CRET-SENTINEL"

	cfg := config.DefaultConfig()
	cfg.Auth.LDAP.BindDN = "cn=svc,dc=example,dc=com"
	cfg.Auth.LDAP.BindPassword = sentinel

	out, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}

	if strings.Contains(string(out), sentinel) {
		t.Fatalf("marshaled config leaks the LDAP bind password sentinel:\n%s", out)
	}
}

func TestMarshalYAML_LDAPBindPasswordEmptyNotRedacted(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Auth.LDAP.BindDN = ""
	cfg.Auth.LDAP.BindPassword = ""

	out, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}

	if strings.Contains(string(out), "<redacted>") {
		t.Fatalf("marshaled config redacts an empty LDAP bind password:\n%s", out)
	}
}

func TestMarshalYAML_LDAPConfigFieldsSurviveRedaction(t *testing.T) {
	// Guards against the alias-type indirection in LDAPConfig.MarshalYAML
	// silently dropping a field: every non-secret field is set to a distinct
	// recognizable value and must still appear in the marshaled output.
	cfg := config.DefaultConfig()
	cfg.Auth.LDAP = config.LDAPConfig{
		Enabled:         true,
		URL:             "ldaps://directory.example.com:636",
		StartTLS:        true,
		TLSSkipVerify:   true,
		CAFile:          "/etc/sqi/ldap-ca.pem",
		Timeout:         42 * time.Second,
		BindDN:          "cn=svc-sqi,dc=example,dc=com",
		BindPassword:    "hunter2",
		BaseDN:          "dc=example,dc=com",
		UserFilter:      "(uid=%s)",
		NestedGroups:    true,
		UserDNTemplate:  "uid=%s,dc=example,dc=com",
		UsernameAttr:    "uid",
		DisplayNameAttr: "cn",
		UniqueIDAttr:    "objectGUID",
		RoleSource:      "local",
		RoleMap: []config.RoleMappingConfig{
			{Group: "cn=admins,dc=example,dc=com", Role: "admin"},
			{Group: "cn=operators,dc=example,dc=com", Role: "operator"},
		},
		DefaultRole: "read-only",
	}

	out, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	got := string(out)

	wantSubstrings := []string{
		"ldaps://directory.example.com:636",
		"/etc/sqi/ldap-ca.pem",
		"42s",
		"cn=svc-sqi,dc=example,dc=com",
		"dc=example,dc=com",
		"(uid=%s)",
		"uid=%s,dc=example,dc=com",
		"uid",
		"cn",
		"objectGUID",
		"local",
		"cn=admins,dc=example,dc=com",
		"admin",
		"cn=operators,dc=example,dc=com",
		"operator",
		"read-only",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(got, want) {
			t.Errorf("marshaled config missing expected field value %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "<redacted>") {
		t.Fatalf("marshaled config does not redact LDAP bind password:\n%s", got)
	}
}

func TestLoad_AuthLDAPBindPasswordSurvivesRedactedMarshal(t *testing.T) {
	// Redaction on marshal must not affect the loaded (unmarshaled) value —
	// round-tripping a redacted dump is not a goal, but loading the real
	// config must still populate the real password.
	t.Setenv("SQI_AUTH_LDAP_BIND_DN", "cn=svc,dc=example,dc=com")
	t.Setenv("SQI_AUTH_LDAP_BIND_PASSWORD", "s3cret")

	cfg, err := config.Load("", config.FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.LDAP.BindPassword != "s3cret" {
		t.Errorf("Auth.LDAP.BindPassword: got %q, want s3cret", cfg.Auth.LDAP.BindPassword)
	}

	// Marshaling the loaded config must still redact.
	out, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	if strings.Contains(string(out), "s3cret") {
		t.Fatalf("marshaled loaded config leaks LDAP bind password:\n%s", out)
	}
}

// ── Auth: bootstrap username/password pairing validation ─────────────────────

func TestValidate_AuthBootstrapPairing(t *testing.T) {
	tests := []struct {
		name     string
		username string
		password string
		wantErr  bool
	}{
		{"both empty is valid (no bootstrap)", "", "", false},
		{"both set is valid", "admin", "s3cret", false},
		{"username only is invalid", "admin", "", true},
		{"password only is invalid", "", "s3cret", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.Auth.Enabled = true
			cfg.Auth.Bootstrap.Username = tt.username
			cfg.Auth.Bootstrap.Password = tt.password

			errs := config.Validate(cfg)
			hasErr := false
			for _, e := range errs {
				if strings.HasPrefix(e.Field, "auth.bootstrap") {
					hasErr = true
				}
			}
			if hasErr != tt.wantErr {
				t.Fatalf("username=%q password=%q: got error=%v, want %v (errs=%+v)", tt.username, tt.password, hasErr, tt.wantErr, errs)
			}
		})
	}
}

func TestValidate_AuthBootstrapPairing_DisabledAuthNoError(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Auth.Enabled = false
	cfg.Auth.Bootstrap.Username = "admin"
	cfg.Auth.Bootstrap.Password = ""

	errs := config.Validate(cfg)
	for _, e := range errs {
		if strings.HasPrefix(e.Field, "auth.") {
			t.Fatalf("auth disabled must not produce validation errors, got %v", e)
		}
	}
}

func TestValidate_AuthBootstrapPairingErrorNeverContainsPassword(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Auth.Enabled = true
	cfg.Auth.Bootstrap.Username = ""
	cfg.Auth.Bootstrap.Password = "super-secret-value"

	errs := config.Validate(cfg)
	for _, e := range errs {
		if strings.Contains(e.Field, "super-secret-value") || strings.Contains(e.Message, "super-secret-value") {
			t.Fatalf("bootstrap password leaked into validation error: %+v", e)
		}
	}
}

// ── http.cors_origins layering and validation ────────────────────────────────

func TestLoad_CORSOrigins_Layering(t *testing.T) {
	t.Run("default is empty", func(t *testing.T) {
		cfg, err := config.Load("", config.FlagOverrides{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(cfg.HTTP.CORSOrigins) != 0 {
			t.Fatalf("http.cors_origins: got %v, want empty", cfg.HTTP.CORSOrigins)
		}
	})

	t.Run("file sets a list", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "cfg.yaml")
		y := "http:\n  cors_origins:\n    - \"https://file.test\"\n"
		if err := os.WriteFile(f, []byte(y), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(f, config.FlagOverrides{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !slices.Equal(cfg.HTTP.CORSOrigins, []string{"https://file.test"}) {
			t.Fatalf("http.cors_origins: got %v", cfg.HTTP.CORSOrigins)
		}
	})

	t.Run("env parses a comma-separated list and trims spaces", func(t *testing.T) {
		t.Setenv("SQI_HTTP_CORS_ORIGINS", "https://a.test, https://b.test:8443")
		cfg, err := config.Load("", config.FlagOverrides{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{"https://a.test", "https://b.test:8443"}
		if !slices.Equal(cfg.HTTP.CORSOrigins, want) {
			t.Fatalf("http.cors_origins: got %v, want %v", cfg.HTTP.CORSOrigins, want)
		}
	})

	t.Run("empty env leaves the file layer intact", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "cfg.yaml")
		y := "http:\n  cors_origins:\n    - \"https://keep.test\"\n"
		if err := os.WriteFile(f, []byte(y), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("SQI_HTTP_CORS_ORIGINS", "")
		cfg, err := config.Load(f, config.FlagOverrides{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !slices.Equal(cfg.HTTP.CORSOrigins, []string{"https://keep.test"}) {
			t.Fatalf("http.cors_origins: got %v, want the file value retained", cfg.HTTP.CORSOrigins)
		}
	})

	t.Run("flag beats env and file", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "cfg.yaml")
		y := "http:\n  cors_origins:\n    - \"https://file.test\"\n"
		if err := os.WriteFile(f, []byte(y), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("SQI_HTTP_CORS_ORIGINS", "https://env.test")
		cfg, err := config.Load(f, config.FlagOverrides{
			HTTPCORSOrigins: []string{"https://flag.test"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !slices.Equal(cfg.HTTP.CORSOrigins, []string{"https://flag.test"}) {
			t.Fatalf("http.cors_origins: got %v, want the flag value", cfg.HTTP.CORSOrigins)
		}
	})
}

func TestValidate_CORSOrigins(t *testing.T) {
	tests := []struct {
		name    string
		origins []string
		wantErr bool
	}{
		{"empty is valid", nil, false},
		{"wildcard is valid", []string{"*"}, false},
		{"scheme and host", []string{"https://ui.test"}, false},
		{"scheme host and port", []string{"http://localhost:5173"}, false},
		{"trailing slash rejected", []string{"https://ui.test/"}, true},
		{"path rejected", []string{"https://ui.test/app"}, true},
		{"whitespace rejected", []string{"https://ui .test"}, true},
		{"missing scheme rejected", []string{"ui.test"}, true},
		{"wildcard subdomain rejected", []string{"https://*.example.com"}, true},
		{"suffix wildcard rejected", []string{"https://app.example.com*"}, true},
		{"normal explicit origin valid", []string{"https://app.example.com"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.HTTP.CORSOrigins = tt.origins
			var got []config.ValidationError
			for _, e := range config.Validate(cfg) {
				if e.Field == "http.cors_origins" {
					got = append(got, e)
				}
			}
			if tt.wantErr && len(got) == 0 {
				t.Fatalf("Validate(%v): got no http.cors_origins error, want one", tt.origins)
			}
			if !tt.wantErr && len(got) != 0 {
				t.Fatalf("Validate(%v): got %v, want no errors", tt.origins, got)
			}
		})
	}
}

func TestAuthLDAPEnvOverrides(t *testing.T) {
	t.Setenv("SQI_AUTH_ENABLED", "true")
	t.Setenv("SQI_AUTH_LDAP_ENABLED", "true")
	t.Setenv("SQI_AUTH_LDAP_URL", "ldaps://dc01.example.com:636")
	t.Setenv("SQI_AUTH_LDAP_BIND_DN", "CN=svc,DC=example,DC=com")
	t.Setenv("SQI_AUTH_LDAP_BIND_PASSWORD", "s3cret")
	t.Setenv("SQI_AUTH_LDAP_BASE_DN", "DC=example,DC=com")
	t.Setenv("SQI_AUTH_LDAP_TIMEOUT", "5s")
	t.Setenv("SQI_AUTH_LDAP_ROLE_SOURCE", "local")
	t.Setenv("SQI_AUTH_LDAP_DEFAULT_ROLE", "user")
	t.Setenv("SQI_AUTH_LDAP_UNIQUE_ID_ATTR", "objectGUID")

	c, err := config.Load("", config.FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.Auth.LDAP.Enabled {
		t.Error("Auth.LDAP.Enabled: got false, want true")
	}
	if c.Auth.LDAP.URL != "ldaps://dc01.example.com:636" {
		t.Errorf("Auth.LDAP.URL: got %q", c.Auth.LDAP.URL)
	}
	if c.Auth.LDAP.BindDN != "CN=svc,DC=example,DC=com" {
		t.Errorf("Auth.LDAP.BindDN: got %q", c.Auth.LDAP.BindDN)
	}
	if c.Auth.LDAP.BindPassword != "s3cret" {
		t.Errorf("Auth.LDAP.BindPassword: got %q", c.Auth.LDAP.BindPassword)
	}
	if c.Auth.LDAP.BaseDN != "DC=example,DC=com" {
		t.Errorf("Auth.LDAP.BaseDN: got %q", c.Auth.LDAP.BaseDN)
	}
	if c.Auth.LDAP.Timeout != 5*time.Second {
		t.Errorf("Auth.LDAP.Timeout: got %v, want 5s", c.Auth.LDAP.Timeout)
	}
	if c.Auth.LDAP.RoleSource != "local" {
		t.Errorf("Auth.LDAP.RoleSource: got %q, want local", c.Auth.LDAP.RoleSource)
	}
	if c.Auth.LDAP.DefaultRole != "user" {
		t.Errorf("Auth.LDAP.DefaultRole: got %q, want user", c.Auth.LDAP.DefaultRole)
	}
	if c.Auth.LDAP.UniqueIDAttr != "objectGUID" {
		t.Errorf("Auth.LDAP.UniqueIDAttr: got %q, want objectGUID", c.Auth.LDAP.UniqueIDAttr)
	}
}

func TestLoad_AuthLDAPFileOverride(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "sqi-server.yaml")
	yaml := `
auth:
  enabled: true
  ldap:
    enabled: true
    url: "ldap://dc.example.com:389"
    start_tls: true
    base_dn: "DC=example,DC=com"
    bind_dn: "CN=svc,DC=example,DC=com"
    bind_password: "filepass"
    user_filter: "(uid=%s)"
    role_map:
      - group: "CN=Admins,DC=example,DC=com"
        role: admin
      - group: "CN=Artists,DC=example,DC=com"
        role: user
    default_role: "read-only"
    unique_id_attr: "entryUUID"
`
	if err := os.WriteFile(f, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(f, config.FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Auth.LDAP.Enabled || !cfg.Auth.LDAP.StartTLS {
		t.Fatalf("ldap enabled/start_tls not applied: %+v", cfg.Auth.LDAP)
	}
	if cfg.Auth.LDAP.URL != "ldap://dc.example.com:389" {
		t.Errorf("url: got %q", cfg.Auth.LDAP.URL)
	}
	if cfg.Auth.LDAP.BaseDN != "DC=example,DC=com" {
		t.Errorf("base_dn: got %q", cfg.Auth.LDAP.BaseDN)
	}
	if cfg.Auth.LDAP.BindDN != "CN=svc,DC=example,DC=com" {
		t.Errorf("bind_dn: got %q", cfg.Auth.LDAP.BindDN)
	}
	if cfg.Auth.LDAP.BindPassword != "filepass" {
		t.Errorf("bind_password: got %q", cfg.Auth.LDAP.BindPassword)
	}
	if cfg.Auth.LDAP.UserFilter != "(uid=%s)" {
		t.Errorf("user_filter: got %q", cfg.Auth.LDAP.UserFilter)
	}
	if len(cfg.Auth.LDAP.RoleMap) != 2 {
		t.Fatalf("role_map: got %d entries, want 2", len(cfg.Auth.LDAP.RoleMap))
	}
	if cfg.Auth.LDAP.RoleMap[0].Role != "admin" || cfg.Auth.LDAP.RoleMap[1].Role != "user" {
		t.Errorf("role_map order not preserved: %+v", cfg.Auth.LDAP.RoleMap)
	}
	if cfg.Auth.LDAP.DefaultRole != "read-only" {
		t.Errorf("default_role: got %q, want read-only", cfg.Auth.LDAP.DefaultRole)
	}
	if cfg.Auth.LDAP.UniqueIDAttr != "entryUUID" {
		t.Errorf("unique_id_attr: got %q, want entryUUID", cfg.Auth.LDAP.UniqueIDAttr)
	}
	// Unset ldap fields keep defaults; sibling sub-blocks are untouched.
	if cfg.Auth.LDAP.RoleSource != "directory" {
		t.Errorf("role_source: expected default directory, got %q", cfg.Auth.LDAP.RoleSource)
	}
	if cfg.Auth.Session.CookieName != "sqi_session" {
		t.Errorf("auth.session.cookie_name: expected default, got %q", cfg.Auth.Session.CookieName)
	}
}

func TestValidate_AuthLDAP(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*config.Config)
		wantErr bool
	}{
		{"disabled ldap needs nothing", func(_ *config.Config) {}, false},
		{"valid search-bind", func(c *config.Config) {
			c.Auth.LDAP = validSearchLDAP()
		}, false},
		{"valid template-bind", func(c *config.Config) {
			l := validSearchLDAP()
			l.BindDN, l.BaseDN = "", ""
			l.UserDNTemplate = "uid=%s,ou=people,dc=example,dc=com"
			c.Auth.LDAP = l
		}, false},
		{"valid anonymous search (no bind_dn, no bind_password)", func(c *config.Config) {
			l := validSearchLDAP()
			l.BindDN, l.BindPassword = "", ""
			c.Auth.LDAP = l
		}, false},
		{"bind_password set without bind_dn is rejected", func(c *config.Config) {
			l := validSearchLDAP()
			l.BindDN = ""
			c.Auth.LDAP = l
		}, true},
		{"ldap enabled without auth enabled", func(c *config.Config) {
			c.Auth.Enabled = false
			c.Auth.LDAP = validSearchLDAP()
		}, true},
		{"missing url", func(c *config.Config) {
			l := validSearchLDAP()
			l.URL = ""
			c.Auth.LDAP = l
		}, true},
		{"bad url scheme", func(c *config.Config) {
			l := validSearchLDAP()
			l.URL = "https://dc.example.com"
			c.Auth.LDAP = l
		}, true},
		{"both bind modes configured", func(c *config.Config) {
			l := validSearchLDAP()
			l.UserDNTemplate = "uid=%s,dc=example,dc=com"
			c.Auth.LDAP = l
		}, true},
		{"neither bind mode configured", func(c *config.Config) {
			l := validSearchLDAP()
			l.BindDN, l.BaseDN, l.UserDNTemplate = "", "", ""
			c.Auth.LDAP = l
		}, true},
		{"start_tls with ldaps", func(c *config.Config) {
			l := validSearchLDAP()
			l.URL = "ldaps://dc.example.com:636"
			l.StartTLS = true
			c.Auth.LDAP = l
		}, true},
		{"nested_groups in template mode", func(c *config.Config) {
			l := validSearchLDAP()
			l.BindDN, l.BaseDN = "", ""
			l.UserDNTemplate = "uid=%s,dc=example,dc=com"
			l.NestedGroups = true
			c.Auth.LDAP = l
		}, true},
		{"unknown role in role_map", func(c *config.Config) {
			l := validSearchLDAP()
			l.RoleMap = []config.RoleMappingConfig{{Group: "CN=X", Role: "superadmin"}}
			c.Auth.LDAP = l
		}, true},
		{"unknown default_role", func(c *config.Config) {
			l := validSearchLDAP()
			l.DefaultRole = "nobody"
			c.Auth.LDAP = l
		}, true},
		{"empty default_role is valid (reject unmapped)", func(c *config.Config) {
			l := validSearchLDAP()
			l.DefaultRole = ""
			c.Auth.LDAP = l
		}, false},
		{"bad role_source", func(c *config.Config) {
			l := validSearchLDAP()
			l.RoleSource = "wherever"
			c.Auth.LDAP = l
		}, true},
		// An empty attribute name is sent to the directory as "", which some
		// servers reject outright. Both fields have defaults, so an empty one
		// means the operator cleared it deliberately — fail at boot rather
		// than on every login.
		{"empty username_attr", func(c *config.Config) {
			l := validSearchLDAP()
			l.UsernameAttr = ""
			c.Auth.LDAP = l
		}, true},
		{"empty display_name_attr", func(c *config.Config) {
			l := validSearchLDAP()
			l.DisplayNameAttr = ""
			c.Auth.LDAP = l
		}, true},
		// Template bind requests the same two attributes, so the requirement
		// is not search-mode-specific.
		{"empty username_attr in template mode", func(c *config.Config) {
			l := validSearchLDAP()
			l.BindDN, l.BaseDN = "", ""
			l.UserDNTemplate = "uid=%s,ou=people,dc=example,dc=com"
			l.UsernameAttr = ""
			c.Auth.LDAP = l
		}, true},
		{"empty display_name_attr in template mode", func(c *config.Config) {
			l := validSearchLDAP()
			l.BindDN, l.BaseDN = "", ""
			l.UserDNTemplate = "uid=%s,ou=people,dc=example,dc=com"
			l.DisplayNameAttr = ""
			c.Auth.LDAP = l
		}, true},
		// unique_id_attr is the account-matching key. Unlike the other two it
		// has no default to fall back on, so an unset value is the state an
		// operator upgrading from C1 arrives in — and starting anyway would
		// mean every login presents an empty identifier.
		{"empty unique_id_attr", func(c *config.Config) {
			l := validSearchLDAP()
			l.UniqueIDAttr = ""
			c.Auth.LDAP = l
		}, true},
		{"empty unique_id_attr in template mode", func(c *config.Config) {
			l := validSearchLDAP()
			l.BindDN, l.BaseDN = "", ""
			l.UserDNTemplate = "uid=%s,ou=people,dc=example,dc=com"
			l.UniqueIDAttr = ""
			c.Auth.LDAP = l
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.Auth.Enabled = true
			tt.mutate(&cfg)
			errs := config.Validate(cfg)
			if tt.wantErr && len(errs) == 0 {
				t.Fatal("expected a validation error, got none")
			}
			if !tt.wantErr && len(errs) > 0 {
				t.Fatalf("expected no validation errors, got %v", errs)
			}
		})
	}
}

// validSearchLDAP returns a minimal valid search-then-bind LDAP config.
func validSearchLDAP() config.LDAPConfig {
	return config.LDAPConfig{
		Enabled:      true,
		URL:          "ldap://dc.example.com:389",
		Timeout:      10 * time.Second,
		BindDN:       "CN=svc,DC=example,DC=com",
		BindPassword: "s3cret",
		BaseDN:       "DC=example,DC=com",
		UserFilter:   "(sAMAccountName=%s)",
		UsernameAttr: "sAMAccountName",
		// All three attribute names go onto the wire verbatim and are
		// required; see validateLDAPAttrs. unique_id_attr additionally has no
		// default, so it must be stated even in a "minimal" config.
		DisplayNameAttr: "displayName",
		UniqueIDAttr:    "entryUUID",
		RoleSource:      "directory",
		DefaultRole:     "read-only",
		RoleMap:         []config.RoleMappingConfig{{Group: "CN=Admins,DC=example,DC=com", Role: "admin"}},
	}
}

// The unset-unique_id_attr error must name its own field and both correct
// values. This is the one config error an operator upgrading from C1 is
// guaranteed to hit, and a generic "invalid ldap config" would leave them
// guessing between two attribute names, only one of which works on their
// server.
func TestValidate_AuthLDAPUniqueIDAttrErrorIsActionable(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Auth.Enabled = true
	cfg.Auth.LDAP = validSearchLDAP()
	cfg.Auth.LDAP.UniqueIDAttr = ""

	errs := config.Validate(cfg)
	var msg string
	for _, e := range errs {
		if e.Field == "auth.ldap.unique_id_attr" {
			msg = e.Message
			break
		}
	}
	if msg == "" {
		t.Fatalf("want an auth.ldap.unique_id_attr error, got %v", errs)
	}
	for _, want := range []string{"SQI_AUTH_LDAP_UNIQUE_ID_ATTR", "objectGUID", "entryUUID"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message does not mention %q, so it does not tell the operator "+
				"what to set: %q", want, msg)
		}
	}
}

// ── auth.oidc ──────────────────────────────────────────────────────────────

func TestOIDCConfigDefaults(t *testing.T) {
	c := config.DefaultConfig()
	if c.Auth.OIDC.Enabled {
		t.Fatal("oidc must default disabled")
	}
	if !slices.Equal(c.Auth.OIDC.Scopes, []string{"openid", "profile", "email"}) {
		t.Fatalf("oidc scopes default = %v, want [openid profile email]", c.Auth.OIDC.Scopes)
	}
	if c.Auth.OIDC.UsernameClaim != "preferred_username" {
		t.Fatalf("oidc username_claim default = %q, want preferred_username", c.Auth.OIDC.UsernameClaim)
	}
	if c.Auth.OIDC.DisplayNameClaim != "name" {
		t.Fatalf("oidc display_name_claim default = %q, want name", c.Auth.OIDC.DisplayNameClaim)
	}
	if c.Auth.OIDC.GroupsClaim != "groups" {
		t.Fatalf("oidc groups_claim default = %q, want groups", c.Auth.OIDC.GroupsClaim)
	}
	if c.Auth.OIDC.RoleSource != "directory" {
		t.Fatalf("oidc role_source default = %q, want directory", c.Auth.OIDC.RoleSource)
	}
	if c.Auth.OIDC.DefaultRole != "read-only" {
		t.Fatalf("oidc default_role default = %q, want read-only", c.Auth.OIDC.DefaultRole)
	}
	if c.Auth.OIDC.ReauthMode != "after_logout" {
		t.Fatalf("oidc reauth_mode default = %q, want after_logout", c.Auth.OIDC.ReauthMode)
	}
	if c.Auth.OIDC.LogoutMode != "local" {
		t.Fatalf("oidc logout_mode default = %q, want local", c.Auth.OIDC.LogoutMode)
	}
	if c.Auth.OIDC.ButtonLabel != "Sign in with SSO" {
		t.Fatalf("oidc button_label default = %q, want %q", c.Auth.OIDC.ButtonLabel, "Sign in with SSO")
	}
	if c.Auth.OIDC.Issuer != "" || c.Auth.OIDC.ClientID != "" || c.Auth.OIDC.ClientSecret != "" ||
		c.Auth.OIDC.RedirectURL != "" || c.Auth.OIDC.PostLogoutRedirectURL != "" {
		t.Fatalf("oidc unset string fields default = %+v, want all empty", c.Auth.OIDC)
	}
	if len(c.Auth.OIDC.RoleMap) != 0 {
		t.Fatalf("oidc role_map default = %+v, want empty", c.Auth.OIDC.RoleMap)
	}
}

func TestAuthOIDCEnvOverrides(t *testing.T) {
	t.Setenv("SQI_AUTH_ENABLED", "true")
	t.Setenv("SQI_AUTH_OIDC_ENABLED", "true")
	t.Setenv("SQI_AUTH_OIDC_ISSUER", "https://idp.example.com")
	t.Setenv("SQI_AUTH_OIDC_CLIENT_ID", "sqi")
	t.Setenv("SQI_AUTH_OIDC_CLIENT_SECRET", "s3cret")
	t.Setenv("SQI_AUTH_OIDC_REDIRECT_URL", "https://sqi.example.com/api/v1/auth/oidc/callback")
	t.Setenv("SQI_AUTH_OIDC_SCOPES", "openid, profile")
	t.Setenv("SQI_AUTH_OIDC_USERNAME_CLAIM", "upn")
	t.Setenv("SQI_AUTH_OIDC_DISPLAY_NAME_CLAIM", "displayName")
	t.Setenv("SQI_AUTH_OIDC_GROUPS_CLAIM", "roles")
	t.Setenv("SQI_AUTH_OIDC_ROLE_SOURCE", "local")
	t.Setenv("SQI_AUTH_OIDC_DEFAULT_ROLE", "user")
	t.Setenv("SQI_AUTH_OIDC_REAUTH_MODE", "always")
	t.Setenv("SQI_AUTH_OIDC_LOGOUT_MODE", "provider")
	t.Setenv("SQI_AUTH_OIDC_POST_LOGOUT_REDIRECT_URL", "https://sqi.example.com/logged-out")
	t.Setenv("SQI_AUTH_OIDC_BUTTON_LABEL", "Sign in with Acme SSO")

	c, err := config.Load("", config.FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.Auth.OIDC.Enabled {
		t.Error("Auth.OIDC.Enabled: got false, want true")
	}
	if c.Auth.OIDC.Issuer != "https://idp.example.com" {
		t.Errorf("Auth.OIDC.Issuer: got %q", c.Auth.OIDC.Issuer)
	}
	if c.Auth.OIDC.ClientID != "sqi" {
		t.Errorf("Auth.OIDC.ClientID: got %q", c.Auth.OIDC.ClientID)
	}
	if c.Auth.OIDC.ClientSecret != "s3cret" {
		t.Errorf("Auth.OIDC.ClientSecret: got %q", c.Auth.OIDC.ClientSecret)
	}
	if c.Auth.OIDC.RedirectURL != "https://sqi.example.com/api/v1/auth/oidc/callback" {
		t.Errorf("Auth.OIDC.RedirectURL: got %q", c.Auth.OIDC.RedirectURL)
	}
	if !slices.Equal(c.Auth.OIDC.Scopes, []string{"openid", "profile"}) {
		t.Errorf("Auth.OIDC.Scopes: got %v, want [openid profile]", c.Auth.OIDC.Scopes)
	}
	if c.Auth.OIDC.UsernameClaim != "upn" {
		t.Errorf("Auth.OIDC.UsernameClaim: got %q, want upn", c.Auth.OIDC.UsernameClaim)
	}
	if c.Auth.OIDC.DisplayNameClaim != "displayName" {
		t.Errorf("Auth.OIDC.DisplayNameClaim: got %q, want displayName", c.Auth.OIDC.DisplayNameClaim)
	}
	if c.Auth.OIDC.GroupsClaim != "roles" {
		t.Errorf("Auth.OIDC.GroupsClaim: got %q, want roles", c.Auth.OIDC.GroupsClaim)
	}
	if c.Auth.OIDC.RoleSource != "local" {
		t.Errorf("Auth.OIDC.RoleSource: got %q, want local", c.Auth.OIDC.RoleSource)
	}
	if c.Auth.OIDC.DefaultRole != "user" {
		t.Errorf("Auth.OIDC.DefaultRole: got %q, want user", c.Auth.OIDC.DefaultRole)
	}
	if c.Auth.OIDC.ReauthMode != "always" {
		t.Errorf("Auth.OIDC.ReauthMode: got %q, want always", c.Auth.OIDC.ReauthMode)
	}
	if c.Auth.OIDC.LogoutMode != "provider" {
		t.Errorf("Auth.OIDC.LogoutMode: got %q, want provider", c.Auth.OIDC.LogoutMode)
	}
	if c.Auth.OIDC.PostLogoutRedirectURL != "https://sqi.example.com/logged-out" {
		t.Errorf("Auth.OIDC.PostLogoutRedirectURL: got %q", c.Auth.OIDC.PostLogoutRedirectURL)
	}
	if c.Auth.OIDC.ButtonLabel != "Sign in with Acme SSO" {
		t.Errorf("Auth.OIDC.ButtonLabel: got %q", c.Auth.OIDC.ButtonLabel)
	}
}

func TestAuthOIDCEnvOverrides_RoleMapHasNoEnvForm(t *testing.T) {
	// role_map is file-only, exactly as auth.ldap.role_map is: a list of
	// group->role pairs has no sane comma-separated env encoding. There is no
	// SQI_AUTH_OIDC_ROLE_MAP to set, so this test documents the absence by
	// checking the default survives Load with every other oidc.* var set.
	t.Setenv("SQI_AUTH_OIDC_ENABLED", "true")
	c, err := config.Load("", config.FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Auth.OIDC.RoleMap) != 0 {
		t.Fatalf("Auth.OIDC.RoleMap: got %+v, want empty (no env form exists)", c.Auth.OIDC.RoleMap)
	}
}

func TestLoad_AuthOIDCFileOverride(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "sqi-server.yaml")
	yamlSrc := `
auth:
  enabled: true
  oidc:
    enabled: true
    issuer: "https://idp.example.com"
    client_id: "sqi"
    client_secret: "filesecret"
    redirect_url: "https://sqi.example.com/api/v1/auth/oidc/callback"
    scopes: ["openid", "profile", "email", "groups"]
    username_claim: "preferred_username"
    display_name_claim: "name"
    groups_claim: "groups"
    role_source: "local"
    role_map:
      - group: "sqi-admins"
        role: admin
      - group: "sqi-artists"
        role: user
    default_role: "read-only"
    reauth_mode: "always"
    logout_mode: "provider"
    post_logout_redirect_url: "https://sqi.example.com/logged-out"
    button_label: "Sign in with Acme SSO"
`
	if err := os.WriteFile(f, []byte(yamlSrc), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(f, config.FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Auth.OIDC.Enabled {
		t.Fatalf("oidc enabled not applied: %+v", cfg.Auth.OIDC)
	}
	if cfg.Auth.OIDC.Issuer != "https://idp.example.com" {
		t.Errorf("issuer: got %q", cfg.Auth.OIDC.Issuer)
	}
	if cfg.Auth.OIDC.ClientID != "sqi" {
		t.Errorf("client_id: got %q", cfg.Auth.OIDC.ClientID)
	}
	if cfg.Auth.OIDC.ClientSecret != "filesecret" {
		t.Errorf("client_secret: got %q", cfg.Auth.OIDC.ClientSecret)
	}
	if cfg.Auth.OIDC.RedirectURL != "https://sqi.example.com/api/v1/auth/oidc/callback" {
		t.Errorf("redirect_url: got %q", cfg.Auth.OIDC.RedirectURL)
	}
	if !slices.Equal(cfg.Auth.OIDC.Scopes, []string{"openid", "profile", "email", "groups"}) {
		t.Errorf("scopes: got %v", cfg.Auth.OIDC.Scopes)
	}
	if len(cfg.Auth.OIDC.RoleMap) != 2 {
		t.Fatalf("role_map: got %d entries, want 2", len(cfg.Auth.OIDC.RoleMap))
	}
	if cfg.Auth.OIDC.RoleMap[0].Role != "admin" || cfg.Auth.OIDC.RoleMap[1].Role != "user" {
		t.Errorf("role_map order not preserved: %+v", cfg.Auth.OIDC.RoleMap)
	}
	if cfg.Auth.OIDC.DefaultRole != "read-only" {
		t.Errorf("default_role: got %q, want read-only", cfg.Auth.OIDC.DefaultRole)
	}
	if cfg.Auth.OIDC.ReauthMode != "always" {
		t.Errorf("reauth_mode: got %q, want always", cfg.Auth.OIDC.ReauthMode)
	}
	if cfg.Auth.OIDC.LogoutMode != "provider" {
		t.Errorf("logout_mode: got %q, want provider", cfg.Auth.OIDC.LogoutMode)
	}
	if cfg.Auth.OIDC.PostLogoutRedirectURL != "https://sqi.example.com/logged-out" {
		t.Errorf("post_logout_redirect_url: got %q", cfg.Auth.OIDC.PostLogoutRedirectURL)
	}
	if cfg.Auth.OIDC.ButtonLabel != "Sign in with Acme SSO" {
		t.Errorf("button_label: got %q", cfg.Auth.OIDC.ButtonLabel)
	}
	// Unset oidc fields keep defaults; sibling sub-blocks are untouched.
	if cfg.Auth.OIDC.UsernameClaim != "preferred_username" {
		t.Errorf("username_claim: got %q", cfg.Auth.OIDC.UsernameClaim)
	}
	if cfg.Auth.LDAP.RoleSource != "directory" {
		t.Errorf("auth.ldap.role_source: expected default directory, got %q", cfg.Auth.LDAP.RoleSource)
	}
}

// TestLoad_AuthOIDCRoleMapExplicitEmptyClearsDefault guards the
// pointer-nil-vs-empty discrimination mergeAuthOIDCFile shares with
// mergeAuthLDAPFile: a "role_map: []" key present in the file must produce a
// non-nil empty slice (a deliberate clear), not be indistinguishable from the
// key being absent (which leaves the untouched default, also empty here, but
// via a different code path — nil vs an explicitly-set empty slice).
func TestLoad_AuthOIDCRoleMapExplicitEmptyClearsDefault(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "sqi-server.yaml")
	yamlSrc := `
auth:
  enabled: true
  oidc:
    enabled: true
    role_map: []
`
	if err := os.WriteFile(f, []byte(yamlSrc), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(f, config.FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.OIDC.RoleMap == nil {
		t.Fatal("an explicit role_map: [] in the file must produce a non-nil empty slice, " +
			"not leave the (also-empty) default untouched")
	}
}

func TestMarshalYAML_OIDCClientSecretRedacted(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Auth.OIDC.ClientID = "sqi"
	cfg.Auth.OIDC.ClientSecret = "hunter2"

	out, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}

	if strings.Contains(string(out), "hunter2") {
		t.Fatalf("marshaled config contains the plaintext OIDC client secret:\n%s", out)
	}
	if !strings.Contains(string(out), "<redacted>") {
		t.Fatalf("marshaled config does not contain the <redacted> placeholder:\n%s", out)
	}
}

func TestMarshalYAML_OIDCClientSecretSentinelNeverAppears(t *testing.T) {
	const sentinel = "S3CRET-SENTINEL"

	cfg := config.DefaultConfig()
	cfg.Auth.OIDC.ClientID = "sqi"
	cfg.Auth.OIDC.ClientSecret = sentinel

	out, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}

	if strings.Contains(string(out), sentinel) {
		t.Fatalf("marshaled config leaks the OIDC client secret sentinel:\n%s", out)
	}
}

func TestMarshalYAML_OIDCClientSecretEmptyNotRedacted(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Auth.OIDC.ClientID = ""
	cfg.Auth.OIDC.ClientSecret = ""

	out, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}

	if strings.Contains(string(out), "<redacted>") {
		t.Fatalf("marshaled config redacts an empty OIDC client secret:\n%s", out)
	}
}

func TestMarshalYAML_OIDCConfigFieldsSurviveRedaction(t *testing.T) {
	// Guards against the alias-type indirection in OIDCConfig.MarshalYAML
	// silently dropping a field: every non-secret field is set to a distinct
	// recognizable value and must still appear in the marshaled output.
	cfg := config.DefaultConfig()
	cfg.Auth.OIDC = config.OIDCConfig{
		Enabled:          true,
		Issuer:           "https://idp.example.com",
		ClientID:         "sqi-client",
		ClientSecret:     "hunter2",
		RedirectURL:      "https://sqi.example.com/api/v1/auth/oidc/callback",
		Scopes:           []string{"openid", "profile", "email"},
		UsernameClaim:    "preferred_username",
		DisplayNameClaim: "name",
		GroupsClaim:      "groups",
		RoleSource:       "local",
		RoleMap: []config.RoleMappingConfig{
			{Group: "sqi-admins", Role: "admin"},
			{Group: "sqi-operators", Role: "operator"},
		},
		DefaultRole:           "read-only",
		ReauthMode:            "always",
		LogoutMode:            "provider",
		PostLogoutRedirectURL: "https://sqi.example.com/logged-out",
		ButtonLabel:           "Sign in with Acme SSO",
	}

	out, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	got := string(out)

	wantSubstrings := []string{
		"https://idp.example.com",
		"sqi-client",
		"https://sqi.example.com/api/v1/auth/oidc/callback",
		"preferred_username",
		"name",
		"groups",
		"local",
		"sqi-admins",
		"admin",
		"sqi-operators",
		"operator",
		"read-only",
		"always",
		"provider",
		"https://sqi.example.com/logged-out",
		"Sign in with Acme SSO",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(got, want) {
			t.Errorf("marshaled config missing expected field value %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "<redacted>") {
		t.Fatalf("marshaled config does not redact OIDC client secret:\n%s", got)
	}
}

func TestLoad_AuthOIDCClientSecretSurvivesRedactedMarshal(t *testing.T) {
	// Redaction on marshal must not affect the loaded (unmarshaled) value —
	// round-tripping a redacted dump is not a goal, but loading the real
	// config must still populate the real secret.
	t.Setenv("SQI_AUTH_OIDC_CLIENT_SECRET", "s3cret")

	cfg, err := config.Load("", config.FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.OIDC.ClientSecret != "s3cret" {
		t.Fatalf("Auth.OIDC.ClientSecret: got %q, want s3cret", cfg.Auth.OIDC.ClientSecret)
	}

	out, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	if !strings.Contains(string(out), "<redacted>") {
		t.Fatalf("marshaled config does not redact OIDC client secret:\n%s", out)
	}
	if cfg.Auth.OIDC.ClientSecret != "s3cret" {
		t.Fatalf("marshaling mutated the in-memory config: ClientSecret = %q, want s3cret", cfg.Auth.OIDC.ClientSecret)
	}
}

func TestValidate_AuthDisabled_OIDCNoErrorsEvenWithBadValues(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Auth.Enabled = false
	cfg.Auth.OIDC = config.OIDCConfig{
		Enabled:    true,
		RoleSource: "nonsense",
		ReauthMode: "nonsense",
		LogoutMode: "nonsense",
	}
	// auth disabled short-circuits everything except the cross-gate error, so
	// exactly one error (auth.oidc.enabled) is expected here, not the pile of
	// downstream OIDC errors this config would otherwise produce.
	errs := config.Validate(cfg)
	if len(errs) != 1 || errs[0].Field != "auth.oidc.enabled" {
		t.Fatalf("want exactly one auth.oidc.enabled error, got %v", errs)
	}
}

func TestValidateAuthOIDC(t *testing.T) {
	base := func() config.Config {
		c := config.DefaultConfig()
		c.Auth.Enabled = true
		c.Auth.OIDC = config.OIDCConfig{
			Enabled: true, Issuer: "https://idp.example.com",
			ClientID: "sqi", ClientSecret: "s3cret",
			RedirectURL:   "https://sqi.example.com/api/v1/auth/oidc/callback",
			Scopes:        []string{"openid", "profile", "email"},
			UsernameClaim: "preferred_username", DisplayNameClaim: "name", GroupsClaim: "groups",
			RoleSource: "directory", DefaultRole: "read-only",
			ReauthMode: "after_logout", LogoutMode: "local",
		}
		return c
	}

	tests := []struct {
		name      string
		mutate    func(*config.Config)
		wantField string
	}{
		{name: "valid", mutate: func(*config.Config) {}},
		{
			name:      "oidc without the auth gate",
			mutate:    func(c *config.Config) { c.Auth.Enabled = false },
			wantField: "auth.oidc.enabled",
		},
		{name: "missing issuer", mutate: func(c *config.Config) { c.Auth.OIDC.Issuer = "" }, wantField: "auth.oidc.issuer"},
		{name: "missing client id", mutate: func(c *config.Config) { c.Auth.OIDC.ClientID = "" }, wantField: "auth.oidc.client_id"},
		{
			name:      "missing client secret",
			mutate:    func(c *config.Config) { c.Auth.OIDC.ClientSecret = "" },
			wantField: "auth.oidc.client_secret",
		},
		{
			name:      "missing redirect url",
			mutate:    func(c *config.Config) { c.Auth.OIDC.RedirectURL = "" },
			wantField: "auth.oidc.redirect_url",
		},
		{
			name:      "bad role source",
			mutate:    func(c *config.Config) { c.Auth.OIDC.RoleSource = "nope" },
			wantField: "auth.oidc.role_source",
		},
		{
			name:      "bad reauth mode",
			mutate:    func(c *config.Config) { c.Auth.OIDC.ReauthMode = "sometimes" },
			wantField: "auth.oidc.reauth_mode",
		},
		{
			name:      "bad logout mode",
			mutate:    func(c *config.Config) { c.Auth.OIDC.LogoutMode = "everywhere" },
			wantField: "auth.oidc.logout_mode",
		},
		{
			name:      "typo'd role in map aborts boot",
			mutate:    func(c *config.Config) { c.Auth.OIDC.RoleMap = []config.RoleMappingConfig{{Group: "g", Role: "admn"}} },
			wantField: "auth.oidc.role_map[0].role",
		},
		{
			name:      "unknown default role",
			mutate:    func(c *config.Config) { c.Auth.OIDC.DefaultRole = "superuser" },
			wantField: "auth.oidc.default_role",
		},
		{name: "missing scopes", mutate: func(c *config.Config) { c.Auth.OIDC.Scopes = nil }, wantField: "auth.oidc.scopes"},
		{
			name:      "schemeless issuer is not absolute",
			mutate:    func(c *config.Config) { c.Auth.OIDC.Issuer = "idp.example.com" },
			wantField: "auth.oidc.issuer",
		},
		{
			name:      "relative redirect url is not absolute",
			mutate:    func(c *config.Config) { c.Auth.OIDC.RedirectURL = "/api/v1/auth/oidc/callback" },
			wantField: "auth.oidc.redirect_url",
		},
		{
			name: "provider logout mode requires post logout redirect url",
			mutate: func(c *config.Config) {
				c.Auth.OIDC.LogoutMode = "provider"
			},
			wantField: "auth.oidc.post_logout_redirect_url",
		},
	}

	hasFieldError := func(errs []config.ValidationError, field string) bool {
		for _, e := range errs {
			if e.Field == field {
				return true
			}
		}
		return false
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base()
			tt.mutate(&cfg)
			errs := config.Validate(cfg)
			if tt.wantField == "" {
				if len(errs) != 0 {
					t.Fatalf("want no errors, got %v", errs)
				}
				return
			}
			if !hasFieldError(errs, tt.wantField) {
				t.Fatalf("want error on %s, got %v", tt.wantField, errs)
			}
		})
	}
}

func TestOIDCConfig_MarshalYAMLRedactsSecret(t *testing.T) {
	c := config.OIDCConfig{ClientSecret: "s3cret"}
	out, err := yaml.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "s3cret") {
		t.Fatalf("client_secret leaked into YAML output:\n%s", out)
	}
}
