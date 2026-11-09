// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/uberware/sqi/internal/envvar"
	"github.com/uberware/sqi/internal/yamlstrict"
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
	// HTTPCORSOrigins overrides HTTP.CORSOrigins when non-empty. Bound to
	// --http-cors-origins.
	HTTPCORSOrigins []string
	// EnforceLimits overrides OpenJD.EnforceLimits when non-nil. Bound to
	// --openjd-enforce-limits. A pointer (not a bool) so an unset flag leaves the
	// lower layers intact while an explicit --openjd-enforce-limits=false can turn
	// the (default-true) gate off.
	EnforceLimits *bool
	// AuthEnabled overrides Auth.Enabled when non-nil. Bound to --auth-enabled.
	// A pointer so an unset flag leaves the lower layers intact.
	AuthEnabled *bool
	// ValidateJobOwner overrides Auth.ValidateJobOwner when non-nil. Bound to
	// --auth-validate-job-owner. A pointer so an unset flag leaves the lower
	// layers intact.
	ValidateJobOwner *bool
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
	cfg, _, err := LoadWithSources(filePath, flags)
	return cfg, err
}

// Sources reports, for a subset of settings, whether the config file or
// environment layer explicitly decided the value — as opposed to it being
// the built-in default that nothing overrode.
//
// Comparing a resolved value against [DefaultConfig]'s is not a sound way to
// answer this: an operator's config file (or the shipped
// config/sqi-server.example.yaml) can restate a default value's exact text
// while editing other keys, in which case value comparison cannot tell
// "explicitly configured to the default" apart from "never touched, so it
// is still the default". Sources is built from the file/env layers
// themselves, before any default-fill happens, so it is not fooled by that.
type Sources struct {
	// StoreSQLitePath is true when store.sqlite_path was set by the config
	// file or by SQI_STORE_SQLITE_PATH.
	StoreSQLitePath bool

	// StoreCheckpointInterval is true when store.checkpoint_interval was set
	// by the config file or by SQI_STORE_CHECKPOINT_INTERVAL.
	StoreCheckpointInterval bool

	// SchedulerReconcileGrace is true when scheduler.reconcile_grace was set
	// by the config file or by SQI_SCHEDULER_RECONCILE_GRACE. When it was not,
	// the loader resolves it to the larger of its default and
	// scheduler.heartbeat_timeout (see [resolveReconcileGrace]).
	SchedulerReconcileGrace bool
}

// LoadWithSources is [Load], additionally reporting which of a subset of
// settings (see [Sources]) were explicitly decided by the config file or
// environment layer.
func LoadWithSources(filePath string, flags FlagOverrides) (Config, Sources, error) {
	cfg := DefaultConfig()
	var src Sources

	// ── Layer 2: config file ──────────────────────────────────────────────
	fc, err := loadFileConfig(filePath)
	if err != nil {
		return Config{}, Sources{}, err
	}
	if fc != nil {
		mergeFileConfig(&cfg, *fc)
		if fc.Store != nil && fc.Store.SQLitePath != nil {
			src.StoreSQLitePath = true
		}
		if fc.Store != nil && fc.Store.CheckpointInterval != nil {
			src.StoreCheckpointInterval = true
		}
		if fc.Scheduler != nil && fc.Scheduler.ReconcileGrace != nil {
			src.SchedulerReconcileGrace = true
		}
	}

	// ── Layer 3: environment variables ───────────────────────────────────
	if err := applyEnv(&cfg); err != nil {
		return Config{}, Sources{}, err
	}
	if os.Getenv("SQI_STORE_SQLITE_PATH") != "" {
		src.StoreSQLitePath = true
	}
	if os.Getenv("SQI_STORE_CHECKPOINT_INTERVAL") != "" {
		src.StoreCheckpointInterval = true
	}
	if strings.TrimSpace(os.Getenv("SQI_SCHEDULER_RECONCILE_GRACE")) != "" {
		src.SchedulerReconcileGrace = true
	}

	// ── Layer 4: CLI flag overrides ───────────────────────────────────────
	applyFlags(&cfg, flags)

	resolveReconcileGrace(&cfg, src.SchedulerReconcileGrace)

	return cfg, src, nil
}

// resolveReconcileGrace raises scheduler.reconcile_grace to
// scheduler.heartbeat_timeout when that is longer, unless the config file or
// SQI_SCHEDULER_RECONCILE_GRACE set it (explicit). Validation requires
// reconcile_grace to be 0 (disabled) or at least heartbeat_timeout, so a
// heartbeat_timeout raised past the 2m default must not fail validation on a
// key the operator never set. An explicit value is left as given, for
// validation to judge. It runs after every layer, so it sees the final
// heartbeat_timeout.
func resolveReconcileGrace(cfg *Config, explicit bool) {
	if !explicit && cfg.Scheduler.ReconcileGrace < cfg.Scheduler.HeartbeatTimeout {
		cfg.Scheduler.ReconcileGrace = cfg.Scheduler.HeartbeatTimeout
	}
}

// ── File layer ────────────────────────────────────────────────────────────────

// roleMappingFile is one auth.*.role_map entry as it appears in the file. Both
// provider blocks use it, so the shape is stated once.
type roleMappingFile struct {
	Group string `yaml:"group"`
	Role  string `yaml:"role"`
}

// fileNATS is the nats block as it appears in the file. The keys that moved
// under nats.embedded (addr, data_dir, max_store_mb, tls) are not fields
// here; see [rejectRemovedNATSKeys].
type fileNATS struct {
	Mode *string `yaml:"mode"`
	Auth *struct {
		Enabled                   *bool         `yaml:"enabled"`
		UserTTL                   *fileDuration `yaml:"user_ttl"`
		JoinTokenTTL              *fileDuration `yaml:"join_token_ttl"`
		JoinTokenSingleUse        *bool         `yaml:"join_token_single_use"`
		EnrollmentEndpointEnabled *bool         `yaml:"enrollment_endpoint_enabled"`
	} `yaml:"auth"`
	Embedded *struct {
		Addr         *string `yaml:"addr"`
		AdvertiseURL *string `yaml:"advertise_url"`
		DataDir      *string `yaml:"data_dir"`
		MaxStoreMB   *int    `yaml:"max_store_mb"`
		TLS          *struct {
			Enabled      *bool   `yaml:"enabled"`
			CertFile     *string `yaml:"cert_file"`
			KeyFile      *string `yaml:"key_file"`
			ClientCAFile *string `yaml:"client_ca_file"`
		} `yaml:"tls"`
	} `yaml:"embedded"`
	External *struct {
		URLs           *[]string     `yaml:"urls"`
		AdvertiseURLs  *[]string     `yaml:"advertise_urls"`
		Account        *string       `yaml:"account"`
		NkeySeedFile   *string       `yaml:"nkey_seed_file"`
		CredsFile      *string       `yaml:"creds_file"`
		StreamReplicas *int          `yaml:"stream_replicas"`
		ConnectTimeout *fileDuration `yaml:"connect_timeout"`
		TLS            *struct {
			CAFile     *string `yaml:"ca_file"`
			CertFile   *string `yaml:"cert_file"`
			KeyFile    *string `yaml:"key_file"`
			ServerName *string `yaml:"server_name"`
		} `yaml:"tls"`
		Callout *struct {
			NkeySeedFile    *string `yaml:"nkey_seed_file"`
			IssuerSeedFile  *string `yaml:"issuer_seed_file"`
			SystemCredsFile *string `yaml:"system_creds_file"`
		} `yaml:"callout"`
	} `yaml:"external"`
}

