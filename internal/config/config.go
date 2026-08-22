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

import (
	"time"

	"github.com/uberware/sqi/internal/auth/oidc"
)

// Config is the complete runtime configuration for sqi-server.
// Zero values are not valid; use [DefaultConfig] or [Load] to obtain a
// populated instance.
type Config struct {
	HTTP          HTTPConfig          `yaml:"http"`
	NATS          NATSConfig          `yaml:"nats"`
	Store         StoreConfig         `yaml:"store"`
	Log           LogConfig           `yaml:"log"`
	Scheduler     SchedulerConfig     `yaml:"scheduler"`
	Discovery     DiscoveryConfig     `yaml:"discovery"`
	OpenJD        OpenJDConfig        `yaml:"openjd"`
	Diagnostics   DiagnosticsConfig   `yaml:"diagnostics"`
	PresetLibrary PresetLibraryConfig `yaml:"preset_library"`
	Auth          AuthConfig          `yaml:"auth"`
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

	// CORSOrigins is the list of browser origins the CORS middleware allows.
	// Empty means "*" (the router's default). With auth enabled a wildcard is
	// dropped at startup — credentials cannot be combined with "*" — so a
	// separately-hosted UI must name its origin explicitly here.
	// Env: SQI_HTTP_CORS_ORIGINS (comma-separated)
	CORSOrigins []string `yaml:"cors_origins"`

	// TLS terminates HTTPS in-process on this listener. Off by default.
	TLS TLSConfig `yaml:"tls"`
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

	// Auth configures per-worker broker authentication. Off by default.
	// Deliberately INDEPENDENT of the top-level auth block: the two protect
	// different surfaces, and coupling them would force an operator who
	// wants worker authentication into user accounts they did not ask for.
	Auth NATSAuthConfig `yaml:"auth"`

	// TLS encrypts the embedded broker's client listener. Off by default.
	TLS NATSTLSConfig `yaml:"tls"`
}

// TLSConfig controls in-process TLS termination for a listener. Zero value
// leaves TLS off, which is the default and must stay byte-for-byte
// equivalent to how the listener behaved before TLS existed.
type TLSConfig struct {
	// Enabled turns the listener into an HTTPS listener. There is no
	// plaintext port when it is on: the listener upgrades in place.
	// Env: SQI_HTTP_TLS_ENABLED
	Enabled bool `yaml:"enabled"`

	// CertFile is the PEM certificate (leaf first, then any intermediates).
	// Env: SQI_HTTP_TLS_CERT_FILE
	CertFile string `yaml:"cert_file"`

	// KeyFile is the PEM private key matching CertFile.
	// Env: SQI_HTTP_TLS_KEY_FILE
	KeyFile string `yaml:"key_file"`
}

// NATSTLSConfig controls TLS on the embedded broker. It is deliberately
// separate from the http.tls block: the two protect different surfaces and
// an operator may legitimately use a publicly-issued certificate on the API
// and a private farm CA on the broker.
type NATSTLSConfig struct {
	// Enabled requires TLS on every broker connection.
	// Env: SQI_NATS_TLS_ENABLED
	Enabled bool `yaml:"enabled"`

	// CertFile is the PEM certificate presented to connecting workers.
	// Env: SQI_NATS_TLS_CERT_FILE
	CertFile string `yaml:"cert_file"`

	// KeyFile is the PEM private key matching CertFile.
	// Env: SQI_NATS_TLS_KEY_FILE
	KeyFile string `yaml:"key_file"`

	// ClientCAFile, when set, requires every worker to present a client
	// certificate signed by this CA. This is a TRANSPORT control layered on
	// top of the per-worker nkey credential, not a replacement for it: the
	// certificate gates who may open a connection, the nkey decides which
	// worker you are.
	// Env: SQI_NATS_TLS_CLIENT_CA_FILE
	ClientCAFile string `yaml:"client_ca_file"`
}

