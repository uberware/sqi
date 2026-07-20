// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/uberware/sqi/internal/auth/policy"
)

// ValidationError describes a single configuration error with the field path
// that is invalid and an actionable message explaining how to fix it.
type ValidationError struct {
	// Field is the dot-separated config path, e.g. "http.addr" or
	// "scheduler.max_tasks_per_worker". It mirrors the YAML key hierarchy.
	Field string
	// Message describes what is wrong and how to correct it.
	Message string
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// Validate checks cfg for missing or invalid values. It returns all violations
// found in a single pass so callers can report every problem at once rather
// than failing on the first one.
//
// A nil or empty slice means the configuration is valid.
func Validate(cfg Config) []ValidationError {
	var errs []ValidationError
	errs = append(errs, validateHTTP(cfg.HTTP)...)
	errs = append(errs, validateNATS(cfg.NATS)...)
	errs = append(errs, validateStore(cfg.Store)...)
	errs = append(errs, validateLog(cfg.Log)...)
	errs = append(errs, validateScheduler(cfg.Scheduler)...)
	errs = append(errs, validateDiscovery(cfg.Discovery)...)
	errs = append(errs, validateDiagnostics(cfg.Diagnostics)...)
	errs = append(errs, validateAuth(cfg.Auth)...)
	return errs
}

func validateHTTP(cfg HTTPConfig) []ValidationError {
	if cfg.Addr == "" {
		return []ValidationError{{
			Field:   "http.addr",
			Message: "must not be empty; set SQI_HTTP_ADDR or http.addr in the config file",
		}}
	}
	if err := validateTCPAddr(cfg.Addr); err != nil {
		return []ValidationError{{
			Field:   "http.addr",
			Message: fmt.Sprintf("invalid address %q: %s", cfg.Addr, err),
		}}
	}
	return validateCORSOrigins(cfg.CORSOrigins)
}

// validateCORSOrigins rejects origins go-chi/cors can never match, so a typo
// fails loudly at boot instead of silently at request time.
func validateCORSOrigins(origins []string) []ValidationError {
	var errs []ValidationError
	for _, o := range origins {
		if o == "*" {
			continue
		}
		if strings.ContainsAny(o, " \t") {
			errs = append(errs, ValidationError{
				Field:   "http.cors_origins",
				Message: fmt.Sprintf("origin %q must not contain whitespace", o),
			})
			continue
		}
		if strings.Contains(o, "*") {
			// go-chi/cors treats ANY embedded '*' as a prefix/suffix wildcard,
			// so "https://*.example.com" or "https://app.example.com*" would be
			// honored — with credentials, once auth is enabled — letting an
			// attacker-registrable origin ride the victim's session cookie. Only
			// the bare "*" (handled above) is supported; reject the rest.
			errs = append(errs, ValidationError{
				Field:   "http.cors_origins",
				Message: fmt.Sprintf("origin %q must not contain a wildcard %q; wildcard patterns are not supported — name explicit origins (only a bare %q is allowed)", o, "*", "*"),
			})
			continue
		}
		u, err := url.Parse(o)
		if err != nil || u.Scheme == "" || u.Host == "" {
			errs = append(errs, ValidationError{
				Field:   "http.cors_origins",
				Message: fmt.Sprintf("origin %q must be scheme://host[:port] or %q", o, "*"),
			})
			continue
		}
		if u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			errs = append(errs, ValidationError{
				Field:   "http.cors_origins",
				Message: fmt.Sprintf("origin %q must not include a path, query, or fragment (drop the trailing slash)", o),
			})
		}
	}
	return errs
}

func validateNATS(cfg NATSConfig) []ValidationError {
	var errs []ValidationError
	if cfg.Addr == "" {
		errs = append(errs, ValidationError{
			Field:   "nats.addr",
			Message: "must not be empty; set SQI_NATS_ADDR or nats.addr in the config file",
		})
	} else if err := validateTCPAddr(cfg.Addr); err != nil {
		errs = append(errs, ValidationError{
			Field:   "nats.addr",
			Message: fmt.Sprintf("invalid address %q: %s", cfg.Addr, err),
		})
	}
	if cfg.DataDir == "" {
		errs = append(errs, ValidationError{
			Field:   "nats.data_dir",
			Message: "must not be empty; set SQI_NATS_DATA_DIR or nats.data_dir in the config file",
		})
	}
	if cfg.MaxStoreMB <= 0 {
		errs = append(errs, ValidationError{
			Field:   "nats.max_store_mb",
			Message: fmt.Sprintf("must be > 0, got %d; set SQI_NATS_MAX_STORE_MB or nats.max_store_mb", cfg.MaxStoreMB),
		})
	}
	return errs
}

