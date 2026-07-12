// SPDX-License-Identifier: AGPL-3.0-or-later

// Package config defines the sqi-worker runtime configuration and provides
// layered loading from built-in defaults, a YAML/JSON file, SQI_WORKER_*
// environment variables, and CLI flag overrides — in that override order.
//
// The WorkerConfig struct is the canonical configuration type. Use [Load] to
// obtain a populated, validated instance. Use [Default] to obtain defaults
// alone.
//
// Note: This package is the authoritative definition of worker configuration.
// Fields added here must be mirrored in the example config file at
// config/sqi-worker.example.yaml.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/uberware/sqi/internal/worker/capabilities"
)

// WorkerConfig is the complete runtime configuration for sqi-worker.
// Zero values are not valid; use [Default] or [Load] to obtain a populated
// instance.
type WorkerConfig struct {
	// NATS is the connection configuration for the remote NATS instance
	// embedded in sqi-server.
	NATS NATSConfig `yaml:"nats"`

	// Worker contains identity and runtime behavior settings.
	Worker WorkerSettings `yaml:"worker"`

	// Log controls structured logging output.
	Log LogConfig `yaml:"log"`

	// Metrics controls the local HTTP server used for health probes and
	// Prometheus metrics.
	Metrics MetricsConfig `yaml:"metrics"`

	// Discovery controls mDNS-based server auto-discovery.
	Discovery DiscoveryConfig `yaml:"discovery"`

	// LogStreamer controls the log-chunk publisher that batches task process
	// output and streams it to sqi-server via NATS.
	LogStreamer LogStreamerConfig `yaml:"log_streamer"`

	// Diagnostics controls publishing the worker's own slog output to
	// sqi-server for display in the web UI.
	Diagnostics DiagnosticsConfig `yaml:"diagnostics"`

	// Staging configures local input/output staging for the stage_locally path
	// delivery. Both fields must be set for staging to function.
	Staging StagingConfig `yaml:"staging"`

	// Capabilities configures software capability auto-detection (built-in DCC
	// detectors plus custom detectors and a disable list).
	Capabilities capabilities.CapabilitiesConfig `yaml:"capabilities"`
}

// StagingConfig is the operator-owned configuration for the stage_locally path
// delivery. sqi never copies bytes itself: it invokes SyncCommand per path.
type StagingConfig struct {
	// ScratchDir is the base directory for per-attempt staged copies.
	ScratchDir string `yaml:"scratch_dir"`
	// SyncCommand is the command template invoked per path, with {src}, {dest},
	// and optional {object_type} placeholders, e.g. "rsync -a {src} {dest}".
	SyncCommand string `yaml:"sync_command"`

	// Defaults enables the built-in copy + TEMP scratch when staging is
	// otherwise unconfigured. Default true; set false to make an unconfigured
	// stage_locally job fail hard instead.
	// Env: SQI_STAGING_DEFAULTS
	Defaults bool `yaml:"defaults"`
}

// DiagnosticsConfig controls the diagnostic-log sink that ships the worker's
// own slog output to sqi-server over core NATS (worker.diag.<workerID>), where
// it is surfaced in the web UI.
type DiagnosticsConfig struct {
	// Enabled turns on diagnostic-log publishing.  When true (the default) the
	// worker's slog records are mirrored to sqi-server in addition to stderr.
	// Env: SQI_DIAGNOSTICS_ENABLED
	Enabled bool `yaml:"enabled"`
}

// LogStreamerConfig controls the log-chunk publisher that streams task
// process stdout/stderr to sqi-server.
type LogStreamerConfig struct {
	// MaxLinesPerChunk is the maximum number of output lines batched into a
	// single NATS message.  A flush is triggered immediately when the buffer
	// reaches this count.  Increase for verbose processes; decrease for more
	// granular live log updates.
	// Default: 50
	// Env: SQI_WORKER_LOG_STREAMER_MAX_LINES_PER_CHUNK
	MaxLinesPerChunk int `yaml:"max_lines_per_chunk"`

	// MaxBytesPerChunk is the maximum total byte count of line content in a
	// single NATS message.  A flush is triggered when the accumulated bytes
	// reach this limit after adding a line.
	// Default: 16384 (16 KB)
	// Env: SQI_WORKER_LOG_STREAMER_MAX_BYTES_PER_CHUNK
	MaxBytesPerChunk int `yaml:"max_bytes_per_chunk"`

	// FlushInterval is the maximum time a line may sit in the buffer before
	// being flushed by the background goroutine regardless of chunk thresholds.
	// Smaller values make the web UI log viewer feel more live at the cost of
	// more frequent small NATS publishes.
	// Default: 500ms
	// Env: SQI_WORKER_LOG_STREAMER_FLUSH_INTERVAL
	FlushInterval time.Duration `yaml:"flush_interval"`
}