// fileConfig is the partial YAML/JSON shape used when unmarshaling a config
// file. Every field uses a pointer so we can distinguish "not set" from a
// zero value, applying only the fields that are present in the file.
type fileConfig struct {
	HTTP *struct {
		Addr        *string   `yaml:"addr"`
		EnablePprof *bool     `yaml:"enable_pprof"`
		CORSOrigins *[]string `yaml:"cors_origins"`
		RateLimit   *struct {
			Enabled *bool `yaml:"enabled"`
		} `yaml:"rate_limit"`
		TLS *struct {
			Enabled  *bool   `yaml:"enabled"`
			CertFile *string `yaml:"cert_file"`
			KeyFile  *string `yaml:"key_file"`
		} `yaml:"tls"`
	} `yaml:"http"`

	NATS *fileNATS `yaml:"nats"`

	Store *struct {
		Driver             *string       `yaml:"driver"`
		SQLitePath         *string       `yaml:"sqlite_path"`
		CheckpointInterval *fileDuration `yaml:"checkpoint_interval"`
		Postgres           *struct {
			DSN      *string `yaml:"dsn"`
			MaxConns *int    `yaml:"max_conns"`
		} `yaml:"postgres"`
	} `yaml:"store"`

	Log *struct {
		Level      *string `yaml:"level"`
		Format     *string `yaml:"format"`
		File       *string `yaml:"file"`
		MaxSizeMB  *int    `yaml:"max_size_mb"`
		MaxBackups *int    `yaml:"max_backups"`
	} `yaml:"log"`

	Scheduler *struct {
		HeartbeatTimeout          *fileDuration `yaml:"heartbeat_timeout"`
		TickInterval              *fileDuration `yaml:"tick_interval"`
		MaxTasksPerWorker         *int          `yaml:"max_tasks_per_worker"`
		OfflineWorkerRetention    *fileDuration `yaml:"offline_worker_retention"`
		JobRetention              *fileDuration `yaml:"job_retention"`
		JobRetentionIncludeFailed *bool         `yaml:"job_retention_include_failed"`
		UnschedulableGrace        *fileDuration `yaml:"unschedulable_grace"`
		ReconcileGrace            *fileDuration `yaml:"reconcile_grace"`
		DefaultMaxAttempts        *int          `yaml:"default_max_attempts"`
		RetryDelay                *fileDuration `yaml:"retry_delay"`
		DefaultFailureLimit       *int          `yaml:"default_failure_limit"`
	} `yaml:"scheduler"`

	Discovery *struct {
		Enabled      *bool   `yaml:"enabled"`
		InstanceName *string `yaml:"instance_name"`
	} `yaml:"discovery"`

	HA *struct {
		Enabled      *bool         `yaml:"enabled"`
		LeaseTTL     *fileDuration `yaml:"lease_ttl"`
		InstanceName *string       `yaml:"instance_name"`
	} `yaml:"ha"`

	Capacity *struct {
		Providers map[string]*struct {
			Command *[]string          `yaml:"command"`
			Env     *map[string]string `yaml:"env"`
			Timeout *fileDuration      `yaml:"timeout"`
		} `yaml:"providers"`
		WakeOnLAN *struct {
			Port   *int      `yaml:"port"`
			SendTo *[]string `yaml:"send_to"`
		} `yaml:"wake_on_lan"`
	} `yaml:"capacity"`

	OpenJD *struct {
		EnforceLimits             *bool         `yaml:"enforce_limits"`
		ExprOperationLimit        *int64        `yaml:"expr_operation_limit"`
		ExprMemoryLimit           *int64        `yaml:"expr_memory_limit"`
		ExprTemplatePositions     *int64        `yaml:"expr_template_positions"`
		ExprTemplateRetainedBytes *int64        `yaml:"expr_template_retained_bytes"`
		ExprSubmissionDeadline    *fileDuration `yaml:"expr_submission_deadline"`
	} `yaml:"openjd"`

	Diagnostics *struct {
		BufferSize *int `yaml:"buffer_size"`
	} `yaml:"diagnostics"`

	PresetLibrary *struct {
		URL *string `yaml:"url"`
	} `yaml:"preset_library"`

	Auth *struct {
		Enabled          *bool `yaml:"enabled"`
		ValidateJobOwner *bool `yaml:"validate_job_owner"`
		Session          *struct {
			TTL          *fileDuration `yaml:"ttl"`
			CookieName   *string       `yaml:"cookie_name"`
			CookieSecure *string       `yaml:"cookie_secure"`
		} `yaml:"session"`
		Bootstrap *struct {
			Username *string `yaml:"username"`
			Password *string `yaml:"password"`
		} `yaml:"bootstrap"`
		LDAP *struct {
			Enabled         *bool              `yaml:"enabled"`
			URL             *string            `yaml:"url"`
			StartTLS        *bool              `yaml:"start_tls"`
			TLSSkipVerify   *bool              `yaml:"tls_skip_verify"`
			CAFile          *string            `yaml:"ca_file"`
			Timeout         *fileDuration      `yaml:"timeout"`
			BindDN          *string            `yaml:"bind_dn"`
			BindPassword    *string            `yaml:"bind_password"`
			BaseDN          *string            `yaml:"base_dn"`
			UserFilter      *string            `yaml:"user_filter"`
			NestedGroups    *bool              `yaml:"nested_groups"`
			UserDNTemplate  *string            `yaml:"user_dn_template"`
			UsernameAttr    *string            `yaml:"username_attr"`
			DisplayNameAttr *string            `yaml:"display_name_attr"`
			UniqueIDAttr    *string            `yaml:"unique_id_attr"`
			RoleSource      *string            `yaml:"role_source"`
			RoleMap         *[]roleMappingFile `yaml:"role_map"`
			DefaultRole     *string            `yaml:"default_role"`
		} `yaml:"ldap"`
		OIDC *struct {
			Enabled               *bool              `yaml:"enabled"`
			Issuer                *string            `yaml:"issuer"`
			ClientID              *string            `yaml:"client_id"`
			ClientSecret          *string            `yaml:"client_secret"`
			RedirectURL           *string            `yaml:"redirect_url"`
			Scopes                *[]string          `yaml:"scopes"`
			UsernameClaim         *string            `yaml:"username_claim"`
			DisplayNameClaim      *string            `yaml:"display_name_claim"`
			GroupsClaim           *string            `yaml:"groups_claim"`
			RoleSource            *string            `yaml:"role_source"`
			RoleMap               *[]roleMappingFile `yaml:"role_map"`
			DefaultRole           *string            `yaml:"default_role"`
			ReauthMode            *string            `yaml:"reauth_mode"`
			LogoutMode            *string            `yaml:"logout_mode"`
			PostLogoutRedirectURL *string            `yaml:"post_logout_redirect_url"`
			ButtonLabel           *string            `yaml:"button_label"`
		} `yaml:"oidc"`
	} `yaml:"auth"`
}

