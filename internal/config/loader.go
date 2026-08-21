// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"errors"
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
	cfg := DefaultConfig()

	// ── Layer 2: config file ──────────────────────────────────────────────
	if err := applyFile(&cfg, filePath); err != nil {
		return Config{}, err
	}

	// ── Layer 3: environment variables ───────────────────────────────────
	if err := applyEnv(&cfg); err != nil {
		return Config{}, err
	}

	// ── Layer 4: CLI flag overrides ───────────────────────────────────────
	applyFlags(&cfg, flags)

	return cfg, nil
}

// ── File layer ────────────────────────────────────────────────────────────────

// roleMappingFile is one auth.*.role_map entry as it appears in the file. Both
// provider blocks use it, so the shape is stated once.
type roleMappingFile struct {
	Group string `yaml:"group"`
	Role  string `yaml:"role"`
}

// fileConfig is the partial YAML/JSON shape used when unmarshaling a config
// file. Every field uses a pointer so we can distinguish "not set" from a
// zero value, applying only the fields that are present in the file.
type fileConfig struct {
	HTTP *struct {
		Addr        *string   `yaml:"addr"`
		EnablePprof *bool     `yaml:"enable_pprof"`
		CORSOrigins *[]string `yaml:"cors_origins"`
	} `yaml:"http"`

	NATS *struct {
		Addr       *string `yaml:"addr"`
		DataDir    *string `yaml:"data_dir"`
		MaxStoreMB *int    `yaml:"max_store_mb"`
		Auth       *struct {
			Enabled                   *bool   `yaml:"enabled"`
			JoinTokenTTL              *string `yaml:"join_token_ttl"`
			JoinTokenSingleUse        *bool   `yaml:"join_token_single_use"`
			EnrollmentEndpointEnabled *bool   `yaml:"enrollment_endpoint_enabled"`
		} `yaml:"auth"`
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
		UnschedulableGrace        *string `yaml:"unschedulable_grace"`
		DefaultMaxAttempts        *int    `yaml:"default_max_attempts"`
		RetryDelay                *string `yaml:"retry_delay"`
		DefaultFailureLimit       *int    `yaml:"default_failure_limit"`
	} `yaml:"scheduler"`

	Discovery *struct {
		Enabled      *bool   `yaml:"enabled"`
		InstanceName *string `yaml:"instance_name"`
	} `yaml:"discovery"`

	OpenJD *struct {
		EnforceLimits             *bool   `yaml:"enforce_limits"`
		ExprOperationLimit        *int64  `yaml:"expr_operation_limit"`
		ExprMemoryLimit           *int64  `yaml:"expr_memory_limit"`
		ExprTemplatePositions     *int64  `yaml:"expr_template_positions"`
		ExprTemplateRetainedBytes *int64  `yaml:"expr_template_retained_bytes"`
		ExprSubmissionDeadline    *string `yaml:"expr_submission_deadline"`
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
			TTL          *string `yaml:"ttl"`
			CookieName   *string `yaml:"cookie_name"`
			CookieSecure *string `yaml:"cookie_secure"`
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
			Timeout         *string            `yaml:"timeout"`
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
	mergeNATSAuthFile(cfg, fc.NATS.Auth)
}

// mergeNATSAuthFile overlays the nats.auth sub-fields from fc onto cfg. Split
// out of [mergeNATSFile] to keep its cyclomatic complexity under the lint
// threshold.
func mergeNATSAuthFile(cfg *Config, a *struct {
	Enabled                   *bool   `yaml:"enabled"`
	JoinTokenTTL              *string `yaml:"join_token_ttl"`
	JoinTokenSingleUse        *bool   `yaml:"join_token_single_use"`
	EnrollmentEndpointEnabled *bool   `yaml:"enrollment_endpoint_enabled"`
},
) {
	if a == nil {
		return
	}
	if a.Enabled != nil {
		cfg.NATS.Auth.Enabled = *a.Enabled
	}
	if a.JoinTokenTTL != nil {
		if d, err := time.ParseDuration(*a.JoinTokenTTL); err == nil {
			cfg.NATS.Auth.JoinTokenTTL = d
		}
	}
	if a.JoinTokenSingleUse != nil {
		cfg.NATS.Auth.JoinTokenSingleUse = *a.JoinTokenSingleUse
	}
	if a.EnrollmentEndpointEnabled != nil {
		cfg.NATS.Auth.EnrollmentEndpointEnabled = *a.EnrollmentEndpointEnabled
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
	if fc.Scheduler.UnschedulableGrace != nil {
		if d, err := time.ParseDuration(*fc.Scheduler.UnschedulableGrace); err == nil {
			cfg.Scheduler.UnschedulableGrace = d
		}
	}
	mergeSchedulerRetryFile(cfg, fc)
}

// mergeSchedulerRetryFile overlays the server-level retry-policy fields
// (default_max_attempts, retry_delay, default_failure_limit) from fc onto
// cfg. Split out of [mergeSchedulerFile] to keep its cyclomatic complexity
// under the lint threshold.
func mergeSchedulerRetryFile(cfg *Config, fc fileConfig) {
	if fc.Scheduler.DefaultMaxAttempts != nil {
		cfg.Scheduler.DefaultMaxAttempts = *fc.Scheduler.DefaultMaxAttempts
	}
	if fc.Scheduler.RetryDelay != nil {
		if d, err := time.ParseDuration(*fc.Scheduler.RetryDelay); err == nil {
			cfg.Scheduler.RetryDelay = d
		}
	}
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
	// A malformed duration leaves the default in place, matching every other
	// duration this loader reads from a file (see mergeSchedulerFile). The
	// default is in range, so an unparseable value cannot turn into a startup
	// failure here -- the env layer, which reports its own parse errors, is
	// where a typo is caught.
	//
	// This is the only one of the ten EXPR keys where that is reachable: the
	// other nine are ints, so a typo fails YAML unmarshalling loudly. Kept for
	// consistency with the loader's other durations rather than made an error
	// here, and documented as an operator-visible quirk in docs/configuration.md
	// under openjd.expr_submission_deadline -- changing it means changing every
	// duration key at once, not this one.
	if fc.OpenJD.ExprSubmissionDeadline != nil {
		if d, err := time.ParseDuration(*fc.OpenJD.ExprSubmissionDeadline); err == nil {
			cfg.OpenJD.ExprSubmissionDeadline = d
		}
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
	TTL          *string `yaml:"ttl"`
	CookieName   *string `yaml:"cookie_name"`
	CookieSecure *string `yaml:"cookie_secure"`
},
) {
	if s == nil {
		return
	}
	if s.TTL != nil {
		if d, err := time.ParseDuration(*s.TTL); err == nil {
			cfg.Auth.Session.TTL = d
		}
	}
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
	if l.Timeout != nil {
		if v, err := time.ParseDuration(*l.Timeout); err == nil {
			d.Timeout = v
		}
	}
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

// ── Environment variable layer ────────────────────────────────────────────────

// applyEnv overlays SQI_* environment variables onto cfg. Typed helpers
// (setBool/setInt/setDuration) return an error when a non-empty value cannot be
// parsed so a malformed security-relevant value — e.g. SQI_AUTH_ENABLED=enabled
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

	setString(&cfg.HTTP.Addr, "SQI_HTTP_ADDR")
	collect(setBool(&cfg.HTTP.EnablePprof, "SQI_HTTP_ENABLE_PPROF"))
	setStringSlice(&cfg.HTTP.CORSOrigins, "SQI_HTTP_CORS_ORIGINS")

	setString(&cfg.NATS.Addr, "SQI_NATS_ADDR")
	setString(&cfg.NATS.DataDir, "SQI_NATS_DATA_DIR")
	collect(setInt(&cfg.NATS.MaxStoreMB, "SQI_NATS_MAX_STORE_MB"))
	collect(setBool(&cfg.NATS.Auth.Enabled, "SQI_NATS_AUTH_ENABLED"))
	collect(setDuration(&cfg.NATS.Auth.JoinTokenTTL, "SQI_NATS_AUTH_JOIN_TOKEN_TTL"))
	collect(setBool(&cfg.NATS.Auth.JoinTokenSingleUse, "SQI_NATS_AUTH_JOIN_TOKEN_SINGLE_USE"))
	collect(setBool(&cfg.NATS.Auth.EnrollmentEndpointEnabled, "SQI_NATS_AUTH_ENROLLMENT_ENDPOINT_ENABLED"))

	setString(&cfg.Store.SQLitePath, "SQI_STORE_SQLITE_PATH")
	collect(setDuration(&cfg.Store.CheckpointInterval, "SQI_STORE_CHECKPOINT_INTERVAL"))

	setString(&cfg.Log.Level, "SQI_LOG_LEVEL")
	setString(&cfg.Log.Format, "SQI_LOG_FORMAT")

	collect(setDuration(&cfg.Scheduler.HeartbeatTimeout, "SQI_SCHEDULER_HEARTBEAT_TIMEOUT"))
	collect(setDuration(&cfg.Scheduler.TickInterval, "SQI_SCHEDULER_TICK_INTERVAL"))
	collect(setInt(&cfg.Scheduler.MaxTasksPerWorker, "SQI_SCHEDULER_MAX_TASKS_PER_WORKER"))
	collect(setDuration(&cfg.Scheduler.OfflineWorkerRetention, "SQI_SCHEDULER_OFFLINE_WORKER_RETENTION"))
	collect(setDuration(&cfg.Scheduler.JobRetention, "SQI_SCHEDULER_JOB_RETENTION"))
	collect(setBool(&cfg.Scheduler.JobRetentionIncludeFailed, "SQI_SCHEDULER_JOB_RETENTION_INCLUDE_FAILED"))
	collect(setDuration(&cfg.Scheduler.UnschedulableGrace, "SQI_SCHEDULER_UNSCHEDULABLE_GRACE"))
	collect(setInt(&cfg.Scheduler.DefaultMaxAttempts, "SQI_SCHEDULER_DEFAULT_MAX_ATTEMPTS"))
	collect(setDuration(&cfg.Scheduler.RetryDelay, "SQI_SCHEDULER_RETRY_DELAY"))
	collect(setInt(&cfg.Scheduler.DefaultFailureLimit, "SQI_SCHEDULER_DEFAULT_FAILURE_LIMIT"))

	collect(setBool(&cfg.Discovery.Enabled, "SQI_DISCOVERY_ENABLED"))
	setString(&cfg.Discovery.InstanceName, "SQI_DISCOVERY_INSTANCE_NAME")

	collect(setBool(&cfg.OpenJD.EnforceLimits, "SQI_OPENJD_ENFORCE_LIMITS"))
	collect(setInt64(&cfg.OpenJD.ExprOperationLimit, "SQI_OPENJD_EXPR_OPERATION_LIMIT"))
	collect(setInt64(&cfg.OpenJD.ExprMemoryLimit, "SQI_OPENJD_EXPR_MEMORY_LIMIT"))
	collect(setInt64(&cfg.OpenJD.ExprTemplatePositions, "SQI_OPENJD_EXPR_TEMPLATE_POSITIONS"))
	collect(setInt64(&cfg.OpenJD.ExprTemplateRetainedBytes, "SQI_OPENJD_EXPR_TEMPLATE_RETAINED_BYTES"))
	collect(setDuration(&cfg.OpenJD.ExprSubmissionDeadline, "SQI_OPENJD_EXPR_SUBMISSION_DEADLINE"))

	collect(setInt(&cfg.Diagnostics.BufferSize, "SQI_DIAGNOSTICS_BUFFER_SIZE"))

	setString(&cfg.PresetLibrary.URL, "SQI_PRESET_LIBRARY_URL")

	collect(setBool(&cfg.Auth.Enabled, "SQI_AUTH_ENABLED"))
	collect(setBool(&cfg.Auth.ValidateJobOwner, "SQI_AUTH_VALIDATE_JOB_OWNER"))
	collect(setDuration(&cfg.Auth.Session.TTL, "SQI_AUTH_SESSION_TTL"))
	setString(&cfg.Auth.Session.CookieName, "SQI_AUTH_SESSION_COOKIE_NAME")
	setString(&cfg.Auth.Session.CookieSecure, "SQI_AUTH_SESSION_COOKIE_SECURE")
	setString(&cfg.Auth.Bootstrap.Username, "SQI_AUTH_BOOTSTRAP_USERNAME")
	setString(&cfg.Auth.Bootstrap.Password, "SQI_AUTH_BOOTSTRAP_PASSWORD")
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
	collect(setBool(&cfg.Enabled, "SQI_AUTH_LDAP_ENABLED"))
	setString(&cfg.URL, "SQI_AUTH_LDAP_URL")
	collect(setBool(&cfg.StartTLS, "SQI_AUTH_LDAP_START_TLS"))
	collect(setBool(&cfg.TLSSkipVerify, "SQI_AUTH_LDAP_TLS_SKIP_VERIFY"))
	setString(&cfg.CAFile, "SQI_AUTH_LDAP_CA_FILE")
	collect(setDuration(&cfg.Timeout, "SQI_AUTH_LDAP_TIMEOUT"))
	setString(&cfg.BindDN, "SQI_AUTH_LDAP_BIND_DN")
	setString(&cfg.BindPassword, "SQI_AUTH_LDAP_BIND_PASSWORD")
	setString(&cfg.BaseDN, "SQI_AUTH_LDAP_BASE_DN")
	setString(&cfg.UserFilter, "SQI_AUTH_LDAP_USER_FILTER")
	collect(setBool(&cfg.NestedGroups, "SQI_AUTH_LDAP_NESTED_GROUPS"))
	setString(&cfg.UserDNTemplate, "SQI_AUTH_LDAP_USER_DN_TEMPLATE")
	setString(&cfg.UsernameAttr, "SQI_AUTH_LDAP_USERNAME_ATTR")
	setString(&cfg.DisplayNameAttr, "SQI_AUTH_LDAP_DISPLAY_NAME_ATTR")
	setString(&cfg.UniqueIDAttr, "SQI_AUTH_LDAP_UNIQUE_ID_ATTR")
	setString(&cfg.RoleSource, "SQI_AUTH_LDAP_ROLE_SOURCE")
	setString(&cfg.DefaultRole, "SQI_AUTH_LDAP_DEFAULT_ROLE")
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
	setString(&cfg.Issuer, "SQI_AUTH_OIDC_ISSUER")
	setString(&cfg.ClientID, "SQI_AUTH_OIDC_CLIENT_ID")
	setString(&cfg.ClientSecret, "SQI_AUTH_OIDC_CLIENT_SECRET")
	setString(&cfg.RedirectURL, "SQI_AUTH_OIDC_REDIRECT_URL")
	setStringSlice(&cfg.Scopes, "SQI_AUTH_OIDC_SCOPES")
	setString(&cfg.UsernameClaim, "SQI_AUTH_OIDC_USERNAME_CLAIM")
	setString(&cfg.DisplayNameClaim, "SQI_AUTH_OIDC_DISPLAY_NAME_CLAIM")
	setString(&cfg.GroupsClaim, "SQI_AUTH_OIDC_GROUPS_CLAIM")
	setString(&cfg.RoleSource, "SQI_AUTH_OIDC_ROLE_SOURCE")
	setString(&cfg.DefaultRole, "SQI_AUTH_OIDC_DEFAULT_ROLE")
	setString(&cfg.ReauthMode, "SQI_AUTH_OIDC_REAUTH_MODE")
	setString(&cfg.LogoutMode, "SQI_AUTH_OIDC_LOGOUT_MODE")
	setString(&cfg.PostLogoutRedirectURL, "SQI_AUTH_OIDC_POST_LOGOUT_REDIRECT_URL")
	setString(&cfg.ButtonLabel, "SQI_AUTH_OIDC_BUTTON_LABEL")
	return setBool(&cfg.Enabled, "SQI_AUTH_OIDC_ENABLED")
}

func setString(dst *string, key string) {
	if v := os.Getenv(key); v != "" {
		*dst = v
	}
}

// setStringSlice sets dst from a comma-separated env var, trimming spaces and
// dropping empty entries. An unset or empty var leaves dst untouched so a
// lower config layer survives.
func setStringSlice(dst *[]string, key string) {
	v := os.Getenv(key)
	if v == "" {
		return
	}
	var out []string
	for part := range strings.SplitSeq(v, ",") {
		if s := strings.TrimSpace(part); s != "" {
			out = append(out, s)
		}
	}
	if len(out) > 0 {
		*dst = out
	}
}

// setInt sets dst from an integer-valued env var. Surrounding whitespace is
// trimmed; an unset or (post-trim) empty var is a no-op that leaves the lower
// config layer intact. A non-empty value that does not parse is a hard error
// naming the key and the offending value.
func setInt(dst *int, key string) error {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("%s: invalid integer %q", key, v)
	}
	*dst = n
	return nil
}

// setInt64 is [setInt] for an int64-valued setting. Separate rather than
// generic because the two are used through pointers to different types and the
// error message must still name the key and the offending value identically.
func setInt64(dst *int64, key string) error {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fmt.Errorf("%s: invalid integer %q", key, v)
	}
	*dst = n
	return nil
}

// setDuration sets dst from a Go duration env var (e.g. "30s", "5m", "1h").
// Trimming and error semantics match [setInt].
func setDuration(dst *time.Duration, key string) error {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("%s: invalid duration %q (want e.g. 30s, 5m, 1h)", key, v)
	}
	*dst = d
	return nil
}

// setBool sets dst from a boolean env var. Accepted tokens (case-insensitive)
// are 1/true/yes/on and 0/false/no/off. Trimming and error semantics match
// [setInt] — critically, a malformed security-relevant flag such as
// SQI_AUTH_ENABLED=enabled errors here rather than silently leaving auth off.
func setBool(dst *bool, key string) error {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return nil
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		*dst = true
	case "0", "false", "no", "off":
		*dst = false
	default:
		return fmt.Errorf("%s: invalid boolean %q (want 1/true/yes/on or 0/false/no/off)", key, v)
	}
	return nil
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
