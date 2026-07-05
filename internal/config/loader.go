// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// FlagOverrides carries values from CLI flags that take highest precedence
// during config loading. Only non-empty / non-zero values override the lower
// layers, so callers can safely pass flag values that were not set without
// clobbering config-file or environment values.
type FlagOverrides struct {
	// LogLevel overrides Log.Level when non-empty. Bound to --log-level.
	LogLevel string
	// LogFormat overrides Log.Format when non-empty. Bound to --log-format.
	LogFormat string
	// HTTPAddr overrides HTTP.Addr when non-empty. Bound to --http-addr.
	HTTPAddr string
	// EnforceLimits overrides OpenJD.EnforceLimits when non-nil. Bound to
	// --openjd-enforce-limits. A pointer (not a bool) so an unset flag leaves the
	// lower layers intact while an explicit --openjd-enforce-limits=false can turn
	// the (default-true) gate off.
	EnforceLimits *bool
}

// defaultSearchPaths returns the ordered list of config file locations
// consulted when no explicit path is given.
func defaultSearchPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}

	paths := []string{
		filepath.Join("config", "sqi-server.yaml"),
		filepath.Join("config", "sqi-server.json"),
	}
	if home != "" {
		paths = append(
			paths,
			filepath.Join(home, ".sqi", "sqi-server.yaml"),
			filepath.Join(home, ".sqi", "sqi-server.json"),
		)
	}
	// Use string literals for absolute /etc paths — filepath.Join with an
	// absolute first segment is flagged by gocritic (filepathJoin).
	paths = append(
		paths,
		"/etc/sqi/sqi-server.yaml",
		"/etc/sqi/sqi-server.json",
	)
	return paths
}

// Load builds a [Config] by applying four layers in increasing precedence:
//
//  1. Built-in defaults ([DefaultConfig])
//  2. YAML/JSON file at filePath, or the first file found in the default
//     search path when filePath is empty
//  3. SQI_* environment variables
//  4. CLI flag overrides via flags
//
// A missing config file is not an error unless filePath was set explicitly.
func Load(filePath string, flags FlagOverrides) (Config, error) {
	cfg := DefaultConfig()

	// ── Layer 2: config file ──────────────────────────────────────────────
	if err := applyFile(&cfg, filePath); err != nil {
		return Config{}, err
	}

	// ── Layer 3: environment variables ───────────────────────────────────
	applyEnv(&cfg)

	// ── Layer 4: CLI flag overrides ───────────────────────────────────────
	applyFlags(&cfg, flags)

	return cfg, nil
}

// ── File layer ────────────────────────────────────────────────────────────────

// fileConfig is the partial YAML/JSON shape used when unmarshaling a config
// file. Every field uses a pointer so we can distinguish "not set" from a
// zero value, applying only the fields that are present in the file.
type fileConfig struct {
	HTTP *struct {
		Addr        *string `yaml:"addr"`
		EnablePprof *bool   `yaml:"enable_pprof"`
	} `yaml:"http"`

	NATS *struct {
		Addr       *string `yaml:"addr"`
		DataDir    *string `yaml:"data_dir"`
		MaxStoreMB *int    `yaml:"max_store_mb"`
	} `yaml:"nats"`

	Store *struct {
		SQLitePath         *string `yaml:"sqlite_path"`
		CheckpointInterval *string `yaml:"checkpoint_interval"`
	} `yaml:"store"`

	Log *struct {
		Level  *string `yaml:"level"`
		Format *string `yaml:"format"`
	} `yaml:"log"`

	Scheduler *struct {
		HeartbeatTimeout          *string `yaml:"heartbeat_timeout"`
		TickInterval              *string `yaml:"tick_interval"`
		MaxTasksPerWorker         *int    `yaml:"max_tasks_per_worker"`
		OfflineWorkerRetention    *string `yaml:"offline_worker_retention"`
		JobRetention              *string `yaml:"job_retention"`
		JobRetentionIncludeFailed *bool   `yaml:"job_retention_include_failed"`
	} `yaml:"scheduler"`

	Discovery *struct {
		Enabled      *bool   `yaml:"enabled"`
		InstanceName *string `yaml:"instance_name"`
	} `yaml:"discovery"`

	OpenJD *struct {
		EnforceLimits *bool `yaml:"enforce_limits"`
	} `yaml:"openjd"`

	Diagnostics *struct {
		BufferSize *int `yaml:"buffer_size"`
	} `yaml:"diagnostics"`

	PresetLibrary *struct {
		URL *string `yaml:"url"`
	} `yaml:"preset_library"`
}

