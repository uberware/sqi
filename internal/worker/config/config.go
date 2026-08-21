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
	"runtime"
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

	// Isolation controls running tasks as a queue-configured OS user
	// (protocol.AssignMsg.Isolation). Auth/isolation is opt-in: a worker with
	// a zero-value IsolationConfig behaves exactly as before this feature
	// existed.
	Isolation IsolationConfig `yaml:"isolation"`

	// Expr bounds what one assignment may spend evaluating OpenJD EXPR
	// expressions on this host. Defaults reproduce the fixed limits every
	// release before EXPR sub-project E4d compiled in.
	Expr ExprConfig `yaml:"expr"`
}

// ExprConfig is the operator-facing form of internal/worker/fmtres's
// ExprLimits: the five numbers that bound phase-3 (task-execution-time) OpenJD
// expression evaluation on THIS host.
//
// Each worker reads its own file, which is what lets a heterogeneous farm size
// these to each host's real memory. The server has its own, separate set
// (internal/config's openjd.expr_* keys) covering submission-time evaluation;
// the two are independent, with one relation between them that configuration
// can break -- see AssignmentPositions.
//
// THREE THINGS TO KNOW BEFORE RAISING ANY OF THEM, none of which is obvious
// from the numbers:
//
//  1. NONE OF THESE BOUNDS WALL CLOCK. Specification section 1.3.10 prices 256
//     bytes at one operation, so an operation's real cost varies by orders of
//     magnitude. Raising a limit lengthens the worst assignment this host can
//     be asked to resolve, roughly in proportion; no setting makes a slow
//     resolution impossible.
//  2. expr.operation_limit AND expr.assignment_positions MULTIPLY. The
//     cumulative operation ceiling for one assignment is their product, so
//     raising both raises it twice over.
//  3. THE BYTE DIMENSIONS COUNT CUMULATIVE ALLOCATION, NOT PEAK LIVE
//     RETENTION. A session that never holds more than a few MB at once is
//     still charged the sum of every environment it enters, so sizing host RAM
//     against expr.assignment_retained_bytes over-provisions, and sizing that
//     number against an observed RSS under-bounds it.
//
// An out-of-range value is a startup failure, not a clamp (see [Validate]).
type ExprConfig struct {
	// OperationLimit is the specification section 1.3.10 operation budget
	// for ONE phase-3 expression evaluation.
	// Default: 1000000   Range: 10000-10000000
	// Env: SQI_WORKER_EXPR_OPERATION_LIMIT
	OperationLimit int64 `yaml:"operation_limit"`

	// MemoryLimit is the specification section 1.3.9 live-byte budget for
	// ONE phase-3 expression evaluation. The per-let-block structural ceiling
	// is 50x this number, which is the figure to size host RAM against.
	// Default: 20000000   Range: 1000000-200000000
	// Env: SQI_WORKER_EXPR_MEMORY_LIMIT
	MemoryLimit int64 `yaml:"memory_limit"`

	// AssignmentPositions is how many expression positions (a command, one
	// args entry, one embedded file, one variable value) one assignment may
	// resolve, summed across the task's own symbol table and every environment
	// the session enters.
	//
	// MUST NOT BE LOWER THAN THE SERVER'S openjd.expr_template_positions. An
	// assignment's positions are a subset of its template's, so a lower value
	// here rejects, on this host and once per task, a job the server already
	// accepted -- naming a budget the submitter never saw. Nothing checks that
	// across the two processes today.
	// Default: 10000   Range: 2000-100000
	// Env: SQI_WORKER_EXPR_ASSIGNMENT_POSITIONS
	AssignmentPositions int64 `yaml:"assignment_positions"`

	// AssignmentRetainedBytes is the cumulative size of every let-bound
	// value every phase-3 symbol table one assignment builds retains. See
	// caveat 3 above: this is allocation across the session's entry phase, not
	// simultaneously live memory.
	// Default: 20000000   Range: 2000000-200000000
	// Env: SQI_WORKER_EXPR_ASSIGNMENT_RETAINED_BYTES
	AssignmentRetainedBytes int64 `yaml:"assignment_retained_bytes"`

	// LetRetainedBytes is what ONE phase-3 symbol table may hold live.
	// Note that it measures the WHOLE table, so a job whose own parameters are
	// large spends budget its let: block never asked for.
	// Default: 10000000   Range: 1000000-100000000
	// Env: SQI_WORKER_EXPR_LET_RETAINED_BYTES
	LetRetainedBytes int64 `yaml:"let_retained_bytes"`
}

