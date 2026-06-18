// SPDX-License-Identifier: AGPL-3.0-or-later

// Package config defines the sqi-server runtime configuration and provides
// layered loading from built-in defaults, a YAML/JSON file, SQI_* environment
// variables, and CLI flag overrides — in that override order.
//
// Typical usage in the serve subcommand:
//
//	cfg, err := config.Load(flagConfigFile, config.FlagOverrides{
//	    LogLevel:  flagLogLevel,
//	    LogFormat: flagLogFormat,
//	    HTTPAddr:  flagHTTPAddr,
//	})
//	if err != nil {
//	    return err
//	}
//	if errs := config.Validate(cfg); len(errs) > 0 {
//	    for _, e := range errs {
//	        fmt.Fprintf(os.Stderr, "config error: %s: %s\n", e.Field, e.Message)
//	    }
//	    return fmt.Errorf("%d configuration error(s)", len(errs))
//	}
package config

import "time"

// Config is the complete runtime configuration for sqi-server.
// Zero values are not valid; use [DefaultConfig] or [Load] to obtain a
// populated instance.
type Config struct {
	HTTP        HTTPConfig        `yaml:"http"`
	NATS        NATSConfig        `yaml:"nats"`
	Store       StoreConfig       `yaml:"store"`
	Log         LogConfig         `yaml:"log"`
	Scheduler   SchedulerConfig   `yaml:"scheduler"`
	Discovery   DiscoveryConfig   `yaml:"discovery"`
	OpenJD      OpenJDConfig      `yaml:"openjd"`
	Diagnostics DiagnosticsConfig `yaml:"diagnostics"`
}

// HTTPConfig controls the REST and WebSocket listener.
type HTTPConfig struct {
	// Addr is the TCP address the HTTP server listens on.
	// Env: SQI_HTTP_ADDR
	Addr string `yaml:"addr"`

	// EnablePprof exposes the Go runtime profiling endpoints at /debug/pprof/
	// when true. Profiling data is sensitive; never enable this on a server
	// accessible to untrusted networks. Disabled by default.
	// Env: SQI_HTTP_ENABLE_PPROF
	EnablePprof bool `yaml:"enable_pprof"`
}

// NATSConfig controls the embedded NATS JetStream broker.
type NATSConfig struct {
	// Addr is the TCP address the embedded NATS server binds to.
	// Default is loopback so external clients cannot reach it directly;
	// sqi-server communicates with NATS in-process.
	// Env: SQI_NATS_ADDR
	Addr string `yaml:"addr"`

	// DataDir is the directory used by JetStream for file-backed stream
	// storage. Created at startup if it does not exist.
	// Env: SQI_NATS_DATA_DIR
	DataDir string `yaml:"data_dir"`

	// MaxStoreMB is the JetStream file-storage cap in megabytes.
	// Env: SQI_NATS_MAX_STORE_MB
	MaxStoreMB int `yaml:"max_store_mb"`
}

// StoreConfig controls the embedded SQLite state store.
type StoreConfig struct {
	// SQLitePath is the path to the SQLite database file.
	// Created at startup if it does not exist.
	// Env: SQI_STORE_SQLITE_PATH
	SQLitePath string `yaml:"sqlite_path"`

	// CheckpointInterval is how often the background goroutine runs a WAL
	// checkpoint (PRAGMA wal_checkpoint(TRUNCATE)) to fold committed WAL frames
	// back into the main database file and keep the WAL from growing unboundedly.
	// Must be > 0. Set to a large value (e.g. "24h") to effectively disable
	// periodic checkpointing (a final checkpoint always runs on shutdown).
	// Env: SQI_STORE_CHECKPOINT_INTERVAL
	CheckpointInterval time.Duration `yaml:"checkpoint_interval"`
}

// LogConfig controls structured log output.
type LogConfig struct {
	// Level is the minimum log level to emit.
	// Accepted values: debug, info, warn, error.
	// Env: SQI_LOG_LEVEL
	Level string `yaml:"level"`

	// Format selects the log output format.
	// Accepted values: json, text.
	// Env: SQI_LOG_FORMAT
	Format string `yaml:"format"`
}