func applyFile(cfg *Config, explicit string) error {
	path, err := resolveFilePath(explicit)
	if err != nil {
		return err
	}
	if path == "" {
		return nil // no file found; not an error
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config file %q: %w", path, err)
	}

	var fc fileConfig
	if err := yaml.Unmarshal(data, &fc); err != nil {
		return fmt.Errorf("parse config file %q: %w", path, err)
	}

	mergeFileConfig(cfg, fc)
	return nil
}

// resolveFilePath returns the path to use for file loading.
// If explicit is set, that path must exist. If explicit is empty, the first
// file found in the default search path is returned (empty string = none found).
func resolveFilePath(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("config file not found: %q", explicit)
		}
		return explicit, nil
	}
	for _, p := range defaultSearchPaths() {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", nil
}

// mergeFileConfig overlays the non-nil fields from fc onto cfg.
// Each section is handled by its own helper to keep cyclomatic complexity low.
func mergeFileConfig(cfg *Config, fc fileConfig) {
	mergeHTTPFile(cfg, fc)
	mergeNATSFile(cfg, fc)
	mergeStoreFile(cfg, fc)
	mergeLogFile(cfg, fc)
	mergeSchedulerFile(cfg, fc)
	mergeDiscoveryFile(cfg, fc)
	mergeOpenJDFile(cfg, fc)
	mergeDiagnosticsFile(cfg, fc)
	mergePresetLibraryFile(cfg, fc)
}

func mergeHTTPFile(cfg *Config, fc fileConfig) {
	if fc.HTTP == nil {
		return
	}
	if fc.HTTP.Addr != nil {
		cfg.HTTP.Addr = *fc.HTTP.Addr
	}
	if fc.HTTP.EnablePprof != nil {
		cfg.HTTP.EnablePprof = *fc.HTTP.EnablePprof
	}
}

func mergeNATSFile(cfg *Config, fc fileConfig) {
	if fc.NATS == nil {
		return
	}
	if fc.NATS.Addr != nil {
		cfg.NATS.Addr = *fc.NATS.Addr
	}
	if fc.NATS.DataDir != nil {
		cfg.NATS.DataDir = *fc.NATS.DataDir
	}
	if fc.NATS.MaxStoreMB != nil {
		cfg.NATS.MaxStoreMB = *fc.NATS.MaxStoreMB
	}
}

func mergeStoreFile(cfg *Config, fc fileConfig) {
	if fc.Store == nil {
		return
	}
	if fc.Store.SQLitePath != nil {
		cfg.Store.SQLitePath = *fc.Store.SQLitePath
	}
	if fc.Store.CheckpointInterval != nil {
		if d, err := time.ParseDuration(*fc.Store.CheckpointInterval); err == nil {
			cfg.Store.CheckpointInterval = d
		}
	}
}

func mergeLogFile(cfg *Config, fc fileConfig) {
	if fc.Log == nil {
		return
	}
	if fc.Log.Level != nil {
		cfg.Log.Level = *fc.Log.Level
	}
	if fc.Log.Format != nil {
		cfg.Log.Format = *fc.Log.Format
	}
}

func mergeSchedulerFile(cfg *Config, fc fileConfig) {
	if fc.Scheduler == nil {
		return
	}
	if fc.Scheduler.HeartbeatTimeout != nil {
		if d, err := time.ParseDuration(*fc.Scheduler.HeartbeatTimeout); err == nil {
			cfg.Scheduler.HeartbeatTimeout = d
		}
	}
	if fc.Scheduler.TickInterval != nil {
		if d, err := time.ParseDuration(*fc.Scheduler.TickInterval); err == nil {
			cfg.Scheduler.TickInterval = d
		}
	}
	if fc.Scheduler.MaxTasksPerWorker != nil {
		cfg.Scheduler.MaxTasksPerWorker = *fc.Scheduler.MaxTasksPerWorker
	}
	if fc.Scheduler.OfflineWorkerRetention != nil {
		if d, err := time.ParseDuration(*fc.Scheduler.OfflineWorkerRetention); err == nil {
			cfg.Scheduler.OfflineWorkerRetention = d
		}
	}
	if fc.Scheduler.JobRetention != nil {
		if d, err := time.ParseDuration(*fc.Scheduler.JobRetention); err == nil {
			cfg.Scheduler.JobRetention = d
		}
	}
	if fc.Scheduler.JobRetentionIncludeFailed != nil {
		cfg.Scheduler.JobRetentionIncludeFailed = *fc.Scheduler.JobRetentionIncludeFailed
	}
}