func validateStore(cfg StoreConfig) []ValidationError {
	var errs []ValidationError
	if cfg.SQLitePath == "" {
		errs = append(errs, ValidationError{
			Field:   "store.sqlite_path",
			Message: "must not be empty; set SQI_STORE_SQLITE_PATH or store.sqlite_path in the config file",
		})
	}
	if cfg.CheckpointInterval <= 0 {
		errs = append(errs, ValidationError{
			Field: "store.checkpoint_interval",
			Message: fmt.Sprintf(
				"must be > 0, got %s; set SQI_STORE_CHECKPOINT_INTERVAL or store.checkpoint_interval",
				cfg.CheckpointInterval,
			),
		})
	}
	return errs
}

func validateLog(cfg LogConfig) []ValidationError {
	var errs []ValidationError
	switch strings.ToLower(cfg.Level) {
	case "debug", "info", "warn", "error":
		// valid
	case "":
		errs = append(errs, ValidationError{
			Field:   "log.level",
			Message: "must not be empty; accepted values: debug, info, warn, error",
		})
	default:
		errs = append(errs, ValidationError{
			Field:   "log.level",
			Message: fmt.Sprintf("unknown level %q; accepted values: debug, info, warn, error", cfg.Level),
		})
	}
	switch strings.ToLower(cfg.Format) {
	case "json", "text":
		// valid
	case "":
		errs = append(errs, ValidationError{
			Field:   "log.format",
			Message: "must not be empty; accepted values: json, text",
		})
	default:
		errs = append(errs, ValidationError{
			Field:   "log.format",
			Message: fmt.Sprintf("unknown format %q; accepted values: json, text", cfg.Format),
		})
	}
	return errs
}

func validateScheduler(cfg SchedulerConfig) []ValidationError {
	var errs []ValidationError
	if cfg.HeartbeatTimeout <= 0 {
		errs = append(errs, ValidationError{
			Field: "scheduler.heartbeat_timeout",
			Message: fmt.Sprintf(
				"must be > 0, got %s; set SQI_SCHEDULER_HEARTBEAT_TIMEOUT or scheduler.heartbeat_timeout",
				cfg.HeartbeatTimeout,
			),
		})
	}
	if cfg.TickInterval <= 0 {
		errs = append(errs, ValidationError{
			Field: "scheduler.tick_interval",
			Message: fmt.Sprintf(
				"must be > 0, got %s; set SQI_SCHEDULER_TICK_INTERVAL or scheduler.tick_interval",
				cfg.TickInterval,
			),
		})
	}
	if cfg.MaxTasksPerWorker < 1 {
		errs = append(errs, ValidationError{
			Field: "scheduler.max_tasks_per_worker",
			Message: fmt.Sprintf(
				"must be ≥ 1, got %d; set SQI_SCHEDULER_MAX_TASKS_PER_WORKER or scheduler.max_tasks_per_worker",
				cfg.MaxTasksPerWorker,
			),
		})
	}
	if cfg.UnschedulableGrace < 0 {
		errs = append(errs, ValidationError{
			Field: "scheduler.unschedulable_grace",
			Message: fmt.Sprintf(
				"must be >= 0 (0 disables the sweep), got %s; set SQI_SCHEDULER_UNSCHEDULABLE_GRACE or scheduler.unschedulable_grace",
				cfg.UnschedulableGrace,
			),
		})
	}
	if cfg.DefaultMaxAttempts < 1 {
		errs = append(errs, ValidationError{
			Field: "scheduler.default_max_attempts",
			Message: fmt.Sprintf(
				"must be >= 1 (1 disables auto-retry), got %d; set SQI_SCHEDULER_DEFAULT_MAX_ATTEMPTS or scheduler.default_max_attempts",
				cfg.DefaultMaxAttempts,
			),
		})
	}
	if cfg.RetryDelay < 0 {
		errs = append(errs, ValidationError{
			Field: "scheduler.retry_delay",
			Message: fmt.Sprintf(
				"must be >= 0 (0 = immediate), got %s; set SQI_SCHEDULER_RETRY_DELAY or scheduler.retry_delay",
				cfg.RetryDelay,
			),
		})
	}
	if cfg.DefaultFailureLimit < 0 {
		errs = append(errs, ValidationError{
			Field: "scheduler.default_failure_limit",
			Message: fmt.Sprintf(
				"must be >= 0 (0 disables the job-level failure ceiling), got %d; set SQI_SCHEDULER_DEFAULT_FAILURE_LIMIT or scheduler.default_failure_limit",
				cfg.DefaultFailureLimit,
			),
		})
	}
	return errs
}