// NATSConfig is the connection configuration for the remote NATS server.
type NATSConfig struct {
	// URL is the NATS server URL, e.g. "nats://localhost:4222".
	// Env: SQI_WORKER_NATS_URL
	URL string `yaml:"url"`

	// TLSCertFile is the path to the client TLS certificate (PEM).
	// Env: SQI_WORKER_NATS_TLS_CERT_FILE
	TLSCertFile string `yaml:"tls_cert_file"`

	// TLSKeyFile is the path to the client TLS key (PEM).
	// Env: SQI_WORKER_NATS_TLS_KEY_FILE
	TLSKeyFile string `yaml:"tls_key_file"`

	// TLSCAFile is the path to the CA certificate used to verify the NATS
	// server certificate (PEM).
	// Env: SQI_WORKER_NATS_TLS_CA_FILE
	TLSCAFile string `yaml:"tls_ca_file"`

	// InsecureSkipVerify disables TLS certificate verification. For
	// development environments only.
	// Env: SQI_WORKER_NATS_INSECURE_SKIP_VERIFY
	InsecureSkipVerify bool `yaml:"insecure_skip_verify"`

	// MaxReconnectAttempts is the maximum number of reconnection attempts
	// before giving up. -1 means retry indefinitely.
	// Env: SQI_WORKER_NATS_MAX_RECONNECT_ATTEMPTS
	MaxReconnectAttempts int `yaml:"max_reconnect_attempts"`

	// ReconnectWait is the base wait duration between reconnection attempts.
	// Actual wait uses exponential backoff with jitter.
	// Env: SQI_WORKER_NATS_RECONNECT_WAIT
	ReconnectWait time.Duration `yaml:"reconnect_wait"`
}

// WorkerSettings controls the worker's identity and runtime behavior.
type WorkerSettings struct {
	// Name is a human-readable label for this worker shown in the web UI.
	// Defaults to the hostname.
	// Env: SQI_WORKER_NAME
	Name string `yaml:"name"`

	// FarmID is the farm this worker belongs to. When set, the worker only
	// receives tasks belonging to that farm. When empty (the default), the
	// worker is unaffiliated and accepts tasks from any farm — suitable for
	// single-farm or development setups.
	// Env: SQI_WORKER_FARM_ID
	FarmID string `yaml:"farm_id"`

	// DataDir is the directory used to persist the worker ID file and
	// session working directories.
	// Env: SQI_WORKER_DATA_DIR
	DataDir string `yaml:"data_dir"`

	// ComputeLocation is the named location (matching a storage location in
	// sqi-server) where this worker's filesystem lives. Used for resolved-mode
	// path mapping.
	// Env: SQI_WORKER_COMPUTE_LOCATION
	ComputeLocation string `yaml:"compute_location"`

	// CapabilityTags is a list of arbitrary capability tags merged with
	// auto-detected capabilities at registration time, e.g. ["maya-2025",
	// "gpu", "highram"].
	// Env: SQI_WORKER_CAPABILITY_TAGS (comma-separated)
	CapabilityTags []string `yaml:"capability_tags"`

	// HeartbeatInterval is how often the worker publishes a heartbeat to the
	// server. Should be shorter than the server's heartbeat sweep interval.
	// Env: SQI_WORKER_HEARTBEAT_INTERVAL
	HeartbeatInterval time.Duration `yaml:"heartbeat_interval"`

	// ShutdownGracePeriod is the maximum time the worker waits for in-flight
	// tasks to finish before force-killing them on shutdown.
	// Env: SQI_WORKER_SHUTDOWN_GRACE_PERIOD
	ShutdownGracePeriod time.Duration `yaml:"shutdown_grace_period"`

	// AllowRoot permits the worker to run as the root user on Linux/macOS.
	// Disabled by default because executing render processes as root is a
	// security risk (see docs/worker-configuration.md, "worker.allow_root").
	// Env: SQI_WORKER_ALLOW_ROOT
	AllowRoot bool `yaml:"allow_root"`

	// KeepFailedSessions retains a session's working directory after the
	// session ends in failure (task cancellation, non-zero exit code, or
	// environment setup error). Useful for post-mortem inspection of partial
	// outputs and environment state. Disabled by default to avoid filling the
	// data directory on busy workers.
	// Env: SQI_WORKER_KEEP_FAILED_SESSIONS
	KeepFailedSessions bool `yaml:"keep_failed_sessions"`

	// QueueIDs restricts this worker to serving the listed queue IDs. An empty
	// list means the worker accepts assignments from all queues via a wildcard
	// JetStream consumer.  Set this when running a heterogeneous farm where
	// some workers specialise in a subset of queues.
	// Env: SQI_WORKER_QUEUE_IDS (comma-separated)
	QueueIDs []string `yaml:"queue_ids"`

	// PullIdleBackoff is the wait duration between pull attempts when the
	// work queue is empty. Prevents tight polling on idle queues.
	// Resets to zero immediately when a task is received.
	// Default: 2s
	// Env: SQI_WORKER_PULL_IDLE_BACKOFF
	PullIdleBackoff time.Duration `yaml:"pull_idle_backoff"`

	// PullNackDelay is the redelivery delay applied to an assignment when
	// pre-execution validation fails. The delay gives other workers
	// a chance to pick it up before NATS redelivers to this one.
	// Default: 5s
	// Env: SQI_WORKER_PULL_NACK_DELAY
	PullNackDelay time.Duration `yaml:"pull_nack_delay"`
}

