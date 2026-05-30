// SPDX-License-Identifier: AGPL-3.0-only

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
	if cfg.SQLitePath == "" {
		return []ValidationError{{
			Field:   "store.sqlite_path",
			Message: "must not be empty; set SQI_STORE_SQLITE_PATH or store.sqlite_path in the config file",
		}}
	}
	return nil
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