func validateDiscovery(cfg DiscoveryConfig) []ValidationError {
	if cfg.InstanceName == "" {
		return []ValidationError{{
			Field:   "discovery.instance_name",
			Message: "must not be empty; set SQI_DISCOVERY_INSTANCE_NAME or discovery.instance_name",
		}}
	}
	return nil
}

func validateDiagnostics(cfg DiagnosticsConfig) []ValidationError {
	if cfg.BufferSize < 0 {
		return []ValidationError{{
			Field: "diagnostics.buffer_size",
			Message: fmt.Sprintf(
				"must be >= 0 (0 disables diagnostics), got %d; set SQI_DIAGNOSTICS_BUFFER_SIZE or diagnostics.buffer_size",
				cfg.BufferSize,
			),
		}}
	}
	return nil
}

// validateAuth validates the auth config. Rules only apply when auth is
// enabled — a disabled auth block must never produce validation errors, since
// its sub-fields carry no operational meaning until the gate is turned on.
//
// Bootstrap.Password is intentionally never echoed here: validation errors
// name the field, never the value.
func validateAuth(cfg AuthConfig) []ValidationError {
	var errs []ValidationError
	if !cfg.Enabled {
		if cfg.LDAP.Enabled {
			errs = append(errs, ValidationError{
				Field:   "auth.ldap.enabled",
				Message: "requires auth.enabled=true; LDAP without the auth gate authenticates nobody",
			})
		}
		return errs
	}
	if cfg.Session.TTL <= 0 {
		errs = append(errs, ValidationError{
			Field:   "auth.session.ttl",
			Message: "must be > 0 when auth is enabled; set SQI_AUTH_SESSION_TTL or auth.session.ttl",
		})
	}
	if cfg.Session.CookieName == "" {
		// An empty name defaults quietly in session.New and newAuthHandler, but
		// middleware.CSRF reads it raw via r.Cookie(""), which always errors —
		// every mutating request would then take the "no session cookie" exempt
		// path and CSRF would be silently disabled. Require it explicitly so
		// all three consumers agree on the name instead of relying on
		// convention.
		errs = append(errs, ValidationError{
			Field:   "auth.session.cookie_name",
			Message: "must not be empty when auth is enabled; set SQI_AUTH_SESSION_COOKIE_NAME or auth.session.cookie_name",
		})
	}
	switch cfg.Session.CookieSecure {
	case "auto", "true", "false":
		// valid
	default:
		errs = append(errs, ValidationError{
			Field: "auth.session.cookie_secure",
			Message: fmt.Sprintf(
				`must be "auto", "true", or "false", got %q; set SQI_AUTH_SESSION_COOKIE_SECURE or auth.session.cookie_secure`,
				cfg.Session.CookieSecure,
			),
		})
	}
	errs = append(errs, validateAuthBootstrap(cfg.Bootstrap)...)
	errs = append(errs, validateAuthLDAP(cfg.LDAP)...)
	return errs
}

// validateAuthBootstrap checks that Bootstrap.Username and Bootstrap.Password
// are either both empty (no bootstrap) or both set. Exactly one set usually
// means a typo'd env var name (e.g. SQI_AUTH_BOOSTRAP_PASSWORD) and risks a
// later bootstrap step creating an admin with an empty password.
func validateAuthBootstrap(cfg BootstrapConfig) []ValidationError {
	var errs []ValidationError
	if cfg.Username != "" && cfg.Password == "" {
		errs = append(errs, ValidationError{
			Field:   "auth.bootstrap.password",
			Message: "must be set when auth.bootstrap.username is set; set SQI_AUTH_BOOTSTRAP_PASSWORD or auth.bootstrap.password",
		})
	}
	if cfg.Password != "" && cfg.Username == "" {
		errs = append(errs, ValidationError{
			Field:   "auth.bootstrap.username",
			Message: "must be set when auth.bootstrap.password is set; set SQI_AUTH_BOOTSTRAP_USERNAME or auth.bootstrap.username",
		})
	}
	return errs
}