// LogConfig controls structured logging output.
type LogConfig struct {
	// Level is the minimum log level: debug, info, warn, error.
	// Env: SQI_WORKER_LOG_LEVEL
	Level string `yaml:"level"`

	// Format is the log output format: json or text.
	// Env: SQI_WORKER_LOG_FORMAT
	Format string `yaml:"format"`
}

// MetricsConfig controls the local HTTP server for health probes and metrics.
type MetricsConfig struct {
	// Addr is the TCP address the local HTTP server listens on.
	// Env: SQI_WORKER_METRICS_ADDR
	Addr string `yaml:"addr"`

	// EnablePprof exposes Go runtime profiling endpoints at /debug/pprof/.
	// Env: SQI_WORKER_METRICS_ENABLE_PPROF
	EnablePprof bool `yaml:"enable_pprof"`
}

// DiscoveryConfig controls mDNS-based sqi-server auto-discovery.
type DiscoveryConfig struct {
	// EnableMDNS enables mDNS browsing for "_sqi._tcp" services on the local
	// network. When true and NATS.URL is empty, the worker uses the first
	// discovered server address.
	// Env: SQI_WORKER_DISCOVERY_ENABLE_MDNS
	EnableMDNS bool `yaml:"enable_mdns"`

	// MDNSTimeout is the maximum duration to wait for mDNS discovery results
	// before falling back to an error.
	// Env: SQI_WORKER_DISCOVERY_MDNS_TIMEOUT
	MDNSTimeout time.Duration `yaml:"mdns_timeout"`
}

// FlagOverrides carries values from CLI flags that take highest precedence
// during config loading. Only non-empty / non-zero values override the lower
// layers.
type FlagOverrides struct {
	// LogLevel overrides Log.Level when non-empty.
	LogLevel string
	// LogFormat overrides Log.Format when non-empty.
	LogFormat string
	// DryRun, when true, makes the start command print resolved config and exit.
	DryRun bool
	// NATSInsecureSkipVerify overrides NATS.InsecureSkipVerify when true.
	// Corresponds to --nats-insecure-skip-verify on the start subcommand.
	NATSInsecureSkipVerify bool
}