// The defaults and the range an operator may configure each expr: key to.
//
// These are deliberate COPIES of internal/worker/fmtres's own constants: this
// package is the operator-facing configuration layer and fmtres is the
// enforcement leaf, and the worker's config package does not import it (the
// same direction internal/config and internal/openjd keep). cmd/sqi-worker is
// the one place that sees both, and its TestExprLimitsBounds_MatchFmtres fails
// if a number here ever drifts from the one it bounds.
//
// Which of the five is a CATASTROPHE bound (a tight ceiling, tied to a
// measured failure) and which is a POLICY bound (wide but finite), and why, is
// recorded on fmtres.ExprLimits' own fields.
const (
	DefaultWorkerExprOperationLimit int64 = 1_000_000
	MinWorkerExprOperationLimit     int64 = 10_000
	MaxWorkerExprOperationLimit     int64 = 10_000_000

	DefaultWorkerExprMemoryLimit int64 = 20_000_000
	MinWorkerExprMemoryLimit     int64 = 1_000_000
	MaxWorkerExprMemoryLimit     int64 = 200_000_000

	DefaultWorkerExprAssignmentPositions int64 = 10_000
	MinWorkerExprAssignmentPositions     int64 = 2_000
	MaxWorkerExprAssignmentPositions     int64 = 100_000

	DefaultWorkerExprAssignmentRetainedBytes int64 = 20_000_000
	MinWorkerExprAssignmentRetainedBytes     int64 = 2_000_000
	MaxWorkerExprAssignmentRetainedBytes     int64 = 200_000_000

	DefaultWorkerExprLetRetainedBytes int64 = 10_000_000
	MinWorkerExprLetRetainedBytes     int64 = 1_000_000
	MaxWorkerExprLetRetainedBytes     int64 = 100_000_000
)