func mergeDiscoveryFile(cfg *Config, fc fileConfig) {
	if fc.Discovery == nil {
		return
	}
	if fc.Discovery.Enabled != nil {
		cfg.Discovery.Enabled = *fc.Discovery.Enabled
	}
	if fc.Discovery.InstanceName != nil {
		cfg.Discovery.InstanceName = *fc.Discovery.InstanceName
	}
}

func mergeOpenJDFile(cfg *Config, fc fileConfig) {
	if fc.OpenJD == nil {
		return
	}
	if fc.OpenJD.EnforceLimits != nil {
		cfg.OpenJD.EnforceLimits = *fc.OpenJD.EnforceLimits
	}
}

func mergeDiagnosticsFile(cfg *Config, fc fileConfig) {
	if fc.Diagnostics == nil {
		return
	}
	if fc.Diagnostics.BufferSize != nil {
		cfg.Diagnostics.BufferSize = *fc.Diagnostics.BufferSize
	}
}

func mergePresetLibraryFile(cfg *Config, fc fileConfig) {
	if fc.PresetLibrary == nil {
		return
	}
	if fc.PresetLibrary.URL != nil {
		cfg.PresetLibrary.URL = *fc.PresetLibrary.URL
	}
}

// ── Environment variable layer ────────────────────────────────────────────────

func applyEnv(cfg *Config) {
	setString(&cfg.HTTP.Addr, "SQI_HTTP_ADDR")
	setBool(&cfg.HTTP.EnablePprof, "SQI_HTTP_ENABLE_PPROF")

	setString(&cfg.NATS.Addr, "SQI_NATS_ADDR")
	setString(&cfg.NATS.DataDir, "SQI_NATS_DATA_DIR")
	setInt(&cfg.NATS.MaxStoreMB, "SQI_NATS_MAX_STORE_MB")

	setString(&cfg.Store.SQLitePath, "SQI_STORE_SQLITE_PATH")
	setDuration(&cfg.Store.CheckpointInterval, "SQI_STORE_CHECKPOINT_INTERVAL")

	setString(&cfg.Log.Level, "SQI_LOG_LEVEL")
	setString(&cfg.Log.Format, "SQI_LOG_FORMAT")

	setDuration(&cfg.Scheduler.HeartbeatTimeout, "SQI_SCHEDULER_HEARTBEAT_TIMEOUT")
	setDuration(&cfg.Scheduler.TickInterval, "SQI_SCHEDULER_TICK_INTERVAL")
	setInt(&cfg.Scheduler.MaxTasksPerWorker, "SQI_SCHEDULER_MAX_TASKS_PER_WORKER")
	setDuration(&cfg.Scheduler.OfflineWorkerRetention, "SQI_SCHEDULER_OFFLINE_WORKER_RETENTION")
	setDuration(&cfg.Scheduler.JobRetention, "SQI_SCHEDULER_JOB_RETENTION")
	setBool(&cfg.Scheduler.JobRetentionIncludeFailed, "SQI_SCHEDULER_JOB_RETENTION_INCLUDE_FAILED")

	setBool(&cfg.Discovery.Enabled, "SQI_DISCOVERY_ENABLED")
	setString(&cfg.Discovery.InstanceName, "SQI_DISCOVERY_INSTANCE_NAME")

	setBool(&cfg.OpenJD.EnforceLimits, "SQI_OPENJD_ENFORCE_LIMITS")

	setInt(&cfg.Diagnostics.BufferSize, "SQI_DIAGNOSTICS_BUFFER_SIZE")

	setString(&cfg.PresetLibrary.URL, "SQI_PRESET_LIBRARY_URL")
}

func setString(dst *string, key string) {
	if v := os.Getenv(key); v != "" {
		*dst = v
	}
}

func setInt(dst *int, key string) {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			*dst = n
		}
	}
}

func setDuration(dst *time.Duration, key string) {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			*dst = d
		}
	}
}

func setBool(dst *bool, key string) {
	if v := os.Getenv(key); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes", "on":
			*dst = true
		case "0", "false", "no", "off":
			*dst = false
		}
	}
}

// ── CLI flag layer ────────────────────────────────────────────────────────────

func applyFlags(cfg *Config, f FlagOverrides) {
	if f.LogLevel != "" {
		cfg.Log.Level = f.LogLevel
	}
	if f.LogFormat != "" {
		cfg.Log.Format = f.LogFormat
	}
	if f.HTTPAddr != "" {
		cfg.HTTP.Addr = f.HTTPAddr
	}
	if f.EnforceLimits != nil {
		cfg.OpenJD.EnforceLimits = *f.EnforceLimits
	}
}