// Default returns a WorkerConfig populated with built-in defaults.
func Default() WorkerConfig {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "sqi-worker"
	}
	return WorkerConfig{
		NATS: NATSConfig{
			MaxReconnectAttempts: -1,
			ReconnectWait:        2 * time.Second,
		},
		Worker: WorkerSettings{
			Name:                hostname,
			DataDir:             defaultDataDir(),
			HeartbeatInterval:   15 * time.Second,
			ShutdownGracePeriod: 30 * time.Second,
			PullIdleBackoff:     2 * time.Second,
			PullNackDelay:       5 * time.Second,
		},
		Log: LogConfig{
			Level:  "info",
			Format: "json",
		},
		Metrics: MetricsConfig{
			Addr: "127.0.0.1:9091",
		},
		Discovery: DiscoveryConfig{
			EnableMDNS:  true,
			MDNSTimeout: 5 * time.Second,
		},
		LogStreamer: LogStreamerConfig{
			MaxLinesPerChunk: 50,
			MaxBytesPerChunk: 16 * 1024,
			FlushInterval:    500 * time.Millisecond,
		},
		Diagnostics: DiagnosticsConfig{
			Enabled: true,
		},
		Staging: StagingConfig{
			Defaults: true,
		},
	}
}

// defaultDataDir returns the platform-appropriate default data directory.
func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "/var/lib/sqi-worker"
	}
	return filepath.Join(home, ".sqi", "worker")
}

// Load returns the effective WorkerConfig by merging layers in override order:
// built-in defaults → YAML/JSON file → SQI_WORKER_* env vars → CLI flags.
//
// configFile may be empty, in which case Load searches the default paths.
func Load(configFile string, flags FlagOverrides) (WorkerConfig, error) {
	cfg := Default()

	// ── Config file layer ─────────────────────────────────────────────────
	path, err := resolveConfigFile(configFile)
	if err != nil {
		return WorkerConfig{}, fmt.Errorf("resolve config file: %w", err)
	}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return WorkerConfig{}, fmt.Errorf("read config file %s: %w", path, err)
		}
		if err := unmarshalConfig(data, &cfg); err != nil {
			return WorkerConfig{}, fmt.Errorf("parse config file %s: %w", path, err)
		}
	}

	// ── Environment variable layer ─────────────────────────────────────────
	applyEnv(&cfg)

	// ── CLI flag layer ─────────────────────────────────────────────────────
	if flags.LogLevel != "" {
		cfg.Log.Level = flags.LogLevel
	}
	if flags.LogFormat != "" {
		cfg.Log.Format = flags.LogFormat
	}
	if flags.NATSInsecureSkipVerify {
		cfg.NATS.InsecureSkipVerify = true
	}

	return cfg, nil
}

// resolveConfigFile returns the config file path to use. If explicit is
// non-empty it is used as-is (error if not found). Otherwise the default
// search paths are probed.
func resolveConfigFile(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("config file not found: %s", explicit)
		}
		return explicit, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	candidates := []string{
		filepath.Join("config", "sqi-worker.yaml"),
		filepath.Join("config", "sqi-worker.json"),
		filepath.Join(home, ".sqi", "sqi-worker.yaml"),
		filepath.Join(home, ".sqi", "sqi-worker.json"),
		"/etc/sqi/sqi-worker.yaml",
		"/etc/sqi/sqi-worker.json",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", nil // no file found; defaults + env only
}

// unmarshalConfig decodes YAML/JSON data into cfg.
// gopkg.in/yaml.v3 handles both YAML and JSON transparently.
func unmarshalConfig(data []byte, cfg *WorkerConfig) error {
	return yaml.Unmarshal(data, cfg)
}

// parseBoolEnv parses a boolean environment variable value. Returns false for
// any value that is not a valid bool string; invalid env values are silently
// ignored in favor of the lower-precedence default or config-file value.
func parseBoolEnv(s string) bool {
	b, err := strconv.ParseBool(s)
	if err != nil {
		return false
	}
	return b
}

// applyEnv overlays SQI_WORKER_* environment variables onto cfg.
// Split into per-section helpers to keep cyclomatic complexity manageable.
func applyEnv(cfg *WorkerConfig) {
	applyNATSEnv(&cfg.NATS)
	applyWorkerEnv(&cfg.Worker)
	applyLogEnv(&cfg.Log)
	applyMetricsEnv(&cfg.Metrics)
	applyDiscoveryEnv(&cfg.Discovery)
	applyLogStreamerEnv(&cfg.LogStreamer)
	applyDiagnosticsEnv(&cfg.Diagnostics)
	applyStagingEnv(&cfg.Staging)
	applyCapabilitiesEnv(&cfg.Capabilities)
}

func applyCapabilitiesEnv(c *capabilities.CapabilitiesConfig) {
	if v := os.Getenv("SQI_WORKER_CAPABILITIES_DISABLE"); v != "" {
		c.Disable = append(c.Disable, splitTags(v)...)
	}
}