// validateAuthLDAP checks the auth.ldap block. It assumes auth.enabled is
// true (validateAuth returns early otherwise). Everything here fails closed:
// a misconfigured directory must abort boot rather than leave a server that
// silently cannot authenticate anyone.
func validateAuthLDAP(cfg LDAPConfig) []ValidationError {
	var errs []ValidationError
	if !cfg.Enabled {
		return errs
	}
	errs = append(errs, validateLDAPTransport(cfg)...)
	errs = append(errs, validateLDAPBindMode(cfg)...)
	errs = append(errs, validateLDAPAttrs(cfg)...)
	errs = append(errs, validateLDAPRoles(cfg)...)
	return errs
}

// validateLDAPAttrs requires the attribute names that both bind modes put on
// the wire verbatim. None has a safe empty meaning: an empty string is sent as
// an attribute name in the search request, which directories reject outright
// or answer in ways nothing here expects.
//
// username_attr and display_name_attr carry defaults, so reaching those errors
// means the operator explicitly cleared one. unique_id_attr deliberately has
// no default and must always be stated — see its error message.
func validateLDAPAttrs(cfg LDAPConfig) []ValidationError {
	var errs []ValidationError
	if cfg.UsernameAttr == "" {
		errs = append(errs, ValidationError{
			Field:   "auth.ldap.username_attr",
			Message: `must not be empty; set SQI_AUTH_LDAP_USERNAME_ATTR or auth.ldap.username_attr (e.g. "sAMAccountName" or "uid")`,
		})
	}
	if cfg.DisplayNameAttr == "" {
		errs = append(errs, ValidationError{
			Field:   "auth.ldap.display_name_attr",
			Message: `must not be empty; set SQI_AUTH_LDAP_DISPLAY_NAME_ATTR or auth.ldap.display_name_attr (e.g. "displayName" or "cn")`,
		})
	}
	if cfg.UniqueIDAttr == "" {
		errs = append(errs, ValidationError{
			Field: "auth.ldap.unique_id_attr",
			Message: `must not be empty; set SQI_AUTH_LDAP_UNIQUE_ID_ATTR or auth.ldap.unique_id_attr ` +
				`("objectGUID" on Active Directory, "entryUUID" on OpenLDAP). There is deliberately no ` +
				`default: no value is correct on both.`,
		})
	}
	return errs
}

func validateLDAPTransport(cfg LDAPConfig) []ValidationError {
	var errs []ValidationError
	switch cfg.URL {
	case "":
		errs = append(errs, ValidationError{
			Field:   "auth.ldap.url",
			Message: "must be set when auth.ldap.enabled; set SQI_AUTH_LDAP_URL or auth.ldap.url",
		})
	default:
		u, err := url.Parse(cfg.URL)
		if err != nil || (u.Scheme != "ldap" && u.Scheme != "ldaps") {
			errs = append(errs, ValidationError{
				Field:   "auth.ldap.url",
				Message: fmt.Sprintf("must be an ldap:// or ldaps:// URL, got %q", cfg.URL),
			})
		} else if cfg.StartTLS && u.Scheme == "ldaps" {
			errs = append(errs, ValidationError{
				Field:   "auth.ldap.start_tls",
				Message: "cannot be used with an ldaps:// URL, which is already TLS",
			})
		}
	}
	if cfg.Timeout <= 0 {
		errs = append(errs, ValidationError{
			Field:   "auth.ldap.timeout",
			Message: "must be > 0; set SQI_AUTH_LDAP_TIMEOUT or auth.ldap.timeout",
		})
	}
	return errs
}

func validateLDAPBindMode(cfg LDAPConfig) []ValidationError {
	var errs []ValidationError
	searchMode := cfg.BindDN != "" || cfg.BaseDN != ""
	templateMode := cfg.UserDNTemplate != ""
	switch {
	case searchMode && templateMode:
		errs = append(errs, ValidationError{
			Field:   "auth.ldap.user_dn_template",
			Message: "cannot be combined with auth.ldap.bind_dn/base_dn — choose template bind or search-then-bind, not both",
		})
	case !searchMode && !templateMode:
		errs = append(errs, ValidationError{
			Field:   "auth.ldap.base_dn",
			Message: "set auth.ldap.bind_dn+base_dn (search-then-bind) or auth.ldap.user_dn_template (template bind)",
		})
	// Reachable only when templateMode is false: the combined case above has
	// already claimed searchMode && templateMode. Spelled out rather than
	// left to case ordering, so reordering these arms cannot silently change
	// which one a both-modes-set config lands in.
	case searchMode && !templateMode:
		errs = append(errs, validateSearchBind(cfg)...)
	case templateMode:
		if !strings.Contains(cfg.UserDNTemplate, "%s") {
			errs = append(errs, ValidationError{
				Field:   "auth.ldap.user_dn_template",
				Message: `must contain %s, the placeholder for the escaped username (e.g. "uid=%s,ou=people,dc=example,dc=com")`,
			})
		}
		if cfg.NestedGroups {
			errs = append(errs, ValidationError{
				Field:   "auth.ldap.nested_groups",
				Message: "requires search-then-bind; template bind reads the flat memberOf attribute and cannot expand nested groups",
			})
		}
	}
	return errs
}