// NATSAuthConfig configures broker authentication and worker enrollment.
type NATSAuthConfig struct {
	// Enabled requires every NATS client to present a per-worker nkey
	// credential. When false the broker accepts any connection, which is the
	// v0.3.0 behavior and the default.
	// Env: SQI_NATS_AUTH_ENABLED
	Enabled bool `yaml:"enabled"`

	// JoinTokenTTL is how long a newly issued worker join token remains
	// valid. Bounded by MinNATSAuthJoinTokenTTL and MaxNATSAuthJoinTokenTTL.
	// Env: SQI_NATS_AUTH_JOIN_TOKEN_TTL
	JoinTokenTTL time.Duration `yaml:"join_token_ttl"`

	// JoinTokenSingleUse consumes a join token on first successful
	// enrollment. Leaving it true is strongly recommended; false exists for
	// image-baked fleets that enroll many identical machines from one token.
	// Env: SQI_NATS_AUTH_JOIN_TOKEN_SINGLE_USE
	JoinTokenSingleUse bool `yaml:"join_token_single_use"`

	// EnrollmentEndpointEnabled mounts POST /api/v1/workers/enroll. Set it
	// false at a site that provisions every credential by hand and wants no
	// enrollment surface at all. Meaningful only when Enabled is true.
	// Env: SQI_NATS_AUTH_ENROLLMENT_ENDPOINT_ENABLED
	EnrollmentEndpointEnabled bool `yaml:"enrollment_endpoint_enabled"`
}

const (
	// MinNATSAuthJoinTokenTTL is the floor: below a minute an operator
	// cannot realistically get the token onto a machine and boot it.
	MinNATSAuthJoinTokenTTL = 1 * time.Minute

	// MaxNATSAuthJoinTokenTTL is the ceiling. A join token mints a worker
	// credential, so it is a bootstrap secret whose whole value is a short
	// blast radius; a token valid for weeks is a standing credential wearing
	// a different name.
	MaxNATSAuthJoinTokenTTL = 24 * time.Hour
)

