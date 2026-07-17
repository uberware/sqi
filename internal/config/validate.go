// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
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
	return nil
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