// validateSearchBind checks the search-then-bind keys. Split out of
// validateLDAPBindMode so that function stays inside the complexity budget
// and each bind mode's rules read as one unit.
func validateSearchBind(cfg LDAPConfig) []ValidationError {
	var errs []ValidationError
	if cfg.BaseDN == "" {
		errs = append(errs, ValidationError{
			Field:   "auth.ldap.base_dn",
			Message: "must be set for search-then-bind; set SQI_AUTH_LDAP_BASE_DN or auth.ldap.base_dn",
		})
	}
	// An empty bind_dn selects anonymous search, a legitimate deployment
	// for a world-readable directory. But a bind_password with no bind_dn
	// to go with it is not that — it is silently discarded and the
	// service searches anonymously, with no error and no log. Whatever
	// password the operator meant to configure never reaches the wire.
	if cfg.BindDN == "" && cfg.BindPassword != "" {
		errs = append(errs, ValidationError{
			Field:   "auth.ldap.bind_dn",
			Message: "must be set when auth.ldap.bind_password is set; set SQI_AUTH_LDAP_BIND_DN or auth.ldap.bind_dn, or clear SQI_AUTH_LDAP_BIND_PASSWORD/auth.ldap.bind_password for anonymous search",
		})
	}
	if cfg.UserFilter == "" || !strings.Contains(cfg.UserFilter, "%s") {
		errs = append(errs, ValidationError{
			Field:   "auth.ldap.user_filter",
			Message: `must contain %s, the placeholder for the escaped username (e.g. "(sAMAccountName=%s)")`,
		})
	}
	return errs
}

func validateLDAPRoles(cfg LDAPConfig) []ValidationError {
	var errs []ValidationError
	switch cfg.RoleSource {
	case "directory", "local":
		// valid
	default:
		errs = append(errs, ValidationError{
			Field:   "auth.ldap.role_source",
			Message: fmt.Sprintf(`must be "directory" or "local", got %q`, cfg.RoleSource),
		})
	}
	for i, m := range cfg.RoleMap {
		if m.Group == "" {
			errs = append(errs, ValidationError{
				Field:   fmt.Sprintf("auth.ldap.role_map[%d].group", i),
				Message: "must not be empty",
			})
		}
		// A typo'd role must abort boot, not silently fall through to
		// default_role — that would hand everyone the wrong privileges with
		// no error to explain why.
		if !policy.IsRole(m.Role) {
			errs = append(errs, ValidationError{
				Field:   fmt.Sprintf("auth.ldap.role_map[%d].role", i),
				Message: fmt.Sprintf("unknown role %q; must be one of admin, operator, user, read-only", m.Role),
			})
		}
	}
	// Empty is meaningful: reject logins that match no group.
	if cfg.DefaultRole != "" && !policy.IsRole(cfg.DefaultRole) {
		errs = append(errs, ValidationError{
			Field:   "auth.ldap.default_role",
			Message: fmt.Sprintf("unknown role %q; must be one of admin, operator, user, read-only, or empty to reject unmapped logins", cfg.DefaultRole),
		})
	}
	return errs
}

// validateTCPAddr checks that addr is a parseable host:port string.
func validateTCPAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("must be host:port — %w", err)
	}
	if port == "" {
		return errors.New("port is required")
	}
	// Allow empty host ("0.0.0.0" is encoded as "" by SplitHostPort for "[::]:port").
	// Attempt a resolution only when a non-empty, non-wildcard host is given.
	if host != "" && host != "0.0.0.0" && host != "::" {
		if net.ParseIP(host) == nil {
			// Not an IP literal — try resolving as a hostname.
			if _, err := net.DefaultResolver.LookupHost(context.Background(), host); err != nil {
				return fmt.Errorf("host %q cannot be resolved: %w", host, err)
			}
		}
	}
	return nil
}