// StoreConfig controls the embedded SQLite state store.
type StoreConfig struct {
	// SQLitePath is the path to the SQLite database file.
	// Created at startup if it does not exist.
	// Env: SQI_STORE_SQLITE_PATH
	SQLitePath string `yaml:"sqlite_path"`

	// CheckpointInterval is how often the background goroutine runs a WAL
	// checkpoint (PRAGMA wal_checkpoint(PASSIVE)) to fold committed WAL frames
	// back into the main database file and keep the WAL from growing unboundedly.
	// The periodic checkpoint is PASSIVE so it never waits on a reader; the
	// TRUNCATE checkpoint runs only on clean shutdown.
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

	// OfflineWorkerRetention is how long a worker may stay offline before the
	// retention sweep hard-deletes it, bounding worker-table growth on farms
	// with ephemeral nodes. Disabled and online workers are never auto-removed.
	// Set to 0 to disable automatic removal. Default: 24h.
	// Env: SQI_SCHEDULER_OFFLINE_WORKER_RETENTION
	OfflineWorkerRetention time.Duration `yaml:"offline_worker_retention"`

	// JobRetention is how long a terminal job is kept before the retention
	// sweep hard-deletes it and all of its data. completed and canceled jobs
	// are always eligible; failed jobs only when JobRetentionIncludeFailed is
	// set. Set to 0 to disable automatic deletion. Default: 168h (7 days).
	// Env: SQI_SCHEDULER_JOB_RETENTION
	JobRetention time.Duration `yaml:"job_retention"`

	// JobRetentionIncludeFailed extends the retention sweep to failed jobs.
	// Default: false. Env: SQI_SCHEDULER_JOB_RETENTION_INCLUDE_FAILED
	JobRetentionIncludeFailed bool `yaml:"job_retention_include_failed"`

	// UnschedulableGrace is how long a ready task may wait with no eligible
	// online worker before it is flagged unschedulable. 0 disables the sweep.
	// Default: 30s. Env: SQI_SCHEDULER_UNSCHEDULABLE_GRACE
	UnschedulableGrace time.Duration `yaml:"unschedulable_grace"`

	// DefaultMaxAttempts is the server-level fallback for the farm-wide default
	// number of attempts a task may make before going terminal-failed. It is the
	// bottom tier of the layered retry policy (Server -> Farm -> Queue -> Job).
	// Must be >= 1; 1 disables auto-retry. Default: 3.
	// Env: SQI_SCHEDULER_DEFAULT_MAX_ATTEMPTS
	DefaultMaxAttempts int `yaml:"default_max_attempts"`

	// RetryDelay is the server-level fallback default backoff before a failed
	// task re-enters the ready queue. 0 = immediate. Default: 30s.
	// Env: SQI_SCHEDULER_RETRY_DELAY
	RetryDelay time.Duration `yaml:"retry_delay"`

	// DefaultFailureLimit is the server-level fallback default job-level
	// failure ceiling. 0 = off (no auto-park). Default: 0.
	// Env: SQI_SCHEDULER_DEFAULT_FAILURE_LIMIT
	DefaultFailureLimit int `yaml:"default_failure_limit"`
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

// AuthConfig controls the opt-in authentication gate (Phase 3).
type AuthConfig struct {
	// Enabled turns on the authentication gate. Default false — the server is
	// open on a trusted local network.
	// Env: SQI_AUTH_ENABLED
	Enabled bool `yaml:"enabled"`

	// ValidateJobOwner rejects a submission whose Owner names no known user.
	// Default true — it keeps Job.Owner a trustworthy key for owner-scoped
	// visibility: the user role's job listings are filtered by owner, so a
	// typo'd owner yields a job invisible to the person who actually owns it
	// and missed by an admin filtering on them. It would likewise be the key
	// for any future per-user concurrency cap (deferred, see Wave D of the
	// Phase 3 tracker). Disable it when owners come from a directory that has
	// not yet provisioned local records.
	// Env: SQI_AUTH_VALIDATE_JOB_OWNER
	ValidateJobOwner bool `yaml:"validate_job_owner"`

	// Session controls server-side session cookie behavior.
	Session SessionConfig `yaml:"session"`

	// Bootstrap seeds the first admin account when auth is enabled and the
	// users table is empty.
	Bootstrap BootstrapConfig `yaml:"bootstrap"`

	// LDAP configures directory authentication. Disabled by default; when
	// enabled it requires Auth.Enabled — LDAP without the auth gate would
	// authenticate nobody.
	LDAP LDAPConfig `yaml:"ldap"`

	// OIDC configures OAuth2/OIDC single sign-on. Disabled by default; when
	// enabled it requires Auth.Enabled — SSO without the auth gate would
	// authenticate nobody.
	OIDC OIDCConfig `yaml:"oidc"`
}

// SessionConfig controls server-side session cookies.
type SessionConfig struct {
	// TTL is the absolute session lifetime; there is no sliding/idle renewal.
	// Env: SQI_AUTH_SESSION_TTL
	TTL time.Duration `yaml:"ttl"`

	// CookieName is the session cookie name. Must match
	// session.DefaultCookieName in internal/auth/session.
	// Env: SQI_AUTH_SESSION_COOKIE_NAME
	CookieName string `yaml:"cookie_name"`

	// CookieSecure controls the cookie's Secure attribute. One of:
	//   - "auto" (default): Secure when the request arrived over TLS or with
	//     X-Forwarded-Proto: https.
	//   - "true": always Secure.
	//   - "false": never Secure — for plain-HTTP LAN deployments.
	// Env: SQI_AUTH_SESSION_COOKIE_SECURE
	CookieSecure string `yaml:"cookie_secure"`
}

// BootstrapConfig seeds the first admin account when auth is enabled and no
// users exist yet. Empty is valid: the server warns and boots without
// creating an admin rather than failing closed.
type BootstrapConfig struct {
	// Username for the seeded first admin. Env: SQI_AUTH_BOOTSTRAP_USERNAME
	Username string `yaml:"username"`
	// Password for the seeded first admin. Redacted by [BootstrapConfig.MarshalYAML]
	// whenever the config is re-marshaled (e.g. `sqi-server config print`), so it
	// never appears in that output. Env: SQI_AUTH_BOOTSTRAP_PASSWORD
	Password string `yaml:"password"`
}

// redactedPassword is the fixed placeholder emitted by [BootstrapConfig.MarshalYAML]
// in place of a non-empty Password. It is a marker, not a valid credential —
// unmarshaling it back would not reproduce the original password.
const redactedPassword = "<redacted>"

// MarshalYAML redacts Password so it never appears in YAML output (e.g.
// `sqi-server config print`, logs of a marshaled config). It does not affect
// unmarshaling: loading a config file with a real `password:` value is
// unaffected. Round-tripping a redacted dump is intentionally not supported —
// redaction wins over round-tripping.
func (b BootstrapConfig) MarshalYAML() (any, error) {
	type plain BootstrapConfig
	out := plain(b)
	if out.Password != "" {
		out.Password = redactedPassword
	}
	return out, nil
}

// LDAPConfig configures LDAP/Active Directory authentication (Phase 3, C1).
//
// LDAP is a login-time credential verifier, not a per-request authenticator:
// an account whose users.auth_source is "ldap" has its password checked
// against the directory by POST /auth/login, which then mints the same
// server-side session every local account gets.
//
// Two bind modes are mutually exclusive. Setting UserDNTemplate selects
// template mode (bind directly as the user, read their own memberOf); leaving
// it empty and setting BindDN/BaseDN selects search-then-bind.
type LDAPConfig struct {
	// Enabled turns on directory authentication.
	// Env: SQI_AUTH_LDAP_ENABLED
	Enabled bool `yaml:"enabled"`

	// URL is the directory endpoint: ldap://host:389 or ldaps://host:636.
	// Env: SQI_AUTH_LDAP_URL
	URL string `yaml:"url"`

	// StartTLS upgrades a plain ldap:// connection to TLS. Invalid with
	// ldaps://, which is already TLS.
	// Env: SQI_AUTH_LDAP_START_TLS
	StartTLS bool `yaml:"start_tls"`

	// TLSSkipVerify disables certificate verification. Logged as a warning at
	// boot; never silently accepted.
	// Env: SQI_AUTH_LDAP_TLS_SKIP_VERIFY
	TLSSkipVerify bool `yaml:"tls_skip_verify"`

	// CAFile is a PEM bundle used to verify the directory's certificate.
	// Env: SQI_AUTH_LDAP_CA_FILE
	CAFile string `yaml:"ca_file"`

	// Timeout bounds a single dial/bind/search cycle.
	// Env: SQI_AUTH_LDAP_TIMEOUT
	Timeout time.Duration `yaml:"timeout"`

	// BindDN is the service account used to search for users (search mode).
	// Env: SQI_AUTH_LDAP_BIND_DN
	BindDN string `yaml:"bind_dn"`

	// BindPassword is the service account's password. Never logged: redacted
	// by [LDAPConfig.MarshalYAML] whenever the config is re-marshaled (e.g.
	// `sqi-server config print`).
	// Env: SQI_AUTH_LDAP_BIND_PASSWORD
	BindPassword string `yaml:"bind_password"`

	// BaseDN is the subtree searched for user entries (search mode).
	// Env: SQI_AUTH_LDAP_BASE_DN
	BaseDN string `yaml:"base_dn"`

	// UserFilter is the search filter; %s is replaced with the escaped
	// username. Env: SQI_AUTH_LDAP_USER_FILTER
	UserFilter string `yaml:"user_filter"`

	// NestedGroups expands nested group membership via the AD matching-rule
	// OID. Search mode only — template mode reads memberOf, which is flat.
	// Env: SQI_AUTH_LDAP_NESTED_GROUPS
	NestedGroups bool `yaml:"nested_groups"`

	// UserDNTemplate builds the user's DN directly; %s is the escaped
	// username. Selects template mode. Env: SQI_AUTH_LDAP_USER_DN_TEMPLATE
	UserDNTemplate string `yaml:"user_dn_template"`

	// UsernameAttr is the attribute holding the login name.
	// Env: SQI_AUTH_LDAP_USERNAME_ATTR
	UsernameAttr string `yaml:"username_attr"`

	// DisplayNameAttr is the attribute holding the human-facing name. Read
	// only at JIT-create; never overwritten afterwards.
	// Env: SQI_AUTH_LDAP_DISPLAY_NAME_ATTR
	DisplayNameAttr string `yaml:"display_name_attr"`

	// UniqueIDAttr names the attribute holding the entry's stable,
	// non-reusable identifier: "objectGUID" on Active Directory, "entryUUID"
	// on OpenLDAP and other RFC 4530 servers.
	//
	// Required when LDAP is enabled, with no default: no single value is
	// correct on both server families, and probing for one would silently
	// guess wrong on a server exposing both. Accounts are matched on this,
	// never on username — a name lets a recycled identity inherit an account.
	// Env: SQI_AUTH_LDAP_UNIQUE_ID_ATTR
	UniqueIDAttr string `yaml:"unique_id_attr"`

	// RoleSource decides who owns an LDAP user's role:
	//   - "directory" (default): recomputed from groups on every login, and
	//     the users API rejects role edits on LDAP accounts.
	//   - "local": groups seed the role at JIT-create only; admins own it
	//     afterwards and the API allows edits.
	// Both halves are driven by this one value — never configure them apart,
	// or a role edit will appear to succeed and silently revert at next login.
	// Env: SQI_AUTH_LDAP_ROLE_SOURCE
	RoleSource string `yaml:"role_source"`

	// RoleMap maps group DNs to roles, first match wins. Order is
	// significant: it is how an operator expresses precedence.
	RoleMap []RoleMappingConfig `yaml:"role_map"`

	// DefaultRole applies when no group matches. Empty means reject the
	// login, so a deployment can require group membership to sign in at all.
	// Env: SQI_AUTH_LDAP_DEFAULT_ROLE
	DefaultRole string `yaml:"default_role"`
}

// MarshalYAML redacts BindPassword so it never appears in YAML output (e.g.
// `sqi-server config print`, logs of a marshaled config). It does not affect
// unmarshaling: loading a config file with a real `bind_password:` value is
// unaffected. Round-tripping a redacted dump is intentionally not supported —
// redaction wins over round-tripping.
func (l LDAPConfig) MarshalYAML() (any, error) {
	type plain LDAPConfig
	out := plain(l)
	if out.BindPassword != "" {
		out.BindPassword = redactedPassword
	}
	return out, nil
}

// RoleMappingConfig is one group-DN → role rule.
type RoleMappingConfig struct {
	Group string `yaml:"group"`
	Role  string `yaml:"role"`
}

// OIDCConfig configures OAuth2/OIDC single sign-on (Phase 3, C2).
//
// OIDC is a login-time verifier, not a per-request authenticator: the
// callback validates the ID token once and then mints the same server-side
// session a local password login produces. Nothing downstream — RBAC, owner
// binding, WebSocket scoping — is aware SSO exists.
//
// Deliberately a single provider block, not a list. Almost every
// organization has one identity provider, and configuration shape is
// uniquely expensive to revise because it lives in operators' files. If
// multi-provider is ever needed, add an auth.oidc.providers list alongside
// these fields and treat them as shorthand for a one-element list — do NOT
// reshape these into a list, which would break every existing config.
type OIDCConfig struct {
	// Enabled turns on SSO. Requires auth.enabled.
	// Env: SQI_AUTH_OIDC_ENABLED
	Enabled bool `yaml:"enabled"`

	// Issuer is the provider's discovery root, e.g.
	// https://login.microsoftonline.com/<tenant>/v2.0. Discovery is
	// performed lazily on first use, not at boot: a briefly unreachable
	// provider must not stop the scheduler from starting.
	// Env: SQI_AUTH_OIDC_ISSUER
	Issuer string `yaml:"issuer"`

	// ClientID is the application's registered identifier.
	// Env: SQI_AUTH_OIDC_CLIENT_ID
	ClientID string `yaml:"client_id"`

	// ClientSecret is the application's secret. Never logged: redacted by
	// [OIDCConfig.MarshalYAML] whenever the config is re-marshaled.
	// Env: SQI_AUTH_OIDC_CLIENT_SECRET
	ClientSecret string `yaml:"client_secret"`

	// RedirectURL is the absolute callback URL registered with the
	// provider. Must resolve to this server's
	// /api/v1/auth/oidc/callback.
	// Env: SQI_AUTH_OIDC_REDIRECT_URL
	RedirectURL string `yaml:"redirect_url"`

	// Scopes requested at authorization. Must include "openid". Note that
	// "groups" is NOT standard OIDC — whether group membership needs a
	// scope, a provider-side mapper, or both varies by vendor.
	// Env: SQI_AUTH_OIDC_SCOPES (comma-separated)
	Scopes []string `yaml:"scopes"`

	// UsernameClaim names the claim holding the login name. Used for
	// display and uniqueness only — accounts are matched on "sub".
	// Env: SQI_AUTH_OIDC_USERNAME_CLAIM
	UsernameClaim string `yaml:"username_claim"`

	// DisplayNameClaim names the claim holding the human-facing name. Read
	// only at provisioning; never overwritten afterwards.
	// Env: SQI_AUTH_OIDC_DISPLAY_NAME_CLAIM
	DisplayNameClaim string `yaml:"display_name_claim"`

	// GroupsClaim names the claim holding group memberships, fed to
	// RoleMap.
	// Env: SQI_AUTH_OIDC_GROUPS_CLAIM
	GroupsClaim string `yaml:"groups_claim"`

	// RoleSource decides who owns an OIDC user's role: "directory"
	// (recomputed from claims on every login; the users API rejects role
	// edits) or "local" (claims seed it at provisioning only). Separate
	// from auth.ldap.role_source on purpose — an operator may trust one
	// provider's groups and not the other's.
	// Env: SQI_AUTH_OIDC_ROLE_SOURCE
	RoleSource string `yaml:"role_source"`

	// RoleMap maps group claims to roles, first match wins. Order
	// expresses precedence. File-only: a list of pairs has no sane env
	// encoding.
	RoleMap []RoleMappingConfig `yaml:"role_map"`

	// DefaultRole applies when no group matches. Empty means reject the
	// login.
	// Env: SQI_AUTH_OIDC_DEFAULT_ROLE
	DefaultRole string `yaml:"default_role"`

	// ReauthMode controls when the provider is told to re-prompt for
	// credentials (prompt=login):
	//   - "after_logout" (default): only on the login following an
	//     explicit logout, so "log out" means the next person at a shared
	//     workstation must authenticate.
	//   - "always": every login. A hard guarantee, at the cost of SSO's
	//     convenience.
	//   - "never": silent re-login always permitted.
	// Env: SQI_AUTH_OIDC_REAUTH_MODE
	ReauthMode string `yaml:"reauth_mode"`

	// LogoutMode controls whether logging out of sqi also ends the session
	// at the provider ("provider") or only locally ("local", default).
	// Provider logout signs the user out of every company tool, so it is
	// off unless asked for.
	// Env: SQI_AUTH_OIDC_LOGOUT_MODE
	LogoutMode string `yaml:"logout_mode"`

	// PostLogoutRedirectURL is where the provider returns the browser
	// after a provider-side logout. Must be registered with the provider.
	// Only read when LogoutMode is "provider".
	// Env: SQI_AUTH_OIDC_POST_LOGOUT_REDIRECT_URL
	PostLogoutRedirectURL string `yaml:"post_logout_redirect_url"`

	// ButtonLabel is the login page's SSO button text.
	// Env: SQI_AUTH_OIDC_BUTTON_LABEL
	ButtonLabel string `yaml:"button_label"`
}

// MarshalYAML redacts ClientSecret so it never appears in YAML output (e.g.
// `sqi-server config print`). Round-tripping a redacted dump is
// intentionally not supported — redaction wins over round-tripping, as with
// [LDAPConfig.MarshalYAML].
func (o OIDCConfig) MarshalYAML() (any, error) {
	type plain OIDCConfig
	out := plain(o)
	if out.ClientSecret != "" {
		out.ClientSecret = redactedPassword
	}
	return out, nil
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

// PresetLibraryConfig controls the community preset library integration.
type PresetLibraryConfig struct {
	// URL is the library's JSON index URL (the raw index file, not a repo page).
	// Empty disables the feature. Default: the official sqi preset library.
	// Env: SQI_PRESET_LIBRARY_URL
	URL string `yaml:"url"`
}

// OpenJDConfig controls OpenJD submission and validation behavior.
type OpenJDConfig struct {
	// EnforceLimits gates quantitative limit validation (maximum name lengths,
	// counts, reserved-name rules). Disable only when onboarding templates that
	// predate the strict limits and cannot yet be updated.
	// Env: SQI_OPENJD_ENFORCE_LIMITS
	EnforceLimits bool `yaml:"enforce_limits"`

	// ExprOperationLimit is how many OpenJD expression-language operations
	// (specification section 1.3.10) ONE expression in a submitted template may
	// spend at validation time.
	//
	// Raising it lengthens the worst request this server can be asked to
	// serve, roughly in proportion: nothing meters wall clock, and an
	// operation's real cost varies by orders of magnitude between arithmetic
	// and a regex over a large string.
	// Range: [MinOpenJDExprOperationLimit, MaxOpenJDExprOperationLimit].
	// Env: SQI_OPENJD_EXPR_OPERATION_LIMIT
	ExprOperationLimit int64 `yaml:"expr_operation_limit"`

	// ExprMemoryLimit is how many live bytes (specification section 1.3.9) ONE
	// expression in a submitted template may hold at validation time.
	//
	// Raising it permits proportionally more memory per evaluation on an
	// unauthenticated request path; the ceiling is one order of magnitude above
	// the default for that reason. (An earlier revision of this comment claimed
	// a fixed internal guard made a larger value inert. It does not -- that was
	// measured and disproved. Treat the ceiling as a deliberate policy limit,
	// not as a point beyond which the setting stops working.)
	// Range: [MinOpenJDExprMemoryLimit, MaxOpenJDExprMemoryLimit].
	// Env: SQI_OPENJD_EXPR_MEMORY_LIMIT
	ExprMemoryLimit int64 `yaml:"expr_memory_limit"`

	// ExprTemplatePositions is how many expression positions ONE template may
	// contain — cumulative across the whole validation pass, not per field.
	//
	// This and ExprOperationLimit MULTIPLY: the cumulative operation ceiling
	// for one request is their product, and raising both raises it twice over.
	//
	// It also has a farm-wide consequence the server cannot check alone: a
	// worker's own per-assignment position cap must not be lower, or a job
	// this server ACCEPTS fails on every task at run time instead of failing
	// the one request that could have reported it.
	// Range: [MinOpenJDExprTemplatePositions, MaxOpenJDExprTemplatePositions].
	// Env: SQI_OPENJD_EXPR_TEMPLATE_POSITIONS
	ExprTemplatePositions int64 `yaml:"expr_template_positions"`

	// ExprTemplateRetainedBytes is how many bytes a template's `let:` bindings
	// may cumulatively retain across one validation pass.
	//
	// Counted as cumulative ALLOCATION, not peak live retention: a template
	// that never holds more than a few MB at once is still charged the sum of
	// every block. Sizing a host's RAM against this number will over-provision.
	// Range: [MinOpenJDExprTemplateRetainedBytes, MaxOpenJDExprTemplateRetainedBytes].
	// Env: SQI_OPENJD_EXPR_TEMPLATE_RETAINED_BYTES
	ExprTemplateRetainedBytes int64 `yaml:"expr_template_retained_bytes"`

	// ExprSubmissionDeadline is how long this server will keep evaluating one
	// submission's expressions before giving up on it.
	//
	// IT IS NOT ONE OF THE FOUR ABOVE, and the difference is the reason it
	// exists. Those four are DETERMINISTIC: a template that breaches one
	// breaches it on every machine, every time, so the breach is a property of
	// the template and the submitter is told their template is invalid (a 422
	// no retry can fix). This one is WALL CLOCK, so the same body would be
	// accepted on an idle host and rejected on a loaded one. A breach here
	// therefore reports that this server gave up (a 503 a retry may clear),
	// never that the template is wrong.
	//
	// It exists because none of the four bounds TIME. Specification section
	// 1.3.10 prices 256 bytes at one operation, so byte-heavy work is nearly
	// free in operations and expensive in seconds; the worst single request
	// measured on this branch is ~17 minutes of server CPU with every budget
	// respected. This is the only bound that does not depend on that
	// measurement being right.
	// Range: [MinOpenJDExprSubmissionDeadline, MaxOpenJDExprSubmissionDeadline].
	// Env: SQI_OPENJD_EXPR_SUBMISSION_DEADLINE
	ExprSubmissionDeadline time.Duration `yaml:"expr_submission_deadline"`
}

// The defaults and the operator-configurable range for [OpenJDConfig]'s four
// EXPR expression limits.
//
// These duplicate internal/openjd's DefaultExprLimits and Min/MaxExpr*
// constants ON PURPOSE: internal/config must not import internal/openjd (that
// would be a cross-import between two same-level internal packages, and
// internal/openjd pulls in internal/store). The duplication is pinned by
// TestExprLimits_ConfigMatchesOpenJD in internal/server -- the one package that
// may import both -- which fails the build if either side moves without the
// other.
//
// The rationale for each number — which bounds are catastrophe bounds with a
// ceiling no operator may raise past, which is a policy bound, and what the
// floors were measured against — lives on internal/openjd's ExprLimits and its
// Min/Max constants, next to the code the numbers govern.
const (
	DefaultOpenJDExprOperationLimit int64 = 10_000
	MinOpenJDExprOperationLimit     int64 = 1_000
	MaxOpenJDExprOperationLimit     int64 = 100_000

	DefaultOpenJDExprMemoryLimit int64 = 1_000_000
	MinOpenJDExprMemoryLimit     int64 = 4_096
	MaxOpenJDExprMemoryLimit     int64 = 10_000_000

	DefaultOpenJDExprTemplatePositions int64 = 10_000
	MinOpenJDExprTemplatePositions     int64 = 256
	MaxOpenJDExprTemplatePositions     int64 = 100_000

	DefaultOpenJDExprTemplateRetainedBytes int64 = 10_000_000
	MinOpenJDExprTemplateRetainedBytes     int64 = 65_536
	MaxOpenJDExprTemplateRetainedBytes     int64 = 100_000_000
)

// The default and the operator-configurable range for
// [OpenJDConfig.ExprSubmissionDeadline], EXPR sub-project H1's wall-clock
// backstop.
//
// Unlike the four above, these are NOT duplicated from internal/openjd: the
// deadline is a per-request absolute instant computed here from this duration,
// so internal/openjd holds no constant to keep in step with. Nothing in this
// package imports internal/openjd, and nothing needs to.
const (
	// DefaultOpenJDExprSubmissionDeadline is deliberately about three orders
	// of magnitude below the measured ~17-minute worst case rather than
	// derived from a template that needs it: no legitimate submission
	// observed on this branch spends anything close to five seconds in the
	// expression checker, and the point of the number is the distance from
	// the exposure, not that any particular second is special.
	DefaultOpenJDExprSubmissionDeadline = 5 * time.Second

	// MinOpenJDExprSubmissionDeadline is the floor because below a second a
	// legitimate large template -- a body near the 4 MiB request cap, on a
	// busy or slow host -- could plausibly fail, and a backstop that rejects
	// valid work is worse than the exposure it guards. It is the point past
	// which this setting stops being a backstop and becomes a load-dependent
	// admission policy.
	MinOpenJDExprSubmissionDeadline = time.Second

	// MaxOpenJDExprSubmissionDeadline is the ceiling because the key exists to
	// bound that ~17-minute figure. A ceiling anywhere near it would bound
	// nothing an operator could not already reach, and this repository's own
	// history records that the worst case has been under-reported three
	// separate times -- so the ceiling is set against what a submission should
	// ever need, not against what has been measured to be possible.
	MaxOpenJDExprSubmissionDeadline = 60 * time.Second
)

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
			Auth: NATSAuthConfig{
				Enabled:                   false,
				JoinTokenTTL:              1 * time.Hour,
				JoinTokenSingleUse:        true,
				EnrollmentEndpointEnabled: true,
			},
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
			HeartbeatTimeout:          30 * time.Second,
			TickInterval:              500 * time.Millisecond,
			MaxTasksPerWorker:         1,
			OfflineWorkerRetention:    24 * time.Hour,
			JobRetention:              7 * 24 * time.Hour,
			JobRetentionIncludeFailed: false,
			UnschedulableGrace:        30 * time.Second,
			DefaultMaxAttempts:        3,
			RetryDelay:                30 * time.Second,
			DefaultFailureLimit:       0,
		},
		Discovery: DiscoveryConfig{
			Enabled:      true,
			InstanceName: "sqi-server",
		},
		OpenJD: OpenJDConfig{
			EnforceLimits:             true,
			ExprOperationLimit:        DefaultOpenJDExprOperationLimit,
			ExprMemoryLimit:           DefaultOpenJDExprMemoryLimit,
			ExprTemplatePositions:     DefaultOpenJDExprTemplatePositions,
			ExprTemplateRetainedBytes: DefaultOpenJDExprTemplateRetainedBytes,
			ExprSubmissionDeadline:    DefaultOpenJDExprSubmissionDeadline,
		},
		Diagnostics: DiagnosticsConfig{
			BufferSize: 1000,
		},
		PresetLibrary: PresetLibraryConfig{
			URL: "https://uberware.github.io/sqi-presets/index.json",
		},
		Auth: AuthConfig{
			Enabled:          false,
			ValidateJobOwner: true,
			Session: SessionConfig{
				TTL:          168 * time.Hour,
				CookieName:   "sqi_session",
				CookieSecure: "auto",
			},
			LDAP: LDAPConfig{
				Enabled:         false,
				Timeout:         10 * time.Second,
				UserFilter:      "(sAMAccountName=%s)",
				UsernameAttr:    "sAMAccountName",
				DisplayNameAttr: "displayName",
				RoleSource:      "directory",
				DefaultRole:     "read-only",
			},
			OIDC: OIDCConfig{
				Enabled:          false,
				Scopes:           []string{"openid", "profile", "email"},
				UsernameClaim:    "preferred_username",
				DisplayNameClaim: "name",
				GroupsClaim:      "groups",
				RoleSource:       "directory",
				DefaultRole:      "read-only",
				ReauthMode:       oidc.ReauthAfterLogout,
				LogoutMode:       oidc.LogoutLocal,
				ButtonLabel:      "Sign in with SSO",
			},
		},
	}
}