// loadFileConfig resolves and parses the config file, returning a nil
// *fileConfig (not an error) when none is found and filePath was not set
// explicitly.
func loadFileConfig(explicit string) (*fileConfig, error) {
	path, err := resolveFilePath(explicit)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return nil, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file %q: %w", path, err)
	}

	if err := rejectRemovedNATSKeys(path, data); err != nil {
		return nil, err
	}

	// A key the loader does not know is an error, not a skipped line: a
	// misspelled key would otherwise leave the server on the default.
	var fc fileConfig
	if err := yamlstrict.Decode(data, &fc); err != nil {
		return nil, fmt.Errorf("parse config file %q: %w", path, err)
	}

	return &fc, nil
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
	mergeHAFile(cfg, fc)
	mergeCapacityFile(cfg, fc)
	mergeOpenJDFile(cfg, fc)
	mergeDiagnosticsFile(cfg, fc)
	mergePresetLibraryFile(cfg, fc)
	mergeAuthFile(cfg, fc)
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
	if fc.HTTP.CORSOrigins != nil {
		cfg.HTTP.CORSOrigins = *fc.HTTP.CORSOrigins
	}
	if fc.HTTP.RateLimit != nil {
		setIfNotNilBool(&cfg.HTTP.RateLimit.Enabled, fc.HTTP.RateLimit.Enabled)
	}
	mergeHTTPTLSFile(cfg, fc.HTTP.TLS)
}

// mergeHTTPTLSFile overlays the http.tls sub-fields from fc onto cfg. Split
// out of [mergeHTTPFile] to keep its cyclomatic complexity under the lint
// threshold, following the mergeNATSAuthFile precedent.
func mergeHTTPTLSFile(cfg *Config, t *struct {
	Enabled  *bool   `yaml:"enabled"`
	CertFile *string `yaml:"cert_file"`
	KeyFile  *string `yaml:"key_file"`
},
) {
	if t == nil {
		return
	}
	setIfNotNilBool(&cfg.HTTP.TLS.Enabled, t.Enabled)
	setIfNotNilString(&cfg.HTTP.TLS.CertFile, t.CertFile)
	setIfNotNilString(&cfg.HTTP.TLS.KeyFile, t.KeyFile)
}

func mergeNATSFile(cfg *Config, fc fileConfig) {
	if fc.NATS == nil {
		return
	}
	setIfNotNilString(&cfg.NATS.Mode, fc.NATS.Mode)
	mergeNATSAuthFile(cfg, fc.NATS)
	mergeNATSEmbeddedFile(cfg, fc.NATS)
	mergeNATSExternalFile(cfg, fc.NATS)
}

// mergeNATSAuthFile overlays the nats.auth sub-fields from n onto cfg. Split
// out of [mergeNATSFile] to keep its cyclomatic complexity under the lint
// threshold.
func mergeNATSAuthFile(cfg *Config, n *fileNATS) {
	a := n.Auth
	if a == nil {
		return
	}
	setIfNotNilBool(&cfg.NATS.Auth.Enabled, a.Enabled)
	setIfNotNilDuration(&cfg.NATS.Auth.UserTTL, a.UserTTL)
	setIfNotNilDuration(&cfg.NATS.Auth.JoinTokenTTL, a.JoinTokenTTL)
	setIfNotNilBool(&cfg.NATS.Auth.JoinTokenSingleUse, a.JoinTokenSingleUse)
	setIfNotNilBool(&cfg.NATS.Auth.EnrollmentEndpointEnabled, a.EnrollmentEndpointEnabled)
}

// mergeNATSEmbeddedFile overlays the nats.embedded sub-fields from n onto cfg.
func mergeNATSEmbeddedFile(cfg *Config, n *fileNATS) {
	e := n.Embedded
	if e == nil {
		return
	}
	d := &cfg.NATS.Embedded
	setIfNotNilString(&d.Addr, e.Addr)
	setIfNotNilString(&d.AdvertiseURL, e.AdvertiseURL)
	setIfNotNilString(&d.DataDir, e.DataDir)
	if e.MaxStoreMB != nil {
		d.MaxStoreMB = *e.MaxStoreMB
	}
	if t := e.TLS; t != nil {
		setIfNotNilBool(&d.TLS.Enabled, t.Enabled)
		setIfNotNilString(&d.TLS.CertFile, t.CertFile)
		setIfNotNilString(&d.TLS.KeyFile, t.KeyFile)
		setIfNotNilString(&d.TLS.ClientCAFile, t.ClientCAFile)
	}
}

// mergeNATSExternalFile overlays the nats.external sub-fields from n onto cfg.
func mergeNATSExternalFile(cfg *Config, n *fileNATS) {
	e := n.External
	if e == nil {
		return
	}
	d := &cfg.NATS.External
	if e.URLs != nil {
		d.URLs = *e.URLs
	}
	if e.AdvertiseURLs != nil {
		d.AdvertiseURLs = *e.AdvertiseURLs
	}
	setIfNotNilString(&d.Account, e.Account)
	setIfNotNilString(&d.NkeySeedFile, e.NkeySeedFile)
	setIfNotNilString(&d.CredsFile, e.CredsFile)
	if e.StreamReplicas != nil {
		d.StreamReplicas = *e.StreamReplicas
	}
	setIfNotNilDuration(&d.ConnectTimeout, e.ConnectTimeout)
	if t := e.TLS; t != nil {
		setIfNotNilString(&d.TLS.CAFile, t.CAFile)
		setIfNotNilString(&d.TLS.CertFile, t.CertFile)
		setIfNotNilString(&d.TLS.KeyFile, t.KeyFile)
		setIfNotNilString(&d.TLS.ServerName, t.ServerName)
	}
	if c := e.Callout; c != nil {
		setIfNotNilString(&d.Callout.NkeySeedFile, c.NkeySeedFile)
		setIfNotNilString(&d.Callout.IssuerSeedFile, c.IssuerSeedFile)
		setIfNotNilString(&d.Callout.SystemCredsFile, c.SystemCredsFile)
	}
}