func applyDiagnosticsEnv(c *DiagnosticsConfig) {
	if v := os.Getenv("SQI_DIAGNOSTICS_ENABLED"); v != "" {
		c.Enabled = parseBoolEnv(v)
	}
}

func applyStagingEnv(c *StagingConfig) {
	if v := os.Getenv("SQI_STAGING_DEFAULTS"); v != "" {
		c.Defaults = parseBoolEnv(v)
	}
}

func applyNATSEnv(c *NATSConfig) {
	if v := os.Getenv("SQI_WORKER_NATS_URL"); v != "" {
		c.URL = v
	}
	if v := os.Getenv("SQI_WORKER_NATS_TLS_CERT_FILE"); v != "" {
		c.TLSCertFile = v
	}
	if v := os.Getenv("SQI_WORKER_NATS_TLS_KEY_FILE"); v != "" {
		c.TLSKeyFile = v
	}
	if v := os.Getenv("SQI_WORKER_NATS_TLS_CA_FILE"); v != "" {
		c.TLSCAFile = v
	}
	if v := os.Getenv("SQI_WORKER_NATS_INSECURE_SKIP_VERIFY"); v != "" {
		c.InsecureSkipVerify = parseBoolEnv(v)
	}
	if v := os.Getenv("SQI_WORKER_NATS_MAX_RECONNECT_ATTEMPTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.MaxReconnectAttempts = n
		}
	}
	if v := os.Getenv("SQI_WORKER_NATS_RECONNECT_WAIT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.ReconnectWait = d
		}
	}
}

func applyWorkerEnv(c *WorkerSettings) {
	if v := os.Getenv("SQI_WORKER_NAME"); v != "" {
		c.Name = v
	}
	if v := os.Getenv("SQI_WORKER_FARM_ID"); v != "" {
		c.FarmID = v
	}
	if v := os.Getenv("SQI_WORKER_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if v := os.Getenv("SQI_WORKER_COMPUTE_LOCATION"); v != "" {
		c.ComputeLocation = v
	}
	if v := os.Getenv("SQI_WORKER_CAPABILITY_TAGS"); v != "" {
		c.CapabilityTags = splitTags(v)
	}
	if v := os.Getenv("SQI_WORKER_HEARTBEAT_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.HeartbeatInterval = d
		}
	}
	if v := os.Getenv("SQI_WORKER_SHUTDOWN_GRACE_PERIOD"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.ShutdownGracePeriod = d
		}
	}
	if v := os.Getenv("SQI_WORKER_ALLOW_ROOT"); v != "" {
		c.AllowRoot = parseBoolEnv(v)
	}
	if v := os.Getenv("SQI_WORKER_KEEP_FAILED_SESSIONS"); v != "" {
		c.KeepFailedSessions = parseBoolEnv(v)
	}
	applyWorkerPullEnv(c)
}

// applyWorkerPullEnv overlays pull-loop env vars onto c. Split from
// applyWorkerEnv to keep each function under the cyclomatic-complexity limit.
func applyWorkerPullEnv(c *WorkerSettings) {
	if v := os.Getenv("SQI_WORKER_QUEUE_IDS"); v != "" {
		c.QueueIDs = splitTags(v)
	}
	if v := os.Getenv("SQI_WORKER_PULL_IDLE_BACKOFF"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.PullIdleBackoff = d
		}
	}
	if v := os.Getenv("SQI_WORKER_PULL_NACK_DELAY"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.PullNackDelay = d
		}
	}
}

func applyLogEnv(c *LogConfig) {
	if v := os.Getenv("SQI_WORKER_LOG_LEVEL"); v != "" {
		c.Level = v
	}
	if v := os.Getenv("SQI_WORKER_LOG_FORMAT"); v != "" {
		c.Format = v
	}
}

func applyMetricsEnv(c *MetricsConfig) {
	if v := os.Getenv("SQI_WORKER_METRICS_ADDR"); v != "" {
		c.Addr = v
	}
	if v := os.Getenv("SQI_WORKER_METRICS_ENABLE_PPROF"); v != "" {
		c.EnablePprof = parseBoolEnv(v)
	}
}

