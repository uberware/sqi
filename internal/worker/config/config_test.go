// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/uberware/sqi/internal/worker/capabilities"
)

// ── Default() ────────────────────────────────────────────────────────────────

func TestDefault_SanityValues(t *testing.T) {
	cfg := Default()

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
	// worker.session_dir must default to empty: the effective session root is
	// resolved at worker startup (cmd/sqi-worker's effectiveSessionRoot), not
	// baked into this package's Default() — an empty value here is what lets
	// that resolution depend on the FINAL data_dir/root-ness once every config
	// layer (file, env, flags) has been applied.
	if cfg.Worker.SessionDir != "" {
		t.Errorf("worker.session_dir default should be empty, got %q", cfg.Worker.SessionDir)
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
`
	f := writeTempYAML(t, yaml)

	t.Setenv("SQI_WORKER_NATS_URL", "nats://env-host:4222")
	t.Setenv("SQI_WORKER_NAME", "from-env")

	cfg, err := Load(f, FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	assertStr(t, "nats.url", cfg.NATS.URL, "nats://env-host:4222")
	assertStr(t, "worker.name", cfg.Worker.Name, "from-env")
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
	t.Setenv("SQI_WORKER_SESSION_DIR", "/custom/sessions")

	cfg, err := Load("", FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	assertStr(t, "worker.session_dir", cfg.Worker.SessionDir, "/custom/sessions")
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
	cfg.Worker.DataDir = ""
	cfg.Log.Level = "bad"
	cfg.Log.Format = "bad"

	errs := Validate(cfg)
	if len(errs) < 3 {
		t.Errorf("expected at least 3 errors for broken config, got %d: %v", len(errs), errs)
	}
}

// ── Validate — isolation.required / worker.allow_root contradiction ──────────

// TestValidate_IsolationRequiredWithoutAllowRootIsRejected is the guard for
// the Minor fix: isolation.required demands the worker be able to assume
// another OS identity, but on POSIX the only mechanism today (setuid/setgid)
// requires the worker itself to run as root, and worker.allow_root=false
// makes the worker refuse to even start as root. Configuring both together is
// a contradiction that would otherwise surface only as a confusing
// root-user-refusal error that never mentions isolation at all.
func TestValidate_IsolationRequiredWithoutAllowRootIsRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the contradiction is POSIX-specific: Windows isolation does not require the worker itself to run privileged")
	}
	cfg := Default()
	cfg.NATS.URL = "nats://localhost:4222"
	cfg.Isolation.Required = true
	cfg.Worker.AllowRoot = false

	errs := Validate(cfg)
	if !containsField(errs, "isolation.required") {
		t.Errorf("expected isolation.required error for the required+!allow_root contradiction, got %v", errs)
	}
}

func TestValidate_IsolationRequiredWithAllowRootIsFine(t *testing.T) {
	cfg := Default()
	cfg.NATS.URL = "nats://localhost:4222"
	cfg.Isolation.Required = true
	cfg.Worker.AllowRoot = true

	errs := Validate(cfg)
	if containsField(errs, "isolation.required") {
		t.Errorf("expected no isolation.required error when allow_root is true, got %v", errs)
	}
}

func TestValidate_IsolationNotRequiredWithoutAllowRootIsFine(t *testing.T) {
	cfg := Default()
	cfg.NATS.URL = "nats://localhost:4222"
	cfg.Isolation.Required = false
	cfg.Worker.AllowRoot = false

	errs := Validate(cfg)
	if containsField(errs, "isolation.required") {
		t.Errorf("expected no isolation.required error when isolation is not required, got %v", errs)
	}
}

// ── Validate — env_passthrough glob syntax ────────────────────────────────────

// TestValidate_MalformedEnvPassthroughGlobIsRejected is the guard for the
// Minor fix: envutil.allowedName's `filepath.Match(...); err == nil && ok`
// silently treats a malformed glob (e.g. an unterminated character class) as
// "does not match" forever, rather than surfacing the operator's typo. That
// swallowing happens deep in the hot path (every inherited env var, every
// isolated task); the config-load-time check here is the only place an
// operator would ever see the mistake.
func TestValidate_MalformedEnvPassthroughGlobIsRejected(t *testing.T) {
	cfg := Default()
	cfg.NATS.URL = "nats://localhost:4222"
	cfg.Isolation.EnvPassthrough = []string{"FLEXLM_*", "["}

	errs := Validate(cfg)
	if !containsField(errs, "isolation.env_passthrough[1]") {
		t.Errorf("expected isolation.env_passthrough[1] error for the malformed glob, got %v", errs)
	}
}

func TestValidate_ValidEnvPassthroughGlobsAreFine(t *testing.T) {
	cfg := Default()
	cfg.NATS.URL = "nats://localhost:4222"
	cfg.Isolation.EnvPassthrough = []string{"FLEXLM_*", "ADSKFLEX_*", "?_LICENSE"}

	errs := Validate(cfg)
	for _, e := range errs {
		if e.Field == "isolation.env_passthrough[0]" || e.Field == "isolation.env_passthrough[1]" || e.Field == "isolation.env_passthrough[2]" {
			t.Errorf("expected no env_passthrough error for valid globs, got %v", errs)
		}
	}
}

// ── worker.queue_ids validation ──────────────────────────────────────────────

func TestValidate_QueueIDRejectsEachInvalidShape(t *testing.T) {
	tests := []struct {
		name string
		id   string
	}{
		{"empty", ""},
		{"dot", "queue.one"},
		{"star", "queue*"},
		{"gt", "queue>"},
		{"whitespace", "queue one"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			cfg.NATS.URL = "nats://localhost:4222"
			cfg.Worker.QueueIDs = []string{tt.id}

			errs := Validate(cfg)
			if !containsField(errs, "worker.queue_ids[0]") {
				t.Fatalf("expected worker.queue_ids[0] error for %q, got %v", tt.id, errs)
			}
			for _, e := range errs {
				if e.Field == "worker.queue_ids[0]" && !strings.Contains(e.Message, tt.id) {
					t.Errorf("error message %q does not name the offending value %q", e.Message, tt.id)
				}
			}
		})
	}
}

func TestValidate_QueueIDAcceptsUUID(t *testing.T) {
	cfg := Default()
	cfg.NATS.URL = "nats://localhost:4222"
	cfg.Worker.QueueIDs = []string{"3f2a9c9e-6b1a-4e2f-9c3d-8f1a2b3c4d5e"}

	errs := Validate(cfg)
	if containsField(errs, "worker.queue_ids[0]") {
		t.Errorf("expected no queue_ids error for a valid UUID, got %v", errs)
	}
}

func TestLoad_QueueIDsYAMLAndEnvAgreeOnEmptyEntries(t *testing.T) {
	// A blank entry from a YAML list and a blank entry from a comma-separated
	// env var must produce the same shape: preserved (not silently dropped)
	// so Validate rejects it, naming its position, on both paths.
	t.Run("yaml", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "sqi-worker.yaml")
		yamlContent := "worker:\n  queue_ids: [\"\", \"q1\"]\n"
		if err := os.WriteFile(path, []byte(yamlContent), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		t.Setenv("SQI_WORKER_NATS_URL", "nats://x:4222")

		cfg, err := Load(path, FlagOverrides{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(cfg.Worker.QueueIDs) != 2 || cfg.Worker.QueueIDs[0] != "" || cfg.Worker.QueueIDs[1] != "q1" {
			t.Fatalf("queue_ids = %v, want [\"\", \"q1\"] (unfiltered, so Validate can reject the empty entry)", cfg.Worker.QueueIDs)
		}
		if !containsField(Validate(cfg), "worker.queue_ids[0]") {
			t.Errorf("expected worker.queue_ids[0] validation error for the blank YAML entry")
		}
	})

	t.Run("env", func(t *testing.T) {
		t.Setenv("SQI_WORKER_NATS_URL", "nats://x:4222")
		t.Setenv("SQI_WORKER_QUEUE_IDS", ",q1")

		cfg, err := Load("", FlagOverrides{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(cfg.Worker.QueueIDs) != 2 || cfg.Worker.QueueIDs[0] != "" || cfg.Worker.QueueIDs[1] != "q1" {
			t.Fatalf("queue_ids = %v, want [\"\", \"q1\"] (unfiltered, so Validate can reject the empty entry)", cfg.Worker.QueueIDs)
		}
		if !containsField(Validate(cfg), "worker.queue_ids[0]") {
			t.Errorf("expected worker.queue_ids[0] validation error for the blank env entry")
		}
	})
}

// ── Diagnostics: defaults and env overrides ──────────────────────────────────

func TestDefault_DiagnosticsEnabledByDefault(t *testing.T) {
	cfg := Default()
	if !cfg.Diagnostics.Enabled {
		t.Error("diagnostics.enabled default should be true")
	}
}

func TestLoad_DiagnosticsEnvOverridesToFalse(t *testing.T) {
	t.Setenv("SQI_WORKER_NATS_URL", "nats://x:4222") // satisfy validation
	t.Setenv("SQI_DIAGNOSTICS_ENABLED", "false")

	cfg, err := Load("", FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Diagnostics.Enabled {
		t.Error("diagnostics.enabled: expected false from env")
	}
}

// ── Staging: defaults and YAML loading ───────────────────────────────────────

func TestDefault_StagingEmpty(t *testing.T) {
	cfg := Default()
	if cfg.Staging.SyncCommand != "" || cfg.Staging.ScratchDir != "" {
		t.Errorf("Staging defaults non-empty: %+v", cfg.Staging)
	}
}

func TestLoad_StagingFromYAML(t *testing.T) {
	body := "staging:\n  scratch_dir: /scratch\n  sync_command: rsync -a {src} {dest}\n"
	f := writeTempFile(t, "worker.yaml", []byte(body))
	cfg, err := Load(f, FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Staging.ScratchDir != "/scratch" || cfg.Staging.SyncCommand != "rsync -a {src} {dest}" {
		t.Errorf("Staging = %+v", cfg.Staging)
	}
}

func TestDefault_StagingDefaultsTrueByDefault(t *testing.T) {
	cfg := Default()
	if !cfg.Staging.Defaults {
		t.Error("staging.defaults default should be true")
	}
}

func TestLoad_StagingDefaultsEnvOverridesToFalse(t *testing.T) {
	t.Setenv("SQI_WORKER_NATS_URL", "nats://x:4222") // satisfy validation
	t.Setenv("SQI_STAGING_DEFAULTS", "false")

	cfg, err := Load("", FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Staging.Defaults {
		t.Error("staging.defaults: expected false from env")
	}
}

func TestLoad_StagingDefaultsFromYAML(t *testing.T) {
	body := "staging:\n  defaults: false\n"
	f := writeTempFile(t, "worker.yaml", []byte(body))
	cfg, err := Load(f, FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Staging.Defaults {
		t.Error("staging.defaults: expected false from yaml")
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

func assertDuration(t *testing.T, label string, got, want time.Duration) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %v, want %v", label, got, want)
	}
}

// ── capabilities ─────────────────────────────────────────────────────────────

func TestLoad_CapabilitiesDetectorsAndDisableEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sqi-worker.yaml")
	const y = `
capabilities:
  detect:
    - tag: inhouse
      checks:
        - exe: inhouse-tool
  disable: [blender]
`
	if err := os.WriteFile(path, []byte(y), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SQI_WORKER_CAPABILITIES_DISABLE", "nuke")

	cfg, err := Load(path, FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Capabilities.Detect) != 1 || cfg.Capabilities.Detect[0].Tag != "inhouse" {
		t.Errorf("detectors not parsed: %+v", cfg.Capabilities.Detect)
	}
	// Env-provided disable appends to file-provided disable.
	if got := strings.Join(cfg.Capabilities.Disable, ","); got != "blender,nuke" {
		t.Errorf("disable merge: got %q, want blender,nuke", got)
	}
}

func TestValidate_RejectsBadDetector(t *testing.T) {
	cfg := Default()
	cfg.Capabilities.Detect = []capabilities.Detector{{Tag: "x"}} // no checks
	if errs := Validate(cfg); len(errs) == 0 {
		t.Errorf("expected validation error for detector with no checks")
	}
}

func TestLoad_NATSCredentialFileDefaultsUnderDataDir(t *testing.T) {
	t.Setenv("SQI_WORKER_NATS_URL", "nats://x:4222") // satisfy validation
	t.Setenv("SQI_WORKER_DATA_DIR", "/tmp/sqi-worker-data")

	cfg, err := Load("", FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := filepath.Join("/tmp/sqi-worker-data", "worker.nk")
	if cfg.NATS.CredentialFile != want {
		t.Errorf("NATS.CredentialFile = %q, want %q", cfg.NATS.CredentialFile, want)
	}
}

func TestLoad_NATSCredentialFileExplicitValuePreserved(t *testing.T) {
	body := "nats:\n  credential_file: /etc/sqi/worker.nk\n"
	f := writeTempFile(t, "worker.yaml", []byte(body))
	cfg, err := Load(f, FlagOverrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.NATS.CredentialFile != "/etc/sqi/worker.nk" {
		t.Errorf("NATS.CredentialFile = %q, want /etc/sqi/worker.nk", cfg.NATS.CredentialFile)
	}
}
