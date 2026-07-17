// SPDX-License-Identifier: AGPL-3.0-or-later

package config_test

import (
	"os"
	"path/filepath"
	"strings"
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

	errs := config.Validate(cfg)
	for _, e := range errs {
		if strings.HasPrefix(e.Field, "auth.") {
			t.Fatalf("auth disabled must not produce validation errors, got %v", e)
		}
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