// SchedulerConfig controls the task assignment loop behavior.
// These fields are defined now for completeness and config file documentation;
// they are consumed by the scheduler component.
type SchedulerConfig struct {
	// HeartbeatTimeout is the duration after which a worker that has not sent
	// a heartbeat is declared offline and its in-flight tasks are reclaimed.
	// Env: SQI_SCHEDULER_HEARTBEAT_TIMEOUT
	HeartbeatTimeout time.Duration `yaml:"heartbeat_timeout"`

	// TickInterval is how often the assignment loop wakes to match ready tasks
	// to available workers.
	// Env: SQI_SCHEDULER_TICK_INTERVAL
	TickInterval time.Duration `yaml:"tick_interval"`

	// MaxTasksPerWorker caps the number of tasks that can be simultaneously
	// assigned to a single worker. Must be ≥ 1.
	// Env: SQI_SCHEDULER_MAX_TASKS_PER_WORKER
	MaxTasksPerWorker int `yaml:"max_tasks_per_worker"`
}

// DiscoveryConfig controls mDNS service advertisement.
type DiscoveryConfig struct {
	// Enabled controls whether sqi-server broadcasts a _sqi._tcp mDNS record
	// on the local network. Disable in environments that forbid multicast.
	// Env: SQI_DISCOVERY_ENABLED
	Enabled bool `yaml:"enabled"`

	// InstanceName is the mDNS service instance name advertised on the
	// network. Defaults to "sqi-server".
	// Env: SQI_DISCOVERY_INSTANCE_NAME
	InstanceName string `yaml:"instance_name"`
}

// DiagnosticsConfig controls the in-memory diagnostic-log ring buffer surfaced
// in the web UI.
type DiagnosticsConfig struct {
	// BufferSize is the maximum diagnostic records retained per component
	// (server + each worker). 0 disables diagnostics entirely: no ring buffer,
	// no worker.diag subscription, and the REST endpoint returns 503. A positive
	// value is the per-component capacity. Default: 1000. Set to 0 to avoid
	// spending memory on something accessed out-of-band
	// (journald/Docker/Loki/etc.). Must not be negative.
	// Env: SQI_DIAGNOSTICS_BUFFER_SIZE
	BufferSize int `yaml:"buffer_size"`
}

// OpenJDConfig controls OpenJD submission and validation behavior.
type OpenJDConfig struct {
	// EnforceLimits gates quantitative limit validation (maximum name lengths,
	// counts, reserved-name rules). Disable only when onboarding templates that
	// predate the strict limits and cannot yet be updated.
	// Env: SQI_OPENJD_ENFORCE_LIMITS
	EnforceLimits bool `yaml:"enforce_limits"`
}

// DefaultConfig returns a [Config] with sensible defaults suitable for local
// development. Production deployments should override fields via a config file
// or SQI_* environment variables.
func DefaultConfig() Config {
	return Config{
		HTTP: HTTPConfig{
			Addr: "0.0.0.0:8080",
		},
		NATS: NATSConfig{
			Addr:       "0.0.0.0:4222",
			DataDir:    "data/nats",
			MaxStoreMB: 1024,
		},
		Store: StoreConfig{
			SQLitePath:         "sqi.db",
			CheckpointInterval: 5 * time.Minute,
		},
		Log: LogConfig{
			Level:  "info",
			Format: "json",
		},
		Scheduler: SchedulerConfig{
			HeartbeatTimeout:  30 * time.Second,
			TickInterval:      500 * time.Millisecond,
			MaxTasksPerWorker: 1,
		},
		Discovery: DiscoveryConfig{
			Enabled:      true,
			InstanceName: "sqi-server",
		},
		OpenJD: OpenJDConfig{
			EnforceLimits: true,
		},
		Diagnostics: DiagnosticsConfig{
			BufferSize: 1000,
		},
	}
}