// IsolationConfig controls running tasks as a queue-configured OS user.
type IsolationConfig struct {
	// Required makes the worker exit at boot when it cannot assume another
	// identity. Default false, preserving pre-isolation behavior: a worker
	// with no isolation capability keeps running ordinary queues.
	// Env: SQI_WORKER_ISOLATION_REQUIRED
	Required bool `yaml:"required"`

	// Provider selects the Windows credential mechanism. "logon_user" (the
	// default) is the only supported value.
	//
	// "s4u" is refused. An S4U logon needs no password, but the token it
	// produces carries no NETWORK credentials: any UNC path, mapped drive, or
	// authenticated license server fails as ANONYMOUS from inside the task.
	// On a render farm, where scene files and outputs live on a share, that
	// disqualifies it as a general-purpose provider, and no driver exists for
	// a worker whose jobs touch local scratch exclusively. Adding it later is
	// additive.
	// Env: SQI_WORKER_ISOLATION_PROVIDER
	Provider string `yaml:"provider"`

	// EnvPassthrough lists daemon environment variable NAME patterns inherited
	// by isolated tasks, in addition to the minimal base. filepath.Match
	// globs. Render farms depend on inherited licensing variables, so a pure
	// allowlist with no escape hatch would break real jobs on day one.
	// Env: SQI_WORKER_ISOLATION_ENV_PASSTHROUGH (comma-separated)
	EnvPassthrough []string `yaml:"env_passthrough"`
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

	// CredentialFile is the path to this worker's nkey seed file. When empty
	// it defaults to <worker.data_dir>/worker.nk. The file is created by
	// enrollment or by `sqi-worker keygen` and must be mode 0600.
	// Env: SQI_WORKER_NATS_CREDENTIAL_FILE
	CredentialFile string `yaml:"credential_file"`

	// JoinToken is a worker enrollment token. Used exactly once, on first
	// start, to obtain a credential; ignored once CredentialFile exists.
	// Prefer JoinTokenFile — a token in a config file is a secret at rest.
	// Env: SQI_WORKER_NATS_JOIN_TOKEN
	JoinToken string `yaml:"join_token"`

	// JoinTokenFile is a path to a file containing a join token. Takes
	// precedence over JoinToken.
	// Env: SQI_WORKER_NATS_JOIN_TOKEN_FILE
	JoinTokenFile string `yaml:"join_token_file"`

	// ServerURL is the sqi-server HTTP base URL used for enrollment, e.g.
	// "http://sqi-server.example:8080". Enrollment runs over REST, not over
	// NATS: the broker's job is to reject unauthenticated connections, so it
	// cannot also be the channel a worker gets its first credential over.
	// When empty, the URL is derived from mDNS discovery.
	// Env: SQI_WORKER_NATS_SERVER_URL
	ServerURL string `yaml:"server_url"`
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

	// DataDir is the directory used to persist the worker ID file
	// (worker.id) — the worker's stable, server-correlated identity. It is
	// NEVER shared with session working directories (see SessionDir) and is
	// never widened for run-as-user traversal: it stays private (0700) for
	// as long as the worker exists.
	// Env: SQI_WORKER_DATA_DIR
	DataDir string `yaml:"data_dir"`

	// SessionDir is the directory under which session working directories
	// (<SessionDir>/<sessionID>/) are created. Deliberately separate from
	// DataDir: session scratch is ephemeral and, when isolation is in use,
	// must be traversable by whichever run-as-user identity a session
	// resolves to, while DataDir holds the persistent worker-id and must stay
	// private. Left empty (the default), the effective value is resolved at
	// startup (see cmd/sqi-worker's effectiveSessionRoot):
	//   - running as root: /var/lib/sqi-worker-sessions (a SIBLING of, never
	//     a descendant of, this package's own data_dir default — see
	//     cmd/sqi-worker's defaultRootSessionDir doc for why the two must
	//     never nest; its ancestors, /var and /var/lib, are 0755 on every
	//     real distribution, so nothing needs to be created or widened
	//     specifically for this)
	//   - otherwise: <DataDir>/sessions, the same location used before this
	//     split existed — real run-as-user isolation cannot function without
	//     root regardless of directory permissions, so there is nothing to
	//     protect by moving it.
	// Env: SQI_WORKER_SESSION_DIR
	SessionDir string `yaml:"session_dir"`

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
		Isolation: IsolationConfig{
			Provider: "logon_user",
		},
		Expr: ExprConfig{
			OperationLimit:          DefaultWorkerExprOperationLimit,
			MemoryLimit:             DefaultWorkerExprMemoryLimit,
			AssignmentPositions:     DefaultWorkerExprAssignmentPositions,
			AssignmentRetainedBytes: DefaultWorkerExprAssignmentRetainedBytes,
			LetRetainedBytes:        DefaultWorkerExprLetRetainedBytes,
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

	// CredentialFile defaults relative to Worker.DataDir, which any of the
	// three layers above may have changed, so it is resolved last rather
	// than at struct-literal time in Default.
	if cfg.NATS.CredentialFile == "" {
		cfg.NATS.CredentialFile = filepath.Join(cfg.Worker.DataDir, "worker.nk")
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
	applyIsolationEnv(&cfg.Isolation)
	applyExprEnv(&cfg.Expr)
}

// applyExprEnv overlays the SQI_WORKER_EXPR_* variables onto c.
//
// A malformed value is IGNORED rather than rejected, matching every other
// numeric env var in this file (applyNATSEnv, applyLogStreamerEnv): the lower
// layer's value survives, and [Validate] still has the final say on whatever
// value results. That is deliberately unlike internal/config's server-side
// loader, which errors on a malformed SQI_OPENJD_EXPR_* value -- this file has
// no error channel to report one through.
func applyExprEnv(c *ExprConfig) {
	setInt64Env("SQI_WORKER_EXPR_OPERATION_LIMIT", &c.OperationLimit)
	setInt64Env("SQI_WORKER_EXPR_MEMORY_LIMIT", &c.MemoryLimit)
	setInt64Env("SQI_WORKER_EXPR_ASSIGNMENT_POSITIONS", &c.AssignmentPositions)
	setInt64Env("SQI_WORKER_EXPR_ASSIGNMENT_RETAINED_BYTES", &c.AssignmentRetainedBytes)
	setInt64Env("SQI_WORKER_EXPR_LET_RETAINED_BYTES", &c.LetRetainedBytes)
}

// setInt64Env overwrites *dst with the parsed value of environment variable
// name, when it is set and parses as a base-10 int64.
func setInt64Env(name string, dst *int64) {
	v := os.Getenv(name)
	if v == "" {
		return
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		*dst = n
	}
}

func applyIsolationEnv(c *IsolationConfig) {
	if v := os.Getenv("SQI_WORKER_ISOLATION_REQUIRED"); v != "" {
		c.Required = parseBoolEnv(v)
	}
	if v := os.Getenv("SQI_WORKER_ISOLATION_PROVIDER"); v != "" {
		c.Provider = v
	}
	if v := os.Getenv("SQI_WORKER_ISOLATION_ENV_PASSTHROUGH"); v != "" {
		c.EnvPassthrough = splitTags(v)
	}
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
	if v := os.Getenv("SQI_WORKER_NATS_CREDENTIAL_FILE"); v != "" {
		c.CredentialFile = v
	}
	if v := os.Getenv("SQI_WORKER_NATS_JOIN_TOKEN"); v != "" {
		c.JoinToken = v
	}
	if v := os.Getenv("SQI_WORKER_NATS_JOIN_TOKEN_FILE"); v != "" {
		c.JoinTokenFile = v
	}
	if v := os.Getenv("SQI_WORKER_NATS_SERVER_URL"); v != "" {
		c.ServerURL = v
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
	if v := os.Getenv("SQI_WORKER_SESSION_DIR"); v != "" {
		c.SessionDir = v
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

	errs = append(errs, validateIsolation(cfg)...)
	errs = append(errs, validateExpr(cfg.Expr)...)

	return errs
}

// validateExpr rejects an out-of-range expr: value at STARTUP rather than
// clamping it. A clamp would leave an operator who typed 100 believing they
// had tightened the host when they had not.
//
// The message names the bound, the observed value, the env var and the YAML
// key, because a limit rejected with only "out of range" sends the operator
// looking through three layers to find which one set it.
func validateExpr(cfg ExprConfig) []ValidationError {
	limits := []struct {
		key      string
		env      string
		min, max int64
		value    int64
	}{
		{
			key: "expr.operation_limit", env: "SQI_WORKER_EXPR_OPERATION_LIMIT",
			value: cfg.OperationLimit,
			min:   MinWorkerExprOperationLimit, max: MaxWorkerExprOperationLimit,
		},
		{
			key: "expr.memory_limit", env: "SQI_WORKER_EXPR_MEMORY_LIMIT",
			value: cfg.MemoryLimit,
			min:   MinWorkerExprMemoryLimit, max: MaxWorkerExprMemoryLimit,
		},
		{
			key: "expr.assignment_positions", env: "SQI_WORKER_EXPR_ASSIGNMENT_POSITIONS",
			value: cfg.AssignmentPositions,
			min:   MinWorkerExprAssignmentPositions, max: MaxWorkerExprAssignmentPositions,
		},
		{
			key: "expr.assignment_retained_bytes", env: "SQI_WORKER_EXPR_ASSIGNMENT_RETAINED_BYTES",
			value: cfg.AssignmentRetainedBytes,
			min:   MinWorkerExprAssignmentRetainedBytes, max: MaxWorkerExprAssignmentRetainedBytes,
		},
		{
			key: "expr.let_retained_bytes", env: "SQI_WORKER_EXPR_LET_RETAINED_BYTES",
			value: cfg.LetRetainedBytes,
			min:   MinWorkerExprLetRetainedBytes, max: MaxWorkerExprLetRetainedBytes,
		},
	}

	var errs []ValidationError
	for _, l := range limits {
		if l.value < l.min || l.value > l.max {
			errs = append(errs, ValidationError{
				Field: l.key,
				Message: fmt.Sprintf(
					"must be between %d and %d, got %d; set %s or %s",
					l.min, l.max, l.value, l.env, l.key,
				),
			})
		}
	}
	return errs
}

// validateIsolation validates the Isolation section: the isolation.required /
// worker.allow_root contradiction, and the env_passthrough glob syntax.
// Extracted from Validate to keep its cyclomatic complexity within the
// project limit.
func validateIsolation(cfg WorkerConfig) []ValidationError {
	var errs []ValidationError

	// isolation.required demands the worker be ABLE to assume another OS
	// identity; on POSIX the only mechanism today is setuid/setgid, which
	// requires the worker to run as root (see isolation.unixProvider.Capable).
	// worker.allow_root=false makes the worker refuse to even start as root
	// (executor.CheckRootUser). Configuring both together is a contradiction
	// an operator would only discover as a confusing root-user startup
	// refusal that never mentions isolation at all — surface it explicitly
	// instead. Windows is exempt: it resolves credentials through the
	// logon_user provider, which does not need the worker process itself to
	// run as a privileged account.
	if cfg.Isolation.Required && !cfg.Worker.AllowRoot && runtime.GOOS != "windows" {
		errs = append(errs, ValidationError{
			Field: "isolation.required",
			Message: "requires the worker to run as root (the POSIX isolation provider can only assume another identity from root), " +
				"but worker.allow_root is false, which refuses to start the worker as root at all; " +
				"set worker.allow_root: true or isolation.required: false",
		})
	}

	for i, pat := range cfg.Isolation.EnvPassthrough {
		if _, err := filepath.Match(pat, ""); err != nil {
			errs = append(errs, ValidationError{
				Field:   fmt.Sprintf("isolation.env_passthrough[%d]", i),
				Message: fmt.Sprintf("invalid glob %q: %v", pat, err),
			})
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