func mergeStoreFile(cfg *Config, fc fileConfig) {
	if fc.Store == nil {
		return
	}
	setIfNotNilString(&cfg.Store.Driver, fc.Store.Driver)
	if fc.Store.SQLitePath != nil {
		cfg.Store.SQLitePath = *fc.Store.SQLitePath
	}
	setIfNotNilDuration(&cfg.Store.CheckpointInterval, fc.Store.CheckpointInterval)
	if pg := fc.Store.Postgres; pg != nil {
		setIfNotNilString(&cfg.Store.Postgres.DSN, pg.DSN)
		if pg.MaxConns != nil {
			cfg.Store.Postgres.MaxConns = *pg.MaxConns
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
	if fc.Log.File != nil {
		cfg.Log.File = *fc.Log.File
	}
	if fc.Log.MaxSizeMB != nil {
		cfg.Log.MaxSizeMB = *fc.Log.MaxSizeMB
	}
	if fc.Log.MaxBackups != nil {
		cfg.Log.MaxBackups = *fc.Log.MaxBackups
	}
}

func mergeSchedulerFile(cfg *Config, fc fileConfig) {
	if fc.Scheduler == nil {
		return
	}
	setIfNotNilDuration(&cfg.Scheduler.HeartbeatTimeout, fc.Scheduler.HeartbeatTimeout)
	setIfNotNilDuration(&cfg.Scheduler.TickInterval, fc.Scheduler.TickInterval)
	if fc.Scheduler.MaxTasksPerWorker != nil {
		cfg.Scheduler.MaxTasksPerWorker = *fc.Scheduler.MaxTasksPerWorker
	}
	setIfNotNilDuration(&cfg.Scheduler.OfflineWorkerRetention, fc.Scheduler.OfflineWorkerRetention)
	setIfNotNilDuration(&cfg.Scheduler.JobRetention, fc.Scheduler.JobRetention)
	if fc.Scheduler.JobRetentionIncludeFailed != nil {
		cfg.Scheduler.JobRetentionIncludeFailed = *fc.Scheduler.JobRetentionIncludeFailed
	}
	mergeSchedulerGraceFile(cfg, fc)
	mergeSchedulerRetryFile(cfg, fc)
}

// mergeSchedulerGraceFile overlays the sweep grace periods
// (unschedulable_grace, reconcile_grace) from fc onto cfg. Split out of
// [mergeSchedulerFile] to keep its cyclomatic complexity under the lint
// threshold.
func mergeSchedulerGraceFile(cfg *Config, fc fileConfig) {
	setIfNotNilDuration(&cfg.Scheduler.UnschedulableGrace, fc.Scheduler.UnschedulableGrace)
	setIfNotNilDuration(&cfg.Scheduler.ReconcileGrace, fc.Scheduler.ReconcileGrace)
}

// mergeSchedulerRetryFile overlays the server-level retry-policy fields
// (default_max_attempts, retry_delay, default_failure_limit) from fc onto
// cfg. Split out of [mergeSchedulerFile] to keep its cyclomatic complexity
// under the lint threshold.
func mergeSchedulerRetryFile(cfg *Config, fc fileConfig) {
	if fc.Scheduler.DefaultMaxAttempts != nil {
		cfg.Scheduler.DefaultMaxAttempts = *fc.Scheduler.DefaultMaxAttempts
	}
	setIfNotNilDuration(&cfg.Scheduler.RetryDelay, fc.Scheduler.RetryDelay)
	if fc.Scheduler.DefaultFailureLimit != nil {
		cfg.Scheduler.DefaultFailureLimit = *fc.Scheduler.DefaultFailureLimit
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

func mergeHAFile(cfg *Config, fc fileConfig) {
	if fc.HA == nil {
		return
	}
	if fc.HA.Enabled != nil {
		cfg.HA.Enabled = *fc.HA.Enabled
	}
	setIfNotNilDuration(&cfg.HA.LeaseTTL, fc.HA.LeaseTTL)
	if fc.HA.InstanceName != nil {
		cfg.HA.InstanceName = *fc.HA.InstanceName
	}
}

// mergeCapacityFile overlays the capacity block. A provider the file names is
// built whole: a key it leaves out takes the default (timeout) or stays empty
// (command, env), never a value from another provider.
func mergeCapacityFile(cfg *Config, fc fileConfig) {
	if fc.Capacity == nil {
		return
	}
	if len(fc.Capacity.Providers) > 0 {
		cfg.Capacity.Providers = make(map[string]CapacityProviderConfig, len(fc.Capacity.Providers))
	}
	for name, p := range fc.Capacity.Providers {
		out := CapacityProviderConfig{Timeout: DefaultCapacityProviderTimeout}
		if p != nil {
			if p.Command != nil {
				out.Command = *p.Command
			}
			if p.Env != nil {
				out.Env = *p.Env
			}
			setIfNotNilDuration(&out.Timeout, p.Timeout)
		}
		cfg.Capacity.Providers[name] = out
	}
	if w := fc.Capacity.WakeOnLAN; w != nil {
		if w.Port != nil {
			cfg.Capacity.WakeOnLAN.Port = *w.Port
		}
		if w.SendTo != nil {
			cfg.Capacity.WakeOnLAN.SendTo = *w.SendTo
		}
	}
}

func mergeOpenJDFile(cfg *Config, fc fileConfig) {
	if fc.OpenJD == nil {
		return
	}
	if fc.OpenJD.EnforceLimits != nil {
		cfg.OpenJD.EnforceLimits = *fc.OpenJD.EnforceLimits
	}
	if fc.OpenJD.ExprOperationLimit != nil {
		cfg.OpenJD.ExprOperationLimit = *fc.OpenJD.ExprOperationLimit
	}
	if fc.OpenJD.ExprMemoryLimit != nil {
		cfg.OpenJD.ExprMemoryLimit = *fc.OpenJD.ExprMemoryLimit
	}
	if fc.OpenJD.ExprTemplatePositions != nil {
		cfg.OpenJD.ExprTemplatePositions = *fc.OpenJD.ExprTemplatePositions
	}
	if fc.OpenJD.ExprTemplateRetainedBytes != nil {
		cfg.OpenJD.ExprTemplateRetainedBytes = *fc.OpenJD.ExprTemplateRetainedBytes
	}
	setIfNotNilDuration(&cfg.OpenJD.ExprSubmissionDeadline, fc.OpenJD.ExprSubmissionDeadline)
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

func mergeAuthFile(cfg *Config, fc fileConfig) {
	if fc.Auth == nil {
		return
	}
	if fc.Auth.Enabled != nil {
		cfg.Auth.Enabled = *fc.Auth.Enabled
	}
	if fc.Auth.ValidateJobOwner != nil {
		cfg.Auth.ValidateJobOwner = *fc.Auth.ValidateJobOwner
	}
	mergeAuthSessionFile(cfg, fc.Auth.Session)
	mergeAuthBootstrapFile(cfg, fc.Auth.Bootstrap)
	mergeAuthLDAPFile(cfg, fc)
	mergeAuthOIDCFile(cfg, fc)
}

// mergeAuthSessionFile overlays the auth.session sub-fields from fc onto cfg.
// Split out of [mergeAuthFile] to keep its cyclomatic complexity under the
// lint threshold.
func mergeAuthSessionFile(cfg *Config, s *struct {
	TTL          *fileDuration `yaml:"ttl"`
	CookieName   *string       `yaml:"cookie_name"`
	CookieSecure *string       `yaml:"cookie_secure"`
},
) {
	if s == nil {
		return
	}
	setIfNotNilDuration(&cfg.Auth.Session.TTL, s.TTL)
	if s.CookieName != nil {
		cfg.Auth.Session.CookieName = *s.CookieName
	}
	if s.CookieSecure != nil {
		cfg.Auth.Session.CookieSecure = *s.CookieSecure
	}
}

// mergeAuthBootstrapFile overlays the auth.bootstrap sub-fields from fc onto
// cfg. Split out of [mergeAuthFile] to keep its cyclomatic complexity under
// the lint threshold.
func mergeAuthBootstrapFile(cfg *Config, b *struct {
	Username *string `yaml:"username"`
	Password *string `yaml:"password"`
},
) {
	if b == nil {
		return
	}
	if b.Username != nil {
		cfg.Auth.Bootstrap.Username = *b.Username
	}
	if b.Password != nil {
		cfg.Auth.Bootstrap.Password = *b.Password
	}
}

// mergeAuthLDAPFile overlays the auth.ldap sub-fields from fc onto cfg. Split
// out of [mergeAuthFile] for cyclomatic complexity. Unlike the sibling
// helpers it takes the whole fileConfig: its shadow struct has too many
// fields to restate as a parameter type.
//
// A malformed timeout is ignored rather than fatal, matching
// [mergeAuthSessionFile]'s handling of auth.session.ttl — the file layer is
// lenient where the env layer is strict, and diverging here would surprise.
func mergeAuthLDAPFile(cfg *Config, fc fileConfig) {
	if fc.Auth == nil || fc.Auth.LDAP == nil {
		return
	}
	l := fc.Auth.LDAP
	d := &cfg.Auth.LDAP
	setIfNotNilBool(&d.Enabled, l.Enabled)
	setIfNotNilString(&d.URL, l.URL)
	setIfNotNilBool(&d.StartTLS, l.StartTLS)
	setIfNotNilBool(&d.TLSSkipVerify, l.TLSSkipVerify)
	setIfNotNilString(&d.CAFile, l.CAFile)
	setIfNotNilDuration(&d.Timeout, l.Timeout)
	setIfNotNilString(&d.BindDN, l.BindDN)
	setIfNotNilString(&d.BindPassword, l.BindPassword)
	setIfNotNilString(&d.BaseDN, l.BaseDN)
	setIfNotNilString(&d.UserFilter, l.UserFilter)
	setIfNotNilBool(&d.NestedGroups, l.NestedGroups)
	setIfNotNilString(&d.UserDNTemplate, l.UserDNTemplate)
	setIfNotNilString(&d.UsernameAttr, l.UsernameAttr)
	setIfNotNilString(&d.DisplayNameAttr, l.DisplayNameAttr)
	setIfNotNilString(&d.UniqueIDAttr, l.UniqueIDAttr)
	setIfNotNilString(&d.RoleSource, l.RoleSource)
	setIfNotNilString(&d.DefaultRole, l.DefaultRole)
	mergeRoleMap(&d.RoleMap, l.RoleMap)
}

// mergeRoleMap overlays a file's role_map onto dst.
//
// The pointer discriminates nil from empty, and both cases are meaningful: a
// nil src (the key is absent from the file) leaves the default list untouched,
// while an explicit empty list ("role_map: []") clears it. Collapsing the two
// would make it impossible to turn off a role map that a lower layer set.
func mergeRoleMap(dst *[]RoleMappingConfig, src *[]roleMappingFile) {
	if src == nil {
		return
	}
	out := make([]RoleMappingConfig, 0, len(*src))
	for _, m := range *src {
		out = append(out, RoleMappingConfig(m))
	}
	*dst = out
}

// mergeAuthOIDCFile overlays the auth.oidc sub-fields from fc onto cfg. Split
// out of [mergeAuthFile] for cyclomatic complexity, mirroring
// [mergeAuthLDAPFile]. Unlike the sibling helpers it takes the whole
// fileConfig: its shadow struct has too many fields to restate as a parameter
// type.
func mergeAuthOIDCFile(cfg *Config, fc fileConfig) {
	if fc.Auth == nil || fc.Auth.OIDC == nil {
		return
	}
	o := fc.Auth.OIDC
	d := &cfg.Auth.OIDC
	setIfNotNilBool(&d.Enabled, o.Enabled)
	setIfNotNilString(&d.Issuer, o.Issuer)
	setIfNotNilString(&d.ClientID, o.ClientID)
	setIfNotNilString(&d.ClientSecret, o.ClientSecret)
	setIfNotNilString(&d.RedirectURL, o.RedirectURL)
	if o.Scopes != nil {
		d.Scopes = *o.Scopes
	}
	setIfNotNilString(&d.UsernameClaim, o.UsernameClaim)
	setIfNotNilString(&d.DisplayNameClaim, o.DisplayNameClaim)
	setIfNotNilString(&d.GroupsClaim, o.GroupsClaim)
	setIfNotNilString(&d.RoleSource, o.RoleSource)
	setIfNotNilString(&d.DefaultRole, o.DefaultRole)
	setIfNotNilString(&d.ReauthMode, o.ReauthMode)
	setIfNotNilString(&d.LogoutMode, o.LogoutMode)
	setIfNotNilString(&d.PostLogoutRedirectURL, o.PostLogoutRedirectURL)
	setIfNotNilString(&d.ButtonLabel, o.ButtonLabel)
	mergeRoleMap(&d.RoleMap, o.RoleMap)
}

// setIfNotNilString assigns *src to *dst when src is non-nil.
func setIfNotNilString(dst, src *string) {
	if src != nil {
		*dst = *src
	}
}

// setIfNotNilBool assigns *src to *dst when src is non-nil.
func setIfNotNilBool(dst, src *bool) {
	if src != nil {
		*dst = *src
	}
}

// fileDuration is a Go duration in the config file ("30s", "5m", "1h"). A value
// that does not parse ("5 sec", a bare "30") fails the load, naming its line,
// as the environment layer's [envvar.Duration] does; it never falls back to the
// default.
type fileDuration time.Duration

// UnmarshalYAML implements [yaml.Unmarshaler].
func (d *fileDuration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("line %d: invalid duration %q (want e.g. 30s, 5m, 1h)", n.Line, s)
	}
	*d = fileDuration(v)
	return nil
}

// setIfNotNilDuration assigns *src to *dst when src is non-nil.
func setIfNotNilDuration(dst *time.Duration, src *fileDuration) {
	if src != nil {
		*dst = time.Duration(*src)
	}
}

// ── Environment variable layer ────────────────────────────────────────────────

// applyEnv overlays SQI_* environment variables onto cfg. The typed [envvar]
// setters return an error when a non-empty value cannot be parsed so a
// malformed security-relevant value — e.g. SQI_AUTH_ENABLED=enabled
// — fails loud at load time instead of silently reverting to the default (a
// fail-open). Every violation is collected and returned together via
// [errors.Join]; an unset or empty var stays a no-op.
func applyEnv(cfg *Config) error {
	var errs []error
	collect := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	envvar.String(&cfg.HTTP.Addr, "SQI_HTTP_ADDR")
	collect(envvar.Bool(&cfg.HTTP.EnablePprof, "SQI_HTTP_ENABLE_PPROF"))
	collect(envvar.Bool(&cfg.HTTP.RateLimit.Enabled, "SQI_HTTP_RATE_LIMIT_ENABLED"))
	collect(envvar.Bool(&cfg.HTTP.TLS.Enabled, "SQI_HTTP_TLS_ENABLED"))
	envvar.String(&cfg.HTTP.TLS.CertFile, "SQI_HTTP_TLS_CERT_FILE")
	envvar.String(&cfg.HTTP.TLS.KeyFile, "SQI_HTTP_TLS_KEY_FILE")
	envvar.StringSlice(&cfg.HTTP.CORSOrigins, "SQI_HTTP_CORS_ORIGINS")

	collect(rejectRemovedNATSEnv())
	applyNATSEnv(cfg, collect)

	envvar.String(&cfg.Store.Driver, "SQI_STORE_DRIVER")
	envvar.String(&cfg.Store.SQLitePath, "SQI_STORE_SQLITE_PATH")
	collect(envvar.Duration(&cfg.Store.CheckpointInterval, "SQI_STORE_CHECKPOINT_INTERVAL"))
	envvar.String(&cfg.Store.Postgres.DSN, "SQI_STORE_POSTGRES_DSN")
	collect(envvar.Int(&cfg.Store.Postgres.MaxConns, "SQI_STORE_POSTGRES_MAX_CONNS"))

	envvar.String(&cfg.Log.Level, "SQI_LOG_LEVEL")
	envvar.String(&cfg.Log.Format, "SQI_LOG_FORMAT")
	envvar.String(&cfg.Log.File, "SQI_LOG_FILE")
	collect(envvar.Int(&cfg.Log.MaxSizeMB, "SQI_LOG_MAX_SIZE_MB"))
	collect(envvar.Int(&cfg.Log.MaxBackups, "SQI_LOG_MAX_BACKUPS"))

	collect(envvar.Duration(&cfg.Scheduler.HeartbeatTimeout, "SQI_SCHEDULER_HEARTBEAT_TIMEOUT"))
	collect(envvar.Duration(&cfg.Scheduler.TickInterval, "SQI_SCHEDULER_TICK_INTERVAL"))
	collect(envvar.Int(&cfg.Scheduler.MaxTasksPerWorker, "SQI_SCHEDULER_MAX_TASKS_PER_WORKER"))
	collect(envvar.Duration(&cfg.Scheduler.OfflineWorkerRetention, "SQI_SCHEDULER_OFFLINE_WORKER_RETENTION"))
	collect(envvar.Duration(&cfg.Scheduler.JobRetention, "SQI_SCHEDULER_JOB_RETENTION"))
	collect(envvar.Bool(&cfg.Scheduler.JobRetentionIncludeFailed, "SQI_SCHEDULER_JOB_RETENTION_INCLUDE_FAILED"))
	collect(envvar.Duration(&cfg.Scheduler.UnschedulableGrace, "SQI_SCHEDULER_UNSCHEDULABLE_GRACE"))
	collect(envvar.Duration(&cfg.Scheduler.ReconcileGrace, "SQI_SCHEDULER_RECONCILE_GRACE"))
	collect(envvar.Int(&cfg.Scheduler.DefaultMaxAttempts, "SQI_SCHEDULER_DEFAULT_MAX_ATTEMPTS"))
	collect(envvar.Duration(&cfg.Scheduler.RetryDelay, "SQI_SCHEDULER_RETRY_DELAY"))
	collect(envvar.Int(&cfg.Scheduler.DefaultFailureLimit, "SQI_SCHEDULER_DEFAULT_FAILURE_LIMIT"))

	collect(envvar.Bool(&cfg.Discovery.Enabled, "SQI_DISCOVERY_ENABLED"))
	envvar.String(&cfg.Discovery.InstanceName, "SQI_DISCOVERY_INSTANCE_NAME")
	collect(envvar.Bool(&cfg.HA.Enabled, "SQI_HA_ENABLED"))
	collect(envvar.Duration(&cfg.HA.LeaseTTL, "SQI_HA_LEASE_TTL"))
	envvar.String(&cfg.HA.InstanceName, "SQI_HA_INSTANCE_NAME")

	collect(envvar.Bool(&cfg.OpenJD.EnforceLimits, "SQI_OPENJD_ENFORCE_LIMITS"))
	collect(envvar.Int64(&cfg.OpenJD.ExprOperationLimit, "SQI_OPENJD_EXPR_OPERATION_LIMIT"))
	collect(envvar.Int64(&cfg.OpenJD.ExprMemoryLimit, "SQI_OPENJD_EXPR_MEMORY_LIMIT"))
	collect(envvar.Int64(&cfg.OpenJD.ExprTemplatePositions, "SQI_OPENJD_EXPR_TEMPLATE_POSITIONS"))
	collect(envvar.Int64(&cfg.OpenJD.ExprTemplateRetainedBytes, "SQI_OPENJD_EXPR_TEMPLATE_RETAINED_BYTES"))
	collect(envvar.Duration(&cfg.OpenJD.ExprSubmissionDeadline, "SQI_OPENJD_EXPR_SUBMISSION_DEADLINE"))

	collect(envvar.Int(&cfg.Diagnostics.BufferSize, "SQI_DIAGNOSTICS_BUFFER_SIZE"))

	envvar.String(&cfg.PresetLibrary.URL, "SQI_PRESET_LIBRARY_URL")

	collect(envvar.Bool(&cfg.Auth.Enabled, "SQI_AUTH_ENABLED"))
	collect(envvar.Bool(&cfg.Auth.ValidateJobOwner, "SQI_AUTH_VALIDATE_JOB_OWNER"))
	collect(envvar.Duration(&cfg.Auth.Session.TTL, "SQI_AUTH_SESSION_TTL"))
	envvar.String(&cfg.Auth.Session.CookieName, "SQI_AUTH_SESSION_COOKIE_NAME")
	envvar.String(&cfg.Auth.Session.CookieSecure, "SQI_AUTH_SESSION_COOKIE_SECURE")
	envvar.String(&cfg.Auth.Bootstrap.Username, "SQI_AUTH_BOOTSTRAP_USERNAME")
	envvar.String(&cfg.Auth.Bootstrap.Password, "SQI_AUTH_BOOTSTRAP_PASSWORD")
	collect(applyLDAPEnv(&cfg.Auth.LDAP))
	collect(applyOIDCEnv(&cfg.Auth.OIDC))

	return errors.Join(errs...)
}

// applyLDAPEnv overlays SQI_AUTH_LDAP_* onto cfg. Split out of [applyEnv] to
// keep its cyclomatic complexity under the lint threshold. role_map has no env
// form — a list of pairs has no sane flat encoding, so it is file-only.
func applyLDAPEnv(cfg *LDAPConfig) error {
	var errs []error
	collect := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	collect(envvar.Bool(&cfg.Enabled, "SQI_AUTH_LDAP_ENABLED"))
	envvar.String(&cfg.URL, "SQI_AUTH_LDAP_URL")
	collect(envvar.Bool(&cfg.StartTLS, "SQI_AUTH_LDAP_START_TLS"))
	collect(envvar.Bool(&cfg.TLSSkipVerify, "SQI_AUTH_LDAP_TLS_SKIP_VERIFY"))
	envvar.String(&cfg.CAFile, "SQI_AUTH_LDAP_CA_FILE")
	collect(envvar.Duration(&cfg.Timeout, "SQI_AUTH_LDAP_TIMEOUT"))
	envvar.String(&cfg.BindDN, "SQI_AUTH_LDAP_BIND_DN")
	envvar.String(&cfg.BindPassword, "SQI_AUTH_LDAP_BIND_PASSWORD")
	envvar.String(&cfg.BaseDN, "SQI_AUTH_LDAP_BASE_DN")
	envvar.String(&cfg.UserFilter, "SQI_AUTH_LDAP_USER_FILTER")
	collect(envvar.Bool(&cfg.NestedGroups, "SQI_AUTH_LDAP_NESTED_GROUPS"))
	envvar.String(&cfg.UserDNTemplate, "SQI_AUTH_LDAP_USER_DN_TEMPLATE")
	envvar.String(&cfg.UsernameAttr, "SQI_AUTH_LDAP_USERNAME_ATTR")
	envvar.String(&cfg.DisplayNameAttr, "SQI_AUTH_LDAP_DISPLAY_NAME_ATTR")
	envvar.String(&cfg.UniqueIDAttr, "SQI_AUTH_LDAP_UNIQUE_ID_ATTR")
	envvar.String(&cfg.RoleSource, "SQI_AUTH_LDAP_ROLE_SOURCE")
	envvar.String(&cfg.DefaultRole, "SQI_AUTH_LDAP_DEFAULT_ROLE")
	return errors.Join(errs...)
}

// applyOIDCEnv overlays SQI_AUTH_OIDC_* onto cfg. Split out of [applyEnv] to
// keep its cyclomatic complexity under the lint threshold. role_map has no
// env form — a list of pairs has no sane flat encoding, so it is file-only,
// exactly as auth.ldap.role_map is.
//
// Unlike [applyLDAPEnv] this block has exactly one fallible setter — every
// other field is a string or a string list — so the error is returned directly
// rather than collected.
func applyOIDCEnv(cfg *OIDCConfig) error {
	envvar.String(&cfg.Issuer, "SQI_AUTH_OIDC_ISSUER")
	envvar.String(&cfg.ClientID, "SQI_AUTH_OIDC_CLIENT_ID")
	envvar.String(&cfg.ClientSecret, "SQI_AUTH_OIDC_CLIENT_SECRET")
	envvar.String(&cfg.RedirectURL, "SQI_AUTH_OIDC_REDIRECT_URL")
	envvar.StringSlice(&cfg.Scopes, "SQI_AUTH_OIDC_SCOPES")
	envvar.String(&cfg.UsernameClaim, "SQI_AUTH_OIDC_USERNAME_CLAIM")
	envvar.String(&cfg.DisplayNameClaim, "SQI_AUTH_OIDC_DISPLAY_NAME_CLAIM")
	envvar.String(&cfg.GroupsClaim, "SQI_AUTH_OIDC_GROUPS_CLAIM")
	envvar.String(&cfg.RoleSource, "SQI_AUTH_OIDC_ROLE_SOURCE")
	envvar.String(&cfg.DefaultRole, "SQI_AUTH_OIDC_DEFAULT_ROLE")
	envvar.String(&cfg.ReauthMode, "SQI_AUTH_OIDC_REAUTH_MODE")
	envvar.String(&cfg.LogoutMode, "SQI_AUTH_OIDC_LOGOUT_MODE")
	envvar.String(&cfg.PostLogoutRedirectURL, "SQI_AUTH_OIDC_POST_LOGOUT_REDIRECT_URL")
	envvar.String(&cfg.ButtonLabel, "SQI_AUTH_OIDC_BUTTON_LABEL")
	return envvar.Bool(&cfg.Enabled, "SQI_AUTH_OIDC_ENABLED")
}

// applyNATSEnv overlays the SQI_NATS_* variables onto cfg.
func applyNATSEnv(cfg *Config, collect func(error)) {
	n := &cfg.NATS
	envvar.String(&n.Mode, "SQI_NATS_MODE")
	collect(envvar.Bool(&n.Auth.Enabled, "SQI_NATS_AUTH_ENABLED"))
	collect(envvar.Duration(&n.Auth.UserTTL, "SQI_NATS_AUTH_USER_TTL"))
	collect(envvar.Duration(&n.Auth.JoinTokenTTL, "SQI_NATS_AUTH_JOIN_TOKEN_TTL"))
	collect(envvar.Bool(&n.Auth.JoinTokenSingleUse, "SQI_NATS_AUTH_JOIN_TOKEN_SINGLE_USE"))
	collect(envvar.Bool(&n.Auth.EnrollmentEndpointEnabled, "SQI_NATS_AUTH_ENROLLMENT_ENDPOINT_ENABLED"))

	em := &n.Embedded
	envvar.String(&em.Addr, "SQI_NATS_EMBEDDED_ADDR")
	envvar.String(&em.AdvertiseURL, "SQI_NATS_EMBEDDED_ADVERTISE_URL")
	envvar.String(&em.DataDir, "SQI_NATS_EMBEDDED_DATA_DIR")
	collect(envvar.Int(&em.MaxStoreMB, "SQI_NATS_EMBEDDED_MAX_STORE_MB"))
	collect(envvar.Bool(&em.TLS.Enabled, "SQI_NATS_EMBEDDED_TLS_ENABLED"))
	envvar.String(&em.TLS.CertFile, "SQI_NATS_EMBEDDED_TLS_CERT_FILE")
	envvar.String(&em.TLS.KeyFile, "SQI_NATS_EMBEDDED_TLS_KEY_FILE")
	envvar.String(&em.TLS.ClientCAFile, "SQI_NATS_EMBEDDED_TLS_CLIENT_CA_FILE")

	ex := &n.External
	envvar.StringSlice(&ex.URLs, "SQI_NATS_EXTERNAL_URLS")
	envvar.StringSlice(&ex.AdvertiseURLs, "SQI_NATS_EXTERNAL_ADVERTISE_URLS")
	envvar.String(&ex.Account, "SQI_NATS_EXTERNAL_ACCOUNT")
	envvar.String(&ex.NkeySeedFile, "SQI_NATS_EXTERNAL_NKEY_SEED_FILE")
	envvar.String(&ex.CredsFile, "SQI_NATS_EXTERNAL_CREDS_FILE")
	collect(envvar.Int(&ex.StreamReplicas, "SQI_NATS_EXTERNAL_STREAM_REPLICAS"))
	collect(envvar.Duration(&ex.ConnectTimeout, "SQI_NATS_EXTERNAL_CONNECT_TIMEOUT"))
	envvar.String(&ex.TLS.CAFile, "SQI_NATS_EXTERNAL_TLS_CA_FILE")
	envvar.String(&ex.TLS.CertFile, "SQI_NATS_EXTERNAL_TLS_CERT_FILE")
	envvar.String(&ex.TLS.KeyFile, "SQI_NATS_EXTERNAL_TLS_KEY_FILE")
	envvar.String(&ex.TLS.ServerName, "SQI_NATS_EXTERNAL_TLS_SERVER_NAME")
	envvar.String(&ex.Callout.NkeySeedFile, "SQI_NATS_EXTERNAL_CALLOUT_NKEY_SEED_FILE")
	envvar.String(&ex.Callout.IssuerSeedFile, "SQI_NATS_EXTERNAL_CALLOUT_ISSUER_SEED_FILE")
	envvar.String(&ex.Callout.SystemCredsFile, "SQI_NATS_EXTERNAL_CALLOUT_SYSTEM_CREDS_FILE")
}

// removedNATSKeys are the nats.* keys that moved under nats.embedded.
var removedNATSKeys = []string{"addr", "data_dir", "max_store_mb", "tls"}

// rejectRemovedNATSKeys fails a config file that still uses a nats.* key that
// moved under nats.embedded. The strict decode would reject them as unknown
// keys too, but without naming where they went.
func rejectRemovedNATSKeys(path string, data []byte) error {
	root := yamlstrict.Root(data)
	if root == nil || root.Kind != yaml.MappingNode {
		return nil
	}
	var errs []error
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "nats" || root.Content[i+1].Kind != yaml.MappingNode {
			continue
		}
		nats := root.Content[i+1]
		for j := 0; j+1 < len(nats.Content); j += 2 {
			k := nats.Content[j]
			if slices.Contains(removedNATSKeys, k.Value) {
				errs = append(errs, fmt.Errorf(
					"config file %q line %d: nats.%s was removed; use nats.embedded.%s",
					path, k.Line, k.Value, k.Value,
				))
			}
		}
	}
	return errors.Join(errs...)
}

// rejectRemovedNATSEnv fails when an SQI_NATS_* variable that moved under
// SQI_NATS_EMBEDDED_* is set. A stale unit file or Compose file would
// otherwise run silently on the defaults.
func rejectRemovedNATSEnv() error {
	var errs []error
	for _, kv := range os.Environ() {
		name, value, _ := strings.Cut(kv, "=")
		if value == "" || !isRemovedNATSEnv(name) {
			continue
		}
		errs = append(errs, fmt.Errorf("environment variable %s was removed; use SQI_NATS_EMBEDDED_%s",
			name, strings.TrimPrefix(name, "SQI_NATS_")))
	}
	return errors.Join(errs...)
}

func isRemovedNATSEnv(name string) bool {
	switch name {
	case "SQI_NATS_ADDR", "SQI_NATS_DATA_DIR", "SQI_NATS_MAX_STORE_MB":
		return true
	}
	return strings.HasPrefix(name, "SQI_NATS_TLS_")
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
	if len(f.HTTPCORSOrigins) > 0 {
		cfg.HTTP.CORSOrigins = f.HTTPCORSOrigins
	}
	if f.EnforceLimits != nil {
		cfg.OpenJD.EnforceLimits = *f.EnforceLimits
	}
	if f.AuthEnabled != nil {
		cfg.Auth.Enabled = *f.AuthEnabled
	}
	if f.ValidateJobOwner != nil {
		cfg.Auth.ValidateJobOwner = *f.ValidateJobOwner
	}
}

// CheckFileKeys reports every key in a config file's data that sqi-server
// would not accept: one it does not know, or one holding a value of the wrong
// shape (a section where a value belongs, or the reverse). Each message names
// the key by its dotted path. It decodes the file as the loader does and
// merges nothing, so it does not read the environment or validate values.
func CheckFileKeys(data []byte) error {
	if err := rejectRemovedNATSKeys("config", data); err != nil {
		return err
	}
	var fc fileConfig
	return yamlstrict.Decode(data, &fc)
}
