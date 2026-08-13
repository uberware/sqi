// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"time"

	"github.com/uberware/sqi/internal/api"
)

// routerConfig maps this server's [Config] onto the HTTP-layer [api.Config]
// that [api.NewRouter] is built with.
//
// It is a function rather than an inline literal in start for the reason
// ExprLimitsFromConfig is: a struct literal assigning one config's fields into
// another's is the shape where an assignment can be dropped, renamed, or
// pointed at the wrong field and still compile, start, and serve. Left inline
// it could only be reached by booting a whole server (NATS, SQLite, a
// listening socket), so in practice nothing tested it at all —
// TestRouterConfig_CarriesTheSubmissionDeadline does now.
//
// workerOfflineThreshold comes from the running scheduler rather than from
// Config, which is why it is a parameter: the scheduler normalizes its own
// heartbeat timeout, and the worker handler must gate on the value actually in
// force, not on the one the operator wrote down.
func routerConfig(cfg Config, workerOfflineThreshold time.Duration) api.Config {
	return api.Config{
		CORSOrigins:            cfg.CORSOrigins,
		EnablePprof:            cfg.EnablePprof,
		DisableRateLimit:       cfg.DisableRateLimit,
		WorkerOfflineThreshold: workerOfflineThreshold,
		AuthEnabled:            cfg.AuthEnabled,
		ValidateJobOwner:       cfg.AuthValidateJobOwner,
		ExprSubmissionDeadline: cfg.OpenJDExprSubmissionDeadline,
	}
}