func applyDiscoveryEnv(c *DiscoveryConfig) {
	if v := os.Getenv("SQI_WORKER_DISCOVERY_ENABLE_MDNS"); v != "" {
		c.EnableMDNS = parseBoolEnv(v)
	}
	if v := os.Getenv("SQI_WORKER_DISCOVERY_MDNS_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.MDNSTimeout = d
		}
	}
}

func applyLogStreamerEnv(c *LogStreamerConfig) {
	if v := os.Getenv("SQI_WORKER_LOG_STREAMER_MAX_LINES_PER_CHUNK"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.MaxLinesPerChunk = n
		}
	}
	if v := os.Getenv("SQI_WORKER_LOG_STREAMER_MAX_BYTES_PER_CHUNK"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.MaxBytesPerChunk = n
		}
	}
	if v := os.Getenv("SQI_WORKER_LOG_STREAMER_FLUSH_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.FlushInterval = d
		}
	}
}

// splitTags splits a comma-separated tag list, trimming whitespace.
func splitTags(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// ValidationError describes a single configuration problem.
type ValidationError struct {
	Field   string
	Message string
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// Validate returns a list of validation errors for cfg.
// An empty slice means the configuration is valid.
func Validate(cfg WorkerConfig) []ValidationError {
	var errs []ValidationError

	if cfg.NATS.URL == "" && !cfg.Discovery.EnableMDNS {
		errs = append(errs, ValidationError{
			Field:   "nats.url",
			Message: "must be set when discovery.enable_mdns is false",
		})
	}
	if cfg.Worker.DataDir == "" {
		errs = append(errs, ValidationError{
			Field:   "worker.data_dir",
			Message: "must not be empty",
		})
	}
	if cfg.Worker.HeartbeatInterval <= 0 {
		errs = append(errs, ValidationError{
			Field:   "worker.heartbeat_interval",
			Message: "must be a positive duration",
		})
	}
	if cfg.Worker.ShutdownGracePeriod <= 0 {
		errs = append(errs, ValidationError{
			Field:   "worker.shutdown_grace_period",
			Message: "must be a positive duration",
		})
	}

	if cfg.Worker.PullIdleBackoff < 0 {
		errs = append(errs, ValidationError{
			Field:   "worker.pull_idle_backoff",
			Message: "must be a non-negative duration (0 uses the built-in default of 2s)",
		})
	}
	if cfg.Worker.PullNackDelay < 0 {
		errs = append(errs, ValidationError{
			Field:   "worker.pull_nack_delay",
			Message: "must be a non-negative duration (0 uses the built-in default of 5s)",
		})
	}

	switch strings.ToLower(cfg.Log.Level) {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, ValidationError{
			Field:   "log.level",
			Message: fmt.Sprintf("unrecognized level %q; must be debug, info, warn, or error", cfg.Log.Level),
		})
	}

	switch strings.ToLower(cfg.Log.Format) {
	case "json", "text":
	default:
		errs = append(errs, ValidationError{
			Field:   "log.format",
			Message: fmt.Sprintf("unrecognized format %q; must be json or text", cfg.Log.Format),
		})
	}

	errs = append(errs, validateLogStreamer(cfg.LogStreamer)...)

	for i, d := range cfg.Capabilities.Detect {
		if err := d.Validate(); err != nil {
			errs = append(errs, ValidationError{Field: fmt.Sprintf("capabilities.detect[%d]", i), Message: err.Error()})
		}
	}

	return errs
}

// validateLogStreamer validates the LogStreamerConfig fields.
// Zero values are self-corrected by logstreamer.Config.applyDefaults, so only
// explicitly negative values (almost certainly typos) are rejected here.
func validateLogStreamer(c LogStreamerConfig) []ValidationError {
	var errs []ValidationError
	if c.MaxLinesPerChunk < 0 {
		errs = append(errs, ValidationError{
			Field:   "log_streamer.max_lines_per_chunk",
			Message: "must not be negative (use 0 to accept the built-in default of 50)",
		})
	}
	if c.MaxBytesPerChunk < 0 {
		errs = append(errs, ValidationError{
			Field:   "log_streamer.max_bytes_per_chunk",
			Message: "must not be negative (use 0 to accept the built-in default of 16384)",
		})
	}
	if c.FlushInterval < 0 {
		errs = append(errs, ValidationError{
			Field:   "log_streamer.flush_interval",
			Message: "must not be negative (use 0 to accept the built-in default of 500ms)",
		})
	}
	return errs
}
